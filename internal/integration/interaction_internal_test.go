package integration

import (
	"context"
	"testing"

	"github.com/jiaweisky/harbor/internal/agent"
	"github.com/jiaweisky/harbor/internal/config"
)

// stubLauncher is a Launcher that only exposes capabilities, for testing the
// interaction-mode override.
type stubLauncher struct{ caps agent.Capabilities }

func (s stubLauncher) Name() string                     { return "stub" }
func (s stubLauncher) Capabilities() agent.Capabilities { return s.caps }
func (s stubLauncher) Probe(context.Context) agent.Info { return agent.Info{} }
func (s stubLauncher) Auth() agent.Authenticator        { return nil }
func (s stubLauncher) AuthReady(context.Context) bool   { return true }
func (s stubLauncher) Start(context.Context, *agent.Request) (agent.Session, error) {
	return nil, agent.ErrUnsupported
}

func TestApplyInteractionMode(t *testing.T) {
	interactive := agent.Capabilities{
		Permission:          agent.PermInteractive,
		PlanApproval:        true,
		InputRequiredResume: true,
	}

	// auto (and unset) keep the integration default.
	for _, mode := range []string{"", "auto"} {
		got, err := applyInteractionMode(stubLauncher{interactive}, mode)
		if err != nil {
			t.Fatalf("applyInteractionMode(%q): %v", mode, err)
		}
		if got.Capabilities().Permission != agent.PermInteractive {
			t.Fatalf("%q: permission = %v, want PermInteractive", mode, got.Capabilities().Permission)
		}
	}

	// interactive forces the permission channel on, even for a flag-only
	// integration; it does not invent integration-specific capabilities.
	flagOnly := agent.Capabilities{Permission: agent.PermFlagOnly}
	got, err := applyInteractionMode(stubLauncher{flagOnly}, "interactive")
	if err != nil {
		t.Fatalf("interactive: %v", err)
	}
	if c := got.Capabilities(); c.Permission != agent.PermInteractive || !c.InputRequiredResume {
		t.Fatalf("interactive: caps = %+v", c)
	}

	// non-interactive drops every interaction capability.
	got, err = applyInteractionMode(stubLauncher{interactive}, "non-interactive")
	if err != nil {
		t.Fatalf("non-interactive: %v", err)
	}
	if c := got.Capabilities(); c.Permission != agent.PermFlagOnly || c.PlanApproval || c.InputRequiredResume || c.AskQuestion {
		t.Fatalf("non-interactive: caps = %+v", c)
	}

	// an unknown mode is rejected.
	if _, err := applyInteractionMode(stubLauncher{interactive}, "sometimes"); err == nil {
		t.Fatal("expected an error for an unknown interaction mode")
	}
}

// TestNewAppliesInteractionMode verifies New (not just Build) applies the
// configured mode to the constructed launcher's capabilities.
func TestNewAppliesInteractionMode(t *testing.T) {
	ac := config.AgentConfig{
		ID: "cb", Binary: "codebuddy",
		Integration: config.IntegrationStreamJSON,
		Interaction: "non-interactive",
	}
	ln, err := New(ac, Options{InheritEnv: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := ln.Capabilities().Permission; got != agent.PermFlagOnly {
		t.Fatalf("permission = %v, want PermFlagOnly", got)
	}

	ac.Interaction = "interactive"
	ln, err = New(ac, Options{InheritEnv: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := ln.Capabilities().Permission; got != agent.PermInteractive {
		t.Fatalf("permission = %v, want PermInteractive", got)
	}
}
