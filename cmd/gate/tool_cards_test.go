package main

import (
	"encoding/json"
	"testing"
)

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

	decorated, err := decorateToolCallResponse(
		response,
		"apply_patch",
		map[string]any{"patch": "*** Update File: cmd/gate/main.go"},
	)
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
