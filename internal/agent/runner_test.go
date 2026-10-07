package agent_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/jiaweisky/harbor/internal/agent"
)

type nopParser struct{}

func (nopParser) Parse([]byte, func(agent.Event)) error { return nil }
func (nopParser) Flush(func(agent.Event)) error         { return nil }

// echoParser emits one event per record, so a flooding CLI fills the buffer.
type echoParser struct{}

func (echoParser) Parse(_ []byte, out func(agent.Event)) error {
	out(agent.Event{Kind: agent.EventTextDelta, Text: "x"})
	return nil
}
func (echoParser) Flush(func(agent.Event)) error { return nil }

// TestRunnerContextCancelKills verifies that canceling the context terminates
// the process group (the regression for "exec.CommandContext only kills direct child").
func TestRunnerContextCancelKills(t *testing.T) {
	r := agent.NewRunner(1, true, "")
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := r.Start(ctx, "task-1", &agent.Invocation{Bin: "sleep", Args: []string{"30"}}, nopParser{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	done := make(chan struct{})
	go func() {
		for range ch {
		}
		close(done)
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("process group not terminated after context cancel")
	}
}

// TestRunnerAbandonedConsumerReleasesSlot verifies that when the consumer stops
// reading (channel fills, sends block) and the process is killed, the read
// goroutine aborts and its concurrency slot is released.
func TestRunnerAbandonedConsumerReleasesSlot(t *testing.T) {
	r := agent.NewRunner(1, true, "")
	flood := &agent.Invocation{
		Bin:  "sh",
		Args: []string{"-c", "i=0; while [ $i -lt 100000 ]; do echo line; i=$((i+1)); done"},
	}

	if _, err := r.Start(context.Background(), "t1", flood, echoParser{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Never read the channel; let the buffer fill and the reader block.
	time.Sleep(100 * time.Millisecond)
	r.Kill("t1")

	// With maxConcurrent=1, a leaked slot would make this block past the timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ch, err := r.Start(ctx, "t2", &agent.Invocation{Bin: "sh", Args: []string{"-c", "echo hi"}}, nopParser{})
	if err != nil {
		t.Fatalf("second Start blocked (slot leaked): %v", err)
	}
	for range ch {
	}
}

// TestRunnerDeliversAllEventsToSlowConsumer verifies backpressure: a slow
// consumer still receives every event produced during normal operation.
func TestRunnerDeliversAllEventsToSlowConsumer(t *testing.T) {
	const n = 300
	r := agent.NewRunner(1, true, "")
	script := "i=0; while [ $i -lt " + strconv.Itoa(n) + " ]; do echo x; i=$((i+1)); done"

	inv := &agent.Invocation{Bin: "sh", Args: []string{"-c", script}}
	ch, err := r.Start(context.Background(), "t1", inv, echoParser{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	got := 0
	for range ch {
		got++
		time.Sleep(time.Millisecond) // force the buffer to fill (backpressure)
	}
	if got != n {
		t.Fatalf("received %d events, want %d", got, n)
	}
}
