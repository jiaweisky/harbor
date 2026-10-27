package opencode_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jiaweisky/harbor/internal/agent"
	"github.com/jiaweisky/harbor/internal/config"
	"github.com/jiaweisky/harbor/internal/integration"
)

// TestRealOpencodeServer drives the real opencode server (OPENCODE_SERVE URL)
// through Harbor's adapter and logs the event sequence, so the adapter's
// handling can be reviewed against the live API.
func TestRealOpencodeServer(t *testing.T) {
	base := os.Getenv("OPENCODE_SERVE")
	if base == "" {
		t.Skip("OPENCODE_SERVE not set")
	}

	ln, err := integration.New(config.AgentConfig{
		ID: "oc", Binary: os.Getenv("OPENCODE_BIN"), Integration: "server",
		ServerURL: base,
	}, integration.Options{Ctx: context.Background()})
	if err != nil {
		t.Fatalf("integration.New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	sess, err := ln.Start(ctx, &agent.Request{Prompt: "create a file named oc-real.txt containing exactly hello real, then reply done"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	n := 0
	for ev := range sess.Events() {
		n++
		switch ev.Kind {
		case agent.EventSession:
			t.Logf("%d Session id=%q", n, ev.SessionID)
		case agent.EventToolCall:
			t.Logf("%d ToolCall tool=%q input=%v", n, ev.Tool, ev.ToolInput)
		case agent.EventToolResult:
			t.Logf("%d ToolResult text=%.80q", n, ev.Text)
		case agent.EventThinking:
			t.Logf("%d Thinking text=%.60q", n, ev.Text)
		case agent.EventUsage:
			t.Logf("%d Usage %+v", n, ev.Usage)
		default:
			t.Logf("%d %v text=%.60q", n, ev.Kind, ev.Text)
		}
	}
	t.Logf("total events: %d", n)
}
