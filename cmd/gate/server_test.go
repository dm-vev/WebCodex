package main

import (
	"context"
	"strings"
	"testing"
	"time"
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

func TestRegisteringAgentStreamCancelsPrevious(t *testing.T) {
	srv := &server{}

	firstCtx, _, _ := srv.activateAgentStream(context.Background())
	secondCtx, second, replaced := srv.activateAgentStream(context.Background())
	defer srv.deactivateAgentStream(second)
	if !replaced {
		t.Fatal("second agent stream did not replace the first")
	}

	select {
	case <-firstCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("previous agent stream was not cancelled")
	}
	if secondCtx.Err() != nil {
		t.Fatal("new agent stream was cancelled")
	}
}
