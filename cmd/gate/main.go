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

const (
	toolCardURI      = "ui://widget/webcodex-tool-card-v18.html"
	toolCardMIMEType = "text/html;profile=mcp-app"
)

var toolCardLegacyURIs = []string{
	"ui://widget/webcodex-tool-card-v4.html",
	"ui://widget/webcodex-tool-card-v5.html",
	"ui://widget/webcodex-tool-card-v6.html",
	"ui://widget/webcodex-tool-card-v7.html",
	"ui://widget/webcodex-tool-card-v8.html",
	"ui://widget/webcodex-tool-card-v9.html",
	"ui://widget/webcodex-tool-card-v10.html",
	"ui://widget/webcodex-tool-card-v11.html",
	"ui://widget/webcodex-tool-card-v12.html",
	"ui://widget/webcodex-tool-card-v13.html",
	"ui://widget/webcodex-tool-card-v14.html",
	"ui://widget/webcodex-tool-card-v15.html",
	"ui://widget/webcodex-tool-card-v16.html",
	"ui://widget/webcodex-tool-card-v17.html",
}

type server struct {
	publicToken string
	agentToken  string
	publicURL   string
	oauthID     string
	oauthSecret string
	timeout     time.Duration
	toolPolicy  toolPolicy
	toolCards   bool

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
		toolCards: boolEnv("WEBCODEX_TOOL_CARDS", false),
		queue:     make(chan protocol.AgentRequest, 128),
		pending:   make(map[string]chan protocol.AgentResponse),
	}
	if srv.publicToken == "" || srv.agentToken == "" {
		log.Fatal("WEBCODEX_PUBLIC_TOKEN and WEBCODEX_AGENT_TOKEN are required")
	}
	if srv.publicURL == "" {
		srv.publicURL = "http://" + env("WEBCODEX_ADDR", "127.0.0.1:8080")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", srv.handleMCP)
	mux.HandleFunc("/mcp/v2", srv.handleMCP)
	mux.HandleFunc("/.well-known/oauth-protected-resource", srv.handleProtectedResource)
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", srv.handleProtectedResource)
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp/v2", srv.handleProtectedResource)
	mux.HandleFunc("/.well-known/oauth-authorization-server", srv.handleOAuthServer)
	mux.HandleFunc("/.well-known/oauth-authorization-server/mcp", srv.handleOAuthServer)
	mux.HandleFunc("/.well-known/oauth-authorization-server/mcp/v2", srv.handleOAuthServer)
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
	authorized := bearerOK(r, s.publicToken)
	log.Printf(
		"mcp %s %s ua=%q auth=%t accept=%q content_type=%q session=%q protocol=%q",
		r.Method,
		r.URL.Path,
		r.UserAgent(),
		authorized,
		r.Header.Get("Accept"),
		r.Header.Get("Content-Type"),
		r.Header.Get("Mcp-Session-Id"),
		r.Header.Get("Mcp-Protocol-Version"),
	)
	w.Header().Set("Mcp-Protocol-Version", "2025-06-18")
	if r.Method == http.MethodGet {
		s.handleMCPStream(w, r, authorized)
		return
	}
	if r.Method == http.MethodDelete {
		if !authorized {
			s.writeMCPUnauthorized(w)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !authorized {
		s.writeMCPUnauthorized(w)
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
					"resources": map[string]any{
						"listChanged": true,
					},
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
	if result, ok := localMCPResult(msg.Method, body); ok {
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

	toolName := ""
	toolArguments := map[string]any{}
	if msg.Method == "tools/call" {
		call, err := parseToolCall(body)
		if err != nil {
			writeRPCError(w, msg.ID, -32602, err.Error())
			return
		}
		if !s.toolPolicy.allows(call.Name) {
			writeRPCError(w, msg.ID, -32602, "tool not allowed")
			return
		}
		toolName = call.Name
		toolArguments = call.Arguments
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
	if msg.Method == "tools/call" {
		if s.toolCards {
			response, err := decorateToolCallResponse(resp.Response, toolName, toolArguments)
			if err != nil {
				log.Printf("mcp response error method=%q id=%s error=%v elapsed=%s", msg.Method, string(msg.ID), err, time.Since(started))
				writeRPCError(w, msg.ID, -32000, err.Error())
				return
			}
			resp.Response = response
		}
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(resp.Response); err != nil {
		log.Printf("write mcp response: %v", err)
	}
	log.Printf("mcp response ok method=%q id=%s bytes=%d elapsed=%s", msg.Method, string(msg.ID), len(resp.Response), time.Since(started))
}

func (s *server) handleMCPStream(w http.ResponseWriter, r *http.Request, authorized bool) {
	if !authorized {
		s.writeMCPUnauthorized(w)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	if _, err := fmt.Fprint(w, ": webcodex stream ready\n\n"); err != nil {
		log.Printf("write mcp stream: %v", err)
	}
}

func (s *server) writeMCPUnauthorized(w http.ResponseWriter) {
	w.Header().Set(
		"WWW-Authenticate",
		fmt.Sprintf(`Bearer resource_metadata="%s/.well-known/oauth-protected-resource"`, s.publicURL),
	)
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

type toolCall struct {
	Name      string
	Arguments map[string]any
}

func parseToolCall(request json.RawMessage) (toolCall, error) {
	var msg struct {
		Params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"params"`
	}
	if err := json.Unmarshal(request, &msg); err != nil {
		return toolCall{}, fmt.Errorf("parse tool call: %w", err)
	}
	if msg.Params.Name == "" {
		return toolCall{}, errors.New("missing tool name")
	}
	if msg.Params.Arguments == nil {
		msg.Params.Arguments = map[string]any{}
	}
	return toolCall{Name: msg.Params.Name, Arguments: msg.Params.Arguments}, nil
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
			if s.toolCards {
				decorateToolDescriptor(toolObject, name)
			}
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

func decorateToolDescriptor(tool map[string]any, name string) {
	card := toolCard(name)
	securitySchemes := []any{map[string]any{"type": "noauth"}}
	tool["title"] = card.title
	tool["securitySchemes"] = securitySchemes
	tool["outputSchema"] = toolCardOutputSchema()
	annotations := ensureMap(tool, "annotations")
	for key, value := range card.annotations() {
		annotations[key] = value
	}
	meta := ensureMap(tool, "_meta")
	meta["securitySchemes"] = securitySchemes
	meta["ui"] = map[string]any{"resourceUri": toolCardURI}
	meta["openai/outputTemplate"] = toolCardURI
	meta["openai/toolInvocation/invoking"] = card.invoking
	meta["openai/toolInvocation/invoked"] = card.invoked
	meta["webcodex/toolIcon"] = card.icon
	meta["webcodex/toolTitle"] = name
}

func ensureMap(parent map[string]any, key string) map[string]any {
	existing, ok := parent[key].(map[string]any)
	if ok {
		return existing
	}
	next := map[string]any{}
	parent[key] = next
	return next
}

func decorateToolCallResponse(response json.RawMessage, name string, arguments map[string]any) (json.RawMessage, error) {
	var msg map[string]any
	if err := json.Unmarshal(response, &msg); err != nil {
		return nil, fmt.Errorf("parse tools/call response: %w", err)
	}
	result, ok := msg["result"].(map[string]any)
	if !ok {
		return response, nil
	}

	card := toolCard(name)
	output := toolResultText(result)
	result["structuredContent"] = map[string]any{
		"tool":          name,
		"title":         name,
		"display_title": card.title,
		"icon":          card.icon,
		"summary":       card.invoked,
		"output":        output,
		"arguments":     arguments,
		"is_error":      result["isError"] == true,
	}
	meta := ensureMap(result, "_meta")
	meta["webcodex/toolCard"] = true
	meta["webcodex/toolIcon"] = card.icon
	meta["webcodex/toolTitle"] = name
	meta["webcodex/displayTitle"] = card.title
	meta["webcodex/output"] = output
	meta["openai/outputTemplate"] = toolCardURI
	meta["ui"] = map[string]any{"resourceUri": toolCardURI}

	encoded, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("encode tools/call response: %w", err)
	}
	return encoded, nil
}

func toolResultText(result map[string]any) string {
	content, ok := result["content"].([]any)
	if !ok {
		return ""
	}
	var parts []string
	for _, item := range content {
		object, ok := item.(map[string]any)
		if !ok || object["type"] != "text" {
			continue
		}
		text, ok := object["text"].(string)
		if ok && text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}

func toolCardOutputSchema() map[string]any {
	properties := map[string]any{}
	for _, name := range []string{"tool", "title", "display_title", "icon", "summary", "output"} {
		properties[name] = map[string]any{"type": "string"}
	}
	properties["arguments"] = map[string]any{"type": "object", "additionalProperties": true}
	return map[string]any{
		"type":                 "object",
		"additionalProperties": true,
		"properties":           properties,
	}
}

type toolCardMeta struct {
	title       string
	icon        string
	invoking    string
	invoked     string
	readOnly    bool
	destructive bool
	openWorld   bool
}

func (c toolCardMeta) annotations() map[string]any {
	return map[string]any{
		"readOnlyHint":    c.readOnly,
		"destructiveHint": c.destructive,
		"openWorldHint":   c.openWorld,
		"idempotentHint":  false,
	}
}

func toolCard(name string) toolCardMeta {
	switch name {
	case "exec_command":
		return toolCardMeta{
			title: "Execute Command", icon: "terminal", invoking: "Running command...", invoked: "Command finished",
			destructive: true, openWorld: true,
		}
	case "write_stdin":
		return toolCardMeta{
			title: "Send Input", icon: "stdin", invoking: "Sending input...", invoked: "Input sent",
			destructive: true,
		}
	case "apply_patch":
		return toolCardMeta{
			title: "Apply Patch", icon: "edit", invoking: "Applying patch...", invoked: "Patch applied",
			destructive: true,
		}
	case "view_image":
		return toolCardMeta{
			title: "view_image", icon: "image", invoking: "Opening image...", invoked: "Image ready",
			readOnly: true,
		}
	case "update_plan":
		return toolCardMeta{
			title: "update_plan", icon: "plan", invoking: "Updating plan...", invoked: "Plan updated",
			destructive: true,
		}
	case "request_user_input":
		return toolCardMeta{
			title: "request_user_input", icon: "question", invoking: "Preparing question...", invoked: "Question ready",
			destructive: true,
		}
	case "get_goal":
		return toolCardMeta{
			title: "get_goal", icon: "target", invoking: "Reading goal...", invoked: "Goal loaded",
			readOnly: true,
		}
	case "create_goal":
		return toolCardMeta{
			title: "create_goal", icon: "target-plus", invoking: "Creating goal...", invoked: "Goal created",
			destructive: true,
		}
	case "update_goal":
		return toolCardMeta{
			title: "update_goal", icon: "target-check", invoking: "Updating goal...", invoked: "Goal updated",
			destructive: true,
		}
	}
	if strings.Contains(name, "search") {
		return toolCardMeta{
			title: name, icon: "search", invoking: "Searching...", invoked: "Search finished",
			readOnly: true, openWorld: true,
		}
	}
	if strings.Contains(name, "read") || strings.Contains(name, "list") || strings.Contains(name, "get") {
		return toolCardMeta{
			title: name, icon: "read", invoking: "Reading...", invoked: "Read complete",
			readOnly: true,
		}
	}
	if strings.Contains(name, "image") {
		return toolCardMeta{
			title: name, icon: "image", invoking: "Working with image...", invoked: "Image result ready",
			destructive: true, openWorld: true,
		}
	}
	return toolCardMeta{
		title: name, icon: "tool", invoking: "Using " + name + "...", invoked: name + " finished",
		destructive: true,
	}
}

func readableToolTitle(name string) string {
	name = strings.TrimPrefix(name, "mcp__")
	name = strings.ReplaceAll(name, "__", " ")
	name = strings.ReplaceAll(name, "_", " ")
	return strings.TrimSpace(name)
}

func localMCPResult(method string, request json.RawMessage) (map[string]any, bool) {
	switch method {
	case "ping":
		return map[string]any{}, true
	case "resources/list":
		resources := []any{toolCardResource(toolCardURI)}
		for _, uri := range toolCardLegacyURIs {
			resources = append(resources, toolCardResource(uri))
		}
		return map[string]any{"resources": resources}, true
	case "resources/read":
		uri, ok := resourceReadURI(request)
		if !ok || !toolCardURIKnown(uri) {
			return map[string]any{"contents": []any{}}, true
		}
		return map[string]any{
			"contents": []any{
				map[string]any{
					"uri":      uri,
					"mimeType": toolCardMIMEType,
					"text":     toolCardHTML(),
					"_meta": map[string]any{
						"ui": map[string]any{
							"prefersBorder": true,
							"csp": map[string]any{
								"connectDomains":  []string{},
								"resourceDomains": []string{},
							},
						},
						"openai/widgetDescription":   "Compact WebCodex tool call result.",
						"openai/widgetPrefersBorder": true,
						"openai/widgetCSP": map[string]any{
							"connect_domains":  []string{},
							"resource_domains": []string{},
						},
					},
				},
			},
		}, true
	case "resources/templates/list":
		return map[string]any{"resourceTemplates": []any{}}, true
	case "prompts/list":
		return map[string]any{"prompts": []any{}}, true
	default:
		return nil, false
	}
}

func toolCardResource(uri string) map[string]any {
	return map[string]any{
		"uri":         uri,
		"name":        "WebCodex tool card",
		"title":       "WebCodex tool card",
		"mimeType":    toolCardMIMEType,
		"description": "Renders Codex tool calls inside ChatGPT Web.",
	}
}

func toolCardURIKnown(uri string) bool {
	if uri == toolCardURI {
		return true
	}
	for _, legacy := range toolCardLegacyURIs {
		if uri == legacy {
			return true
		}
	}
	return false
}

func resourceReadURI(request json.RawMessage) (string, bool) {
	var msg struct {
		Params struct {
			URI string `json:"uri"`
		} `json:"params"`
	}
	if err := json.Unmarshal(request, &msg); err != nil {
		return "", false
	}
	return msg.Params.URI, msg.Params.URI != ""
}

func toolCardHTML() string {
	return `<!doctype html>
<html>
<head>
  <meta charset="utf-8">
  <style>
    :root{color-scheme:light;--panel:#fff;--line:#d9dddf;--text:#1f2223;--muted:#62696b;--soft:#eef1f0;--green:#1f7a4d;--green-soft:#e6f3eb;--amber:#9a5b00;--amber-soft:#fff3d9;--red:#a33a32;--red-soft:#fde9e7;--code:#101214}
    *{box-sizing:border-box}
    body{margin:0;background:transparent;color:var(--text);font:14px/1.45 Inter,ui-sans-serif,system-ui,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif}
    .card{border:1px solid var(--line);border-radius:8px;background:var(--panel);overflow:hidden;box-shadow:0 1px 1px rgba(31,34,35,.04)}
    .head{display:grid;grid-template-columns:32px minmax(0,1fr) minmax(24px,auto);gap:12px;align-items:center;padding:13px 16px}
    .has-body .head{border-bottom:1px solid var(--line)}
    .icon{display:grid;place-items:center;width:32px;height:32px;border-radius:7px;background:transparent;color:#000;overflow:hidden}
    .icon svg{width:24px;height:24px;display:block}
    .title{min-width:0}
    .title strong,.title span{display:block;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
    .title strong{font-size:14px;font-weight:650}
    .title span{margin-top:2px;color:var(--muted);font-size:12px}
    .pill{padding:4px 8px;border-radius:999px;background:var(--green-soft);color:var(--green);font-size:12px;font-weight:650;white-space:nowrap}
    .pill.warn{background:var(--amber-soft);color:var(--amber)}
    .pill.bad{background:var(--red-soft);color:var(--red)}
    .spinner{justify-self:end;width:16px;height:16px;border:2px solid #d9dddf;border-top-color:#1f2223;border-radius:50%;animation:spin .8s linear infinite}
    @keyframes spin{to{transform:rotate(360deg)}}
    .body{padding:13px 16px;display:none}
    .loaded.has-body .body{display:grid;gap:12px}
    .loaded .spinner{display:none}
    .loading .pill{display:none}
    .status{display:inline-flex;gap:6px;align-items:center;justify-self:end;white-space:nowrap}
    .duration{color:var(--text);font-size:14px;font-weight:650;white-space:nowrap}
    .terminal{margin:0;padding:12px;overflow:auto;border-radius:7px;background:var(--code);color:#d6d9da;font:12px/1.5 "SFMono-Regular",Consolas,"Liberation Mono",monospace}
    .collapse-btn{display:none;width:max-content;margin-top:2px;padding:0;border:0;background:transparent;color:var(--muted);font:12px/1.45 inherit;cursor:pointer}
    .has-output .collapse-btn{display:block}
    .has-output #sub{display:none}
    .collapse-btn:hover{color:var(--text);text-decoration:underline}
    .term-line{white-space:pre-wrap;overflow-wrap:anywhere}
    .t-dim{color:#7f878a}.t-green{color:#55d17a}.t-blue{color:#6ab4ff}.t-yellow{color:#f2c94c}.t-red{color:#ff6b64}.t-bold{font-weight:700}
    .files{display:grid;gap:6px}
    .changes summary{cursor:pointer;color:var(--muted);font-size:12px;list-style-position:inside}
    .changes[open] summary{margin-bottom:6px}
    .file{display:flex;justify-content:space-between;gap:12px;padding:8px 10px;border:1px solid var(--line);border-radius:7px;background:#fbfcfc}
    .file code{min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
    .file span{flex:0 0 auto;color:var(--muted);font-size:12px}
    .hidden{display:none!important}
    @media(max-width:640px){.head{grid-template-columns:32px minmax(0,1fr) minmax(24px,auto);padding:13px 16px}.body{padding:13px 16px}.spinner{grid-column:auto}.status{grid-column:auto;grid-row:auto;justify-self:end}.pill{width:max-content}}
  </style>
</head>
<body>
  <div id="card" class="card loading">
    <div class="head">
      <div class="icon"><svg id="icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"></svg></div>
		      <div class="title"><strong id="title">Execute Command</strong><span id="sub">Running command...</span><button id="collapseBtn" class="collapse-btn" type="button">collapse</button></div>
      <div class="spinner" aria-label="Loading"></div>
	      <div class="status"><span id="duration" class="duration"></span><span id="pill" class="pill">completed</span></div>
    </div>
    <div class="body">
		      <div id="files" class="files hidden"></div>
		      <div id="outputWrap"><div id="terminal" class="terminal" role="img" aria-label="VT100 terminal output"></div></div>
    </div>
  </div>
  <script>
    const icons = {
      terminal: '<path d="M4 17V7a2 2 0 0 1 2-2h12a2 2 0 0 1 2 2v10a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2Z"/><path d="m8 9 3 3-3 3M13 15h3"/>',
      stdin: '<path d="M4 7h16M4 12h9M4 17h6"/><path d="m15 15 2 2 4-4"/>',
      edit: '<path d="M4 20h16"/><path d="M6 16.5V19h2.5L18.8 8.7a2.1 2.1 0 0 0 0-3l-.5-.5a2.1 2.1 0 0 0-3 0z"/><path d="m14 6 4 4"/>',
      image: '<rect x="4" y="5" width="16" height="14" rx="2"/><path d="m4 15 4-4 4 4 2-2 6 6"/><circle cx="15" cy="9" r="1"/>',
      read: '<path d="M4 19.5V5.8A2.8 2.8 0 0 1 6.8 3H20v16H6.8A2.8 2.8 0 0 0 4 21.8"/><path d="M8 7h8M8 11h8"/>',
      search: '<circle cx="11" cy="11" r="7"/><path d="m16 16 4 4"/>',
      plan: '<path d="M8 6h13M8 12h13M8 18h13"/><path d="M3 6h.01M3 12h.01M3 18h.01"/>',
      question: '<path d="M12 18h.01"/><path d="M9.1 9a3 3 0 1 1 5.8 1c-.7 1.2-2.1 1.6-2.6 2.9"/>',
      target: '<circle cx="12" cy="12" r="8"/><circle cx="12" cy="12" r="3"/>',
      "target-plus": '<circle cx="12" cy="12" r="8"/><path d="M12 8v8M8 12h8"/>',
      "target-check": '<circle cx="12" cy="12" r="8"/><path d="m8.5 12 2.3 2.3 4.7-5"/>',
      tool: '<path d="M14.7 6.3a4 4 0 0 0-5 5L4 17v3h3l5.7-5.7a4 4 0 0 0 5-5l-2.4 2.4-2.8-2.8z"/>'
    };
	    const toolIcons = {
	      exec_command: "terminal",
	      write_stdin: "stdin",
      apply_patch: "edit",
      view_image: "image",
      update_plan: "plan",
      request_user_input: "question",
      get_goal: "target",
      create_goal: "target-plus",
	      update_goal: "target-check"
	    };
	    const toolTitles = {
	      exec_command: "Execute Command",
	      write_stdin: "Send Input",
	      apply_patch: "Apply Patch",
	      view_image: "View Image",
	      update_plan: "Update Plan",
	      request_user_input: "Ask User",
	      get_goal: "Read Goal",
	      create_goal: "Create Goal",
	      update_goal: "Update Goal"
	    };
    function extract(value) {
      if (!value || typeof value !== "object") return {};
      const meta = metaFields(value);
      if (value.tool || value.title || value.output || value.arguments) return { ...meta, ...value };
      const text = contentText(value);
      const candidates = [
        value.structuredContent,
        value.toolOutput?.structuredContent,
        value.toolOutput,
        value.toolResponseMetadata?.structuredContent,
        value.toolResponseMetadata?.mcp_tool_result?.structuredContent,
        value.toolResponseMetadata?.call_tool_result?.structuredContent,
        value.mcp_tool_result?.structuredContent,
        value.call_tool_result?.structuredContent,
        value.result?.structuredContent
      ];
      for (const candidate of candidates) {
        if (candidate && typeof candidate === "object") {
          return { ...meta, ...candidate, output: candidate.output || meta.output || text || "" };
        }
      }
      return meta.output || text ? { ...meta, output: meta.output || text } : meta;
    }
    function metaFields(value) {
      const meta = value._meta || value.toolResponseMetadata || value.toolOutput?._meta || value.result?._meta || {};
      if (!meta || typeof meta !== "object") return {};
      return {
        display_title: meta["webcodex/displayTitle"],
        output: meta["webcodex/output"],
        icon: meta["webcodex/toolIcon"]
      };
    }
    function contentText(value) {
      const pools = [value.content, value.toolOutput?.content, value.result?.content, value.mcp_tool_result?.content, value.call_tool_result?.content];
      for (const content of pools) {
        if (!Array.isArray(content)) continue;
        const parts = [];
        for (const item of content) {
          if (item && typeof item === "object" && item.type === "text" && typeof item.text === "string") parts.push(item.text);
        }
        if (parts.length) return parts.join("\n");
      }
      return "";
    }
    function globals() {
      return window.openai || {};
    }
    function shellPart(value) {
      const text = String(value);
      return /^[A-Za-z0-9_./:=+-]+$/.test(text) ? text : JSON.stringify(text);
    }
    function commandText(args) {
      if (!args || typeof args !== "object") return "";
      if (Array.isArray(args.cmd)) return args.cmd.map(shellPart).join(" ");
      if (typeof args.cmd === "string") return args.cmd;
      if (typeof args.command === "string") return args.command;
      if (typeof args.chars === "string") return args.chars.replace(/\s+/g, " ").trim();
      if (typeof args.path === "string") return args.path;
      return "";
    }
    function parseJSON(text) {
      if (!text || typeof text !== "string") return null;
      const trimmed = text.trim();
      if (!trimmed.startsWith("{") && !trimmed.startsWith("[")) return null;
      try { return JSON.parse(trimmed); } catch { return null; }
    }
    function patchFiles(args) {
      const patch = args && typeof args.patch === "string" ? args.patch : "";
      if (!patch) return [];
      const seen = new Set();
      const files = [];
      for (const line of patch.split(/\r?\n/)) {
        const match = line.match(/^\*\*\* (?:Add|Update|Delete) File: (.+)$/);
        if (!match || seen.has(match[1])) continue;
        seen.add(match[1]);
        files.push({ path: match[1], stat: line.includes("Add File") ? "add" : line.includes("Delete File") ? "delete" : "edit" });
      }
      return files;
    }
    function ansi(text) {
      const root = document.createDocumentFragment();
      const lines = String(text || "").replace(/\x1b\][^\x07]*(?:\x07|\x1b\\)/g, "").split(/\r?\n/);
      for (const line of lines) {
        const div = document.createElement("div");
        div.className = "term-line";
        let cls = "";
        let bold = false;
        const parts = line.split(/(\x1b\[[0-9;]*m)/g);
        for (const part of parts) {
          const m = part.match(/^\x1b\[([0-9;]*)m$/);
          if (m) {
            for (const code of (m[1] || "0").split(";").map((x) => Number(x || 0))) {
              if (code === 0) { cls = ""; bold = false; }
              if (code === 1) bold = true;
              if (code === 2) cls = "t-dim";
              if (code === 31) cls = "t-red";
              if (code === 32) cls = "t-green";
              if (code === 33) cls = "t-yellow";
              if (code === 34) cls = "t-blue";
              if (code >= 90 && code <= 97) cls = "t-dim";
            }
            continue;
          }
          if (!part) continue;
          const span = document.createElement("span");
          span.className = [cls, bold ? "t-bold" : ""].filter(Boolean).join(" ");
          span.textContent = part;
          div.appendChild(span);
        }
        root.appendChild(div);
      }
      return root;
    }
	    function escapeHTML(text) {
	      return text.replace(/[&<>"']/g, (ch) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[ch]));
	    }
	    function durationText(data, output) {
	      if (data.duration) return formatDuration(data.duration);
	      if (data.duration_ms) return formatDuration(Number(data.duration_ms) / 1000);
	      const match = String(output || "").match(/Wall time:\s*([^\n]+)/);
	      return match ? formatDuration(match[1].trim()) : "";
	    }
	    function formatDuration(value) {
	      const text = String(value).trim();
	      const match = text.match(/([0-9]+(?:\.[0-9]+)?)/);
	      const seconds = match ? Number(match[1]) : NaN;
	      if (Number.isFinite(seconds)) return seconds.toFixed(1) + "s";
	      return text.replace(/\s*seconds?$/i, "s");
	    }
    function renderFiles(files) {
      const box = document.getElementById("files");
      box.innerHTML = "";
      if (!files.length) {
        box.classList.add("hidden");
        return;
      }
      box.classList.remove("hidden");
      const shown = files.slice(0, 3);
      for (const file of shown) {
        const row = document.createElement("div");
        row.className = "file";
        row.innerHTML = '<code></code><span></span>';
        row.querySelector("code").textContent = file.path;
        row.querySelector("span").textContent = file.stat || "changed";
        box.appendChild(row);
      }
      if (files.length > shown.length) {
        const details = document.createElement("details");
        details.className = "changes";
        const summary = document.createElement("summary");
        summary.textContent = String(files.length - shown.length) + " more files";
        details.appendChild(summary);
        const inner = document.createElement("div");
        inner.className = "files";
        for (const file of files.slice(shown.length)) {
          const row = document.createElement("div");
          row.className = "file";
          row.innerHTML = '<code></code><span></span>';
          row.querySelector("code").textContent = file.path;
          row.querySelector("span").textContent = file.stat || "changed";
          inner.appendChild(row);
        }
        details.appendChild(inner);
        box.appendChild(details);
      }
    }
		    let rendered = false;
		    let state = {};
		    let collapsed = false;
		    let sawOutput = false;
	    function mergeState(next) {
	      next = next || {};
	      for (const [key, value] of Object.entries(next)) {
	        if (value === undefined || value === null || value === "") continue;
	        if (Array.isArray(value) && value.length === 0) continue;
	        if (typeof value === "object" && !Array.isArray(value) && Object.keys(value).length === 0) continue;
	        if (key === "arguments" && typeof state.arguments === "object" && typeof value === "object") {
	          state.arguments = { ...state.arguments, ...value };
	        } else {
	          state[key] = value;
	        }
	      }
	      return state;
	    }
	    function render(data) {
	      if ((!data || !Object.keys(data).length) && !Object.keys(state).length) return;
	      data = mergeState(data);
	      const input = globals().toolInput || {};
      const args = data.arguments && typeof data.arguments === "object" ? data.arguments : input;
      const parsedOutput = parseJSON(data.output);
      const merged = parsedOutput && typeof parsedOutput === "object" && !Array.isArray(parsedOutput) ? { ...parsedOutput, ...data } : data;
      const tool = merged.tool || merged.title || "exec_command";
	      const displayTitle = merged.display_title || merged.displayTitle || toolTitles[tool] || tool;
      const cmd = commandText(args) || commandText(merged.arguments) || merged.command || "";
      const icon = merged.icon || toolIcons[tool] || "tool";
	      const files = Array.isArray(merged.files) ? merged.files : patchFiles(args);
	      const output = String(merged.output || merged.text || merged.stdout || "");
	      if (output && !sawOutput) {
	        collapsed = true;
	        sawOutput = true;
	      }
	      const isError = merged.is_error || merged.isError || /(^|\n)(error|failed|panic):/i.test(output);
      document.getElementById("icon").innerHTML = icons[icon] || icons.tool;
	      document.getElementById("title").textContent = displayTitle;
	      document.getElementById("sub").textContent = tool === "exec_command" ? "Running command..." : "Running tool...";
	      document.getElementById("collapseBtn").textContent = collapsed ? "expand" : "collapse";
	      const pill = document.getElementById("pill");
	      document.getElementById("duration").textContent = durationText(merged, output);
	      pill.textContent = isError ? "error" : files.length ? files.length + " files changed" : "completed";
	      pill.className = "pill" + (isError ? " bad" : files.length ? " warn" : "");
	      renderFiles(files);
	      const terminal = document.getElementById("terminal");
	      const outputWrap = document.getElementById("outputWrap");
	      terminal.innerHTML = "";
	      terminal.appendChild(ansi((cmd ? "$ " + cmd + "\n" : "") + output));
	      outputWrap.classList.toggle("hidden", !output);
		      const hasOutput = Boolean(output);
		      const hasBody = Boolean(!collapsed && (hasOutput || files.length));
	      document.getElementById("card").className = (hasOutput ? "card loaded has-output" : "card loading") + (hasBody ? " has-body" : "");
	      rendered = hasOutput || hasBody;
    }
    function renderGlobals() {
      render(extract(globals().toolOutput || globals().toolResponseMetadata || {}));
    }
    renderGlobals();
    window.addEventListener("openai:set_globals", (event) => {
      render(extract(
        event.detail?.globals?.toolOutput ||
        event.detail?.globals?.toolResponseMetadata ||
        event.detail?.globals?.mcp_tool_result ||
        event.detail ||
        globals().toolOutput ||
        globals().toolResponseMetadata ||
        {}
      ));
    }, { passive: true });
	    window.addEventListener("message", (event) => {
      if (event.source !== window.parent) return;
      const message = event.data;
      if (!message || message.jsonrpc !== "2.0") return;
      if (message.method === "ui/notifications/tool-result") {
        render(extract(message.params || {}));
      }
	    }, { passive: true });
	    document.getElementById("collapseBtn").addEventListener("click", () => {
	      const button = document.getElementById("collapseBtn");
	      collapsed = !collapsed;
	      button.textContent = collapsed ? "expand" : "collapse";
	      render(state);
	    });
    let polls = 0;
    const timer = setInterval(() => {
      if (rendered || polls++ > 40) {
        clearInterval(timer);
        return;
      }
      renderGlobals();
    }, 250);
  </script>
</body>
</html>`
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

func boolEnv(name string, fallback bool) bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	switch value {
	case "":
		return fallback
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}
