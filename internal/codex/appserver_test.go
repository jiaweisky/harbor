package codex_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jiaweisky/harbor/internal/agent"
	"github.com/jiaweisky/harbor/internal/cli"
	"github.com/jiaweisky/harbor/internal/codex"
)

// fakeAppServer speaks just enough of the app-server protocol: initialize,
// thread/start, turn/start, two agentMessage deltas, then turn/completed.
const fakeAppServer = `#!/usr/bin/env bash
read -r _ # initialize
printf '{"jsonrpc":"2.0","id":1,"result":{"userAgent":"fake"}}\n'
read -r _ # thread/start
printf '{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"th_1"}}}\n'
read -r _ # turn/start
printf '{"jsonrpc":"2.0","id":3,"result":{"turn":{"id":"tu_1"}}}\n'
delta_head='{"jsonrpc":"2.0","method":"item/agentMessage/delta","params":'
printf '%s{"threadId":"th_1","turnId":"tu_1","itemId":"i1","delta":"po"}}\n' "$delta_head"
printf '%s{"threadId":"th_1","turnId":"tu_1","itemId":"i1","delta":"ng"}}\n' "$delta_head"
printf '{"jsonrpc":"2.0","method":"turn/completed","params":'
printf '{"threadId":"th_1","turn":{"id":"tu_1","status":"completed"}}}\n'
sleep 30
`

func writeFake(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(bin, []byte(fakeAppServer), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
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
