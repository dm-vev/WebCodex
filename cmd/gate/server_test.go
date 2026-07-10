package main

import (
	"strings"
	"testing"
)

func TestNewServerRequiresTokens(t *testing.T) {
	t.Setenv("WEBCODEX_PUBLIC_TOKEN", "")
	t.Setenv("WEBCODEX_AGENT_TOKEN", "")

	_, err := newServer()
	if err == nil {
		t.Fatal("newServer succeeded without required tokens")
	}
	if !strings.Contains(err.Error(), "WEBCODEX_PUBLIC_TOKEN") {
		t.Fatalf("error = %q, want missing token names", err)
	}
}
