//go:build realoc

package bridge_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/jiaweisky/harbor/internal/config"
	"github.com/jiaweisky/harbor/internal/core"
	"github.com/jiaweisky/harbor/internal/workspace"
)

// opencodeWorkspaceHandler builds a handler whose CLI is started in dir, so
// relative tool writes land there.
func opencodeWorkspaceHandler(t *testing.T, ctx context.Context, dir string, ac config.AgentConfig) a2asrv.RequestHandler {
	return opencodeHandlerWith(t, ctx, workspace.NewResolver(dir), 120*time.Second, ac)
}

// expectText asserts a completed task's artifact contains want. Tool tasks can be
// slow, so this does not impose the fast-completion bound sendAndExpect uses.
func expectText(t *testing.T, task *a2a.Task, want string) {
	t.Helper()
	if text := artifactText(task); !strings.Contains(strings.ToLower(text), want) {
		t.Errorf("expected %q, got %q", want, text)
	}
}

func opencodeExecConfig() config.AgentConfig {
	return config.AgentConfig{
		ID: "oc", Binary: "opencode", Integration: core.IntegrationExec,
		Args:    []string{"run", "{{.Prompt}}"},
		Session: config.SessionConfig{IDFlag: "--session", ContinueFlag: "--continue"},
	}
}

func TestRealOpencodeExecTool(t *testing.T) {
	requireOpencode(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h := opencodeWorkspaceHandler(t, ctx, t.TempDir(), opencodeExecConfig())
	prompt := "Use your Bash tool to run the command `echo $((6 * 7))`, then reply with only the command's output."
	task, _ := sendTask(t, h, message(prompt))
	expectText(t, task, "42")
}

func TestRealOpencodeExecFile(t *testing.T) {
	requireOpencode(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	dir := t.TempDir()
	h := opencodeWorkspaceHandler(t, ctx, dir, opencodeExecConfig())
	prompt := "Use your file tools to create the file output.txt in the current working " +
		"directory whose exact contents are OC42 (no trailing newline). Then reply with the single word: written"
	task, _ := sendTask(t, h, message(prompt))
	expectText(t, task, "written")

	got, err := os.ReadFile(filepath.Join(dir, "output.txt"))
	if err != nil {
		t.Fatalf("the model did not create output.txt: %v", err)
	}
	if strings.TrimSpace(string(got)) != "OC42" {
		t.Errorf("file contents = %q, want OC42", string(got))
	}
}

func TestRealOpencodeACPTool(t *testing.T) {
	requireOpencode(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	dir := t.TempDir()
	h := opencodeWorkspaceHandler(t, ctx, dir, config.AgentConfig{
		ID: "oc", Binary: "opencode", Integration: core.IntegrationACP, Args: []string{"acp"},
	})
	prompt := "Use your Bash tool to run the command `echo $((6 * 7))`, then reply with only the command's output."
	task, _ := sendTask(t, h, message(prompt))
	expectText(t, task, "42")
}

func TestRealOpencodeACPToolFile(t *testing.T) {
	requireOpencode(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	dir := t.TempDir()
	h := opencodeWorkspaceHandler(t, ctx, dir, config.AgentConfig{
		ID: "oc", Binary: "opencode", Integration: core.IntegrationACP, Args: []string{"acp"},
	})
	prompt := "Use your file tools to create the file acp.txt in the current working " +
		"directory whose exact contents are ACP42. Then reply with the single word: written"
	task, _ := sendTask(t, h, message(prompt))
	expectText(t, task, "written")

	got, err := os.ReadFile(filepath.Join(dir, "acp.txt"))
	if err != nil {
		t.Fatalf("the model did not create acp.txt: %v", err)
	}
	if strings.TrimSpace(string(got)) != "ACP42" {
		t.Errorf("file contents = %q, want ACP42", string(got))
	}
}

// TestRealOpencodeFileAttachment verifies a raw file part reaches opencode via
// its -f attachment flag (its content is available to the model).
func TestRealOpencodeFileAttachment(t *testing.T) {
	requireOpencode(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ac := opencodeExecConfig()
	ac.FileArgs = []string{"-f", "{{.File}}"}
	h := opencodeWorkspaceHandler(t, ctx, t.TempDir(), ac)

	file := a2a.NewRawPart([]byte("the codeword is ORANGE"))
	file.Filename = "secret.txt"
	file.MediaType = "text/plain"
	m := a2a.NewMessage(a2a.MessageRoleUser,
		a2a.NewTextPart("Reply with only the codeword found in the attached file."), file)

	task, _ := sendTask(t, h, m)
	if got := strings.ToLower(artifactText(task)); !strings.Contains(got, "orange") {
		t.Errorf("artifact = %q, want it to mention the codeword", got)
	}
}

func TestRealOpencodeServerTool(t *testing.T) {
	requireOpencode(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h := opencodeHandler(t, ctx, config.AgentConfig{
		ID: "oc", Binary: "opencode", Integration: core.IntegrationServer,
	})
	prompt := "Use your Bash tool to run the command `echo $((6 * 7))`, then reply with only the command's output."
	task, _ := sendTask(t, h, message(prompt))
	expectText(t, task, "42")
}

// TestRealOpencodeServerCancel cancels a running server-mode task via the A2A
// CancelTask path.
func TestRealOpencodeServerCancel(t *testing.T) {
	requireOpencode(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h := opencodeHandler(t, ctx, config.AgentConfig{
		ID: "oc", Binary: "opencode", Integration: core.IntegrationServer,
	})
	cancelRunningTask(t, h, "Use your Bash tool to run the command `sleep 90`, then reply done.")
}
