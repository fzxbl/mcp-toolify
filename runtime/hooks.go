package runtime

import "context"

// StartupHooks 是 server 启动时要执行的钩子（如启动后台 GC 协程）。
// 由各业务包在自己的 init() 里 append，避免 runtime 反向 import 业务包造成循环依赖。
var StartupHooks []func(context.Context)

// runStartupHooks 由 server 启动时调用，依次执行所有已注册钩子。
func runStartupHooks(ctx context.Context) {
	for _, h := range StartupHooks {
		if h != nil {
			h(ctx)
		}
	}
}

// ShutdownHooks 是进程优雅退出前要同步执行的钩子（如关闭会话、杀掉子进程）。
// 由业务包 init() 里 append；进程收到退出信号时由 gtask.BeforeShutdown 回调统一执行。
var ShutdownHooks []func()

// RunShutdownHooks 依次执行所有已注册的退出钩子。幂等由各钩子自身保证。
func RunShutdownHooks() {
	for _, h := range ShutdownHooks {
		if h != nil {
			h()
		}
	}
}
