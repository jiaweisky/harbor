package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/jiaweisky/harbor/internal/agent"
	"github.com/jiaweisky/harbor/internal/changes"
	"github.com/jiaweisky/harbor/internal/core"
	"github.com/jiaweisky/harbor/internal/telemetry"
)

// error codes written to TaskStatus.Message.Metadata["harbor.errorCode"].
const (
	codeInternal           = "internal"
	codeCLINotFound        = "cli_not_found"
	codeUnsupportedContent = "unsupported_content"
	codeTimeout            = "timeout"
	codeMaxTurns           = "max_turns"
	codeAgentUnavailable   = "agent_unavailable"
	// codeInteractionUnexpected marks a permission prompt from a non-interactive
	// agent whose CLI was not configured to bypass permissions.
	codeInteractionUnexpected = "interaction_unexpected"
)

// Metadata keys carried on task status messages. The interaction id is both
// written on an input-required prompt and read back from a client's answer.
const (
	metaKeyKind          = "harbor.kind"
	metaKeyErrorCode     = "harbor.errorCode"
	metaKeyInteractionID = "harbor.interactionId"
	metaKeySessionID     = "harbor.sessionId"
)

// errNoText is returned when an incoming message has no textual part.
var errNoText = errors.New("no text part in message")

// SessionStore binds an A2A context to a CLI-native session id.
type SessionStore interface {
	Get(contextID string) (string, bool)
	Bind(contextID, sessionID string)
}

// Workspace resolves the working directory for a task and serializes access to
// it. The workspace package implements it; tests may pass nil for the default.
type Workspace interface {
	Resolve(contextID string) (string, error)
	Lock(dir string) func()
}

type nopWorkspace struct{}

func (nopWorkspace) Resolve(string) (string, error) { return "", nil }
func (nopWorkspace) Lock(string) func()             { return func() {} }

// CLIExecutor turns a coding-agent CLI session into A2A task events.
type CLIExecutor struct {
	launcher               agent.Launcher
	sessions               SessionStore
	live                   *agent.LiveSessions
	workspace              Workspace
	timeout                time.Duration // total task timeout; 0 disables
	idleTimeout            time.Duration // fail a task with no output for this long; 0 disables
	keepalive              time.Duration // emit a keepalive when quiet for this long; 0 disables
	breaker                *breaker      // nil disables
	tasks                  sync.Map      // taskID -> *taskState (all per-task transient state)
	maxTurns               int
	nonImageArtifacts      bool     // emit an artifact for each changed non-image file
	artifactInlineMaxBytes int64    // max changed-file bytes inlined; 0 uses the default
	diffInlineMaxBytes     int64    // max diff bytes; 0 uses the default
	nonGitChangeGlobs      []string // opt-in globs for change capture outside git
	taskLookup             func(context.Context, a2a.TaskID) (*a2a.Task, bool)
	idem                   *idemCache // messageId -> taskID (duplicate-send guard)
	metrics                *agentMetrics
}

// NewCLIExecutor creates an executor over the given launcher. limits bounds the
// task duration and the failure circuit breaker.
func NewCLIExecutor(
	ln agent.Launcher, sessions SessionStore, live *agent.LiveSessions,
	ws Workspace, limits core.Limits,
) *CLIExecutor {
	if ws == nil {
		ws = nopWorkspace{}
	}
	return &CLIExecutor{
		launcher:    ln,
		sessions:    sessions,
		live:        live,
		workspace:   ws,
		timeout:     limits.Timeout,
		idleTimeout: limits.IdleTimeout,
		maxTurns:    limits.MaxTurns,
		breaker:     newBreaker(limits.Breaker),
		idem:        newIdemCache(4096),
		metrics:     &agentMetrics{},
	}
}

// SetNonImageFileArtifacts enables emitting a per-file artifact for each changed
// non-image file.
func (e *CLIExecutor) SetNonImageFileArtifacts(on bool) { e.nonImageArtifacts = on }

// SetArtifactInlineMaxBytes bounds how much of one changed file is inlined into a
// file or image artifact; 0 uses the default.
func (e *CLIExecutor) SetArtifactInlineMaxBytes(n int64) { e.artifactInlineMaxBytes = n }

// SetDiffInlineMaxBytes bounds the captured diff before it is truncated; 0 uses
// the default.
func (e *CLIExecutor) SetDiffInlineMaxBytes(n int64) { e.diffInlineMaxBytes = n }

// SetNonGitChangeGlobs enables non-git change capture for the given relative
// glob patterns; an empty list disables it.
func (e *CLIExecutor) SetNonGitChangeGlobs(globs []string) {
	e.nonGitChangeGlobs = append([]string(nil), globs...)
}

// SetKeepaliveInterval makes the executor emit a lightweight working update
// when the CLI has been silent for this long, so a liveness watchdog does not
// mistake a long output-less step for a dead agent. 0 disables it.
func (e *CLIExecutor) SetKeepaliveInterval(d time.Duration) { e.keepalive = d }

// SetTaskLookup installs a lookup used to answer a duplicate send (same
// messageId) from the earlier task's result instead of re-running the CLI.
func (e *CLIExecutor) SetTaskLookup(fn func(context.Context, a2a.TaskID) (*a2a.Task, bool)) {
	e.taskLookup = fn
}

// Name reports the agent id this executor serves.
func (e *CLIExecutor) Name() string { return e.launcher.Name() }

// Ready reports whether the agent can accept tasks: false while its circuit
// breaker is open or probing after failures.
func (e *CLIExecutor) Ready() bool { return e.breaker.State() == stateNameClosed }

// BreakerState reports the current circuit-breaker state name.
func (e *CLIExecutor) BreakerState() string { return e.breaker.State() }

// Snapshot returns a point-in-time copy of the agent's counters and health.
func (e *CLIExecutor) Snapshot() MetricsSnapshot {
	snap := MetricsSnapshot{
		Name:               e.Name(),
		Submitted:          e.metrics.submitted.Load(),
		Active:             e.metrics.active.Load(),
		Completed:          e.metrics.completed.Load(),
		Failed:             e.metrics.failed.Load(),
		Canceled:           e.metrics.canceled.Load(),
		Rejected:           e.metrics.rejected.Load(),
		BreakerState:       e.breaker.State(),
		DurationSumSeconds: float64(e.metrics.durSumMS.Load()) / 1000,
		DurationCount:      e.metrics.durCount.Load(),
		TokensIn:           e.metrics.tokensIn.Load(),
		TokensOut:          e.metrics.tokensOut.Load(),
		CostUSD:            float64(e.metrics.costMicro.Load()) / 1e6,
	}
	snap.DurationBuckets = make([]int64, len(durationBucketBounds))
	for i := range durationBucketBounds {
		snap.DurationBuckets[i] = e.metrics.durBuckets[i].Load()
	}
	return snap
}

// Execute implements a2asrv.AgentExecutor. A task paused on input-required is
// resumed; any other task starts a new CLI session.
func (e *CLIExecutor) Execute(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		taskID := string(ec.TaskID)
		if e.answerWithoutRunning(ctx, ec, yield) {
			return
		}
		username := ""
		if ec.User != nil {
			username = ec.User.Name
		}
		// One span per request; a paused (input-required) task's span ends when
		// the request returns, and its subprocess span (see Runner) ends when the
		// process does. The context carries the span into the CLI child span.
		ctx, span := telemetry.Start(ctx, "harbor.task",
			attribute.String("harbor.agent", e.Name()),
			attribute.String("harbor.task_id", taskID),
			attribute.String("harbor.user", username))
		defer span.End()
		emit := e.instrument(taskID, username, span, yield)
		if !e.breaker.Allow() {
			if ec.StoredTask == nil && !emit(a2a.NewSubmittedTask(ec, ec.Message), nil) {
				return
			}
			emit(rejectedStatus(ec, "agent is temporarily unavailable after repeated failures"), nil)
			return
		}
		if isWaitingForInput(ec) {
			e.resume(ctx, ec, taskID, emit)
			return
		}
		e.start(ctx, ec, taskID, emit)
	}
}

// instrument wraps yield to record per-task metrics and audit log entries as
// events flow to the client. It records a submission and a terminal outcome at
// most once per invocation, and observes the task duration (from submission,
// across a resume) on the terminal. A resumed task counts its outcome when it
// truly finishes rather than when it pauses.
func (e *CLIExecutor) instrument(
	taskID, username string, span trace.Span, yield func(a2a.Event, error) bool,
) func(a2a.Event, error) bool {
	name := e.Name()
	start := time.Now()
	submitted, finished := false, false
	return func(ev a2a.Event, err error) bool {
		if !submitted {
			if tk, ok := ev.(*a2a.Task); ok && tk.Status.State == a2a.TaskStateSubmitted {
				submitted = true
				e.metrics.submitted.Add(1)
				telemetry.TaskSubmitted(name)
				slog.Info("task submitted", "agent", name, "task", taskID, "user", username)
			}
		}
		if !finished {
			if outcome, ok := terminalOutcome(ev); ok {
				finished = true
				e.metrics.record(outcome)
				span.SetAttributes(attribute.String("harbor.outcome", outcome))
				if st := e.loadState(taskID); st != nil {
					start = st.started // measure from submission, across a resume
				}
				dur := time.Since(start)
				e.metrics.observeDuration(dur)
				telemetry.TaskFinished(name, outcome)
				telemetry.TaskDuration(name, dur)
				slog.Info("task finished", "agent", name, "task", taskID, "outcome", outcome, "user", username)
			}
		}
		return yield(ev, err)
	}
}

// start creates the task and begins a new CLI session.
func (e *CLIExecutor) start(
	ctx context.Context, ec *a2asrv.ExecutorContext, taskID string,
	yield func(a2a.Event, error) bool,
) {
	st := e.startTask(ctx, taskID)
	if e.takeTurn(st) {
		e.cleanup(taskID, false)
		yield(failedStatus(ec, codeMaxTurns, "max turns exceeded"), nil)
		return
	}
	if ec.StoredTask == nil {
		if !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
			e.cleanup(taskID, false)
			return
		}
	}
	if !yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateWorking, nil), nil) {
		e.cleanup(taskID, false)
		return
	}

	req, err := e.toRequest(ctx, ec)
	if err != nil {
		e.cleanup(taskID, false)
		yield(failedStatus(ec, codeForRequestError(err), err.Error()), nil)
		return
	}
	if req.OutputSchema != nil {
		st.setStructured(&structuredState{})
	}
	st.setLock(e.workspace.Lock(req.WorkDir))
	if req.WorkDir != "" {
		st.setBaseline(changes.Snapshot(req.WorkDir, e.nonGitChangeGlobs...))
	}
	e.launchAndPump(ctx, st, ec, taskID, req, false, yield)
}

// launchAndPump starts the CLI for a prepared task and pumps its events. A
// resumed session announces the working state after the process starts; a fresh
// start already announced it, so emitWorking is false there.
func (e *CLIExecutor) launchAndPump(
	parent context.Context, st *taskState, ec *a2asrv.ExecutorContext, taskID string,
	req *agent.Request, emitWorking bool, yield func(a2a.Event, error) bool,
) {
	sess, err := e.launcher.Start(agent.WithTaskID(st.ctx, taskID), req)
	if err != nil {
		e.cleanup(taskID, true)
		yield(failedStatus(ec, codeForStartError(err), err.Error()), nil)
		return
	}
	e.live.Put(taskID, sess)
	e.watchExecution(parent, taskID)
	if emitWorking && !yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateWorking, nil), nil) {
		e.teardown(taskID, sess)
		return
	}
	e.pump(st, ec, taskID, sess, yield)
}

// startTask creates a task's state and starts its process lifetime. The context
// is detached from parent (canceled when the RPC returns) so a paused task stays
// alive; it is bounded by the configured task timeout.
func (e *CLIExecutor) startTask(parent context.Context, taskID string) *taskState {
	base := context.WithoutCancel(parent)
	runCtx, cancel := context.WithCancel(base)
	if e.timeout > 0 {
		runCtx, cancel = context.WithTimeout(base, e.timeout)
	}
	st := &taskState{ctx: runCtx, cancel: cancel, started: time.Now()}
	e.tasks.Store(taskID, st)
	e.metrics.active.Add(1)
	telemetry.TaskActive(1)
	return st
}

// loadState returns a task's state, or nil when the task has none.
func (e *CLIExecutor) loadState(taskID string) *taskState {
	if v, ok := e.tasks.Load(taskID); ok {
		return v.(*taskState)
	}
	return nil
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
	st *taskState, ec *a2asrv.ExecutorContext, taskID string, sess agent.Session,
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

	var idle *time.Timer
	var idleC <-chan time.Time
	if e.idleTimeout > 0 {
		idle = time.NewTimer(e.idleTimeout)
		idleC = idle.C
		defer idle.Stop()
	}

	var keep *time.Ticker
	var keepC <-chan time.Time
	if e.keepalive > 0 {
		keep = time.NewTicker(e.keepalive)
		keepC = keep.C
		defer keep.Stop()
	}

	ch := sess.Events()
	lastActivity := time.Now()
	for {
		select {
		case ev, ok := <-ch:
			lastActivity = time.Now()
			if idle != nil {
				idle.Reset(e.idleTimeout)
			}
			if !ok {
				e.finish(st, ec, taskID, sess, sink, emit)
				return
			}
			if !e.handleEvent(st, ec, taskID, sess, sink, emit, ev) {
				// A pause keeps the process and workdir lock alive for the client's
				// answer; reap them if the answer never arrives. Skip a state torn
				// down by a consumer disconnect.
				if st.pendingInteraction() != nil && st.ctx.Err() == nil {
					e.armPauseReaper(st, taskID, sess)
				}
				return
			}
		case <-ticker.C:
			if !sink.Flush() {
				return
			}
		case <-keepC:
			if time.Since(lastActivity) >= e.keepalive {
				if !emit(traceStatus(ec, "keepalive", "working", nil)) {
					return
				}
				lastActivity = time.Now()
			}
		case <-idleC:
			e.idleTimeoutFail(ec, taskID, sess, sink, emit)
			return
		}
	}
}

// handleEvent processes one session event and reports whether the loop should
// continue. A false result means the pump may stop (yield stopped, or a
// terminal/input-required state was emitted).
func (e *CLIExecutor) handleEvent(
	st *taskState, ec *a2asrv.ExecutorContext, taskID string, sess agent.Session,
	sink *textSink, emit func(a2a.Event) bool, ev agent.Event,
) bool {
	switch ev.Kind {
	case agent.EventSession:
		e.sessions.Bind(contextKey(ec), ev.SessionID)
		return true
	case agent.EventTextDelta, agent.EventTextFinal:
		// TextFinal is the result-record fallback, emitted only when no textual
		// output was seen, so it never duplicates a delta.
		return sink.Write(ev.Text)
	case agent.EventStructuredOutput:
		if !sink.Flush() {
			return false
		}
		e.markStructured(st)
		return emitStructured(ec, emit, ev.Data)
	case agent.EventPermissionRequest:
		if !sink.Flush() {
			return false
		}
		e.handlePermissionRequest(ec, taskID, st, emit, ev)
		return false
	case agent.EventError:
		sink.Flush()
		if sess.Canceled() {
			e.forget(taskID)
			return false
		}
		e.fail(ec, taskID, emit, codeInternal, eventErrorText(ev))
		return false
	case agent.EventDone:
		// The CLI reported its terminal result. A duplex session stays alive
		// waiting for more input, so finish the task here instead of waiting
		// for the process to exit.
		if !sink.Flush() {
			return false
		}
		if sess.Canceled() {
			e.forget(taskID)
			return false
		}
		e.emitStructuredFallback(st, ec, sink, emit)
		e.complete(ec, taskID, emit)
		return false
	case agent.EventThinking, agent.EventToolCall, agent.EventToolResult, agent.EventUsage:
		if !sink.Flush() {
			return false
		}
		if ev.Kind == agent.EventUsage {
			e.metrics.addUsage(ev.Usage)
			if ev.Usage != nil {
				telemetry.TaskUsage(e.Name(), ev.Usage.InputTokens, ev.Usage.OutputTokens, ev.Usage.CostUSD)
			}
		}
		return emitTrace(ec, emit, ev)
	}
	return true
}

// handlePermissionRequest pauses a task for the client's answer, or fails it when
// the agent is non-interactive and the CLI should not have prompted.
func (e *CLIExecutor) handlePermissionRequest(
	ec *a2asrv.ExecutorContext, taskID string, st *taskState, emit func(a2a.Event) bool, ev agent.Event,
) {
	if e.launcher.Capabilities().Permission != agent.PermInteractive {
		e.fail(ec, taskID, emit, codeInteractionUnexpected,
			"non-interactive agent received a permission request; configure the CLI to bypass permissions")
		return
	}
	itx := interactionFromEvent(ev)
	st.setInteraction(itx)
	// Keep the session alive for the client's answer; record its id so a restart
	// can resume the CLI session and inject the answer.
	sid, _ := e.sessions.Get(contextKey(ec))
	emit(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateInputRequired, interactionMessage(ev.Text, itx, sid)))
}

// emitStructured delivers a schema-validated result as a JSON DataPart artifact.
func emitStructured(ec *a2asrv.ExecutorContext, emit func(a2a.Event) bool, data map[string]any) bool {
	if data == nil {
		return true
	}
	evt := a2a.NewArtifactEvent(ec, a2a.NewDataPart(data))
	if evt.Artifact != nil {
		evt.Artifact.Name = "result"
	}
	return emit(evt)
}

// markStructured records that a task emitted an explicit structured result. The
// caller always has a live task state.
func (e *CLIExecutor) markStructured(st *taskState) {
	ss := st.structuredState()
	if ss == nil {
		ss = &structuredState{}
		st.setStructured(ss)
	}
	ss.emitted = true
}

// emitStructuredFallback emits a schema-validated DataPart parsed from the final
// text when the request asked for structured output but the CLI produced no
// explicit structured event (exec CLIs whose output is the JSON itself). It is a
// no-op otherwise.
func (e *CLIExecutor) emitStructuredFallback(
	st *taskState, ec *a2asrv.ExecutorContext, sink *textSink, emit func(a2a.Event) bool,
) {
	ss := st.structuredState()
	if ss == nil || ss.emitted {
		return
	}
	if data := jsonObject(sink.Text()); data != nil {
		ss.emitted = true
		emitStructured(ec, emit, data)
	}
}

// jsonObject parses text as a JSON object, or returns nil.
func jsonObject(text string) map[string]any {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "{") {
		return nil
	}
	var m map[string]any
	if json.Unmarshal([]byte(text), &m) != nil {
		return nil
	}
	return m
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

// finish flushes pending text and emits the terminal state after the event
// channel closes (the CLI process has exited). A context deadline is reported
// as a timeout failure.
func (e *CLIExecutor) finish(
	st *taskState, ec *a2asrv.ExecutorContext, taskID string, sess agent.Session,
	sink *textSink, emit func(a2a.Event) bool,
) {
	defer e.forget(taskID)
	if !sink.Flush() {
		return
	}
	if sess.Canceled() {
		return // Cancel already emitted the canceled state.
	}
	if errors.Is(st.ctx.Err(), context.DeadlineExceeded) {
		e.fail(ec, taskID, emit, codeTimeout, "task timed out")
		return
	}
	e.emitStructuredFallback(st, ec, sink, emit)
	e.emitChanges(ec, taskID, emit)
	e.breaker.Success()
	emit(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, nil))
}

// complete finishes a task on a terminal event the CLI reported, without
// waiting for the process to exit (a duplex CLI keeps running for more input).
func (e *CLIExecutor) complete(ec *a2asrv.ExecutorContext, taskID string, emit func(a2a.Event) bool) {
	e.emitChanges(ec, taskID, emit)
	e.breaker.Success()
	emit(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, nil))
	e.forget(taskID)
}

// idleTimeoutFail fails a task whose CLI produced no output within the idle
// timeout. It stops the process and reports a timeout.
func (e *CLIExecutor) idleTimeoutFail(
	ec *a2asrv.ExecutorContext, taskID string, sess agent.Session,
	sink *textSink, emit func(a2a.Event) bool,
) {
	sink.Flush()
	if sess.Canceled() {
		return // Cancel already ended the task.
	}
	e.fail(ec, taskID, emit, codeTimeout, "task idle timeout")
}

// fail captures a task's changes and emits a failed status with a
// machine-readable code, then releases the task's state.
func (e *CLIExecutor) fail(
	ec *a2asrv.ExecutorContext, taskID string, emit func(a2a.Event) bool, code, msg string,
) {
	e.emitChanges(ec, taskID, emit)
	e.breaker.Failure()
	emit(failedStatus(ec, code, msg))
	e.forget(taskID)
}

// Cancel implements a2asrv.AgentExecutor.
func (e *CLIExecutor) Cancel(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		taskID := string(ec.TaskID)
		username := ""
		if ec.User != nil {
			username = ec.User.Name
		}
		_, span := telemetry.Start(ctx, "harbor.cancel",
			attribute.String("harbor.agent", e.Name()),
			attribute.String("harbor.task_id", taskID),
			attribute.String("harbor.user", username))
		defer span.End()
		emit := e.instrument(taskID, username, span, yield)
		if sess, ok := e.live.Get(taskID); ok {
			_ = sess.Cancel()
		}
		e.breaker.Resolve()
		emit(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCanceled, nil), nil)
		e.forget(taskID)
	}
}
