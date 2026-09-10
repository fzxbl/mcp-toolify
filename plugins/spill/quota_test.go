package spill

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fzxbl/mcp-toolify/runtime"
)

// textResult 造一个含指定大小文本的结果。
func textResult(n int) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: strings.Repeat("q", n)}},
	}
}

// storeWithQuota 在临时目录上造一个带上限的 store。
func storeWithQuota(t *testing.T, q quota) *store {
	t.Helper()
	st, err := newStore(t.TempDir(), time.Hour, time.Hour, q)
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	t.Cleanup(st.close)
	return st
}

// countFiles 统计落盘目录里的 spill 文件个数。
func countFiles(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	n := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), fileExt) {
			n++
		}
	}
	return n
}

// TestPutRejectsOversizedFile 覆盖 I3 的单文件上限：超上限的结果不落盘，
// 且不留半截文件（半截 JSON 下载下来是不可解析的）。
func TestPutRejectsOversizedFile(t *testing.T) {
	st := storeWithQuota(t, quota{MaxFileBytes: 4 << 10, MaxTotalBytes: 1 << 20})
	_, _, err := st.put("a.read", textResult(100<<10), testOwner)
	if err == nil {
		t.Fatal("超过单文件上限时 put 必须失败")
	}
	if !strings.Contains(err.Error(), "上限") {
		t.Errorf("错误应说明是上限问题: %v", err)
	}
	if n := countFiles(t, st.dir); n != 0 {
		t.Errorf("落盘目录残留 %d 个文件，半截文件必须删掉", n)
	}
}

// TestTotalQuotaEvictsOldest 覆盖 I3 的总量上限：磁盘不是无底洞。
// 默认 ttl=30m 之内，一个能连续吐大结果的工具足够把盘写满；盘满之后
// on_error=deny 会把每次大结果调用都变成错误，从磁盘问题升级成服务不可用。
func TestTotalQuotaEvictsOldest(t *testing.T) {
	st := storeWithQuota(t, quota{MaxFileBytes: 8 << 10, MaxTotalBytes: 12 << 10})
	var ids []string
	for i := 0; i < 6; i++ {
		id, _, err := st.put("a.read", textResult(4<<10), testOwner)
		if err != nil {
			t.Fatalf("put #%d: %v", i, err)
		}
		ids = append(ids, id)
		// 让 mtime 严格递增，淘汰顺序才是确定的。
		p, _ := st.pathFor(id)
		mod := time.Now().Add(time.Duration(i) * time.Second)
		if err := os.Chtimes(p, mod, mod); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
	}
	st.recount(nil)
	if used := st.usage(); used > 12<<10 {
		t.Errorf("总量 %d 超过上限 %d", used, 12<<10)
	}
	// 最新的一定还在，最旧的一定已经被淘汰。
	if p, _ := st.pathFor(ids[len(ids)-1]); !exists(p) {
		t.Error("最新落盘的文件被淘汰了")
	}
	if p, _ := st.pathFor(ids[0]); exists(p) {
		t.Error("最旧的文件没有被淘汰")
	}
}

// TestTotalQuotaFailsWhenNothingToEvict：腾不出空间时必须显式失败（交给 on_error
// 策略），而不是把盘写爆。
func TestTotalQuotaFailsWhenNothingToEvict(t *testing.T) {
	st := storeWithQuota(t, quota{MaxFileBytes: 64 << 10, MaxTotalBytes: 2 << 10})
	_, _, err := st.put("a.read", textResult(8<<10), testOwner)
	if err == nil {
		t.Fatal("单份结果就超过总量上限时必须失败")
	}
	if n := countFiles(t, st.dir); n != 0 {
		t.Errorf("失败后残留 %d 个文件", n)
	}
}

// TestQuotaConfigDefaultsAndValidation：上限有默认值、负数报错、
// 单文件上限大于总量上限报错（那样任何一次大结果都落不下来）。
func TestQuotaConfigDefaultsAndValidation(t *testing.T) {
	o := okOptions(t, Section{})
	if o.Quota.MaxFileBytes != defaultMaxFileMiB<<20 ||
		o.Quota.MaxTotalBytes != defaultMaxTotalMiB<<20 {
		t.Errorf("默认上限 = %+v", o.Quota)
	}
	bad := []Section{
		{MaxFileMiB: -1},
		{MaxTotalMiB: -1},
		{MaxFileMiB: 100, MaxTotalMiB: 1},
		// 大到换算成字节就溢出 int64：宁可启动失败，也不要把负数上限带上线
		// （负的上限会被 store 当成「不限」）。
		{MaxTotalMiB: math.MaxInt64>>20 + 1},
	}
	for _, s := range bad {
		if _, err := s.normalize(); err == nil {
			t.Errorf("%+v 必须报错", s)
		}
	}
}

// TestQuotaMiBUnitsConvertToBytes：磁盘上限以 MiB 为单位配置，内部换算成字节。
//
// 为什么单位是 MiB 而不是字节：这两项是容量决策（「这个部署最多占多少盘」），
// 写成 67108864 得人心算才知道是 64MiB，改的时候也容易多打或少打一个 0。
// 而 threshold_bytes 仍是字节——它要和「一份结果多大」对齐，量级在 KiB。
func TestQuotaMiBUnitsConvertToBytes(t *testing.T) {
	o := okOptions(t, Section{MaxFileMiB: 3, MaxTotalMiB: 7})
	if o.Quota.MaxFileBytes != 3<<20 || o.Quota.MaxTotalBytes != 7<<20 {
		t.Errorf("MiB 没有换算成字节: %+v", o.Quota)
	}
}

// exists 判断路径是否存在。
func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// TestGCLeavesOtherProcessFiles 覆盖 I5：落盘目录可能被两个进程共享（默认目录在
// 同机多副本下天然共享），各自 ttl 不同。审查实测：A 的 ttl=1m 扫一遍就删掉了 B 那份
// ttl=24h、只落了 2 小时的文件。GC 只能回收「自己的」文件。
func TestGCLeavesOtherProcessFiles(t *testing.T) {
	withBaseURL(t, "") // 无对外地址：id 不带副本归属信息，最难判断归属的那一档
	dir := t.TempDir()
	long, err := newStore(dir, 24*time.Hour, time.Hour, quota{})
	if err != nil {
		t.Fatalf("newStore B: %v", err)
	}
	t.Cleanup(long.close)
	short, err := newStore(dir, time.Minute, time.Hour, quota{})
	if err != nil {
		t.Fatalf("newStore A: %v", err)
	}
	t.Cleanup(short.close)

	peerID, _, err := long.put("b.read", textResult(1024), testOwner)
	if err != nil {
		t.Fatalf("put B: %v", err)
	}
	peerPath, _ := long.pathFor(peerID)
	twoHoursAgo := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(peerPath, twoHoursAgo, twoHoursAgo); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	ownID, _, err := short.put("a.read", textResult(1024), testOwner)
	if err != nil {
		t.Fatalf("put A: %v", err)
	}
	ownPath, _ := short.pathFor(ownID)
	if err := os.Chtimes(ownPath, twoHoursAgo, twoHoursAgo); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	short.gcOnce()

	if !exists(peerPath) {
		t.Error("另一个进程仍在用的文件被 GC 删了（它的 ttl 是 24h）")
	}
	if exists(ownPath) {
		t.Error("自己的过期文件没被回收（GC 不能因为怕误删而什么都不删）")
	}
}

// TestGCLeavesOtherReplicaFiles：id 内嵌的副本地址不是本副本时，无论多旧都不回收。
// 这是多副本共享目录时的第二条判据（第一条是「本实例创建的」）。
func TestGCLeavesOtherReplicaFiles(t *testing.T) {
	withBaseURL(t, "http://127.0.0.1:18011")
	st := newTestStore(t, time.Minute, time.Hour)

	runtime.SetSelfAddr("10.9.9.9:18011")
	peerID := runtime.NewOwnedID()
	runtime.SetSelfAddr("127.0.0.1:18011")

	path := filepath.Join(st.dir, peerID+fileExt)
	if err := os.WriteFile(path, []byte(`{"owner":{"token":"x"}}`), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	longAgo := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(path, longAgo, longAgo); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	st.gcOnce()

	if !exists(path) {
		t.Error("归属其它副本的文件被 GC 删了")
	}
}

// TestConcurrentPutsAndGC 覆盖 M4 的并发盲区：多个请求同时落盘、GC 同时在扫，
// 账本与文件操作不能打架（-race 下跑）。
func TestConcurrentPutsAndGC(t *testing.T) {
	st := storeWithQuota(t, quota{MaxFileBytes: 1 << 20, MaxTotalBytes: 8 << 20})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				if _, _, err := st.put("a.read", textResult(4096), testOwner); err != nil {
					t.Errorf("put: %v", err)
					return
				}
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				st.gcOnce()
			}
		}()
	}
	wg.Wait()
	if st.usage() < 0 {
		t.Errorf("账本被并发写坏了: %d", st.usage())
	}
}

// TestSpillFileStructuredContentMatchesWire 覆盖 M6：落盘文件里的
// structuredContent 必须与线上返回体形状一致。SDK 把 []byte 序列化成 base64
// 字符串，所以不能把 []byte 当 JSON 原文透写。
func TestSpillFileStructuredContentMatchesWire(t *testing.T) {
	st := newTestStore(t, time.Hour, time.Hour)
	// 刻意用**合法 JSON 的字节串**：SDK 对 []byte 的 wire 形状是 base64 字符串，
	// 而不是把这段字节当 JSON 原文透写。用一段非法 JSON 的话，jsonRawOf 里的
	// json.Valid 会先兜住，验不到「只认 json.RawMessage」这条（审查 M6）。
	raw := []byte(`{"looks":"like json"}`)
	id, _, err := st.put("a.read", &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: "hi"}},
		StructuredContent: raw,
	}, testOwner)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	path, _ := st.pathFor(id)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var got struct {
		StructuredContent json.RawMessage `json:"structuredContent"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("落盘内容不是合法 JSON: %v", err)
	}
	want, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(got.StructuredContent) != string(want) {
		t.Errorf("structuredContent = %s，want %s（与 SDK 的 wire 形状一致）",
			got.StructuredContent, want)
	}
}

// TestMetaIDReadableByOuterPlugin 覆盖 M2：spill 把落盘 id 写进 Call.Meta，
// 外层插件（在 next 返回之后）必须能读到它，否则这个键就是个没人能用的装饰。
func TestMetaIDReadableByOuterPlugin(t *testing.T) {
	withBaseURL(t, "http://127.0.0.1:8011")
	st := newTestStore(t, time.Hour, time.Hour)
	var seen string
	outer := func(next runtime.Handler) runtime.Handler {
		return func(ctx context.Context, c *runtime.Call) (*runtime.Result, error) {
			res, err := next(ctx, c)
			seen, _ = c.Meta[MetaID].(string)
			return res, err
		}
	}
	h := runtime.Chain([]runtime.Middleware{outer, middleware(okOptions(t, Section{}), st)},
		handlerReturning(&runtime.Result{Tool: textResult(100 << 10)}))
	res, err := h(context.Background(), textCall("a.read"))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if seen == "" {
		t.Fatal("外层插件读不到 Call.Meta[MetaID]")
	}
	if !strings.Contains(resultText(t, res), seen) {
		t.Errorf("Meta 里的 id %q 与摘要里的不一致", seen)
	}
}

// TestPutPropagatesMidWriteIOError 覆盖复审 M-2：写**中途**的 I/O 失败必须原样冒出。
//
// 原来的守卫（TestSpillWriteFailureIsExplicit）错在「建文件」阶段，把
// `if err := writeSpillJSON(...); err == nil { err = w.w.Flush() }` 这种遮蔽具名
// 返回值的写法放回去，它照样绿 —— 而那恰好是本插件出过的真 bug（写失败被吞掉、
// put 报成功、摘要指向一份坏文件）。这里注入一个「能建、但写必然失败」的句柄，
// 直接钉住这条不变量。
func TestPutPropagatesMidWriteIOError(t *testing.T) {
	st := storeWithQuota(t, quota{})
	// 以只读方式建出目标文件：文件真的存在，但任何写都会 EBADF。
	st.openForWrite = func(name string) (*os.File, error) {
		return st.root.OpenFile(name, os.O_RDONLY|os.O_CREATE|os.O_EXCL, filePerm)
	}

	id, n, err := st.put("a.read", textResult(64<<10), testOwner)
	if err == nil {
		t.Fatalf("写中途 I/O 失败必须报错，实际 id=%q size=%d", id, n)
	}
	if !strings.Contains(err.Error(), "spill 写文件") {
		t.Errorf("错误没带落盘上下文: %v", err)
	}
	if id != "" || n != 0 {
		t.Errorf("失败时不得返回可用的 id/大小: id=%q size=%d", id, n)
	}
	// 半截文件必须被清掉，账本里也不能留残渣。
	if got := countFiles(t, st.dir); got != 0 {
		t.Errorf("落盘目录残留 %d 个文件，写失败后应清空", got)
	}
	st.mu.Lock()
	left := len(st.created)
	st.mu.Unlock()
	if left != 0 {
		t.Errorf("账本残留 %d 条记录", left)
	}
}
