package agent_test

import (
	"testing"

	"github.com/jiaweisky/harbor/internal/agent"
)

func TestListenURL(t *testing.T) {
	cases := []struct {
		line string
		want string
		ok   bool
	}{
		{"opencode server listening on http://127.0.0.1:5099\n", "http://127.0.0.1:5099", true},
		{"  \x1b[38;5;241mEndpoint\x1b[39m  http://127.0.0.1:8123", "http://127.0.0.1:8123", true},
		{"opencode server starting", "", false},
	}
	for _, c := range cases {
		got, ok := agent.ListenURL(c.line)
		if ok != c.ok || got != c.want {
			t.Errorf("ListenURL(%q) = %q, %v; want %q, %v", c.line, got, ok, c.want, c.ok)
		}
	}
}
