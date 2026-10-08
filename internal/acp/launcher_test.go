package acp_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jiaweisky/harbor/internal/acp"
	"github.com/jiaweisky/harbor/internal/agent"
)

func fakeACP(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "testdata", "fake-cli", "fake-acp.sh"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLauncherTurn(t *testing.T) {
	ln := acp.NewLauncher(acp.Config{Name: "acp", Binary: fakeACP(t), InheritEnv: true})
	if !ln.Capabilities().InputRequiredResume {
		t.Fatal("ACP must support input-required resume")
	}

	ctx, cancel := context.WithTimeout(agent.WithTaskID(context.Background(), "t1"), 10*time.Second)
	defer cancel()
	sess, err := ln.Start(ctx, &agent.Request{Prompt: "hi", WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer sess.Cancel()

	var text, thinking string
	var usage *agent.Usage
	var answered bool
	deadline := time.After(10 * time.Second)

loop:
	for {
		select {
		case ev, ok := <-sess.Events():
			if !ok {
				break loop
			}
			switch ev.Kind {
			case agent.EventThinking:
				thinking += ev.Text
			case agent.EventTextDelta:
				text += ev.Text
			case agent.EventUsage:
				usage = ev.Usage
			case agent.EventPermissionRequest:
				if !strings.Contains(ev.Text, "allow_once") {
					t.Fatalf("permission text missing options: %q", ev.Text)
				}
				if err := sess.Send(ctx, agent.Input{Answer: "allow_once"}); err != nil {
					t.Fatalf("send answer: %v", err)
				}
				answered = true
			case agent.EventError:
				t.Fatalf("event error: %v", ev.Err)
			}
		case <-deadline:
			t.Fatal("timed out waiting for the turn to finish")
		}
	}

	if text != "Hello world" {
		t.Fatalf("text = %q, want %q", text, "Hello world")
	}
	if thinking != "thinking" {
		t.Fatalf("thinking = %q, want %q", thinking, "thinking")
	}
	if usage == nil || usage.InputTokens != 7 || usage.OutputTokens != 2 {
		t.Fatalf("usage = %+v, want {7 2}", usage)
	}
	if !answered {
		t.Fatal("permission request was never answered")
	}
}

// TestLauncherResumesSession verifies a request carrying a session id loads that
// session (session/load) instead of creating a new one, and the id is reported.
func TestLauncherResumesSession(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "acp.log")
	ln := acp.NewLauncher(acp.Config{
		Name: "acp", Binary: fakeACP(t), InheritEnv: true,
		Env: []string{"FAKE_ACP_LOG=" + logPath},
	})
	ctx, cancel := context.WithTimeout(agent.WithTaskID(context.Background(), "t3"), 10*time.Second)
	defer cancel()

	sess, err := ln.Start(ctx, &agent.Request{Prompt: "hi", WorkDir: t.TempDir(), SessionID: "ses_prev"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer sess.Cancel()

	var sessionID string
	for ev := range sess.Events() {
		switch ev.Kind {
		case agent.EventSession:
			sessionID = ev.SessionID
		case agent.EventPermissionRequest:
			if err := sess.Send(ctx, agent.Input{Answer: "allow_once"}); err != nil {
				t.Fatalf("send answer: %v", err)
			}
		case agent.EventError:
			t.Fatalf("event error: %v", ev.Err)
		}
	}
	if sessionID != "ses_fake" {
		t.Fatalf("session id = %q, want ses_fake", sessionID)
	}
	rec, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(rec), "session/load") || !strings.Contains(string(rec), "ses_prev") {
		t.Fatalf("expected a session/load for ses_prev, got:\n%s", rec)
	}
}

func TestLauncherCancel(t *testing.T) {
	ln := acp.NewLauncher(acp.Config{Name: "acp", Binary: fakeACP(t), InheritEnv: true})
	ctx, cancel := context.WithTimeout(agent.WithTaskID(context.Background(), "t2"), 10*time.Second)
	defer cancel()
	sess, err := ln.Start(ctx, &agent.Request{Prompt: "hi", WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := sess.Cancel(); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if !sess.Canceled() {
		t.Fatal("session should report canceled")
	}
	closed := make(chan struct{})
	go func() {
		for range sess.Events() {
		}
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("events channel did not close after cancel")
	}
}
