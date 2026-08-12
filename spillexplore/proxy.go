package spillexplore

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/fzxbl/mcp-toolify/runtime"
)

func init() {
	// 注入 runtime 的“本地-only explore”执行器，供内部 /spill-explore 端点调用。
	runtime.LocalSpillExplore = exploreLocal
}

// exploreLocal 只在本地文件上执行 explore（resolve 命中本地才有结果），绝不代理。
func exploreLocal(id, op string, lineOffset, limit int, pattern, jqExpr string, depth, maxBytes int) (map[string]any, error) {
	path, ok := runtime.ResolveSpillPath(id)
	if !ok {
		return nil, fmt.Errorf("spill resource not found: %s", id)
	}
	return exploreAt(path, id, op, lineOffset, limit, pattern, jqExpr, depth, maxBytes)
}

// proxyExplore 把 explore 调用单跳转发到归属副本的内部端点，回传其小结果。
func proxyExplore(ownerHostPort, id, op string, lineOffset, limit int, pattern, jqExpr string, depth, maxBytes int) (map[string]any, error) {
	token := runtime.SpillPeerToken()
	if token == "" {
		return nil, fmt.Errorf("spill resource not found: %s (remote owner but cross-replica proxy disabled)", id)
	}
	reqBody, _ := json.Marshal(map[string]any{
		"id": id, "op": op, "lineOffset": lineOffset, "limit": limit,
		"pattern": pattern, "jqExpr": jqExpr, "depth": depth, "maxBytes": maxBytes,
	})
	url := "http://" + ownerHostPort + "/spill-explore"
	client := &http.Client{Timeout: runtime.SpillPeerTimeout()}

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ { // 1 次重试
		req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(reqBody))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Spill-Peer-Token", token)
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("spill resource not found: %s (owner %s status %d)", id, ownerHostPort, resp.StatusCode)
		}
		var res map[string]any
		derr := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&res)
		resp.Body.Close()
		if derr != nil {
			return nil, fmt.Errorf("decode proxied explore result: %w", derr)
		}
		return res, nil
	}
	return nil, fmt.Errorf("spill resource not found: %s (owner %s unreachable: %v)", id, ownerHostPort, lastErr)
}
