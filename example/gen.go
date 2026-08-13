// Package example wires code generation for the runnable sample: `go generate`
// reads mcpgen.yaml and writes the typed tool wrappers into ./tools.
package example

//go:generate go run github.com/fzxbl/mcp-toolify/cmd/mcpgen -config ./mcpgen.yaml
