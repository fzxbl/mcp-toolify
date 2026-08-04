package case_write

import "github.com/fzxbl/mcp-toolify/cmd/mcpgen/testdata/case_write/authz"

// UpdateThing 修改某个对象。
//
// param: cluster — 集群名
// param: app — 应用名
// param: spec — 新 spec
//
// mcp:tool
// mcp:tags=write
// mcp:bind=user:authz.User
// mcp:import=github.com/fzxbl/mcp-toolify/cmd/mcpgen/testdata/case_write/authz
func UpdateThing(cluster, app, spec string, user authz.CredGenerator) error {
	_ = user
	return nil
}
