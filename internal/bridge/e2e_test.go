package bridge_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/push"

	"github.com/jiaweisky/harbor/internal/agent"
	"github.com/jiaweisky/harbor/internal/bridge"
	"github.com/jiaweisky/harbor/internal/config"
	"github.com/jiaweisky/harbor/internal/integration"
	"github.com/jiaweisky/harbor/internal/session"
	"github.com/jiaweisky/harbor/internal/workspace"
)

func scriptPath(t *testing.T, name string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "testdata", "fake-cli", name))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func newLauncher(
	t *testing.T, script string, integ config.Integration, args []string, env map[string]string,
) agent.Launcher {
	return newLauncherParser(t, script, integ, "", args, env)
}

func newLauncherParser(
	t *testing.T, script string, integ config.Integration, parserName string,
	args []string, env map[string]string,
) agent.Launcher {
	t.Helper()
	ac := config.AgentConfig{
		ID:          string(integ),
		Binary:      script,
		Integration: integ,
		Parser:      parserName,
		Args:        args,
		Env:         env,
		Session:     config.SessionConfig{IDFlag: "--session-id", ResumeFlag: "--resume", ContinueFlag: "--continue"},
	}
	ln, err := integration.New(ac, integration.Options{InheritEnv: true})
	if err != nil {
		t.Fatalf("integration.New: %v", err)
	}
	return ln
}

func newHandler(t *testing.T, ln agent.Launcher) a2asrv.RequestHandler {
	t.Helper()
	exec := bridge.NewCLIExecutor(ln, session.New(), agent.NewLiveSessions(), nil, config.LimitsConfig{})
	return a2asrv.NewHandler(exec, a2asrv.WithCapabilityChecks(&a2a.AgentCapabilities{Streaming: true}))
}

// newPwdHandler returns a handler whose CLI reports its working directory,
// confined to root.
func newPwdHandler(t *testing.T, root string) a2asrv.RequestHandler {
	t.Helper()
	ln := newLauncher(t, scriptPath(t, "fake-pwd.sh"), "exec", nil, nil)
	exec := bridge.NewCLIExecutor(ln, session.New(), agent.NewLiveSessions(),
		workspace.NewResolver(root, false), config.LimitsConfig{})
	return a2asrv.NewHandler(exec)
}

func message(text string) *a2a.Message {
	return a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))
}

func artifactText(task *a2a.Task) string {
	var b strings.Builder
	for _, a := range task.Artifacts {
		for _, p := range a.Parts {
			b.WriteString(p.Text())
		}
	}
	return b.String()
}

// interactionFromMessage extracts the structured interaction from the DataPart
// of an input-required message.
func interactionFromMessage(t *testing.T, m *a2a.Message) agent.Interaction {
	t.Helper()
	if m == nil {
		t.Fatal("input-required message is nil")
	}
	for _, p := range m.Parts {
		if p.Data() == nil {
			continue
		}
		b, err := json.Marshal(p.Data())
		if err != nil {
			t.Fatalf("marshal interaction: %v", err)
		}
		var itx agent.Interaction
		if err := json.Unmarshal(b, &itx); err != nil {
			t.Fatalf("unmarshal interaction: %v", err)
		}
		return itx
	}
	t.Fatal("no DataPart interaction in message")
	return agent.Interaction{}
}

func TestSendMessage_codebuddy(t *testing.T) {
	h := newHandler(t, newLauncher(t, scriptPath(t, "fake-codebuddy.sh"), "stream-json", nil, nil))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := h.SendMessage(ctx, &a2a.SendMessageRequest{Message: message("hi")})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	task, ok := resp.(*a2a.Task)
	if !ok {
		t.Fatalf("result type %T", resp)
	}
	if task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("state = %s", task.Status.State)
	}
	if got := artifactText(task); !strings.Contains(got, "Hello world") {
		t.Fatalf("artifact text = %q", got)
	}
}

func TestStreaming_codebuddy(t *testing.T) {
	h := newHandler(t, newLauncher(t, scriptPath(t, "fake-codebuddy.sh"), "stream-json", nil, nil))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	updates := 0
	for ev, err := range h.SendStreamingMessage(ctx, &a2a.SendMessageRequest{Message: message("hi")}) {
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		if _, ok := ev.(*a2a.TaskArtifactUpdateEvent); ok {
			updates++
		}
	}
	if updates < 1 {
		t.Fatalf("expected >=1 artifact update, got %d", updates)
	}
}

// newPausedACPTask starts an ACP task whose fake CLI requests permission, so the
// task pauses in input-required. It returns the handler, the request context,
// its cancel, and the paused task.
func newPausedACPTask(t *testing.T) (a2asrv.RequestHandler, context.Context, context.CancelFunc, *a2a.Task) {
	t.Helper()
	ln := newLauncher(t, scriptPath(t, "fake-acp.sh"), "acp", nil, nil)
	exec := bridge.NewCLIExecutor(ln, session.New(), agent.NewLiveSessions(), nil,
		config.LimitsConfig{Timeout: 10 * time.Second})
	h := a2asrv.NewHandler(exec, a2asrv.WithCapabilityChecks(&a2a.AgentCapabilities{Streaming: true}))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	resp, err := h.SendMessage(ctx, &a2a.SendMessageRequest{Message: message("go")})
	if err != nil {
		cancel()
		t.Fatalf("first SendMessage: %v", err)
	}
	task := resp.(*a2a.Task)
	if task.Status.State != a2a.TaskStateInputRequired {
		cancel()
		t.Fatalf("state = %s, want input-required", task.Status.State)
	}
	return h, ctx, cancel, task
}

// resumeTask answers a paused task and returns the resulting task.
func resumeTask(
	t *testing.T, h a2asrv.RequestHandler, ctx context.Context,
	paused *a2a.Task, answer *a2a.Message,
) *a2a.Task {
	t.Helper()
	answer.TaskID = paused.ID
	answer.ContextID = paused.ContextID
	resp, err := h.SendMessage(ctx, &a2a.SendMessageRequest{Message: answer})
	if err != nil {
		t.Fatalf("resume SendMessage: %v", err)
	}
	return resp.(*a2a.Task)
}

// TestInputRequiredResume verifies that a task paused on input-required keeps
// its CLI process alive across the completing RPC and resumes to completion.
func TestInputRequiredResume(t *testing.T) {
	h, ctx, cancel, task := newPausedACPTask(t)
	defer cancel()

	itx := interactionFromMessage(t, task.Status.Message)
	if itx.Kind != agent.InteractionPermission {
		t.Fatalf("interaction kind = %q, want permission", itx.Kind)
	}
	if itx.ID == "" || !itx.Resume {
		t.Fatalf("interaction = %+v, want an id and resume=true", itx)
	}
	if len(itx.Options) == 0 {
		t.Fatalf("interaction has no options: %+v", itx)
	}

	answer := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewDataPart(map[string]any{
		"kind": string(itx.Kind), "id": itx.ID, "answer": "allow_once",
	}))
	done := resumeTask(t, h, ctx, task, answer)
	if done.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("state = %s, want completed", done.Status.State)
	}
	if got := artifactText(done); !strings.Contains(got, "Hello world") {
		t.Fatalf("artifact text = %q, want it to contain %q", got, "Hello world")
	}
}

func TestInputRequiredAcceptsTextWithMetadataID(t *testing.T) {
	h, ctx, cancel, task := newPausedACPTask(t)
	defer cancel()

	itx := interactionFromMessage(t, task.Status.Message)
	answer := message("allow_once")
	answer.Metadata = map[string]any{"harbor.interactionId": itx.ID}

	if done := resumeTask(t, h, ctx, task, answer); done.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("state = %s, want completed", done.Status.State)
	}
}

func TestInputRequiredRejectsMismatchedID(t *testing.T) {
	h, ctx, cancel, task := newPausedACPTask(t)
	defer cancel()

	answer := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewDataPart(map[string]any{
		"id": "itx_stale", "answer": "allow_once",
	}))
	done := resumeTask(t, h, ctx, task, answer)
	if done.Status.State != a2a.TaskStateFailed {
		t.Fatalf("state = %s, want failed", done.Status.State)
	}
	if code := done.Status.Message.Metadata["harbor.errorCode"]; code != "internal" {
		t.Fatalf("errorCode = %v, want internal", code)
	}
}

func TestSessionResume_codebuddy(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "args.log")
	env := map[string]string{"HARBOR_ARG_LOG": logPath}

	h := newHandler(t, newLauncher(t, scriptPath(t, "fake-codebuddy.sh"), "stream-json", nil, env))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const ctxID = "ctx-resume-1"
	m1 := message("first")
	m1.ContextID = ctxID
	if _, err := h.SendMessage(ctx, &a2a.SendMessageRequest{Message: m1}); err != nil {
		t.Fatalf("first SendMessage: %v", err)
	}

	m2 := message("second")
	m2.ContextID = ctxID
	if _, err := h.SendMessage(ctx, &a2a.SendMessageRequest{Message: m2}); err != nil {
		t.Fatalf("second SendMessage: %v", err)
	}

	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read arg log: %v", err)
	}
	if !strings.Contains(string(b), "--resume sess-123") {
		t.Fatalf("expected --resume sess-123 in args, got:\n%s", string(b))
	}
}

func TestSendMessage_opencode(t *testing.T) {
	h := newHandler(t, newLauncher(t, scriptPath(t, "fake-opencode.sh"), "exec", []string{"run", "{{.Prompt}}"}, nil))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := h.SendMessage(ctx, &a2a.SendMessageRequest{Message: message("list files")})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	task := resp.(*a2a.Task)
	if task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("state = %s", task.Status.State)
	}
	if got := artifactText(task); !strings.Contains(got, "listing files") {
		t.Fatalf("artifact text = %q", got)
	}
}

func TestCancel(t *testing.T) {
	h := newHandler(t, newLauncher(t, scriptPath(t, "fake-slow.sh"), "stream-json", nil, nil))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	seq := h.SendStreamingMessage(ctx, &a2a.SendMessageRequest{Message: message("go")})
	taskCh := make(chan a2a.TaskID, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev, err := range seq {
			if err != nil {
				return
			}
			if tk, ok := ev.(*a2a.Task); ok {
				select {
				case taskCh <- tk.ID:
				default:
				}
			}
		}
	}()

	var taskID a2a.TaskID
	select {
	case taskID = <-taskCh:
	case <-time.After(5 * time.Second):
		t.Fatal("no task id received")
	}

	// Wait until the task is working (subprocess started) before canceling.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		got, err := h.GetTask(ctx, &a2a.GetTaskRequest{ID: taskID})
		if err == nil && got.Status.State == a2a.TaskStateWorking {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := h.CancelTask(ctx, &a2a.CancelTaskRequest{ID: taskID}); err != nil {
		t.Fatalf("CancelTask: %v", err)
	}

	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("stream did not finish after cancel")
	}

	got, err := h.GetTask(ctx, &a2a.GetTaskRequest{ID: taskID})
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status.State != a2a.TaskStateCanceled {
		t.Fatalf("state = %s", got.Status.State)
	}
}

func TestCardVersion(t *testing.T) {
	ln := newLauncher(t, scriptPath(t, "fake-codebuddy.sh"), "stream-json", nil, nil)
	info := ln.Probe(context.Background())
	card := bridge.BuildCard(ln, info, "http://example/agents/x/", false)
	if len(card.SupportedInterfaces) != 1 {
		t.Fatalf("interfaces = %d", len(card.SupportedInterfaces))
	}
	if card.SupportedInterfaces[0].ProtocolVersion != a2a.Version {
		t.Fatalf("protocol version = %s", card.SupportedInterfaces[0].ProtocolVersion)
	}
}

func TestCardDeclaresInteractionExtension(t *testing.T) {
	acpLn := newLauncher(t, scriptPath(t, "fake-acp.sh"), "acp", nil, nil)
	acpCard := bridge.BuildCard(acpLn, acpLn.Probe(context.Background()), "http://example/agents/a/", false)
	if !hasExtension(acpCard, bridge.InteractionExtensionURI) {
		t.Fatalf("interactive card missing interaction extension: %+v", acpCard.Capabilities.Extensions)
	}

	execLn := newLauncher(t, scriptPath(t, "fake-pwd.sh"), "exec", nil, nil)
	execCard := bridge.BuildCard(execLn, execLn.Probe(context.Background()), "http://example/agents/e/", false)
	if hasExtension(execCard, bridge.InteractionExtensionURI) {
		t.Fatalf("non-interactive card should not declare the interaction extension")
	}
}

func hasExtension(card *a2a.AgentCard, uri string) bool {
	for _, e := range card.Capabilities.Extensions {
		if e.URI == uri {
			return true
		}
	}
	return false
}

func TestModelPassthrough(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "args.log")
	h := newHandler(t, newLauncher(t, scriptPath(t, "fake-codebuddy.sh"), "stream-json", nil,
		map[string]string{"HARBOR_ARG_LOG": logPath}))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	m := message("hi")
	m.Metadata = map[string]any{"harbor.model": "gpt-x"}
	if _, err := h.SendMessage(ctx, &a2a.SendMessageRequest{Message: m}); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read arg log: %v", err)
	}
	if !strings.Contains(string(b), "--model gpt-x") {
		t.Fatalf("expected --model gpt-x in args, got:\n%s", string(b))
	}
}

func TestErrorCode(t *testing.T) {
	h := newHandler(t, newLauncher(t, scriptPath(t, "fake-fail.sh"), "stream-json", nil, nil))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := h.SendMessage(ctx, &a2a.SendMessageRequest{Message: message("hi")})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	task := resp.(*a2a.Task)
	if task.Status.State != a2a.TaskStateFailed {
		t.Fatalf("state = %s", task.Status.State)
	}
	if task.Status.Message == nil {
		t.Fatal("no status message")
	}
	code, _ := task.Status.Message.Metadata["harbor.errorCode"].(string)
	if code != "internal" {
		t.Fatalf("errorCode = %q", code)
	}
}

func TestSendMessage_codex(t *testing.T) {
	ln := newLauncherParser(t, scriptPath(t, "fake-codex.sh"), "exec", "codex", []string{"exec", "{{.Prompt}}"}, nil)
	h := newHandler(t, ln)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := h.SendMessage(ctx, &a2a.SendMessageRequest{Message: message("hi")})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	task := resp.(*a2a.Task)
	if task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("state = %s", task.Status.State)
	}
	if got := artifactText(task); !strings.Contains(got, "Hello from codex") {
		t.Fatalf("artifact text = %q", got)
	}
}

func TestSendMessage_genericRegex(t *testing.T) {
	ac := config.AgentConfig{
		ID:          "my",
		Binary:      scriptPath(t, "fake-mycoder.sh"),
		Integration: "exec",
		Parser:      "regex",
		Regex:       &config.RegexConfig{Text: `^OUT:\s*(.+)$`},
		Args:        []string{"{{.Prompt}}"},
	}
	ln, err := integration.New(ac, integration.Options{InheritEnv: true})
	if err != nil {
		t.Fatalf("integration.New: %v", err)
	}
	h := newHandler(t, ln)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := h.SendMessage(ctx, &a2a.SendMessageRequest{Message: message("hi")})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	task := resp.(*a2a.Task)
	if task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("state = %s", task.Status.State)
	}
	got := artifactText(task)
	if !strings.Contains(got, "hello from my-coder") || strings.Contains(got, "noise") {
		t.Fatalf("artifact text = %q", got)
	}
}

// newHandlerTimeout returns a handler with a total task timeout.
func newHandlerTimeout(t *testing.T, ln agent.Launcher, d time.Duration) a2asrv.RequestHandler {
	t.Helper()
	exec := bridge.NewCLIExecutor(ln, session.New(), agent.NewLiveSessions(), nil, config.LimitsConfig{Timeout: d})
	return a2asrv.NewHandler(exec)
}

func TestBreakerRejectsAfterFailures(t *testing.T) {
	ln := newLauncher(t, scriptPath(t, "fake-fail.sh"), "stream-json", nil, nil)
	limits := config.LimitsConfig{
		Breaker: &config.BreakerConfig{Threshold: 2, Cooldown: time.Hour, MaxCooldown: time.Hour},
	}
	exec := bridge.NewCLIExecutor(ln, session.New(), agent.NewLiveSessions(), nil, limits)
	h := a2asrv.NewHandler(exec)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for i := 0; i < 2; i++ {
		resp, err := h.SendMessage(ctx, &a2a.SendMessageRequest{Message: message("hi")})
		if err != nil {
			t.Fatalf("SendMessage: %v", err)
		}
		if task := resp.(*a2a.Task); task.Status.State != a2a.TaskStateFailed {
			t.Fatalf("run %d: state = %s", i, task.Status.State)
		}
	}

	resp, err := h.SendMessage(ctx, &a2a.SendMessageRequest{Message: message("hi")})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	task := resp.(*a2a.Task)
	if task.Status.State != a2a.TaskStateRejected {
		t.Fatalf("expected rejected, got %s", task.Status.State)
	}
	if task.Status.Message == nil || task.Status.Message.Metadata["harbor.errorCode"] != "agent_unavailable" {
		t.Fatalf("expected agent_unavailable: %+v", task.Status.Message)
	}
}

func TestTimeout(t *testing.T) {
	ln := newLauncher(t, scriptPath(t, "fake-slow.sh"), "stream-json", nil, nil)
	h := newHandlerTimeout(t, ln, 400*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := h.SendMessage(ctx, &a2a.SendMessageRequest{Message: message("hi")})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	task := resp.(*a2a.Task)
	if task.Status.State != a2a.TaskStateFailed {
		t.Fatalf("state = %s", task.Status.State)
	}
	if task.Status.Message == nil || task.Status.Message.Metadata["harbor.errorCode"] != "timeout" {
		t.Fatalf("expected timeout code: %+v", task.Status.Message)
	}
}

// gitRepo creates a git repository with one committed file.
func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("init")
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", ".")
	runGit("commit", "-m", "init")
	return dir
}

func TestChangesArtifact(t *testing.T) {
	repo := gitRepo(t)
	ln := newLauncher(t, scriptPath(t, "fake-mutate.sh"), "exec", nil, nil)
	exec := bridge.NewCLIExecutor(ln, session.New(), agent.NewLiveSessions(),
		workspace.NewResolver(repo, false), config.LimitsConfig{})
	h := a2asrv.NewHandler(exec)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	m := message("hi")
	m.Metadata = map[string]any{"harbor.workDir": "."}
	resp, err := h.SendMessage(ctx, &a2a.SendMessageRequest{Message: m})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	task := resp.(*a2a.Task)
	if task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("state = %s", task.Status.State)
	}

	var diff string
	for _, a := range task.Artifacts {
		if a.Name == "changes" {
			for _, p := range a.Parts {
				diff += p.Text()
			}
		}
	}
	if !strings.Contains(diff, "tracked.txt") || !strings.Contains(diff, "+appended line") {
		t.Fatalf("changes artifact missing diff:\n%s", diff)
	}
}

func TestTraceEvents(t *testing.T) {
	ln := newLauncher(t, scriptPath(t, "fake-trace.sh"), "stream-json", nil, nil)
	h := newHandler(t, ln)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	kinds := map[string]bool{}
	for ev, err := range h.SendStreamingMessage(ctx, &a2a.SendMessageRequest{Message: message("hi")}) {
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		su, ok := ev.(*a2a.TaskStatusUpdateEvent)
		if !ok || su.Status.Message == nil {
			continue
		}
		if k, ok := su.Status.Message.Metadata["harbor.kind"].(string); ok {
			kinds[k] = true
		}
	}
	for _, want := range []string{"thinking", "tool_call", "tool_result", "usage"} {
		if !kinds[want] {
			t.Fatalf("missing trace kind %q; got %v", want, kinds)
		}
	}
}

func TestPushNotification(t *testing.T) {
	ln := newLauncher(t, scriptPath(t, "fake-slow.sh"), "stream-json", nil, nil)
	limits := config.LimitsConfig{Timeout: 800 * time.Millisecond}
	exec := bridge.NewCLIExecutor(ln, session.New(), agent.NewLiveSessions(), nil, limits)
	h := a2asrv.NewHandler(exec,
		a2asrv.WithPushNotifications(push.NewInMemoryStore(),
			push.NewHTTPPushSender(&push.HTTPSenderConfig{AllowPrivateNetworks: true, Timeout: 5 * time.Second})))

	hit := make(chan struct{}, 1)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case hit <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer hook.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	seq := h.SendStreamingMessage(ctx, &a2a.SendMessageRequest{Message: message("hi")})
	taskCh := make(chan a2a.TaskID, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev, err := range seq {
			if err != nil {
				return
			}
			if tk, ok := ev.(*a2a.Task); ok {
				select {
				case taskCh <- tk.ID:
				default:
				}
			}
		}
	}()

	var taskID a2a.TaskID
	select {
	case taskID = <-taskCh:
	case <-time.After(3 * time.Second):
		t.Fatal("no task id received")
	}
	cfg := &a2a.PushConfig{TaskID: taskID, URL: hook.URL}
	if _, err := h.CreateTaskPushConfig(ctx, cfg); err != nil {
		t.Fatalf("CreateTaskPushConfig: %v", err)
	}

	select {
	case <-hit:
	case <-time.After(5 * time.Second):
		t.Fatal("no push notification delivered")
	}
	<-done
}

func TestCLIHomeIsolation(t *testing.T) {
	home := t.TempDir()
	ac := config.AgentConfig{
		ID:          "h",
		Binary:      scriptPath(t, "fake-home.sh"),
		Integration: "exec",
		Args:        []string{"{{.Prompt}}"},
	}
	ln, err := integration.New(ac, integration.Options{InheritEnv: true, CLIHome: home})
	if err != nil {
		t.Fatalf("integration.New: %v", err)
	}
	h := newHandler(t, ln)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := h.SendMessage(ctx, &a2a.SendMessageRequest{Message: message("hi")})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	want := "HOME=" + filepath.Join(home, "default")
	if got := artifactText(resp.(*a2a.Task)); !strings.Contains(got, want) {
		t.Fatalf("artifact %q does not contain %q", got, want)
	}
}

func TestWorkdirApplied(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	m := message("hi")
	m.Metadata = map[string]any{"harbor.workDir": "repo"}
	resp, err := newPwdHandler(t, root).SendMessage(ctx, &a2a.SendMessageRequest{Message: m})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	task := resp.(*a2a.Task)
	if task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("state = %s", task.Status.State)
	}
	want, _ := filepath.EvalSymlinks(repo)
	if got := artifactText(task); !strings.Contains(got, want) {
		t.Fatalf("artifact %q does not contain workdir %q", got, want)
	}
}

func TestWorkdirDenied(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	m := message("hi")
	m.Metadata = map[string]any{"harbor.workDir": "../outside"}
	resp, err := newPwdHandler(t, root).SendMessage(ctx, &a2a.SendMessageRequest{Message: m})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	task := resp.(*a2a.Task)
	if task.Status.State != a2a.TaskStateFailed {
		t.Fatalf("state = %s", task.Status.State)
	}
	if task.Status.Message == nil || task.Status.Message.Metadata["harbor.errorCode"] != "workdir_denied" {
		t.Fatalf("missing workdir_denied error code: %+v", task.Status.Message)
	}
}

func TestCardUsesDisplayName(t *testing.T) {
	ln := newLauncher(t, scriptPath(t, "fake-codebuddy.sh"), "stream-json", nil, nil)
	info := ln.Probe(context.Background())
	info.DisplayName = "CodeBuddy Pro"
	card := bridge.BuildCard(ln, info, "http://example/agents/cb/", false)
	if card.Name != "Harbor/CodeBuddy Pro" {
		t.Fatalf("card name = %q", card.Name)
	}
	if len(card.Skills) != 1 || card.Skills[0].ID != ln.Name() {
		t.Fatalf("skill id should stay the agent id: %+v", card.Skills)
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
		{"localhost no auth", "localhost:8080", "none", false, false},
		{"exposed no auth", "0.0.0.0:8080", "none", false, true},
		{"empty host no auth", ":8080", "none", false, true}, // empty host binds all interfaces
		{"exposed with bearer", "0.0.0.0:8080", "bearer", false, false},
		{"exposed but allow_insecure", "0.0.0.0:8080", "none", true, false},
	}
	for _, c := range cases {
		err := bridge.RequireAuthOnExposedBind(c.listen, c.mode, c.allowInsecure)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: RequireAuthOnExposedBind(%q,%q,%v) err=%v, wantErr=%v",
				c.name, c.listen, c.mode, c.allowInsecure, err, c.wantErr)
		}
	}
}

func TestDiscoveryAggregate(t *testing.T) {
	ln := newLauncher(t, scriptPath(t, "fake-codebuddy.sh"), "stream-json", nil, nil)
	card := bridge.BuildCard(ln, ln.Probe(context.Background()), "http://example/agents/x/", false)

	mux := http.NewServeMux()
	mux.Handle("/.well-known/agents.json", bridge.NewCardsHandler([]*a2a.AgentCard{card}))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/.well-known/agents.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var cards []json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&cards); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(cards) != 1 {
		t.Fatalf("cards = %d", len(cards))
	}
}

func TestAuthScope(t *testing.T) {
	ln := newLauncher(t, scriptPath(t, "fake-codebuddy.sh"), "stream-json", nil, nil)
	card := bridge.BuildCard(ln, ln.Probe(context.Background()), "http://example/agents/x/", true)

	exec := bridge.NewCLIExecutor(ln, session.New(), agent.NewLiveSessions(), nil, config.LimitsConfig{})
	h := a2asrv.NewHandler(exec)

	mux := http.NewServeMux()
	mux.Handle("/agents/x/", bridge.BearerMiddleware([]string{"secret"}, a2asrv.NewJSONRPCHandler(h)))
	mux.Handle("/agents/x/.well-known/agent-card.json", a2asrv.NewStaticAgentCardHandler(card))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Card is public (no token) -> 200.
	cardResp, err := http.Get(srv.URL + "/agents/x/.well-known/agent-card.json")
	if err != nil {
		t.Fatal(err)
	}
	defer cardResp.Body.Close()
	if cardResp.StatusCode != http.StatusOK {
		t.Fatalf("card status = %d", cardResp.StatusCode)
	}

	// RPC without token -> 401.
	body := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"message/send","params":{}}`)
	rpcResp, err := http.Post(srv.URL+"/agents/x/", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer rpcResp.Body.Close()
	if rpcResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("rpc status = %d", rpcResp.StatusCode)
	}
}
