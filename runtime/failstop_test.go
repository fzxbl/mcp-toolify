package runtime

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// stopProbe 模拟一个「插件在 Install 里起了协程、并把停止函数登记进 OnStop」的形状。
// 用 channel 判定协程真的退出了，不看 NumGoroutine —— 后者受同包其它用例与 GC 影响，
// 是典型的负载敏感 flaky 判据。
type stopProbe struct {
	stop chan struct{}
	done chan struct{}
}

func newStopProbe() *stopProbe {
	p := &stopProbe{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(p.done)
		t := time.NewTicker(time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-p.stop:
				return
			case <-t.C:
			}
		}
	}()
	return p
}

// close 幂等，与 spill/confirm 的清理钩子同形。
func (p *stopProbe) close() {
	select {
	case <-p.stop:
	default:
		close(p.stop)
	}
}

// exited 等协程退出，超时即失败。
func (p *stopProbe) exited(t *testing.T) bool {
	t.Helper()
	select {
	case <-p.done:
		return true
	case <-time.After(3 * time.Second):
		return false
	}
}

// TestBuildFailureRunsStopHooks：启动失败必须回收插件已经起的协程。
//
// 为什么必须由基座兜：失败原因常常在最后一个插件 Install 之后才发现（配置认领检查、
// build 期校验钩子），此时四个插件的协程/连接池都已经起来了；指望每个调用方在每条
// 失败路径上记得清一次，是必漏的约定。
func TestBuildFailureRunsStopHooks(t *testing.T) {
	cases := map[string]func(t *testing.T) *Registry{
		"build 期钩子报错": func(t *testing.T) *Registry {
			r := New(Config{ConfigPath: writeTokenConfig(t, okTokenConfig)}, nil)
			r.OnBuild(func() error { return fmt.Errorf("%s", "没有注册落地函数") })
			return r
		},
		"无人认领的配置项": func(t *testing.T) *Registry {
			return New(Config{ConfigPath: writeTokenConfig(t,
				okTokenConfig+"\n[qouta]\nbackend = \"redis\"\n")}, nil)
		},
		"缺少必需插件": func(t *testing.T) *Registry {
			return New(Config{ConfigPath: writeTokenConfig(t, okTokenConfig),
				RequiredPlugins: []string{"audit"}}, nil)
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			r := mk(t)
			probe := newStopProbe()
			r.OnStop(probe.close)
			defer probe.close() // 用例自己兜底，断言失败时不留协程
			if _, _, err := r.Handlers(); err == nil {
				t.Fatal("该配置必须启动失败")
			}
			if !probe.exited(t) {
				t.Error("启动失败后插件协程必须被回收（基座应在失败路径上跑一次 RunStop）")
			}
		})
	}
}

// TestListenFailureRunsStopHooks：端口占用同样是「起不来」，协程照样要回收。
func TestListenFailureRunsStopHooks(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer busy.Close()

	r := New(Config{Addr: busy.Addr().String(),
		ConfigPath: writeTokenConfig(t, okTokenConfig)}, nil)
	probe := newStopProbe()
	r.OnStop(probe.close)
	defer probe.close()
	if err := r.Start(context.Background()); err == nil {
		t.Fatal("端口已被占用时 Start 必须报错")
	}
	if !probe.exited(t) {
		t.Error("监听失败后插件协程必须被回收")
	}
}

// TestRunStopIsIdempotent：清理只该发生一次。失败路径上基座已经兜过一次，
// 宿主照旧 defer 一次 RunStop —— 两者叠加不得把钩子跑两遍
// （二次关连接池/句柄会打出一串「关闭失败」噪声，掩盖真正的启动失败原因）。
func TestRunStopIsIdempotent(t *testing.T) {
	r := New(Config{ConfigPath: writeTokenConfig(t, okTokenConfig)}, nil)
	calls := 0
	r.OnStop(func() { calls++ })
	r.OnBuild(func() error { return fmt.Errorf("%s", "启动失败") })
	if _, _, err := r.Handlers(); err == nil {
		t.Fatal("必须启动失败")
	}
	r.RunStop(context.Background())
	r.RunStop(context.Background())
	if calls != 1 {
		t.Errorf("清理钩子执行 %d 次，要求恰好 1 次", calls)
	}
}

// TestStoppedRegistryRefusesRestart：清理跑过之后本 Registry 报废，build/Start 一律拒绝。
//
// 复审实测的降级路径：listen 失败（基座已在该路径兜过一次 RunStop）后，同一个 Registry
// 换个端口重试 Start 会**真的起起来**，但插件已被清理——只读工具照常返回结果，受配额
// 管辖的高危工具永久被拒，spill 的落盘与 confirm 的待确认单再也不被回收。
// 「端口占用 → 换端口重试」是很自然的宿主写法，所以这条必须直接拒绝而不是降级服务。
func TestStoppedRegistryRefusesRestart(t *testing.T) {
	r := New(Config{Addr: "127.0.0.1:0",
		ConfigPath: writeTokenConfig(t, okTokenConfig)}, nil)
	r.RunStop(context.Background()) // 模拟「失败路径已兜过清理」或宿主显式提前 stop

	if _, _, err := r.Handlers(); err == nil {
		t.Error("清理过的 Registry 不该还能组装出 handler")
	} else if !strings.Contains(err.Error(), "不能再启动") {
		t.Errorf("err = %v，want 说明该 Registry 已报废", err)
	}
	// 用一个已取消的 ctx：万一 checkStopped 失效，Start 会真的开始服务，
	// 用 Background 就得阻塞到超时（变异实验里实测卡了 600s 才被测试框架打断）。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.Start(ctx); err == nil {
		t.Error("清理过的 Registry 不该还能 Start")
	}
}

// TestFreshRegistryStartsAfterAnotherStopped：报废是**实例级**的，重建这条路必须通畅。
// 少了这条正向断言，一个「一律拒绝」的实现也能让上面那条变绿。
func TestFreshRegistryStartsAfterAnotherStopped(t *testing.T) {
	cfg := writeTokenConfig(t, okTokenConfig)
	old := New(Config{ConfigPath: cfg}, nil)
	old.RunStop(context.Background())

	fresh := New(Config{ConfigPath: cfg}, nil)
	defer fresh.RunStop(context.Background())
	h, _, err := fresh.Handlers()
	if err != nil {
		t.Fatalf("新建的 Registry 必须能正常组装: %v", err)
	}
	if h == nil {
		t.Error("新建的 Registry 应交出 handler")
	}
}

// TestStopHookPanicDoesNotSkipOthers：一个坏钩子不连坐其余清理。
//
// 与幂等是配套的：幂等意味着「第二次调用不会补跑」，若第一个钩子 panic 就中断，
// 后面的连接池与文件句柄将永远没人关——一个坏钩子废掉整条清理链。
func TestStopHookPanicDoesNotSkipOthers(t *testing.T) {
	r := New(Config{ConfigPath: writeTokenConfig(t, okTokenConfig)}, nil)
	second := 0
	r.OnStop(func() { panic("关闭失败") })
	r.OnStop(func() { second++ })

	r.RunStop(context.Background()) // panic 不该打穿 RunStop
	if second != 1 {
		t.Errorf("第二个清理钩子执行 %d 次，要求 1 次（前一个 panic 不该连坐）", second)
	}
}

// TestDuplicateRouteFailsStartup：同一个 pattern 重复注册即启动失败。
//
// 重复注册在任何时刻都是 bug：被覆盖的那一路从此静默不可达（两个插件都注册 /healthz，
// 其中一个的探活永远返回另一个的结果），而 map 赋值不留任何痕迹。
func TestDuplicateRouteFailsStartup(t *testing.T) {
	for _, tc := range []struct {
		name   string
		second func(r *Registry)
	}{
		{"需鉴权重复", func(r *Registry) { r.Route("/dup/", http.NotFoundHandler()) }},
		{"跨免鉴权重复", func(r *Registry) { r.RoutePublic("/dup/", http.NotFoundHandler()) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := New(Config{ConfigPath: writeTokenConfig(t, okTokenConfig)}, nil)
			r.Route("/dup/", http.NotFoundHandler())
			tc.second(r)
			_, _, err := r.Handlers()
			if err == nil {
				t.Fatal("重复注册同一个路由必须启动失败")
			}
			if !strings.Contains(err.Error(), "/dup/") {
				t.Errorf("err = %v，want 指出是哪个 pattern", err)
			}
		})
	}
}

// TestDistinctRoutesStillWork：不同 pattern 照常注册，避免上面那条靠「一律拒绝」蒙对。
func TestDistinctRoutesStillWork(t *testing.T) {
	r := New(Config{ConfigPath: writeTokenConfig(t, okTokenConfig)}, nil)
	defer r.RunStop(context.Background())
	r.Route("/a/", http.NotFoundHandler())
	r.RoutePublic("/b", http.NotFoundHandler())
	if _, _, err := r.Handlers(); err != nil {
		t.Fatalf("不同 pattern 不该冲突: %v", err)
	}
	if _, ok := r.Routes()["/a/"]; !ok {
		t.Errorf("需鉴权路由 /a/ 应登记，实际 %v", r.Routes())
	}
	if _, ok := r.publicRoutes["/b"]; !ok {
		t.Errorf("免鉴权路由 /b 应登记，实际 %v", r.publicRoutes)
	}
}
