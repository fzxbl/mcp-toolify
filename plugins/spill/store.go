package spill

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fzxbl/mcp-toolify/runtime"
)

// downloadPath 是下载端点的路由前缀。插件用 Registry.Route 注册它（自动套 token
// 鉴权）：这个端点提供的是大结果原文，用 RoutePublic 就等于开了一个无认证后门。
const downloadPath = "/spill/"

const (
	fileExt  = ".json" // 落盘文件统一是一份 JSON 对象
	dirPerm  = 0o700   // 目录只给本进程用户：里面是工具返回的业务数据
	filePerm = 0o600
	// writeBufSize 是落盘时的缓冲区大小：大结果分段写出，不在内存里拼整串。
	writeBufSize = 64 << 10
	// ownerHeaderLimit 是读取文件头部 owner 字段时允许扫过的最大字节数。
	// owner 字段是文件的第一个键、且写入时字段已被截断到 maxOwnerFieldBytes，
	// 正常情况下几百字节；限制它是为了不让一个被篡改的文件把下载请求拖成大读。
	ownerHeaderLimit = 8 << 10
	// maxOwnerFieldBytes 是 owner 里单个字段落盘时的截断上限。
	// Subject.ID 可能来自请求头（开了 trust_identity_header 时由调用方填），
	// 不设上限就等于让调用方决定文件头有多大。
	maxOwnerFieldBytes = 256
	// adoptGrace 是「认领孤儿文件」的额外宽限：id 不带副本归属信息（未配置
	// PublicBaseURL）且不是本实例创建的文件，只有在超过 ttl + adoptGrace 之后才回收。
	// 同一台机器上两个进程共享默认目录时，各自 ttl 不同，直接按自己的 ttl 删
	// 会把对方仍在用的文件删掉（审查实测）。
	adoptGrace = 24 * time.Hour
)

// idPattern 是**唯一**允许拼进文件路径的 id 形状，与基座 runtime.NewOwnedID 的
// 两种输出对齐：无归属时是 32 个十六进制字符；有归属时是 "<owner-seg>.<16 hex>"，
// owner-seg 为版本标记 + 小写无填充 base32（字符集 a-z2-7）。
//
// 校验不是可选的：id 来自 URL 路径，`/spill/../../etc/passwd` 只要拼进
// filepath.Join 就是一次任意文件读取。长度上限防的是超长路径。
var idPattern = regexp.MustCompile(`^(?:[0-9a-f]{32}|[a-z2-7]{2,256}\.[0-9a-f]{16})$`)

// fileOwner 是一份 spill 文件的属主，随文件一起落盘（文件的第一个字段）。
//
// 为什么曾经需要：下载端点若套 token 认证，token 只回答「你是不是合法调用方」，
// 回答不了「这份结果是不是你的」。公开下载后这层已不再是下载侧门槛——保护边界是
// 不可猜测的 id；fileOwner 仍写入文件头，供审计与排查。
type fileOwner struct {
	// Token 是 token 的**用途名**（[[tokens]].name），不是 token 值。
	Token string `json:"token"`
	// Subject 是调用人标识（Subject.ID），仅在 HasIdentity 时有意义。
	Subject string `json:"subject,omitempty"`
	// HasIdentity 区分「有真实身份」与「匿名」。基座默认不信任客户端身份头，
	// Subject.ID 常态为空，此时下载粒度退化为「同 token 用途名可下载」。
	HasIdentity bool `json:"has_identity"`
}

// ownerOfSubject 从调用主体取属主，并把字段截断到 maxOwnerFieldBytes。
func ownerOfSubject(s *runtime.Subject) fileOwner {
	if s == nil {
		return fileOwner{}
	}
	return fileOwner{
		Token:       cutField(s.Token),
		Subject:     cutField(s.ID),
		HasIdentity: s.ID != "",
	}
}

// cutField 截断落盘的属主字段（按字节，不保证 UTF-8 边界完整性——它只用于相等比较，
// 而比较双方都经过同样的截断）。
func cutField(s string) string {
	if len(s) > maxOwnerFieldBytes {
		return s[:maxOwnerFieldBytes]
	}
	return s
}

// allows 判断 req 这个请求主体能否下载属主为 o 的文件。
//
// 规则：token 用途名必须相同；属主有身份时还要求是同一个人。
// 属主匿名（HasIdentity=false）时不比对身份——那是基座默认配置下的常态，
// 把空串当成「一个 id 为空的人」会让所有下载都失败。
// 属主 token 为空（只可能来自不经 HTTP 的内部调用）时一律拒绝：这类文件没有
// 可比对的主体，宁可取不回来也不能给错人。
func (o fileOwner) allows(req fileOwner) bool {
	if o.Token == "" || o.Token != req.Token {
		return false
	}
	return !o.HasIdentity || o.Subject == req.Subject
}

// quota 是磁盘用量上限。零值表示不限制，仅供内部构造与测试使用——
// 配置层（Section.normalize）永远给出正数，没有「关掉上限」的写法。
type quota struct {
	// MaxFileBytes 是单个 spill 文件的上限，写超即中止并删掉半截文件。
	MaxFileBytes int64
	// MaxTotalBytes 是落盘目录里本实例可回收文件的总量上限。
	MaxTotalBytes int64
}

// errQuota 是超出磁盘上限的哨兵错误，便于日志与用例区分「写不进去」与「不让写」。
var errQuota = errors.New("超出 spill 磁盘上限")

// store 是 spill 文件的磁盘存储：落盘、按 TTL 回收、按 id 提供下载。
//
// 显式构造（不是全局单例）：单测要能各自拿一个临时目录，多实例也不该互相踩。
type store struct {
	dir     string
	ttl     time.Duration
	gcEvery time.Duration
	q       quota

	// frozenDownloadPath 是 Mount 之后固化的对外下载路径前缀（如 /mcp/plugin/spill/）。
	// URL 必须读它，而不是每次现取可变的 routePrefix：后者在启动后若被清空，
	// 路由仍挂在带前缀的路径上，模型却会拿到裸 /spill/<id>。
	frozenDownloadPath atomic.Value // string

	// root 是落盘目录的**句柄**（os.OpenRoot）。所有文件操作都经它做，不再拼绝对
	// 路径去调 os.OpenFile —— 原因见 newStore 的注释（启动后目录被换掉的窗口）。
	root *os.Root

	// dirDev/dirIno 是启动时落盘目录的 dev+ino（dirIDKnown=false 表示平台拿不到）。
	// swapWarned 记住上一轮对账结论，只在状态**变化**时打日志，不每轮刷屏。
	dirDev, dirIno uint64
	dirIDKnown     bool
	swapWarned     atomic.Bool

	// openForWrite 是 createFile 的可替换点，**只有测试会设置**（注入一个「能建、
	// 但写必然失败」的句柄）。存在的理由：写中途的 I/O 失败在真实环境里只发生在
	// 盘满/配额/设备错误上，没有它就没法把「写错误必须原样冒出、不被局部 err 遮住」
	// 这条不变量钉在用例里 —— 而那正是本插件出过的真 bug（复审 M-2）。
	openForWrite func(name string) (*os.File, error)

	// mu 保护磁盘用量账本。created 记的是**本实例**创建的文件（GC 只敢删自己的，
	// 见 gcOnce 的认领规则），total 是当前可回收文件的总字节数。
	mu      sync.Mutex
	created map[string]int64
	total   int64

	stop      chan struct{}
	done      chan struct{}
	started   atomic.Bool
	startOnce sync.Once
	stopOnce  sync.Once
}

// newStore 构造 store，建好落盘目录并校验它的安全性。
//
// 三项检查都会让启动失败，而不是留到运行期：
//   - 目录建不出来：一个「装了 spill 但其实写不进去」的进程，只会在第一次超大结果
//     时才暴露。
//   - 目录是符号链接：链接可以在服务启动前被换掉，等于把落盘目录交给别人决定。
//   - 目录 group/world 可写：os.MkdirAll 对**已存在**的目录不改权限，所以
//     「目录 0700」这条承诺在预先存在的 0777 目录上是静默失效的（审查实测）。
//     而默认目录 <tmp>/mcp-toolify/spill 是可预测路径 —— 这正是经典的 /tmp
//     预创建攻击：别人先建好目录，就能读走里面所有工具结果、或塞软链诱导读取。
func newStore(dir string, ttl, gcEvery time.Duration, q quota) (*store, error) {
	if dir == "" {
		return nil, fmt.Errorf("%s", "spill 落盘目录为空")
	}
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return nil, fmt.Errorf("spill 建目录 %s: %w", dir, err)
	}
	if err := ensurePrivateDir(dir); err != nil {
		return nil, err
	}
	// 目录句柄只在这里取一次，之后所有读写删都走它（root.OpenFile / root.Remove / …）。
	// 这解决两个 ensurePrivateDir 单靠启动时一次校验管不住的缺口（复审 I-1）：
	//   - 「启动后替换」：拿到句柄后路径解析从这个 fd 开始，攻击者把目录 mv 走再换成
	//     软链也没用 —— 句柄仍然指向原来那个 inode，后续文件照旧落在真目录里，
	//     既不会写进攻击者目录，也不会从攻击者目录读出伪造的属主文件。
	//   - 「O_NOFOLLOW 只保护路径最后一段」：目录部分的软链由 os.Root 拦下
	//     （逃出根的路径直接报 path escapes from parent）。
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("spill 打开目录句柄 %s: %w", dir, err)
	}
	s := &store{
		dir: dir, ttl: ttl, gcEvery: gcEvery, q: q,
		root:    root,
		created: map[string]int64{},
		stop:    make(chan struct{}), done: make(chan struct{}),
	}
	// 记下目录身份，供每轮 GC 对账（见 checkDirIdentity）。取不到就不做这项检查。
	if info, err := os.Lstat(dir); err == nil {
		s.dirDev, s.dirIno, s.dirIDKnown = fileIdentity(info)
	}
	return s, nil
}

// checkDirIdentity 对账「启动时记下的 dev+ino」与当前落盘目录路径，不一致就打一条
// warning。**非致命**，且只在状态变化时打（不每轮刷屏），成本是每个 gc_interval
// 一次 Lstat（默认 5 分钟一次 syscall）。
//
// 为什么需要：目录句柄钉住了原 inode，所以「有人把落盘目录挪走/换成软链」这件事对
// 本服务完全静默 —— 落盘照旧成功、下载照旧正常。这条日志是它唯一的可观测信号。
//
// 为什么文案要写「不要重启」：重启会让新进程的 os.OpenRoot 落在攻击者布置的那条
// 软链上（那时目录检查看到的是一个归属正确、0700 的新目录，全部通过）。
// 正确处置是先查是谁动了这个路径，再决定怎么恢复。
func (s *store) checkDirIdentity() {
	if !s.dirIDKnown {
		return
	}
	// 路径已经不存在（被 mv 走、被删）也算换手：句柄仍在，但那个路径不再是它。
	swapped := true
	if info, err := os.Lstat(s.dir); err == nil {
		dev, ino, ok := fileIdentity(info)
		if !ok {
			return // 平台拿不到身份，不做这项判断
		}
		swapped = dev != s.dirDev || ino != s.dirIno
	}
	if s.swapWarned.Swap(swapped) == swapped {
		return // 结论与上一轮相同，不重复打
	}
	if !swapped {
		log.Printf("[mcp] spill: 落盘目录路径 %s 已恢复为启动时那个目录", s.dir)
		return
	}
	log.Printf("[mcp] spill warning: 落盘目录路径 %s 已不是启动时那个目录"+
		"（有人把它挪走、删掉或换成了符号链接）。本服务的落盘**未受影响**："+
		"文件仍在原目录（进程持有目录句柄，inode 保持不变），下载也照旧。"+
		"**不要为此重启服务** —— 重启会让新的目录句柄落到被换上去的那个路径上。"+
		"请先排查是谁在动这个路径", s.dir)
}

// nameFor 把 id 换成落盘目录内的**相对**文件名；ok=false 表示 id 形状不合法。
// root 系操作只接受目录内相对名，不再有可拼接的绝对路径。
func (s *store) nameFor(id string) (string, bool) {
	if !idPattern.MatchString(id) {
		return "", false
	}
	return id + fileExt, true
}

// idOfFileName 从落盘文件名反解出 id：本插件认三种文件——中间件落盘的
// <id>.json、宿主经 Writer 写入的 <id>.raw（见 raw.go）、交给第三方写入方的
// <id>.ext-<format> 及其兄弟文件（见 extern.go）。其余文件（别人放进目录的东西）
// 一律不算 spill 文件，扫描与回收都不碰。
func idOfFileName(name string) (string, bool) {
	for _, ext := range []string{fileExt, rawExt} {
		if id, ok := strings.CutSuffix(name, ext); ok && idPattern.MatchString(id) {
			return id, true
		}
	}
	return idOfExternFileName(name)
}

// fileKind 是一份 spill 内容的落盘形态，决定属主判定与 payload 起点从哪来。
type fileKind uint8

const (
	kindEnvelope fileKind = iota // 中间件落盘的 <id>.json 信封，属主是第一个字段
	kindRaw                      // 宿主经 Writer 写入的 <id>.raw，首行是文件头
	kindExtern                   // 交给第三方写入方的 <id>.ext-<format>，无文件头
)

// resolve 找出某个 id 实际落盘的文件名、它的形态，以及 extern 形态的格式。
func (s *store) resolve(id string) (name string, kind fileKind, f Format, ok bool) {
	if !idPattern.MatchString(id) {
		return "", kindEnvelope, "", false
	}
	if _, err := s.root.Lstat(id + fileExt); err == nil {
		return id + fileExt, kindEnvelope, "", true
	}
	if _, err := s.root.Lstat(id + rawExt); err == nil {
		return id + rawExt, kindRaw, "", true
	}
	for _, format := range externFormats {
		n := id + externExt(format)
		if _, err := s.root.Lstat(n); err == nil {
			return n, kindExtern, format, true
		}
	}
	return "", kindEnvelope, "", false
}

// openFile 以只读方式打开目录内的文件，且不跟随符号链接。
//
// 为什么要先 Lstat：os.Root 会**自己解析**留在根内的相对软链，于是传给底层的
// O_NOFOLLOW 拿到的已经是解析后的目标 —— 「目录内的软链指向目录内另一个文件」这种
// 形态挡不住（实测直接 200）。Lstat 不跟随最后一段，是这里唯一可靠的判据。
// 逃出落盘目录的软链另有 os.Root 结构性兜住（path escapes from parent）。
//
// 残余 TOCTOU：Lstat 与 OpenFile 之间路径可能被换成软链。要利用它得先能在这个
// 0700、且已被 fd 钉住的目录里创建文件 —— 那时攻击者本来就能直接改结果文件了。
func (s *store) openFile(name string) (*os.File, error) {
	info, err := s.root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("spill 文件 %s 不是普通文件，拒绝读取", name)
	}
	// 硬链接同样是「普通文件」，Lstat 判不出来，只能看链接数（见 checkNotHardLink）。
	if err := checkNotHardLink(name, info); err != nil {
		return nil, err
	}
	return s.root.OpenFile(name, os.O_RDONLY|noFollowFlag, 0)
}

// createFile 新建目录内的文件（必须不存在），同样不跟随符号链接。
func (s *store) createFile(name string) (*os.File, error) {
	return s.root.OpenFile(name,
		os.O_WRONLY|os.O_CREATE|os.O_EXCL|noFollowFlag, filePerm)
}

// ensurePrivateDir 校验并（能改就改）收紧落盘目录的权限。
//
// 先 chmod 再复查，而不是一上来就拒绝：目录由本进程用户拥有时（常见的
// 「上次跑的时候 umask 松了」）应该顺手修好；改不动（目录属于别人）才启动失败。
func ensurePrivateDir(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("spill 检查目录 %s: %w", dir, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("spill 落盘目录 %s 是符号链接，拒绝使用："+
			"链接目标可在启动前被替换", dir)
	}
	if !info.IsDir() {
		return fmt.Errorf("spill 落盘目录 %s 不是目录", dir)
	}
	if !dirOwnedByUs(info) {
		return fmt.Errorf("spill 落盘目录 %s 不属于当前用户，拒绝使用："+
			"目录的拥有者可以删改里面的工具结果", dir)
	}
	if err := checkDirMode(info.Mode().Perm()); err != nil {
		if cerr := os.Chmod(dir, dirPerm); cerr != nil {
			return fmt.Errorf("spill 落盘目录 %s: %w（尝试收紧权限失败: %v）", dir, err, cerr)
		}
		info, serr := os.Lstat(dir)
		if serr != nil {
			return fmt.Errorf("spill 复查目录 %s: %w", dir, serr)
		}
		if err := checkDirMode(info.Mode().Perm()); err != nil {
			return fmt.Errorf("spill 落盘目录 %s: %w", dir, err)
		}
		log.Printf("[mcp] spill: 落盘目录 %s 的权限已收紧为 %#o", dir, os.FileMode(dirPerm))
	}
	return nil
}

// checkDirMode 判定落盘目录的权限位是否安全。
//
// 为什么不能只靠 MkdirAll 的 0700：os.MkdirAll 对**已存在**的目录不改权限，
// 所以「目录 0700」这条承诺在预先存在的 0777 目录上是静默失效的（审查实测）。
// 而默认目录 <tmp>/mcp-toolify/spill 是可预测路径 —— 这正是经典的 /tmp 预创建攻击：
// 别人先建好宽权限目录，就能读走里面所有工具结果、或塞软链诱导下载端点读别处。
func checkDirMode(perm os.FileMode) error {
	if perm&0o077 != 0 {
		return fmt.Errorf("权限 %#o 允许同组或其他用户访问：里面是工具返回的原始结果，"+
			"必须是 %#o", perm, os.FileMode(dirPerm))
	}
	return nil
}

// startGC 启动后台回收：先立刻扫一遍（回收上一个进程留下的文件），之后按周期扫。
// 幂等，重复调用只启动一个协程。
func (s *store) startGC() {
	s.startOnce.Do(func() {
		s.started.Store(true)
		go s.gcLoop()
	})
}

// close 停掉回收协程并等它退出，由插件注册进 Registry.OnStop。
// 幂等：Start 的两条退出路径都会跑 OnStop。
//
// 刻意不删文件：正在被 agent 下载的结果不该因为一次重启就消失，
// 到期由下一个进程启动时的第一次扫描回收（见 startGC）。
func (s *store) close() {
	s.stopOnce.Do(func() {
		close(s.stop)
		if s.started.Load() {
			<-s.done
		}
		if s.root != nil {
			// 句柄占着一个 fd，且关掉之后任何迟到的写/读都会明确失败，
			// 不会摸到一个已经不属于本进程的目录。
			if err := s.root.Close(); err != nil {
				log.Printf("[mcp] spill warning: 关闭目录句柄失败: %v", err)
			}
		}
		log.Printf("[mcp] spill: 已停止回收协程，落盘目录 %s 的文件按 ttl=%s 到期回收",
			s.dir, s.ttl)
	})
}

// gcLoop 按周期回收过期文件，直到 close。
func (s *store) gcLoop() {
	defer close(s.done)
	s.gcOnce()
	t := time.NewTicker(s.gcEvery)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.gcOnce()
		}
	}
}

// gcOnce 扫一遍目录，删掉超过 TTL 的 spill 文件，并把总量压回上限之内。
//
// 「哪些文件算自己的」有明确规则（见 mine）：落盘目录可能被两个进程共享
// （默认目录在同机多副本下天然共享），而各自的 ttl 不同 —— 按自己的 ttl 去删
// 目录里所有形状像 spill 的文件，会把对方仍在用的结果删掉（审查实测：A 的 ttl=1m
// 扫一遍就删掉了 B 那份 ttl=24h、只落了 2 小时的文件）。
func (s *store) gcOnce() {
	s.checkDirIdentity()
	files := s.scan()
	removed, freed := 0, int64(0)
	kept := make([]diskFile, 0, len(files))
	for _, f := range files {
		if !s.expired(f.mod) {
			kept = append(kept, f)
			continue
		}
		if s.remove(f) {
			removed++
			freed += f.size
		}
	}
	// 总量兜底：TTL 之内也可能把盘写满（一个能连续吐大结果的工具，半小时足够）。
	// 盘满之后 on_error=deny 会把每次大结果调用都变成错误，从磁盘问题升级成
	// 服务不可用，所以宁可提前按「最旧优先」腾地方。
	evicted, evictedBytes := s.evictToLimit(kept, 0)
	s.recount(nil)
	if removed+evicted > 0 {
		log.Printf("[mcp] spill: 回收 %d 个过期文件（%d 字节）、按总量上限淘汰 %d 个"+
			"（%d 字节），ttl=%s dir=%s", removed, freed, evicted, evictedBytes, s.ttl, s.dir)
	}
}

// diskFile 是目录扫描的一条记录。
type diskFile struct {
	id   string
	name string
	size int64
	mod  time.Time
}

// scan 列出目录里**属于本实例**的 spill 文件（按修改时间从旧到新）。
func (s *store) scan() []diskFile {
	entries, err := fs.ReadDir(s.root.FS(), ".")
	if err != nil {
		log.Printf("[mcp] spill warning: 扫描落盘目录 %s 失败: %v", s.dir, err)
		return nil
	}
	out := make([]diskFile, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		id, ok := idOfFileName(e.Name())
		if !ok {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue // 软链等非普通文件一律不碰
		}
		if !s.mine(id, info.ModTime()) {
			continue
		}
		out = append(out, diskFile{id: id, name: e.Name(),
			size: info.Size(), mod: info.ModTime()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].mod.Before(out[j].mod) })
	return out
}

// mine 判断某个 spill 文件是否可以由本实例回收。三条之一即可：
//  1. 本实例创建的；
//  2. id 内嵌的副本地址就是本副本（同一副本的前任进程留下的残留）；
//  3. id 不带副本归属信息（未配置 PublicBaseURL）且已经超过 ttl + adoptGrace ——
//     这是给「无从判断归属」留的兜底回收，宽限一天以免删掉同机另一进程仍在用的文件。
//
// 残留限制（已知、有意）：多个部署共享同一个 dir 且都不配 PublicBaseURL 时，
// ttl 超过 adoptGrace 的那一方仍可能被对方提前回收。请给每个部署单独配 dir。
func (s *store) mine(id string, mod time.Time) bool {
	s.mu.Lock()
	_, own := s.created[id]
	s.mu.Unlock()
	if own {
		return true
	}
	if owner, ok := runtime.OwnerOf(id); ok {
		return owner == runtime.SelfHostPort()
	}
	return time.Since(mod) > s.ttl+adoptGrace
}

// remove 删除一个文件并同步账本；返回是否真的删掉了。
func (s *store) remove(f diskFile) bool {
	if err := s.root.Remove(f.name); err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[mcp] spill warning: 删除文件 %s 失败: %v", f.name, err)
		}
		return false
	}
	s.mu.Lock()
	delete(s.created, f.id)
	s.mu.Unlock()
	return true
}

// evictToLimit 在总量超过上限时按「最旧优先」淘汰，直到腾出 need 字节的余量。
// keep 是本轮仍然存活的文件（从旧到新）。返回淘汰个数与释放字节数。
func (s *store) evictToLimit(keep []diskFile, need int64) (int, int64) {
	if s.q.MaxTotalBytes <= 0 {
		return 0, 0
	}
	total := int64(0)
	for _, f := range keep {
		total += f.size
	}
	count, freed := 0, int64(0)
	for _, f := range keep {
		if total+need <= s.q.MaxTotalBytes {
			break
		}
		if !s.remove(f) {
			continue
		}
		total -= f.size
		freed += f.size
		count++
	}
	return count, freed
}

// recount 重算账本总量（extra 为刚写入、尚未被扫描到的文件）。
func (s *store) recount(extra *diskFile) {
	total := int64(0)
	for _, f := range s.scan() {
		total += f.size
	}
	if extra != nil {
		total += extra.size
	}
	s.mu.Lock()
	s.total = total
	s.mu.Unlock()
}

// usage 返回当前账本里的总字节数（测试与日志用）。
func (s *store) usage() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.total
}

// expired 判断某个修改时间是否已超过 TTL。
func (s *store) expired(mod time.Time) bool { return time.Since(mod) > s.ttl }

// pathFor 把 id 解析成落盘的绝对路径；ok=false 表示 id 不是本插件生成的形状。
//
// 只用于日志与测试断言。真正的读写删一律走 nameFor + root（见 newStore）：
// 拼出来的绝对路径每次解析都要重新走一遍目录部分，正是复审 I-1 那个窗口。
func (s *store) pathFor(id string) (string, bool) {
	if !idPattern.MatchString(id) {
		return "", false
	}
	return filepath.Join(s.dir, id+fileExt), true
}

// freezeDownloadPath 在 Mount/build 期固化对外下载路径。
// 必须在 setRoutePrefix 之后调用（Registry.Mount → Handlers → OnBuild）。
func (s *store) freezeDownloadPath() {
	s.frozenDownloadPath.Store(runtime.RoutePath(downloadPath))
}

// url 返回该 id 的对外下载地址；没有**可用**的对外地址时返回空串并告警一次。
//
// 对外入口每次现取 PublicBaseURL（基座在 Start / Handlers 里才设置它）；
// 路径前缀则读 Mount/build 期固化的值——不能每次现取可变 routePrefix，否则会出现
// 「路由挂在 /mcp/plugin/spill/、链接却拼成 /spill/」的脱节。
//
// 「可用」要单独判一次：未显式配置时基座回退到实际监听地址，监听 ":8011" 得到的是
// http://[::]:8011 —— 一个 agent 根本连不上的通配地址（审查实测）。这种 URL 放进模型
// 上下文比不给更糟，所以宁可不给 URL、只给本地路径，并打一条启动级告警。
func (s *store) url(id string) string {
	base := runtime.PublicBaseURL()
	if usableBase(base) {
		path, _ := s.frozenDownloadPath.Load().(string)
		if path == "" {
			path = runtime.RoutePath(downloadPath)
		}
		return base + path + id
	}
	warnBaseOnce.Do(func() {
		log.Printf("[mcp] spill warning: 对外地址 %q 不可用于下载（未配置 PublicBaseURL /"+
			"SelfAddr，或其 host 是通配地址）：摘要里不会给下载 URL。多副本部署请把"+
			"PublicBaseURL 配成 agent 可达的入口（域名/VIP 均可），把 SelfAddr 配成本副本可"+
			"直连的 host:port（后者不要配 LB 地址，那样 id 无法区分副本）", base)
	})
	return ""
}

// warnBaseOnce 保证「对外地址不可用」只告警一次，不刷满日志。
var warnBaseOnce sync.Once

// usableBase 判断对外基础地址是否真的能被 agent 访问：空串、无 host、
// 通配地址（0.0.0.0 / [::] / ::）都不算。
func usableBase(base string) bool {
	if base == "" {
		return false
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return false
	}
	host := u.Hostname()
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		return false
	}
	return true
}

// put 把一次工具结果落盘，返回 id 与落盘字节数。own 是本次调用的主体，随文件一起
// 写在文件头部，下载时据此判定「这份结果是不是你的」。
//
// **分段写**，不在内存里拼整串：Content 逐段序列化后立刻冲进带缓冲的文件，
// structuredContent 若已是 json.RawMessage（生成的工具走这条路）直接透写。
// 内存峰值因此随「最大的那一段内容」增长，而不是随段数增长（判据见
// TestSpillWritesResultInSegments：下层收到的最大一次写只有一段那么大）；
// 单段仍会被 json.Marshal 整串展开一次，
// 这是为了让落盘文件与 SDK 的 wire 形状严格一致，没有手写编码器的走偏风险。
//
// 已知取舍：落盘成功之后，外层插件仍可能拒绝这次调用（例如 audit 的
// on_error=deny 在落地失败时拒绝返回结果），此时文件已经写下去了，会孤留一个
// TTL 周期后被回收。宁可留一个到期自清的孤儿文件，也不要在链路上倒着删文件——
// 那需要 spill 知道外层的判决结果，而中间件链是单向的。
func (s *store) put(tool string, res *mcp.CallToolResult, own fileOwner) (id string, size int64, err error) {
	id = runtime.NewOwnedID()
	name, ok := s.nameFor(id)
	if !ok {
		// 只可能是基座的 id 生成规则变了而这里的校验没跟上。宁可落盘失败，
		// 也不把一个形状不明的 id 拼进文件路径。
		return "", 0, fmt.Errorf("生成的 spill id 形状不合法: %q", id)
	}
	create := s.createFile
	if s.openForWrite != nil {
		create = s.openForWrite
	}
	f, err := create(name)
	if err != nil {
		return "", 0, fmt.Errorf("spill 建文件: %w", err)
	}
	w := &countingWriter{w: bufio.NewWriterSize(f, writeBufSize), limit: s.q.MaxFileBytes}
	// 写错误必须原样带出来，不能被 f.Close() 的返回值覆盖、也不能被局部 err 遮住：
	// 「磁盘写失败但 put 报成功」会让摘要指向一份坏文件。
	werr := writeSpillJSON(w, tool, res, own)
	if werr == nil {
		werr = w.w.Flush()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		// 半截文件既不能给模型也不能留在盘上：留着它会让下载端点吐出坏 JSON。
		_ = s.root.Remove(name)
		if errors.Is(werr, errQuota) {
			return "", 0, fmt.Errorf("spill 写文件: %w（单文件上限 %d 字节）",
				werr, s.q.MaxFileBytes)
		}
		return "", 0, fmt.Errorf("spill 写文件: %w", werr)
	}
	s.mu.Lock()
	s.created[id] = w.n
	s.mu.Unlock()
	if err := s.enforceTotal(id); err != nil {
		_ = s.root.Remove(name)
		s.mu.Lock()
		delete(s.created, id)
		s.mu.Unlock()
		return "", 0, err
	}
	return id, w.n, nil
}

// enforceTotal 在总量超限时按「最旧优先」淘汰其它文件腾地方；腾不出来就报错，
// 由调用方删掉刚写的这份并走 on_error 策略。
func (s *store) enforceTotal(newID string) error {
	s.recount(nil)
	if s.q.MaxTotalBytes <= 0 || s.usage() <= s.q.MaxTotalBytes {
		return nil
	}
	keep := make([]diskFile, 0)
	newSize := int64(0)
	for _, f := range s.scan() {
		if f.id == newID {
			newSize = f.size
			continue
		}
		keep = append(keep, f)
	}
	// need 传新文件的大小：淘汰的目标是「腾出足够容纳它的余量」，
	// 只按旧文件算总量会得出「已经够了」的错误结论。
	evicted, freed := s.evictToLimit(keep, newSize)
	s.recount(nil)
	if s.usage() > s.q.MaxTotalBytes {
		return fmt.Errorf("%w：落盘目录已用 %d 字节，上限 %d 字节（已淘汰 %d 个旧文件、"+
			"释放 %d 字节仍不够）", errQuota, s.usage(), s.q.MaxTotalBytes, evicted, freed)
	}
	log.Printf("[mcp] spill: 为新文件腾出空间，按最旧优先淘汰 %d 个文件（%d 字节）",
		evicted, freed)
	return nil
}

// writeSpillJSON 按固定结构流式写出（owner 必须是第一个字段，下载时只读文件头部
// 就能拿到属主，不必把整份结果读进内存）：
//
//	{"owner":{...},"tool":"a.read","at":"<RFC3339>","content":[...],"structuredContent":...}
func writeSpillJSON(w *countingWriter, tool string, res *mcp.CallToolResult, own fileOwner) error {
	ownerJSON, err := json.Marshal(own)
	if err != nil {
		return fmt.Errorf("序列化属主: %w", err)
	}
	toolJSON, err := json.Marshal(tool)
	if err != nil {
		return fmt.Errorf("序列化工具名: %w", err)
	}
	w.str(`{"owner":`)
	w.raw(ownerJSON)
	w.str(`,"tool":`)
	w.raw(toolJSON)
	w.str(`,"at":`)
	w.str(`"` + time.Now().Format(time.RFC3339) + `"`)
	w.str(`,"content":[`)
	for i, item := range res.Content {
		if i > 0 {
			w.str(",")
		}
		b, err := json.Marshal(item)
		if err != nil {
			return fmt.Errorf("序列化 content[%d]: %w", i, err)
		}
		w.raw(b)
	}
	w.str("]")
	if res.StructuredContent != nil {
		w.str(`,"structuredContent":`)
		if err := writeStructured(w, res.StructuredContent); err != nil {
			return err
		}
	}
	w.str("}")
	return w.err
}

// readOwner 从文件头部读出属主。只用 json.Decoder 解到第一个字段就停，
// 且限读 ownerHeaderLimit 字节 —— 一份几十兆的结果不该为了鉴权被整份读进内存。
func readOwner(f *os.File) (fileOwner, error) {
	dec := json.NewDecoder(io.LimitReader(f, ownerHeaderLimit))
	tok, err := dec.Token()
	if err != nil {
		return fileOwner{}, fmt.Errorf("读 spill 文件头: %w", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return fileOwner{}, fmt.Errorf("%s", "spill 文件不是 JSON 对象")
	}
	key, err := dec.Token()
	if err != nil {
		return fileOwner{}, fmt.Errorf("读 spill 文件首字段: %w", err)
	}
	if name, ok := key.(string); !ok || name != "owner" {
		return fileOwner{}, fmt.Errorf("%s", "spill 文件首字段不是 owner")
	}
	var own fileOwner
	if err := dec.Decode(&own); err != nil {
		return fileOwner{}, fmt.Errorf("解析 spill 文件属主: %w", err)
	}
	return own, nil
}

// writeStructured 写 structuredContent：已是 JSON 原文（生成的工具给的是
// json.RawMessage）就直接透写，避免再 marshal 一遍几十兆；其余类型走编码器。
func writeStructured(w *countingWriter, v any) error {
	if raw, ok := jsonRawOf(v); ok {
		w.raw(raw)
		return w.err
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		return fmt.Errorf("序列化 structuredContent: %w", err)
	}
	return w.err
}

// jsonRawOf 把「已经是 JSON 原文」的 structuredContent 取成字节；ok=false 表示不是。
//
// 只认 json.RawMessage：SDK 对 []byte 的处理是编码成 base64 字符串，把 []byte 也当
// JSON 原文透写会让落盘文件里的 structuredContent 与线上返回体形状不一致（审查 M6）。
// 只认合法 JSON：透写一段坏字节会把整个文件写成不可解析的 JSON。
func jsonRawOf(v any) ([]byte, bool) {
	b, ok := v.(json.RawMessage)
	if !ok || !json.Valid(b) {
		return nil, false
	}
	return b, true
}

// countingWriter 累计写出的字节数，并把首个写错误粘住（后续调用直接跳过），
// 让流式写出的代码不必在每一行后面判错。
// limit > 0 时是单文件字节上限，写超即以 errQuota 中止（不截断成坏 JSON）。
type countingWriter struct {
	w     *bufio.Writer
	n     int64
	limit int64
	err   error
}

func (c *countingWriter) Write(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	if c.limit > 0 && c.n+int64(len(p)) > c.limit {
		c.err = errQuota
		return 0, c.err
	}
	n, err := c.w.Write(p)
	c.n += int64(n)
	if err != nil {
		c.err = err
	}
	return n, err
}

func (c *countingWriter) raw(b []byte) { _, _ = c.Write(b) }
func (c *countingWriter) str(s string) { _, _ = c.Write([]byte(s)) }

// downloadHandler 返回 /spill/<id> 的下载 handler。
//
// 用 Registry.RoutePublic 注册：浏览器直接打开链接，不要求 Authorization。
// 保护边界是不可猜测的 spill id（随机段 + 可选的副本归属信息），外加目录权限与 TTL。
//
// 公开下载不再做按调用主体的属主校验——浏览器没有 MCP Subject，那层校验会把所有
// 直链都拦成 404。多副本下归属兄弟副本的请求在进入本 handler 之前就已被基座反代走。
func (s *store) downloadHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "spill 下载端点只支持 GET/HEAD", http.StatusMethodNotAllowed)
			return
		}
		// 只取路径最后一段：本端点可能被宿主挂在任意前缀之下（Handlers 把路由交给
		// 宿主自己 mount），按固定前缀裁剪会在换了挂载点之后整体失效。
		id := idFromDownloadPath(r.URL.Path)
		if !idPattern.MatchString(id) {
			// 形状不合法的 id 一律 404，不回显它：回显等于给探测者一面镜子。
			http.NotFound(w, r)
			return
		}
		// 归属兄弟副本的 id 在进入本 handler 之前就已被基座反代走（见 Install 里的
		// runtime.RegisterOwnerRoutedRoute）；能走到这里说明该由本副本回答。
		name, kind, format, ok := s.resolve(id)
		if !ok {
			http.NotFound(w, r)
			return
		}
		f, err := s.openFile(name)
		if err != nil {
			// 含这几种：文件是符号链接（O_NOFOLLOW）、软链逃出落盘目录（os.Root）、
			// 目录句柄已关闭（进程正在退出）。
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() || s.expired(info.ModTime()) {
			// 非普通文件（软链/目录/设备）一律不提供；
			// 过期文件在 GC 醒来之前也不再提供：TTL 是对使用方的承诺，
			// 不该取决于回收协程的周期。
			http.NotFound(w, r)
			return
		}
		switch kind {
		case kindRaw:
			hdr, off, err := readRawHeader(f)
			if err != nil {
				log.Printf("[mcp] spill warning: 无法读取 %s 的文件头，拒绝下载: %v", id, err)
				http.NotFound(w, r)
				return
			}
			s.serveContent(w, r, id, hdr, off, f, info)
			return
		case kindExtern:
			s.serveContent(w, r, id, externHeader(id, format), 0, f, info)
			return
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			http.Error(w, "spill 读取失败", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Disposition", `attachment; filename="`+id+fileExt+`"`)
		http.ServeContent(w, r, id+fileExt, info.ModTime(), f)
	})
}

// serveContent 提供宿主写入的内容：公开下载，只把 payload 段交出去。
func (s *store) serveContent(w http.ResponseWriter, r *http.Request, id string,
	hdr rawHeader, off int64, f *os.File, info os.FileInfo) {
	filename := hdr.Name
	if filename == "" {
		filename = id
	}
	w.Header().Set("Content-Type", hdr.Format.mime())
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	http.ServeContent(w, r, filename, info.ModTime(),
		io.NewSectionReader(f, off, info.Size()-off))
}

// downloadOwnerExtractor 是基座路径 owner 路由的提取器（见 Install 里的
// runtime.RegisterOwnerRoutedRoute）：从下载路径里取出 id，交由基座判断该 id 是否
// 归属某个已知兄弟副本，若是则把这次下载单跳反代过去。
//
// 为什么需要转发：id 内嵌产出该文件的副本地址，但文件只存在那台机器的本地磁盘上。
// agent 侧拿到的下载 URL 常常经过 LB（或干脆是 LB 地址），落到哪个副本是随机的，
// 没有转发时 (N-1)/N 的请求都会 404。
//
// 返回空串即「本次不参与 owner 路由」，用来表达两种本地优先的情形：
//   - id 形状不合法：交给 handler 回 404（那里不回显 id）；
//   - 本地就有这份文件（同机多副本共享目录时会这样）：不必绕一趟属主。
//
// 前提：PublicBaseURL 必须是**本副本可直连的地址**（Pod IP 之类），不能配成 LB
// 地址 —— 配 LB 时所有副本的 SelfHostPort 相同，id 里也就没有区分副本的信息。
// 转发目标只取 runtime.PeerAllowed 认可的地址（服务发现或静态 peers），防 SSRF。
// 公开下载不再要求属主副本再做 token 认证。
//
// 已知风险（复审 M-3，沿用基座 runtime/owner_routing.go 的既有约定，本插件不单独改）：
// 副本间走的是明文 http://。要收掉这条风险需要在 peer 之间加 TLS。
func (s *store) downloadOwnerExtractor(r *http.Request) string {
	id := idFromDownloadPath(r.URL.Path)
	if !idPattern.MatchString(id) {
		return ""
	}
	if _, _, _, ok := s.resolve(id); ok {
		return "" // 本地就有，不必转发
	}
	return id
}

// idFromDownloadPath 从下载请求的路径里取 id：只取最后一段。
// 本端点可能被宿主挂在任意前缀之下（Registry.Mount 给出的前缀，或 Handlers 模式下由宿主自己
// mount），按固定前缀裁剪会在换了挂载点之后整体失效。
func idFromDownloadPath(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}
