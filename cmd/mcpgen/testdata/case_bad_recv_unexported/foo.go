package case_bad_recv_unexported

// svc 未导出的接收者类型。
type svc struct{}

// Run 方法的接收者类型未导出。
//
// mcp:tool
func (s *svc) Run() error { return nil }
