package runtime

import (
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"
)

// toolifyModule 是基座自身的模块路径，用于从构建信息里找出宿主依赖的基座版本。
const toolifyModule = "github.com/fzxbl/mcp-toolify"

// develVersion 是读不到构建版本时的取值，与 go 工具链的写法一致。
const develVersion = "(devel)"

// majorSuffix 匹配模块路径末尾的大版本段（/v2、/v3……），它不是服务名。
var majorSuffix = regexp.MustCompile(`^v[0-9]+$`)

// serverIdentity 是 initialize 时返回给客户端的 serverInfo，外加启动日志用的基座版本。
type serverIdentity struct {
	Name    string
	Version string
	// Toolify 是宿主依赖的基座版本，只进启动日志，不进 serverInfo。
	Toolify string
	// VersionUnknown 表示 Version 既没配置也没能从构建信息推导出来，启动时据此告警。
	VersionUnknown bool
}

// resolveServerIdentity 以 cfg 显式配置为准，缺省部分从构建信息推导。
// bi 为 nil 表示读不到构建信息（debug.ReadBuildInfo 返回 ok=false）。
//
// 版本取主模块版本：go build 在含 .git 的目录下会自动写入 VCS tag，没有 tag 时是
// 带 commit id 的伪版本（v0.0.0-<时间>-<commit>），有未提交改动时带 +dirty。
// go run 或构建目录里没有 .git（如镜像构建只拷源码）时只有 "(devel)"。
func resolveServerIdentity(cfg Config, bi *debug.BuildInfo) serverIdentity {
	id := serverIdentity{Name: cfg.Name, Version: cfg.Version, Toolify: develVersion}
	var mainPath, mainVersion string
	if bi != nil {
		mainPath, mainVersion = bi.Main.Path, bi.Main.Version
		id.Toolify = toolifyVersion(bi)
	}
	if id.Name == "" {
		id.Name = moduleName(mainPath)
	}
	if id.Name == "" {
		// 连主模块路径都没有时退回可执行文件名，总比写死一个与本服务无关的名字强。
		id.Name = strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")
	}
	if id.Version == "" {
		id.Version = mainVersion
		if id.Version == "" || id.Version == develVersion {
			id.Version = develVersion
			id.VersionUnknown = true
		}
	}
	return id
}

// moduleName 取模块路径的最后一段作为服务名，跳过大版本后缀：
// github.com/acme/order-mcp/v2 → order-mcp。
func moduleName(modPath string) string {
	if modPath == "" {
		return ""
	}
	name := path.Base(modPath)
	if majorSuffix.MatchString(name) && path.Dir(modPath) != "." {
		name = path.Base(path.Dir(modPath))
	}
	return name
}

// toolifyVersion 从构建信息里找基座版本：宿主是基座本身（示例、测试）时取主模块版本，
// 否则取依赖列表里的版本，被 replace 时取替换后的版本。
func toolifyVersion(bi *debug.BuildInfo) string {
	if bi.Main.Path == toolifyModule {
		return orDevel(bi.Main.Version)
	}
	for _, d := range bi.Deps {
		if d.Path != toolifyModule {
			continue
		}
		if d.Replace != nil {
			// 本地目录替换没有版本号，记下替换路径便于排查。
			if d.Replace.Version == "" {
				return develVersion + " => " + d.Replace.Path
			}
			return d.Replace.Version
		}
		return orDevel(d.Version)
	}
	return develVersion
}

func orDevel(v string) string {
	if v == "" {
		return develVersion
	}
	return v
}
