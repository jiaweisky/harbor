package parser_test

import (
	"testing"

	"github.com/jiaweisky/harbor/internal/agent"
	"github.com/jiaweisky/harbor/internal/parser"
)

func TestBuiltinParsersRegistered(t *testing.T) {
	for _, tc := range []struct{ name, pattern string }{
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

// testParser is a minimal custom parser registered from a test.
type testParser struct{}

func (testParser) Parse(rec []byte, out func(agent.Event)) error {
	out(agent.Event{Kind: agent.EventTextDelta, Text: string(rec)})
	return nil
}

func (testParser) Flush(func(agent.Event)) error { return nil }

func TestRegisterCustomParser(t *testing.T) {
	parser.RegisterParser("test-format", func(string) (agent.Parser, error) { return testParser{}, nil })

	p, err := parser.Build("test-format", "")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	var got []agent.Event
	if err := p.Parse([]byte("hi"), func(e agent.Event) { got = append(got, e) }); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got) != 1 || got[0].Text != "hi" {
		t.Fatalf("got %+v", got)
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
