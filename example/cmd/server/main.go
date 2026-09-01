// Command server 是 mcp-toolify 的完整组装示例：基座 + 唯一的内置插件 spill
// （大结果落盘）。
//
// 审计（audit）、配额（quota）、二次确认（confirm）都已移出本仓库，作为**外置插件**
// 的范例——任何插件都必须能以外部 module 的形式装进来，否则基座就悄悄给内置插件开了
// 特例。装它们的方式与这里一样：import 该 module 后按「外 → 内」的顺序各调一次
// Install（推荐顺序见 README 的「插件顺序」一节）。
//
// 改了工具函数后重新生成 wrapper（生成物不入库，构建前现场生成）：
//
//	go generate ./example/...
//
// 运行（token 鉴权是强制的，因此必须给配置文件）：
//
//	go run ./example/cmd/server -addr :8011 -config ./example/conf/mcp.toml
//
// 试一次只读调用：
//
//	curl -sS localhost:8011 -H 'Authorization: Bearer replace-me-readonly' \
//	  -H 'Content-Type: application/json' \
//	  -H 'Accept: application/json, text/event-stream' \
//	  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call",
//	       "params":{"name":"greeter.greet","arguments":{"name":"world","excited":true}}}'
package main

import (
	"context"
	"flag"
	"log"

	toolify "github.com/fzxbl/mcp-toolify"
	"github.com/fzxbl/mcp-toolify/example/tools"
	"github.com/fzxbl/mcp-toolify/plugins/spill"
	"github.com/fzxbl/mcp-toolify/runtime"
)

// build 装配基座与插件，返回可 Start / Handlers 的 Registry。
//
// **安装顺序就是洋葱链的进入顺序（外 → 内）**。spill 必须最内：它要看到工具返回的
// **原始**结果才能按真实大小判定是否落盘，外面还有插件改写结果时就判不准了。
//
// 装外置插件时的相对位置同样不是任意的（都是外置插件自己的文档该说清的事，这里给出
// 本仓库推荐的顺序）：审计最外——被后面任何插件拒掉的调用也要留下记录；阻塞式的二次
// 确认要在配额之外——还在等人确认的调用什么都没执行，不该已经占掉额度；配额在 spill
// 之外——额度扣在真正要执行的那一刻。
//
// 与 main 分开是为了让端到端冒烟测试能复用同一份装配——「示例能不能起来」这件事
// 只有真的装一遍才算验证过。
func build(cfg runtime.Config) (*runtime.Registry, error) {
	r := toolify.New(cfg, tools.RegisterAll)
	for _, install := range []func(*runtime.Registry) error{
		spill.Install,
	} {
		if err := install(r); err != nil {
			return r, err
		}
	}
	return r, nil
}

func main() {
	addr := flag.String("addr", ":8011", "http listen addr")
	config := flag.String("config", "./example/conf/mcp.toml",
		"path to the TOML config (must contain [[tokens]])")
	public := flag.String("public-base-url", "",
		"this replica's directly reachable base URL, e.g. http://10.1.2.3:8011")
	flag.Parse()

	r, err := build(runtime.Config{Addr: *addr, ConfigPath: *config, PublicBaseURL: *public})
	if err != nil {
		log.Fatalf("装配插件失败: %v", err)
	}
	if err := r.Start(context.Background()); err != nil {
		log.Fatalf("mcp-toolify server exited: %v", err)
	}
}
