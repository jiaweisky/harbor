package parser

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/jiaweisky/harbor/internal/agent"
)

// StreamJSON parses newline-delimited JSON emitted by a CLI's structured stream
// mode. It prefers incremental deltas over full assistant messages so that,
// when partial streaming is enabled, text is not emitted twice.
type StreamJSON struct {
	sawDelta bool
}

// NewStreamJSON creates a stateful stream-json parser.
func NewStreamJSON() *StreamJSON { return &StreamJSON{} }

// Parse handles one NDJSON record.
func (p *StreamJSON) Parse(rec []byte, out func(agent.Event)) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec, &raw); err != nil {
		out(agent.Event{Kind: agent.EventTextDelta, Text: string(rec) + "\n", Raw: rec})
		return nil
	}
	var typ string
	_ = json.Unmarshal(raw["type"], &typ)

	switch typ {
	case "system":
		p.parseSystem(rec, out)
	case "stream_event":
		p.parseStreamEvent(rec, out)
	case "assistant":
		p.parseAssistant(rec, out)
	case "user":
		p.parseUser(rec, out)
	case "control_request":
		p.parseControlRequest(rec, out)
	case "result":
		p.parseResult(rec, out)
	}
	return nil
}

// Flush implements agent.Parser.
func (p *StreamJSON) Flush(func(agent.Event)) error { return nil }

// parseStreamEvent handles an incremental text delta.
func (p *StreamJSON) parseStreamEvent(rec []byte, out func(agent.Event)) {
	p.sawDelta = true
	if text := deltaText(rec); text != "" {
		out(agent.Event{Kind: agent.EventTextDelta, Text: text, Raw: rec})
	}
}

// parseSystem captures the CLI session id.
func (p *StreamJSON) parseSystem(rec []byte, out func(agent.Event)) {
	var s struct {
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(rec, &s)
	out(agent.Event{Kind: agent.EventSession, SessionID: s.SessionID, Raw: rec})
}

// parseAssistant handles a complete assistant message (text / thinking / tool call).
// Text is skipped once a delta has been seen to avoid duplication.
func (p *StreamJSON) parseAssistant(rec []byte, out func(agent.Event)) {
	for _, b := range contentBlocks(rec) {
		switch b.typ {
		case "text":
			if b.text != "" && !p.sawDelta {
				out(agent.Event{Kind: agent.EventTextDelta, Text: b.text, Raw: rec})
			}
		case "thinking":
			// Claude puts reasoning under "thinking"; fall back to "text".
			if text := firstNonEmpty(b.thinking, b.text); text != "" {
				out(agent.Event{Kind: agent.EventThinking, Text: text, Raw: rec})
			}
		case "tool_use":
			out(agent.Event{Kind: agent.EventToolCall, Tool: b.name, ToolInput: b.input, Raw: rec})
		}
	}
}

// parseUser surfaces tool results echoed back by the CLI (best effort).
func (p *StreamJSON) parseUser(rec []byte, out func(agent.Event)) {
	for _, b := range contentBlocks(rec) {
		if b.typ == "tool_result" {
			out(agent.Event{Kind: agent.EventToolResult, Raw: rec})
		}
	}
}

// parseControlRequest handles a CLI control_request. A can_use_tool request is
// surfaced as an input-required interaction — a question for AskUserQuestion, a
// permission otherwise; other subtypes have no Harbor-side handler and are
// ignored.
func (p *StreamJSON) parseControlRequest(rec []byte, out func(agent.Event)) {
	var m struct {
		RequestID string `json:"request_id"`
		Request   struct {
			Subtype  string         `json:"subtype"`
			ToolName string         `json:"tool_name"`
			Input    map[string]any `json:"input"`
		} `json:"request"`
	}
	if json.Unmarshal(rec, &m) != nil || m.Request.Subtype != "can_use_tool" || m.RequestID == "" {
		return
	}
	if m.Request.ToolName == "AskUserQuestion" {
		out(questionEvent(m.RequestID, m.Request.Input, rec))
		return
	}
	out(permissionEvent(m.RequestID, m.Request.ToolName, m.Request.Input, rec))
}

// permissionEvent builds the input-required event for a tool permission request.
func permissionEvent(id, tool string, input map[string]any, raw []byte) agent.Event {
	return agent.Event{
		Kind:      agent.EventPermissionRequest,
		Text:      permissionText(tool, input),
		Tool:      tool,
		ToolInput: input,
		Interaction: &agent.Interaction{
			Kind:    agent.InteractionPermission,
			ID:      id,
			Action:  tool,
			Target:  permissionTarget(input),
			Options: []string{"allow", "deny"},
			Resume:  true,
		},
		Raw: raw,
	}
}

// questionEvent builds the input-required event for an AskUserQuestion request.
func questionEvent(id string, input map[string]any, raw []byte) agent.Event {
	qs := askUserQuestions(input)
	return agent.Event{
		Kind:      agent.EventPermissionRequest,
		Text:      questionText(qs),
		Tool:      "AskUserQuestion",
		ToolInput: input,
		Interaction: &agent.Interaction{
			Kind:    agent.InteractionQuestion,
			ID:      id,
			Action:  "question",
			Target:  firstQuestionText(qs),
			Options: firstQuestionOptions(qs),
			Resume:  true,
		},
		Raw: raw,
	}
}

// askUserQuestion is one question of an AskUserQuestion tool input
// ({questions:[{question, header, options:[{label, ...}], multiSelect}]}).
type askUserQuestion struct {
	Question string `json:"question"`
	Options  []struct {
		Label string `json:"label"`
	} `json:"options"`
}

// askUserQuestions parses the "questions" array of an AskUserQuestion input.
func askUserQuestions(input map[string]any) []askUserQuestion {
	b, err := json.Marshal(input["questions"])
	if err != nil {
		return nil
	}
	var qs []askUserQuestion
	_ = json.Unmarshal(b, &qs)
	return qs
}

// firstQuestionText returns the first question's text (the answer key).
func firstQuestionText(qs []askUserQuestion) string {
	if len(qs) == 0 {
		return ""
	}
	return qs[0].Question
}

// firstQuestionOptions returns the first question's option labels.
func firstQuestionOptions(qs []askUserQuestion) []string {
	if len(qs) == 0 {
		return nil
	}
	return questionLabels(qs[0])
}

// questionLabels returns a question's non-empty option labels.
func questionLabels(q askUserQuestion) []string {
	labels := make([]string, 0, len(q.Options))
	for _, o := range q.Options {
		if o.Label != "" {
			labels = append(labels, o.Label)
		}
	}
	return labels
}

// questionText renders the questions and their options as a prompt.
func questionText(qs []askUserQuestion) string {
	if len(qs) == 0 {
		return "the agent is asking a question"
	}
	var b strings.Builder
	for i, q := range qs {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(q.Question)
		if labels := questionLabels(q); len(labels) > 0 {
			b.WriteString(" [" + strings.Join(labels, ", ") + "]")
		}
	}
	return b.String()
}

// permissionText renders a human-readable prompt for a tool permission request.
func permissionText(tool string, input map[string]any) string {
	s := "permission required: " + tool
	if t := permissionTarget(input); t != "" {
		s += " (" + t + ")"
	}
	return s + " — reply allow or deny"
}

// permissionTarget picks the most descriptive string in a tool's input.
func permissionTarget(input map[string]any) string {
	for _, k := range []string{"command", "file_path", "path", "pattern", "url"} {
		if v, ok := input[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// parseResult handles the terminal result record.
func (p *StreamJSON) parseResult(rec []byte, out func(agent.Event)) {
	var r struct {
		Result    string  `json:"result"`
		IsError   bool    `json:"is_error"`
		SessionID string  `json:"session_id"`
		CostUSD   float64 `json:"total_cost_usd"`
		Usage     *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	_ = json.Unmarshal(rec, &r)
	if r.IsError {
		out(agent.Event{Kind: agent.EventError, Err: errors.New(r.Result), Raw: rec})
		return
	}
	if !p.sawDelta {
		out(agent.Event{Kind: agent.EventTextFinal, Text: r.Result, SessionID: r.SessionID, Raw: rec})
	}
	if r.Usage != nil || r.CostUSD > 0 {
		u := &agent.Usage{CostUSD: r.CostUSD}
		if r.Usage != nil {
			u.InputTokens = r.Usage.InputTokens
			u.OutputTokens = r.Usage.OutputTokens
		}
		out(agent.Event{Kind: agent.EventUsage, Usage: u, Raw: rec})
	}
	out(agent.Event{Kind: agent.EventDone, Raw: rec})
}

type contentBlock struct {
	typ      string
	text     string
	thinking string
	name     string
	input    map[string]any
}

// contentBlocks extracts message content blocks from a record.
func contentBlocks(rec []byte) []contentBlock {
	var msg struct {
		Message struct {
			Content []struct {
				Type     string         `json:"type"`
				Text     string         `json:"text"`
				Thinking string         `json:"thinking"`
				Name     string         `json:"name"`
				Input    map[string]any `json:"input"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(rec, &msg); err != nil {
		return nil
	}
	blocks := make([]contentBlock, 0, len(msg.Message.Content))
	for _, c := range msg.Message.Content {
		blocks = append(blocks, contentBlock{
			typ: c.Type, text: c.Text, thinking: c.Thinking, name: c.Name, input: c.Input,
		})
	}
	return blocks
}

// deltaText extracts the text from a streamed partial-message event.
func deltaText(rec []byte) string {
	var e struct {
		Event struct {
			Delta struct {
				Text string `json:"text"`
			} `json:"delta"`
		} `json:"event"`
	}
	if err := json.Unmarshal(rec, &e); err != nil {
		return ""
	}
	return e.Event.Delta.Text
}

func init() {
	RegisterParser("stream-json", func(string) (agent.Parser, error) { return NewStreamJSON(), nil })
}
