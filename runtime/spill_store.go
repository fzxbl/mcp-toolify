package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"
)

// SpillFormat 是 spill 文件声明的内容格式。
type SpillFormat string

const (
	FormatJSON  SpillFormat = "json"  // .json
	FormatJSONL SpillFormat = "jsonl" // .jsonl
	FormatText  SpillFormat = "text"  // .txt
)

// ext 返回该格式对应的文件扩展名。
func (f SpillFormat) ext() string {
	switch f {
	case FormatJSONL:
		return ".jsonl"
	case FormatText:
		return ".txt"
	default:
		return ".json"
	}
}

// mime 返回该格式对应的 MIME 类型。
func (f SpillFormat) mime() string {
	switch f {
	case FormatJSONL:
		return "application/x-ndjson"
	case FormatText:
		return "text/plain; charset=utf-8"
	default:
		return "application/json"
	}
}

// spillTTLConfig 描述磁盘 spill 文件的各类存活时间及 GC 周期。
type spillTTLConfig struct {
	Generic  time.Duration
	Sysprobe time.Duration
	GC       time.Duration
}

// diskSpillStore 是磁盘化的 spill store，把工具大返回值落到磁盘文件而非内存。
type diskSpillStore struct {
	dir string
	ttl spillTTLConfig
	mu  sync.Mutex
}

// newDiskSpillStore 创建磁盘 spill store，并确保目录存在。
func newDiskSpillStore(dir string, ttl spillTTLConfig) *diskSpillStore {
	_ = os.MkdirAll(dir, 0755)
	return &diskSpillStore{dir: dir, ttl: ttl}
}

// create 返回 id 及应写入的文件路径，文件名 <tool>-<id><ext>。
func (s *diskSpillStore) create(toolName string, f SpillFormat) (id, path string) {
	id = newSpillID()
	path = filepath.Join(s.dir, toolName+"-"+id+f.ext())
	return
}

// resolve 按 *-<id>.* 定位文件。一个 id 可能对应多个文件（例如 logit 会给
// <base>.log 额外写一个 <base>.log.wf 兄弟文件），此时返回后缀段最少、名字最短的
// 那个（即主文件，如 .log 而非 .log.wf），保证 spill_explore 读到的是主内容。
func (s *diskSpillStore) resolve(id string) (string, bool) {
	matches, _ := filepath.Glob(filepath.Join(s.dir, "*-"+id+".*"))
	if len(matches) == 0 {
		return "", false
	}
	best := matches[0]
	for _, m := range matches[1:] {
		bb, mb := filepath.Base(best), filepath.Base(m)
		if bd, md := strings.Count(bb, "."), strings.Count(mb, "."); md < bd || (md == bd && len(mb) < len(bb)) {
			best = m
		}
	}
	return best, true
}

// spillFormatOf 用反射推导：底层为 slice/array 且元素非 byte → jsonl；否则 json。
func spillFormatOf(out any) SpillFormat {
	rv := reflect.ValueOf(out)
	for rv.IsValid() && (rv.Kind() == reflect.Ptr || rv.Kind() == reflect.Interface) {
		rv = rv.Elem()
	}
	if rv.IsValid() &&
		(rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array) &&
		rv.Type().Elem().Kind() != reflect.Uint8 {
		return FormatJSONL
	}
	return FormatJSON
}

// marshalSpill 按格式序列化：jsonl 逐元素一行；json 整体。
func marshalSpill(out any, f SpillFormat) ([]byte, error) {
	if f != FormatJSONL {
		return json.Marshal(out)
	}
	rv := reflect.ValueOf(out)
	for rv.Kind() == reflect.Ptr || rv.Kind() == reflect.Interface {
		rv = rv.Elem()
	}
	var buf bytes.Buffer
	for i := 0; i < rv.Len(); i++ {
		b, err := json.Marshal(rv.Index(i).Interface())
		if err != nil {
			return nil, err
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	return buf.Bytes(), nil
}

// ttlFor 按文件名前缀区分类别 TTL：sysprobe.* 用 Sysprobe，其余 Generic。
func (s *diskSpillStore) ttlFor(name string) time.Duration {
	if strings.HasPrefix(filepath.Base(name), "sysprobe.") {
		return s.ttl.Sysprobe
	}
	return s.ttl.Generic
}

// expired 判断文件是否超过其类别 TTL。
func (s *diskSpillStore) expired(name string, info os.FileInfo) bool {
	return time.Since(info.ModTime()) > s.ttlFor(name)
}

// gcOnce 遍历目录一次，删除已过期的 spill 文件。
func (s *diskSpillStore) gcOnce() {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, _ := os.ReadDir(s.dir)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		p := filepath.Join(s.dir, e.Name())
		if s.expired(p, info) {
			_ = os.Remove(p)
		}
	}
}

// gcLoop 按 GC 周期循环执行 gcOnce，直到 ctx 取消。
func (s *diskSpillStore) gcLoop(ctx context.Context) {
	t := time.NewTicker(s.ttl.GC)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.gcOnce()
		}
	}
}

// reconcile 把 *.meta.json 中 status=running 改为 interrupted（重启后其 goroutine 已消失）。
func (s *diskSpillStore) reconcile() {
	matches, _ := filepath.Glob(filepath.Join(s.dir, "*.meta.json"))
	for _, m := range matches {
		b, err := os.ReadFile(m)
		if err != nil {
			continue
		}
		if strings.Contains(string(b), `"status":"running"`) {
			nb := strings.Replace(string(b), `"status":"running"`, `"status":"interrupted"`, 1)
			_ = os.WriteFile(m, []byte(nb), 0644)
		}
	}
}

var (
	globalSpillStore *diskSpillStore
	spillInitOnce    sync.Once
)

// InitSpillStore 幂等初始化磁盘 store：建目录 + reconcile + 启动 GC。
// dir 为空时用 <os.TempDir>/mcp-toolify/spill。TTL 各项为 0 时使用默认值。
func InitSpillStore(ctx context.Context, dir string, ttl spillTTLConfig) {
	spillInitOnce.Do(func() {
		if dir == "" {
			dir = filepath.Join(os.TempDir(), "mcp-toolify", "spill")
		}
		if ttl.Generic == 0 {
			ttl.Generic = 30 * time.Minute
		}
		if ttl.Sysprobe == 0 {
			ttl.Sysprobe = 2 * time.Hour
		}
		if ttl.GC == 0 {
			ttl.GC = 5 * time.Minute
		}
		globalSpillStore = newDiskSpillStore(dir, ttl)
		globalSpillStore.reconcile()
		go globalSpillStore.gcLoop(ctx)
	})
}

// spillStoreOrDefault 保证在未显式 Init 时也有可用 store（stdio/测试场景）。
func spillStoreOrDefault() *diskSpillStore {
	if globalSpillStore == nil {
		InitSpillStore(context.Background(), "", spillTTLConfig{})
	}
	return globalSpillStore
}

// setSpillStoreForTest 供同包测试直接注入 store，绕过 sync.Once。
func setSpillStoreForTest(s *diskSpillStore) {
	globalSpillStore = s
}

// ResolveSpillPath 供外部工具包（如 spill_explore）按 id 定位 spill 文件路径。
func ResolveSpillPath(id string) (string, bool) {
	return spillStoreOrDefault().resolve(id)
}

// SpillDownloadURLFor 返回该 id 可选的直连下载 URL；未配置对外地址时为空串。
func SpillDownloadURLFor(id string) string {
	if base := getSpillBaseURL(); base != "" {
		return base + spillDownloadPath + id
	}
	return ""
}

// NewSpillID 导出 id 生成，供 sysprobe 等外部包复用同一套 id 约定。
func NewSpillID() string {
	return newSpillID()
}
