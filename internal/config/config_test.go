package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jiaweisky/harbor/internal/config"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agents.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExecRequiresPromptTemplate(t *testing.T) {
	path := writeConfig(t, `
version: 1
agents:
  - id: cb
    binary: codebuddy
    integration: exec
    args: ["-p", "--output-format", "stream-json"]
`)
	_, err := config.Load(path)
	if err == nil || !strings.Contains(err.Error(), "{{.Prompt}}") {
		t.Fatalf("expected a prompt-template error, got %v", err)
	}
}

func TestExecWithPromptTemplateIsValid(t *testing.T) {
	path := writeConfig(t, `
version: 1
agents:
  - id: oc
    binary: opencode
    integration: exec
    args: ["run", "{{.Prompt}}"]
`)
	if _, err := config.Load(path); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestUnknownIntegrationIsRejected(t *testing.T) {
	path := writeConfig(t, `
version: 1
agents:
  - id: x
    binary: b
    integration: bogus
`)
	_, err := config.Load(path)
	if err == nil || !strings.Contains(err.Error(), "unknown integration") {
		t.Fatalf("expected unknown-integration error, got %v", err)
	}
}

func TestAtLeastOneAgentRequired(t *testing.T) {
	path := writeConfig(t, "version: 1\nagents: []\n")
	if _, err := config.Load(path); err == nil {
		t.Fatal("expected an error for an empty agent list")
	}
}

func TestProfileFillsDefaultIntegration(t *testing.T) {
	path := writeConfig(t, `
version: 1
agents:
  - id: cb
    binary: /usr/local/bin/codebuddy
    args: ["-p"]
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cfg.Agents[0].Integration; got != config.IntegrationStreamJSON {
		t.Fatalf("integration = %q, want %q", got, config.IntegrationStreamJSON)
	}
}

func TestProfileRejectsUnsupportedIntegration(t *testing.T) {
	path := writeConfig(t, `
version: 1
agents:
  - id: c
    binary: claude
    integration: exec
    args: ["{{.Prompt}}"]
`)
	_, err := config.Load(path)
	if err == nil || !strings.Contains(err.Error(), "does not support integration") {
		t.Fatalf("expected a profile-mismatch error, got %v", err)
	}
}

func TestUnknownBinaryRequiresIntegration(t *testing.T) {
	path := writeConfig(t, `
version: 1
agents:
  - id: x
    binary: my-bot
`)
	_, err := config.Load(path)
	if err == nil || !strings.Contains(err.Error(), "unknown binary") {
		t.Fatalf("expected an unknown-binary error, got %v", err)
	}
}

func TestWorkdirRootMustExist(t *testing.T) {
	path := writeConfig(t, `
version: 1
sandbox:
  workdir_root: /no/such/harbor-dir-xyz
agents:
  - id: oc
    binary: opencode
    integration: exec
    args: ["run", "{{.Prompt}}"]
`)
	_, err := config.Load(path)
	if err == nil || !strings.Contains(err.Error(), "workdir_root") {
		t.Fatalf("expected a workdir_root error, got %v", err)
	}
}

func TestInheritEnvDefaultsTrue(t *testing.T) {
	path := writeConfig(t, `
version: 1
agents:
  - id: oc
    binary: opencode
    integration: exec
    args: ["run", "{{.Prompt}}"]
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.Sandbox.InheritEnvEnabled() {
		t.Fatal("inherit_env should default to true")
	}
}

func TestInheritEnvCanBeDisabled(t *testing.T) {
	path := writeConfig(t, `
version: 1
sandbox:
  inherit_env: false
agents:
  - id: oc
    binary: opencode
    integration: exec
    args: ["run", "{{.Prompt}}"]
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Sandbox.InheritEnvEnabled() {
		t.Fatal("inherit_env: false should disable inheritance")
	}
}
