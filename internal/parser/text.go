package parser

import "github.com/jiaweisky/harbor/internal/agent"

// Text emits one text delta per stdout line (best-effort line streaming).
type Text struct{}

// NewText creates a plain-text line parser.
func NewText() *Text { return &Text{} }

// Parse emits the record as a text delta.
func (p *Text) Parse(rec []byte, out func(agent.Event)) error {
	out(agent.Event{Kind: agent.EventTextDelta, Text: string(rec) + "\n", Raw: rec})
	return nil
}

// Flush implements agent.Parser.
func (p *Text) Flush(func(agent.Event)) error { return nil }

func init() {
	RegisterParser("text", func(string) (agent.Parser, error) { return NewText(), nil })
}
