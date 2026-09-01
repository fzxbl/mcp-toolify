package runtime

import (
	"log"
	"sync"
	"time"
)

// warnThrottle 是基座内部的告警限流器：同一类告警在 interval 内只打一条，
// 并在放行的那条里带上「被压制多少条 / 累计多少条」。
//
// 为什么必须限流：基座这几条告警（入站 logid 不合规、准入拒绝）都由**调用方**触发，
// 一个跑错配置的客户端能把日志刷满，进而把真正要看的行冲掉。
//
// 时钟用真实时钟而不是可注入的事件时钟：限流是运维设施，不该跟着被测/被伪造的时钟走。
// 这与 plugins/quota 的 throttledLogger 是同一套形状——刻意各自实现而不抽公共包，
// 基座不为插件提供工具函数是本项目的分层约定。
type warnThrottle struct {
	interval time.Duration

	mu       sync.Mutex
	last     time.Time
	suppress int64
	total    int64
}

// warn 记一次告警事件，按限流决定是否真的打日志。
func (w *warnThrottle) warn(format string, args ...any) {
	if w == nil {
		log.Printf("[mcp] warning: "+format, args...)
		return
	}
	now := time.Now()
	w.mu.Lock()
	w.total++
	// last 为零值 => 首次事件，必打（沿触发）。
	// 时间差为负 => 机器时钟往回跳了，放行而不是静默。
	d := now.Sub(w.last)
	if !w.last.IsZero() && d >= 0 && d < w.interval {
		w.suppress++
		w.mu.Unlock()
		return
	}
	suppressed, total := w.suppress, w.total
	w.suppress, w.last = 0, now
	w.mu.Unlock()
	if suppressed > 0 {
		log.Printf("[mcp] warning: "+format+"（同类告警被压制 %d 条，累计 %d 条）",
			append(args, suppressed, total)...)
		return
	}
	log.Printf("[mcp] warning: "+format+"（累计 %d 条）", append(args, total)...)
}
