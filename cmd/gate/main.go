package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"webcodex/internal/protocol"
)

type jsonrpcMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
}

type server struct {
	publicToken string
	agentToken  string
	publicURL   string
	oauthID     string
	oauthSecret string
	timeout     time.Duration
	toolPolicy  toolPolicy

	queue   chan protocol.AgentRequest
	mu      sync.Mutex
	pending map[string]chan protocol.AgentResponse
}

type toolPolicy struct {
	allowed map[string]bool
	denied  map[string]bool
}

func main() {
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
		queue:   make(chan protocol.AgentRequest, 128),
		pending: make(map[string]chan protocol.AgentResponse),
	}
	if srv.publicToken == "" || srv.agentToken == "" {
		log.Fatal("WEBCODEX_PUBLIC_TOKEN and WEBCODEX_AGENT_TOKEN are required")
	}
	if srv.publicURL == "" {
		srv.publicURL = "http://" + env("WEBCODEX_ADDR", "127.0.0.1:8080")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", srv.handleMCP)
	mux.HandleFunc("/.well-known/oauth-protected-resource", srv.handleProtectedResource)
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", srv.handleProtectedResource)
	mux.HandleFunc("/.well-known/oauth-authorization-server", srv.handleOAuthServer)
	mux.HandleFunc("/.well-known/oauth-authorization-server/mcp", srv.handleOAuthServer)
	mux.HandleFunc("/oauth/authorize", srv.handleAuthorize)
	mux.HandleFunc("/oauth/token", srv.handleToken)
	mux.HandleFunc("/agent/stream", srv.handleAgentStream)
	mux.HandleFunc("/agent/result", srv.handleAgentResult)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	addr := env("WEBCODEX_ADDR", ":8080")
	log.Printf("gate listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("listen: %v", err)
	}
}

func (s *server) handleMCP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	log.Printf("mcp %s %s ua=%q auth=%t", r.Method, r.URL.Path, r.UserAgent(), bearerOK(r, s.publicToken))
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !bearerOK(r, s.publicToken) {
		w.Header().Set(
			"WWW-Authenticate",
			fmt.Sprintf(`Bearer resource_metadata="%s/.well-known/oauth-protected-resource"`, s.publicURL),
		)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeRPCError(w, nil, -32700, "read request")
		return
	}
	var msg jsonrpcMessage
	if err := json.Unmarshal(body, &msg); err != nil {
		log.Printf("mcp invalid json bytes=%d", len(body))
		writeRPCError(w, nil, -32700, "invalid json")
		return
	}
	log.Printf("mcp request method=%q id=%s bytes=%d", msg.Method, string(msg.ID), len(body))
	if msg.Method == "initialize" {
		writeJSON(w, map[string]any{
			"jsonrpc": "2.0",
			"id":      msg.ID,
			"result": map[string]any{
				"protocolVersion": "2025-06-18",
				"capabilities": map[string]any{
					"tools": map[string]any{
						"listChanged": true,
					},
				},
				"serverInfo": map[string]string{
					"name":    "webcodex",
					"title":   "WebCodex",
					"version": "0.1.0",
				},
			},
		})
		log.Printf("mcp response ok method=%q id=%s local=true elapsed=%s", msg.Method, string(msg.ID), time.Since(started))
		return
	}
	if result, ok := localMCPResult(msg.Method); ok {
		writeJSON(w, map[string]any{
			"jsonrpc": "2.0",
			"id":      msg.ID,
			"result":  result,
		})
		log.Printf("mcp response ok method=%q id=%s local=true elapsed=%s", msg.Method, string(msg.ID), time.Since(started))
		return
	}

	if len(msg.ID) == 0 {
		if err := s.enqueue(r.Context(), protocol.AgentRequest{ID: "", Request: body}); err != nil {
			log.Printf("mcp notification enqueue method=%q error=%v", msg.Method, err)
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		log.Printf("mcp notification accepted method=%q elapsed=%s", msg.Method, time.Since(started))
		w.WriteHeader(http.StatusAccepted)
		return
	}

	if msg.Method == "tools/call" {
		toolName, err := toolCallName(body)
		if err != nil {
			writeRPCError(w, msg.ID, -32602, err.Error())
			return
		}
		if !s.toolPolicy.allows(toolName) {
			writeRPCError(w, msg.ID, -32602, "tool not allowed")
			return
		}
	}

	resp, err := s.callAgent(r, body)
	if err != nil {
		log.Printf("mcp response error method=%q id=%s error=%v elapsed=%s", msg.Method, string(msg.ID), err, time.Since(started))
		writeRPCError(w, msg.ID, -32000, err.Error())
		return
	}
	if msg.Method == "tools/list" {
		response, err := s.filterToolsList(resp.Response)
		if err != nil {
			log.Printf("mcp response error method=%q id=%s error=%v elapsed=%s", msg.Method, string(msg.ID), err, time.Since(started))
			writeRPCError(w, msg.ID, -32000, err.Error())
			return
		}
		resp.Response = response
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(resp.Response); err != nil {
		log.Printf("write mcp response: %v", err)
	}
	log.Printf("mcp response ok method=%q id=%s bytes=%d elapsed=%s", msg.Method, string(msg.ID), len(resp.Response), time.Since(started))
}

func newToolPolicy(allowedEnv, deniedEnv string) toolPolicy {
	return toolPolicy{
		allowed: parseToolSet(allowedEnv),
		denied:  parseToolSet(deniedEnv),
	}
}

func parseToolSet(value string) map[string]bool {
	result := map[string]bool{}
	for _, part := range strings.Split(value, ",") {
		name := strings.TrimSpace(part)
		if name != "" {
			result[name] = true
		}
	}
	return result
}

func (p toolPolicy) allows(name string) bool {
	if p.denied[name] {
		return false
	}
	if len(p.allowed) == 0 {
		return true
	}
	return p.allowed[name]
}

func toolCallName(request json.RawMessage) (string, error) {
	var msg struct {
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if err := json.Unmarshal(request, &msg); err != nil {
		return "", fmt.Errorf("parse tool call: %w", err)
	}
	if msg.Params.Name == "" {
		return "", errors.New("missing tool name")
	}
	return msg.Params.Name, nil
}

func (s *server) filterToolsList(response json.RawMessage) (json.RawMessage, error) {
	var msg map[string]any
	if err := json.Unmarshal(response, &msg); err != nil {
		return nil, fmt.Errorf("parse tools/list response: %w", err)
	}
	result, ok := msg["result"].(map[string]any)
	if !ok {
		return response, nil
	}
	tools, ok := result["tools"].([]any)
	if !ok {
		return response, nil
	}

	filtered := tools[:0]
	for _, tool := range tools {
		toolObject, ok := tool.(map[string]any)
		if !ok {
			filtered = append(filtered, tool)
			continue
		}
		name, ok := toolObject["name"].(string)
		if !ok || s.toolPolicy.allows(name) {
			filtered = append(filtered, tool)
		}
	}
	result["tools"] = filtered

	encoded, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("encode filtered tools/list response: %w", err)
	}
	return encoded, nil
}

func localMCPResult(method string) (map[string]any, bool) {
	switch method {
	case "ping":
		return map[string]any{}, true
	case "resources/list":
		return map[string]any{"resources": []any{}}, true
	case "resources/templates/list":
		return map[string]any{"resourceTemplates": []any{}}, true
	case "prompts/list":
		return map[string]any{"prompts": []any{}}, true
	default:
		return nil, false
	}
}

func (s *server) handleProtectedResource(w http.ResponseWriter, r *http.Request) {
	log.Printf("oauth protected-resource %s %s ua=%q", r.Method, r.URL.Path, r.UserAgent())
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, map[string]any{
		"resource":              s.publicURL + "/mcp",
		"authorization_servers": []string{s.publicURL},
	})
}

func (s *server) handleOAuthServer(w http.ResponseWriter, r *http.Request) {
	log.Printf("oauth server-metadata %s %s ua=%q", r.Method, r.URL.Path, r.UserAgent())
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, map[string]any{
		"issuer":                                s.publicURL,
		"authorization_endpoint":                s.publicURL + "/oauth/authorize",
		"token_endpoint":                        s.publicURL + "/oauth/token",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code"},
		"code_challenge_methods_supported":      []string{"S256", "plain"},
		"token_endpoint_auth_methods_supported": []string{"client_secret_post", "client_secret_basic"},
		"scopes_supported":                      []string{"mcp"},
	})
}

func (s *server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	log.Printf("oauth authorize client_id=%q redirect_uri=%q ua=%q", r.URL.Query().Get("client_id"), r.URL.Query().Get("redirect_uri"), r.UserAgent())
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.oauthID != "" && r.URL.Query().Get("client_id") != s.oauthID {
		http.Error(w, "unknown client", http.StatusUnauthorized)
		return
	}
	redirectURI := r.URL.Query().Get("redirect_uri")
	if redirectURI == "" {
		http.Error(w, "missing redirect_uri", http.StatusBadRequest)
		return
	}
	target, err := http.NewRequest(http.MethodGet, redirectURI, nil)
	if err != nil {
		http.Error(w, "bad redirect_uri", http.StatusBadRequest)
		return
	}
	query := target.URL.Query()
	query.Set("code", "webcodex")
	if state := r.URL.Query().Get("state"); state != "" {
		query.Set("state", state)
	}
	target.URL.RawQuery = query.Encode()
	http.Redirect(w, r, target.URL.String(), http.StatusFound)
}

func (s *server) handleToken(w http.ResponseWriter, r *http.Request) {
	log.Printf("oauth token %s content_type=%q ua=%q", r.Method, r.Header.Get("Content-Type"), r.UserAgent())
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	params, err := tokenParams(r)
	if err != nil {
		http.Error(w, "bad token request", http.StatusBadRequest)
		return
	}
	clientID, clientSecret := params["client_id"], params["client_secret"]
	if authID, authSecret, ok := r.BasicAuth(); ok {
		clientID, clientSecret = authID, authSecret
	}
	if s.oauthID != "" && clientID != s.oauthID {
		http.Error(w, "unknown client", http.StatusUnauthorized)
		return
	}
	if s.oauthSecret != "" && clientSecret != s.oauthSecret {
		http.Error(w, "bad client secret", http.StatusUnauthorized)
		return
	}
	if params["grant_type"] != "authorization_code" {
		http.Error(w, "unsupported grant_type", http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{
		"access_token": s.publicToken,
		"token_type":   "Bearer",
		"expires_in":   31536000,
		"scope":        "mcp",
	})
}

func tokenParams(r *http.Request) (map[string]string, error) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var values map[string]string
		if err := json.NewDecoder(r.Body).Decode(&values); err != nil {
			return nil, err
		}
		return values, nil
	}
	if err := r.ParseForm(); err != nil {
		return nil, err
	}
	values := make(map[string]string, len(r.Form))
	for key := range r.Form {
		values[key] = r.Form.Get(key)
	}
	return values, nil
}

func (s *server) callAgent(r *http.Request, request json.RawMessage) (protocol.AgentResponse, error) {
	id, err := randomID()
	if err != nil {
		return protocol.AgentResponse{}, fmt.Errorf("create request id: %w", err)
	}

	resultCh := make(chan protocol.AgentResponse, 1)
	s.mu.Lock()
	s.pending[id] = resultCh
	s.mu.Unlock()
	defer s.forget(id)

	if err := s.enqueue(r.Context(), protocol.AgentRequest{ID: id, Request: request}); err != nil {
		return protocol.AgentResponse{}, err
	}

	timer := time.NewTimer(s.timeout)
	defer timer.Stop()

	select {
	case result := <-resultCh:
		if result.Error != "" {
			return protocol.AgentResponse{}, errors.New(result.Error)
		}
		return result, nil
	case <-timer.C:
		return protocol.AgentResponse{}, errors.New("agent call timed out")
	case <-r.Context().Done():
		return protocol.AgentResponse{}, r.Context().Err()
	}
}

func (s *server) enqueue(ctx context.Context, request protocol.AgentRequest) error {
	select {
	case s.queue <- request:
		return nil
	case <-ctx.Done():
		return errors.New("request cancelled")
	default:
		return errors.New("agent queue is full or no agent is connected")
	}
}

func (s *server) handleAgentStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !bearerOK(r, s.agentToken) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")

	if _, err := fmt.Fprintln(w); err != nil {
		return
	}
	flusher.Flush()

	heartbeat := time.NewTicker(5 * time.Second)
	defer heartbeat.Stop()

	enc := json.NewEncoder(w)
	for {
		select {
		case request := <-s.queue:
			if err := enc.Encode(request); err != nil {
				return
			}
			flusher.Flush()
		case <-heartbeat.C:
			if _, err := fmt.Fprintln(w); err != nil {
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func (s *server) handleAgentResult(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !bearerOK(r, s.agentToken) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var result protocol.AgentResponse
	if err := json.NewDecoder(r.Body).Decode(&result); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	resultCh := s.pending[result.ID]
	s.mu.Unlock()
	if resultCh == nil {
		http.Error(w, "unknown request id", http.StatusNotFound)
		return
	}

	resultCh <- result
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) forget(id string) {
	s.mu.Lock()
	delete(s.pending, id)
	s.mu.Unlock()
}

func bearerOK(r *http.Request, token string) bool {
	return r.Header.Get("Authorization") == "Bearer "+token
}

func writeRPCError(w http.ResponseWriter, id json.RawMessage, code int, message string) {
	resp := map[string]any{
		"jsonrpc": "2.0",
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	}
	if len(id) != 0 {
		resp["id"] = json.RawMessage(id)
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("write rpc error: %v", err)
	}
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("write json: %v", err)
	}
}

func randomID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}

func env(name, fallback string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	return value
}

func durationEnv(name string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return duration
}
