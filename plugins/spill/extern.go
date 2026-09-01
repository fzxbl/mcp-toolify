package spill

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/fzxbl/mcp-toolify/runtime"
)

// externExtPrefix 是「交给第三方写入方的文件」的扩展名前缀，完整形态是
// <id>.ext-<format>（如 <id>.ext-text）。
//
// 为什么格式写在文件名里、而不像 .raw 那样写在文件第一行：这类文件的写入方是
// 只接受**文件名**的第三方（日志库、批量执行框架），它们会自己 os.Create ——
// 那是一次截断，任何预先写好的文件头都会被抹掉。文件名是唯一存得住的地方。
const externExtPrefix = ".ext-"

// externFormats 是允许出现在 extern 文件名里的格式，顺序与匹配无关。
var externFormats = []Format{FormatJSON, FormatJSONL, FormatText}

// externExt 返回某个格式的 extern 扩展名。
func externExt(f Format) string { return externExtPrefix + string(f) }

// fileSuffix 返回下载文件名用的扩展名（text 用 .txt，更符合使用者预期）。
func (f Format) fileSuffix() string {
	if f == FormatText {
		return "txt"
	}
	return string(f)
}

// externHeader 合成 extern 文件的「虚拟文件头」：这类内容没有落盘的文件头，
// 一律按共享处理（与 Put 同一条放宽，理由见 CreatePath）。
func externHeader(id string, f Format) rawHeader {
	return rawHeader{Shared: true, Name: id + "." + f.fileSuffix(), Format: f}
}

// idOfExternFileName 从 extern 文件名反解 id，兄弟文件（<id>.ext-text.wf 这种）
// 也归到同一个 id。
//
// 为什么要认兄弟文件：第三方写入方会顺带产生它们（logit 的 .wf、批量执行框架的
// .debug）。不认就等于没人回收——扫描跳过、GC 不删、总量记账也漏，落盘目录只增不减。
func idOfExternFileName(name string) (string, bool) {
	i := strings.Index(name, externExtPrefix)
	if i <= 0 {
		return "", false
	}
	id, rest := name[:i], name[i:]
	if !idPattern.MatchString(id) {
		return "", false
	}
	for _, f := range externFormats {
		if ext := externExt(f); rest == ext || strings.HasPrefix(rest, ext+".") {
			return id, true
		}
	}
	return "", false
}

// CreatePath 在 spill 存储里预留一份内容，返回 id 与可直接写入的**文件路径**。
//
// 给路径而不是 io.Writer，是因为宿主侧真正的写入方常常是只接受文件名的第三方
// （批量执行框架、日志库）。它们会自己创建/截断这个路径，所以这类内容里没有本
// 插件的文件头，格式记在文件名后缀里。
//
// 与 Create 的差别（选用判据）：
//   - Create 走本插件的 Writer：有单文件上限、内容可绑定属主（CreateFor）。
//   - CreatePath 交出路径后本插件管不到写入过程：**没有**单文件上限（越界要靠
//     调用方自己或 ulimit 兜底），内容按共享处理（任何通过 token 认证的调用方
//     都能下载，见 Put 的说明）。
//
// 写入方顺带产生的兄弟文件（<path>.wf 之类）与本份内容共用同一个 id，会一起被
// TTL 回收；但只有 path 这一个文件能被下载与 spill_explore 探索。
func CreatePath(f Format) (id, path string, err error) {
	st, err := storeOrErr()
	if err != nil {
		return "", "", err
	}
	if !f.valid() {
		return "", "", fmt.Errorf("spill 格式 %q 不认识（只接受 json / jsonl / text）", f)
	}
	return st.reservePath(f)
}

// reservePath 占住一个 id 对应的 extern 文件名，并返回它的绝对路径。
//
// 先把空文件建出来（O_EXCL）：id 与路径要在返回给调用方的那一刻就已经排他占用，
// 否则两次 CreatePath 之间的窗口里，另一方可以抢先在这个名字上放一条软链。
func (s *store) reservePath(f Format) (string, string, error) {
	id := runtime.NewOwnedID()
	if !idPattern.MatchString(id) {
		return "", "", fmt.Errorf("生成的 spill id 形状不合法: %q", id)
	}
	name := id + externExt(f)
	file, err := s.createFile(name)
	if err != nil {
		return "", "", fmt.Errorf("spill 预留文件: %w", err)
	}
	if err := file.Close(); err != nil {
		return "", "", fmt.Errorf("spill 预留文件: %w", err)
	}
	// 先记账：写入是第三方在做的，账本要在那之前就有这条记录，否则并发的 GC 会
	// 把它当成别的进程留下的孤儿文件（见 store.mine）。
	s.noteWrite(id, 0)
	return id, filepath.Join(s.dir, name), nil
}
