package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// buildGenerated 渲染各源包与 all_gen.go，go build + go vet，返回输出目录。
func buildGenerated(t *testing.T, pkgDirs ...string) string {
	t.Helper()
	outDir, err := os.MkdirTemp("testdata", "zzgen_*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(outDir) })

	var pkgs []pkgInfo
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
		pkgs = append(pkgs, newPkgInfo(cands))
	}
	if err := renderAll(outDir, pkgs); err != nil {
		t.Fatalf("renderAll: %v", err)
	}
	var out []byte
	for _, sub := range []string{"build", "vet"} {
		out, err = exec.Command("go", sub, "./"+filepath.ToSlash(outDir)).CombinedOutput()
		if err != nil {
			break
		}
	}
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

func TestCompile_Method(t *testing.T) {
	buildGenerated(t, "./testdata/case_method")
}

func TestCompile_Aggregate(t *testing.T) {
	buildGenerated(t, "./testdata/case_simple", "./testdata/case_method")
}
