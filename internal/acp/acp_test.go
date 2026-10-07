package acp

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/jiaweisky/harbor/internal/agent"
)

// scriptedWriter answers each outbound request via a canned response, so the
// request/response path runs deterministically without goroutines.
type scriptedWriter struct {
	c        *conn
	response func(method string, id int64) string
	written  []string
}

func (w *scriptedWriter) Write(p []byte) (int, error) {
	line := strings.TrimSpace(string(p))
	w.written = append(w.written, line)
	var m rpcMessage
	if err := json.Unmarshal([]byte(line), &m); err == nil && m.Method != "" && w.response != nil {
		id, _ := parseID(m.ID)
		if resp := w.response(m.Method, id); resp != "" {
			w.c.handle([]byte(resp), func(agent.Event) {})
		}
	}
	return len(p), nil
}

func newTestConn() (*conn, *session, *scriptedWriter) {
	s := &session{}
	c := &conn{pending: map[int64]chan rpcMessage{}, s: s}
	s.conn = c
	w := &scriptedWriter{c: c}
	c.stdin = w
	return c, s, w
}

func TestSendCorrelatesResponse(t *testing.T) {
	c, _, w := newTestConn()
	w.response = func(method string, id int64) string {
		return `{"jsonrpc":"2.0","id":` + itoa(id) + `,"result":{"sessionId":"ses_1"}}`
	}
	res, err := c.send(context.Background(), "session/new", map[string]any{"cwd": "/tmp"})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	var ns newSessionResponse
	if err := json.Unmarshal(res, &ns); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ns.SessionID != "ses_1" {
		t.Fatalf("session id = %q, want ses_1", ns.SessionID)
	}
}

func TestSendPropagatesRPCError(t *testing.T) {
	c, _, w := newTestConn()
	w.response = func(method string, id int64) string {
		return `{"jsonrpc":"2.0","id":` + itoa(id) + `,"error":{"code":-32602,"message":"nope"}}`
	}
	_, err := c.send(context.Background(), "session/prompt", nil)
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("expected rpc error, got %v", err)
	}
}

func TestSessionUpdateEmitsEvents(t *testing.T) {
	c, _, _ := newTestConn()
	var got []agent.Event
	emit := func(ev agent.Event) { got = append(got, ev) }

	c.handle(updateLine("agent_thought_chunk", "why"), emit)
	c.handle(updateLine("agent_message_chunk", "Hello"), emit)
	c.handle(updateLine("agent_message_chunk", " world"), emit)
	c.handle(toolCallLine("tool_call", "bash"), emit)

	want := []agent.EventKind{
		agent.EventThinking, agent.EventTextDelta, agent.EventTextDelta, agent.EventToolCall,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d events, want %d: %+v", len(got), len(want), got)
	}
	for i, k := range want {
		if got[i].Kind != k {
			t.Fatalf("event %d kind = %v, want %v", i, got[i].Kind, k)
		}
	}
	if got[1].Text != "Hello" || got[2].Text != " world" {
		t.Fatalf("unexpected text: %q %q", got[1].Text, got[2].Text)
	}
	if got[3].Tool != "bash" {
		t.Fatalf("tool = %q, want bash", got[3].Tool)
	}
}

func TestToolCallUpdateOnlyEmitsWhenFinished(t *testing.T) {
	c, _, _ := newTestConn()
	var got []agent.Event
	emit := func(ev agent.Event) { got = append(got, ev) }

	c.handle(statusLine("in_progress"), emit)
	if len(got) != 0 {
		t.Fatalf("in-progress update emitted %d events, want 0", len(got))
	}
	c.handle(statusLine("completed"), emit)
	if len(got) != 1 || got[0].Kind != agent.EventToolResult {
		t.Fatalf("completed update = %+v, want one tool result", got)
	}
}

func TestPermissionRequestAndAnswer(t *testing.T) {
	c, s, w := newTestConn()
	var got []agent.Event
	c.handle(permissionLine(), func(ev agent.Event) { got = append(got, ev) })

	if len(got) != 1 || got[0].Kind != agent.EventPermissionRequest {
		t.Fatalf("expected one permission event, got %+v", got)
	}
	if !strings.Contains(got[0].Text, "allow_once") {
		t.Fatalf("permission text missing option ids: %q", got[0].Text)
	}
	itx := got[0].Interaction
	if itx == nil || itx.Kind != agent.InteractionPermission || !itx.Resume {
		t.Fatalf("interaction = %+v, want a permission with resume", itx)
	}
	if len(itx.Options) != 2 || itx.Options[0] != "allow_once" {
		t.Fatalf("interaction options = %v", itx.Options)
	}
	if err := s.Send(context.Background(), agent.Input{Answer: "allow_once"}); err != nil {
		t.Fatalf("send answer: %v", err)
	}
	last := w.written[len(w.written)-1]
	if !strings.Contains(last, `"id":9001`) || !strings.Contains(last, `"optionId":"allow_once"`) {
		t.Fatalf("unexpected permission response: %s", last)
	}
}

func TestUnadvertisedMethodIsRefused(t *testing.T) {
	c, _, w := newTestConn()
	c.handle([]byte(`{"jsonrpc":"2.0","id":5,"method":"fs/read_text_file","params":{}}`),
		func(agent.Event) {})
	if len(w.written) != 1 {
		t.Fatalf("expected one response, got %d", len(w.written))
	}
	if !strings.Contains(w.written[0], "-32601") {
		t.Fatalf("expected method-not-found, got %s", w.written[0])
	}
}

func updateRecord(update map[string]any) []byte {
	m := map[string]any{
		"jsonrpc": "2.0",
		"method":  "session/update",
		"params":  map[string]any{"sessionId": "ses_1", "update": update},
	}
	b, _ := json.Marshal(m)
	return b
}

func updateLine(kind, text string) []byte {
	return updateRecord(map[string]any{
		"sessionUpdate": kind,
		"content":       map[string]any{"type": "text", "text": text},
	})
}

func toolCallLine(kind, title string) []byte {
	return updateRecord(map[string]any{"sessionUpdate": kind, "title": title, "kind": "execute"})
}

func statusLine(status string) []byte {
	return updateRecord(map[string]any{"sessionUpdate": "tool_call_update", "status": status})
}

func permissionLine() []byte {
	m := map[string]any{
		"jsonrpc": "2.0",
		"id":      9001,
		"method":  "session/request_permission",
		"params": map[string]any{
			"sessionId": "ses_1",
			"toolCall":  map[string]any{"toolCallId": "tc1", "title": "run tests", "kind": "execute"},
			"options": []map[string]any{
				{"optionId": "allow_once", "name": "Allow", "kind": "allow_once"},
				{"optionId": "reject_once", "name": "Reject", "kind": "reject_once"},
			},
		},
	}
	b, _ := json.Marshal(m)
	return b
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }
