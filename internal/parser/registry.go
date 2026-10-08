// Package parser normalizes raw coding-agent CLI output into agent.Events.
//
// Parsers register themselves by name, so a new output format is a new file
// that calls RegisterParser from its init, so no central switch needs editing.
package parser

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/jiaweisky/harbor/internal/agent"
)

// Builder constructs a parser. pattern carries the configuration for parsers
// that need one (for example "regex"); others ignore it.
type Builder func(pattern string) (agent.Parser, error)

var (
	mu       sync.RWMutex
	builders = map[string]Builder{}
)

// RegisterParser associates a parser name with its builder. It is called from a
// parser's init.
func RegisterParser(name string, b Builder) {
	mu.Lock()
	builders[name] = b
	mu.Unlock()
}

// Build constructs a parser by name.
func Build(name, pattern string) (agent.Parser, error) {
	mu.RLock()
	build, ok := builders[name]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown parser %q (known: %s)", name, strings.Join(Names(), ", "))
	}
	return build(pattern)
}

// Names lists the registered parser names.
func Names() []string {
	mu.RLock()
	names := make([]string, 0, len(builders))
	for name := range builders {
		names = append(names, name)
	}
	mu.RUnlock()
	sort.Strings(names)
	return names
}
