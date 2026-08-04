package authz

// CredGenerator 是一个需要由调用方提供具体实现的接口入参示例。
type CredGenerator interface {
	Cred() string
}

// User 是 CredGenerator 的一个具体实现，可由 JSON 构造，供 mcp:bind 使用。
type User struct {
	ID string `json:"id"`
}

// Cred 实现 CredGenerator。
func (u User) Cred() string { return u.ID }
