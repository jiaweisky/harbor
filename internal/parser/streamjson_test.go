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
