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
	sawDelta bool // a streamed text delta was seen; skip the duplicate full-message text
	sawText  bool // text was emitted; suppress the result's final-text fallback
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

// parseStreamEvent handles an incremental delta: assistant text or extended
// thinking. Forwarding thinking keeps a long reasoning phase visible (and an
// otherwise silent stream from looking idle to a liveness watchdog).
func (p *StreamJSON) parseStreamEvent(rec []byte, out func(agent.Event)) {
	text, thinking := deltaText(rec)

	switch {
	case thinking != "":
		out(agent.Event{Kind: agent.EventThinking, Text: thinking, Raw: rec})
	case text != "":
		p.sawDelta = true
		p.sawText = true
		out(agent.Event{Kind: agent.EventTextDelta, Text: text, Raw: rec})
	}
}

// parseSystem captures the CLI session id. Real CLIs emit several system
// subtypes over one run (claude sends thinking_tokens progress records, for
// example); only the init announcement binds the session.
func (p *StreamJSON) parseSystem(rec []byte, out func(agent.Event)) {
	var s struct {
		Subtype   string `json:"subtype"`
		SessionID string `json:"session_id"`
	}

	_ = json.Unmarshal(rec, &s)
	if s.Subtype != "init" || s.SessionID == "" {
		return
	}
	out(agent.Event{Kind: agent.EventSession, SessionID: s.SessionID, Raw: rec})
}

// parseAssistant handles a complete assistant message (text / thinking / tool
// call). Its text is skipped only when a streamed delta already covered it.
func (p *StreamJSON) parseAssistant(rec []byte, out func(agent.Event)) {
	for _, b := range contentBlocks(rec) {
		switch b.typ {
		case "text":
			if b.text != "" && !p.sawDelta {
				out(agent.Event{Kind: agent.EventTextDelta, Text: b.text, Raw: rec})
				p.sawText = true
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

// parseUser surfaces tool results echoed back by the CLI (best effort): the
// tool's text output travels on the event so the client sees what the tool
// produced, not just that it ran.
func (p *StreamJSON) parseUser(rec []byte, out func(agent.Event)) {
	for _, b := range contentBlocks(rec) {
		if b.typ == "tool_result" {
			out(agent.Event{Kind: agent.EventToolResult, Text: b.contentText, Raw: rec})
		}
	}
}

// parseResult handles the terminal result record.
func (p *StreamJSON) parseResult(rec []byte, out func(agent.Event)) {
	var r struct {
		Result           string          `json:"result"`
		IsError          bool            `json:"is_error"`
		SessionID        string          `json:"session_id"`
		CostUSD          float64         `json:"total_cost_usd"`
		StructuredOutput json.RawMessage `json:"structured_output"`
		Usage            *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}

	_ = json.Unmarshal(rec, &r)
	if r.IsError {
		out(agent.Event{Kind: agent.EventError, Err: errors.New(r.Result), Raw: rec})
		return
	}

	if data := structuredData(r.StructuredOutput); data != nil {
		out(agent.Event{Kind: agent.EventStructuredOutput, Data: data, Raw: rec})
	}

	if !p.sawText {
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

// structuredData decodes a result's structured_output object, or nil when it is
// absent or not a JSON object.
func structuredData(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}

	var data map[string]any
	if json.Unmarshal(raw, &data) != nil || data == nil {
		return nil
	}

	return data
}

type contentBlock struct {
	typ         string
	text        string
	thinking    string
	name        string
	input       map[string]any
	contentText string // text carried by a tool_result block
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
				Content  json.RawMessage `json:"content"`
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
			contentText: toolResultText(c.Content),
		})
	}

	return blocks
}

// toolResultText extracts the text of a tool_result block's content, which the
// CLIs send either as a plain string or as an array of content blocks.
func toolResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}

	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}

	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if b.Text != "" {
			parts = append(parts, b.Text)
		}
	}

	return strings.Join(parts, "\n")
}

// deltaText extracts the assistant text and extended-thinking text from a
// streamed partial-message delta (either may be empty).
func deltaText(rec []byte) (text, thinking string) {
	var e struct {
		Event struct {
			Delta struct {
				Text     string `json:"text"`
				Thinking string `json:"thinking"`
			} `json:"delta"`
		} `json:"event"`
	}

	if err := json.Unmarshal(rec, &e); err != nil {
		return "", ""
	}

	return e.Event.Delta.Text, e.Event.Delta.Thinking
}
