package runtime

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestOnBuildHooksRunOnceBeforeServing：build 期钩子必须在「组装完成、开始接流之前」
// 恰好跑一次，且按注册顺序执行。
//
// 「恰好一次」的判据是 Handlers 幂等（buildOnce）：钩子跑两遍意味着插件的启动期自检
// 也会重复执行（重复报错、重复起协程）。
func TestOnBuildHooksRunOnceBeforeServing(t *testing.T) {
	r := New(Config{ConfigPath: writeTokenConfig(t, okTokenConfig)}, nil)
	var order []string
	r.OnBuild(func() error { order = append(order, "first"); return nil })
	r.OnBuild(func() error { order = append(order, "second"); return nil })

	h, _, err := r.Handlers()
	if err != nil {
		t.Fatalf("Handlers: %v", err)
	}
	if h == nil {
		t.Fatal("钩子都通过时必须拿到 MCP handler")
	}
	if _, _, err := r.Handlers(); err != nil {
		t.Fatalf("Handlers 第二次: %v", err)
	}
	if strings.Join(order, ",") != "first,second" {
		t.Errorf("钩子执行序列 = %v，要求按注册顺序恰好各跑一次", order)
	}
}

// TestOnBuildErrorFailsStartup：钩子返回 error 必须让启动失败，且不交出 handler——
// 「注册时机晚于接流」这类空窗要真的堵在接流前，而不是只靠运行期兜底。
func TestOnBuildErrorFailsStartup(t *testing.T) {
	r := New(Config{ConfigPath: writeTokenConfig(t, okTokenConfig)}, nil)
	r.OnBuild(func() error { return fmt.Errorf("%s", "没有注册落地函数") })
	var laterRan bool
	r.OnBuild(func() error { laterRan = true; return nil })

	h, routes, err := r.Handlers()
	if err == nil {
		t.Fatal("build 期钩子报错必须让启动失败")
	}
	if !strings.Contains(err.Error(), "没有注册落地函数") {
		t.Errorf("错误里必须带上钩子给出的原因: %v", err)
	}
	if h != nil || routes != nil {
		t.Error("启动失败时不得交出 handler 或路由")
	}
	if laterRan {
		t.Error("首个钩子失败后必须立刻中止，不再跑后面的钩子")
	}
	// 缓存的失败结果同样不得在第二次调用时变成成功。
	if _, _, err := r.Handlers(); err == nil {
		t.Error("Handlers 幂等：失败结果必须被缓存")
	}
}

// TestOnBuildRunsAfterValidateAndConfigCheck：钩子必须排在 loadBase / 配置认领检查 /
// validate **之后**。插件的启动期自检（audit 的 sink、confirm 的 notifier）若跑在这些
// 检查之前，会用一个次要原因盖掉真正的启动失败原因。
func TestOnBuildRunsAfterValidateAndConfigCheck(t *testing.T) {
	cases := map[string]Config{
		"缺少必需插件": {ConfigPath: writeTokenConfig(t, okTokenConfig),
			RequiredPlugins: []string{"audit"}},
		"无人认领的配置项": {ConfigPath: writeTokenConfig(t,
			okTokenConfig+"\n[qouta]\nbackend = \"redis\"\n")},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			r := New(cfg, nil)
			var ran bool
			r.OnBuild(func() error { ran = true; return nil })
			if _, _, err := r.Handlers(); err == nil {
				t.Fatal("该配置必须启动失败")
			}
			if ran {
				t.Error("基座自身的校验没过时不该执行插件的 build 期钩子")
			}
		})
	}
}

// TestOnBuildSeesFinalPublicBaseURL：钩子要能看到最终生效的对外地址——
// 插件的启动期自检（如「多副本部署是否配了 PublicBaseURL」）依赖它已经定型。
func TestOnBuildSeesFinalPublicBaseURL(t *testing.T) {
	old := PublicBaseURL()
	t.Cleanup(func() { SetPublicBaseURL(old) })
	SetPublicBaseURL("")

	r := New(Config{ConfigPath: writeTokenConfig(t, okTokenConfig),
		PublicBaseURL: "http://10.1.2.3:8011"}, nil)
	var seen string
	r.OnBuild(func() error { seen = PublicBaseURL(); return nil })
	if _, _, err := r.Handlers(); err != nil {
		t.Fatalf("Handlers: %v", err)
	}
	if seen != "http://10.1.2.3:8011" {
		t.Errorf("钩子里看到的对外地址 = %q，要求已是最终值", seen)
	}
}

// TestOnBuildIgnoresNilHook：nil 钩子不得让启动 panic（宿主传进来一个 nil 函数只是
// 编码疏忽，不该表现成进程崩溃）。
func TestOnBuildIgnoresNilHook(t *testing.T) {
	r := New(Config{ConfigPath: writeTokenConfig(t, okTokenConfig)}, nil)
	r.OnBuild(nil)
	if _, _, err := r.Handlers(); err != nil {
		t.Fatalf("Handlers: %v", err)
	}
}

// TestOnBuildPanicBecomesStartupError：钩子 panic 必须变成启动失败。
//
// 为什么值得一条用例：build 用 sync.Once 缓存结果，而 Once.Do 在 f panic 时**也**会置
// done。若不 recover，第一次 Handlers() 把 panic 抛给宿主，宿主 recover 之后再调一次
// 就会拿到 (nil, nil) —— 一个「没有 handler 也没有错误」的启动成功，挂上去每个请求
// nil 解引用。启动必须 fail-closed。
func TestOnBuildPanicBecomesStartupError(t *testing.T) {
	r := New(Config{ConfigPath: writeTokenConfig(t, okTokenConfig)}, nil)
	r.OnBuild(func() error { panic("sink 表是 nil") })
	var laterRan bool
	r.OnBuild(func() error { laterRan = true; return nil })

	h, routes, err := func() (http.Handler, map[string]http.Handler, error) {
		defer func() {
			if p := recover(); p != nil {
				t.Fatalf("钩子 panic 不该打穿 Handlers: %v", p)
			}
		}()
		return r.Handlers()
	}()
	if err == nil {
		t.Fatal("钩子 panic 必须让启动失败")
	}
	if !strings.Contains(err.Error(), "panic") || !strings.Contains(err.Error(), "sink 表是 nil") {
		t.Errorf("错误里要能看出是钩子 panic 及其原因: %v", err)
	}
	if h != nil || routes != nil {
		t.Error("启动失败时不得交出 handler 或路由")
	}
	if laterRan {
		t.Error("panic 的钩子之后不该继续跑后面的钩子")
	}
	// 关键回归点：第二次调用必须仍然是错误，不能变成「无 handler 也无 error」。
	h2, _, err2 := r.Handlers()
	if err2 == nil {
		t.Fatal("第二次 Handlers 必须仍然报错（Once 已置 done，零值结果等于静默启动成功）")
	}
	if h2 != nil {
		t.Error("第二次 Handlers 不得交出 handler")
	}
}

// TestOnBuildHookMustNotMutateRegistry：钩子里做注册类调用必须启动失败。
//
// 此刻链还没装、但 plugin chain 日志已经打过，required_plugins 校验与配置认领检查也
// 都跑完了，所以这些调用**全是静默失效**（`r.Use` 甚至会真生效却不出现在启动日志里）。
func TestOnBuildHookMustNotMutateRegistry(t *testing.T) {
	cases := map[string]func(r *Registry){
		"Use":         func(r *Registry) { r.Use(func(next Handler) Handler { return next }) },
		"Named":       func(r *Registry) { r.Named("late-plugin") },
		"Route":       func(r *Registry) { r.Route("/late/", http.NotFoundHandler()) },
		"RoutePublic": func(r *Registry) { r.RoutePublic("/healthz", http.NotFoundHandler()) },
		"OnBuild":     func(r *Registry) { r.OnBuild(func() error { return nil }) },
		"Tool": func(r *Registry) {
			r.Tool(func(s *mcp.Server) {
				mcp.AddTool(s, &mcp.Tool{Name: "late.tool"},
					func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (
						*mcp.CallToolResult, any, error) {
						return nil, "ok", nil
					})
			})
		},
		"Config": func(r *Registry) {
			var cfg struct {
				Log LogConfig `toml:"log"`
			}
			_ = r.Config(&cfg)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := New(Config{ConfigPath: writeTokenConfig(t, okTokenConfig)}, nil)
			r.OnBuild(func() error { mutate(r); return nil })
			_, _, err := r.Handlers()
			if err == nil {
				t.Fatalf("钩子里调 %s 必须让启动失败（否则是静默失效）", name)
			}
			if !strings.Contains(err.Error(), "改动了注册表") {
				t.Errorf("错误应点出「钩子改了注册表」: %v", err)
			}
		})
	}
}

// TestOnBuildHookMayRegisterStopHook：OnStop 不在禁止之列——清理钩子在 RunStop 时才用，
// 钩子里登记是真生效的，拦它只会逼插件把清理逻辑挪到更早、更容易漏的地方。
func TestOnBuildHookMayRegisterStopHook(t *testing.T) {
	r := New(Config{ConfigPath: writeTokenConfig(t, okTokenConfig)}, nil)
	stopped := false
	r.OnBuild(func() error { r.OnStop(func() { stopped = true }); return nil })
	if _, _, err := r.Handlers(); err != nil {
		t.Fatalf("Handlers: %v", err)
	}
	r.RunStop(context.Background())
	if !stopped {
		t.Error("钩子里登记的清理钩子必须能被 RunStop 执行")
	}
}
