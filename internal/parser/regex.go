package parser

import (
	"fmt"
	"regexp"

	"github.com/jiaweisky/harbor/internal/agent"
)

// Regex extracts text from each output line using a pattern. When the pattern
// has a capture group, the first group is emitted; otherwise the whole match.
type Regex struct {
	re *regexp.Regexp
}

// NewRegex compiles a regex parser.
func NewRegex(pattern string) (*Regex, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("regex parser: %w", err)
	}

	return &Regex{re: re}, nil
}

// Parse emits the first capture (or the whole match) of each line, if any.
func (p *Regex) Parse(rec []byte, out func(agent.Event)) error {
	m := p.re.FindSubmatch(rec)
	if m == nil {
		return nil
	}

	text := m[0]
	if len(m) > 1 {
		text = m[1]
	}

	if len(text) > 0 {
		out(agent.Event{Kind: agent.EventTextDelta, Text: string(text) + "\n", Raw: rec})
	}

	return nil
}

// Flush implements agent.Parser.
func (p *Regex) Flush(func(agent.Event)) error { return nil }
