package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"strings"
)

const (
	toolCardURI      = "ui://widget/webcodex-tool-card-v18.html"
	toolCardMIMEType = "text/html;profile=mcp-app"
)

// Legacy resource URIs keep existing ChatGPT app versions working after card updates.
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

// toolCardTemplates is embedded so the gate remains a single deployable binary.
//
//go:embed templates/tool-card.html
var toolCardTemplates embed.FS

var toolCardTemplate = template.Must(
	template.ParseFS(toolCardTemplates, "templates/tool-card.html"),
)

func renderToolCardHTML() (string, error) {
	var rendered strings.Builder
	if err := toolCardTemplate.ExecuteTemplate(&rendered, "tool-card.html", nil); err != nil {
		return "", fmt.Errorf("render tool card: %w", err)
	}
	return rendered.String(), nil
}

// localMCPResult handles MCP requests that do not require the connected local agent.
func localMCPResult(
	method string,
	request json.RawMessage,
	toolCards bool,
) (map[string]any, bool, error) {
	switch method {
	case "ping":
		return map[string]any{}, true, nil
	case "resources/list":
		if !toolCards {
			return map[string]any{"resources": []any{}}, true, nil
		}
		resources := []any{toolCardResource(toolCardURI)}
		for _, uri := range toolCardLegacyURIs {
			resources = append(resources, toolCardResource(uri))
		}
		return map[string]any{"resources": resources}, true, nil
	case "resources/read":
		if !toolCards {
			return map[string]any{"contents": []any{}}, true, nil
		}
		uri, ok := resourceReadURI(request)
		if !ok || !toolCardURIKnown(uri) {
			return map[string]any{"contents": []any{}}, true, nil
		}
		html, err := renderToolCardHTML()
		if err != nil {
			return nil, true, err
		}
		return map[string]any{
			"contents": []any{
				map[string]any{
					"uri":      uri,
					"mimeType": toolCardMIMEType,
					"text":     html,
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
		}, true, nil
	case "resources/templates/list":
		return map[string]any{"resourceTemplates": []any{}}, true, nil
	case "prompts/list":
		return map[string]any{"prompts": []any{}}, true, nil
	default:
		return nil, false, nil
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
