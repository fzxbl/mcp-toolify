package main

import (
	"strings"
	"testing"
)

func TestCheckUnique(t *testing.T) {
	load := func(paths ...string) [][]Candidate {
		var out [][]Candidate
		for _, p := range paths {
			c, err := parseCandidates(p)
			if err != nil {
				t.Fatalf("parse %s: %v", p, err)
			}
			out = append(out, c)
		}
		return out
	}
	t.Run("dup tool", func(t *testing.T) {
		err := checkUnique(load("./testdata/case_dup_tool"))
		if err == nil || !strings.Contains(err.Error(), "duplicate tool name") || !strings.Contains(err.Error(), "dup.same") {
			t.Fatalf("err = %v", err)
		}
		if strings.Count(err.Error(), "foo.go:") != 2 {
			t.Errorf("应含两处位置: %v", err)
		}
	})
	t.Run("dup pkg", func(t *testing.T) {
		err := checkUnique(load("./testdata/case_dup_pkg_a/x", "./testdata/case_dup_pkg_b/x"))
		if err == nil || !strings.Contains(err.Error(), "duplicate package name") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("ok", func(t *testing.T) {
		if err := checkUnique(load("./testdata/case_simple", "./testdata/case_write")); err != nil {
			t.Fatal(err)
		}
	})
}

func TestCheckIdentCollisions(t *testing.T) {
	load := func(paths ...string) [][]Candidate {
		var out [][]Candidate
		for _, p := range paths {
			c, err := parseCandidates(p)
			if err != nil {
				t.Fatalf("parse %s: %v", p, err)
			}
			out = append(out, c)
		}
		return out
	}
	bad := []struct {
		name  string
		paths []string
		ident string
	}{
		{"func vs method", []string{"./testdata/case_bad_ident_func_method"}, "handle_case_bad_ident_func_method_Orders_Get"},
		{"recv underscore", []string{"./testdata/case_bad_ident_recv"}, "handle_case_bad_ident_recv_A_B_C"},
		{"mixed-case pkg", []string{"./testdata/case_bad_ident_pkg_a/x", "./testdata/case_bad_ident_pkg_b/x"}, "ABCInput"},
		{"pkg case register", []string{"./testdata/case_bad_ident_pkg_c/x", "./testdata/case_bad_ident_pkg_d/x"}, "Register_Foo"},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			pkgs := load(c.paths...)
			if err := checkUnique(pkgs); err != nil {
				t.Fatalf("checkUnique 不应先失败: %v", err)
			}
			err := checkIdentCollisions(pkgs)
			if err == nil || !strings.Contains(err.Error(), "generated identifier collision") || !strings.Contains(err.Error(), c.ident) {
				t.Fatalf("err = %v", err)
			}
			if strings.Count(err.Error(), "foo.go:") != 2 {
				t.Errorf("应含两处位置: %v", err)
			}
		})
	}
	t.Run("ok", func(t *testing.T) {
		pkgs := load("./testdata/case_simple", "./testdata/case_method", "./testdata/case_ctx")
		if err := checkIdentCollisions(pkgs); err != nil {
			t.Fatal(err)
		}
	})
}
