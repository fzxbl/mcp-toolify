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
