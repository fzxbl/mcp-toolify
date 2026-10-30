package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// buildGenerated 渲染各源包与 all_gen.go 并 go build，返回输出目录。
func buildGenerated(t *testing.T, pkgDirs ...string) string {
	t.Helper()
	return buildGeneratedImpl(t, true, pkgDirs)
}

// buildGeneratedPkgOnly 与 buildGenerated 相同，但不生成 all_gen.go。
func buildGeneratedPkgOnly(t *testing.T, pkgDirs ...string) string {
	t.Helper()
	return buildGeneratedImpl(t, false, pkgDirs)
}

func buildGeneratedImpl(t *testing.T, withAll bool, pkgDirs []string) string {
	t.Helper()
	outDir, err := os.MkdirTemp("testdata", "zzgen_*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(outDir) })

	var idents []string
	for _, dir := range pkgDirs {
		cands, err := parseCandidates(dir)
		if err != nil {
			t.Fatalf("parse %s: %v", dir, err)
		}
		if len(cands) == 0 {
			t.Fatalf("%s: 没有候选工具", dir)
		}
		if err := renderPackage(outDir, cands); err != nil {
			t.Fatalf("render %s: %v", dir, err)
		}
		idents = append(idents, exportName(cands[0].PkgName))
	}
	if withAll {
		if err := renderAll(outDir, idents); err != nil {
			t.Fatalf("renderAll: %v", err)
		}
	}
	out, err := exec.Command("go", "build", "./"+filepath.ToSlash(outDir)).CombinedOutput()
	if err != nil {
		var files string
		matches, _ := filepath.Glob(filepath.Join(outDir, "*.go"))
		for _, f := range matches {
			b, _ := os.ReadFile(f)
			files += "\n--- " + f + " ---\n" + string(b)
		}
		t.Fatalf("go build 失败: %v\n%s\n%s", err, out, files)
	}
	return outDir
}

func TestCompile_Ctx(t *testing.T) {
	buildGenerated(t, "./testdata/case_ctx")
}

// 只生成包文件：Task 5 之前 all_gen.go 仍按旧签名调用 Register_<Pkg>(s, opts)。
func TestCompile_Method(t *testing.T) {
	buildGeneratedPkgOnly(t, "./testdata/case_method")
}
