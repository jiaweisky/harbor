package codex_test

import (
	"context"
	"testing"
	"time"

	"github.com/jiaweisky/harbor/internal/agent"
	"github.com/jiaweisky/harbor/internal/cli"
	"github.com/jiaweisky/harbor/internal/codex"
	"github.com/jiaweisky/harbor/internal/fakeclitest"
)

// writeFake builds the fake app-server binary for tests.
func writeFake(t *testing.T) string {
	t.Helper()
	return fakeclitest.Path(t, "fake-codex-appserver")
}

func TestAppServerForwardsItems(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ln := codex.NewServerLauncher(ctx, codex.Config{
		Name: "cx", Binary: writeFake(t), Auth: cli.Unverified(),
		Env:  []string{"FAKE_ITEMS=1"},
	})
	sess, err := ln.Start(ctx, &agent.Request{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	var evs []agent.Event
	for ev := range sess.Events() {
		evs = append(evs, ev)
	}
	if len(evs) != 5 ||
		evs[0].Kind != agent.EventSession ||
		evs[1].Kind != agent.EventThinking || evs[1].Text != "thinking" ||
		evs[2].Kind != agent.EventToolCall || evs[2].Tool != "shell" ||
		evs[3].Kind != agent.EventToolCall || evs[3].Tool != "file_change" ||
		evs[3].ToolInput["changes"] == nil ||
		evs[4].Kind != agent.EventDone {
		t.Fatalf("events = %+v, want session, thinking, two tool calls, done", evs)
	}
}

func TestAppServerTurn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ln := codex.NewServerLauncher(ctx, codex.Config{
		Name: "cx", Binary: writeFake(t), Auth: cli.Unverified(),
	})
	sess, err := ln.Start(ctx, &agent.Request{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	var evs []agent.Event
	for ev := range sess.Events() {
		evs = append(evs, ev)
	}
	if len(evs) != 4 ||
		evs[0].Kind != agent.EventSession || evs[0].SessionID != "th_1" ||
		evs[1].Text != "po" || evs[2].Text != "ng" ||
		evs[3].Kind != agent.EventDone {
		t.Fatalf("events = %+v", evs)
	}
}
