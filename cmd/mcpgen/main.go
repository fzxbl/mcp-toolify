package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type config struct {
	Output struct {
		Dir string `yaml:"dir"`
	} `yaml:"output"`
	Packages []string `yaml:"packages"`
}

func main() {
	cfgPath := flag.String("config", "mcpgen.yaml", "")
	flag.Parse()

	data, err := os.ReadFile(*cfgPath)
	check(err)
	var cfg config
	check(yaml.Unmarshal(data, &cfg))

	outDir, _ := filepath.Abs(filepath.Join(filepath.Dir(*cfgPath), cfg.Output.Dir))
	check(os.MkdirAll(outDir, 0o755))

	var pkgIdents []string
	for _, pkg := range cfg.Packages {
		cands, err := parseCandidates(pkg)
		check(err)
		if len(cands) == 0 {
			continue
		}
		check(renderPackage(outDir, cands))
		pkgIdents = append(pkgIdents, exportName(cands[0].PkgName))
	}
	check(renderAll(outDir, pkgIdents))
	fmt.Printf("generated %d packages into %s\n", len(pkgIdents), outDir)
}

func check(err error) {
	if err == nil {
		return
	}
	fmt.Fprintln(os.Stderr, "mcpgen:", err)
	os.Exit(1)
}
