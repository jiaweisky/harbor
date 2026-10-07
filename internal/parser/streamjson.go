// Package parser normalizes raw coding-agent CLI output into agent.Events.
package parser

import (
	"encoding/json"
	"errors"

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
			out(agent.Event{Kind: agent.EventThinking, Text: b.text, Raw: rec})
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

// parseResult handles the terminal result record.
func (p *StreamJSON) parseResult(rec []byte, out func(agent.Event)) {
	var r struct {
		Result    string `json:"result"`
		IsError   bool   `json:"is_error"`
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(rec, &r)
	if r.IsError {
		out(agent.Event{Kind: agent.EventError, Err: errors.New(r.Result), Raw: rec})
		return
	}
	if !p.sawDelta {
		out(agent.Event{Kind: agent.EventTextFinal, Text: r.Result, SessionID: r.SessionID, Raw: rec})
	}
	out(agent.Event{Kind: agent.EventDone, Raw: rec})
}

type contentBlock struct {
	typ   string
	text  string
	name  string
	input map[string]any
}

// contentBlocks extracts message content blocks from a record.
func contentBlocks(rec []byte) []contentBlock {
	var msg struct {
		Message struct {
			Content []struct {
				Type  string         `json:"type"`
				Text  string         `json:"text"`
				Name  string         `json:"name"`
				Input map[string]any `json:"input"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(rec, &msg); err != nil {
		return nil
	}
	blocks := make([]contentBlock, 0, len(msg.Message.Content))
	for _, c := range msg.Message.Content {
		blocks = append(blocks, contentBlock{typ: c.Type, text: c.Text, name: c.Name, input: c.Input})
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
