package bridge

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/jiaweisky/harbor/internal/agent"
)

// fakeSession is a controllable agent.Session for driving the executor directly.
type fakeSession struct {
	events   chan agent.Event
	canceled chan struct{}
	once     sync.Once
}

func newFakeSession() *fakeSession { return &fakeSession{canceled: make(chan struct{})} }

func (s *fakeSession) Events() <-chan agent.Event              { return s.events }
func (s *fakeSession) Send(context.Context, agent.Input) error { return nil }
func (s *fakeSession) Cancel() error                           { s.once.Do(func() { close(s.canceled) }); return nil }
func (s *fakeSession) Canceled() bool {
	select {
	case <-s.canceled:
		return true
	default:
		return false
	}
}

// TestWatchExecutionStopsCLIOnAbort verifies the server's inactivity abort
// stops the CLI promptly.
func TestWatchExecutionStopsCLIOnAbort(t *testing.T) {
	live := agent.NewLiveSessions()
	e := &CLIExecutor{live: live, metrics: &agentMetrics{}}
	sess := newFakeSession()
	live.Put("t1", sess)
	e.tasks.Store("t1", &taskState{cancel: func() {}})

	ctx, cancel := context.WithCancelCause(context.Background())
	e.watchExecution(ctx, "t1")
	cancel(a2asrv.ErrAgentInactivityTimeout)

	select {
	case <-sess.canceled:
	case <-time.After(time.Second):
		t.Fatal("CLI was not stopped on the inactivity abort")
	}
}

// TestWatchExecutionIgnoresNormalReturn verifies a normal completion (the SDK
// cancels the context with an internal sentinel) does not stop the CLI, so
// resume still works.
func TestWatchExecutionIgnoresNormalReturn(t *testing.T) {
	live := agent.NewLiveSessions()
	e := &CLIExecutor{live: live, metrics: &agentMetrics{}}
	sess := newFakeSession()
	live.Put("t1", sess)
	e.tasks.Store("t1", &taskState{cancel: func() {}})

	ctx, cancel := context.WithCancelCause(context.Background())
	e.watchExecution(ctx, "t1")
	cancel(errors.New("consumer stopped"))

	select {
	case <-sess.canceled:
		t.Fatal("CLI must not be stopped when the request returns normally")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestPumpEmitsKeepaliveWhenSilent verifies a silent CLI produces periodic
// keepalive updates rather than an apparently idle stream.
func TestPumpEmitsKeepaliveWhenSilent(t *testing.T) {
	e := &CLIExecutor{keepalive: 30 * time.Millisecond, metrics: &agentMetrics{}}
	st := &taskState{ctx: context.Background()}
	sess := newFakeSession()
	sess.events = make(chan agent.Event, 1)
	ec := &a2asrv.ExecutorContext{TaskID: "t1", ContextID: "c1"}

	var mu sync.Mutex
	var got []a2a.Event
	yield := func(ev a2a.Event, _ error) bool {
		mu.Lock()
		got = append(got, ev)
		mu.Unlock()
		return true
	}

	done := make(chan struct{})
	go func() { e.pump(st, ec, "t1", sess, yield); close(done) }()
	time.Sleep(120 * time.Millisecond)
	close(sess.events)
	<-done

	mu.Lock()
	defer mu.Unlock()
	for _, ev := range got {
		if su, ok := ev.(*a2a.TaskStatusUpdateEvent); ok &&
			su.Status.Message != nil && su.Status.Message.Metadata[metaKeyKind] == "keepalive" {
			return
		}
	}
	t.Fatalf("no keepalive emitted; events = %v", got)
}
