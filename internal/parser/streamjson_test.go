package parser_test

import (
	"testing"

	"github.com/jiaweisky/harbor/internal/agent"
	"github.com/jiaweisky/harbor/internal/parser"
)

func TestStreamJSONThinkingAndUsage(t *testing.T) {
	p := parser.NewStreamJSON()

	rec := `{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"reasoning"}]}}`
	if evs := collect(t, p, rec); len(evs) != 1 ||
		evs[0].Kind != agent.EventThinking || evs[0].Text != "reasoning" {
		t.Fatalf("thinking: %+v", evs)
	}

	res := `{"type":"result","result":"done","is_error":false,"session_id":"s",` +
		`"total_cost_usd":0.05,"usage":{"input_tokens":10,"output_tokens":20}}`
	var usage *agent.Usage
	for _, e := range collect(t, p, res) {
		if e.Kind == agent.EventUsage {
			usage = e.Usage
		}
	}
	if usage == nil || usage.InputTokens != 10 || usage.OutputTokens != 20 || usage.CostUSD != 0.05 {
		t.Fatalf("usage: %+v", usage)
	}
}

func TestStreamJSONControlRequest(t *testing.T) {
	p := parser.NewStreamJSON()
	rec := `{"type":"control_request","request_id":"perm_1",` +
		`"request":{"subtype":"can_use_tool","tool_name":"Bash",` +
		`"input":{"command":"echo hi"},"tool_use_id":"toolu_1"}}`

	evs := collect(t, p, rec)
	if len(evs) != 1 || evs[0].Kind != agent.EventPermissionRequest {
		t.Fatalf("control_request: %+v", evs)
	}
	itx := evs[0].Interaction
	if evs[0].Tool != "Bash" {
		t.Errorf("tool = %q, want Bash", evs[0].Tool)
	}
	if itx == nil || itx.Kind != agent.InteractionPermission || itx.ID != "perm_1" ||
		itx.Action != "Bash" || itx.Target != "echo hi" || !itx.Resume || len(itx.Options) == 0 {
		t.Fatalf("interaction = %+v", itx)
	}

	// A control_request Harbor has no handler for is ignored.
	other := `{"type":"control_request","request_id":"x","request":{"subtype":"interrupt"}}`
	if evs := collect(t, p, other); len(evs) != 0 {
		t.Fatalf("non-permission control_request should be ignored, got %+v", evs)
	}
}
