// Package parser normalizes raw coding-agent CLI output into agent.Events.
// The supported formats are built into this package; Build is the only entry
// point.
package parser

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jiaweisky/harbor/internal/agent"
	"github.com/jiaweisky/harbor/internal/core"
)

var builders = map[core.ParserKind]func(pattern string) (agent.Parser, error){
	core.ParserText: func(string) (agent.Parser, error) {
		return NewText(), nil
	},
	core.ParserStreamJSON: func(string) (agent.Parser, error) {
		return NewStreamJSON(), nil
	},
	core.ParserCodex: func(string) (agent.Parser, error) {
		return NewCodex(), nil
	},
	core.ParserRegex: func(pattern string) (agent.Parser, error) {
		if pattern == "" {
			return nil, fmt.Errorf(`parser "regex" requires regex.text`)
		}
		return NewRegex(pattern)
	},
}

// Build constructs a parser by name. pattern carries the configuration for
// parsers that need one (for example "regex"); others ignore it.
func Build(name core.ParserKind, pattern string) (agent.Parser, error) {
	if name == "" {
		name = core.ParserText
	}
	build, ok := builders[name]
	if !ok {
		return nil, fmt.Errorf("unknown parser %q (known: %s)", name, strings.Join(Names(), ", "))
	}
	return build(pattern)
}

// Names lists the built-in parser names, sorted.
func Names() []string {
	names := make([]string, 0, len(builders))
	for name := range builders {
		names = append(names, string(name))
	}
	sort.Strings(names)
	return names
}
