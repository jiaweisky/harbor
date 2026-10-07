package bridge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/jiaweisky/harbor/internal/agent"
	"github.com/jiaweisky/harbor/internal/changes"
	"github.com/jiaweisky/harbor/internal/config"
	"github.com/jiaweisky/harbor/internal/workspace"
)

// error codes written to TaskStatus.Message.Metadata["harbor.errorCode"].
const (
	codeInternal           = "internal"
	codeCLINotFound        = "cli_not_found"
	codeUnsupportedContent = "unsupported_content"
	codeWorkdirDenied      = "workdir_denied"
	codeTimeout            = "timeout"
	codeAgentUnavailable   = "agent_unavailable"
)

// Metadata keys carried on task status messages. The interaction id is both
// written on an input-required prompt and read back from a client's answer.
const (
	metaKeyKind          = "harbor.kind"
	metaKeyErrorCode     = "harbor.errorCode"
	metaKeyInteractionID = "harbor.interactionId"
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
	launcher     agent.Launcher
	sessions     SessionStore
	live         *agent.LiveSessions
	workspace    *workspace.Resolver
	timeout      time.Duration // total task timeout; 0 disables
	breaker      *breaker      // nil disables
	baselines    sync.Map      // taskID -> changes.Baseline
	locks        sync.Map      // taskID -> func() (workdir lock release)
	runs         sync.Map      // taskID -> *taskRun (process lifetime, decoupled from the request)
	interactions sync.Map      // taskID -> *agent.Interaction awaiting an answer
}

// taskRun is the lifetime of one CLI process. Its context is detached from the
// RPC context so a task paused on input-required keeps the process alive after
// the request returns; it is canceled only on a terminal state or Cancel.
type taskRun struct {
	ctx    context.Context
	cancel context.CancelFunc
}

// NewCLIExecutor creates an executor over the given launcher. limits bounds the
// task duration and the failure circuit breaker.
func NewCLIExecutor(
	ln agent.Launcher, sessions SessionStore, live *agent.LiveSessions,
	ws *workspace.Resolver, limits config.LimitsConfig,
) *CLIExecutor {
	return &CLIExecutor{
		launcher:  ln,
		sessions:  sessions,
		live:      live,
		workspace: ws,
		timeout:   limits.Timeout,
		breaker:   newBreaker(limits.Breaker),
	}
}

// Execute implements a2asrv.AgentExecutor. A task paused on input-required is
// resumed; any other task starts a new CLI session.
func (e *CLIExecutor) Execute(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if !e.breaker.Allow() {
			if ec.StoredTask == nil && !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
				return
			}
			yield(rejectedStatus(ec, "agent is temporarily unavailable after repeated failures"), nil)
			return
		}
		taskID := string(ec.TaskID)
		if isWaitingForInput(ec) {
			e.resume(ec, taskID, yield)
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
		e.breaker.Resolve()
		yield(failedStatus(ec, codeForRequestError(err), err.Error()), nil)
		return
	}
	e.locks.Store(taskID, e.workspace.Lock(req.WorkDir))
	if req.WorkDir != "" {
		e.baselines.Store(taskID, changes.Snapshot(req.WorkDir))
	}
	runCtx := e.beginRun(ctx, taskID)
	sess, err := e.launcher.Start(agent.WithTaskID(runCtx, taskID), req)
	if err != nil {
		e.cleanup(taskID, true)
		yield(failedStatus(ec, codeForStartError(err), err.Error()), nil)
		return
	}
	e.live.Put(taskID, sess)
	e.pump(runCtx, ec, taskID, sess, yield)
}

// beginRun starts a process lifetime for a task and returns its context. The
// context is detached from parent (which is canceled when the RPC returns) so a
// paused task stays alive; it is bounded by the configured task timeout.
func (e *CLIExecutor) beginRun(parent context.Context, taskID string) context.Context {
	base := context.WithoutCancel(parent)
	runCtx, cancel := context.WithCancel(base)
	if e.timeout > 0 {
		runCtx, cancel = context.WithTimeout(base, e.timeout)
	}
	e.runs.Store(taskID, &taskRun{ctx: runCtx, cancel: cancel})
	return runCtx
}

// endRun cancels and forgets a task's process lifetime. It is idempotent.
func (e *CLIExecutor) endRun(taskID string) {
	if v, ok := e.runs.LoadAndDelete(taskID); ok {
		v.(*taskRun).cancel()
	}
}

// resume answers a task paused on input-required by writing to its live session.
func (e *CLIExecutor) resume(
	ec *a2asrv.ExecutorContext, taskID string,
	yield func(a2a.Event, error) bool,
) {
	v, ok := e.runs.Load(taskID)
	if !ok {
		yield(failedStatus(ec, codeInternal, "session lost"), nil)
		return
	}
	run := v.(*taskRun)
	if err := run.ctx.Err(); err != nil {
		// The process already ended (typically the task timed out while paused).
		e.cleanup(taskID, true)
		yield(failedStatus(ec, codeForRunError(err), err.Error()), nil)
		return
	}
	sess, ok := e.live.Get(taskID)
	if !ok {
		e.cleanup(taskID, true)
		yield(failedStatus(ec, codeInternal, "session lost"), nil)
		return
	}
	in, err := e.answerFromMessage(taskID, ec.Message)
	if err != nil {
		// A bad client reply is not an agent failure; tear the process down
		// without penalizing the breaker.
		e.teardown(taskID, sess)
		yield(failedStatus(ec, codeInternal, err.Error()), nil)
		return
	}
	e.interactions.Delete(taskID)
	if err := sess.Send(run.ctx, in); err != nil {
		e.cleanup(taskID, true)
		yield(failedStatus(ec, codeInternal, err.Error()), nil)
		return
	}
	if !yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateWorking, nil), nil) {
		e.teardown(taskID, sess)
		return
	}
	e.pump(run.ctx, ec, taskID, sess, yield)
}

// teardown stops a task's process and releases its per-task state after the
// consumer goes away.
func (e *CLIExecutor) teardown(taskID string, sess agent.Session) {
	_ = sess.Cancel()
	e.cleanup(taskID, false)
}

// cleanup releases a task's per-task state. When fail is true the failure
// breaker is penalized; otherwise it is resolved.
func (e *CLIExecutor) cleanup(taskID string, fail bool) {
	e.endRun(taskID)
	e.live.Del(taskID)
	e.baselines.Delete(taskID)
	e.interactions.Delete(taskID)
	e.releaseLock(taskID)
	if fail {
		e.breaker.Failure()
	} else {
		e.breaker.Resolve()
	}
}

func codeForRunError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return codeTimeout
	}
	return codeInternal
}

// pump consumes session events, coalescing text into artifact updates and
// keeping the task alive across an input-required pause.
func (e *CLIExecutor) pump(
	ctx context.Context, ec *a2asrv.ExecutorContext, taskID string, sess agent.Session,
	yield func(a2a.Event, error) bool,
) {
	emit := func(ev a2a.Event) bool {
		if yield(ev, nil) {
			return true
		}
		// The consumer went away: tear the CLI down so no process leaks.
		e.teardown(taskID, sess)
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
				e.finish(ctx, ec, taskID, sess, sink, emit)
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
		itx := interactionFromEvent(ev)
		e.interactions.Store(taskID, itx)
		// Keep the session alive for the client's answer.
		emit(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateInputRequired, interactionMessage(ev.Text, itx)))
		return false
	case agent.EventError:
		sink.Flush()
		e.endRun(taskID)
		e.live.Del(taskID)
		e.interactions.Delete(taskID)
		if sess.Canceled() {
			return false
		}
		e.emitChanges(ec, taskID, emit)
		e.releaseLock(taskID)
		e.breaker.Failure()
		emit(failedStatus(ec, codeInternal, eventErrorText(ev)))
		return false
	case agent.EventThinking, agent.EventToolCall, agent.EventToolResult, agent.EventUsage:
		if !sink.Flush() {
			return false
		}
		return emitTrace(ec, emit, ev)
	}
	return true
}

// emitTrace surfaces reasoning, tool activity, and usage as working status
// updates, so a client can observe progress without polluting the answer.
func emitTrace(ec *a2asrv.ExecutorContext, emit func(a2a.Event) bool, ev agent.Event) bool {
	switch ev.Kind {
	case agent.EventThinking:
		return emit(traceStatus(ec, "thinking", ev.Text, nil))
	case agent.EventToolCall:
		return emit(traceStatus(ec, "tool_call", "tool: "+ev.Tool,
			map[string]any{"tool": ev.Tool, "input": ev.ToolInput}))
	case agent.EventToolResult:
		return emit(traceStatus(ec, "tool_result", "tool result", nil))
	case agent.EventUsage:
		return emit(traceStatus(ec, "usage", "usage", usageMeta(ev.Usage)))
	}
	return true
}

// traceStatus builds a working status update tagged with a harbor.kind.
func traceStatus(ec *a2asrv.ExecutorContext, kind, text string, extra map[string]any) *a2a.TaskStatusUpdateEvent {
	m := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(text))
	meta := map[string]any{metaKeyKind: kind}
	for k, v := range extra {
		meta[k] = v
	}
	m.Metadata = meta
	return a2a.NewStatusUpdateEvent(ec, a2a.TaskStateWorking, m)
}

func usageMeta(u *agent.Usage) map[string]any {
	if u == nil {
		return nil
	}
	return map[string]any{
		"input_tokens":  u.InputTokens,
		"output_tokens": u.OutputTokens,
		"cost_usd":      u.CostUSD,
	}
}

// finish flushes pending text and emits the terminal state after the event
// channel closes (the CLI process has exited). A context deadline is reported
// as a timeout failure.
func (e *CLIExecutor) finish(
	ctx context.Context, ec *a2asrv.ExecutorContext, taskID string, sess agent.Session,
	sink *textSink, emit func(a2a.Event) bool,
) {
	defer e.releaseLock(taskID)
	defer e.endRun(taskID)
	defer e.interactions.Delete(taskID)
	if !sink.Flush() {
		return
	}
	e.live.Del(taskID)
	if sess.Canceled() {
		return // Cancel already emitted the canceled state.
	}
	e.emitChanges(ec, taskID, emit)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		e.breaker.Failure()
		emit(failedStatus(ec, codeTimeout, "task timed out"))
		return
	}
	e.breaker.Success()
	emit(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, nil))
}

// releaseLock releases the workdir lock held for a task, if any. It is idempotent.
func (e *CLIExecutor) releaseLock(taskID string) {
	if v, ok := e.locks.LoadAndDelete(taskID); ok {
		v.(func())()
	}
}

// emitChanges captures the task's working-tree changes and emits them as a
// "changes" artifact. It is a no-op when there is no baseline or no diff.
func (e *CLIExecutor) emitChanges(ec *a2asrv.ExecutorContext, taskID string, emit func(a2a.Event) bool) {
	v, ok := e.baselines.LoadAndDelete(taskID)
	if !ok {
		return
	}
	dir, err := e.workspace.Resolve(ec.ContextID, metaString(mergeMetadata(ec), "harbor.workDir"))
	if err != nil || dir == "" {
		return
	}
	res := changes.Capture(dir, v.(changes.Baseline), 0)
	if res.Diff == "" {
		return
	}
	part := a2a.NewTextPart(res.Diff)
	part.MediaType = "text/x-diff"
	evt := a2a.NewArtifactEvent(ec, part)
	evt.Artifact.Name = "changes"
	evt.Artifact.Metadata = map[string]any{"files": toAnySlice(res.Files), "truncated": res.Truncated}
	emit(evt)
}

func toAnySlice(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

// Cancel implements a2asrv.AgentExecutor.
func (e *CLIExecutor) Cancel(_ context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		taskID := string(ec.TaskID)
		if sess, ok := e.live.Get(taskID); ok {
			_ = sess.Cancel()
			e.live.Del(taskID)
		}
		e.endRun(taskID)
		e.baselines.Delete(taskID)
		e.interactions.Delete(taskID)
		e.releaseLock(taskID)
		e.breaker.Resolve()
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
	return codedStatus(ec, a2a.TaskStateFailed, code, msg)
}

// rejectedStatus builds a rejected status event (the agent did not run).
func rejectedStatus(ec *a2asrv.ExecutorContext, msg string) *a2a.TaskStatusUpdateEvent {
	return codedStatus(ec, a2a.TaskStateRejected, codeAgentUnavailable, msg)
}

// codedStatus builds a terminal status event carrying a machine-readable code.
func codedStatus(ec *a2asrv.ExecutorContext, state a2a.TaskState, code, msg string) *a2a.TaskStatusUpdateEvent {
	m := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(msg))
	if m.Metadata == nil {
		m.Metadata = map[string]any{}
	}
	m.Metadata[metaKeyErrorCode] = code
	return a2a.NewStatusUpdateEvent(ec, state, m)
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

// interactionFromEvent builds the canonical descriptor for a permission event.
// It uses the adapter's structured interaction when present and fills the gaps
// from the event's tool fields, assigning a fresh id for cross-talk checks.
func interactionFromEvent(ev agent.Event) *agent.Interaction {
	itx := &agent.Interaction{Kind: agent.InteractionPermission, Resume: true}
	if ev.Interaction != nil {
		c := *ev.Interaction
		itx = &c
	}
	if itx.Kind == "" {
		itx.Kind = agent.InteractionPermission
	}
	if itx.ID == "" {
		itx.ID = newInteractionID()
	}
	if itx.Action == "" {
		itx.Action = ev.Tool
	}
	return itx
}

// interactionMessage builds the input-required message: a human-readable text
// part plus a DataPart carrying the structured interaction for rich clients.
func interactionMessage(prompt string, itx *agent.Interaction) *a2a.Message {
	m := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(prompt))
	m.Parts = append(m.Parts, a2a.NewDataPart(interactionPayload(itx)))
	m.Metadata = map[string]any{
		metaKeyKind:          string(itx.Kind),
		metaKeyInteractionID: itx.ID,
	}
	return m
}

// interactionPayload renders an interaction as JSON-native values (string,
// bool, []any). The DataPart must not hold the Go struct: the task store
// serializes tasks with gob, which requires every type carried in an interface
// to be registered. Marshaling through JSON keeps the wire form and the stored
// form identical and derived from the struct's own tags.
func interactionPayload(itx *agent.Interaction) map[string]any {
	m := map[string]any{}
	if b, err := json.Marshal(itx); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

// answerFromMessage maps a client's reply onto an agent.Input. The interaction
// id may arrive in a structured DataPart or in the message metadata; either way
// it is checked against the pending interaction. A reply with no id is accepted
// as a text fallback for clients that do not use the extension.
func (e *CLIExecutor) answerFromMessage(taskID string, m *a2a.Message) (agent.Input, error) {
	id, answer := parseAnswer(m)
	if cur, ok := e.pendingInteraction(taskID); ok && id != "" && id != cur.ID {
		return agent.Input{}, fmt.Errorf("interaction id mismatch: got %q, want %q", id, cur.ID)
	}
	return agent.Input{Kind: "answer", ID: id, Answer: answer}, nil
}

// parseAnswer extracts the interaction id and answer text from a client reply.
// It prefers a structured DataPart and falls back to the message metadata
// (key "harbor.interactionId") plus the first text part.
func parseAnswer(m *a2a.Message) (id, answer string) {
	if m == nil {
		return "", ""
	}
	for _, p := range m.Parts {
		if p == nil || p.Data() == nil {
			continue
		}
		b, err := json.Marshal(p.Data())
		if err != nil {
			continue
		}
		var a agent.Answer
		if err := json.Unmarshal(b, &a); err != nil {
			continue
		}
		if a.ID != "" || a.Answer != "" {
			return a.ID, a.Answer
		}
	}
	return metaString(m.Metadata, metaKeyInteractionID), firstTextPart(m)
}

// pendingInteraction returns the input request a paused task is waiting on.
func (e *CLIExecutor) pendingInteraction(taskID string) (*agent.Interaction, bool) {
	v, ok := e.interactions.Load(taskID)
	if !ok {
		return nil, false
	}
	return v.(*agent.Interaction), true
}

// newInteractionID returns a short random interaction identifier.
func newInteractionID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "itx_unknown"
	}
	return "itx_" + hex.EncodeToString(b[:])
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
