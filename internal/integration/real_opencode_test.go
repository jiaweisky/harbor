//go:build realoc

package integration_test

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/jiaweisky/harbor/internal/config"
	"github.com/jiaweisky/harbor/internal/core"
	"github.com/jiaweisky/harbor/internal/integration"
)

// TestBuildOpencodeDoesNotLogin verifies that startup does not gate opencode on
// a login ticket: it resolves providers from its own config, so Build must not
// launch the interactive `opencode auth login`.
func TestBuildOpencodeDoesNotLogin(t *testing.T) {
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Fatalf("opencode not installed: %v", err)
	}
	ac := config.AgentConfig{
		ID: "oc", Binary: "opencode", Integration: core.IntegrationExec,
		Args: []string{"run", "{{.Prompt}}"},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	start := time.Now()
	ln, info, err := integration.Build(ctx, ac, integration.Options{InheritEnv: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if ln == nil || !info.Available {
		t.Fatalf("info = %+v", info)
	}
	if el := time.Since(start); el > 15*time.Second {
		t.Errorf("Build took %s; it likely launched an interactive login", el)
	}
	if !ln.AuthReady(ctx) {
		t.Errorf("opencode should be auth-ready without a login ticket")
	}
}
