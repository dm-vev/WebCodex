package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

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

func decorateToolCallResponse(
	response json.RawMessage,
	name string,
	arguments map[string]any,
) (json.RawMessage, error) {
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
