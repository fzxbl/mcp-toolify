package main

import (
	"bytes"
	"embed"
	"fmt"
	"go/format"
	"go/types"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/template"
)

//go:embed tmpl/*.tmpl
var tmplFS embed.FS

type fieldData struct {
	GoName, GoType, JSONName, Description string
}

type toolData struct {
	ToolName    string
	HandlerName string
	InputType   string
	OutputType  string
	OutputZero  string
	OutputExpr  string
	// SpillExpr 是交给 runtime.MaybeSpill 的原始返回值表达式（不套 {"result":...} 包装），
	// 使 MaybeSpill 能按真实返回类型推导落盘格式（slice → jsonl，其余 → json）并测量
	// 结果体积以决定是否落盘。非单值返回时回退为 OutputExpr。
	SpillExpr        string
	HasError         bool
	CallExpr         string
	DescriptionLit   string
	TagsLit          string
	CapabilityLit    string // runtime.ReadOnly | runtime.ReadWrite
	RiskLit          string // runtime.RiskNone|RiskLow|RiskMedium|RiskHigh
	InputFields      []fieldData
	NeedsInputSchema bool
}

type fileData struct {
	PkgPath  string
	PkgName  string
	PkgIdent string
	Imports  []string // 额外 import 路径（去重，排序），不含 src/runtime/giano/mcp 等已硬编码项
	Tools    []toolData
}

func renderPackage(outDir string, cands []Candidate) error {
	if len(cands) == 0 {
		return nil
	}
	pkgPath := cands[0].Pkg
	pkgName := cands[0].PkgName
	fd := fileData{
		PkgPath:  pkgPath,
		PkgName:  pkgName,
		PkgIdent: exportName(pkgName),
	}
	imps := map[string]struct{}{}
	for _, c := range cands {
		td, err := buildToolData(c, pkgPath, imps)
		if err != nil {
			return fmt.Errorf("%s.%s: %w", c.PkgName, c.Name, err)
		}
		fd.Tools = append(fd.Tools, td)
		// 显式 mcp:import 声明的额外 import：供 mcp:bind 引用的外部包类型使用。
		for _, ip := range c.Imports {
			imps[ip] = struct{}{}
		}
		// 参数类型引用到的外部包 import 已由 typeExpr 在 buildToolData 中记录进 imps。
		// 返回值统一打包进 map[string]any，生成代码不直接引用其类型名，无需为其收集 import。
	}
	for k := range imps {
		fd.Imports = append(fd.Imports, k)
	}
	sort.Strings(fd.Imports)
	tmpl, err := template.ParseFS(tmplFS, "tmpl/tool.go.tmpl")
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, fd); err != nil {
		return err
	}
	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		return fmt.Errorf("format: %w\n--- raw output ---\n%s", err, buf.String())
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	out := filepath.Join(outDir, pkgName+"_gen.go")
	return os.WriteFile(out, formatted, 0o644)
}

func buildToolData(c Candidate, srcPkgPath string, imps map[string]struct{}) (toolData, error) {
	td := toolData{
		HandlerName: "handle_" + c.PkgName + "_" + c.Name,
		// 生成文件同属 tools 包，不同源包可能有同名函数（如 pandora.InstanceInfo
		// 与 ibns.InstanceInfo），故 input 结构体名带上包名前缀避免重复声明。
		InputType: exportName(c.PkgName) + c.Name + "Input",
	}
	if c.ToolName != "" {
		td.ToolName = c.ToolName
	} else {
		td.ToolName = c.PkgName + "." + snakeCase(c.Name)
	}
	td.DescriptionLit = strconv.Quote(normalizeDesc(c.Description, c.Detail))

	tagItems := make([]string, 0, len(c.Tags))
	for _, t := range c.Tags {
		tagItems = append(tagItems, strconv.Quote(strings.TrimSpace(t)))
	}
	td.TagsLit = strings.Join(tagItems, ", ")

	td.CapabilityLit = "runtime.ReadOnly"
	for _, tag := range c.Tags {
		if strings.TrimSpace(tag) == "write" {
			td.CapabilityLit = "runtime.ReadWrite"
			break
		}
	}
	switch c.Risk {
	case "low":
		td.RiskLit = "runtime.RiskLow"
	case "medium":
		td.RiskLit = "runtime.RiskMedium"
	case "high":
		td.RiskLit = "runtime.RiskHigh"
	default:
		td.RiskLit = "runtime.RiskNone"
	}

	var callArgs []string
	for _, p := range c.Params {
		// 绑定到具体类型的参数（接口入参等）：input 字段类型用 mcp:bind 指定的具体类型，
		// 调用时直接把该值传给原（可能是接口的）形参。绑定类型所在的外部包由 mcp:import 声明。
		var goType string
		if p.BindType != "" {
			goType = p.BindType
		} else {
			goType = typeExpr(p.Type, srcPkgPath, imps)
		}
		td.InputFields = append(td.InputFields, fieldData{
			GoName:      exportName(p.Name),
			GoType:      goType,
			JSONName:    snakeCase(p.Name),
			Description: sanitizeJSONSchemaDoc(p.Doc),
		})
		callArgs = append(callArgs, "in."+exportName(p.Name))
	}

	// input 中存在 interface{} 字段时，用 runtime.AnyInputSchema 显式生成
	// 半受限 schema，避免 SDK 默认反射产生无类型约束的空节点。
	for _, p := range c.Params {
		if p.BindType != "" {
			continue
		}
		if containsInterface(p.Type, map[string]bool{}) {
			td.NeedsInputSchema = true
			break
		}
	}

	switch len(c.Returns) {
	case 0:
		td.OutputType = "any"
		td.OutputZero = "nil"
		td.OutputExpr = "nil"
		td.CallExpr = fmt.Sprintf("src.%s(%s)", callExpr(c), strings.Join(callArgs, ", "))
	case 1:
		if isErrorType(c.Returns[0].Type) {
			td.OutputType = "any"
			td.OutputZero = "nil"
			td.OutputExpr = "nil"
			td.HasError = true
			td.CallExpr = fmt.Sprintf("callErr := src.%s(%s)", callExpr(c), strings.Join(callArgs, ", "))
		} else {
			// MCP 规范：structuredContent 必须是 JSON object，
			// 因此把任意返回值统一包成 {"result": ...}。
			//
			// OutputType 用 any（而非 map[string]any）：避免 SDK 派生 outputSchema。
			// 一旦声明 outputSchema，严格客户端会要求每次结果都带合规 structuredContent，
			// 在错误路径 / 空结果时直接校验报错。用 any 则不下发 outputSchema，
			// 成功时仍照常填充 structuredContent，对空结果天然宽容。
			td.OutputType = "any"
			td.OutputZero = "nil"
			td.OutputExpr = `map[string]any{"result": out}`
			td.SpillExpr = "out"
			td.CallExpr = fmt.Sprintf("out := src.%s(%s)", callExpr(c), strings.Join(callArgs, ", "))
		}
	case 2:
		if isErrorType(c.Returns[1].Type) {
			td.OutputType = "any"
			td.OutputZero = "nil"
			td.OutputExpr = `map[string]any{"result": out}`
			td.SpillExpr = "out"
			td.HasError = true
			td.CallExpr = fmt.Sprintf("out, callErr := src.%s(%s)", callExpr(c), strings.Join(callArgs, ", "))
		} else {
			// 两个非 error 返回值：按返回变量名打包成 map。
			td.buildMultiReturn(c)
		}
	default:
		// >2 返回值：按返回变量名打包成 map，末位若为 error 单独处理。
		td.buildMultiReturn(c)
	}
	// 非单值返回场景（0 值 / 纯 error / 多值）没有单一原始返回值可 spill，
	// 回退用 OutputExpr（打包后的 map）。
	if td.SpillExpr == "" {
		td.SpillExpr = td.OutputExpr
	}
	return td, nil
}

// buildMultiReturn 把多个返回值（可能末位是 error）打包成 map[string]any。
func (td *toolData) buildMultiReturn(c Candidate) {
	n := len(c.Returns)
	hasErr := n > 0 && isErrorType(c.Returns[n-1].Type)
	valCount := n
	if hasErr {
		valCount = n - 1
	}
	var lhs []string
	var entries []string
	for i := 0; i < valCount; i++ {
		v := fmt.Sprintf("r%d", i)
		lhs = append(lhs, v)
		key := snakeCase(c.Returns[i].Name)
		if key == "" || key == "_" {
			key = fmt.Sprintf("result%d", i)
		}
		entries = append(entries, fmt.Sprintf("%q: %s", key, v))
	}
	if hasErr {
		lhs = append(lhs, "callErr")
		td.HasError = true
	}
	td.OutputType = "any"
	td.OutputZero = "nil"
	td.OutputExpr = "map[string]any{" + strings.Join(entries, ", ") + "}"
	td.CallExpr = fmt.Sprintf("%s := src.%s(%s)", strings.Join(lhs, ", "), callExpr(c), strings.Join(callArgsOf(c), ", "))
}

// callArgsOf 重建调用实参列表（与 buildToolData 中保持一致）。
func callArgsOf(c Candidate) []string {
	var args []string
	for _, p := range c.Params {
		args = append(args, "in."+exportName(p.Name))
	}
	return args
}

// containsInterface 报告类型 t 中是否（递归）含有空 interface 字段。
// seen 防止命名类型自引用导致死循环。
func containsInterface(t types.Type, seen map[string]bool) bool {
	switch x := t.(type) {
	case *types.Interface:
		return x.NumMethods() == 0
	case *types.Pointer:
		return containsInterface(x.Elem(), seen)
	case *types.Slice:
		return containsInterface(x.Elem(), seen)
	case *types.Array:
		return containsInterface(x.Elem(), seen)
	case *types.Map:
		return containsInterface(x.Key(), seen) || containsInterface(x.Elem(), seen)
	case *types.Named:
		key := x.String()
		if seen[key] {
			return false
		}
		seen[key] = true
		return containsInterface(x.Underlying(), seen)
	case *types.Struct:
		for i := 0; i < x.NumFields(); i++ {
			if containsInterface(x.Field(i).Type(), seen) {
				return true
			}
		}
	}
	return false
}

func callExpr(c Candidate) string {
	if c.Recv == "" {
		return c.Name
	}
	return c.Recv + "(0)." + c.Name
}

func isErrorType(t types.Type) bool { return t.String() == "error" }

// normalizeDesc 把 godoc 的首句 + 余下正文整理成一行紧凑文本：
//   - 去除每行前后空白
//   - 把连续空行合并为一次段落分隔（用 " | "），普通换行变成空格
//   - 折叠多余空白
//
// 这样工具描述传给 MCP 客户端时不会再出现多重 \n\n，肉眼也更易读。
func normalizeDesc(head, body string) string {
	parts := []string{strings.TrimSpace(head)}
	// 把 body 按空行切成段落，每段内的换行替换成空格。
	for _, para := range strings.Split(body, "\n\n") {
		var lines []string
		for _, ln := range strings.Split(para, "\n") {
			if ln = strings.TrimSpace(ln); ln != "" {
				lines = append(lines, ln)
			}
		}
		if len(lines) > 0 {
			parts = append(parts, strings.Join(lines, " "))
		}
	}
	out := strings.Join(filterEmpty(parts), " | ")
	return strings.Join(strings.Fields(out), " ")
}

func filterEmpty(ss []string) []string {
	out := ss[:0]
	for _, s := range ss {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// jsonschemaDisallowedPrefix 与 jsonschema-go 内部规则一致：禁止 tag 描述以
// "WORD=" 开头（WORD 为一段非空白字符），该前缀被库保留用于未来扩展。
// 参见 github.com/google/jsonschema-go/jsonschema/infer.go 的 disallowedPrefixRegexp。
var jsonschemaDisallowedPrefix = regexp.MustCompile(`^[^ \t\n]*=`)

// sanitizeJSONSchemaDoc 处理用于 `jsonschema:"..."` struct tag 的描述：
//   - 反引号在 raw string struct tag 中无法转义，替换为单引号。
//   - 双引号会破坏 tag 边界，替换为单引号。
//   - jsonschema 解析器禁止描述以 `WORD=` 开头（会被当作未知选项）；仅在命中
//     该前缀时于首部补一个空格规避，正文中的 `=`（如 label selector 语法
//     `key=value,key2!=v2`）原样保留，不再被误伤成 `:`。
func sanitizeJSONSchemaDoc(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "`", "'")
	s = strings.ReplaceAll(s, `"`, "'")
	if jsonschemaDisallowedPrefix.MatchString(s) {
		s = " " + s
	}
	return s
}

// typeExpr 渲染类型 t 在生成文件中的类型表达式：
//   - 源包（生成 wrapper 的目标函数所在包）用 "src" 别名；
//   - 其他外部包用其包名，并把该包的 import path 记录进 imps（供生成 import 块）；
//   - 内置类型（string/int/...）原样输出。
//
// 通过 go/types 的 Qualifier 实现，对任意工程/包路径通用，不含任何硬编码前缀。
func typeExpr(t types.Type, srcPkgPath string, imps map[string]struct{}) string {
	return types.TypeString(t, func(p *types.Package) string {
		if p == nil {
			return ""
		}
		if p.Path() == srcPkgPath {
			return "src"
		}
		imps[p.Path()] = struct{}{}
		return p.Name()
	})
}

func renderAll(outDir string, pkgIdents []string) error {
	tmpl, err := template.ParseFS(tmplFS, "tmpl/all.go.tmpl")
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, struct{ Packages []string }{pkgIdents}); err != nil {
		return err
	}
	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(outDir, "all_gen.go"), formatted, 0o644)
}
