// Command server is a minimal mcp-toolify server exposing the example tools.
//
// Regenerate wrappers after editing tool functions:
//
//	go generate ./...
//
// Run over stdio (for MCP clients that spawn a subprocess):
//
//	go run ./example/cmd/server
//
// Run over HTTP:
//
//	go run ./example/cmd/server -transport http -addr :8080
package main

import (
	"context"
	"flag"
	"log"

	toolify "github.com/fzxbl/mcp-toolify"
	"github.com/fzxbl/mcp-toolify/example/tools"
)

func main() {
	transport := flag.String("transport", "stdio", "transport: stdio | http")
	addr := flag.String("addr", ":8080", "http listen addr (used when -transport=http)")
	flag.Parse()

	cfg := toolify.Config{Transport: *transport, Addr: *addr}
	if err := toolify.Start(context.Background(), cfg, tools.RegisterAll); err != nil {
		log.Fatalf("mcp-toolify server exited: %v", err)
	}
}
