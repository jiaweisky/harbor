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

func TestStreamJSONFinalTextFallback(t *testing.T) {
	p := parser.NewStreamJSON()
	var sawDelta, sawFinal bool
	for _, e := range collect(t, p, `{"type":"result","result":"done","is_error":false}`) {
		switch e.Kind {
		case agent.EventTextDelta:
			sawDelta = true
		case agent.EventTextFinal:
			sawFinal = true
		}
	}
	if sawDelta || !sawFinal {
		t.Fatalf("want a final-text fallback with no delta, got delta=%v final=%v", sawDelta, sawFinal)
	}
}

func TestStreamJSONAssistantMessagesAccumulate(t *testing.T) {
	p := parser.NewStreamJSON()
	evs := collect(t, p, `{"type":"assistant","message":{"content":[{"type":"text","text":"Hello "}]}}`)
	evs = append(evs, collect(t, p, `{"type":"assistant","message":{"content":[{"type":"text","text":"world"}]}}`)...)
	evs = append(evs, collect(t, p, `{"type":"result","result":"Hello world","is_error":false}`)...)

	var text string
	finals := 0
	for _, e := range evs {
		switch e.Kind {
		case agent.EventTextDelta:
			text += e.Text
		case agent.EventTextFinal:
			finals++
		}
	}
	if text != "Hello world" || finals != 0 {
		t.Fatalf("text = %q (finals=%d), want Hello world with no final fallback", text, finals)
	}
}

func TestStreamJSONPartialDedup(t *testing.T) {
	p := parser.NewStreamJSON()
	evs := collect(t, p, `{"type":"stream_event","event":{"delta":{"text":"Hi"}}}`)
	evs = append(evs, collect(t, p, `{"type":"assistant","message":{"content":[{"type":"text","text":"Hi there"}]}}`)...)

	var text string
	for _, e := range evs {
		if e.Kind == agent.EventTextDelta {
			text += e.Text
		}
	}
	if text != "Hi" {
		t.Fatalf("text = %q, want Hi (the full assistant message is skipped after a delta)", text)
	}
}

// TestStreamJSONThinkingDelta verifies an extended-thinking delta is surfaced,
// so a long reasoning phase is not an invisible (and apparently idle) stream.
func TestStreamJSONThinkingDelta(t *testing.T) {
	p := parser.NewStreamJSON()
	rec := `{"type":"stream_event","event":{"delta":{"type":"thinking_delta","thinking":"hmm"}}}`
	evs := collect(t, p, rec)
	if len(evs) != 1 || evs[0].Kind != agent.EventThinking || evs[0].Text != "hmm" {
		t.Fatalf("thinking delta: %+v", evs)
	}
}

// TestStreamJSONThinkingDeltaKeepsText verifies a thinking delta does not
// suppress the assistant's text (only a streamed text delta does).
func TestStreamJSONThinkingDeltaKeepsText(t *testing.T) {
	p := parser.NewStreamJSON()
	collect(t, p, `{"type":"stream_event","event":{"delta":{"type":"thinking_delta","thinking":"hmm"}}}`)
	evs := collect(t, p, `{"type":"assistant","message":{"content":[{"type":"text","text":"answer"}]}}`)

	var text string
	for _, e := range evs {
		if e.Kind == agent.EventTextDelta {
			text += e.Text
		}
	}
	if text != "answer" {
		t.Fatalf("text = %q, want answer (thinking must not suppress the message text)", text)
	}
}

func TestStreamJSONElicitationForm(t *testing.T) {
	p := parser.NewStreamJSON()
	rec := `{"type":"control_request","request_id":"el_1",` +
		`"request":{"subtype":"elicitation_create","mode":"form","message":"Pick a color",` +
		`"requestedSchema":{"type":"object","properties":{"color":{"type":"string","enum":["red","blue"]}},` +
		`"required":["color"]}}}`

	evs := collect(t, p, rec)
	if len(evs) != 1 || evs[0].Kind != agent.EventPermissionRequest {
		t.Fatalf("elicitation_create: %+v", evs)
	}
	itx := evs[0].Interaction
	if itx == nil || itx.Kind != agent.InteractionQuestion || itx.Action != "form" || itx.ID != "el_1" {
		t.Fatalf("interaction = %+v, want a form question", itx)
	}
	if itx.Schema == nil {
		t.Fatal("form interaction should carry the requested schema")
	}
	if len(itx.Options) != 2 || itx.Options[0] != "red" || itx.Options[1] != "blue" {
		t.Fatalf("options = %v, want [red blue]", itx.Options)
	}
}

func TestStreamJSONExitPlanMode(t *testing.T) {
	p := parser.NewStreamJSON()
	rec := `{"type":"control_request","request_id":"plan_1",` +
		`"request":{"subtype":"can_use_tool","tool_name":"ExitPlanMode","input":{"plan":"Create README"}}}`

	evs := collect(t, p, rec)
	if len(evs) != 1 || evs[0].Kind != agent.EventPermissionRequest {
		t.Fatalf("ExitPlanMode: %+v", evs)
	}
	itx := evs[0].Interaction
	if itx == nil || itx.Kind != agent.InteractionPlanApproval || itx.ID != "plan_1" {
		t.Fatalf("interaction = %+v, want a plan approval", itx)
	}
	if len(itx.Options) == 0 {
		t.Fatalf("plan approval should offer allow/deny: %+v", itx)
	}
}

func TestStreamJSONStructuredOutput(t *testing.T) {
	p := parser.NewStreamJSON()
	rec := `{"type":"result","result":"done","is_error":false,` +
		`"structured_output":{"city":"Paris","population_millions":2.1}}`

	var data map[string]any
	for _, e := range collect(t, p, rec) {
		if e.Kind == agent.EventStructuredOutput {
			data = e.Data
		}
	}
	if data == nil || data["city"] != "Paris" {
		t.Fatalf("structured output = %v, want city Paris", data)
	}
}

func TestStreamJSONToolResultCarriesOutput(t *testing.T) {
	stringContent := `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"done"}]}}`
	blockContent := `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t2","content":[{"type":"text","text":"line1"},{"type":"text","text":"line2"}]}]}}`

	p := parser.NewStreamJSON()
	evs := collect(t, p, stringContent)
	evs = append(evs, collect(t, p, blockContent)...)

	if len(evs) != 2 || evs[0].Kind != agent.EventToolResult || evs[1].Kind != agent.EventToolResult {
		t.Fatalf("events = %+v, want two tool results", evs)
	}
	if evs[0].Text != "done" {
		t.Fatalf("string content text = %q, want %q", evs[0].Text, "done")
	}
	if evs[1].Text != "line1\nline2" {
		t.Fatalf("block content text = %q, want %q", evs[1].Text, "line1\nline2")
	}
}

func TestStreamJSONAskUserQuestion(t *testing.T) {
	p := parser.NewStreamJSON()
	rec := `{"type":"control_request","request_id":"perm_q1",` +
		`"request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion",` +
		`"input":{"questions":[{"question":"Favorite color?","header":"Color","multiSelect":false,` +
		`"options":[{"label":"Red","description":""},{"label":"Blue","description":""}]}]},` +
		`"tool_use_id":"t1"}}`

	evs := collect(t, p, rec)
	if len(evs) != 1 || evs[0].Kind != agent.EventPermissionRequest {
		t.Fatalf("ask-user control_request: %+v", evs)
	}
	itx := evs[0].Interaction
	if itx == nil || itx.Kind != agent.InteractionQuestion {
		t.Fatalf("interaction = %+v, want a question", itx)
	}
	if itx.ID != "perm_q1" || itx.Target != "Favorite color?" {
		t.Fatalf("interaction = %+v, want id/target from the question", itx)
	}
	if len(itx.Options) != 2 || itx.Options[0] != "Red" || itx.Options[1] != "Blue" {
		t.Fatalf("options = %v, want [Red Blue]", itx.Options)
	}
}

func TestStreamJSONAskUserQuestionMultiple(t *testing.T) {
	p := parser.NewStreamJSON()
	rec := `{"type":"control_request","request_id":"perm_qm",` +
		`"request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion",` +
		`"input":{"questions":[` +
		`{"question":"Favorite color?","header":"Color","options":[{"label":"Red"},{"label":"Blue"}]},` +
		`{"question":"Favorite animal?","header":"Animal","options":[{"label":"Cat"},{"label":"Dog"}]}]},` +
		`"tool_use_id":"t1"}}`

	evs := collect(t, p, rec)
	if len(evs) != 1 || evs[0].Interaction == nil {
		t.Fatalf("events = %+v", evs)
	}
	itx := evs[0].Interaction
	if len(itx.Questions) != 2 {
		t.Fatalf("questions = %+v, want 2", itx.Questions)
	}
	q := itx.Questions[1]
	if q.Question != "Favorite animal?" || q.Header != "Animal" {
		t.Fatalf("second question = %+v", q)
	}
	if len(q.Options) != 2 || q.Options[0] != "Cat" || q.Options[1] != "Dog" {
		t.Fatalf("second options = %v, want [Cat Dog]", q.Options)
	}
}
