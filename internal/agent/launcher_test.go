package agent_test

import (
	"context"
	"testing"

	"github.com/jiaweisky/harbor/internal/agent"
)

func TestProbeWithoutVersionArgsSkipsProbing(t *testing.T) {
	l := agent.NewHeadlessLauncher(agent.LauncherConfig{Name: "x", Binary: "sh"})
	info := l.Probe(context.Background())
	if !info.Available {
		t.Fatalf("sh should be found: %v", info.ProbeErr)
	}
	if info.CLIVersion != "" {
		t.Fatalf("version = %q, want empty when probing is disabled", info.CLIVersion)
	}
}

func TestProbeUsesVersionOverride(t *testing.T) {
	l := agent.NewHeadlessLauncher(agent.LauncherConfig{Name: "x", Binary: "sh", VersionOverride: "1.2.3"})
	if got := l.Probe(context.Background()).CLIVersion; got != "1.2.3" {
		t.Fatalf("version = %q, want override", got)
	}
}
