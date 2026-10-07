package integration_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jiaweisky/harbor/internal/config"
	"github.com/jiaweisky/harbor/internal/integration"
)

// fakeBinary writes an executable named `name` that prints versionOut for any
// arguments, so it acts as a stand-in for a known CLI.
func fakeBinary(t *testing.T, name, versionOut string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	script := "#!/bin/sh\necho \"" + versionOut + "\"\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBuildRejectsOldVersion(t *testing.T) {
	bin := fakeBinary(t, "codebuddy", "2.0.0") // below codebuddy's minimum
	ac := config.AgentConfig{ID: "cb", Binary: bin}
	_, _, err := integration.Build(context.Background(), ac, integration.Options{InheritEnv: true})
	if err == nil || !strings.Contains(err.Error(), "below the minimum supported version") {
		t.Fatalf("expected a minimum-version error, got %v", err)
	}
}

func TestBuildAcceptsSupportedVersion(t *testing.T) {
	bin := fakeBinary(t, "codebuddy", "9.9.9 (Fake CLI)")
	ac := config.AgentConfig{ID: "cb", Binary: bin}
	ln, info, err := integration.Build(context.Background(), ac, integration.Options{InheritEnv: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ln == nil || !info.Enabled || info.Version != "9.9.9" {
		t.Fatalf("launcher=%v info=%+v", ln != nil, info)
	}
}
