package spill

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync/atomic"
	"time"
)

// Format 是宿主写入内容的格式声明。它决定下载时的 Content-Type，也决定
// spill_explore 能对它做哪些操作（jsonl 可逐行 jq、text 只能 read/grep）。
type Format string

// 三种格式。刻意只有这三种：它们对应「一份 JSON」「一行一条记录」「纯文本日志」
// 这三类真实用法，多出来的格式没有对应的探索方式。
const (
	FormatJSON  Format = "json"  // 单个 JSON 值（对象或数组）
	FormatJSONL Format = "jsonl" // 一行一个 JSON 值，可增量追加
	FormatText  Format = "text"  // 纯文本，按行读/grep
)

// valid 判断格式是否是本插件认识的三种之一。
func (f Format) valid() bool {
	switch f {
	case FormatJSON, FormatJSONL, FormatText:
		return true
	}
	return false
}

// mime 返回该格式的 Content-Type。
func (f Format) mime() string {
	switch f {
	case FormatJSONL:
		return "application/x-ndjson; charset=utf-8"
	case FormatText:
		return "text/plain; charset=utf-8"
	default:
		return "application/json; charset=utf-8"
	}
}

// current 是本进程已安装的 spill 存储，由 Install 设置、OnStop 清除。
//
// 为什么允许一个包级变量（本仓库的纪律是「测试注入点用未导出字段、禁止包级可变
// var」）：Put/Create 这组 API 是**给使用方的**，宿主的业务代码（异步探测任务、
// 大结果导出）拿不到、也不该拿到插件在 Install 里构造的那个 store 指针 —— 与
// audit.OnEvent / confirm.OnRequest 属于同一类「宿主侧入口」例外。原子指针而不是
// 裸变量：宿主的后台协程可能在进程退出、OnStop 已经关掉目录句柄之后还在写。
var current atomic.Pointer[store]

// ErrNotInstalled 表示还没有 Install（或已经 OnStop）就调用了写入/读取 API。
//
// 刻意不「按需自动初始化一个默认 store」：那会在没人配置落盘目录时悄悄往系统临时
// 目录写业务数据，且与配置认领制给出的目录不是同一个——宿主会看到 spill_explore
// 找不到自己刚写的文件。
var ErrNotInstalled = errors.New("spill 插件尚未安装（或已停止）：请先 spill.Install(r)")

// storeOrErr 取当前 store。
func storeOrErr() (*store, error) {
	if st := current.Load(); st != nil {
		return st, nil
	}
	return nil, ErrNotInstalled
}

// Info 是一份 spill 内容的元信息。
type Info struct {
	// ID 是内容 id（内嵌产出副本的地址，见 runtime.NewOwnedID）。
	ID string
	// Name 是宿主写入时给的名字，仅用于人读与下载文件名。
	Name string
	// Format 是写入时声明的格式。
	Format Format
	// Size 是 payload 字节数（不含内部头部）。
	Size int64
	// ModTime 是最后一次写入时间；TTL 从它开始算。
	ModTime time.Time
	// Shared 为真表示这份内容没有绑定调用主体，任何通过 token 认证的调用方都能下载。
	Shared bool
}

// Put 把一段宿主产生的内容写入 spill 存储，返回可用于下载与探索的 id。
//
// 典型用法是「工具自己产出了一份大结果，不想经中间件的自动落盘」：例如把一次
// SQL 查询结果导成 CSV、把批量接口的返回摊平成 jsonl 交给 agent 逐行探索。
//
// 属主：写入方不是一次 MCP 调用，没有可比对的调用主体，因此这类内容是**共享**的
// ——任何通过 `/spill/<id>` token 认证的调用方都能下载（见 Info.Shared）。
// 只该给某一个人看的内容不要用它写；需要按人隔离时用 PutFor 传入 Subject。
func Put(name string, f Format, data []byte) (id string, err error) {
	w, err := Create(name, f)
	if err != nil {
		return "", err
	}
	if _, err := w.Write(data); err != nil {
		_ = w.Close()
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	return w.ID(), nil
}

// Create 创建一份可**增量写入**的 spill 内容，返回的 Writer 边写边可被
// spill_explore 读到（status=running 时就能读，不必等写完）。
//
// 这是异步任务（批量探测、长时间导出）该用的入口：先拿到 id 交给调用方，
// 后台协程持续追加。Writer 不是并发安全的，多个协程写同一份内容请自行串行化。
func Create(name string, f Format) (*Writer, error) { return create(name, f, nil) }

// Open 按 id 打开一份 spill 内容（只读），返回的 ReadSeekCloser 只覆盖 payload。
//
// 宿主一般不需要它 —— agent 侧用 spill_explore 或下载端点。它存在是为了让宿主的
// 后续处理（例如把上一步的输出喂给下一步）不必绕回 HTTP。
func Open(id string) (io.ReadSeekCloser, Info, error) {
	st, err := storeOrErr()
	if err != nil {
		return nil, Info{}, err
	}
	f, hdr, off, info, err := st.openContent(id)
	if err != nil {
		return nil, Info{}, err
	}
	return &sectionCloser{
		SectionReader: io.NewSectionReader(f, off, info.Size()-off),
		f:             f,
	}, hdr.info(id, info, off), nil
}

// URLFor 返回该 id 的对外下载地址；未配置可用的 PublicBaseURL 时为空串。
//
// 空串不是错误：单副本本地场景没有对外地址，调用方仍可用 spill_explore 探索。
// 把空串直接拼进给模型的文案会得到 "下载：" 这种半句话，请自行判空。
func URLFor(id string) string {
	st, err := storeOrErr()
	if err != nil {
		return ""
	}
	return st.url(id)
}

// Writer 是一份正在写入的 spill 内容。
type Writer struct {
	st   *store
	id   string
	name string
	f    *os.File
	// written 是已写入的 payload 字节数，用于单文件上限判定。
	written int64
	closed  bool
}

// ID 返回这份内容的 id（Create 返回时即已确定，可以先交给调用方再慢慢写）。
func (w *Writer) ID() string { return w.id }

// Write 追加内容。超出单文件上限时返回错误并停止写入（已写部分仍可读）。
func (w *Writer) Write(p []byte) (int, error) {
	if w.closed {
		return 0, fmt.Errorf("spill 内容 %s 已关闭，不能继续写", w.id)
	}
	if q := w.st.q.MaxFileBytes; q > 0 && w.written+int64(len(p)) > q {
		return 0, fmt.Errorf("%w: 单个文件上限 %d 字节", errQuota, q)
	}
	n, err := w.f.Write(p)
	w.written += int64(n)
	if n > 0 {
		w.st.noteWrite(w.id, w.written)
	}
	return n, err
}

// Close 结束写入。多次调用是安全的（第二次起是 no-op）。
func (w *Writer) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	err := w.f.Close()
	// 写完再核一次总量上限：与中间件落盘同一套账本，超了按最旧优先淘汰。
	if terr := w.st.enforceTotal(w.id); terr != nil && err == nil {
		err = terr
	}
	return err
}

// sectionCloser 让 Open 返回的分段读句柄也能关闭底层文件。
type sectionCloser struct {
	*io.SectionReader
	f *os.File
}

// Close 关闭底层文件句柄。
func (s *sectionCloser) Close() error { return s.f.Close() }
