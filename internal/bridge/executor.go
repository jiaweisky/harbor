package bridge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/jiaweisky/harbor/internal/agent"
	"github.com/jiaweisky/harbor/internal/changes"
	"github.com/jiaweisky/harbor/internal/config"
	"github.com/jiaweisky/harbor/internal/telemetry"
	"github.com/jiaweisky/harbor/internal/workspace"
)

// error codes written to TaskStatus.Message.Metadata["harbor.errorCode"].
const (
	codeInternal           = "internal"
	codeCLINotFound        = "cli_not_found"
	codeUnsupportedContent = "unsupported_content"
	codeWorkdirDenied      = "workdir_denied"
	codeTimeout            = "timeout"
	codeMaxTurns           = "max_turns"
	codeAgentUnavailable   = "agent_unavailable"
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

// CLIExecutor turns a coding-agent CLI session into A2A task events.
type CLIExecutor struct {
	launcher      agent.Launcher
	sessions      SessionStore
	live          *agent.LiveSessions
	workspace     *workspace.Resolver
	timeout       time.Duration // total task timeout; 0 disables
	idleTimeout   time.Duration // fail a task with no output for this long; 0 disables
	breaker       *breaker      // nil disables
	baselines     sync.Map      // taskID -> changes.Baseline
	locks         sync.Map      // taskID -> func() (workdir lock release)
	runs          sync.Map      // taskID -> *taskRun (process lifetime, decoupled from the request)
	interactions  sync.Map      // taskID -> *agent.Interaction awaiting an answer
	taskStart     sync.Map      // taskID -> time.Time (submission, for duration)
	structured    sync.Map      // taskID -> *structuredState
	turns         sync.Map      // taskID -> *atomic.Int64 (start + resumes)
	maxTurns      int
	fileArtifacts bool // emit a per-file artifact for each changed file
	taskLookup    func(context.Context, a2a.TaskID) (*a2a.Task, bool)
	idem          *idemCache // messageId -> taskID (duplicate-send guard)
	metrics       *agentMetrics
}

// idemCache is a bounded messageId -> taskID map used to make repeated sends
// idempotent (the same message must not start a second execution).
type idemCache struct {
	mu    sync.Mutex
	m     map[string]a2a.TaskID
	order []string
	max   int
}

func newIdemCache(max int) *idemCache {
	return &idemCache{m: map[string]a2a.TaskID{}, max: max}
}

// getOrPut returns the existing task id for key, or records id and reports that
// it was absent. It is atomic, so two concurrent duplicates cannot both start a
// new execution.
func (c *idemCache) getOrPut(key string, id a2a.TaskID) (a2a.TaskID, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if v, ok := c.m[key]; ok {
		return v, true
	}
	c.storeLocked(key, id)
	return id, false
}

// set re-points a key to a new task id.
func (c *idemCache) set(key string, id a2a.TaskID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.storeLocked(key, id)
}

// storeLocked records id for key, evicting the oldest entry when at capacity. It
// must be called with the lock held.
func (c *idemCache) storeLocked(key string, id a2a.TaskID) {
	if _, ok := c.m[key]; !ok {
		if len(c.order) >= c.max {
			delete(c.m, c.order[0])
			c.order = c.order[1:]
		}
		c.order = append(c.order, key)
	}
	c.m[key] = id
}

// structuredState tracks a task whose request asked for structured output, so
// the executor can fall back to parsing the final text when the CLI has no
// explicit structured-output event (exec CLIs).
type structuredState struct{ emitted bool }

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

// SetFileArtifacts enables emitting a per-file artifact for each changed file.
func (e *CLIExecutor) SetFileArtifacts(on bool) { e.fileArtifacts = on }

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
			e.resume(ec, taskID, emit)
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
				e.taskStart.Store(taskID, start)
				slog.Info("task submitted", "agent", name, "task", taskID, "user", username)
			}
		}
		if !finished {
			if outcome, ok := terminalOutcome(ev); ok {
				finished = true
				e.metrics.record(outcome)
				span.SetAttributes(attribute.String("harbor.outcome", outcome))
				if ts, ok := e.taskStart.LoadAndDelete(taskID); ok {
					start = ts.(time.Time) // measure from submission, across a resume
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
	if e.takeTurn(taskID) {
		e.breaker.Resolve()
		yield(failedStatus(ec, codeMaxTurns, "max turns exceeded"), nil)
		return
	}
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
	if req.OutputSchema != nil {
		e.structured.Store(taskID, &structuredState{})
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
	e.metrics.active.Add(1)
	telemetry.TaskActive(1)
	return runCtx
}

// endRun cancels and forgets a task's process lifetime. It is idempotent, so
// the active gauge is decremented exactly once per run.
func (e *CLIExecutor) endRun(taskID string) {
	if v, ok := e.runs.LoadAndDelete(taskID); ok {
		v.(*taskRun).cancel()
		e.metrics.active.Add(-1)
		telemetry.TaskActive(-1)
	}
}

// resume answers a task paused on input-required by writing to its live session.
func (e *CLIExecutor) resume(
	ec *a2asrv.ExecutorContext, taskID string,
	yield func(a2a.Event, error) bool,
) {
	if e.takeTurn(taskID) {
		if sess, ok := e.live.Get(taskID); ok {
			e.teardown(taskID, sess)
		}
		e.breaker.Resolve()
		yield(failedStatus(ec, codeMaxTurns, "max turns exceeded"), nil)
		return
	}
	v, ok := e.runs.Load(taskID)
	if !ok {
		e.resumeFresh(ec, taskID, yield)
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
		e.resumeFresh(ec, taskID, yield)
		return
	}
	in, err := e.answerFromMessage(ec, taskID, ec.Message)
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

// resumeFresh recovers a task paused on input-required whose live process is gone
// (for example after a Harbor restart): it resumes the CLI's recorded session and
// injects the client's answer as the next prompt. The injection is best-effort
// (the CLI restarts and may replay context) and requires a recorded session id
// and a resume-capable CLI.
func (e *CLIExecutor) resumeFresh(
	ec *a2asrv.ExecutorContext, taskID string, yield func(a2a.Event, error) bool,
) {
	sid := e.sessionFromTask(ec)
	if sid == "" || !e.launcher.Capabilities().SessionResume {
		e.forget(taskID)
		yield(failedStatus(ec, codeInternal, "session lost"), nil)
		return
	}
	in, err := e.answerFromMessage(ec, taskID, ec.Message)
	if err != nil || strings.TrimSpace(in.Answer) == "" {
		yield(failedStatus(ec, codeInternal, "session lost"), nil)
		return
	}
	dir, err := e.workspace.Resolve(contextKey(ec), metaString(mergeMetadata(ec), "harbor.workDir"))
	if err != nil {
		yield(failedStatus(ec, codeForRequestError(err), err.Error()), nil)
		return
	}
	runCtx := e.beginRun(context.Background(), taskID)
	sess, err := e.launcher.Start(agent.WithTaskID(runCtx, taskID),
		&agent.Request{Prompt: in.Answer, SessionID: sid, WorkDir: dir})
	if err != nil {
		e.cleanup(taskID, true)
		yield(failedStatus(ec, codeForStartError(err), err.Error()), nil)
		return
	}
	e.live.Put(taskID, sess)
	if !yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateWorking, nil), nil) {
		e.teardown(taskID, sess)
		return
	}
	e.pump(runCtx, ec, taskID, sess, yield)
}

// sessionFromTask returns the CLI session id recorded with a paused task.
func (e *CLIExecutor) sessionFromTask(ec *a2asrv.ExecutorContext) string {
	if ec.StoredTask == nil || ec.StoredTask.Status.Message == nil {
		return ""
	}
	return metaString(ec.StoredTask.Status.Message.Metadata, metaKeySessionID)
}

// teardown stops a task's process and releases its per-task state after the
// consumer goes away.
func (e *CLIExecutor) teardown(taskID string, sess agent.Session) {
	_ = sess.Cancel()
	e.cleanup(taskID, false)
}

// forget releases a task's transient state: its process lifetime, live session,
// change baseline, pending interaction, and workdir lock. It is idempotent and
// does not touch the failure breaker.
func (e *CLIExecutor) forget(taskID string) {
	e.endRun(taskID)
	e.live.Del(taskID)
	e.baselines.Delete(taskID)
	e.interactions.Delete(taskID)
	e.structured.Delete(taskID)
	e.turns.Delete(taskID)
	e.releaseLock(taskID)
}

// takeTurn counts a turn for the task (a start or a resume) and reports
// whether it exceeds the configured cap. A cap of 0 disables the check.
func (e *CLIExecutor) takeTurn(taskID string) bool {
	if e.maxTurns <= 0 {
		return false
	}
	v, _ := e.turns.LoadOrStore(taskID, new(atomic.Int64))
	return int(v.(*atomic.Int64).Add(1)) > e.maxTurns
}

// cleanup releases a task's state. When fail is true the failure breaker is
// penalized; otherwise it is resolved.
func (e *CLIExecutor) cleanup(taskID string, fail bool) {
	e.forget(taskID)
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

	var idle *time.Timer
	var idleC <-chan time.Time
	if e.idleTimeout > 0 {
		idle = time.NewTimer(e.idleTimeout)
		idleC = idle.C
		defer idle.Stop()
	}

	ch := sess.Events()
	for {
		select {
		case ev, ok := <-ch:
			if idle != nil {
				idle.Reset(e.idleTimeout)
			}
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
	ec *a2asrv.ExecutorContext, taskID string, sess agent.Session,
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
		e.markStructured(taskID)
		return emitStructured(ec, emit, ev.Data)
	case agent.EventPermissionRequest:
		if !sink.Flush() {
			return false
		}
		itx := interactionFromEvent(ev)
		e.interactions.Store(taskID, itx)
		// Keep the session alive for the client's answer; record its id so a
		// restart can resume the CLI session and inject the answer.
		sid, _ := e.sessions.Get(contextKey(ec))
		emit(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateInputRequired, interactionMessage(ev.Text, itx, sid)))
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
		e.emitStructuredFallback(ec, taskID, sink, emit)
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

// markStructured records that a task emitted an explicit structured result.
func (e *CLIExecutor) markStructured(taskID string) {
	if v, ok := e.structured.Load(taskID); ok {
		v.(*structuredState).emitted = true
		return
	}
	e.structured.Store(taskID, &structuredState{emitted: true})
}

// emitStructuredFallback emits a schema-validated DataPart parsed from the final
// text when the request asked for structured output but the CLI produced no
// explicit structured event (exec CLIs whose output is the JSON itself). It is a
// no-op otherwise.
func (e *CLIExecutor) emitStructuredFallback(
	ec *a2asrv.ExecutorContext, taskID string, sink *textSink, emit func(a2a.Event) bool,
) {
	v, ok := e.structured.Load(taskID)
	if !ok || v.(*structuredState).emitted {
		return
	}
	if data := jsonObject(sink.Text()); data != nil {
		e.markStructured(taskID)
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

// finish flushes pending text and emits the terminal state after the event
// channel closes (the CLI process has exited). A context deadline is reported
// as a timeout failure.
func (e *CLIExecutor) finish(
	ctx context.Context, ec *a2asrv.ExecutorContext, taskID string, sess agent.Session,
	sink *textSink, emit func(a2a.Event) bool,
) {
	defer e.forget(taskID)
	if !sink.Flush() {
		return
	}
	if sess.Canceled() {
		return // Cancel already emitted the canceled state.
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		e.fail(ec, taskID, emit, codeTimeout, "task timed out")
		return
	}
	e.emitStructuredFallback(ec, taskID, sink, emit)
	e.emitChanges(ec, taskID, emit)
	e.breaker.Success()
	emit(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, nil))
}

// complete finishes a task on a terminal event the CLI reported, without
// waiting for the process to exit (a duplex CLI keeps running for more input).
func (e *CLIExecutor) complete(ec *a2asrv.ExecutorContext, taskID string, emit func(a2a.Event) bool) {
	e.emitChanges(ec, taskID, emit)
	e.forget(taskID)
	e.breaker.Success()
	emit(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, nil))
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

// fail captures a task's changes, releases its state, and emits a failed status
// with a machine-readable code.
func (e *CLIExecutor) fail(
	ec *a2asrv.ExecutorContext, taskID string, emit func(a2a.Event) bool, code, msg string,
) {
	e.emitChanges(ec, taskID, emit)
	e.forget(taskID)
	e.breaker.Failure()
	emit(failedStatus(ec, code, msg))
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
	dir, err := e.workspace.Resolve(contextKey(ec), metaString(mergeMetadata(ec), "harbor.workDir"))
	if err != nil || dir == "" {
		return
	}
	res := changes.Capture(dir, v.(changes.Baseline), 0)
	// Surface changed image files (for example a text-to-image result) as raw
	// image artifacts, alongside the textual diff.
	e.emitImageArtifacts(ec, dir, res.Files, emit)
	if e.fileArtifacts {
		e.emitFileArtifacts(ec, dir, res.Files, emit)
	}
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

// maxImageBytes caps a single emitted image artifact.
const maxImageBytes = 8 << 20

// imageMediaTypes maps a lower-case file extension to its image MIME type. It
// covers the formats the supported CLIs produce.
var imageMediaTypes = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".gif": "image/gif", ".webp": "image/webp",
}

// maxFileArtifactBytes caps a single per-file artifact.
const maxFileArtifactBytes = 1 << 20

// textMediaTypes maps a lower-case file extension to a text media type; unknown
// extensions fall back to application/octet-stream.
var textMediaTypes = map[string]string{
	".txt": "text/plain", ".md": "text/markdown", ".json": "application/json",
	".yaml": "application/yaml", ".yml": "application/yaml", ".xml": "application/xml",
	".csv": "text/csv", ".diff": "text/x-diff", ".patch": "text/x-diff",
	".go": "text/plain", ".py": "text/plain", ".js": "text/plain", ".ts": "text/plain",
	".sh": "text/plain", ".html": "text/html", ".css": "text/css",
}

// emitImageArtifacts emits one raw image artifact per changed image file in the
// task's working directory, so a generated image reaches the client as an image
// part (not merely a path in the diff).
func (e *CLIExecutor) emitImageArtifacts(
	ec *a2asrv.ExecutorContext, dir string, files []string, emit func(a2a.Event) bool,
) {
	for _, rel := range files {
		media, ok := imageMediaTypes[strings.ToLower(filepath.Ext(rel))]
		if !ok {
			continue
		}
		if !emitChangedFile(ec, dir, rel, "image", media, maxImageBytes, emit) {
			return
		}
	}
}

// emitFileArtifacts emits one raw FilePart per changed non-image file (bounded
// per file), so a client receives the produced files, not only a diff.
func (e *CLIExecutor) emitFileArtifacts(
	ec *a2asrv.ExecutorContext, dir string, files []string, emit func(a2a.Event) bool,
) {
	for _, rel := range files {
		ext := strings.ToLower(filepath.Ext(rel))
		if _, isImage := imageMediaTypes[ext]; isImage {
			continue // already emitted as an image artifact
		}
		media, ok := textMediaTypes[ext]
		if !ok {
			media = "application/octet-stream"
		}
		if !emitChangedFile(ec, dir, rel, "file", media, maxFileArtifactBytes, emit) {
			return
		}
	}
}

// emitChangedFile emits one raw artifact for a changed file. It returns false
// only when the consumer has gone away (so the caller should stop emitting);
// unreadable or oversized files are skipped (returns true).
func emitChangedFile(
	ec *a2asrv.ExecutorContext, dir, rel, artifactName, media string, maxBytes int64,
	emit func(a2a.Event) bool,
) bool {
	data, ok := readChangedFile(dir, rel, maxBytes)
	if !ok {
		return true
	}
	part := a2a.NewRawPart(data)
	part.MediaType = media
	part.Filename = filepath.Base(rel)
	evt := a2a.NewArtifactEvent(ec, part)
	evt.Artifact.Name = artifactName
	evt.Artifact.Metadata = map[string]any{"path": rel}
	return emit(evt)
}

// readChangedFile reads a changed file inside dir when rel is a safe relative
// path to a regular file within maxBytes.
func readChangedFile(dir, rel string, maxBytes int64) ([]byte, bool) {
	if filepath.IsAbs(rel) || strings.HasPrefix(filepath.Clean(rel), "..") {
		return nil, false
	}
	path := filepath.Join(dir, rel)
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxBytes {
		return nil, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	return data, true
}

func toAnySlice(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
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
		e.forget(taskID)
		e.breaker.Resolve()
		emit(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCanceled, nil), nil)
	}
}

// toRequest maps the A2A request onto an agent.Request, merging message-level
// and request-level metadata (request-level wins).
func (e *CLIExecutor) toRequest(ec *a2asrv.ExecutorContext) (*agent.Request, error) {
	meta := mergeMetadata(ec)
	dir, err := e.workspace.Resolve(contextKey(ec), metaString(meta, "harbor.workDir"))
	if err != nil {
		return nil, err
	}
	in, err := e.promptFromParts(ec.Message, dir)
	if err != nil {
		return nil, err
	}
	blocks, images, files := in.Blocks, in.Images, in.Files
	if len(blocks) == 0 && len(images) == 0 && len(files) == 0 && len(in.ImageFiles) == 0 {
		return nil, errNoText
	}
	sid, _ := e.sessions.Get(contextKey(ec))
	return &agent.Request{
		Prompt:       strings.Join(blocks, "\n"),
		Blocks:       blocks,
		SystemExtra:  metaString(meta, "harbor.systemPrompt"),
		Model:        metaString(meta, "harbor.model"),
		Agent:        metaString(meta, "harbor.agent"),
		WorkDir:      dir,
		SessionID:    sid,
		Continue:     sid == "" && metaBool(meta, "harbor.continue"),
		Fork:         metaBool(meta, "harbor.fork"),
		MaxTurns:     metaInt(meta, "harbor.maxTurns"),
		OutputSchema: metaMap(meta, "harbor.outputSchema"),
		Images:       images,
		Files:        files,
		ImageFiles:   in.ImageFiles,
		Meta:         meta,
	}, nil
}

// contextKey scopes a context id to its owner, so one caller cannot resume
// another caller's CLI session or working tree by reusing a contextId. A NUL
// separator keeps the user and context parts unambiguous.
func contextKey(ec *a2asrv.ExecutorContext) string {
	if ec.User == nil || ec.User.Name == "" {
		return ec.ContextID
	}
	return ec.User.Name + "\x00" + ec.ContextID
}

func isWaitingForInput(ec *a2asrv.ExecutorContext) bool {
	return ec.StoredTask != nil && ec.StoredTask.Status.State == a2a.TaskStateInputRequired
}

// answerWithoutRunning reports whether a request is answered from existing
// state instead of starting a CLI run: a repeated message returns the earlier
// task, and a message to a finished task returns it unchanged.
func (e *CLIExecutor) answerWithoutRunning(
	ctx context.Context, ec *a2asrv.ExecutorContext, yield func(a2a.Event, error) bool,
) bool {
	if orig, ok := e.duplicateTask(ctx, ec); ok {
		e.replayDuplicate(ec, orig, yield)
		return true
	}
	if ec.StoredTask != nil && isTerminalState(ec.StoredTask.Status.State) {
		yield(ec.StoredTask, nil)
		return true
	}
	return false
}

// isTerminalState reports whether a task state is final (no further execution).
func isTerminalState(s a2a.TaskState) bool {
	switch s {
	case a2a.TaskStateCompleted, a2a.TaskStateFailed, a2a.TaskStateCanceled, a2a.TaskStateRejected:
		return true
	}
	return false
}

// duplicateTask returns the earlier task for a repeated messageId, so a
// duplicate send is not executed twice. The first occurrence records the mapping.
func (e *CLIExecutor) duplicateTask(ctx context.Context, ec *a2asrv.ExecutorContext) (*a2a.Task, bool) {
	if e.taskLookup == nil || ec.StoredTask != nil || ec.Message == nil || ec.Message.ID == "" {
		return nil, false
	}
	prev, existed := e.idem.getOrPut(ec.Message.ID, ec.TaskID)
	if !existed || prev == ec.TaskID {
		return nil, false
	}
	task, ok := e.taskLookup(ctx, prev)
	if !ok || task == nil || task.Status.State == a2a.TaskStateRejected {
		// The earlier attempt was rejected (e.g. the breaker was open); do not
		// pin that transient result, let this attempt proceed.
		e.idem.set(ec.Message.ID, ec.TaskID)
		return nil, false
	}
	return task, true
}

// replayDuplicate answers a duplicate send by replaying the earlier task's
// artifacts and state under the new task, so the CLI is not run twice. The new
// task records the original as harbor.duplicateOf.
func (e *CLIExecutor) replayDuplicate(
	ec *a2asrv.ExecutorContext, orig *a2a.Task, yield func(a2a.Event, error) bool,
) {
	tk := a2a.NewSubmittedTask(ec, ec.Message)
	if tk.Metadata == nil {
		tk.Metadata = map[string]any{}
	}
	tk.Metadata["harbor.duplicateOf"] = string(orig.ID)
	if !yield(tk, nil) {
		return
	}
	for _, a := range orig.Artifacts {
		evt := a2a.NewArtifactEvent(ec, a.Parts...)
		evt.Artifact.Name = a.Name
		evt.Artifact.Metadata = a.Metadata
		if !yield(evt, nil) {
			return
		}
	}
	state := orig.Status.State
	if state == "" || state == a2a.TaskStateSubmitted {
		state = a2a.TaskStateCompleted
	}
	yield(a2a.NewStatusUpdateEvent(ec, state, orig.Status.Message), nil)
}

func codeForRequestError(err error) string {
	switch {
	case errors.Is(err, errNoText), errors.Is(err, errUnsupportedContent):
		return codeUnsupportedContent
	case errors.Is(err, workspace.ErrDenied):
		return codeWorkdirDenied
	default:
		return codeInternal
	}
}

func codeForStartError(err error) string {
	switch {
	case errors.Is(err, agent.ErrCLINotFound):
		return codeCLINotFound
	default:
		return codeInternal
	}
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
func interactionMessage(prompt string, itx *agent.Interaction, sessionID string) *a2a.Message {
	m := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(prompt))
	m.Parts = append(m.Parts, a2a.NewDataPart(interactionPayload(itx)))
	m.Metadata = map[string]any{
		metaKeyKind:          string(itx.Kind),
		metaKeyInteractionID: itx.ID,
	}
	// Record the CLI session so a paused task can be resumed after a restart.
	if sessionID != "" {
		m.Metadata[metaKeySessionID] = sessionID
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
func (e *CLIExecutor) answerFromMessage(
	ec *a2asrv.ExecutorContext, taskID string, m *a2a.Message,
) (agent.Input, error) {
	id, answer, content := parseAnswer(m)
	kind, action, target := "answer", "", ""
	if cur, ok := e.pendingInteraction(ec, taskID); ok {
		if id != "" && id != cur.ID {
			return agent.Input{}, fmt.Errorf("interaction id mismatch: got %q, want %q", id, cur.ID)
		}
		// Use the interaction's own id, kind, action, and target so the session
		// can build the right response even when the client answered with text.
		id, kind, action, target = cur.ID, string(cur.Kind), cur.Action, cur.Target
	}
	return agent.Input{
		Kind: kind, ID: id, Action: action, Target: target, Answer: answer, Content: content,
	}, nil
}

// parseAnswer extracts the interaction id, answer text, and form content from a
// client reply. It prefers a structured DataPart and falls back to the message
// metadata (key "harbor.interactionId") plus the first text part.
func parseAnswer(m *a2a.Message) (id, answer string, content map[string]any) {
	if m == nil {
		return "", "", nil
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
		if a.ID != "" || a.Answer != "" || a.Content != nil {
			return a.ID, a.Answer, a.Content
		}
	}
	return metaString(m.Metadata, metaKeyInteractionID), firstTextPart(m), nil
}

// pendingInteraction returns the input request a paused task is waiting on. It
// falls back to the interaction recorded in the persisted input-required status
// message, so a paused task remains answerable across a restart (when a durable
// task store is configured).
func (e *CLIExecutor) pendingInteraction(ec *a2asrv.ExecutorContext, taskID string) (*agent.Interaction, bool) {
	if v, ok := e.interactions.Load(taskID); ok {
		return v.(*agent.Interaction), true
	}
	if ec.StoredTask != nil {
		if itx, ok := interactionFromStatusMessage(ec.StoredTask.Status.Message); ok {
			return itx, true
		}
	}
	return nil, false
}

// interactionFromStatusMessage reconstructs the interaction carried by an
// input-required status message.
func interactionFromStatusMessage(m *a2a.Message) (*agent.Interaction, bool) {
	if m == nil {
		return nil, false
	}
	for _, p := range m.Parts {
		if p == nil || p.Data() == nil {
			continue
		}
		b, err := json.Marshal(p.Data())
		if err != nil {
			continue
		}
		var itx agent.Interaction
		if err := json.Unmarshal(b, &itx); err == nil && itx.ID != "" {
			return &itx, true
		}
	}
	return nil, false
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

// metaMap returns a nested JSON object under key, or nil when absent.
func metaMap(m map[string]any, key string) map[string]any {
	v, _ := m[key].(map[string]any)
	return v
}
