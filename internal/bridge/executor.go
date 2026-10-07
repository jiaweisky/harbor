package bridge

import (
	"context"
	"errors"
	"iter"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/jiaweisky/harbor/internal/agent"
	"github.com/jiaweisky/harbor/internal/workspace"
)

// error codes written to TaskStatus.Message.Metadata["harbor.errorCode"].
const (
	codeInternal           = "internal"
	codeCLINotFound        = "cli_not_found"
	codeUnsupportedContent = "unsupported_content"
	codeWorkdirDenied      = "workdir_denied"
)

// errNoText is returned when an incoming message has no textual part.
var errNoText = errors.New("no text part in message")

// SessionStore binds an A2A context to a CLI-native session id.
type SessionStore interface {
	Get(contextID string) (string, bool)
	Bind(contextID, sessionID string)
}

// CLIExecutor turns a coding-agent CLI session into A2A task events.
type CLIExecutor struct {
	launcher  agent.Launcher
	sessions  SessionStore
	live      *agent.LiveSessions
	workspace *workspace.Resolver
}

// NewCLIExecutor creates an executor over the given launcher.
func NewCLIExecutor(
	ln agent.Launcher, sessions SessionStore, live *agent.LiveSessions, ws *workspace.Resolver,
) *CLIExecutor {
	return &CLIExecutor{launcher: ln, sessions: sessions, live: live, workspace: ws}
}

// Execute implements a2asrv.AgentExecutor. A task paused on input-required is
// resumed; any other task starts a new CLI session.
func (e *CLIExecutor) Execute(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		taskID := string(ec.TaskID)
		if isWaitingForInput(ec) {
			e.resume(ctx, ec, taskID, yield)
			return
		}
		e.start(ctx, ec, taskID, yield)
	}
}

// start creates the task and begins a new CLI session.
func (e *CLIExecutor) start(
	ctx context.Context, ec *a2asrv.ExecutorContext, taskID string,
	yield func(a2a.Event, error) bool,
) {
	if ec.StoredTask == nil {
		if !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
			return
		}
	}
	if !yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateWorking, nil), nil) {
		return
	}

	req, err := e.toRequest(ec)
	if err != nil {
		yield(failedStatus(ec, codeForRequestError(err), err.Error()), nil)
		return
	}
	sess, err := e.launcher.Start(agent.WithTaskID(ctx, taskID), req)
	if err != nil {
		yield(failedStatus(ec, codeForStartError(err), err.Error()), nil)
		return
	}
	e.live.Put(taskID, sess)
	e.pump(ec, taskID, sess, yield)
}

// resume answers a task paused on input-required by writing to its live session.
func (e *CLIExecutor) resume(
	ctx context.Context, ec *a2asrv.ExecutorContext, taskID string,
	yield func(a2a.Event, error) bool,
) {
	sess, ok := e.live.Get(taskID)
	if !ok {
		yield(failedStatus(ec, codeInternal, "session lost"), nil)
		return
	}
	if err := sess.Send(ctx, inputFromMessage(ec.Message)); err != nil {
		e.live.Del(taskID)
		yield(failedStatus(ec, codeInternal, err.Error()), nil)
		return
	}
	if !yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateWorking, nil), nil) {
		return
	}
	e.pump(ec, taskID, sess, yield)
}

// pump consumes session events, coalescing text into artifact updates and
// keeping the task alive across an input-required pause.
func (e *CLIExecutor) pump(
	ec *a2asrv.ExecutorContext, taskID string, sess agent.Session,
	yield func(a2a.Event, error) bool,
) {
	emit := func(ev a2a.Event) bool {
		if yield(ev, nil) {
			return true
		}
		// The consumer went away: tear the CLI down so no process leaks.
		_ = sess.Cancel()
		e.live.Del(taskID)
		return false
	}
	sink := newTextSink(ec, emit)

	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()

	ch := sess.Events()
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				e.finish(ec, taskID, sess, sink, emit)
				return
			}
			if !e.handleEvent(ec, taskID, sess, sink, emit, ev) {
				return
			}
		case <-ticker.C:
			if !sink.Flush() {
				return
			}
		}
	}
}

// handleEvent processes one session event and reports whether the loop should
// continue. A false result means the pump may stop (yield stopped, or a
// terminal/input-required state was emitted).
func (e *CLIExecutor) handleEvent(
	ec *a2asrv.ExecutorContext, taskID string, sess agent.Session,
	sink *textSink, emit func(a2a.Event) bool, ev agent.Event,
) bool {
	switch ev.Kind {
	case agent.EventSession:
		e.sessions.Bind(ec.ContextID, ev.SessionID)
		return true
	case agent.EventTextDelta:
		return sink.Write(ev.Text)
	case agent.EventPermissionRequest:
		if !sink.Flush() {
			return false
		}
		// Keep the session alive for the client's answer.
		emit(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateInputRequired,
			a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(ev.Text))))
		return false
	case agent.EventError:
		sink.Flush()
		e.live.Del(taskID)
		if sess.Canceled() {
			return false
		}
		emit(failedStatus(ec, codeInternal, eventErrorText(ev)))
		return false
	}
	return true
}

// finish flushes pending text and emits the terminal state after the event
// channel closes (the CLI process has exited).
func (e *CLIExecutor) finish(
	ec *a2asrv.ExecutorContext, taskID string, sess agent.Session,
	sink *textSink, emit func(a2a.Event) bool,
) {
	if !sink.Flush() {
		return
	}
	e.live.Del(taskID)
	if sess.Canceled() {
		return // Cancel already emitted the canceled state.
	}
	emit(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, nil))
}

// Cancel implements a2asrv.AgentExecutor.
func (e *CLIExecutor) Cancel(_ context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		taskID := string(ec.TaskID)
		if sess, ok := e.live.Get(taskID); ok {
			_ = sess.Cancel()
			e.live.Del(taskID)
		}
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCanceled, nil), nil)
	}
}

// toRequest maps the A2A request onto an agent.Request, merging message-level
// and request-level metadata (request-level wins).
func (e *CLIExecutor) toRequest(ec *a2asrv.ExecutorContext) (*agent.Request, error) {
	prompt := firstTextPart(ec.Message)
	if prompt == "" {
		return nil, errNoText
	}
	meta := mergeMetadata(ec)
	dir, err := e.workspace.Resolve(ec.ContextID, metaString(meta, "harbor.workDir"))
	if err != nil {
		return nil, err
	}
	sid, _ := e.sessions.Get(ec.ContextID)
	return &agent.Request{
		Prompt:      prompt,
		SystemExtra: metaString(meta, "harbor.systemPrompt"),
		Model:       metaString(meta, "harbor.model"),
		Agent:       metaString(meta, "harbor.agent"),
		WorkDir:     dir,
		SessionID:   sid,
		Continue:    sid == "" && metaBool(meta, "harbor.continue"),
		MaxTurns:    metaInt(meta, "harbor.maxTurns"),
		Meta:        meta,
	}, nil
}

func isWaitingForInput(ec *a2asrv.ExecutorContext) bool {
	return ec.StoredTask != nil && ec.StoredTask.Status.State == a2a.TaskStateInputRequired
}

func codeForRequestError(err error) string {
	switch {
	case errors.Is(err, errNoText):
		return codeUnsupportedContent
	case errors.Is(err, workspace.ErrDenied):
		return codeWorkdirDenied
	default:
		return codeInternal
	}
}

func codeForStartError(err error) string {
	if errors.Is(err, agent.ErrCLINotFound) {
		return codeCLINotFound
	}
	return codeInternal
}

func mergeMetadata(ec *a2asrv.ExecutorContext) map[string]any {
	meta := map[string]any{}
	if ec.Message != nil {
		for k, v := range ec.Message.Metadata {
			meta[k] = v
		}
	}
	for k, v := range ec.Metadata {
		meta[k] = v
	}
	return meta
}

// failedStatus builds a failed status event carrying a machine-readable code.
func failedStatus(ec *a2asrv.ExecutorContext, code, msg string) *a2a.TaskStatusUpdateEvent {
	m := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(msg))
	if m.Metadata == nil {
		m.Metadata = map[string]any{}
	}
	m.Metadata["harbor.errorCode"] = code
	return a2a.NewStatusUpdateEvent(ec, a2a.TaskStateFailed, m)
}

func eventErrorText(ev agent.Event) string {
	if ev.Err != nil {
		return ev.Err.Error()
	}
	return "cli error"
}

func firstTextPart(m *a2a.Message) string {
	if m == nil {
		return ""
	}
	for _, p := range m.Parts {
		if p != nil {
			if t := p.Text(); t != "" {
				return t
			}
		}
	}
	return ""
}

func inputFromMessage(m *a2a.Message) agent.Input {
	return agent.Input{Kind: "answer", Answer: firstTextPart(m)}
}

func metaString(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

func metaBool(m map[string]any, key string) bool {
	v, _ := m[key].(bool)
	return v
}

func metaInt(m map[string]any, key string) int {
	switch v := m[key].(type) {
	case int:
		return v
	case float64:
		return int(v)
	}
	return 0
}
