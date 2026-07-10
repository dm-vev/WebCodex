// Command agent keeps an outbound connection to the gate and proxies calls to Codex MCP.
package main

import (
	"context"
	"log"
	"net/http"
	"strings"
	"time"
)

func main() {
	gateURL := strings.TrimRight(env("WEBCODEX_GATE_URL", ""), "/")
	token := env("WEBCODEX_AGENT_TOKEN", "")
	codexCmd := env(
		"WEBCODEX_CODEX_MCP_CMD",
		"third_party/codex/codex-rs/target/debug/codex-mcp-server",
	)
	if gateURL == "" || token == "" {
		log.Fatal("WEBCODEX_GATE_URL and WEBCODEX_AGENT_TOKEN are required")
	}

	mcp, err := startMCP(context.Background(), codexCmd)
	if err != nil {
		log.Fatalf("start codex mcp: %v", err)
	}
	if err := mcp.initialize(context.Background()); err != nil {
		log.Fatalf("initialize codex mcp: %v", err)
	}

	client := &http.Client{}
	for {
		if err := streamOnce(context.Background(), client, gateURL, token, mcp); err != nil {
			log.Printf("stream: %v", err)
			time.Sleep(time.Second)
		}
	}
}
