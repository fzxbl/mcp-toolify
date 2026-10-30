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

// recvNamed 返回方法接收者的命名类型（剥掉别名与 *）；非方法或无法识别时为 nil。
func recvNamed(sig *types.Signature) *types.Named {
	recv := sig.Recv()
	if recv == nil {
		return nil
	}
	t := types.Unalias(recv.Type())
	if p, ok := t.(*types.Pointer); ok {
		t = types.Unalias(p.Elem())
	}
	n, _ := t.(*types.Named)
	return n
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
	if sig.Recv() != nil {
		if n := recvNamed(sig); n != nil && !n.Obj().Exported() {
			return fmt.Errorf("unexported receiver type %s: 生成的包无法引用未导出类型，请把接收者类型改为导出名。不需要状态的工具请写成包级函数；写成方法意味着宿主必须交一个实例", n.Obj().Name())
		}
		if sig.RecvTypeParams().Len() > 0 {
			return fmt.Errorf("generic receiver type %s: 泛型接收者无法实例化，请改用具体类型的包装类型。不需要状态的工具请写成包级函数；写成方法意味着宿主必须交一个实例", recvTypeName(fn, sig))
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

// checkIdentCollisions 检查全部生成标识符在 tools 包内是否唯一：
// 每个候选的 handler 函数名与 input 类型名、每个包的 Register_ 函数名（同属包级作用域），
// 以及 Impls 结构体的字段名（<包标识>_<类型名>，独立的字段作用域）。
// 标识符一律用 render.go 中的 *Of 函数推导，与生成器共用同一来源。
// 必须在写任何文件之前调用；冲突时报错带标识符与两处声明位置。
func checkIdentCollisions(pkgs [][]Candidate) error {
	scope := map[string]string{}  // 包级标识符 -> 首个声明位置
	fields := map[string]string{} // Impls 字段名 -> 首个声明位置
	claim := func(seen map[string]string, ident, pos string) error {
		if prev, ok := seen[ident]; ok {
			return fmt.Errorf("generated identifier collision: 生成的标识符 %q 被 %s 与 %s 同时使用，会导致生成代码重复声明；请重命名其中一个函数、方法或类型（或包名）使生成名不同", ident, prev, pos)
		}
		seen[ident] = pos
		return nil
	}
	for _, cands := range pkgs {
		if len(cands) == 0 {
			continue
		}
		if err := claim(scope, registerNameOf(cands[0].PkgName), cands[0].Pos); err != nil {
			return err
		}
		recvSeen := map[string]bool{}
		for _, c := range cands {
			for _, id := range []string{handlerNameOf(c), inputTypeOf(c)} {
				if err := claim(scope, id, c.Pos); err != nil {
					return err
				}
			}
			if c.Recv != "" && !recvSeen[c.Recv] {
				recvSeen[c.Recv] = true
				if err := claim(fields, implFieldOf(c.PkgName, c.Recv), c.Pos); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// recvTypeName 返回接收者类型名；泛型接收者在类型检查后可能无法还原为 Named，此时回退到 AST。
func recvTypeName(fn *ast.FuncDecl, sig *types.Signature) string {
	if n := recvNamed(sig); n != nil {
		return n.Obj().Name()
	}
	if fn.Recv != nil && len(fn.Recv.List) > 0 {
		e := fn.Recv.List[0].Type
		for {
			switch x := e.(type) {
			case *ast.StarExpr:
				e = x.X
				continue
			case *ast.ParenExpr:
				e = x.X
				continue
			case *ast.IndexExpr:
				e = x.X
				continue
			case *ast.IndexListExpr:
				e = x.X
				continue
			case *ast.Ident:
				return x.Name
			}
			break
		}
	}
	return fn.Name.Name
}
