package parser

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jiaweisky/harbor/internal/agent"
)

// builders maps a parser name to its constructor. The regex parser needs a
// pattern (from config); the others ignore it.
var builders = map[string]func(pattern string) (agent.Parser, error){
	"text": func(string) (agent.Parser, error) {
		return NewText(), nil
	},
	"stream-json": func(string) (agent.Parser, error) {
		return NewStreamJSON(), nil
	},
	"codex": func(string) (agent.Parser, error) {
		return NewCodex(), nil
	},
	"regex": func(pattern string) (agent.Parser, error) {
		if pattern == "" {
			return nil, fmt.Errorf("parser %q requires regex.text", "regex")
		}
		return NewRegex(pattern)
	},
}

// Build constructs a parser by name.
func Build(name, pattern string) (agent.Parser, error) {
	build, ok := builders[name]
	if !ok {
		return nil, fmt.Errorf("unknown parser %q (known: %s)", name, strings.Join(Names(), ", "))
	}
	return build(pattern)
}

// Names lists the parser names accepted by Build.
func Names() []string {
	names := make([]string, 0, len(builders))
	for name := range builders {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
