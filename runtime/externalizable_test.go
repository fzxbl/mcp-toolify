package runtime

import (
	"os/exec"
	"strings"
	"testing"
)

// modulePath 是本仓库的 module 路径；断言只关心「以它开头」的 import。
const modulePath = "github.com/fzxbl/mcp-toolify"

// TestPluginsOnlyDependOnPublicPackages 守住一条架构约束：任何插件都必须能移出本仓库、
// 以外部 module 注册进来。外部 module 只能用导出 API，所以插件的**生产代码**只准依赖
// runtime 与 selector 两个包。
//
// 为什么要机器守：违反它在单仓库开发时完全看不出来（同一个 module 里 import 谁都能编过），
// 只有真去拆包时才炸，而那时依赖已经长满了。
//
// 刻意只看 .Imports（生产依赖）而不看 .TestImports：跨插件的**组合用例**是本仓库该有的
// 东西（例如 spill 的用例装一份 audit 来验证链上协作），收紧它会把有价值的集成测试逼掉。
func TestPluginsOnlyDependOnPublicPackages(t *testing.T) {
	allowed := map[string]bool{
		modulePath + "/runtime":  true,
		modulePath + "/selector": true,
	}
	out, err := exec.Command("go", "list", "-f",
		`{{.ImportPath}} {{join .Imports " "}}`, modulePath+"/plugins/...").CombinedOutput()
	if err != nil {
		t.Fatalf("go list 失败: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 0 || lines[0] == "" {
		t.Fatalf("go list 没有列出任何插件包，断言等于空转：\n%s", out)
	}
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		pkg, imports := fields[0], fields[1:]
		for _, imp := range imports {
			if !strings.HasPrefix(imp, modulePath) || allowed[imp] {
				continue
			}
			t.Errorf("%s 依赖了 %s：插件生产代码只准 import %s/{runtime,selector}，"+
				"否则它无法作为外部 module 存在", pkg, imp, modulePath)
		}
	}
}
