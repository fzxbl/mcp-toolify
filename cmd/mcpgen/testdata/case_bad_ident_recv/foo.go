package case_bad_ident_recv

// A_B 类型。
type A_B struct{}

// A 类型。
type A struct{}

// C 方法。
//
// mcp:tool
// mcp:name=ident.ab_c
func (x *A_B) C() string { return "" }

// B_C 方法。
//
// mcp:tool
// mcp:name=ident.a_bc
func (x *A) B_C() string { return "" }
