//go:build realcl

package bridge_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/jiaweisky/harbor/internal/agent"
	"github.com/jiaweisky/harbor/internal/bridge"
	"github.com/jiaweisky/harbor/internal/config"
	"github.com/jiaweisky/harbor/internal/core"
	"github.com/jiaweisky/harbor/internal/integration"
	"github.com/jiaweisky/harbor/internal/session"
	"github.com/jiaweisky/harbor/internal/workspace"
)

func claudeHandler(t *testing.T, dir string, ac config.AgentConfig) a2asrv.RequestHandler {
	t.Helper()
	ln, err := integration.New(ac, integration.Options{InheritEnv: true, Ctx: t.Context()})
	if err != nil {
		t.Fatalf("integration.New: %v", err)
	}
	exec := bridge.NewCLIExecutor(ln, session.New(), agent.NewLiveSessions(),
		workspace.NewResolver(dir), core.Limits{Timeout: 120 * time.Second})
	return a2asrv.NewHandler(exec, a2asrv.WithCapabilityChecks(&a2a.AgentCapabilities{Streaming: true}))
}

func claudeConfig() config.AgentConfig {
	return config.AgentConfig{
		ID: "cl", Binary: "claude", Integration: core.IntegrationStreamJSON,
		Args: []string{
			"-p", "--input-format", "stream-json", "--output-format", "stream-json",
			"--include-partial-messages", "--verbose", "--dangerously-skip-permissions",
		},
		Session: config.SessionConfig{IDFlag: "--session-id", ResumeFlag: "--resume", ContinueFlag: "-c"},
	}
}

func requireClaude(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("claude"); err != nil {
		t.Fatalf("claude not installed: %v", err)
	}
}

func expectClaudeText(t *testing.T, task *a2a.Task, want string) {
	t.Helper()
	if text := artifactText(task); !strings.Contains(strings.ToLower(text), want) {
		t.Errorf("expected %q, got %q", want, text)
	}
}

func TestRealClaudeBashTool(t *testing.T) {
	requireClaude(t)
	h := claudeHandler(t, t.TempDir(), claudeConfig())
	task, _ := sendTask(t, h, message(
		"Use your Bash tool to run the command `echo $((6 * 7))`, then reply with only the command's output.",
	))
	expectClaudeText(t, task, "42")
}

func TestRealClaudeFileWrite(t *testing.T) {
	requireClaude(t)
	dir := t.TempDir()
	h := claudeHandler(t, dir, claudeConfig())
	task, _ := sendTask(t, h, message(
		"Use your file tools to create the file output.txt in the current working directory "+
			"whose exact contents are CLAUDE42 (no trailing newline). Then reply with the single word: written",
	))
	expectClaudeText(t, task, "written")

	got, err := os.ReadFile(filepath.Join(dir, "output.txt"))
	if err != nil {
		t.Fatalf("the model did not create output.txt: %v", err)
	}
	if strings.TrimSpace(string(got)) != "CLAUDE42" {
		t.Errorf("file contents = %q, want CLAUDE42", string(got))
	}
}

func TestRealClaudeFileRead(t *testing.T) {
	requireClaude(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "input.txt"), []byte("READ42"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := claudeHandler(t, dir, claudeConfig())
	task, _ := sendTask(t, h, message(
		"Read the file input.txt in the current working directory and reply with only its contents.",
	))
	expectClaudeText(t, task, "read42")
}
