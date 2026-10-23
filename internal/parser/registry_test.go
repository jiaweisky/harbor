package parser_test

import (
	"testing"

	"github.com/jiaweisky/harbor/internal/core"
	"github.com/jiaweisky/harbor/internal/parser"
)

func TestBuiltinParsersRegistered(t *testing.T) {
	for _, tc := range []struct {
		name    core.ParserKind
		pattern string
	}{
		{"text", ""},
		{"stream-json", ""},
		{"codex", ""},
		{"regex", "^(.+)$"},
	} {
		if _, err := parser.Build(tc.name, tc.pattern); err != nil {
			t.Errorf("Build(%q): %v", tc.name, err)
		}
	}
}

func TestRegexRequiresPattern(t *testing.T) {
	if _, err := parser.Build("regex", ""); err == nil {
		t.Fatal("regex without a pattern should fail")
	}
}

func TestUnknownParser(t *testing.T) {
	if _, err := parser.Build("nope", ""); err == nil {
		t.Fatal("an unknown parser should fail")
	}
}
