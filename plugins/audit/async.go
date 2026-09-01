package audit

import (
	"fmt"
	"log"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"
)

// 本文件是审计事件的**异步投递**实现：中间件在返回路径上只把事件塞进队列，
// 落地由后台 worker 做。
//
// 为什么审计必须异步、且没有「阻塞直到落地」这一档：
//   - 审计判定发生在**返回路径**上，此刻工具已经执行完了。同步等落地、失败就拒绝返回，
//     挡不住任何副作用（删除、写入已经生效），只是把结果藏起来，还得劝调用方别重试。
//     真正需要 fail-closed 的是执行**前**的门（人白名单、二次确认、配额），那几层各自有。
//   - 落地方常常是远端（HTTP 审计中心、消息队列）。同步等它，等于把审计后端的
//     P99 加到每一次 MCP 调用上；后端抖动时连 initialize 都会变慢。
//
// 代价必须写在文档里、不能藏：异步之后「每次调用必有记录」降级为**尽力而为**——
// 队列打满、进程被 kill -9、机器掉盘都会丢事件。因此丢弃绝不静默：计数 + 限流告警 +
// ReadStats() 供宿主接监控。

// 默认值。0 / 空串一律表示「取默认值」。
const (
	// defaultQueueSize 是事件队列容量。取值权衡：队列越大越能吸收落地抖动，
	// 但每个事件都占内存（入参/结果已在快照时截断到 KiB 级，所以 4096 条约几 MiB）。
	defaultQueueSize = 4096
	// defaultFlushTimeout 是进程退出时排空队列的时间预算。
	// 不能太长：它直接加在服务停止耗时上，发布/重启都要等它。
	defaultFlushTimeout = 3 * time.Second
	// alarmInterval 是各类告警日志的最小间隔（每类独立计时）。
	// 丢弃与落地失败都是**持续性**故障（后端挂了、队列一直满），逐条打会按 QPS 刷满磁盘。
	alarmInterval = time.Minute
)

// Stats 是审计投递的运行期计数，供宿主接监控用。
//
// 有了它，「异步会丢事件」才是一件可观测的事而不是一句免责声明：
// Dropped 持续增长说明队列容量或落地速度不够，Failed 增长说明 Sink 本身有问题。
type Stats struct {
	// Enqueued 是成功入队的事件数。
	Enqueued int64
	// Dropped 是被丢弃的事件数：入队时队列已满，或进程已进入停止流程。
	Dropped int64
	// Delivered 是已交给全部 Sink 且无一报错的事件数。
	Delivered int64
	// Failed 是至少有一个 Sink 报错或 panic 的事件数。
	Failed int64
}

// dispatcher 是异步投递器：一个有界队列 + 一个后台 worker。
//
// 单 worker（不是 worker 池）是刻意的：审计事件的**先后顺序**在对账时有意义，
// 多 worker 会让同一副本内的事件乱序落地，而并发投递省下的时间对「一天几万条」
// 这个量级没有意义。
type dispatcher struct {
	ch   chan Event
	quit chan struct{}
	done chan struct{}
	// flushTimeout 是 worker 在收到停止信号后排空队列的预算。
	flushTimeout time.Duration

	// stopping 让入队侧知道「已经在停了」。用原子标记而不是关闭 ch：
	// 关闭 channel 之后仍在途的 enqueue 会 panic（向已关闭 channel 发送），
	// 而请求路径上的 panic 是绝对不能有的。
	stopping atomic.Bool
	stopOnce sync.Once

	enqueued  atomic.Int64
	dropped   atomic.Int64
	delivered atomic.Int64
	failed    atomic.Int64

	dropAlarm *throttledLogger
	sinkAlarm *throttledLogger
}

// newDispatcher 建投递器并起 worker。
func newDispatcher(queueSize int, flushTimeout time.Duration) *dispatcher {
	d := &dispatcher{
		ch:           make(chan Event, queueSize),
		quit:         make(chan struct{}),
		done:         make(chan struct{}),
		flushTimeout: flushTimeout,
		dropAlarm:    &throttledLogger{interval: alarmInterval},
		sinkAlarm:    &throttledLogger{interval: alarmInterval},
	}
	go d.run()
	return d
}

// enqueue 把事件放进队列。**绝不阻塞**：队列满就丢弃并计数。
func (d *dispatcher) enqueue(e Event) {
	if d.stopping.Load() {
		d.drop(e, "投递器已停止（进程正在退出）")
		return
	}
	select {
	case d.ch <- e:
		d.enqueued.Add(1)
	default:
		// 丢**当前这条**、不挤掉队列里更早的那条：丢旧要加锁重排，而且已入队的事件
		// 先到先落才能保住审计时序。代价是故障期间丢的是最新的事件，日志里会说清。
		d.drop(e, "事件队列已满")
	}
}

// drop 记一次丢弃并按类限流告警。
func (d *dispatcher) drop(e Event, why string) {
	n := d.dropped.Add(1)
	if ok, suppressed, total := d.dropAlarm.record(); ok {
		log.Printf("[mcp] audit error: 审计事件被丢弃（%s）logid=%s method=%s tool=%s "+
			"累计丢弃=%d%s", why, e.LogID, e.Method, e.Tool, n,
			suppressedNote(suppressed, total))
	}
}

// run 是 worker 主循环。
func (d *dispatcher) run() {
	defer close(d.done)
	for {
		select {
		case e := <-d.ch:
			d.deliver(e)
		case <-d.quit:
			d.flush()
			return
		}
	}
}

// flush 在退出前尽量把队列里剩下的事件投出去，超出预算的按丢弃计数。
//
// 给预算而不是无限等：Sink 可能正卡在一个连不上的远端，那会把整个进程的停止流程拖住。
func (d *dispatcher) flush() {
	deadline := time.After(d.flushTimeout)
	for {
		select {
		case e := <-d.ch:
			d.deliver(e)
		case <-deadline:
			d.dropRemaining("排空超时（flush_timeout 用尽）")
			return
		default:
			// 队列已空：正常收尾。
			d.dropRemaining("")
			return
		}
	}
}

// dropRemaining 把队列里还剩的事件计成丢弃（reason 为空表示队列本就是空的）。
func (d *dispatcher) dropRemaining(reason string) {
	n := int64(len(d.ch))
	if n == 0 {
		return
	}
	d.dropped.Add(n)
	log.Printf("[mcp] audit error: 进程退出时有 %d 条审计事件未能落地（%s）", n, reason)
}

// deliver 把事件交给全部 Sink。
//
// 一个 Sink 失败或 panic 不影响其余的（逐个 recover）；失败只进日志，
// 不可能再影响业务结果——这条链路已经在请求返回之后了。
func (d *dispatcher) deliver(e Event) {
	fns := sinkList()
	if len(fns) == 0 {
		// Install 期已经拦过「一个 Sink 都没注册」，走到这里说明宿主在接流之后把
		// Sink 清掉了。仍然计成失败并告警，而不是当作成功。
		if ok, suppressed, total := d.sinkAlarm.record(); ok {
			log.Printf("[mcp] audit error: 没有任何落地函数，事件被丢弃 logid=%s%s",
				e.LogID, suppressedNote(suppressed, total))
		}
		// 计数放最后（与下面失败/成功两条路一致）：观察方以计数当作「这条处理完了」
		// 的信号，先加计数会让它在日志落笔之前就认为收尾。
		d.failed.Add(1)
		return
	}
	bad := false
	for i, fn := range fns {
		if err := callSink(fn, e); err != nil {
			bad = true
			if ok, suppressed, total := d.sinkAlarm.record(); ok {
				log.Printf("[mcp] audit error: 落地失败 sink[%d] logid=%s method=%s "+
					"tool=%s: %v%s", i, e.LogID, e.Method, e.Tool, err,
					suppressedNote(suppressed, total))
			}
		}
	}
	if bad {
		d.failed.Add(1)
		return
	}
	d.delivered.Add(1)
}

// stop 停止投递：先关入队口，再让 worker 限时排空。可重复调用。
func (d *dispatcher) stop() {
	d.stopOnce.Do(func() {
		// 顺序要紧：先置 stopping，新事件才不会在排空过程中源源不断进来，
		// 否则 flush 可能永远追不上队尾。
		d.stopping.Store(true)
		close(d.quit)
	})
	select {
	case <-d.done:
	case <-time.After(d.flushTimeout + time.Second):
		// worker 自己有排空预算，这里只是兜底：Sink 卡死时不能把进程停止流程挂住。
		log.Printf("[mcp] audit error: %s", "审计 worker 未在预算内退出（某个落地函数卡住了？），不再等待")
	}
	s := d.stats()
	log.Printf("[mcp] audit: 投递收尾 enqueued=%d delivered=%d failed=%d dropped=%d",
		s.Enqueued, s.Delivered, s.Failed, s.Dropped)
}

// stats 取当前计数快照。
func (d *dispatcher) stats() Stats {
	return Stats{
		Enqueued:  d.enqueued.Load(),
		Dropped:   d.dropped.Load(),
		Delivered: d.delivered.Load(),
		Failed:    d.failed.Load(),
	}
}

// callSink 调用单个落地函数，把 panic 转成 error。
//
// 堆栈只写本地日志：panic 现场是定位落地函数 bug 的唯一线索。
func callSink(fn Sink, e Event) (err error) {
	defer func() {
		if p := recover(); p != nil {
			log.Printf("[mcp] audit error: 落地函数 panic logid=%s method=%s tool=%s: %v\n%s",
				e.LogID, e.Method, e.Tool, p, debug.Stack())
			err = fmt.Errorf("panic: %v", p)
		}
	}()
	return fn(e)
}

// throttledLogger 是一个极小的日志限流器：同类事件的**第一条**立即打（状态变化沿），
// 之后 interval 内的都被压制但**计数**，下一条放行的日志带上「被压制多少条 / 累计多少条」。
//
// 为什么必须限流：审计的两类告警（丢弃、落地失败）都出现在**持续性**故障里
// （后端挂掉、队列一直满），逐条打会按 QPS 刷满磁盘。但「必然留痕」的承诺不能丢，
// 所以是限流 + 汇总计数，不是丢弃。
//
// 与 quota 插件里的同名类型是有意的重复实现：插件之间不互相 import
// （装一个插件不该把另一个拖进编译产物）。
type throttledLogger struct {
	interval time.Duration

	mu       sync.Mutex
	last     time.Time // 上次放行日志的时刻；零值表示还没打过
	suppress int64     // 自上次放行起被压制的条数
	total    int64     // 累计事件数（含被压制的）
}

// record 记一次事件。返回是否该打日志，以及（若该打）被压制的条数与累计事件总数。
func (l *throttledLogger) record() (shouldLog bool, suppressed, total int64) {
	if l == nil {
		return true, 0, 0
	}
	t := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.total++
	// last 为零值 => 首次事件，必打（沿触发）。
	// 时间差为负 => 机器时钟往回跳了，放行而不是静默：静默会把「时钟异常」一起吞掉。
	d := t.Sub(l.last)
	if !l.last.IsZero() && d >= 0 && d < l.interval {
		l.suppress++
		return false, 0, l.total
	}
	suppressed, l.suppress, l.last = l.suppress, 0, t
	return true, suppressed, l.total
}

// suppressedNote 拼汇总后缀。没有被压制的条目时返回空串，避免每条日志都带一串 0。
func suppressedNote(suppressed, total int64) string {
	if suppressed <= 0 {
		return ""
	}
	return fmt.Sprintf("（同类日志被限流压制 %d 条，累计 %d 条）", suppressed, total)
}
