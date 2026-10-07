package integration_test

import (
	"strings"
	"testing"

	"github.com/jiaweisky/harbor/internal/agent"
	"github.com/jiaweisky/harbor/internal/config"
	"github.com/jiaweisky/harbor/internal/integration"
)

func TestNewRecognizedButUnimplemented(t *testing.T) {
	for _, in := range []config.Integration{config.IntegrationServer, config.IntegrationACP} {
		_, err := integration.New(config.AgentConfig{ID: "x", Binary: "sh", Integration: in}, true)
		if err == nil {
			t.Fatalf("%s: expected an error", in)
		}
		if !strings.Contains(err.Error(), "recognized but not implemented") {
			t.Errorf("%s: unhelpful error: %v", in, err)
		}
	}
}

func TestNewUnknownIntegration(t *testing.T) {
	_, err := integration.New(config.AgentConfig{ID: "x", Binary: "sh", Integration: "bogus"}, true)
	if err == nil || !strings.Contains(err.Error(), "unknown integration") {
		t.Fatalf("expected unknown-integration error, got %v", err)
	}
}

func TestNewRejectsIntegrationUnsupportedByBinary(t *testing.T) {
	_, err := integration.New(config.AgentConfig{ID: "c", Binary: "claude", Integration: config.IntegrationExec}, true)
	if err == nil || !strings.Contains(err.Error(), "does not support integration") {
		t.Fatalf("expected profile mismatch error, got %v", err)
	}
}

func TestNewResolvesProfileDefault(t *testing.T) {
	// codebuddy with no explicit integration resolves to its profile default.
	ln, err := integration.New(config.AgentConfig{ID: "cb", Binary: "codebuddy"}, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := ln.Capabilities().Streaming; got != agent.StreamToken {
		t.Fatalf("expected stream-json (token streaming), got %v", got)
	}
}
