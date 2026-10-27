package agent

import (
	"context"
	"sync"
	"sync/atomic"
)

// ServerSession is the Session implementation shared by server-integrated CLIs.
// The launcher owns the event loop: it emits events with Emit, wires Abort to
// stop upstream work, and calls Close once the run ends. Cancel marks the
// session canceled, runs Abort, and cancels the session context.
type ServerSession struct {
	events   chan Event
	cancel   context.CancelFunc
	canceled atomic.Bool
	mu       sync.Mutex
	abort    func()
}

// NewServerSession returns a session whose events channel is buffered with the
// given capacity. cancel is invoked by Cancel.
func NewServerSession(buffer int, cancel context.CancelFunc) *ServerSession {
	return &ServerSession{events: make(chan Event, buffer), cancel: cancel}
}

func (s *ServerSession) Events() <-chan Event { return s.events }

// Send is unsupported: server-integrated CLIs take input over HTTP, not stdin.
func (s *ServerSession) Send(context.Context, Input) error { return ErrUnsupported }

func (s *ServerSession) Canceled() bool { return s.canceled.Load() }

// SetAbort sets the function Cancel calls to stop upstream work, such as
// canceling a run. It is safe to call before or after the run starts.
func (s *ServerSession) SetAbort(abort func()) {
	s.mu.Lock()
	s.abort = abort
	s.mu.Unlock()
}

func (s *ServerSession) Cancel() error {
	s.canceled.Store(true)

	s.mu.Lock()
	abort := s.abort
	s.mu.Unlock()

	if abort != nil {
		abort()
	}

	s.cancel()

	return nil
}

// Emit sends an event to the consumer, blocking until it is read.
func (s *ServerSession) Emit(e Event) { s.events <- e }

// EmitError emits an error unless ctx is already done, in which case the
// executor owns the terminal state and the error is dropped.
func (s *ServerSession) EmitError(ctx context.Context, err error) {
	if ctx.Err() != nil {
		return
	}

	s.events <- Event{Kind: EventError, Err: err}
}

// Close closes the events channel. The launcher calls it once when the run ends.
func (s *ServerSession) Close() { close(s.events) }
