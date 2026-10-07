package version_test

import (
	"testing"

	"github.com/jiaweisky/harbor/internal/version"
)

func TestParse(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"2.161.4", "v2.161.4", true},
		{"2.1.292 (Claude Code)", "v2.1.292", true},
		{"codex-cli 0.160.1", "v0.160.1", true},
		{"no version here", "", false},
	}
	for _, c := range cases {
		got, ok := version.Parse(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("Parse(%q) = (%q,%v), want (%q,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestLess(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
		ok   bool
	}{
		{"2.0.0", "2.161.4", true, true},
		{"2.1.292 (Claude Code)", "2.1.292", false, true},
		{"3.0.0", "2.161.4", false, true},
		{"bogus", "2.0.0", false, false},
	}
	for _, c := range cases {
		got, ok := version.Less(c.a, c.b)
		if ok != c.ok || got != c.want {
			t.Errorf("Less(%q,%q) = (%v,%v), want (%v,%v)", c.a, c.b, got, ok, c.want, c.ok)
		}
	}
}
