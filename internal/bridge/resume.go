package bridge

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/jiaweisky/harbor/internal/agent"
	"github.com/jiaweisky/harbor/internal/telemetry"
)

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
