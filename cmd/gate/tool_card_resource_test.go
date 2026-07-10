package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRenderToolCardHTMLMatchesEmbeddedTemplate(t *testing.T) {
	rendered, err := renderToolCardHTML()
	if err != nil {
		t.Fatalf("render tool card: %v", err)
	}
	source, err := toolCardTemplates.ReadFile("templates/tool-card.html")
	if err != nil {
		t.Fatalf("read embedded tool card: %v", err)
	}
	if rendered != string(source) {
		t.Fatal("rendered tool card differs from embedded template")
	}
}

func TestLocalMCPResultReadsToolCardResource(t *testing.T) {
	request := json.RawMessage(`{"params":{"uri":"ui://widget/webcodex-tool-card-v18.html"}}`)

	result, ok, err := localMCPResult("resources/read", request)
	if err != nil {
		t.Fatalf("read tool card resource: %v", err)
	}
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

	result, ok, err := localMCPResult("resources/read", request)
	if err != nil {
		t.Fatalf("read tool card resource: %v", err)
	}
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
