package main

import (
	"fmt"
	"go/ast"
	"go/types"
)

// isContextType 判断 t 是否为 context.Context（含别名）。
func isContextType(t types.Type) bool {
	return types.Unalias(t).String() == "context.Context"
}

// validateSignature 拒绝会生成出无法编译或语义错误代码的签名。
// 报错 = 英文关键短语 + 中文替代写法；调用方负责补上 file:line。
func validateSignature(fn *ast.FuncDecl, sig *types.Signature) error {
	if !fn.Name.IsExported() {
		return fmt.Errorf("unexported function %s: 未导出的函数无法从生成的包调用，请改为导出名（首字母大写）", fn.Name.Name)
	}
	if sig.TypeParams().Len() > 0 {
		return fmt.Errorf("generic function %s: 泛型函数无法生成 JSON schema，请改用具体类型的包装函数", fn.Name.Name)
	}
	if recv := sig.Recv(); recv != nil {
		t := types.Unalias(recv.Type())
		if p, ok := t.(*types.Pointer); ok {
			t = types.Unalias(p.Elem())
		}
		if n, ok := t.(*types.Named); ok && !n.Obj().Exported() {
			return fmt.Errorf("unexported receiver type %s: 生成的包无法引用未导出类型，请把接收者类型改为导出名", n.Obj().Name())
		}
		if sig.RecvTypeParams().Len() > 0 {
			return fmt.Errorf("generic receiver type %s: 泛型接收者无法实例化，请改用具体类型的包装类型", fn.Name.Name)
		}
	}
	if sig.Variadic() {
		return fmt.Errorf("variadic parameter in %s: 可变参数无法稳定映射为 JSON 入参，请改为切片参数", fn.Name.Name)
	}
	seen := map[string]string{} // 归一化后的名字 -> 原参数名
	for i := 0; i < sig.Params().Len(); i++ {
		p := sig.Params().At(i)
		if i == 0 && isContextType(p.Type()) {
			continue // 首位 ctx 按位置传入，允许无名或 _，不会成为 JSON 字段
		}
		if p.Name() == "" || p.Name() == "_" {
			return fmt.Errorf("unnamed parameter #%d in %s: 参数必须有名字（不能省略或写成 _），因为名字就是 JSON 字段名", i+1, fn.Name.Name)
		}
		if isContextType(p.Type()) {
			return fmt.Errorf("context.Context must be the first parameter (参数 %q 位于第 %d 位): 请把 ctx 挪到第一个参数", p.Name(), i+1)
		}
		for _, key := range []string{"E:" + exportName(p.Name()), "S:" + snakeCase(p.Name())} {
			if prev, dup := seen[key]; dup {
				return fmt.Errorf("parameter name collision: %q 与 %q 在生成的字段名/JSON 名上冲突，请改名使其区分", prev, p.Name())
			}
			seen[key] = p.Name()
		}
	}
	return nil
}

// checkUnique 检查跨包唯一性：源包名不得重复（生成的文件名与标识符依赖包名），
// 最终工具名（含 mcp:name 覆盖）不得重复。报错带两处声明位置。
func checkUnique(pkgs [][]Candidate) error {
	pkgSeen := map[string]string{}  // 包名 -> 首个候选位置
	toolSeen := map[string]string{} // 工具名 -> 首个声明位置
	for _, cands := range pkgs {
		if len(cands) == 0 {
			continue
		}
		name := cands[0].PkgName
		if prev, ok := pkgSeen[name]; ok {
			return fmt.Errorf("duplicate package name %q: %s 与 %s 的源包名相同，包名必须唯一，请重命名其中一个包", name, prev, cands[0].Pos)
		}
		pkgSeen[name] = cands[0].Pos
		for _, c := range cands {
			tn := toolNameOf(c)
			if prev, ok := toolSeen[tn]; ok {
				return fmt.Errorf("duplicate tool name %q: %s 与 %s 的工具名相同，请用 mcp:name= 重命名其中一个", tn, prev, c.Pos)
			}
			toolSeen[tn] = c.Pos
		}
	}
	return nil
}
