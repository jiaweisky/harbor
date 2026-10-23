package integration_test

import (
	"context"
	"testing"

	"github.com/jiaweisky/harbor/internal/agent"
	"github.com/jiaweisky/harbor/internal/config"
	"github.com/jiaweisky/harbor/internal/core"
	"github.com/jiaweisky/harbor/internal/integration"
)

// stubLauncher is a minimal Launcher for adapter-registration tests.
type stubLauncher struct{ name string }

func (s stubLauncher) Name() string                   { return s.name }
func (stubLauncher) Capabilities() agent.Capabilities { return agent.Capabilities{InputText: true} }
func (stubLauncher) Probe(context.Context) agent.Info { return agent.Info{Name: "stub"} }
func (stubLauncher) Auth() agent.Authenticator        { return nil }
func (stubLauncher) AuthReady(context.Context) bool   { return true }
func (stubLauncher) Start(context.Context, *agent.Request) (agent.Session, error) {
	return nil, nil
}

// TestRegisteredAdapterIsUsed verifies a builder registered for a CLI binary is
// used by New instead of the built-in construction.
func TestRegisteredAdapterIsUsed(t *testing.T) {
	const bin = "harbor-test-weird-cli"
	integration.RegisterAdapter(bin, func(ac core.AgentSpec, _ integration.AdapterOptions) (agent.Launcher, error) {
		return stubLauncher{name: ac.ID}, nil
	})

	ln, err := integration.New(config.AgentConfig{
		ID: "weird", Binary: bin, Integration: config.IntegrationExec,
		Args: []string{"{{.Prompt}}"},
	}, integration.Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := ln.(stubLauncher); !ok {
		t.Fatalf("launcher type = %T, want the registered stubLauncher", ln)
	}
}

// TestAdapterErrUnhandledFallsThrough verifies a builder may decline an agent
// (ErrUnhandled) so the built-in construction still runs.
func TestAdapterErrUnhandledFallsThrough(t *testing.T) {
	const bin = "harbor-test-fallthrough"
	integration.RegisterAdapter(bin, func(core.AgentSpec, integration.AdapterOptions) (agent.Launcher, error) {
		return nil, integration.ErrUnhandled
	})

	ln, err := integration.New(config.AgentConfig{
		ID: "ft", Binary: bin, Integration: config.IntegrationExec,
		Args: []string{"{{.Prompt}}"},
	}, integration.Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := ln.(stubLauncher); ok {
		t.Fatal("a builder returning ErrUnhandled must not be used")
	}
}

// TestBuiltinAdaptersRegistered verifies the first-party dedicated adapters
// self-register.
func TestBuiltinAdaptersRegistered(t *testing.T) {
	for _, name := range []string{"opencode", "codebuddy", "codex"} {
		if _, ok := integration.LookupAdapter(name); !ok {
			t.Errorf("built-in adapter %q is not registered", name)
		}
	}
}
