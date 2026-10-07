package integration_test

import (
	"strings"
	"testing"

	"github.com/jiaweisky/harbor/internal/agent"
	"github.com/jiaweisky/harbor/internal/config"
	"github.com/jiaweisky/harbor/internal/integration"
)

func newAdapter(ac config.AgentConfig) (agent.Launcher, error) {
	return integration.New(ac, integration.Options{InheritEnv: true})
}

func TestNewRecognizedButUnimplemented(t *testing.T) {
	_, err := newAdapter(config.AgentConfig{ID: "x", Binary: "sh", Integration: config.IntegrationServer})
	if err == nil {
		t.Fatal("server: expected an error")
	}
	if !strings.Contains(err.Error(), "recognized but not implemented") {
		t.Errorf("server: unhelpful error: %v", err)
	}
}

func TestNewACP(t *testing.T) {
	ln, err := newAdapter(config.AgentConfig{
		ID: "oc", Binary: "sh", Integration: config.IntegrationACP, Args: []string{"acp"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	caps := ln.Capabilities()
	if !caps.InputRequiredResume || caps.Streaming != agent.StreamToken {
		t.Fatalf("unexpected ACP capabilities: %+v", caps)
	}
}

func TestNewUnknownIntegration(t *testing.T) {
	_, err := newAdapter(config.AgentConfig{ID: "x", Binary: "sh", Integration: "bogus"})
	if err == nil || !strings.Contains(err.Error(), "unknown integration") {
		t.Fatalf("expected unknown-integration error, got %v", err)
	}
}

func TestNewRejectsIntegrationUnsupportedByBinary(t *testing.T) {
	ac := config.AgentConfig{ID: "c", Binary: "claude", Integration: config.IntegrationExec}
	if _, err := newAdapter(ac); err == nil || !strings.Contains(err.Error(), "does not support integration") {
		t.Fatalf("expected profile mismatch error, got %v", err)
	}
}

func TestNewResolvesProfileDefault(t *testing.T) {
	// codebuddy with no explicit integration resolves to its profile default.
	ln, err := newAdapter(config.AgentConfig{ID: "cb", Binary: "codebuddy"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := ln.Capabilities().Streaming; got != agent.StreamToken {
		t.Fatalf("expected stream-json (token streaming), got %v", got)
	}
}
