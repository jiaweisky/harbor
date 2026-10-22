package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestAgentCardFieldsRequired(t *testing.T) {
	displayName := writeConfig(t, `
version: 1
agents:
  - id: cb
    binary: codebuddy
    integration: stream-json
    description: "Test agent."
`)
	_, err := config.Load(displayName)
	if err == nil || !strings.Contains(err.Error(), "display_name is required") {
		t.Fatalf("expected a display_name error, got %v", err)
	}

	description := writeConfig(t, `
version: 1
agents:
  - id: cb
    binary: codebuddy
    integration: stream-json
    display_name: "CodeBuddy"
`)
	_, err = config.Load(description)
	if err == nil || !strings.Contains(err.Error(), "description is required") {
		t.Fatalf("expected a description error, got %v", err)
	}
}

func TestExecRequiresPromptTemplate(t *testing.T) {
	path := writeConfig(t, `
version: 1
agents:
  - id: cb
    display_name: "cb"
    description: "Test agent for the cb CLI."
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
    display_name: "oc"
    description: "Test agent for the oc CLI."
    binary: opencode
    integration: exec
    args: ["run", "{{.Prompt}}"]
`)
	if _, err := config.Load(path); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCodexProfileDefaultOutputSchemaArgs(t *testing.T) {
	path := writeConfig(t, `
version: 1
agents:
  - id: cx
    display_name: "cx"
    description: "Test agent for the cx CLI."
    binary: codex
    integration: exec
    args: ["exec", "--json", "{{.Prompt}}"]
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := cfg.Agents[0].OutputSchemaArgs
	if len(got) != 2 || got[0] != "--output-schema" || got[1] != "{{.SchemaFile}}" {
		t.Fatalf("OutputSchemaArgs = %v, want --output-schema {{.SchemaFile}}", got)
	}
}

func TestOpencodeProfileDefaultFileArgs(t *testing.T) {
	path := writeConfig(t, `
version: 1
agents:
  - id: oc
    display_name: "oc"
    description: "Test agent for the oc CLI."
    binary: opencode
    integration: exec
    args: ["run", "{{.Prompt}}"]
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := cfg.Agents[0].FileArgs
	if len(got) != 2 || got[0] != "-f" || got[1] != "{{.File}}" {
		t.Fatalf("FileArgs = %v, want -f {{.File}}", got)
	}
}

func TestServerProtocolValidation(t *testing.T) {
	valid := writeConfig(t, `
version: 1
agents:
  - id: cb
    display_name: "cb"
    description: "Test agent for the cb CLI."
    binary: codebuddy
    integration: server
    server_protocol: acp
`)
	if _, err := config.Load(valid); err != nil {
		t.Fatalf("unexpected error for acp protocol: %v", err)
	}

	streamable := writeConfig(t, `
version: 1
agents:
  - id: cb
    display_name: "cb"
    description: "Test agent for the cb CLI."
    binary: codebuddy
    integration: server
    server_protocol: acp-streamable-http
`)
	if _, err := config.Load(streamable); err != nil {
		t.Fatalf("unexpected error for acp-streamable-http protocol: %v", err)
	}

	bad := writeConfig(t, `
version: 1
agents:
  - id: cb
    display_name: "cb"
    description: "Test agent for the cb CLI."
    binary: codebuddy
    integration: server
    server_protocol: grpc
`)
	if _, err := config.Load(bad); err == nil || !strings.Contains(err.Error(), "server_protocol") {
		t.Fatalf("expected a server_protocol error, got %v", err)
	}
}

func TestInteractionValidation(t *testing.T) {
	for _, mode := range []string{"auto", "interactive", "non-interactive"} {
		path := writeConfig(t, `
version: 1
agents:
  - id: cb
    display_name: "cb"
    description: "Test agent for the cb CLI."
    binary: codebuddy
    integration: stream-json
    interaction: `+mode+`
`)
		cfg, err := config.Load(path)
		if err != nil {
			t.Fatalf("interaction %q: unexpected error: %v", mode, err)
		}
		if cfg.Agents[0].Interaction != mode {
			t.Fatalf("interaction = %q, want %q", cfg.Agents[0].Interaction, mode)
		}
	}

	def := writeConfig(t, `
version: 1
agents:
  - id: cb
    display_name: "cb"
    description: "Test agent for the cb CLI."
    binary: codebuddy
    integration: stream-json
`)
	cfg, err := config.Load(def)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Agents[0].Interaction != "auto" {
		t.Fatalf("default interaction = %q, want auto", cfg.Agents[0].Interaction)
	}

	bad := writeConfig(t, `
version: 1
agents:
  - id: cb
    display_name: "cb"
    description: "Test agent for the cb CLI."
    binary: codebuddy
    integration: stream-json
    interaction: sometimes
`)
	if _, err := config.Load(bad); err == nil || !strings.Contains(err.Error(), "interaction") {
		t.Fatalf("expected an interaction error, got %v", err)
	}

	// interactive requires a pausable integration.
	badCombo := writeConfig(t, `
version: 1
agents:
  - id: oc
    display_name: "oc"
    description: "Test agent for the oc CLI."
    binary: opencode
    integration: exec
    interaction: interactive
    args: ["run", "{{.Prompt}}"]
`)
	if _, err := config.Load(badCombo); err == nil || !strings.Contains(err.Error(), "pausable") {
		t.Fatalf("expected a pausable-integration error, got %v", err)
	}
}

func TestACPProfileFillsDefaultArgs(t *testing.T) {
	path := writeConfig(t, `
version: 1
agents:
  - id: oc
    display_name: "oc"
    description: "Test agent for the oc CLI."
    binary: opencode
    integration: acp
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cfg.Agents[0].Args; len(got) != 1 || got[0] != "acp" {
		t.Fatalf("args = %v, want [acp]", got)
	}
}

func TestACPRequiresArgs(t *testing.T) {
	path := writeConfig(t, `
version: 1
agents:
  - id: cb
    display_name: "cb"
    description: "Test agent for the cb CLI."
    binary: codebuddy
    integration: acp
`)
	_, err := config.Load(path)
	if err == nil || !strings.Contains(err.Error(), "ACP mode") {
		t.Fatalf("expected an ACP-args error, got %v", err)
	}
}

func TestUnknownIntegrationIsRejected(t *testing.T) {
	path := writeConfig(t, `
version: 1
agents:
  - id: x
    display_name: "x"
    description: "Test agent for the x CLI."
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
    display_name: "cb"
    description: "Test agent for the cb CLI."
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
    display_name: "c"
    description: "Test agent for the c CLI."
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
    display_name: "x"
    description: "Test agent for the x CLI."
    binary: my-bot
`)
	_, err := config.Load(path)
	if err == nil || !strings.Contains(err.Error(), "unknown binary") {
		t.Fatalf("expected an unknown-binary error, got %v", err)
	}
}

func TestLimitsParsed(t *testing.T) {
	path := writeConfig(t, `
version: 1
agents:
  - id: oc
    display_name: "oc"
    description: "Test agent for the oc CLI."
    binary: opencode
    integration: exec
    args: ["run", "{{.Prompt}}"]
    limits:
      max_concurrent: 2
      timeout: 30m
      idle_timeout: 5m
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	lim := cfg.Agents[0].Limits
	if lim.Timeout != 30*time.Minute || lim.IdleTimeout != 5*time.Minute || lim.MaxConcurrent != 2 {
		t.Fatalf("limits = %+v", lim)
	}
}

func TestIsolationWorktreeRequiresRoot(t *testing.T) {
	path := writeConfig(t, `
version: 1
sandbox:
  isolation: worktree
agents:
  - id: oc
    display_name: "oc"
    description: "Test agent for the oc CLI."
    binary: opencode
    integration: exec
    args: ["run", "{{.Prompt}}"]
`)
	_, err := config.Load(path)
	if err == nil || !strings.Contains(err.Error(), "isolation_root") {
		t.Fatalf("expected an isolation_root error, got %v", err)
	}
}

func TestArtifactInlineMaxBytesMustNotBeNegative(t *testing.T) {
	path := writeConfig(t, `
version: 1
sandbox:
  artifact_inline_max_bytes: -1
agents:
  - id: oc
    display_name: "oc"
    description: "Test agent for the oc CLI."
    binary: opencode
    integration: exec
    args: ["run", "{{.Prompt}}"]
`)
	_, err := config.Load(path)
	if err == nil || !strings.Contains(err.Error(), "artifact_inline_max_bytes") {
		t.Fatalf("expected an artifact_inline_max_bytes error, got %v", err)
	}
}

func TestSandboxArtifactSettings(t *testing.T) {
	path := writeConfig(t, `
version: 1
sandbox:
  non_image_file_artifacts: true
  artifact_inline_max_bytes: 1234
  diff_inline_max_bytes: 5678
agents:
  - id: oc
    display_name: "oc"
    description: "Test agent for the oc CLI."
    binary: opencode
    integration: exec
    args: ["run", "{{.Prompt}}"]
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Sandbox.NonImageFileArtifacts {
		t.Fatal("non_image_file_artifacts was not parsed")
	}
	if cfg.Sandbox.ArtifactInlineMaxBytes != 1234 {
		t.Fatalf("artifact_inline_max_bytes = %d", cfg.Sandbox.ArtifactInlineMaxBytes)
	}
	if cfg.Sandbox.DiffInlineMaxBytes != 5678 {
		t.Fatalf("diff_inline_max_bytes = %d", cfg.Sandbox.DiffInlineMaxBytes)
	}
}

func TestDiffInlineMaxBytesMustNotBeNegative(t *testing.T) {
	path := writeConfig(t, `
version: 1
sandbox:
  diff_inline_max_bytes: -1
agents:
  - id: oc
    display_name: "oc"
    description: "Test agent for the oc CLI."
    binary: opencode
    integration: exec
    args: ["run", "{{.Prompt}}"]
`)
	_, err := config.Load(path)
	if err == nil || !strings.Contains(err.Error(), "diff_inline_max_bytes") {
		t.Fatalf("expected a diff_inline_max_bytes error, got %v", err)
	}
}

func TestNonGitChangeGlobs(t *testing.T) {
	path := writeConfig(t, `
version: 1
sandbox:
  non_git_change_globs: ["src/**", "*.md"]
agents:
  - id: oc
    display_name: "oc"
    description: "Test agent for the oc CLI."
    binary: opencode
    integration: exec
    args: ["run", "{{.Prompt}}"]
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Sandbox.NonGitChangeGlobs) != 2 {
		t.Fatalf("globs = %v", cfg.Sandbox.NonGitChangeGlobs)
	}
}

func TestNonGitChangeGlobsRejectsInvalidPatterns(t *testing.T) {
	for _, glob := range []string{"/tmp/**", "../outside/**", "src/../secret", "src/..", "src/["} {
		path := writeConfig(t, `
version: 1
sandbox:
  non_git_change_globs: ["`+glob+`"]
agents:
  - id: oc
    display_name: "oc"
    description: "Test agent for the oc CLI."
    binary: opencode
    integration: exec
    args: ["run", "{{.Prompt}}"]
`)
		if _, err := config.Load(path); err == nil || !strings.Contains(err.Error(), "non_git_change_globs") {
			t.Fatalf("glob %q: expected a validation error, got %v", glob, err)
		}
	}
}

func TestStoreMustBeKnown(t *testing.T) {
	path := writeConfig(t, `
version: 1
server:
  store: bogus
agents:
  - id: oc
    display_name: "oc"
    description: "Test agent for the oc CLI."
    binary: opencode
    integration: exec
    args: ["run", "{{.Prompt}}"]
`)
	_, err := config.Load(path)
	if err == nil || !strings.Contains(err.Error(), "store") {
		t.Fatalf("expected a store error, got %v", err)
	}
}

func TestSQLiteStoreRequiresAPath(t *testing.T) {
	path := writeConfig(t, `
version: 1
server:
  store: sqlite
agents:
  - id: oc
    display_name: "oc"
    description: "Test agent for the oc CLI."
    binary: opencode
    integration: exec
    args: ["run", "{{.Prompt}}"]
`)
	_, err := config.Load(path)
	if err == nil || !strings.Contains(err.Error(), "store_path") {
		t.Fatalf("expected a path error, got %v", err)
	}
}

func TestAgentInactivityTimeout(t *testing.T) {
	path := writeConfig(t, `
version: 1
server:
  agent_inactivity_timeout: 30s
agents:
  - id: oc
    display_name: "oc"
    description: "Test agent for the oc CLI."
    binary: opencode
    integration: exec
    args: ["run", "{{.Prompt}}"]
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.AgentInactivityTimeout == nil || *cfg.Server.AgentInactivityTimeout != 30*time.Second {
		t.Fatalf("agent_inactivity_timeout = %v, want 30s", cfg.Server.AgentInactivityTimeout)
	}
}

func TestAgentWorkdirMustExist(t *testing.T) {
	path := writeConfig(t, `
version: 1
agents:
  - id: oc
    display_name: "oc"
    description: "Test agent for the oc CLI."
    binary: opencode
    integration: exec
    args: ["run", "{{.Prompt}}"]
    workdir: /no/such/harbor-dir-xyz
`)
	_, err := config.Load(path)
	if err == nil || !strings.Contains(err.Error(), "workdir") {
		t.Fatalf("expected a workdir error, got %v", err)
	}
}

func TestInheritEnvDefaultsTrue(t *testing.T) {
	path := writeConfig(t, `
version: 1
agents:
  - id: oc
    display_name: "oc"
    description: "Test agent for the oc CLI."
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
    display_name: "oc"
    description: "Test agent for the oc CLI."
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

func TestContainerDefaults(t *testing.T) {
	path := writeConfig(t, `
version: 1
sandbox:
  container:
    image: harbor/claude:latest
agents:
  - id: oc
    display_name: "oc"
    description: "Test agent for the oc CLI."
    binary: opencode
    integration: exec
    args: ["run", "{{.Prompt}}"]
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ct := cfg.Sandbox.Container
	if ct == nil {
		t.Fatal("container config was dropped")
	}
	if ct.Network != "none" {
		t.Errorf("network = %q, want none", ct.Network)
	}
	if ct.Runtime != "docker" {
		t.Errorf("runtime = %q, want docker", ct.Runtime)
	}
}

func TestContainerRequiresImage(t *testing.T) {
	path := writeConfig(t, `
version: 1
sandbox:
  container:
    network: none
agents:
  - id: oc
    display_name: "oc"
    description: "Test agent for the oc CLI."
    binary: opencode
    integration: exec
    args: ["run", "{{.Prompt}}"]
`)
	_, err := config.Load(path)
	if err == nil || !strings.Contains(err.Error(), "sandbox.container.image is required") {
		t.Fatalf("expected a container image error, got %v", err)
	}
}

func TestContainerRejectsUnknownNetwork(t *testing.T) {
	path := writeConfig(t, `
version: 1
sandbox:
  container:
    image: harbor/claude:latest
    network: host
agents:
  - id: oc
    display_name: "oc"
    description: "Test agent for the oc CLI."
    binary: opencode
    integration: exec
    args: ["run", "{{.Prompt}}"]
`)
	_, err := config.Load(path)
	if err == nil || !strings.Contains(err.Error(), "sandbox.container.network") {
		t.Fatalf("expected a container network error, got %v", err)
	}
}

func TestCodexProfileDefaultImageArgs(t *testing.T) {
	path := writeConfig(t, `
version: 1
agents:
  - id: codex
    display_name: "codex"
    description: "Test agent for the codex CLI."
    binary: codex
    integration: exec
    args: ["exec", "{{.Prompt}}"]
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	a := cfg.Agents[0]
	if len(a.ImageArgs) != 2 || a.ImageArgs[0] != "-i" {
		t.Fatalf("image_args = %v, want [-i {{.Image}}]", a.ImageArgs)
	}
	if len(a.InputImage) == 0 {
		t.Fatal("input_image should default from the codex profile")
	}
}

func TestCodebuddyProfileOutputImage(t *testing.T) {
	p, ok := config.LookupProfile("codebuddy")
	if !ok || len(p.OutputImage) == 0 {
		t.Fatalf("codebuddy profile OutputImage = %v, want image types", p.OutputImage)
	}
}

func TestRequireContainerForDangerous(t *testing.T) {
	bad := writeConfig(t, `
version: 1
sandbox:
  require_container_for_dangerous: true
agents:
  - id: cb
    display_name: "cb"
    description: "Test agent for the cb CLI."
    binary: codebuddy
    integration: stream-json
    allow_dangerous: true
`)
	if _, err := config.Load(bad); err == nil || !strings.Contains(err.Error(), "require_container_for_dangerous") {
		t.Fatalf("expected a container-required error, got %v", err)
	}

	good := writeConfig(t, `
version: 1
sandbox:
  require_container_for_dangerous: true
  container: { image: "harbor/cb:latest" }
agents:
  - id: cb
    display_name: "cb"
    description: "Test agent for the cb CLI."
    binary: codebuddy
    integration: stream-json
    allow_dangerous: true
`)
	if _, err := config.Load(good); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestForkFlagDefaultsFromProfile(t *testing.T) {
	path := writeConfig(t, `
version: 1
agents:
  - id: cb
    display_name: "cb"
    description: "Test agent for the cb CLI."
    binary: codebuddy
    integration: stream-json
    args: ["-p"]
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Agents[0].Session.ForkFlag != "--fork-session" {
		t.Fatalf("fork_flag = %q, want --fork-session", cfg.Agents[0].Session.ForkFlag)
	}
}

func TestCardOverrideConfig(t *testing.T) {
	path := writeConfig(t, `
version: 1
agents:
  - id: cb
    display_name: "cb"
    binary: codebuddy
    integration: stream-json
    args: ["-p"]
    description: "My CodeBuddy"
    skills:
      - id: code-review
        name: Code review
        description: Review a diff for issues
        tags: ["review", "quality"]
        examples: ["review this diff"]
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	a := cfg.Agents[0]
	if a.Description != "My CodeBuddy" {
		t.Fatalf("description = %q", a.Description)
	}
	if len(a.Skills) != 1 || a.Skills[0].ID != "code-review" || len(a.Skills[0].Tags) != 2 {
		t.Fatalf("skills = %+v", a.Skills)
	}
}

func TestSkillsRequireIDNameDescription(t *testing.T) {
	path := writeConfig(t, `
version: 1
agents:
  - id: cb
    display_name: "cb"
    description: "Test agent for the cb CLI."
    binary: codebuddy
    integration: stream-json
    args: ["-p"]
    skills:
      - id: only-id
`)
	_, err := config.Load(path)
	if err == nil || !strings.Contains(err.Error(), "requires id, name, and description") {
		t.Fatalf("expected a skills validation error, got %v", err)
	}
}

func TestTelemetryConfig(t *testing.T) {
	path := writeConfig(t, `
version: 1
telemetry:
  endpoint: "localhost:4317"
  insecure: true
agents:
  - id: oc
    display_name: "oc"
    description: "Test agent for the oc CLI."
    binary: opencode
    integration: exec
    args: ["run", "{{.Prompt}}"]
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Telemetry.Endpoint != "localhost:4317" || !cfg.Telemetry.Insecure {
		t.Fatalf("telemetry = %+v", cfg.Telemetry)
	}
	if cfg.Telemetry.ServiceName != "harbor" {
		t.Errorf("service_name = %q, want default harbor", cfg.Telemetry.ServiceName)
	}
}
