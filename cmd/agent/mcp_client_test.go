package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestInitializeRequestIsOneJSONRPCLine(t *testing.T) {
	if strings.Contains(initializeRequest, "\n") {
		t.Fatal("initialize request contains a newline")
	}

	var request struct {
		JSONRPC string `json:"jsonrpc"`
		ID      string `json:"id"`
		Method  string `json:"method"`
	}
	if err := json.Unmarshal([]byte(initializeRequest), &request); err != nil {
		t.Fatalf("parse initialize request: %v", err)
	}
	if request.JSONRPC != "2.0" || request.ID != "webcodex-init" || request.Method != "initialize" {
		t.Fatalf("unexpected initialize request: %#v", request)
	}
}
