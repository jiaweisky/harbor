package parser

import (
	"encoding/json"
	"errors"

	"github.com/jiaweisky/harbor/internal/agent"
)

// Codex parses the JSONL emitted by `codex exec --json`. Each line is an
// envelope with a "type"; item-level events carry an "item" object whose "type"
// is one of agent_message, reasoning, command_execution, file_change, or error.
//
// Only a terminal `turn.failed` is reported as an error: the transient `error`
// records codex emits while reconnecting are not failures.
type Codex struct{}

// NewCodex creates a codex JSONL parser.
func NewCodex() *Codex { return &Codex{} }

// codexItem is one item of a completed turn. The known fields are decoded for
// the common item types; Raw keeps the whole item so file_change events can
// forward codex's own change structure verbatim.
type codexItem struct {
	Type    string          `json:"type"`
	Text    string          `json:"text"`
	Content string          `json:"content"`
	Command string          `json:"command"`
	Raw     json.RawMessage `json:"-"`
}

// UnmarshalJSON decodes the known item fields and captures the raw item.
func (i *codexItem) UnmarshalJSON(data []byte) error {
	type alias codexItem
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*i = codexItem(a)
	i.Raw = append(i.Raw, data...)
	return nil
}

// codexUsage carries the token counts codex reports on a completed turn.
type codexUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type codexEvent struct {
	Type     string     `json:"type"`
	ThreadID string     `json:"thread_id"`
	Item     *codexItem `json:"item"`
	Usage    *codexUsage `json:"usage"`
	Error    *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Parse handles one JSONL record.
func (p *Codex) Parse(rec []byte, out func(agent.Event)) error {
	var env codexEvent
	if err := json.Unmarshal(rec, &env); err != nil {
		return nil // codex output is strictly JSONL; skip anything else
	}

	switch env.Type {
	case "thread.started":
		if env.ThreadID != "" {
			out(agent.Event{Kind: agent.EventSession, SessionID: env.ThreadID, Raw: rec})
		}
	case "turn.completed":
		if env.Usage != nil {
			out(agent.Event{Kind: agent.EventUsage, Usage: &agent.Usage{
				InputTokens: env.Usage.InputTokens, OutputTokens: env.Usage.OutputTokens,
			}, Raw: rec})
		}
		out(agent.Event{Kind: agent.EventDone, Raw: rec})
	case "turn.failed":
		out(agent.Event{Kind: agent.EventError, Err: codexError(env.Error), Raw: rec})
	case "item.completed":
		emitCodexItem(env.Item, rec, out)
	}
	return nil
}

// Flush implements agent.Parser.
func (p *Codex) Flush(func(agent.Event)) error { return nil }

func emitCodexItem(item *codexItem, rec []byte, out func(agent.Event)) {
	if item == nil {
		return
	}
	switch item.Type {
	case "agent_message":
		if text := firstNonEmpty(item.Text, item.Content); text != "" {
			out(agent.Event{Kind: agent.EventTextDelta, Text: text, Raw: rec})
		}
	case "reasoning":
		if text := firstNonEmpty(item.Text, item.Content); text != "" {
			out(agent.Event{Kind: agent.EventThinking, Text: text, Raw: rec})
		}
	case "command_execution":
		out(agent.Event{Kind: agent.EventToolCall, Tool: "shell",
			ToolInput: map[string]any{"command": item.Command}, Raw: rec})
	case "file_change":
		// The item carries codex's own change structure (per-file kinds and
		// diffs); forward it verbatim as the tool input so the client sees the
		// modification exactly as codex reported it.
		input := map[string]any{}
		_ = json.Unmarshal(item.Raw, &input)
		out(agent.Event{Kind: agent.EventToolCall, Tool: "file_change", ToolInput: input, Raw: rec})
	}
}

func codexError(e *struct {
	Message string `json:"message"`
}) error {
	if e != nil && e.Message != "" {
		return errors.New(e.Message)
	}
	return errors.New("turn failed")
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
