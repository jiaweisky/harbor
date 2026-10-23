package bridge

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/jiaweisky/harbor/internal/agent"
	"github.com/jiaweisky/harbor/internal/config"
	"github.com/jiaweisky/harbor/internal/core"
	"github.com/jiaweisky/harbor/internal/integration"
	"github.com/jiaweisky/harbor/internal/session"
)

// TestResumeFreshInjectsAnswer verifies that answering a task whose live process
// is gone (for example after a restart) resumes the CLI session and injects the
// answer.
func TestResumeFreshInjectsAnswer(t *testing.T) {
	script, err := filepath.Abs(filepath.Join("..", "..", "testdata", "fake-cli", "fake-resume.sh"))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := integration.New(config.AgentConfig{
		ID: "rec", Binary: script, Integration: config.IntegrationStreamJSON,
		Args:    []string{"-p"},
		Session: config.SessionConfig{ResumeFlag: "--resume"},
	}, integration.Options{InheritEnv: true})
	if err != nil {
		t.Fatalf("integration.New: %v", err)
	}
	exec := NewCLIExecutor(ln, session.New(), agent.NewLiveSessions(), nil,
		core.Limits{Timeout: 10 * time.Second})
	h := a2asrv.NewHandler(exec, a2asrv.WithCapabilityChecks(&a2a.AgentCapabilities{Streaming: true}))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	resp, err := h.SendMessage(ctx, &a2a.SendMessageRequest{
		Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("go")),
	})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	task := resp.(*a2a.Task)
	if task.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("state = %s, want input-required", task.Status.State)
	}
	if sid := task.Status.Message.Metadata[metaKeySessionID]; sid != "sess-xyz" {
		t.Fatalf("sessionId = %v, want sess-xyz", sid)
	}

	// Simulate the process being gone (Harbor restarted): drop the live session
	// and the whole per-task state, so the interaction must be reconstructed
	// from the persisted input-required status message.
	exec.live.Del(string(task.ID))
	exec.tasks.Delete(string(task.ID))

	answer := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("allow"))
	answer.TaskID = task.ID
	answer.ContextID = task.ContextID
	resp2, err := h.SendMessage(ctx, &a2a.SendMessageRequest{Message: answer})
	if err != nil {
		t.Fatalf("resume SendMessage: %v", err)
	}
	done := resp2.(*a2a.Task)
	if done.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("state = %s, want completed", done.Status.State)
	}
	if got := taskText(done); !strings.Contains(got, "resumed") {
		t.Fatalf("artifact = %q, want the resumed result", got)
	}
}

// taskText concatenates a task's artifact text (internal test helper).
func taskText(task *a2a.Task) string {
	var b strings.Builder
	for _, a := range task.Artifacts {
		for _, p := range a.Parts {
			b.WriteString(p.Text())
		}
	}
	return b.String()
}

// TestDuplicateTaskScopedByCaller verifies a repeated message id is treated as a
// duplicate only for the same caller, so one caller cannot replay another's task.
func TestDuplicateTaskScopedByCaller(t *testing.T) {
	orig := &a2a.Task{ID: "t1", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}}
	e := &CLIExecutor{idem: newIdemCache(8)}
	e.taskLookup = func(_ context.Context, id a2a.TaskID) (*a2a.Task, bool) {
		return orig, id == "t1"
	}
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi"))
	msg.ID = "m1"

	alice := func(taskID a2a.TaskID) *a2asrv.ExecutorContext {
		return &a2asrv.ExecutorContext{
			TaskID: taskID, ContextID: "c1", Message: msg,
			User: a2asrv.NewAuthenticatedUser("alice", nil),
		}
	}
	if _, ok := e.duplicateTask(context.Background(), alice("t1")); ok {
		t.Fatal("the first occurrence must not be a duplicate")
	}
	if _, ok := e.duplicateTask(context.Background(), alice("t2")); !ok {
		t.Fatal("the same caller with the same message id must replay")
	}
	bob := &a2asrv.ExecutorContext{
		TaskID: "t3", ContextID: "c2", Message: msg,
		User: a2asrv.NewAuthenticatedUser("bob", nil),
	}
	if _, ok := e.duplicateTask(context.Background(), bob); ok {
		t.Fatal("a different caller must not replay another caller's task")
	}
}
