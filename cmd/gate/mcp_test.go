package main

import (
	"net/http"
	"net/http/httptest"
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
