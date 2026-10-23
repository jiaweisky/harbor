package agent_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jiaweisky/harbor/internal/agent"
	"github.com/jiaweisky/harbor/internal/sandbox"
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
	r := agent.NewRunner(1, true, "", nil, nil)
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
	r := agent.NewRunner(1, true, "", nil, nil)
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

// TestRunnerSetsPWDToDir verifies that a child launched with Dir also sees PWD
// set to it, so a CLI that reads PWD runs in the right directory (regression:
// opencode used the parent's PWD and wrote outside the workspace).
func TestRunnerSetsPWDToDir(t *testing.T) {
	r := agent.NewRunner(1, true, "", nil, nil)
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	inv := &agent.Invocation{Bin: "sh", Args: []string{"-c", `printf %s "$PWD"`}, Dir: dir}
	ch, err := r.Start(ctx, "t1", inv, agent.NewLineParser())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	var out strings.Builder
	for ev := range ch {
		if ev.Kind == agent.EventTextDelta {
			out.WriteString(ev.Text)
		}
	}
	if got := strings.TrimSpace(out.String()); got != dir {
		t.Errorf("child PWD = %q, want %q", got, dir)
	}
}

// TestRunnerContainerRunsCLI verifies that with a container sandbox the CLI is
// launched through the container runtime, with the task directory mounted at its
// host path and the environment forwarded, and that its output still streams.
func TestRunnerContainerRunsCLI(t *testing.T) {
	script, err := filepath.Abs(filepath.Join("..", "..", "testdata", "fake-cli", "fake-docker.sh"))
	if err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "docker.log")
	t.Setenv("FAKE_DOCKER_LOG", log)
	t.Setenv("FAKE_DOCKER_IMAGE", "fakeimage")

	dir := t.TempDir()
	r := agent.NewRunner(1, true, "",
		&sandbox.Container{
			Runtime: script, Image: "fakeimage", Mounts: []string{dir},
			ExtraArgs: []string{"--read-only"},
		}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	inv := &agent.Invocation{Bin: "sh", Args: []string{"-c", "echo OK"}, Dir: dir}
	ch, err := r.Start(ctx, "t1", inv, agent.NewLineParser())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	var out strings.Builder
	for ev := range ch {
		if ev.Kind == agent.EventTextDelta {
			out.WriteString(ev.Text)
		}
	}
	if !strings.Contains(out.String(), "OK") {
		t.Fatalf("container CLI output = %q, want OK", out.String())
	}

	rec, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("read docker log: %v", err)
	}
	for _, want := range []string{"run", "--rm", "-i", dir + ":" + dir, "--read-only", "fakeimage", "echo OK"} {
		if !strings.Contains(string(rec), want) {
			t.Errorf("container argv missing %q; log:\n%s", want, rec)
		}
	}
}

// TestRunnerRawStream verifies raw stdout/stderr are written when raw stream
// logging is enabled.
func TestRunnerRawStream(t *testing.T) {
	dir := t.TempDir()
	r := agent.NewRunner(1, true, "", nil, agent.NewRuntime("", dir, false))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	inv := &agent.Invocation{Bin: "sh", Args: []string{"-c", "echo hello; echo oops 1>&2"}}
	ch, err := r.Start(ctx, "t1", inv, nopParser{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	for range ch {
	}

	out, err := os.ReadFile(filepath.Join(dir, "t1.out"))
	if err != nil || !strings.Contains(string(out), "hello") {
		t.Fatalf("raw stdout = %q, err = %v", out, err)
	}
	errOut, err := os.ReadFile(filepath.Join(dir, "t1.err"))
	if err != nil || !strings.Contains(string(errOut), "oops") {
		t.Fatalf("raw stderr = %q, err = %v", errOut, err)
	}
}

// TestRunnerDeliversAllEventsToSlowConsumer verifies backpressure: a slow
// consumer still receives every event produced during normal operation.
func TestRunnerDeliversAllEventsToSlowConsumer(t *testing.T) {
	const n = 300
	r := agent.NewRunner(1, true, "", nil, nil)
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

// TestRunnerClosesStdinOneShot verifies that a one-shot invocation with no
// initial Stdin gets EOF on stdin, so a CLI that reads stdin does not block
// (regression: `opencode run` / `codex exec` hung on an open pipe).
func TestRunnerClosesStdinOneShot(t *testing.T) {
	r := agent.NewRunner(1, true, "", nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	inv := &agent.Invocation{Bin: "sh", Args: []string{"-c", "cat >/dev/null; echo done"}}
	ch, err := r.Start(ctx, "t1", inv, agent.NewLineParser())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	var out strings.Builder
	for ev := range ch {
		if ev.Kind == agent.EventTextDelta {
			out.WriteString(ev.Text)
		}
	}
	if !strings.Contains(out.String(), "done") {
		t.Fatalf("CLI blocked on stdin (no EOF); output = %q", out.String())
	}
}
