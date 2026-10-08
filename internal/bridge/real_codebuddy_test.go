//go:build realcb

package bridge_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/jiaweisky/harbor/internal/acp"
	"github.com/jiaweisky/harbor/internal/agent"
	"github.com/jiaweisky/harbor/internal/bridge"
	"github.com/jiaweisky/harbor/internal/config"
	"github.com/jiaweisky/harbor/internal/integration"
	"github.com/jiaweisky/harbor/internal/session"
)

func codebuddyHandler(t *testing.T, ac config.AgentConfig) a2asrv.RequestHandler {
	t.Helper()
	// The context bounds a spawned `codebuddy --serve` process in server mode.
	ln, err := integration.New(ac, integration.Options{InheritEnv: true, Ctx: t.Context()})
	if err != nil {
		t.Fatalf("integration.New: %v", err)
	}
	exec := bridge.NewCLIExecutor(ln, session.New(), agent.NewLiveSessions(), nil,
		config.LimitsConfig{Timeout: 150 * time.Second})
	return a2asrv.NewHandler(exec, a2asrv.WithCapabilityChecks(&a2a.AgentCapabilities{Streaming: true}))
}

func requireCodebuddy(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("codebuddy"); err != nil {
		t.Skip("codebuddy not installed")
	}
}

func TestRealCodebuddyStreamJSON(t *testing.T) {
	requireCodebuddy(t)
	h := codebuddyHandler(t, config.AgentConfig{
		ID: "cb", Binary: "codebuddy", Integration: config.IntegrationStreamJSON,
		Args: []string{
			"-p", "--input-format", "stream-json",
			"--output-format", "stream-json", "--include-partial-messages",
		},
		Session: config.SessionConfig{IDFlag: "--session-id", ResumeFlag: "--resume", ContinueFlag: "-c"},
	})
	sendAndExpect(t, h, message("reply with the single word: pong"), "pong")
}

func TestRealCodebuddyResume(t *testing.T) {
	requireCodebuddy(t)
	h := codebuddyHandler(t, config.AgentConfig{
		ID: "cb", Binary: "codebuddy", Integration: config.IntegrationStreamJSON,
		Args: []string{
			"-p", "--input-format", "stream-json",
			"--output-format", "stream-json", "--include-partial-messages",
		},
		Session: config.SessionConfig{IDFlag: "--session-id", ResumeFlag: "--resume", ContinueFlag: "-c"},
	})
	m1 := message("reply with the single word: alpha")
	m1.ContextID = "real-cb-resume"
	sendAndExpect(t, h, m1, "alpha")

	m2 := message("reply with the single word: beta")
	m2.ContextID = "real-cb-resume"
	sendAndExpect(t, h, m2, "beta")
}

func TestRealCodebuddyACP(t *testing.T) {
	requireCodebuddy(t)
	h := codebuddyHandler(t, config.AgentConfig{
		ID: "cb", Binary: "codebuddy", Integration: config.IntegrationACP,
		Args: []string{"--acp"},
	})
	sendAndExpect(t, h, message("reply with the single word: pong"), "pong")
}

func TestRealCodebuddyExec(t *testing.T) {
	requireCodebuddy(t)
	h := codebuddyHandler(t, config.AgentConfig{
		ID: "cb", Binary: "codebuddy", Integration: config.IntegrationExec,
		Args: []string{"-p", "{{.Prompt}}"},
	})
	sendAndExpect(t, h, message("reply with the single word: pong"), "pong")
}

func TestRealCodebuddyServer(t *testing.T) {
	requireCodebuddy(t)
	h := codebuddyHandler(t, config.AgentConfig{
		ID: "cb", Binary: "codebuddy", Integration: config.IntegrationServer,
	})
	sendAndExpect(t, h, message("reply with the single word: pong"), "pong")
}

func TestRealCodebuddyServerACP(t *testing.T) {
	requireCodebuddy(t)
	h := codebuddyHandler(t, config.AgentConfig{
		ID: "cb", Binary: "codebuddy", Integration: config.IntegrationServer,
		ServerProtocol: config.ServerProtocolACP,
	})
	sendAndExpect(t, h, message("reply with the single word: pong"), "pong")
}

// TestRealCodebuddyServerACPTool proves the ACP-over-HTTP transport drives real
// tool use (server started with a bypass permission mode so no prompt pauses the
// turn).
func TestRealCodebuddyServerACPTool(t *testing.T) {
	requireCodebuddy(t)
	ln := acp.NewHTTPLauncher(acp.HTTPConfig{
		Name: "cb-acp", Binary: "codebuddy",
		Args: []string{"--serve", "--host", "127.0.0.1", "--auth", "none",
			"--permission-mode", "bypassPermissions"},
		Headers:    http.Header{"X-CodeBuddy-Request": {"1"}},
		InheritEnv: true, Lifecycle: t.Context(),
	})
	exec := bridge.NewCLIExecutor(ln, session.New(), agent.NewLiveSessions(), nil,
		config.LimitsConfig{Timeout: 150 * time.Second})
	h := a2asrv.NewHandler(exec, a2asrv.WithCapabilityChecks(&a2a.AgentCapabilities{Streaming: true}))

	prompt := "Use your Bash tool to run the command `echo $((6 * 7))`, then reply with only the command's output."
	sendAndExpect(t, h, message(prompt), "42")
}

func TestRealCodebuddyServerResume(t *testing.T) {
	requireCodebuddy(t)
	h := codebuddyHandler(t, config.AgentConfig{
		ID: "cb", Binary: "codebuddy", Integration: config.IntegrationServer,
	})
	m1 := message("reply with the single word: alpha")
	m1.ContextID = "real-cb-serve-resume"
	sendAndExpect(t, h, m1, "alpha")

	m2 := message("reply with the single word: beta")
	m2.ContextID = "real-cb-serve-resume"
	sendAndExpect(t, h, m2, "beta")
}

// TestRealCodebuddyPermission verifies a headless (stream-json) tool permission
// pauses the task as input-required and resumes to completion when answered.
func TestRealCodebuddyPermission(t *testing.T) {
	requireCodebuddy(t)
	h := codebuddyHandler(t, config.AgentConfig{
		ID: "cb", Binary: "codebuddy", Integration: config.IntegrationStreamJSON,
		Args: []string{
			"-p", "--input-format", "stream-json",
			"--output-format", "stream-json", "--include-partial-messages",
		},
		Session: config.SessionConfig{IDFlag: "--session-id", ResumeFlag: "--resume", ContinueFlag: "-c"},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	task := sendPaused(t, h, ctx, "Use the Bash tool to run the command: echo perm42")
	itx := interactionFromMessage(t, task.Status.Message)
	if itx.Kind != agent.InteractionPermission || itx.ID == "" {
		t.Fatalf("interaction = %+v, want a permission with an id", itx)
	}

	done := answerEvent(t, h, ctx, task, itx, "allow")
	if done.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("state = %s, want completed (text=%q)", done.Status.State, artifactText(done))
	}
	if got := artifactText(done); !strings.Contains(strings.ToLower(got), "perm42") {
		t.Errorf("artifact = %q, want it to contain perm42", got)
	}
}

// TestRealCodebuddyQuestion verifies the AskUserQuestion tool (enabled by the
// SDK capability handshake) surfaces as an input-required question and the
// chosen answer is injected back through the control_response.
func TestRealCodebuddyQuestion(t *testing.T) {
	requireCodebuddy(t)
	h := codebuddyHandler(t, config.AgentConfig{
		ID: "cb", Binary: "codebuddy", Integration: config.IntegrationStreamJSON,
		SDKCaps: true,
		Args: []string{
			"-p", "--input-format", "stream-json",
			"--output-format", "stream-json", "--include-partial-messages",
		},
		Session: config.SessionConfig{IDFlag: "--session-id", ResumeFlag: "--resume", ContinueFlag: "-c"},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()

	prompt := "Use the AskUserQuestion tool to ask my favorite color, offering exactly Red and Blue. " +
		"Then reply with only my choice."
	task := sendPaused(t, h, ctx, prompt)
	itx := interactionFromMessage(t, task.Status.Message)
	if itx.Kind != agent.InteractionQuestion || itx.Target == "" {
		t.Fatalf("interaction = %+v, want a question with a target", itx)
	}

	done := answerEvent(t, h, ctx, task, itx, "Blue")
	if done.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("state = %s, want completed (text=%q)", done.Status.State, artifactText(done))
	}
	if got := artifactText(done); !strings.Contains(strings.ToLower(got), "blue") {
		t.Errorf("artifact = %q, want it to mention the chosen answer (blue)", got)
	}
}

// TestRealCodebuddyServerBashTool verifies serve mode can actually use tools:
// the model must run a shell command and report its output.
func TestRealCodebuddyServerBashTool(t *testing.T) {
	requireCodebuddy(t)
	h := codebuddyHandler(t, config.AgentConfig{
		ID: "cb", Binary: "codebuddy", Integration: config.IntegrationServer,
	})
	prompt := "Use your shell (Bash) tool to run the command `echo $((6 * 7))`, " +
		"then reply with only the command's output."
	sendAndExpect(t, h, message(prompt), "42")
}

// TestRealCodebuddyServerFileTool verifies serve mode writes a file the task
// asked for, at an absolute path outside the server's working directory.
func TestRealCodebuddyServerFileTool(t *testing.T) {
	requireCodebuddy(t)
	target := filepath.Join(t.TempDir(), "output.txt")
	h := codebuddyHandler(t, config.AgentConfig{
		ID: "cb", Binary: "codebuddy", Integration: config.IntegrationServer,
	})
	prompt := fmt.Sprintf(
		"Use your file-writing tool to create the file %q whose exact contents are "+
			"HELLO42 (no trailing newline). Then reply with only the word: written", target)
	sendAndExpect(t, h, message(prompt), "written")

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("the model did not create %s: %v", target, err)
	}
	if strings.TrimSpace(string(got)) != "HELLO42" {
		t.Errorf("file contents = %q, want HELLO42", string(got))
	}
}

// TestRealCodebuddyServerResumeToolContext verifies tools and conversation
// continuity together: turn one writes a value, turn two reads and increments
// the same file in the same conversation.
func TestRealCodebuddyServerResumeToolContext(t *testing.T) {
	requireCodebuddy(t)
	target := filepath.Join(t.TempDir(), "counter.txt")
	h := codebuddyHandler(t, config.AgentConfig{
		ID: "cb", Binary: "codebuddy", Integration: config.IntegrationServer,
	})

	m1 := message(fmt.Sprintf(
		"Use your file-writing tool to create the file %q containing exactly the "+
			"single character 1. Then reply with only the character you wrote.", target))
	m1.ContextID = "real-cb-serve-tool"
	sendAndExpect(t, h, m1, "1")

	m2 := message(fmt.Sprintf(
		"Read the file %q, add one to the number in it, write the new value back to "+
			"the same file, then reply with only the new number.", target))
	m2.ContextID = "real-cb-serve-tool"
	sendAndExpect(t, h, m2, "2")

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read %s: %v", target, err)
	}
	if strings.TrimSpace(string(got)) != "2" {
		t.Errorf("file contents = %q, want 2", strings.TrimSpace(string(got)))
	}
}

// TestRealCodebuddyServerCancel verifies a running serve-mode task can be
// canceled through the A2A CancelTask path.
func TestRealCodebuddyServerCancel(t *testing.T) {
	requireCodebuddy(t)
	h := codebuddyHandler(t, config.AgentConfig{
		ID: "cb", Binary: "codebuddy", Integration: config.IntegrationServer,
	})
	cancelRunningTask(t, h, "Use the Bash tool to run the command: sleep 90 ; then reply done.")
}
