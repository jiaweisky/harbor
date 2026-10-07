package parser_test

import (
	"testing"

	"github.com/jiaweisky/harbor/internal/agent"
	"github.com/jiaweisky/harbor/internal/parser"
)

func TestRegexParse(t *testing.T) {
	p, err := parser.NewRegex(`^OUT:\s*(.+)$`)
	if err != nil {
		t.Fatal(err)
	}

	if evs := collect(t, p, "OUT: hello"); len(evs) != 1 ||
		evs[0].Kind != agent.EventTextDelta || evs[0].Text != "hello\n" {
		t.Fatalf("match: %+v", evs)
	}
	if evs := collect(t, p, "noise line"); len(evs) != 0 {
		t.Fatalf("non-match should emit nothing: %+v", evs)
	}
}

func TestBuildRegexRequiresPattern(t *testing.T) {
	if _, err := parser.Build("regex", ""); err == nil {
		t.Fatal("expected an error when regex.text is missing")
	}
}

func TestBuildUnknownParser(t *testing.T) {
	if _, err := parser.Build("bogus", ""); err == nil {
		t.Fatal("expected an error for an unknown parser")
	}
}
