//go:build realcx

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

func codexHandler(t *testing.T, dir string, ac config.AgentConfig) a2asrv.RequestHandler {
	t.Helper()
	ln, err := integration.New(ac, integration.Options{InheritEnv: true, Ctx: t.Context()})
	if err != nil {
		t.Fatalf("integration.New: %v", err)
	}
	exec := bridge.NewCLIExecutor(ln, session.New(), agent.NewLiveSessions(),
		workspace.NewResolver(dir), core.Limits{Timeout: 120 * time.Second})
	return a2asrv.NewHandler(exec, a2asrv.WithCapabilityChecks(&a2a.AgentCapabilities{Streaming: true}))
}

func codexConfig() config.AgentConfig {
	model := os.Getenv("HARBOR_REAL_CODEX_MODEL")
	if model == "" {
		model = "gpt-5.6-terra"
	}

	return config.AgentConfig{
		ID: "cx", Binary: "codex", Integration: core.IntegrationExec, Parser: core.ParserCodex,
		Args: []string{
			"exec", "--json", "--skip-git-repo-check",
			"--dangerously-bypass-approvals-and-sandbox", "-m", model, "{{.Prompt}}",
		},
	}
}

func requireCodex(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("codex"); err != nil {
		t.Fatalf("codex not installed: %v", err)
	}
}

func expectCodexText(t *testing.T, task *a2a.Task, want string) {
	t.Helper()
	if text := artifactText(task); !strings.Contains(strings.ToLower(text), want) {
		t.Errorf("expected %q, got %q", want, text)
	}
}

func TestRealCodexBashTool(t *testing.T) {
	requireCodex(t)
	h := codexHandler(t, t.TempDir(), codexConfig())
	task, _ := sendTask(t, h, message(
		"Use your shell tool to run the command `echo $((6 * 7))`, then reply with only the command's output.",
	))
	expectCodexText(t, task, "42")
}

func TestRealCodexFileWrite(t *testing.T) {
	requireCodex(t)
	dir := t.TempDir()
	h := codexHandler(t, dir, codexConfig())
	task, _ := sendTask(t, h, message(
		"Use your file tools to create the file output.txt in the current working directory "+
			"whose exact contents are CODEX42 (no trailing newline). Then reply with the single word: written",
	))
	expectCodexText(t, task, "written")

	got, err := os.ReadFile(filepath.Join(dir, "output.txt"))
	if err != nil {
		t.Fatalf("the model did not create output.txt: %v", err)
	}
	if strings.TrimSpace(string(got)) != "CODEX42" {
		t.Errorf("file contents = %q, want CODEX42", string(got))
	}
}

func TestRealCodexFileRead(t *testing.T) {
	requireCodex(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "input.txt"), []byte("READ42"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := codexHandler(t, dir, codexConfig())
	task, _ := sendTask(t, h, message(
		"Read the file input.txt in the current working directory and reply with only its contents.",
	))
	expectCodexText(t, task, "read42")
}
