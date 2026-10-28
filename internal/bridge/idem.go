package bridge

import (
	"context"
	"sync"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

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
