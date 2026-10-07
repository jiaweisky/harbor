package agent

import (
	"context"
	"testing"
)

type nopParserInternal struct{}

func (nopParserInternal) Parse([]byte, func(Event)) error { return nil }
func (nopParserInternal) Flush(func(Event)) error         { return nil }

// TestRunnerReapsProcessEntry verifies the process table entry is removed once
// the process completes normally, so it does not grow without bound.
func TestRunnerReapsProcessEntry(t *testing.T) {
	r := NewRunner(1, true)
	ch, err := r.Start(context.Background(), "t1",
		&Invocation{Bin: "sh", Args: []string{"-c", "echo hi"}}, nopParserInternal{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	for range ch {
	}

	count := 0
	r.procs.Range(func(_, _ any) bool { count++; return true })
	if count != 0 {
		t.Fatalf("process entry not reaped after completion: %d", count)
	}
}
