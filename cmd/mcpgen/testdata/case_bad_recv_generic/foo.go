package case_bad_recv_generic

// Box 泛型类型。
type Box[T any] struct{}

// Run 泛型接收者。
//
// mcp:tool
func (b *Box[T]) Run() error { return nil }
