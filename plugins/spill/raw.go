package spill

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/fzxbl/mcp-toolify/runtime"
)

// rawExt 是「宿主自己写入的内容」的扩展名，与中间件落盘的 .json 区分开。
//
// 为什么不共用 .json 那套信封（{"owner":..,"tool":..,"content":[..]}）：宿主的用法是
// **边写边读**（异步任务写日志、agent 用 spill_explore 增量看），而信封是一个必须
// 完整闭合的 JSON 值——写一半的信封既不能被解析、也不能按行探索。
const rawExt = ".raw"

// rawHeader 是 raw 文件的第一行（JSON + '\n'），后面紧跟 payload 原文。
//
// 放在文件里而不是另开一个 sidecar 文件：GC、总量记账、目录扫描都按「一个 id 一个
// 文件」写的，多一个兄弟文件会让这三处各自漏算一半。
type rawHeader struct {
	// Owner 是绑定的调用主体（PutFor 才有）；Shared 为真时无意义。
	Owner fileOwner `json:"owner"`
	// Shared 表示这份内容没有绑定主体：任何通过 token 认证的调用方都能下载。
	// 宿主后台协程写入时没有可比对的主体，这是刻意接受的放宽（见 Put 的说明）。
	Shared bool `json:"shared"`
	// Name 是宿主给的名字，仅用于人读与下载文件名。
	Name string `json:"name"`
	// Format 决定 Content-Type 与 spill_explore 可用的操作。
	Format Format `json:"format"`
}

// info 把文件头与 os.FileInfo 合成对外的 Info（Size 只算 payload）。
func (h rawHeader) info(id string, fi os.FileInfo, payloadOff int64) Info {
	return Info{
		ID: id, Name: h.Name, Format: h.Format,
		Size: fi.Size() - payloadOff, ModTime: fi.ModTime(), Shared: h.Shared,
	}
}

// maxRawNameBytes 限制写进文件头的名字长度：名字会进下载响应头与工具返回文案，
// 不设上限等于让调用方决定这两处有多大。
const maxRawNameBytes = 128

// safeName 清洗宿主给的名字：只留可打印 ASCII 里对文件名与响应头安全的字符。
//
// 它会进 Content-Disposition 的 filename="..."，回车/引号会撕开响应头。
func safeName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
		if b.Len() >= maxRawNameBytes {
			break
		}
	}
	if b.Len() == 0 {
		return "spill"
	}
	return b.String()
}

// PutFor 与 Put 相同，但把内容绑定到某个调用主体：只有同一个 token 用途名
// （主体有身份时还要求同一个人）才能下载，与中间件自动落盘的属主判定一致。
//
// 在工具处理函数里能拿到 *runtime.Call 时优先用它——那时「这份内容是谁的」是确定的。
func PutFor(sub *runtime.Subject, name string, f Format, data []byte) (id string, err error) {
	own := ownerOfSubject(sub)
	w, err := create(name, f, &own)
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

// CreateFor 与 Create 相同，但把内容绑定到某个调用主体（见 PutFor）。
func CreateFor(sub *runtime.Subject, name string, f Format) (*Writer, error) {
	own := ownerOfSubject(sub)
	return create(name, f, &own)
}

// create 是 Create / CreateFor 的共同实现：取当前 store、建文件、写头部。
func create(name string, f Format, own *fileOwner) (*Writer, error) {
	st, err := storeOrErr()
	if err != nil {
		return nil, err
	}
	if !f.valid() {
		return nil, fmt.Errorf("spill 格式 %q 不认识（只接受 json / jsonl / text）", f)
	}
	return st.createRaw(safeName(name), f, own)
}

// createRaw 建一个 raw 文件并写入头部，返回可继续写 payload 的 Writer。
func (s *store) createRaw(name string, f Format, own *fileOwner) (*Writer, error) {
	id := runtime.NewOwnedID()
	if !idPattern.MatchString(id) {
		return nil, fmt.Errorf("生成的 spill id 形状不合法: %q", id)
	}
	hdr := rawHeader{Name: name, Format: f, Shared: own == nil}
	if own != nil {
		hdr.Owner = *own
	}
	line, err := json.Marshal(hdr)
	if err != nil {
		return nil, fmt.Errorf("spill 序列化文件头: %w", err)
	}
	file, err := s.createFile(id + rawExt)
	if err != nil {
		return nil, fmt.Errorf("spill 建文件: %w", err)
	}
	if _, err := file.Write(append(line, '\n')); err != nil {
		_ = file.Close()
		_ = s.root.Remove(id + rawExt)
		return nil, fmt.Errorf("spill 写文件头: %w", err)
	}
	// 先记账再返回：写入是增量的，账本要在第一次 Write 之前就有这条记录，
	// 否则并发的 GC 会把它当成「别的进程留下的孤儿文件」（见 store.mine）。
	s.noteWrite(id, 0)
	return &Writer{st: s, id: id, name: name, f: file}, nil
}

// noteWrite 更新账本里某份 raw 内容的已写字节数。
func (s *store) noteWrite(id string, written int64) {
	s.mu.Lock()
	s.created[id] = written
	s.mu.Unlock()
}

// openContent 打开一份宿主写入的内容：返回文件句柄、（真实或合成的）文件头、
// payload 起始偏移与文件信息。
//
// 过期文件按「不存在」处理：TTL 是对使用方的承诺，不该取决于回收协程的周期。
// 中间件落盘的 <id>.json 信封不在这里的服务范围内——它是一次调用的完整结果，
// 由下载端点整份提供，探索它没有「边写边读」的语义。
func (s *store) openContent(id string) (*os.File, rawHeader, int64, os.FileInfo, error) {
	name, kind, format, ok := s.resolve(id)
	if !ok {
		return nil, rawHeader{}, 0, nil, fmt.Errorf("spill 内容 %s 不存在", id)
	}
	if kind == kindEnvelope {
		return nil, rawHeader{}, 0, nil,
			fmt.Errorf("spill 内容 %s 是中间件落盘的调用结果，请用下载地址整份取回", id)
	}
	f, err := s.openFile(name)
	if err != nil {
		return nil, rawHeader{}, 0, nil, err
	}
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || s.expired(fi.ModTime()) {
		_ = f.Close()
		if err == nil {
			err = fmt.Errorf("spill 内容 %s 不可用（已过期或不是普通文件）", id)
		}
		return nil, rawHeader{}, 0, nil, err
	}
	if kind == kindExtern {
		// 第三方写入方的文件里没有文件头，payload 从 0 开始（见 extern.go）。
		return f, externHeader(id, format), 0, fi, nil
	}
	hdr, off, err := readRawHeader(f)
	if err != nil {
		_ = f.Close()
		return nil, rawHeader{}, 0, nil, err
	}
	return f, hdr, off, fi, nil
}

// readRawHeader 读第一行的 JSON 头，返回头部与 payload 起始偏移。
//
// 用有界读：头部是本插件自己写的、正常几百字节，限制它是为了不让一个被篡改的
// 文件把一次读拖成整份大文件的扫描（与 readOwner 同一理由）。
func readRawHeader(f *os.File) (rawHeader, int64, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return rawHeader{}, 0, err
	}
	br := bufio.NewReaderSize(io.LimitReader(f, ownerHeaderLimit), ownerHeaderLimit)
	line, err := br.ReadBytes('\n')
	if err != nil {
		return rawHeader{}, 0, fmt.Errorf("spill 读文件头: %w", err)
	}
	var hdr rawHeader
	if err := json.Unmarshal(line[:len(line)-1], &hdr); err != nil {
		return rawHeader{}, 0, fmt.Errorf("spill 文件头不是合法 JSON: %w", err)
	}
	if !hdr.Format.valid() {
		return rawHeader{}, 0, fmt.Errorf("spill 文件头声明了未知格式 %q", hdr.Format)
	}
	return hdr, int64(len(line)), nil
}
