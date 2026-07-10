package main

import (
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"webcodex/internal/protocol"
)

// server owns the public MCP endpoint and the state shared with the connected agent.
type server struct {
	publicToken string
	agentToken  string
	publicURL   string
	oauthID     string
	oauthSecret string
	timeout     time.Duration
	toolPolicy  toolPolicy
	toolCards   bool

	// queue carries requests to the single outbound agent stream. pending correlates
	// asynchronous agent results with the original MCP calls.
	queue   chan protocol.AgentRequest
	mu      sync.Mutex
	pending map[string]chan protocol.AgentResponse
}

// newServer builds the gate from environment variables and validates required secrets.
func newServer() (*server, error) {
	srv := &server{
		publicToken: env("WEBCODEX_PUBLIC_TOKEN", ""),
		agentToken:  env("WEBCODEX_AGENT_TOKEN", ""),
		publicURL:   strings.TrimRight(env("WEBCODEX_PUBLIC_URL", ""), "/"),
		oauthID:     env("WEBCODEX_OAUTH_CLIENT_ID", ""),
		oauthSecret: env("WEBCODEX_OAUTH_CLIENT_SECRET", ""),
		timeout:     durationEnv("WEBCODEX_CALL_TIMEOUT", 2*time.Minute),
		toolPolicy: newToolPolicy(
			env("WEBCODEX_ALLOWED_TOOLS", ""),
			env("WEBCODEX_DENIED_TOOLS", ""),
		),
		toolCards: boolEnv("WEBCODEX_TOOL_CARDS", false),
		queue:     make(chan protocol.AgentRequest, 128),
		pending:   make(map[string]chan protocol.AgentResponse),
	}
	if srv.publicToken == "" || srv.agentToken == "" {
		return nil, errors.New("WEBCODEX_PUBLIC_TOKEN and WEBCODEX_AGENT_TOKEN are required")
	}
	if srv.publicURL == "" {
		srv.publicURL = "http://" + env("WEBCODEX_ADDR", "127.0.0.1:8080")
	}
	return srv, nil
}

// routes declares every public endpoint served by the gate.
func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", s.handleMCP)
	mux.HandleFunc("/mcp/v2", s.handleMCP)
	mux.HandleFunc("/.well-known/oauth-protected-resource", s.handleProtectedResource)
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", s.handleProtectedResource)
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp/v2", s.handleProtectedResource)
	mux.HandleFunc("/.well-known/oauth-authorization-server", s.handleOAuthServer)
	mux.HandleFunc("/.well-known/oauth-authorization-server/mcp", s.handleOAuthServer)
	mux.HandleFunc("/.well-known/oauth-authorization-server/mcp/v2", s.handleOAuthServer)
	mux.HandleFunc("/oauth/authorize", s.handleAuthorize)
	mux.HandleFunc("/oauth/token", s.handleToken)
	mux.HandleFunc("/agent/stream", s.handleAgentStream)
	mux.HandleFunc("/agent/result", s.handleAgentResult)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}
