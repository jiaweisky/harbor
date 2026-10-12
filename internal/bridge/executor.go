package bridge

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"net/http"
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
	"github.com/jiaweisky/harbor/internal/blob"
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
	codeTimeout            = "timeout"
	codeMaxTurns           = "max_turns"
	codeAgentUnavailable   = "agent_unavailable"
	// codeInteractionUnexpected marks a permission prompt from a non-interactive
	// agent whose CLI was not configured to bypass permissions.
	codeInteractionUnexpected = "interaction_unexpected"
	// codeStorageError marks a data-plane read or write failure.
	codeStorageError = "storage_error"
)

// Metadata keys carried on task status messages. The interaction id is both
// written on an input-required prompt and read back from a client's answer.
const (
	metaKeyKind          = "harbor.kind"
	metaKeyErrorCode     = "harbor.errorCode"
	metaKeyInteractionID = "harbor.interactionId"
	metaKeySessionID     = "harbor.sessionId"
)

// DataPart kinds for the broker exchange: a sender reserves an upload slot and
// Harbor returns a presigned target.
const (
	kindUploadSlotRequest = "storage/upload-slot-request"
	kindUploadSlot        = "storage/upload-slot"
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
	launcher        agent.Launcher
	sessions        SessionStore
	live            *agent.LiveSessions
	workspace       *workspace.Resolver
	timeout         time.Duration // total task timeout; 0 disables
	idleTimeout     time.Duration // fail a task with no output for this long; 0 disables
	keepalive       time.Duration // emit a keepalive when quiet for this long; 0 disables
	breaker         *breaker      // nil disables
	tasks           sync.Map      // taskID -> *taskState (all per-task transient state)
	maxTurns        int
	fileArtifacts   bool          // emit a per-file artifact for each changed file
	store           blob.Store    // file data plane; nil disables it
	uploadClient    *http.Client  // sender-side client; nil uses the default
	inboundMedia    string        // spool | inline
	inlineMax       int64         // sender inline threshold; 0 disables reference uploads
	maxBytes        int64         // largest accepted reference (broker)
	maxArtifactRead int64         // largest file inlined as an artifact; 0 uses the default
	mediaTypes      []string      // allowed ingest media types
	downloadTO      time.Duration // bound on reading a reference; 0 disables
	taskLookup      func(context.Context, a2a.TaskID) (*a2a.Task, bool)
	idem            *idemCache // messageId -> taskID (duplicate-send guard)
	metrics         *agentMetrics
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
// explicit structured-output event (exec CLIs). It is touched only by the task's
// own goroutine.
type structuredState struct{ emitted bool }

// taskState is the transient state of one task execution. A single entry in
// CLIExecutor.tasks owns everything a task holds while it runs: its process
// lifetime (ctx/cancel), the workdir lock, the change baseline, the turn count,
// the structured-output flag, and any pending interaction or paused-task reaper.
// forget releases the entry and everything in it together.
type taskState struct {
	// ctx is detached from the RPC context so a task paused on input-required
	// keeps its process alive after the request returns; it is canceled on a
	// terminal state, Cancel, or the configured task timeout.
	ctx     context.Context
	cancel  context.CancelFunc
	started time.Time

	turns atomic.Int64

	// mu guards the fields below, which are touched from more than one
	// goroutine (the task's own goroutine, Cancel, and the paused-task reaper).
	mu          sync.Mutex
	lock        func()
	baseline    *changes.Baseline
	structured  *structuredState
	interaction *agent.Interaction
	reap        context.CancelFunc
}

func (s *taskState) setStructured(st *structuredState) {
	s.mu.Lock()
	s.structured = st
	s.mu.Unlock()
}

func (s *taskState) structuredState() *structuredState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.structured
}

func (s *taskState) setBaseline(b changes.Baseline) {
	s.mu.Lock()
	s.baseline = &b
	s.mu.Unlock()
}

// takeBaseline returns the change baseline once, then clears it so a second
// call does not re-emit the same changes.
func (s *taskState) takeBaseline() (changes.Baseline, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.baseline == nil {
		return changes.Baseline{}, false
	}
	b := *s.baseline
	s.baseline = nil
	return b, true
}

func (s *taskState) setLock(release func()) {
	if release == nil {
		return
	}
	s.mu.Lock()
	s.lock = release
	s.mu.Unlock()
}

func (s *taskState) takeLock() func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.lock
	s.lock = nil
	return f
}

func (s *taskState) setInteraction(it *agent.Interaction) {
	s.mu.Lock()
	s.interaction = it
	s.mu.Unlock()
}

func (s *taskState) takeInteraction() {
	s.mu.Lock()
	s.interaction = nil
	s.mu.Unlock()
}

func (s *taskState) pendingInteraction() *agent.Interaction {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.interaction
}

func (s *taskState) setReap(cancel context.CancelFunc) {
	s.mu.Lock()
	s.reap = cancel
	s.mu.Unlock()
}

func (s *taskState) takeReap() context.CancelFunc {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.reap
	s.reap = nil
	return c
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

// SetStore installs the file data-plane store; nil disables it.
func (e *CLIExecutor) SetStore(s blob.Store) { e.store = s }

// SetUploadClient sets the HTTP client used to reach a consumer's upload target.
func (e *CLIExecutor) SetUploadClient(c *http.Client) { e.uploadClient = c }

// SetInboundMedia selects how inbound attachments reach the model (spool|inline).
func (e *CLIExecutor) SetInboundMedia(mode string) { e.inboundMedia = mode }

// SetInlineMaxBytes sets the sender's inline threshold: an outbound artifact
// larger than this is uploaded to the consumer's ingest target (when one is
// supplied) instead of inlined. 0 keeps the legacy fixed caps.
func (e *CLIExecutor) SetInlineMaxBytes(n int64) { e.inlineMax = n }

// SetStoragePolicy sets the ingest bounds the broker enforces.
func (e *CLIExecutor) SetStoragePolicy(maxBytes int64, mediaTypes []string) {
	e.maxBytes = maxBytes
	e.mediaTypes = mediaTypes
}

// SetDownloadTimeout bounds reading a reference; 0 disables it.
func (e *CLIExecutor) SetDownloadTimeout(d time.Duration) { e.downloadTO = d }

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
		if e.handleStorageRequest(ctx, ec, yield) {
			return
		}
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
		st.setBaseline(changes.Snapshot(req.WorkDir))
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

// resume answers a task paused on input-required by writing to its live session.
func (e *CLIExecutor) resume(
	ctx context.Context, ec *a2asrv.ExecutorContext, taskID string,
	yield func(a2a.Event, error) bool,
) {
	st := e.loadState(taskID)
	if st != nil {
		e.disarmPauseReaper(st)
		if e.takeTurn(st) {
			e.stopPausedTask(taskID)
			yield(failedStatus(ec, codeMaxTurns, "max turns exceeded"), nil)
			return
		}
		if err := st.ctx.Err(); err != nil {
			// The process already ended (typically the task timed out while paused).
			e.cleanup(taskID, true)
			yield(failedStatus(ec, codeForRunError(err), err.Error()), nil)
			return
		}
	}
	sess, ok := e.live.Get(taskID)
	if st == nil || !ok {
		// No live process (for example after a restart): drop any stale state
		// and resume the CLI's recorded session instead.
		e.forget(taskID)
		e.resumeFresh(ctx, ec, taskID, yield)
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
	st.takeInteraction()
	if err := sess.Send(st.ctx, in); err != nil {
		e.cleanup(taskID, true)
		yield(failedStatus(ec, codeInternal, err.Error()), nil)
		return
	}
	if !yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateWorking, nil), nil) {
		e.teardown(taskID, sess)
		return
	}
	e.watchExecution(ctx, taskID)
	e.pump(st, ec, taskID, sess, yield)
}

// stopPausedTask tears down a paused task's live process, or releases its state
// when no live process remains.
func (e *CLIExecutor) stopPausedTask(taskID string) {
	if sess, ok := e.live.Get(taskID); ok {
		e.teardown(taskID, sess)
		return
	}
	e.cleanup(taskID, false)
}

// resumeFresh recovers a task paused on input-required whose live process is gone
// (for example after a Harbor restart): it resumes the CLI's recorded session and
// injects the client's answer as the next prompt. The injection is best-effort
// (the CLI restarts and may replay context) and requires a recorded session id
// and a resume-capable CLI.
func (e *CLIExecutor) resumeFresh(
	ctx context.Context, ec *a2asrv.ExecutorContext, taskID string, yield func(a2a.Event, error) bool,
) {
	sid := e.sessionFromTask(ec)
	if sid == "" || !e.launcher.Capabilities().SessionResume {
		e.forget(taskID)
		yield(failedStatus(ec, codeInternal, "session lost"), nil)
		return
	}
	in, err := e.answerFromMessage(ec, taskID, ec.Message)
	if err != nil || strings.TrimSpace(in.Answer) == "" {
		e.forget(taskID)
		yield(failedStatus(ec, codeInternal, "session lost"), nil)
		return
	}
	dir, err := e.workspace.Resolve(contextKey(ec))
	if err != nil {
		e.forget(taskID)
		yield(failedStatus(ec, codeForRequestError(err), err.Error()), nil)
		return
	}
	st := e.startTask(ctx, taskID)
	e.launchAndPump(ctx, st, ec, taskID,
		&agent.Request{Prompt: in.Answer, SessionID: sid, WorkDir: dir}, true, yield)
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

// watchExecution stops the task's CLI when the server aborts the execution with
// the agent inactivity timeout. A normal return (the request completed, e.g. a
// pause, which also cancels the context) must not stop the CLI, so the client
// can still resume it; a client cancel is handled by Cancel.
func (e *CLIExecutor) watchExecution(parent context.Context, taskID string) {
	if parent.Done() == nil {
		return
	}
	go func() {
		<-parent.Done()
		if !errors.Is(context.Cause(parent), a2asrv.ErrAgentInactivityTimeout) {
			return
		}
		if sess, ok := e.live.Get(taskID); ok {
			e.teardown(taskID, sess)
		}
	}()
}

// forget releases a task's state: its process lifetime, workdir lock, change
// baseline, pending interaction, paused-task reaper, and live session. It is the
// single release path and is idempotent; it does not touch the failure breaker.
func (e *CLIExecutor) forget(taskID string) {
	v, ok := e.tasks.LoadAndDelete(taskID)
	if !ok {
		return
	}
	st := v.(*taskState)
	st.cancel()
	if c := st.takeReap(); c != nil {
		c()
	}
	if f := st.takeLock(); f != nil {
		f()
	}
	e.live.Del(taskID)
	e.metrics.active.Add(-1)
	telemetry.TaskActive(-1)
}

// armPauseReaper reaps a task paused on input-required if it is not answered
// before idleTimeout elapses or its run deadline passes. Without it a paused
// task holds its CLI process and workdir lock until the client answers.
func (e *CLIExecutor) armPauseReaper(st *taskState, taskID string, sess agent.Session) {
	ctx, cancel := context.WithCancel(context.Background())
	st.setReap(cancel)
	go func() {
		defer cancel()
		var timeoutC <-chan time.Time
		if e.idleTimeout > 0 {
			t := time.NewTimer(e.idleTimeout)
			defer t.Stop()
			timeoutC = t.C
		}
		select {
		case <-st.ctx.Done():
		case <-timeoutC:
		case <-ctx.Done():
			return
		}
		e.teardown(taskID, sess)
	}()
}

// disarmPauseReaper stops a paused task's reaper, called when it resumes. It is
// idempotent.
func (e *CLIExecutor) disarmPauseReaper(st *taskState) {
	if c := st.takeReap(); c != nil {
		c()
	}
}

// takeTurn counts a turn for the task (a start or a resume) and reports
// whether it exceeds the configured cap. A cap of 0 disables the check.
func (e *CLIExecutor) takeTurn(st *taskState) bool {
	if e.maxTurns <= 0 {
		return false
	}
	return int(st.turns.Add(1)) > e.maxTurns
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

// emitChanges captures the task's working-tree changes and emits them as a
// "changes" artifact. It is a no-op when there is no baseline or no diff.
func (e *CLIExecutor) emitChanges(ec *a2asrv.ExecutorContext, taskID string, emit func(a2a.Event) bool) {
	st := e.loadState(taskID)
	if st == nil {
		return
	}
	base, ok := st.takeBaseline()
	if !ok {
		return
	}
	dir, err := e.workspace.Resolve(contextKey(ec))
	if err != nil || dir == "" {
		return
	}
	res := changes.Capture(dir, base, maxDiffReadBytes)
	// Surface changed image files (for example a text-to-image result) as raw
	// image artifacts, alongside the textual diff.
	e.emitImageArtifacts(ec, dir, res.Files, emit)
	if e.fileArtifacts {
		e.emitFileArtifacts(ec, dir, res.Files, emit)
	}
	if res.Diff == "" {
		return
	}
	e.emitDiff(ec, res, emit)
}

// emitDiff delivers the change diff: uploaded and referenced when it exceeds the
// inline threshold and a consumer target is available, else inlined as text.
func (e *CLIExecutor) emitDiff(ec *a2asrv.ExecutorContext, res changes.Result, emit func(a2a.Event) bool) {
	data := []byte(res.Diff)
	var part *a2a.Part
	if ref := e.uploadArtifact(ec, "changes.diff", "text/x-diff", data); ref != "" {
		part = a2a.NewFileURLPart(a2a.URL(ref), "text/x-diff")
		part.Filename = "changes.diff"
	} else {
		part = a2a.NewTextPart(res.Diff)
		part.MediaType = "text/x-diff"
	}
	part.Metadata = digestMeta(data)
	evt := a2a.NewArtifactEvent(ec, part)
	evt.Artifact.Name = "changes"
	evt.Artifact.Metadata = map[string]any{"files": toAnySlice(res.Files), "truncated": res.Truncated}
	emit(evt)
}

// defaultArtifactReadLimit is the default bound on how much of a single changed
// file is read into memory before it is inlined or uploaded.
const defaultArtifactReadLimit = 256 << 20

// maxArtifactBytes is the effective inline-read bound for this executor.
func (e *CLIExecutor) artifactReadLimit() int64 {
	if e.maxArtifactRead > 0 {
		return e.maxArtifactRead
	}
	return defaultArtifactReadLimit
}

// maxDiffReadBytes bounds the captured diff before it is uploaded or truncated.
const maxDiffReadBytes = 32 << 20

// imageMediaTypes maps a lower-case file extension to its image MIME type. It
// covers the formats the supported CLIs produce.
var imageMediaTypes = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".gif": "image/gif", ".webp": "image/webp",
}

// textMediaTypes maps a lower-case file extension to a text media type; unknown
// extensions fall back to application/octet-stream.
var textMediaTypes = map[string]string{
	".txt": "text/plain", ".md": "text/markdown", ".json": "application/json",
	".yaml": "application/yaml", ".yml": "application/yaml", ".xml": "application/xml",
	".csv": "text/csv", ".diff": "text/x-diff", ".patch": "text/x-diff",
	".go": "text/plain", ".py": "text/plain", ".js": "text/plain", ".ts": "text/plain",
	".sh": "text/plain", ".html": "text/html", ".css": "text/css",
}

// emitImageArtifacts emits one image artifact per changed image file in the
// task's working directory (inlined, or a URL reference when uploaded), so a
// generated image reaches the client as an image part rather than a diff line.
func (e *CLIExecutor) emitImageArtifacts(
	ec *a2asrv.ExecutorContext, dir string, files []string, emit func(a2a.Event) bool,
) {
	for _, rel := range files {
		media, ok := imageMediaTypes[strings.ToLower(filepath.Ext(rel))]
		if !ok {
			continue
		}
		if !e.emitChangedFile(ec, dir, rel, "image", media, emit) {
			return
		}
	}
}

// emitFileArtifacts emits one file artifact per changed non-image file (inlined,
// or a URL reference when uploaded), so a client receives the produced files,
// not only a diff.
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
		if !e.emitChangedFile(ec, dir, rel, "file", media, emit) {
			return
		}
	}
}

// uploadOutcome reports what uploadFile did with a changed file.
type uploadOutcome int

const (
	fileInlined      uploadOutcome = iota // no target, or within the inline bound
	fileUploaded                          // uploaded and referenced
	fileUploadFailed                      // an upload was attempted and failed
)

// emitChangedFile emits one artifact for a changed file. A file larger than the
// inline threshold is uploaded to the consumer's ingest target (when one was
// supplied) and referenced; otherwise it is inlined as raw bytes. A file that
// could neither be uploaded nor inlined is reported with a storage_error rather
// than dropped. It returns false only when the consumer has gone away.
func (e *CLIExecutor) emitChangedFile(
	ec *a2asrv.ExecutorContext, dir, rel, artifactName, media string,
	emit func(a2a.Event) bool,
) bool {
	// Prefer a streaming upload (the file never enters memory); otherwise inline
	// a bounded read of it.
	ref, sha, size, outcome := e.uploadFile(ec, dir, rel, media)
	if outcome == fileUploaded {
		part := a2a.NewFileURLPart(a2a.URL(ref), media)
		part.Filename = filepath.Base(rel)
		part.Metadata = map[string]any{"sha256": sha, "size": size}
		evt := a2a.NewArtifactEvent(ec, part)
		evt.Artifact.Name = artifactName
		evt.Artifact.Metadata = map[string]any{"path": rel}
		return emit(evt)
	}
	data, ok := readChangedFile(dir, rel, e.artifactReadLimit())
	if !ok {
		if outcome == fileUploadFailed {
			return emit(e.storageErrorArtifact(ec, rel, artifactName))
		}
		return true
	}
	return emit(e.fileArtifact(ec, rel, artifactName, media, data))
}

// storageErrorArtifact reports a file that was neither uploaded nor inlined, so
// the client learns which file was dropped. The task still completes.
func (e *CLIExecutor) storageErrorArtifact(
	ec *a2asrv.ExecutorContext, rel, artifactName string,
) *a2a.TaskArtifactUpdateEvent {
	part := a2a.NewTextPart(fmt.Sprintf("[file: %s] could not be transferred to storage", filepath.Base(rel)))
	evt := a2a.NewArtifactEvent(ec, part)
	evt.Artifact.Name = artifactName
	evt.Artifact.Metadata = map[string]any{"path": rel, "storage_error": true}
	return evt
}

// uploadFile streams a changed file to the consumer's ingest target when it
// exceeds the inline threshold and a target was supplied. It returns the
// reference, content hash, and size when uploaded; otherwise the outcome tells
// the caller whether to inline.
func (e *CLIExecutor) uploadFile(
	ec *a2asrv.ExecutorContext, dir, rel, media string,
) (ref, sha string, size int64, outcome uploadOutcome) {
	path, err := safePath(dir, rel)
	if err != nil {
		return "", "", 0, fileInlined
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", "", 0, fileInlined
	}
	t, limit, ok := e.uploadTarget(ec)
	if !ok || limit <= 0 || info.Size() <= limit {
		return "", "", 0, fileInlined
	}
	sha, err = hashFile(path)
	if err != nil {
		return "", "", 0, fileInlined
	}
	f, err := os.Open(path)
	if err != nil {
		return "", "", 0, fileInlined
	}
	defer f.Close()
	ref, ok = e.putToTarget(t, media, applyKeyRule(t.keyRule, sha), f, info.Size())
	if !ok {
		return "", "", 0, fileUploadFailed
	}
	return ref, sha, info.Size(), fileUploaded
}

// fileArtifact builds the inline artifact for a changed file.
func (e *CLIExecutor) fileArtifact(
	ec *a2asrv.ExecutorContext, rel, artifactName, media string, data []byte,
) *a2a.TaskArtifactUpdateEvent {
	part := a2a.NewRawPart(data)
	part.MediaType = media
	part.Filename = filepath.Base(rel)
	part.Metadata = digestMeta(data)
	evt := a2a.NewArtifactEvent(ec, part)
	evt.Artifact.Name = artifactName
	evt.Artifact.Metadata = map[string]any{"path": rel}
	return evt
}

// digestMeta is the integrity metadata carried on a file part.
func digestMeta(data []byte) map[string]any {
	return map[string]any{"sha256": sha256Hex(data), "size": len(data)}
}

// sha256Hex is the lowercase hex SHA-256 of data.
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// hashFile streams path and returns its hex SHA-256.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// uploadTarget returns the consumer's ingest target and the effective inline
// limit, reporting false when no target was supplied.
func (e *CLIExecutor) uploadTarget(ec *a2asrv.ExecutorContext) (consumerIngest, int64, bool) {
	t, ok := ingestTarget(ec)
	if !ok {
		return consumerIngest{}, 0, false
	}
	return t, e.inlineLimit(t), true
}

// uploadArtifact uploads in-memory data (the diff) to the consumer's ingest
// target when it exceeds the inline threshold, returning the reference or "".
func (e *CLIExecutor) uploadArtifact(ec *a2asrv.ExecutorContext, rel, media string, data []byte) string {
	t, limit, ok := e.uploadTarget(ec)
	if !ok || limit <= 0 || int64(len(data)) <= limit {
		return ""
	}
	ref, _ := e.putToTarget(t, media, applyKeyRule(t.keyRule, sha256Hex(data)), bytes.NewReader(data), int64(len(data)))
	return ref
}

// putToTarget uploads r to the target under key and returns the reference.
func (e *CLIExecutor) putToTarget(t consumerIngest, media, key string, r io.Reader, size int64) (string, bool) {
	target := blob.UploadTarget{URL: strings.TrimRight(t.base, "/") + "/" + key, Method: t.method}
	if media != "" {
		target.Headers = map[string]string{"Content-Type": media}
	}
	ref, err := blob.Upload(context.Background(), e.uploadClient, target, r, size)
	if err != nil {
		slog.Warn("artifact upload failed; inlining instead", "key", key, "err", err)
		return "", false
	}
	if ref == "" {
		// The receiver reported no reference (a plain object store), so form it
		// from the advertised read base and key.
		ref = strings.TrimRight(t.readBase(), "/") + "/" + key
	}
	return ref, true
}

// inlineLimit is the effective inline threshold: the local default, capped by
// the consumer's advertised maximum when it is smaller.
func (e *CLIExecutor) inlineLimit(t consumerIngest) int64 {
	limit := e.inlineMax
	if t.maxInline > 0 && (limit <= 0 || t.maxInline < limit) {
		limit = t.maxInline
	}
	return limit
}

// safePath joins dir and rel, rejecting absolute paths and traversal.
func safePath(dir, rel string) (string, error) {
	if filepath.IsAbs(rel) || strings.HasPrefix(filepath.Clean(rel), "..") {
		return "", fmt.Errorf("unsafe path %q", rel)
	}
	return filepath.Join(dir, rel), nil
}

// consumerIngest is a caller-supplied target for response files.
type consumerIngest struct {
	base      string
	method    string
	keyRule   string
	reference string // read base; empty means base
	maxInline int64  // consumer's max inline size; 0 means unspecified
}

// ingestTarget reads the caller's ingest target from the request metadata
// (harbor.ingest = {base, method, key, reference, maxInlineBytes}).
func ingestTarget(ec *a2asrv.ExecutorContext) (consumerIngest, bool) {
	v, ok := mergeMetadata(ec)["harbor.ingest"].(map[string]any)
	if !ok {
		return consumerIngest{}, false
	}
	base, _ := v["base"].(string)
	if base == "" {
		return consumerIngest{}, false
	}
	method, _ := v["method"].(string)
	key, _ := v["key"].(string)
	ref, _ := v["reference"].(string)
	return consumerIngest{
		base: base, method: method, keyRule: key, reference: ref,
		maxInline: int64Field(v, "maxInlineBytes"),
	}, true
}

// readBase returns the base the reference is read from.
func (c consumerIngest) readBase() string {
	if c.reference != "" {
		return c.reference
	}
	return c.base
}

// applyKeyRule renders the object key from the consumer's rule and a content
// hash. An empty or {sha256} rule uses the hash; any other rule is a prefix that
// also carries the hash, so distinct files never collide.
func applyKeyRule(rule, hash string) string {
	switch {
	case rule == "":
		return hash
	case strings.Contains(rule, "{sha256}"):
		return strings.ReplaceAll(rule, "{sha256}", hash)
	default:
		return strings.Trim(rule, "/") + "/" + hash
	}
}

// readChangedFile reads a changed file inside dir when rel is a safe relative
// path to a regular file within maxBytes.
func readChangedFile(dir, rel string, maxBytes int64) ([]byte, bool) {
	path, err := safePath(dir, rel)
	if err != nil {
		return nil, false
	}
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
		e.breaker.Resolve()
		emit(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCanceled, nil), nil)
		e.forget(taskID)
	}
}

// toRequest maps the A2A request onto an agent.Request, merging message-level
// and request-level metadata (request-level wins).
func (e *CLIExecutor) toRequest(ctx context.Context, ec *a2asrv.ExecutorContext) (*agent.Request, error) {
	meta := mergeMetadata(ec)
	dir, err := e.workspace.Resolve(contextKey(ec))
	if err != nil {
		return nil, err
	}
	attach := filepath.Join(dir, ".a2a", "attachments", turnDir(ec))
	in, err := e.promptFromParts(ctx, ec.Message, attach)
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

// turnDir is the per-turn subdirectory inbound attachments are materialized
// into, derived from the task id so turns do not overwrite each other in the
// shared workspace.
func turnDir(ec *a2asrv.ExecutorContext) string {
	id := filepath.Base(string(ec.TaskID))
	if id == "" || id == "." || id == ".." || id == string(filepath.Separator) {
		return "turn"
	}
	return id
}

func isWaitingForInput(ec *a2asrv.ExecutorContext) bool {
	return ec.StoredTask != nil && ec.StoredTask.Status.State == a2a.TaskStateInputRequired
}

// handleStorageRequest answers a broker request (a storage/upload-slot-request
// DataPart) with a presigned target, without starting a CLI. It reports whether
// the message was a storage request.
func (e *CLIExecutor) handleStorageRequest(
	ctx context.Context, ec *a2asrv.ExecutorContext, yield func(a2a.Event, error) bool,
) bool {
	req := storageDataPart(ec.Message, kindUploadSlotRequest)
	if req == nil {
		return false
	}
	if !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
		return true
	}
	slot, err := e.reserve(ctx, req)
	if err != nil {
		yield(failedStatus(ec, codeUnsupportedContent, err.Error()), nil)
		return true
	}
	evt := a2a.NewArtifactEvent(ec, a2a.NewDataPart(slot))
	evt.Artifact.Name = "upload-slot"
	if !yield(evt, nil) {
		return true
	}
	yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, nil), nil)
	return true
}

// reserve validates a broker request and returns the upload-slot payload.
func (e *CLIExecutor) reserve(ctx context.Context, req map[string]any) (map[string]any, error) {
	if e.store == nil {
		return nil, errors.New("storage is not configured")
	}
	name := stringField(req, "name")
	mediaType := mediaTypeFor(name, stringField(req, "mediaType"))
	size := int64Field(req, "size")
	if e.maxBytes > 0 && size > e.maxBytes {
		return nil, fmt.Errorf("file size %d exceeds the limit %d", size, e.maxBytes)
	}
	if !mediaAllowed(e.mediaTypes, mediaType) {
		return nil, fmt.Errorf("media type %q is not accepted", mediaType)
	}
	t, err := e.store.Reserve(ctx, name, mediaType, size)
	if err != nil {
		return nil, err
	}
	slot := map[string]any{
		"kind":      kindUploadSlot,
		"uploadUrl": t.URL,
		"method":    t.Method,
		"reference": t.Ref(),
	}
	if len(t.Headers) > 0 {
		slot["headers"] = t.Headers
	}
	if !t.Expires.IsZero() {
		slot["expiresAt"] = t.Expires.UTC().Format(time.RFC3339)
	}
	return slot, nil
}

// storageDataPart returns the DataPart value carrying the given kind, if any.
func storageDataPart(m *a2a.Message, kind string) map[string]any {
	if m == nil {
		return nil
	}
	for _, p := range m.Parts {
		d, ok := p.Data().(map[string]any)
		if !ok {
			continue
		}
		if k, _ := d["kind"].(string); k == kind {
			return d
		}
	}
	return nil
}

func stringField(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

// mediaAllowed reports whether mime matches any allowlist pattern, where a
// pattern may be an exact type, a subtype wildcard ("text/*"), or "*/*".
func mediaAllowed(patterns []string, mime string) bool {
	mime = strings.ToLower(strings.TrimSpace(mime))
	if mime == "" {
		return false
	}
	for _, p := range patterns {
		p = strings.ToLower(strings.TrimSpace(p))
		switch {
		case p == "*/*" || p == mime:
			return true
		case strings.HasSuffix(p, "/*") && strings.HasPrefix(mime, strings.TrimSuffix(p, "*")):
			return true
		}
	}
	return false
}

// int64Field reads a numeric field that may decode as int, float64, or
// json.Number.
func int64Field(m map[string]any, key string) int64 {
	switch v := m[key].(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		return int64(v)
	case json.Number:
		n, _ := v.Int64()
		return n
	}
	return 0
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
// The mapping is scoped by caller so one caller cannot replay another's task by
// reusing a message id; a context id is not part of the scope because the server
// may assign a fresh one per request.
func (e *CLIExecutor) duplicateTask(ctx context.Context, ec *a2asrv.ExecutorContext) (*a2a.Task, bool) {
	if e.taskLookup == nil || ec.StoredTask != nil || ec.Message == nil || ec.Message.ID == "" {
		return nil, false
	}
	key := callerScope(ec) + "\x00" + ec.Message.ID
	prev, existed := e.idem.getOrPut(key, ec.TaskID)
	if !existed || prev == ec.TaskID {
		return nil, false
	}
	task, ok := e.taskLookup(ctx, prev)
	if !ok || task == nil || task.Status.State == a2a.TaskStateRejected {
		// The earlier attempt was rejected (e.g. the breaker was open); do not
		// pin that transient result, let this attempt proceed.
		e.idem.set(key, ec.TaskID)
		return nil, false
	}
	return task, true
}

// callerScope returns the authenticated caller's identity for idempotency
// scoping, or empty when unauthenticated (a message id is only unique per
// caller).
func callerScope(ec *a2asrv.ExecutorContext) string {
	if ec.User == nil {
		return ""
	}
	return ec.User.Name
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
	case errors.Is(err, errStorage):
		return codeStorageError
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
	if cur, ok := e.answerableInteraction(ec, taskID); ok {
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

// answerableInteraction returns the input request a paused task is waiting on.
// It falls back to the interaction recorded in the persisted input-required
// status message, so a paused task remains answerable across a restart (when a
// durable task store is configured).
func (e *CLIExecutor) answerableInteraction(ec *a2asrv.ExecutorContext, taskID string) (*agent.Interaction, bool) {
	if st := e.loadState(taskID); st != nil {
		if it := st.pendingInteraction(); it != nil {
			return it, true
		}
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
