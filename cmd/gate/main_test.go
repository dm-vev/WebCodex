package main

import (
	"encoding/json"
	"testing"
)

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
