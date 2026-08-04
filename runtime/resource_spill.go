package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// 本文件实现"工具大返回值走 MCP Resource"的运行时支撑：
//
// 生成的 handler 成功路径统一调用 MaybeSpill：当结果估算 token 数超过配置阈值
// （conf/mcp/mcp.toml 的 [spill] max_result_tokens）时，不再把完整结果塞进
// structuredContent，而是按格式序列化后落到磁盘文件，返回一段简短摘要 +
// 一个 ResourceLink（spill://<id>）。客户端按需通过 resources/read 读取完整内容，
// 从而避免大 payload 直接撑爆上下文。
//
// 资源通过一个 ResourceTemplate（spill://{id}）暴露，由 RegisterSpillResource
// 在 server 启动时注册；磁盘 store 带 TTL，由后台 GC 协程定期回收（见 spill_store.go）。

const spillScheme = "spill"

// spillBaseURL 是 agent 侧可直连的下载基础地址（不含末尾斜杠），
// 形如 http://host:8011。由 runHTTP 在启动时通过 SetSpillBaseURL 设置。
// 为空时 SpillResult 不吐出下载 URL（例如 stdio 传输下没有 HTTP 端点）。
var (
	spillBaseMu  sync.RWMutex
	spillBaseURL string
)

// SetSpillBaseURL 设置 spill 下载端点的对外基础地址。跨机部署时应传入
// agent 可直连的地址；传空串表示当前传输不提供 HTTP 下载（如 stdio）。
func SetSpillBaseURL(base string) {
	spillBaseMu.Lock()
	defer spillBaseMu.Unlock()
	spillBaseURL = strings.TrimRight(base, "/")
}

func getSpillBaseURL() string {
	spillBaseMu.RLock()
	defer spillBaseMu.RUnlock()
	return spillBaseURL
}

// spillDownloadPath 是 HTTP 下载端点的路由前缀。
const spillDownloadPath = "/spill/"

// newSpillID 生成 16 字节随机 id 的十六进制串，供磁盘 store 的 create 复用。
func newSpillID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// SpillResult 把工具返回值按推导出的格式（json/jsonl）序列化并落到磁盘文件，
// 返回一个只含摘要 + ResourceLink 的 CallToolResult。toolName 用于文件名与摘要文案。
//
// 无条件落盘，供手写工具（sysprobe / terminal_* 等）在明确知道结果很大时直接调用。
// 生成的 handler 走 MaybeSpill，按体积自动判定。
func SpillResult(toolName string, out any) (*mcp.CallToolResult, error) {
	f := spillFormatOf(out)
	data, err := marshalSpill(out, f)
	if err != nil {
		return nil, fmt.Errorf("spill marshal: %w", err)
	}
	return writeSpill(toolName, data, f)
}

// MaybeSpill 按结果体积决定返回形态：估算 token 数超过配置阈值时落盘为 spill 资源，
// 否则原样返回 structuredContent。
//
// raw 是原始返回值（用于 json/jsonl 格式推导与体积测量），structured 是打包后的
// structuredContent（形如 map[string]any{"result": raw}）。序列化失败时记日志后
// 退回内联路径交给 SDK 处理，保持与旧行为一致；落盘失败时同样降级为内联返回，
// 不影响工具调用成功——spill 只是上下文体积优化，不该把成功的调用变成失败。
//
// 因此第三个返回值目前恒为 nil，仅保留签名位以便将来扩展（生成的 handler 依赖
// 这个三返回值形状）。生成的 handler 以 `return MaybeSpill(...)` 形式调用它，
// 工具函数本身不受影响。
func MaybeSpill(toolName string, raw any, structured any) (*mcp.CallToolResult, any, error) {
	threshold := spillThreshold()
	if threshold < 0 {
		return nil, structured, nil
	}
	f := spillFormatOf(raw)
	data, err := marshalSpill(raw, f)
	if err != nil {
		log.Printf("[mcp] %s spill marshal failed: %v, fall back to inline", toolName, err)
		return nil, structured, nil
	}
	if estimateTokens(data) <= threshold {
		return nil, structured, nil
	}
	res, err := writeSpill(toolName, data, f)
	if err != nil {
		log.Printf("[mcp] %s spill write failed: %v, fall back to inline", toolName, err)
		return nil, structured, nil
	}
	return res, nil, nil
}

// writeSpill 把已序列化的数据落盘，返回摘要文本 + ResourceLink 的 CallToolResult。
//
// 若已通过 SetSpillBaseURL 配置了对外基础地址（HTTP 传输），摘要中还会给出
// 一个可直连下载的 http(s) URL，供带 shell/沙盒的 agent 用 curl 等下载后在本地处理，
// 全程不把完整数据塞进上下文。
func writeSpill(toolName string, data []byte, f SpillFormat) (*mcp.CallToolResult, error) {
	st := spillStoreOrDefault()
	id, path := st.create(toolName, f)
	if err := os.WriteFile(path, data, 0644); err != nil {
		return nil, fmt.Errorf("spill write: %w", err)
	}
	uri := spillScheme + "://" + id
	size := int64(len(data))

	var b strings.Builder
	fmt.Fprintf(&b, "%s 的完整结果较大（%d 字节，格式 %s），已存为临时资源 %s。\n", toolName, size, f, uri)
	fmt.Fprintf(&b, "建议用 spill_explore 工具（read/grep/schema/jq）按需探索该资源，避免完整数据进入上下文。\n")
	if base := getSpillBaseURL(); base != "" {
		fmt.Fprintf(&b, "如需原始文件，也可直接下载：%s%s%s\n", base, spillDownloadPath, id)
	}

	link := &mcp.ResourceLink{
		URI: uri, Name: "result", Title: toolName + " 完整结果",
		Description: "用 spill_explore 探索或直接读取", MIMEType: f.mime(), Size: &size,
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: b.String()}, link}}, nil
}

// SpillDownloadHandler 返回一个 http.Handler，按 /spill/<id> 路径把此前
// SpillResult 落盘的文件直接作为可下载内容返回。挂到 MCP server 同一个 HTTP
// 端口上，供跨机 agent 直连下载。未命中/过期返回 404。
func SpillDownloadHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, spillDownloadPath)
		if id == "" || strings.Contains(id, "/") {
			http.NotFound(w, r)
			return
		}
		path, ok := spillStoreOrDefault().resolve(id)
		if !ok {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, path)
	})
}

// RegisterSpillResource 注册 spill://{id} 资源模板，使客户端可以 resources/read
// 读取此前 SpillResult 落盘的内容。由 runtime.Run 在 server 启动时调用。
func RegisterSpillResource(s *mcp.Server) {
	s.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: spillScheme + "://{id}",
		Name:        "spill",
		Title:       "工具大返回结果的临时资源",
		Description: "返回体积超过阈值的工具会把完整结果存为此类资源，按需读取以节省上下文。",
		MIMEType:    "application/json",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		uri := req.Params.URI
		id := strings.TrimPrefix(uri, spillScheme+"://")
		path, ok := spillStoreOrDefault().resolve(id)
		if !ok {
			return nil, mcp.ResourceNotFoundError(uri)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, mcp.ResourceNotFoundError(uri)
		}
		mime := "application/json"
		if strings.HasSuffix(path, ".jsonl") {
			mime = "application/x-ndjson"
		} else if strings.HasSuffix(path, ".txt") {
			mime = "text/plain; charset=utf-8"
		}
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: uri, MIMEType: mime, Text: string(data)}}}, nil
	})
}
