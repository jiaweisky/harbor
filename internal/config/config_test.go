package config_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jiaweisky/harbor/internal/cli"
	"github.com/jiaweisky/harbor/internal/config"
	"github.com/jiaweisky/harbor/internal/core"
)

// loadConfig loads a config file and applies the CLI preparation step.
func loadConfig(t *testing.T, path string) (*config.Config, error) {
	t.Helper()
	return cli.Load(path)
}

// writeRawConfig writes body as-is.
func writeRawConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agents.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// writeConfig writes body with the required listen, state_dir, and workdir
// filled in unless the fixture sets its own server, sandbox, or workdir.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	if !strings.Contains(body, "listen:") {
		body = "server:\n  listen: \"127.0.0.1:8080\"\n" + body
	}
	if !strings.Contains(body, "state_dir:") {
		body = "sandbox:\n  state_dir: " + strconv.Quote(t.TempDir()) + "\n" + body
	}
	if !strings.Contains(body, "workdir:") {
		var lines []string
		for _, line := range strings.Split(body, "\n") {
			lines = append(lines, line)
			if strings.HasPrefix(line, "  - id:") {
				lines = append(lines, "    workdir: \".\"")
			}
		}
		body = strings.Join(lines, "\n")
	}
	return writeRawConfig(t, body)
}

func TestLoadExpandsEnvironment(t *testing.T) {
	t.Setenv("HARBOR_TEST_DESCRIPTION", "from env")
	t.Setenv("HARBOR_TEST_TOKEN", "s3cret")
	t.Setenv("HARBOR_TEST_EMPTY", "")
	t.Setenv("HARBOR_TEST_NESTED", "${HARBOR_TEST_TOKEN}")
	path := writeConfig(t, `
version: 1
agents:
  - id: cb
    display_name: "cb"
    description: "${HARBOR_TEST_DESCRIPTION}"
    binary: codebuddy
    integration: stream-json
    env:
      TOKEN: "$HARBOR_TEST_TOKEN"
      LOG_LEVEL: "${HARBOR_TEST_UNSET_9F3C:-info}"
      FALLBACK: "${HARBOR_TEST_EMPTY:-fallback}"
      SET_EMPTY: "${HARBOR_TEST_EMPTY}"
      LITERAL: "$${HARBOR_TEST_TOKEN}"
      DOLLAR: "$$"
      JSON_SCHEMA: "$$schema"
      POSITIONAL: "$1"
      BRACED_DIGIT: "${1}"
      NESTED: "${HARBOR_TEST_NESTED}"
`)
	cfg, err := loadConfig(t, path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	agent := cfg.Agents[0]
	if agent.Description != "from env" {
		t.Errorf("description = %q, want %q", agent.Description, "from env")
	}
	want := map[string]string{
		"TOKEN":        "s3cret",
		"LOG_LEVEL":    "info",
		"FALLBACK":     "fallback",
		"SET_EMPTY":    "",
		"LITERAL":      "${HARBOR_TEST_TOKEN}",
		"DOLLAR":       "$",
		"JSON_SCHEMA":  "$schema",
		"POSITIONAL":   "$1",
		"BRACED_DIGIT": "${1}",
		"NESTED":       "${HARBOR_TEST_TOKEN}",
	}
	for key, value := range want {
		if got := agent.Env[key]; got != value {
			t.Errorf("env[%s] = %q, want %q", key, got, value)
		}
	}
}

func TestLoadRejectsMalformedEnvironmentReference(t *testing.T) {
	path := writeConfig(t, `
version: 1
agents:
  - id: cb
    display_name: "cb"
    description: "Test agent."
    binary: codebuddy
    integration: stream-json
    env:
      TOKEN: "${HARBOR_TEST_UNSET_9F3C"
`)
	_, err := loadConfig(t, path)
	if err == nil || !strings.Contains(err.Error(), "config: closing brace") {
		t.Fatalf("expected a config expansion error, got %v", err)
	}
}

func TestLoadRejectsUnsetEnvironmentReference(t *testing.T) {
	path := writeConfig(t, `
version: 1
agents:
  - id: cb
    display_name: "cb"
    description: "Test agent."
    binary: codebuddy
    integration: stream-json
    env:
      TOKEN: "${HARBOR_TEST_UNSET_9F3C}"
`)
	_, err := loadConfig(t, path)
	if err == nil || !strings.Contains(err.Error(), "not set") {
		t.Fatalf("expected an unset-variable error, got %v", err)
	}
}

func TestLoadQuotedEnvironmentValues(t *testing.T) {
	cases := []struct {
		name  string
		ref   string
		value string
		want  string
	}{
		{"double quotes keep specials", `"${HARBOR_QUOTE_VALUE}"`, "a: b #c", "a: b #c"},
		{"single quotes keep backslashes", `'${HARBOR_QUOTE_VALUE}'`, `C:\work\dir`, `C:\work\dir`},
		{"single quotes keep double quotes", `'${HARBOR_QUOTE_VALUE}'`, `he said "hi"`, `he said "hi"`},
		{"newline folds to space", `"${HARBOR_QUOTE_VALUE}"`, "a\nb", "a b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HARBOR_QUOTE_VALUE", tc.value)
			path := writeConfig(t, `
version: 1
agents:
  - id: cb
    display_name: "cb"
    description: `+tc.ref+`
    binary: codebuddy
    integration: stream-json
`)
			cfg, err := loadConfig(t, path)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := cfg.Agents[0].Description; got != tc.want {
				t.Errorf("description = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLoadUnquotedEnvironmentValueNeedsQuoting(t *testing.T) {
	t.Setenv("HARBOR_QUOTE_VALUE", "a: b")
	path := writeConfig(t, `
version: 1
agents:
  - id: cb
    display_name: "cb"
    description: ${HARBOR_QUOTE_VALUE}
    binary: codebuddy
    integration: stream-json
`)
	if _, err := loadConfig(t, path); err == nil {
		t.Fatal("expected a YAML error for an unquoted value containing \"a: b\"")
	}
}

func TestExampleConfigLoads(t *testing.T) {
	if _, err := loadConfig(t, "../../examples/agents.yaml"); err != nil {
		t.Fatalf("examples/agents.yaml: %v", err)
	}
}

func TestListenRequired(t *testing.T) {
	path := writeRawConfig(t, `
version: 1
sandbox:
  state_dir: "/tmp/harbor-test-state"
agents:
  - id: oc
    display_name: "oc"
    description: "Test agent for the oc CLI."
    binary: opencode
    integration: exec
    args: ["run", "{{.Prompt}}"]
`)
	_, err := loadConfig(t, path)
	if err == nil || !strings.Contains(err.Error(), "listen") {
		t.Fatalf("expected a listen error, got %v", err)
	}
}

func TestStateDirRequired(t *testing.T) {
	path := writeRawConfig(t, `
version: 1
server:
  listen: "127.0.0.1:8080"
agents:
  - id: oc
    display_name: "oc"
    description: "Test agent for the oc CLI."
    binary: opencode
    integration: exec
    args: ["run", "{{.Prompt}}"]
`)
	_, err := loadConfig(t, path)
	if err == nil || !strings.Contains(err.Error(), "state_dir") {
		t.Fatalf("expected a state_dir error, got %v", err)
	}
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
	_, err := loadConfig(t, displayName)
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
	_, err = loadConfig(t, description)
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
	_, err := loadConfig(t, path)
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
	if _, err := loadConfig(t, path); err != nil {
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
	cfg, err := loadConfig(t, path)
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
	cfg, err := loadConfig(t, path)
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
	if _, err := loadConfig(t, valid); err != nil {
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
	if _, err := loadConfig(t, streamable); err != nil {
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
	if _, err := loadConfig(t, bad); err == nil || !strings.Contains(err.Error(), "server_protocol") {
		t.Fatalf("expected a server_protocol error, got %v", err)
	}
}

func TestInteractionValidation(t *testing.T) {
	for _, mode := range []core.Interaction{
		core.InteractionAuto, core.InteractionInteractive, core.InteractionNonInteractive,
	} {
		path := writeConfig(t, `
version: 1
agents:
  - id: cb
    display_name: "cb"
    description: "Test agent for the cb CLI."
    binary: codebuddy
    integration: stream-json
    interaction: `+string(mode)+`
`)
		cfg, err := loadConfig(t, path)
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
	cfg, err := loadConfig(t, def)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Agents[0].Interaction != core.InteractionAuto {
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
	if _, err := loadConfig(t, bad); err == nil || !strings.Contains(err.Error(), "interaction") {
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
	if _, err := loadConfig(t, badCombo); err == nil || !strings.Contains(err.Error(), "pausable") {
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
	cfg, err := loadConfig(t, path)
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
	_, err := loadConfig(t, path)
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
	_, err := loadConfig(t, path)
	if err == nil || !strings.Contains(err.Error(), "unknown integration") {
		t.Fatalf("expected unknown-integration error, got %v", err)
	}
}

func TestAtLeastOneAgentRequired(t *testing.T) {
	path := writeConfig(t, "version: 1\nagents: []\n")
	if _, err := loadConfig(t, path); err == nil {
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
	cfg, err := loadConfig(t, path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cfg.Agents[0].Integration; got != core.IntegrationStreamJSON {
		t.Fatalf("integration = %q, want %q", got, core.IntegrationStreamJSON)
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
	_, err := loadConfig(t, path)
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
	_, err := loadConfig(t, path)
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
	cfg, err := loadConfig(t, path)
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
  state_dir: "/tmp/harbor-test-state"
  isolation: worktree
agents:
  - id: oc
    display_name: "oc"
    description: "Test agent for the oc CLI."
    binary: opencode
    integration: exec
    args: ["run", "{{.Prompt}}"]
`)
	_, err := loadConfig(t, path)
	if err == nil || !strings.Contains(err.Error(), "isolation_root") {
		t.Fatalf("expected an isolation_root error, got %v", err)
	}
}

func TestCopyIsolationRootMustNotContainWorkdir(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "project")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `
version: 1
sandbox:
  state_dir: "/tmp/harbor-test-state"
  isolation: copy
  isolation_root: ` + strconv.Quote(root) + `
agents:
  - id: oc
    display_name: "oc"
    description: "Test agent for the oc CLI."
    binary: opencode
    integration: exec
    args: ["run", "{{.Prompt}}"]
    workdir: ` + strconv.Quote(work) + `
`
	_, err := loadConfig(t, writeConfig(t, body))
	if err == nil || !strings.Contains(err.Error(), "must not contain") {
		t.Fatalf("expected an isolation_root containment error, got %v", err)
	}

	sibling := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	body = strings.Replace(body, strconv.Quote(work), strconv.Quote(sibling), 1)
	if _, err := loadConfig(t, writeConfig(t, body)); err != nil {
		t.Fatalf("sibling workdir: unexpected error: %v", err)
	}
}

func TestCopyIsolationRootMustNotBeInsideWorkdir(t *testing.T) {
	work := t.TempDir()
	root := filepath.Join(work, "iso")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `
version: 1
sandbox:
  state_dir: "/tmp/harbor-test-state"
  isolation: copy
  isolation_root: ` + strconv.Quote(root) + `
agents:
  - id: oc
    display_name: "oc"
    description: "Test agent for the oc CLI."
    binary: opencode
    integration: exec
    args: ["run", "{{.Prompt}}"]
    workdir: ` + strconv.Quote(work) + `
`
	_, err := loadConfig(t, writeConfig(t, body))
	if err == nil || !strings.Contains(err.Error(), "must not contain") {
		t.Fatalf("expected a workdir containment error, got %v", err)
	}
}

func TestCopyIsolationRootSymlinkedInsideWorkdir(t *testing.T) {
	work := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(work, link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	body := `
version: 1
sandbox:
  state_dir: "/tmp/harbor-test-state"
  isolation: copy
  isolation_root: ` + strconv.Quote(filepath.Join(link, "a", "b")) + `
agents:
  - id: oc
    display_name: "oc"
    description: "Test agent for the oc CLI."
    binary: opencode
    integration: exec
    args: ["run", "{{.Prompt}}"]
    workdir: ` + strconv.Quote(work) + `
`
	_, err := loadConfig(t, writeConfig(t, body))
	if err == nil || !strings.Contains(err.Error(), "must not contain") {
		t.Fatalf("expected a symlinked containment error, got %v", err)
	}
}

func TestCopyStrategy(t *testing.T) {
	cases := []struct {
		name      string
		isolation string
		strategy  string
		wantErr   string
	}{
		{"full", "copy", "full", ""},
		{"cow", "copy", "cow", ""},
		{"unknown", "copy", "magic", "must be one of"},
		{"without copy", "shared", "full", "only valid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, `
version: 1
sandbox:
  state_dir: "/tmp/harbor-test-state"
  isolation: `+tc.isolation+`
  isolation_root: "/tmp/harbor-test-iso"
  copy_strategy: `+tc.strategy+`
agents:
  - id: oc
    display_name: "oc"
    description: "Test agent for the oc CLI."
    binary: opencode
    integration: exec
    args: ["run", "{{.Prompt}}"]
`)
			cfg, err := loadConfig(t, path)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if string(cfg.Sandbox.CopyStrategy) != tc.strategy {
					t.Errorf("copy_strategy = %q, want %q", cfg.Sandbox.CopyStrategy, tc.strategy)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected an error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestOverlayFSRejectsContainer(t *testing.T) {
	body := `
version: 1
sandbox:
  state_dir: "/tmp/harbor-test-state"
  isolation: overlayfs
  isolation_root: "/tmp/harbor-test-iso"
  container:
    runtime: docker
    image: "img:latest"
agents:
  - id: oc
    display_name: "oc"
    description: "Test agent for the oc CLI."
    binary: opencode
    integration: exec
    args: ["run", "{{.Prompt}}"]
`
	_, err := loadConfig(t, writeConfig(t, body))
	if err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("expected a container/overlayfs error, got %v", err)
	}
}

func TestOverlayFSWorkdirMustNotContainRoot(t *testing.T) {
	work := filepath.Join(t.TempDir(), "project")
	root := filepath.Join(work, "iso")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `
version: 1
sandbox:
  state_dir: "/tmp/harbor-test-state"
  isolation: overlayfs
  isolation_root: ` + strconv.Quote(root) + `
agents:
  - id: oc
    display_name: "oc"
    description: "Test agent for the oc CLI."
    binary: opencode
    integration: exec
    args: ["run", "{{.Prompt}}"]
    workdir: ` + strconv.Quote(work) + `
`
	_, err := loadConfig(t, writeConfig(t, body))
	if err == nil || !strings.Contains(err.Error(), "must not contain") {
		t.Fatalf("expected an isolation_root containment error, got %v", err)
	}
}

func TestOverlayFSRequiresRoot(t *testing.T) {
	body := `
version: 1
sandbox:
  state_dir: "/tmp/harbor-test-state"
  isolation: overlayfs
agents:
  - id: oc
    display_name: "oc"
    description: "Test agent for the oc CLI."
    binary: opencode
    integration: exec
    args: ["run", "{{.Prompt}}"]
`
	_, err := loadConfig(t, writeConfig(t, body))
	if err == nil || !strings.Contains(err.Error(), "isolation_root") {
		t.Fatalf("expected an isolation_root error, got %v", err)
	}
}

func TestRequireAuthOnExposedBind(t *testing.T) {
	cases := []struct {
		name          string
		listen, mode  string
		allowInsecure bool
		wantErr       bool
	}{
		{"loopback no auth", "127.0.0.1:8080", "none", false, false},
		{"loopback range no auth", "127.0.0.2:8080", "none", false, false},
		{"localhost no auth", "localhost:8080", "none", false, false},
		{"ipv6 loopback no auth", "[::1]:8080", "none", false, false},
		{"exposed no auth", "0.0.0.0:8080", "none", false, true},
		{"ipv6 exposed no auth", "[::]:8080", "none", false, true},
		{"empty host no auth", ":8080", "none", false, true},
		{"exposed with bearer", "0.0.0.0:8080", "bearer", false, false},
		{"exposed but allow_insecure", "0.0.0.0:8080", "none", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, `
version: 1
server:
  listen: `+strconv.Quote(tc.listen)+`
  allow_insecure: `+strconv.FormatBool(tc.allowInsecure)+`
  auth:
    mode: `+tc.mode+`
agents:
  - id: oc
    display_name: "oc"
    description: "Test agent for the oc CLI."
    binary: opencode
    integration: exec
    args: ["run", "{{.Prompt}}"]
`)
			_, err := loadConfig(t, path)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Load(%q,%q,%v) err=%v, wantErr=%v", tc.listen, tc.mode, tc.allowInsecure, err, tc.wantErr)
			}
		})
	}
}

func TestWorkdirRequired(t *testing.T) {
	path := writeRawConfig(t, `
version: 1
server:
  listen: "127.0.0.1:8080"
sandbox:
  state_dir: "/tmp/harbor-test-state"
agents:
  - id: oc
    display_name: "oc"
    description: "Test agent for the oc CLI."
    binary: opencode
    integration: exec
    args: ["run", "{{.Prompt}}"]
`)
	_, err := loadConfig(t, path)
	if err == nil || !strings.Contains(err.Error(), "workdir is required") {
		t.Fatalf("expected a workdir error, got %v", err)
	}
}

func TestWorkdirStoredAbsolute(t *testing.T) {
	path := writeConfig(t, `
version: 1
agents:
  - id: oc
    display_name: "oc"
    description: "Test agent for the oc CLI."
    binary: opencode
    integration: exec
    args: ["run", "{{.Prompt}}"]
    workdir: "."
`)
	cfg, err := loadConfig(t, path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Agents[0].Workdir; got != want {
		t.Errorf("workdir = %q, want %q", got, want)
	}
}

func TestSandboxPathsStoredAbsolute(t *testing.T) {
	path := writeConfig(t, `
version: 1
sandbox:
  state_dir: `+strconv.Quote(t.TempDir())+`
  isolation_root: "relative/iso"
  cli_home: "relative/home"
agents:
  - id: oc
    display_name: "oc"
    description: "Test agent for the oc CLI."
    binary: opencode
    integration: exec
    args: ["run", "{{.Prompt}}"]
`)
	cfg, err := loadConfig(t, path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, tc := range []struct {
		name string
		got  string
		raw  string
	}{
		{"isolation_root", cfg.Sandbox.IsolationRoot, "relative/iso"},
		{"cli_home", cfg.Sandbox.CLIHome, "relative/home"},
	} {
		want, err := filepath.Abs(tc.raw)
		if err != nil {
			t.Fatal(err)
		}
		if tc.got != want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, want)
		}
	}
}

func TestStoreMustBeKnown(t *testing.T) {
	path := writeConfig(t, `
version: 1
server:
  listen: "127.0.0.1:8080"
  store: bogus
agents:
  - id: oc
    display_name: "oc"
    description: "Test agent for the oc CLI."
    binary: opencode
    integration: exec
    args: ["run", "{{.Prompt}}"]
`)
	_, err := loadConfig(t, path)
	if err == nil || !strings.Contains(err.Error(), "store") {
		t.Fatalf("expected a store error, got %v", err)
	}
}

func TestSQLiteStoreRequiresAPath(t *testing.T) {
	path := writeConfig(t, `
version: 1
server:
  listen: "127.0.0.1:8080"
  store: sqlite
agents:
  - id: oc
    display_name: "oc"
    description: "Test agent for the oc CLI."
    binary: opencode
    integration: exec
    args: ["run", "{{.Prompt}}"]
`)
	_, err := loadConfig(t, path)
	if err == nil || !strings.Contains(err.Error(), "store_path") {
		t.Fatalf("expected a path error, got %v", err)
	}
}

func TestAgentInactivityTimeout(t *testing.T) {
	path := writeConfig(t, `
version: 1
server:
  listen: "127.0.0.1:8080"
  agent_inactivity_timeout: 30s
agents:
  - id: oc
    display_name: "oc"
    description: "Test agent for the oc CLI."
    binary: opencode
    integration: exec
    args: ["run", "{{.Prompt}}"]
`)
	cfg, err := loadConfig(t, path)
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
	_, err := loadConfig(t, path)
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
	cfg, err := loadConfig(t, path)
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
  state_dir: "/tmp/harbor-test-state"
  inherit_env: false
agents:
  - id: oc
    display_name: "oc"
    description: "Test agent for the oc CLI."
    binary: opencode
    integration: exec
    args: ["run", "{{.Prompt}}"]
`)
	cfg, err := loadConfig(t, path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Sandbox.InheritEnvEnabled() {
		t.Fatal("inherit_env: false should disable inheritance")
	}
}

func TestContainerRuntimeRequired(t *testing.T) {
	path := writeConfig(t, `
version: 1
sandbox:
  state_dir: "/tmp/harbor-test-state"
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
	_, err := loadConfig(t, path)
	if err == nil || !strings.Contains(err.Error(), "sandbox.container.runtime is required") {
		t.Fatalf("expected a runtime error, got %v", err)
	}
}

func TestContainerRuntime(t *testing.T) {
	cases := []struct {
		runtime string
		wantErr bool
	}{
		{"docker", false},
		{"podman", false},
		{"nerdctl", false},
		{"lxc", true},
	}
	for _, tc := range cases {
		path := writeConfig(t, `
version: 1
sandbox:
  state_dir: "/tmp/harbor-test-state"
  container:
    image: harbor/claude:latest
    runtime: `+tc.runtime+`
agents:
  - id: oc
    display_name: "oc"
    description: "Test agent for the oc CLI."
    binary: opencode
    integration: exec
    args: ["run", "{{.Prompt}}"]
`)
		cfg, err := loadConfig(t, path)
		if (err != nil) != tc.wantErr {
			t.Errorf("runtime %q: err = %v, wantErr %v", tc.runtime, err, tc.wantErr)
			continue
		}
		if err == nil && string(cfg.Sandbox.Container.Runtime) != tc.runtime {
			t.Errorf("runtime = %q, want %q", cfg.Sandbox.Container.Runtime, tc.runtime)
		}
	}
}

func TestContainerExtraArgs(t *testing.T) {
	path := writeConfig(t, `
version: 1
sandbox:
  state_dir: "/tmp/harbor-test-state"
  container:
    image: harbor/claude:latest
    runtime: podman
    extra_args: ["--network=none", "--read-only", "--pids-limit=512"]
agents:
  - id: oc
    display_name: "oc"
    description: "Test agent for the oc CLI."
    binary: opencode
    integration: exec
    args: ["run", "{{.Prompt}}"]
`)
	cfg, err := loadConfig(t, path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ct := cfg.Sandbox.Container
	if ct.Runtime != core.ContainerRuntimePodman {
		t.Errorf("runtime = %q, want podman", ct.Runtime)
	}
	want := []string{"--network=none", "--read-only", "--pids-limit=512"}
	if !reflect.DeepEqual(ct.ExtraArgs, want) {
		t.Errorf("extra_args = %q, want %q", ct.ExtraArgs, want)
	}
}

func TestContainerRequiresImage(t *testing.T) {
	path := writeConfig(t, `
version: 1
sandbox:
  state_dir: "/tmp/harbor-test-state"
  container:
    runtime: docker
agents:
  - id: oc
    display_name: "oc"
    description: "Test agent for the oc CLI."
    binary: opencode
    integration: exec
    args: ["run", "{{.Prompt}}"]
`)
	_, err := loadConfig(t, path)
	if err == nil || !strings.Contains(err.Error(), "sandbox.container.image is required") {
		t.Fatalf("expected a container image error, got %v", err)
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
	cfg, err := loadConfig(t, path)
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
	p, ok := cli.Lookup("codebuddy")
	if !ok || len(p.OutputImage) == 0 {
		t.Fatalf("codebuddy profile OutputImage = %v, want image types", p.OutputImage)
	}
}

func TestRequireContainerForDangerous(t *testing.T) {
	bad := writeConfig(t, `
version: 1
sandbox:
  state_dir: "/tmp/harbor-test-state"
  require_container_for_dangerous: true
agents:
  - id: cb
    display_name: "cb"
    description: "Test agent for the cb CLI."
    binary: codebuddy
    integration: stream-json
    allow_dangerous: true
`)
	if _, err := loadConfig(t, bad); err == nil || !strings.Contains(err.Error(), "require_container_for_dangerous") {
		t.Fatalf("expected a container-required error, got %v", err)
	}

	good := writeConfig(t, `
version: 1
sandbox:
  state_dir: "/tmp/harbor-test-state"
  require_container_for_dangerous: true
  container: { image: "harbor/cb:latest", runtime: docker }
agents:
  - id: cb
    display_name: "cb"
    description: "Test agent for the cb CLI."
    binary: codebuddy
    integration: stream-json
    allow_dangerous: true
`)
	if _, err := loadConfig(t, good); err != nil {
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
	cfg, err := loadConfig(t, path)
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
	cfg, err := loadConfig(t, path)
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
	_, err := loadConfig(t, path)
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
	cfg, err := loadConfig(t, path)
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

func TestAgentSpecMapping(t *testing.T) {
	ac := config.AgentConfig{
		ID: "a", DisplayName: "A", Description: "d", Binary: "b",
		Integration: core.IntegrationExec, Parser: "codex",
		Regex:     &config.RegexConfig{Text: "^x$"},
		ServerURL: "http://x", ServerProtocol: core.ServerProtocolACP,
		Args: []string{"a"}, Env: map[string]string{"K": "V"},
		AllowDangerous: true, SDKCaps: true,
		OutputSchemaArgs: []string{"o"}, FileArgs: []string{"f"}, ImageArgs: []string{"i"},
		InputImage: []string{"image/png"}, Interaction: core.InteractionInteractive,
		Session: config.SessionConfig{IDFlag: "--id", ResumeFlag: "--r", ContinueFlag: "--c", ForkFlag: "--f"},
		Limits: config.LimitsConfig{
			MaxConcurrent: 2, Timeout: time.Minute, IdleTimeout: time.Second, MaxTurns: 3,
			Breaker: &config.BreakerConfig{
				Threshold: 4, FailureRatio: 0.5, HalfOpenSuccesses: 2,
				Cooldown: time.Second, MaxCooldown: time.Minute,
			},
		},
		Skills: []config.SkillConfig{{
			ID: "s", Name: "n", Description: "d",
			Tags: []string{"t"}, Examples: []string{"e"},
			InputModes: []string{"in"}, OutputModes: []string{"out"},
		}},
		Version: "1",
	}
	want := core.AgentSpec{
		ID: "a", DisplayName: "A", Description: "d", Binary: "b",
		Integration: core.IntegrationExec, Parser: core.ParserCodex,
		Regex:     &core.Regex{Text: "^x$"},
		ServerURL: "http://x", ServerProtocol: core.ServerProtocolACP,
		Args: []string{"a"}, Env: map[string]string{"K": "V"},
		AllowDangerous: true, SDKCaps: true,
		OutputSchemaArgs: []string{"o"}, FileArgs: []string{"f"}, ImageArgs: []string{"i"},
		InputImage: []string{"image/png"}, Interaction: core.InteractionInteractive,
		Session: core.Session{IDFlag: "--id", ResumeFlag: "--r", ContinueFlag: "--c", ForkFlag: "--f"},
		Limits: core.Limits{
			MaxConcurrent: 2, Timeout: time.Minute, IdleTimeout: time.Second, MaxTurns: 3,
			Breaker: &core.Breaker{
				Threshold: 4, FailureRatio: 0.5, HalfOpenSuccesses: 2,
				Cooldown: time.Second, MaxCooldown: time.Minute,
			},
		},
		Skills: []core.Skill{{
			ID: "s", Name: "n", Description: "d",
			Tags: []string{"t"}, Examples: []string{"e"},
			InputModes: []string{"in"}, OutputModes: []string{"out"},
		}},
		Version: "1",
	}
	if got := ac.Spec(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Spec() = %+v, want %+v", got, want)
	}
}
