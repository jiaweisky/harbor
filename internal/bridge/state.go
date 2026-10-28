package bridge

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jiaweisky/harbor/internal/agent"
)

// structuredState tracks a task whose request asked for structured output, so
// the executor can fall back to parsing the final text when the CLI has no
// explicit structured-output event (exec CLIs). It is touched only by the task's
// own goroutine.
type structuredState struct{ emitted bool }

// taskState is the transient state of one task execution. A single entry in
// CLIExecutor.tasks owns everything a task holds while it runs: its process
// lifetime (ctx/cancel), the workdir lock, the turn count,
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
