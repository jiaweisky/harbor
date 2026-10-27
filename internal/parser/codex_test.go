package parser_test

import (
	"testing"

	"github.com/jiaweisky/harbor/internal/agent"
	"github.com/jiaweisky/harbor/internal/parser"
)

func collect(t *testing.T, p agent.Parser, rec string) []agent.Event {
	t.Helper()
	var events []agent.Event
	if err := p.Parse([]byte(rec), func(e agent.Event) { events = append(events, e) }); err != nil {
		t.Fatalf("Parse(%s): %v", rec, err)
	}
	return events
}

func TestCodexFileChangeAndUsage(t *testing.T) {
	p := parser.NewCodex()

	fileChange := `{"type":"item.completed","item":{"type":"file_change","changes":{"/w/main.go":{"kind":"update","unified_diff":"--- a\n+++ b"}}}}`
	evs := collect(t, p, fileChange)
	if len(evs) != 1 || evs[0].Kind != agent.EventToolCall || evs[0].Tool != "file_change" {
		t.Fatalf("file_change: %+v", evs)
	}
	if input, _ := evs[0].ToolInput["changes"].(map[string]any); len(input) == 0 {
		t.Fatalf("file_change input = %+v, want the verbatim item", evs[0].ToolInput)
	}

	usageTurn := `{"type":"turn.completed","usage":{"input_tokens":11,"output_tokens":22}}`
	evs = collect(t, p, usageTurn)
	if len(evs) != 2 || evs[0].Kind != agent.EventUsage || evs[0].Usage == nil ||
		evs[0].Usage.InputTokens != 11 || evs[0].Usage.OutputTokens != 22 ||
		evs[1].Kind != agent.EventDone {
		t.Fatalf("turn.completed with usage: %+v", evs)
	}
}

func TestCodexParse(t *testing.T) {
	p := parser.NewCodex()

	if evs := collect(t, p, `{"type":"thread.started","thread_id":"t-1"}`); len(evs) != 1 ||
		evs[0].Kind != agent.EventSession || evs[0].SessionID != "t-1" {
		t.Fatalf("thread.started: %+v", evs)
	}

	rec := `{"type":"item.completed","item":{"id":"i1","type":"agent_message","text":"Hello"}}`
	if evs := collect(t, p, rec); len(evs) != 1 ||
		evs[0].Kind != agent.EventTextDelta || evs[0].Text != "Hello" {
		t.Fatalf("agent_message: %+v", evs)
	}

	rec = `{"type":"item.completed","item":{"id":"i2","type":"command_execution","command":"ls"}}`
	evs := collect(t, p, rec)
	if len(evs) != 1 || evs[0].Kind != agent.EventToolCall || evs[0].Tool != "shell" {
		t.Fatalf("command_execution: %+v", evs)
	}

	if evs := collect(t, p, `{"type":"turn.completed"}`); len(evs) != 1 || evs[0].Kind != agent.EventDone {
		t.Fatalf("turn.completed: %+v", evs)
	}

	if evs := collect(t, p, `{"type":"turn.failed","error":{"message":"boom"}}`); len(evs) != 1 ||
		evs[0].Kind != agent.EventError || evs[0].Err == nil || evs[0].Err.Error() != "boom" {
		t.Fatalf("turn.failed: %+v", evs)
	}

	// Transient error records and item-level errors are not failures.
	if evs := collect(t, p, `{"type":"error","message":"Reconnecting... 1/5"}`); len(evs) != 0 {
		t.Fatalf("transient error should be ignored: %+v", evs)
	}
	rec = `{"type":"item.completed","item":{"id":"i0","type":"error","message":"warn"}}`
	if evs := collect(t, p, rec); len(evs) != 0 {
		t.Fatalf("item error should be ignored: %+v", evs)
	}
}
