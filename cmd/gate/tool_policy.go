package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// toolPolicy applies the deny list before the optional allow list.
type toolPolicy struct {
	allowed map[string]bool
	denied  map[string]bool
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

// filterToolsList applies the configured policy and optional ChatGPT card metadata.
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
