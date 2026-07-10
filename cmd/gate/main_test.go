package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandleMCPStream(t *testing.T) {
	srv := &server{publicToken: "secret"}
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Accept", "text/event-stream")
	rec := httptest.NewRecorder()

	srv.handleMCP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("content type = %q, want text/event-stream", rec.Header().Get("Content-Type"))
	}
}

func TestToolPolicyFiltersToolsList(t *testing.T) {
	srv := &server{
		toolPolicy: newToolPolicy("exec_command,apply_patch", "apply_patch"),
	}
	response := json.RawMessage(`{
		"jsonrpc":"2.0",
		"id":1,
		"result":{
			"tools":[
				{"name":"exec_command"},
				{"name":"apply_patch"},
				{"name":"read_mcp_resource"}
			]
		}
	}`)

	filtered, err := srv.filterToolsList(response)
	if err != nil {
		t.Fatalf("filter tools list: %v", err)
	}

	var msg struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(filtered, &msg); err != nil {
		t.Fatalf("parse filtered response: %v", err)
	}
	if len(msg.Result.Tools) != 1 {
		t.Fatalf("tool count = %d, want 1", len(msg.Result.Tools))
	}
	if msg.Result.Tools[0].Name != "exec_command" {
		t.Fatalf("tool = %q, want exec_command", msg.Result.Tools[0].Name)
	}
}

func TestFilterToolsListAddsToolCardMetadata(t *testing.T) {
	srv := &server{
		toolPolicy: newToolPolicy("", ""),
		toolCards:  true,
	}
	response := json.RawMessage(`{
		"jsonrpc":"2.0",
		"id":1,
		"result":{"tools":[{"name":"exec_command"}]}
	}`)

	filtered, err := srv.filterToolsList(response)
	if err != nil {
		t.Fatalf("filter tools list: %v", err)
	}

	var msg struct {
		Result struct {
			Tools []struct {
				Annotations  map[string]any `json:"annotations"`
				Meta         map[string]any `json:"_meta"`
				OutputSchema map[string]any `json:"outputSchema"`
				Title        string         `json:"title"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(filtered, &msg); err != nil {
		t.Fatalf("parse filtered response: %v", err)
	}
	tool := msg.Result.Tools[0]
	if tool.Meta["openai/outputTemplate"] != toolCardURI {
		t.Fatalf("output template = %v, want %q", tool.Meta["openai/outputTemplate"], toolCardURI)
	}
	if tool.Meta["securitySchemes"] == nil {
		t.Fatal("securitySchemes meta is missing")
	}
	if tool.Title != "Execute Command" {
		t.Fatalf("title = %q, want Execute Command", tool.Title)
	}
	if tool.OutputSchema["type"] != "object" {
		t.Fatalf("output schema type = %v, want object", tool.OutputSchema["type"])
	}
	if tool.Meta["webcodex/toolIcon"] != "terminal" {
		t.Fatalf("tool icon = %v, want terminal", tool.Meta["webcodex/toolIcon"])
	}
	if tool.Annotations["openWorldHint"] != true {
		t.Fatalf("openWorldHint = %v, want true", tool.Annotations["openWorldHint"])
	}
}

func TestFilterToolsListLeavesToolCardsDisabledByDefault(t *testing.T) {
	srv := &server{toolPolicy: newToolPolicy("", "")}
	response := json.RawMessage(`{
		"jsonrpc":"2.0",
		"id":1,
		"result":{"tools":[{"name":"exec_command"}]}
	}`)

	filtered, err := srv.filterToolsList(response)
	if err != nil {
		t.Fatalf("filter tools list: %v", err)
	}

	var msg struct {
		Result struct {
			Tools []struct {
				Meta map[string]any `json:"_meta"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(filtered, &msg); err != nil {
		t.Fatalf("parse filtered response: %v", err)
	}
	if msg.Result.Tools[0].Meta != nil {
		t.Fatalf("meta = %#v, want nil", msg.Result.Tools[0].Meta)
	}
}

func TestDecorateToolCallResponseAddsStructuredContent(t *testing.T) {
	response := json.RawMessage(`{
		"jsonrpc":"2.0",
		"id":1,
		"result":{"content":[{"type":"text","text":"ok"}]}
	}`)

	decorated, err := decorateToolCallResponse(response, "apply_patch", map[string]any{"patch": "*** Update File: cmd/gate/main.go"})
	if err != nil {
		t.Fatalf("decorate tool call: %v", err)
	}

	var msg struct {
		Result struct {
			StructuredContent map[string]any `json:"structuredContent"`
			Meta              map[string]any `json:"_meta"`
		} `json:"result"`
	}
	if err := json.Unmarshal(decorated, &msg); err != nil {
		t.Fatalf("parse decorated response: %v", err)
	}
	if msg.Result.StructuredContent["icon"] != "edit" {
		t.Fatalf("icon = %v, want edit", msg.Result.StructuredContent["icon"])
	}
	if msg.Result.StructuredContent["display_title"] != "Apply Patch" {
		t.Fatalf("display title = %v, want Apply Patch", msg.Result.StructuredContent["display_title"])
	}
	if msg.Result.StructuredContent["output"] != "ok" {
		t.Fatalf("output = %v, want ok", msg.Result.StructuredContent["output"])
	}
	if _, ok := msg.Result.StructuredContent["arguments"].(map[string]any); !ok {
		t.Fatalf("arguments = %#v, want object", msg.Result.StructuredContent["arguments"])
	}
	if msg.Result.Meta["webcodex/toolCard"] != true {
		t.Fatalf("tool card meta = %v, want true", msg.Result.Meta["webcodex/toolCard"])
	}
	if msg.Result.Meta["webcodex/output"] != "ok" {
		t.Fatalf("meta output = %v, want ok", msg.Result.Meta["webcodex/output"])
	}
	if msg.Result.Meta["openai/outputTemplate"] != toolCardURI {
		t.Fatalf("result output template = %v, want %q", msg.Result.Meta["openai/outputTemplate"], toolCardURI)
	}
}

func TestLocalMCPResultReadsToolCardResource(t *testing.T) {
	request := json.RawMessage(`{"params":{"uri":"ui://widget/webcodex-tool-card-v18.html"}}`)

	result, ok := localMCPResult("resources/read", request)
	if !ok {
		t.Fatal("resources/read was not handled locally")
	}
	contents, ok := result["contents"].([]any)
	if !ok || len(contents) != 1 {
		t.Fatalf("contents = %#v, want one item", result["contents"])
	}
	content, ok := contents[0].(map[string]any)
	if !ok {
		t.Fatalf("content item = %#v, want object", contents[0])
	}
	if content["mimeType"] != toolCardMIMEType {
		t.Fatalf("mimeType = %v, want %s", content["mimeType"], toolCardMIMEType)
	}
	if !strings.Contains(content["text"].(string), "mergeState") {
		t.Fatal("tool card html is missing stable render state")
	}
	if !strings.Contains(content["text"].(string), "collapse") {
		t.Fatal("tool card html is missing collapse label")
	}
	if !strings.Contains(content["text"].(string), "collapseBtn") {
		t.Fatal("tool card html is missing collapse button")
	}
	if !strings.Contains(content["text"].(string), `"$ " + cmd`) {
		t.Fatal("tool card html does not render command in terminal")
	}
	if !strings.Contains(content["text"].(string), "durationText") {
		t.Fatal("tool card html is missing duration parser")
	}
	if !strings.Contains(content["text"].(string), "formatDuration") {
		t.Fatal("tool card html is missing duration formatter")
	}
	if !strings.Contains(content["text"].(string), "32px minmax(0,1fr) minmax(24px,auto)") {
		t.Fatal("tool card html is missing mobile status layout")
	}
	if !strings.Contains(content["text"].(string), "padding:13px 16px") {
		t.Fatal("tool card html is missing symmetric padding")
	}
}

func TestLocalMCPResultReadsLegacyToolCardResource(t *testing.T) {
	request := json.RawMessage(`{"params":{"uri":"ui://widget/webcodex-tool-card-v6.html"}}`)

	result, ok := localMCPResult("resources/read", request)
	if !ok {
		t.Fatal("resources/read was not handled locally")
	}
	contents := result["contents"].([]any)
	content := contents[0].(map[string]any)
	if content["uri"] != "ui://widget/webcodex-tool-card-v6.html" {
		t.Fatalf("uri = %v, want legacy uri", content["uri"])
	}
	if !strings.Contains(content["text"].(string), "Execute Command") {
		t.Fatal("legacy resource did not return latest html")
	}
}
