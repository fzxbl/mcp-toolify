package main

import (
	"fmt"
	"go/ast"
	"go/types"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"

	"github.com/fzxbl/mcp-toolify/selector"
)

// Param 描述一个函数参数。
type Param struct {
	Name string // 形参名
	Type types.Type
	Doc  string // 来自 godoc 的 "param: <name> — desc"
	// BindType 非空时，input 字段类型用该具体类型替代（接口入参无法由 JSON 构造）。
	// 由函数上的 mcp:bind=<param>:<Type> 标记提供，形如 "auth.User" / "src.Label"，
	// 已是生成文件可用的类型表达式。
	BindType string
}

// Result 描述一个返回值。
type Result struct {
	Name string // 返回变量名（用于多返回打包成 map key），可能为空
	Type types.Type
}

// Candidate 表示一个待生成的 MCP tool。
type Candidate struct {
	Pkg         string // 包导入路径
	PkgName     string // 包名（用于命名 + import alias）
	Name        string // 函数名
	Recv        string // 方法接收者类型名（空表示包级函数）
	Description string // 第一句 godoc
	Detail      string // 余下 godoc 正文
	Params      []Param
	Returns     []Result
	// 标记
	ToolName string            // mcp:name=...
	Labels   map[string]string // mcp:labels=k=v,k2=v2
	Binds    map[string]string // mcp:bind=param:Type，param -> 具体类型表达式（可含包前缀，如 authz.User）
	Imports  []string          // mcp:import=<path>，供 mcp:bind 引用的外部包类型所在 import path（可多条）
}

func parseCandidates(pkgPath string) ([]Candidate, error) {
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo | packages.NeedDeps |
			packages.NeedImports,
	}
	pkgs, err := packages.Load(cfg, pkgPath)
	if err != nil {
		return nil, err
	}
	if len(pkgs) == 0 {
		return nil, fmt.Errorf("no packages loaded for %s", pkgPath)
	}
	var out []Candidate
	for _, pkg := range pkgs {
		if len(pkg.Errors) > 0 {
			return nil, fmt.Errorf("%s: %v", pkg.PkgPath, pkg.Errors[0])
		}
		for _, file := range pkg.Syntax {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Doc == nil {
					continue
				}
				if !hasMarker(fn.Doc, "mcp:tool") {
					continue
				}
				cand, err := buildCandidate(pkg, fn)
				if err != nil {
					// 带上 file:line，标注写错时能直接跳到出错的函数注释。
					return nil, fmt.Errorf("%s.%s (%s): %w",
						pkg.PkgPath, fn.Name.Name, pkg.Fset.Position(fn.Pos()), err)
				}
				out = append(out, cand)
			}
		}
	}
	return out, nil
}

func hasMarker(g *ast.CommentGroup, marker string) bool {
	if g == nil {
		return false
	}
	for _, c := range g.List {
		line := strings.TrimSpace(strings.TrimPrefix(c.Text, "//"))
		if line == marker || strings.HasPrefix(line, marker+"=") {
			return true
		}
	}
	return false
}

func buildCandidate(pkg *packages.Package, fn *ast.FuncDecl) (Candidate, error) {
	cand := Candidate{
		Pkg:     pkg.PkgPath,
		PkgName: pkg.Name,
		Name:    fn.Name.Name,
	}
	if fn.Recv != nil && len(fn.Recv.List) == 1 {
		if id, ok := fn.Recv.List[0].Type.(*ast.Ident); ok {
			cand.Recv = id.Name
		}
	}

	// 解析注释
	desc, detail, paramDocs, markers := parseDoc(fn.Doc)
	// 去掉 godoc 约定的函数名前缀
	desc = strings.TrimSpace(strings.TrimPrefix(desc, fn.Name.Name))
	cand.Description = desc
	cand.Detail = detail
	cand.ToolName = markers["name"]
	if err := checkMarkers(markers); err != nil {
		return cand, err
	}
	// `// mcp:labels=`（写了标记但值为空）必须报错：静默当成「没有 labels」会让
	// 工具在 deny-by-default 的准入里彻底不可见，而作者以为自己标过了。
	if v, ok := markers["labels"]; ok && strings.TrimSpace(v) == "" {
		return cand, fmt.Errorf("%s", "empty mcp:labels: 写了标记就必须给出至少一个 k=v")
	}
	labels, err := parseLabels(markers["labels"])
	if err != nil {
		return cand, err
	}
	cand.Labels = labels
	cand.Binds = parseBinds(markers)
	if v := markers["import"]; v != "" {
		for _, ip := range strings.Split(v, ";") {
			if ip = strings.TrimSpace(ip); ip != "" {
				cand.Imports = append(cand.Imports, ip)
			}
		}
	}

	// 类型信息
	sig := pkg.TypesInfo.Defs[fn.Name].Type().(*types.Signature)
	for i := 0; i < sig.Params().Len(); i++ {
		p := sig.Params().At(i)
		param := Param{
			Name: p.Name(),
			Type: p.Type(),
			Doc:  paramDocs[p.Name()],
		}
		// 接口 / 无法由 JSON 构造的入参必须由函数上的 mcp:bind 显式声明绑定到具体类型。
		if bt, ok := cand.Binds[p.Name()]; ok {
			param.BindType = resolveBindType(bt)
		}
		cand.Params = append(cand.Params, param)
	}
	for i := 0; i < sig.Results().Len(); i++ {
		r := sig.Results().At(i)
		cand.Returns = append(cand.Returns, Result{Name: r.Name(), Type: r.Type()})
	}
	return cand, nil
}

// resolveBindType 把 mcp:bind 里写的类型名转成生成文件可用的类型表达式。
//   - 已带包前缀（含 '.'，如 "auth.User"）：原样使用，需保证该包已被生成文件 import。
//   - 裸类型名（如 "Label"）：视为源包内类型，加 "src." 别名前缀。
func resolveBindType(bt string) string {
	if strings.Contains(bt, ".") {
		return bt
	}
	return "src." + bt
}

// parseBinds 解析所有 mcp:bind=param:Type 标记。markers 中 bind 标记会被聚合到
// markers["bind"] 里（用 ';' 分隔多条），形如 "label:Label;operation:SpecOperator"。
func parseBinds(markers map[string]string) map[string]string {
	raw := markers["bind"]
	if raw == "" {
		return nil
	}
	out := map[string]string{}
	for _, item := range strings.Split(raw, ";") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if i := strings.IndexByte(item, ':'); i > 0 {
			out[strings.TrimSpace(item[:i])] = strings.TrimSpace(item[i+1:])
		}
	}
	return out
}

// hasParam reports whether the function has a parameter with the given name (case-insensitive).
func hasParam(c Candidate, name string) bool {
	for _, p := range c.Params {
		if strings.EqualFold(p.Name, name) {
			return true
		}
	}
	return false
}

func parseDoc(g *ast.CommentGroup) (desc, detail string, params map[string]string, markers map[string]string) {
	params = map[string]string{}
	markers = map[string]string{}
	if g == nil {
		return
	}
	var buf []string
	for _, c := range g.List {
		line := strings.TrimSpace(strings.TrimPrefix(c.Text, "//"))
		switch {
		case strings.HasPrefix(line, "mcp:"):
			rest := strings.TrimPrefix(line, "mcp:")
			eq := strings.IndexByte(rest, '=')
			if eq < 0 {
				markers[rest] = ""
			} else {
				key, val := rest[:eq], rest[eq+1:]
				// bind / import / labels 可出现多次：bind/import 用 ';' 聚合，
				// labels 用 ',' 聚合（与单行多 label 写法同构）。多行写成同一个 key
				// 时由 parseLabels 的 duplicate-key 检查拦下，不会静默覆盖。
				switch {
				case (key == "bind" || key == "import") && markers[key] != "":
					markers[key] += ";" + val
				case key == "labels" && markers[key] != "":
					markers[key] += "," + val
				default:
					markers[key] = val
				}
			}
		case strings.HasPrefix(line, "param:"):
			body := strings.TrimSpace(strings.TrimPrefix(line, "param:"))
			for _, sep := range []string{"—", "-"} {
				if idx := strings.Index(body, sep); idx > 0 {
					name := strings.TrimSpace(body[:idx])
					params[name] = strings.TrimSpace(body[idx+len(sep):])
					break
				}
			}
		default:
			buf = append(buf, line)
		}
	}
	full := strings.Join(buf, "\n")
	parts := strings.SplitN(full, "\n\n", 2)
	desc = strings.TrimSpace(parts[0])
	if len(parts) > 1 {
		detail = strings.TrimSpace(parts[1])
	}
	return
}

// knownMarkers 是允许出现在 godoc 里的全部 mcp: 标记。
// 白名单而非黑名单：拼错（mcp:label=）、大小写不符（mcp:Labels=）或已废弃的标记
// 若被静默忽略，工具会带着"标签凭空消失"的样子生成出来，无从察觉。
var knownMarkers = map[string]bool{
	"tool": true, "name": true, "labels": true, "bind": true, "import": true,
}

// checkMarkers 校验 godoc 里的 mcp: 标记名；不在当前契约内的标记一律报错。
func checkMarkers(markers map[string]string) error {
	keys := make([]string, 0, len(markers))
	for k := range markers {
		keys = append(keys, k)
	}
	sort.Strings(keys) // 多个未知标记时报错稳定，便于测试与复现
	for _, k := range keys {
		if !knownMarkers[k] {
			return fmt.Errorf("unknown marker mcp:%s (known: bind, import, labels, name, tool)", k)
		}
	}
	return nil
}

// parseLabels 解析 mcp:labels=k=v,k2=v2；重复 key、空项、非法 key/value 都报错。
// key/value 的字符集规则由 selector 包唯一持有（ValidateLabelKey/ValidateLabelValue），
// 生成期与匹配期共用，避免出现"能标注却永远匹配不上"的 label。
func parseLabels(raw string) (map[string]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	out := map[string]string{}
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		i := strings.IndexByte(item, '=')
		if i <= 0 {
			return nil, fmt.Errorf("invalid label %q (want k=v)", item)
		}
		k, v := strings.TrimSpace(item[:i]), strings.TrimSpace(item[i+1:])
		if v == "" {
			return nil, fmt.Errorf("invalid label %q (want k=v)", item)
		}
		if err := selector.ValidateLabelKey(k); err != nil {
			return nil, err
		}
		if err := selector.ValidateLabelValue(v); err != nil {
			return nil, err
		}
		if _, dup := out[k]; dup {
			return nil, fmt.Errorf("duplicate label key %q", k)
		}
		out[k] = v
	}
	return out, nil
}
