package bridge_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/push"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"

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

// firstDataPart returns the first JSON object artifact part, or nil.
func firstDataPart(task *a2a.Task) map[string]any {
	for _, a := range task.Artifacts {
		for _, p := range a.Parts {
			if m, ok := p.Data().(map[string]any); ok {
				return m
			}
		}
	}
	return nil
}

// sendTask runs one prompt and returns the completed task and its elapsed time,
// failing on error or a non-completed state. Shared by the tagged real-CLI tests.
func sendTask(t *testing.T, h a2asrv.RequestHandler, m *a2a.Message) (*a2a.Task, time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	start := time.Now()
	resp, err := h.SendMessage(ctx, &a2a.SendMessageRequest{Message: m})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	task := resp.(*a2a.Task)
	elapsed := time.Since(start)
	t.Logf("state=%s elapsed=%s text=%q", task.Status.State, elapsed, artifactText(task))
	if task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("state = %s, want completed", task.Status.State)
	}
	return task, elapsed
}

// sendAndExpect runs one prompt and asserts the task completes quickly with want
// text (a fast completion proves the CLI result ends the task, not a timeout).
func sendAndExpect(t *testing.T, h a2asrv.RequestHandler, m *a2a.Message, want string) {
	t.Helper()
	task, elapsed := sendTask(t, h, m)
	if text := artifactText(task); !strings.Contains(strings.ToLower(text), want) {
		t.Errorf("expected %q, got %q", want, text)
	}
	if elapsed > 40*time.Second {
		t.Errorf("took %s; should complete on the CLI result, not the timeout", elapsed)
	}
}

// cancelRunningTask starts a task, waits for the CLI to actually run, cancels it
// through the A2A CancelTask path, and asserts it ends canceled. It skips if the
// task completes before the cancel. Shared by the tagged real-CLI tests.
func cancelRunningTask(t *testing.T, h a2asrv.RequestHandler, prompt string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	seq := h.SendStreamingMessage(ctx, &a2a.SendMessageRequest{Message: message(prompt)})
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
	case <-time.After(15 * time.Second):
		t.Fatal("no task id received")
	}
	// A task is marked working at submission, before the CLI runs; give the CLI
	// time to start before canceling.
	time.Sleep(10 * time.Second)
	if got, err := h.GetTask(ctx, &a2a.GetTaskRequest{ID: taskID}); err == nil &&
		got.Status.State == a2a.TaskStateCompleted {
		t.Skip("task completed before it could be canceled")
	}
	if _, err := h.CancelTask(ctx, &a2a.CancelTaskRequest{ID: taskID}); err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("stream did not finish after cancel")
	}
	got, err := h.GetTask(ctx, &a2a.GetTaskRequest{ID: taskID})
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status.State != a2a.TaskStateCanceled {
		t.Fatalf("state = %s, want canceled", got.Status.State)
	}
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

// newStreamJSONHandler builds a stream-json handler over the given fake CLI.
func newStreamJSONHandler(t *testing.T, script string) (a2asrv.RequestHandler, context.Context, context.CancelFunc) {
	t.Helper()
	ln := newLauncher(t, scriptPath(t, script), "stream-json", []string{"-p"}, nil)
	exec := bridge.NewCLIExecutor(ln, session.New(), agent.NewLiveSessions(), nil,
		config.LimitsConfig{Timeout: 10 * time.Second})
	h := a2asrv.NewHandler(exec, a2asrv.WithCapabilityChecks(&a2a.AgentCapabilities{Streaming: true}))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	return h, ctx, cancel
}

// sendPaused sends a prompt and returns the task once it is input-required.
func sendPaused(t *testing.T, h a2asrv.RequestHandler, ctx context.Context, prompt string) *a2a.Task {
	t.Helper()
	resp, err := h.SendMessage(ctx, &a2a.SendMessageRequest{Message: message(prompt)})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	task := resp.(*a2a.Task)
	if task.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("state = %s, want input-required", task.Status.State)
	}
	return task
}

// answerEvent resumes a paused task with a structured {id, answer} DataPart.
func answerEvent(
	t *testing.T, h a2asrv.RequestHandler, ctx context.Context, task *a2a.Task, itx agent.Interaction, answer string,
) *a2a.Task {
	t.Helper()
	m := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewDataPart(map[string]any{"id": itx.ID, "answer": answer}))
	return resumeTask(t, h, ctx, task, m)
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

// TestStreamJSONPermissionResume verifies a stream-json control_request
// (can_use_tool) surfaces as input-required and the answer is written back as a
// control_response, letting the CLI finish.
func TestStreamJSONPermissionResume(t *testing.T) {
	h, ctx, cancel := newStreamJSONHandler(t, "fake-permission.sh")
	defer cancel()

	task := sendPaused(t, h, ctx, "run echo")
	itx := interactionFromMessage(t, task.Status.Message)
	if itx.Kind != agent.InteractionPermission || itx.ID == "" {
		t.Fatalf("interaction = %+v, want a permission with an id", itx)
	}
	if itx.Action != "Bash" || len(itx.Options) == 0 {
		t.Fatalf("interaction = %+v, want action Bash and options", itx)
	}

	done := answerEvent(t, h, ctx, task, itx, "allow")
	if done.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("state = %s, want completed", done.Status.State)
	}
	if got := artifactText(done); !strings.Contains(got, "allowed") {
		t.Errorf("artifact text = %q, want it to contain allowed", got)
	}
}

// TestStreamJSONQuestionResume verifies an AskUserQuestion can_use_tool surfaces
// as a question interaction and the chosen answer is written back through the
// control_response's updatedInput.
func TestStreamJSONQuestionResume(t *testing.T) {
	h, ctx, cancel := newStreamJSONHandler(t, "fake-question.sh")
	defer cancel()

	task := sendPaused(t, h, ctx, "ask me")
	itx := interactionFromMessage(t, task.Status.Message)
	if itx.Kind != agent.InteractionQuestion || itx.Target != "Favorite color?" {
		t.Fatalf("interaction = %+v, want a question targeted at the prompt", itx)
	}

	done := answerEvent(t, h, ctx, task, itx, "Blue")
	if done.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("state = %s, want completed", done.Status.State)
	}
	if got := artifactText(done); !strings.Contains(got, "chose Blue") {
		t.Errorf("artifact text = %q, want it to contain chose Blue", got)
	}
}

// TestStreamJSONQuestionMultiAnswer verifies a multi-question AskUserQuestion
// surfaces every question and a content-keyed answer writes all answers back.
func TestStreamJSONQuestionMultiAnswer(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "qa.log")
	t.Setenv("FAKE_QA_LOG", logPath)
	h, ctx, cancel := newStreamJSONHandler(t, "fake-question-multi.sh")
	defer cancel()

	task := sendPaused(t, h, ctx, "ask me")
	itx := interactionFromMessage(t, task.Status.Message)
	if len(itx.Questions) != 2 || itx.Questions[1].Question != "Favorite animal?" {
		t.Fatalf("questions = %+v, want two with the second being Favorite animal?", itx.Questions)
	}

	m := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewDataPart(map[string]any{
		"id": itx.ID,
		"content": map[string]any{
			"Favorite color?":  "Red",
			"Favorite animal?": "Dog",
		},
	}))
	done := resumeTask(t, h, ctx, task, m)
	if done.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("state = %s, want completed", done.Status.State)
	}
	rec, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read control_response log: %v", err)
	}
	for _, want := range []string{`"Favorite color?":"Red"`, `"Favorite animal?":"Dog"`} {
		if !strings.Contains(string(rec), want) {
			t.Errorf("control_response missing %q; got %s", want, rec)
		}
	}
}

// TestStreamJSONElicitationForm verifies an elicitation_create form surfaces as
// a question interaction and the client's content is written back as an accept
// control_response.
func TestStreamJSONElicitationForm(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "stdin.log")
	ln := newLauncher(t, scriptPath(t, "fake-form.sh"), "stream-json", []string{"-p"},
		map[string]string{"HARBOR_STDIN_LOG": logPath})
	exec := bridge.NewCLIExecutor(ln, session.New(), agent.NewLiveSessions(), nil,
		config.LimitsConfig{Timeout: 10 * time.Second})
	h := a2asrv.NewHandler(exec, a2asrv.WithCapabilityChecks(&a2a.AgentCapabilities{Streaming: true}))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	task := sendPaused(t, h, ctx, "ask me")
	itx := interactionFromMessage(t, task.Status.Message)
	if itx.Kind != agent.InteractionQuestion || itx.Action != "form" || itx.Schema == nil {
		t.Fatalf("interaction = %+v, want a form question with a schema", itx)
	}

	answer := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewDataPart(map[string]any{
		"id": itx.ID, "content": map[string]any{"color": "blue"},
	}))
	done := resumeTask(t, h, ctx, task, answer)
	if done.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("state = %s, want completed", done.Status.State)
	}
	sent, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read stdin log: %v", err)
	}
	if !strings.Contains(string(sent), `"action":"accept"`) || !strings.Contains(string(sent), `"color":"blue"`) {
		t.Fatalf("control_response = %q, want an accept with the form content", sent)
	}
}

// TestStreamJSONPlanApproval verifies an ExitPlanMode can_use_tool surfaces as a
// plan-approval interaction answered like a permission.
func TestStreamJSONPlanApproval(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "stdin.log")
	ln := newLauncher(t, scriptPath(t, "fake-plan.sh"), "stream-json", []string{"-p"},
		map[string]string{"HARBOR_STDIN_LOG": logPath})
	exec := bridge.NewCLIExecutor(ln, session.New(), agent.NewLiveSessions(), nil,
		config.LimitsConfig{Timeout: 10 * time.Second})
	h := a2asrv.NewHandler(exec, a2asrv.WithCapabilityChecks(&a2a.AgentCapabilities{Streaming: true}))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	task := sendPaused(t, h, ctx, "plan this")
	itx := interactionFromMessage(t, task.Status.Message)
	if itx.Kind != agent.InteractionPlanApproval {
		t.Fatalf("interaction = %+v, want a plan approval", itx)
	}

	done := answerEvent(t, h, ctx, task, itx, "allow")
	if done.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("state = %s, want completed", done.Status.State)
	}
	sent, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read stdin log: %v", err)
	}
	if !strings.Contains(string(sent), `"allowed":true`) {
		t.Fatalf("control_response = %q, want an allow", sent)
	}
}

// TestStreamJSONStructuredOutput verifies a harbor.outputSchema request produces
// a schema-validated JSON DataPart artifact.
func TestStreamJSONStructuredOutput(t *testing.T) {
	h, _, cancel := newStreamJSONHandler(t, "fake-structured.sh")
	defer cancel()

	m := message("capital of France")
	m.Metadata = map[string]any{"harbor.outputSchema": map[string]any{"type": "object"}}
	task, _ := sendTask(t, h, m)

	data := firstDataPart(task)
	if data == nil || data["city"] != "Paris" {
		t.Fatalf("structured artifact = %v, want city Paris", data)
	}
}

// TestStreamJSONImageInput verifies a raw image part reaches the CLI as a native
// image content block on stdin (stream-json), not a file path.
// stdinLoggerHandler builds a stream-json handler over a fake "codebuddy" that
// appends its first stdin line (the initial user message) to logPath. Naming the
// fake "codebuddy" gives it the profile's input capabilities.
func stdinLoggerHandler(t *testing.T, logPath string) a2asrv.RequestHandler {
	t.Helper()
	script := "#!/bin/sh\nread -r line\n" +
		`echo "$line" >> "$HARBOR_STDIN_LOG"` + "\n" +
		`echo '{"type":"result","result":"ok","is_error":false}'` + "\n"
	ln, err := integration.New(config.AgentConfig{
		ID: "cb", Binary: fakeBin(t, "codebuddy", script), Integration: config.IntegrationStreamJSON,
		Args: []string{"-p"}, Env: map[string]string{"HARBOR_STDIN_LOG": logPath},
	}, integration.Options{InheritEnv: true})
	if err != nil {
		t.Fatalf("integration.New: %v", err)
	}
	exec := bridge.NewCLIExecutor(ln, session.New(), agent.NewLiveSessions(), nil,
		config.LimitsConfig{Timeout: 30 * time.Second})
	return a2asrv.NewHandler(exec, a2asrv.WithCapabilityChecks(&a2a.AgentCapabilities{Streaming: true}))
}

// streamJSONContent decodes the initial user message's content array from the
// logged stdin line.
func streamJSONContent(t *testing.T, logPath string) []map[string]any {
	t.Helper()
	sent, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read stdin log: %v", err)
	}
	var msg struct {
		Message struct {
			Content []map[string]any `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(sent))), &msg); err != nil {
		t.Fatalf("unmarshal stdin %q: %v", sent, err)
	}
	return msg.Message.Content
}

func TestStreamJSONImageInput(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "stdin.log")
	h := stdinLoggerHandler(t, logPath)

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	img := a2a.NewRawPart([]byte("PNGBYTES"))
	img.MediaType = "image/png"
	img.Filename = "x.png"
	m := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("describe this"), img)
	if _, err := h.SendMessage(ctx, &a2a.SendMessageRequest{Message: m}); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	content := streamJSONContent(t, logPath)
	if len(content) != 2 || content[1]["type"] != "image" {
		t.Fatalf("content = %v, want a text block then an image block", content)
	}
	if src, _ := content[1]["source"].(map[string]any); src["media_type"] != "image/png" {
		t.Fatalf("image source = %v", content[1]["source"])
	}
}

// TestStreamJSONDataBlocks verifies text and data parts travel as separate
// content blocks (a DataPart is its own JSON block), not one concatenated text.
func TestStreamJSONDataBlocks(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "stdin.log")
	h := stdinLoggerHandler(t, logPath)

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	m := a2a.NewMessage(a2a.MessageRoleUser,
		a2a.NewTextPart("first"), a2a.NewDataPart(map[string]any{"k": "v"}), a2a.NewTextPart("second"))
	if _, err := h.SendMessage(ctx, &a2a.SendMessageRequest{Message: m}); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	content := streamJSONContent(t, logPath)
	if len(content) != 3 {
		t.Fatalf("content = %v, want three blocks", content)
	}
	if content[0]["text"] != "first" || content[2]["text"] != "second" {
		t.Fatalf("text blocks = %v / %v", content[0], content[2])
	}
	if content[1]["text"] != `{"k":"v"}` {
		t.Fatalf("data block = %v, want its own JSON block", content[1])
	}
}

// TestExecStructuredOutput verifies an exec agent configured with
// output_schema_args receives the schema flag and its JSON-only final text is
// surfaced as a DataPart artifact.
func TestExecStructuredOutput(t *testing.T) {
	root := t.TempDir()
	logPath := filepath.Join(root, "args.log")
	script := "#!/bin/sh\n" +
		`echo "$@" >> "$HARBOR_ARG_LOG"` + "\n" +
		`printf '%s' '{"city":"Paris"}'` + "\n"
	ac := config.AgentConfig{
		ID: "mc", Binary: fakeBin(t, "mycoder", script),
		Env:              map[string]string{"HARBOR_ARG_LOG": logPath},
		OutputSchemaArgs: []string{"--output-schema", "{{.SchemaFile}}"},
	}
	ln := newExecLauncher(t, ac, integration.Options{InheritEnv: true})
	h := newHandler(t, ln)

	m := message("capital of France")
	m.Metadata = map[string]any{"harbor.outputSchema": map[string]any{"type": "object"}}
	task, _ := sendTask(t, h, m)

	if data := firstDataPart(task); data == nil || data["city"] != "Paris" {
		t.Fatalf("structured artifact = %v, want city Paris", data)
	}
	args, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read arg log: %v", err)
	}
	if !strings.Contains(string(args), "--output-schema") {
		t.Fatalf("args = %q, want --output-schema", args)
	}
}

// TestExecFileAttachment verifies a raw file part is attached via the CLI's
// configured file args (its path on the command line) rather than only named in
// the prompt.
func TestExecFileAttachment(t *testing.T) {
	root := t.TempDir()
	logPath := filepath.Join(root, "args.log")
	script := "#!/bin/sh\n" +
		`echo "$@" >> "$HARBOR_ARG_LOG"` + "\n" +
		`echo ok` + "\n"
	ac := config.AgentConfig{
		ID: "mc", Binary: fakeBin(t, "mycoder", script),
		Env:      map[string]string{"HARBOR_ARG_LOG": logPath},
		FileArgs: []string{"-f", "{{.File}}"},
	}
	ln := newExecLauncher(t, ac, integration.Options{InheritEnv: true})
	h := newHandler(t, ln)

	file := a2a.NewRawPart([]byte("attachment body"))
	file.Filename = "notes.txt"
	file.MediaType = "text/plain"
	m := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("summarize"), file)
	task, _ := sendTask(t, h, m)
	if task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("state = %s", task.Status.State)
	}

	args, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read arg log: %v", err)
	}
	if !strings.Contains(string(args), "-f ") || !strings.Contains(string(args), ".harbor/uploads") {
		t.Fatalf("args = %q, want -f <uploaded file>", args)
	}
	if strings.Contains(string(args), "saved to") {
		t.Errorf("an attached file should not also be named with a path line: %q", args)
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

// userCtx returns a request context carrying an authenticated caller identity.
func userCtx(name string) context.Context {
	ctx, cc := a2asrv.NewCallContext(context.Background(), nil)
	cc.User = a2asrv.NewAuthenticatedUser(name, nil)
	return ctx
}

// TestSessionScopedByUser verifies that a CLI session bound to one caller's
// context cannot be resumed by another caller reusing the same contextId.
func TestSessionScopedByUser(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "args.log")
	env := map[string]string{"HARBOR_ARG_LOG": logPath}
	h := newHandler(t, newLauncher(t, scriptPath(t, "fake-codebuddy.sh"), "stream-json", nil, env))

	send := func(ctx context.Context, ctxID string) {
		t.Helper()
		m := message("hi")
		m.ContextID = ctxID
		if _, err := h.SendMessage(ctx, &a2a.SendMessageRequest{Message: m}); err != nil {
			t.Fatalf("SendMessage: %v", err)
		}
	}
	readLog := func() string {
		t.Helper()
		b, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatalf("read arg log: %v", err)
		}
		return string(b)
	}

	alice, bob := userCtx("alice"), userCtx("bob")
	send(alice, "shared-ctx") // alice's first turn: no session yet
	send(bob, "shared-ctx")   // bob reuses the id: must not resume alice's session
	if got := readLog(); strings.Contains(got, "--resume") {
		t.Fatalf("a different caller resumed the session:\n%s", got)
	}
	send(alice, "shared-ctx") // alice resumes her own session
	if got := readLog(); !strings.Contains(got, "--resume sess-123") {
		t.Fatalf("alice's second turn did not resume her session:\n%s", got)
	}
}

// TestServerModeEndToEnd drives the opencode "server" integration through the
// A2A handler against a fake HTTP server, checking session reuse by context.
func TestServerModeEndToEnd(t *testing.T) {
	var creates, prompts int32
	mux := http.NewServeMux()
	mux.HandleFunc("/session", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&creates, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "ses_srv"})
	})
	mux.HandleFunc("/session/ses_srv/message", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&prompts, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"parts": []map[string]any{{"type": "text", "text": "from server"}},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ln, err := integration.New(config.AgentConfig{
		ID: "oc", Binary: fakeBin(t, "opencode", "#!/bin/sh\n"),
		Integration: config.IntegrationServer, ServerURL: srv.URL,
	}, integration.Options{Ctx: context.Background()})
	if err != nil {
		t.Fatalf("integration.New: %v", err)
	}
	h := newHandler(t, ln)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	m1 := message("hi")
	m1.ContextID = "srv-ctx"
	resp, err := h.SendMessage(ctx, &a2a.SendMessageRequest{Message: m1})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if got := artifactText(resp.(*a2a.Task)); !strings.Contains(got, "from server") {
		t.Fatalf("artifact = %q", got)
	}

	m2 := message("again")
	m2.ContextID = "srv-ctx"
	if _, err := h.SendMessage(ctx, &a2a.SendMessageRequest{Message: m2}); err != nil {
		t.Fatalf("second SendMessage: %v", err)
	}
	if creates != 1 || prompts != 2 {
		t.Fatalf("creates=%d prompts=%d, want 1 and 2", creates, prompts)
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

func TestIdleTimeout(t *testing.T) {
	ln := newLauncher(t, scriptPath(t, "fake-slow.sh"), "stream-json", nil, nil)
	exec := bridge.NewCLIExecutor(ln, session.New(), agent.NewLiveSessions(), nil,
		config.LimitsConfig{IdleTimeout: 200 * time.Millisecond, Timeout: 20 * time.Second})
	h := a2asrv.NewHandler(exec, a2asrv.WithCapabilityChecks(&a2a.AgentCapabilities{Streaming: true}))

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	resp, err := h.SendMessage(ctx, &a2a.SendMessageRequest{Message: message("go")})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	task := resp.(*a2a.Task)
	if task.Status.State != a2a.TaskStateFailed {
		t.Fatalf("state = %s, want failed", task.Status.State)
	}
	if code := task.Status.Message.Metadata["harbor.errorCode"]; code != "timeout" {
		t.Fatalf("errorCode = %v, want timeout", code)
	}
}

func TestCancel(t *testing.T) {
	exec := bridge.NewCLIExecutor(
		newLauncher(t, scriptPath(t, "fake-slow.sh"), "stream-json", nil, nil),
		session.New(), agent.NewLiveSessions(), nil, config.LimitsConfig{})
	h := a2asrv.NewHandler(exec, a2asrv.WithCapabilityChecks(&a2a.AgentCapabilities{Streaming: true}))
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
	if snap := exec.Snapshot(); snap.Canceled != 1 || snap.Active != 0 {
		t.Fatalf("snapshot = %+v, want 1 canceled and 0 active", snap)
	}
}

// runMetricsTask runs one prompt against a stream-json fake CLI and returns the
// executor so its metrics snapshot can be asserted.
func runMetricsTask(t *testing.T, script string) *bridge.CLIExecutor {
	t.Helper()
	exec := bridge.NewCLIExecutor(
		newLauncher(t, scriptPath(t, script), "stream-json", nil, nil),
		session.New(), agent.NewLiveSessions(), nil, config.LimitsConfig{})
	h := a2asrv.NewHandler(exec, a2asrv.WithCapabilityChecks(&a2a.AgentCapabilities{Streaming: true}))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := h.SendMessage(ctx, &a2a.SendMessageRequest{Message: message("hi")}); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	return exec
}

func TestExecutorMetricsCompleted(t *testing.T) {
	exec := runMetricsTask(t, "fake-codebuddy.sh")
	snap := exec.Snapshot()
	if snap.Submitted != 1 || snap.Completed != 1 || snap.Active != 0 {
		t.Fatalf("snapshot = %+v, want 1 submitted, 1 completed, 0 active", snap)
	}
	if !exec.Ready() || snap.BreakerState != "closed" {
		t.Fatalf("ready=%v breaker=%q, want ready and closed", exec.Ready(), snap.BreakerState)
	}
	if snap.DurationCount != 1 || snap.DurationSumSeconds < 0 {
		t.Fatalf("duration = %+v, want one observation", snap)
	}
}

// TestExecutorMetricsUsage verifies token/cost totals and the Prometheus
// duration histogram/usage counters are recorded and rendered.
func TestExecutorMetricsUsage(t *testing.T) {
	snap := runMetricsTask(t, "fake-usage.sh").Snapshot()
	if snap.TokensIn != 11 || snap.TokensOut != 22 || snap.CostUSD < 0.012 {
		t.Fatalf("usage = %+v, want 11/22 tokens and cost >= 0.012", snap)
	}

	var buf bytes.Buffer
	bridge.WritePrometheus(&buf, []bridge.MetricsSnapshot{snap})
	out := buf.String()
	for _, want := range []string{
		`harbor_task_duration_seconds_bucket{agent="stream-json",le="+Inf"} 1`,
		`harbor_task_duration_seconds_count{agent="stream-json"} 1`,
		`harbor_task_tokens_total{agent="stream-json",direction="input"} 11`,
		`harbor_task_tokens_total{agent="stream-json",direction="output"} 22`,
		`harbor_task_cost_usd_total{agent="stream-json"}`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics missing %q\n%s", want, out)
		}
	}
}

func TestPromptFromParts(t *testing.T) {
	root := t.TempDir()
	logPath := filepath.Join(root, "args.log")
	ln := newLauncher(t, scriptPath(t, "fake-codebuddy.sh"), "exec",
		[]string{"-p", "{{.Prompt}}"}, map[string]string{"HARBOR_ARG_LOG": logPath})
	exec := bridge.NewCLIExecutor(ln, session.New(), agent.NewLiveSessions(),
		workspace.NewResolver(root, false), config.LimitsConfig{})
	h := a2asrv.NewHandler(exec, a2asrv.WithCapabilityChecks(&a2a.AgentCapabilities{Streaming: true}))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	file := a2a.NewRawPart([]byte("file body"))
	file.Filename = "notes.txt"
	file.MediaType = "text/plain"
	link := a2a.NewFileURLPart("https://example.test/spec.pdf", "application/pdf")
	link.Filename = "spec.pdf"

	m := a2a.NewMessage(a2a.MessageRoleUser,
		a2a.NewTextPart("first part"),
		a2a.NewTextPart("second part"),
		file,
		link,
	)
	m.Metadata = map[string]any{"harbor.workDir": "."}
	if _, err := h.SendMessage(ctx, &a2a.SendMessageRequest{Message: m}); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read arg log: %v", err)
	}
	got := string(log)
	for _, want := range []string{"first part", "second part", "notes.txt", "https://example.test/spec.pdf"} {
		if !strings.Contains(got, want) {
			t.Fatalf("prompt missing %q:\n%s", want, got)
		}
	}

	uploaded := filepath.Join(root, ".harbor", "uploads", "1-notes.txt")
	if b, err := os.ReadFile(uploaded); err != nil || string(b) != "file body" {
		t.Fatalf("uploaded file: err=%v body=%q", err, b)
	}
}

func TestRejectsUnsupportedImage(t *testing.T) {
	root := t.TempDir()
	logPath := filepath.Join(root, "args.log")
	ln := newLauncher(t, scriptPath(t, "fake-codebuddy.sh"), "exec",
		[]string{"-p", "{{.Prompt}}"}, map[string]string{"HARBOR_ARG_LOG": logPath})
	exec := bridge.NewCLIExecutor(ln, session.New(), agent.NewLiveSessions(),
		workspace.NewResolver(root, false), config.LimitsConfig{})
	h := a2asrv.NewHandler(exec, a2asrv.WithCapabilityChecks(&a2a.AgentCapabilities{Streaming: true}))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	img := a2a.NewRawPart([]byte("png"))
	img.Filename = "x.png"
	img.MediaType = "image/png"
	m := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("describe this"), img)
	m.Metadata = map[string]any{"harbor.workDir": "."}

	resp, err := h.SendMessage(ctx, &a2a.SendMessageRequest{Message: m})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	task := resp.(*a2a.Task)
	if task.Status.State != a2a.TaskStateFailed {
		t.Fatalf("state = %s, want failed", task.Status.State)
	}
	if code := task.Status.Message.Metadata["harbor.errorCode"]; code != "unsupported_content" {
		t.Fatalf("errorCode = %v, want unsupported_content", code)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("CLI should not have started; arg log present (err=%v)", err)
	}
}

// fakeBin writes an executable named name and returns its path.
func fakeBin(t *testing.T, name, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// newExecLauncher builds an exec launcher over an arbitrary binary.
func newExecLauncher(t *testing.T, ac config.AgentConfig, opts integration.Options) agent.Launcher {
	t.Helper()
	ac.Integration = "exec"
	ac.Parser = "text"
	ac.Args = []string{"-p", "{{.Prompt}}"}
	ln, err := integration.New(ac, opts)
	if err != nil {
		t.Fatalf("integration.New: %v", err)
	}
	return ln
}

func TestCardVersion(t *testing.T) {
	ln := newLauncher(t, scriptPath(t, "fake-codebuddy.sh"), "stream-json", nil, nil)
	info := ln.Probe(context.Background())
	card := bridge.BuildCard(ln, info, "http://example/agents/x/", false)
	if len(card.SupportedInterfaces) != 2 {
		t.Fatalf("interfaces = %d, want JSON-RPC and HTTP+JSON", len(card.SupportedInterfaces))
	}
	if card.SupportedInterfaces[0].ProtocolBinding != a2a.TransportProtocolJSONRPC ||
		card.SupportedInterfaces[0].ProtocolVersion != a2a.Version {
		t.Fatalf("first interface = %+v, want JSON-RPC at %s", card.SupportedInterfaces[0], a2a.Version)
	}
	if card.SupportedInterfaces[1].ProtocolBinding != a2a.TransportProtocolHTTPJSON {
		t.Fatalf("second interface = %+v, want HTTP+JSON", card.SupportedInterfaces[1])
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

// TestExecImageAttachment verifies an image part for an exec CLI with image_args
// is materialized to disk and passed on the command line (codex -i).
func TestExecImageAttachment(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "args.log")
	ln, err := integration.New(config.AgentConfig{
		ID: "codex", Binary: scriptPath(t, "fake-codex.sh"), Integration: config.IntegrationExec,
		Parser: "codex", Args: []string{"exec", "{{.Prompt}}"},
		ImageArgs:  []string{"-i", "{{.Image}}"},
		InputImage: []string{"image/png"},
		Env:        map[string]string{"HARBOR_ARG_LOG": logPath},
	}, integration.Options{InheritEnv: true})
	if err != nil {
		t.Fatalf("integration.New: %v", err)
	}
	exec := bridge.NewCLIExecutor(ln, session.New(), agent.NewLiveSessions(), nil, config.LimitsConfig{})
	h := a2asrv.NewHandler(exec)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	img := a2a.NewRawPart([]byte("PNGDATA"))
	img.MediaType = "image/png"
	img.Filename = "pic.png"
	m := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("describe this"), img)
	resp, err := h.SendMessage(ctx, &a2a.SendMessageRequest{Message: m})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if task := resp.(*a2a.Task); task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("state = %s, want completed", task.Status.State)
	}
	rec, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read arg log: %v", err)
	}
	args := string(rec)
	if !strings.Contains(args, "-i ") || !strings.Contains(args, "1-pic.png") {
		t.Fatalf("args missing the image attachment:\n%s", args)
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

// TestImageOutputArtifact verifies a task that writes an image file into its
// working directory surfaces it as a raw image artifact.
func TestImageOutputArtifact(t *testing.T) {
	root := t.TempDir()
	ln := newLauncher(t, scriptPath(t, "fake-image.sh"), "exec", []string{"{{.Prompt}}"}, nil)
	exec := bridge.NewCLIExecutor(ln, session.New(), agent.NewLiveSessions(),
		workspace.NewResolver(root, false), config.LimitsConfig{})
	h := a2asrv.NewHandler(exec)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	m := message("draw a picture")
	m.Metadata = map[string]any{"harbor.workDir": "."}
	resp, err := h.SendMessage(ctx, &a2a.SendMessageRequest{Message: m})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	task := resp.(*a2a.Task)
	if task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("state = %s, want completed", task.Status.State)
	}

	var found bool
	for _, a := range task.Artifacts {
		if a.Name != "image" {
			continue
		}
		for _, p := range a.Parts {
			if p.MediaType != "image/png" || p.Filename != "generated.png" {
				t.Fatalf("part = media %q file %q, want image/png generated.png", p.MediaType, p.Filename)
			}
			if len(p.Raw()) == 0 {
				t.Fatal("image artifact has no raw bytes")
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("no image artifact; artifacts = %+v", task.Artifacts)
	}
}

// TestMaxTurnsExceeded verifies a resume past the per-task turn limit fails.
func TestMaxTurnsExceeded(t *testing.T) {
	ln := newLauncher(t, scriptPath(t, "fake-question.sh"), "stream-json", []string{"-p"}, nil)
	exec := bridge.NewCLIExecutor(ln, session.New(), agent.NewLiveSessions(), nil,
		config.LimitsConfig{Timeout: 10 * time.Second, MaxTurns: 1})
	h := a2asrv.NewHandler(exec, a2asrv.WithCapabilityChecks(&a2a.AgentCapabilities{Streaming: true}))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	task := sendPaused(t, h, ctx, "ask me")
	itx := interactionFromMessage(t, task.Status.Message)
	done := answerEvent(t, h, ctx, task, itx, "Blue")
	if done.Status.State != a2a.TaskStateFailed {
		t.Fatalf("state = %s, want failed", done.Status.State)
	}
	if code, _ := done.Status.Message.Metadata["harbor.errorCode"].(string); code != "max_turns" {
		t.Fatalf("errorCode = %q, want max_turns", code)
	}
}

// TestFileArtifacts verifies the opt-in per-file artifact for a changed file.
func TestFileArtifacts(t *testing.T) {
	repo := gitRepo(t)
	ln := newLauncher(t, scriptPath(t, "fake-mutate.sh"), "exec", nil, nil)
	exec := bridge.NewCLIExecutor(ln, session.New(), agent.NewLiveSessions(),
		workspace.NewResolver(repo, false), config.LimitsConfig{})
	exec.SetFileArtifacts(true)
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
	var found bool
	for _, a := range task.Artifacts {
		if a.Name != "file" {
			continue
		}
		for _, p := range a.Parts {
			if p.Filename == "tracked.txt" && strings.Contains(string(p.Raw()), "appended line") {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("no per-file artifact for tracked.txt; artifacts = %+v", task.Artifacts)
	}
}

// TestDuplicateMessageIdempotent verifies a repeated send (same messageId) does
// not start a second execution and returns the original task.
func TestDuplicateMessageIdempotent(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "args.log")
	ln := newLauncher(t, scriptPath(t, "fake-codebuddy.sh"), "stream-json", nil,
		map[string]string{"HARBOR_ARG_LOG": logPath})
	exec := bridge.NewCLIExecutor(ln, session.New(), agent.NewLiveSessions(), nil,
		config.LimitsConfig{Timeout: 10 * time.Second})
	ts := taskstore.NewInMemory(nil)
	exec.SetTaskLookup(func(ctx context.Context, id a2a.TaskID) (*a2a.Task, bool) {
		st, err := ts.Get(ctx, id)
		if err != nil || st == nil {
			return nil, false
		}
		return st.Task, true
	})
	h := a2asrv.NewHandler(exec, a2asrv.WithTaskStore(ts),
		a2asrv.WithCapabilityChecks(&a2a.AgentCapabilities{Streaming: true}))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	send := func() *a2a.Task {
		m := message("hi")
		m.ID = "dup-id-1"
		resp, err := h.SendMessage(ctx, &a2a.SendMessageRequest{Message: m})
		if err != nil {
			t.Fatalf("SendMessage: %v", err)
		}
		return resp.(*a2a.Task)
	}
	first := send()
	second := send()
	if second.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("duplicate task state = %s, want completed", second.Status.State)
	}
	if artifactText(first) != artifactText(second) {
		t.Fatalf("duplicate result %q != original %q", artifactText(second), artifactText(first))
	}
	if got, _ := second.Metadata["harbor.duplicateOf"].(string); got != string(first.ID) {
		t.Fatalf("duplicateOf = %q, want %q", got, first.ID)
	}
	rec, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read arg log: %v", err)
	}
	if n := len(strings.Split(strings.TrimSpace(string(rec)), "\n")); n != 1 {
		t.Fatalf("CLI ran %d times, want 1", n)
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
	want := "HOME=" + home
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
