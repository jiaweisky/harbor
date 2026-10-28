package cli

import (
	"path/filepath"

	"github.com/jiaweisky/harbor/internal/agent"
)

// authenticators maps a CLI binary to its auth factory. The factory receives the
// probed version so an adapter can branch on a breaking output-format change.
var authenticators = map[string]AuthFactory{
	"claude":    func(string) agent.Authenticator { return claude{} },
	"codebuddy": func(string) agent.Authenticator { return codebuddy{} },
	"codex":     func(string) agent.Authenticator { return codex{} },
	"opencode":  func(string) agent.Authenticator { return opencode{} },
}

// authFor returns the authenticator for a CLI binary path or name at the given
// version.
func authFor(binary, version string) (agent.Authenticator, bool) {
	f, ok := authenticators[filepath.Base(binary)]
	if !ok {
		return nil, false
	}

	return f(version), true
}

// Unverified returns the authenticator for a CLI Harbor has no built-in adapter
// for: login state cannot be determined, so it never blocks.
func Unverified() agent.Authenticator { return unverified{} }

type unverified struct{}

func (unverified) LoggedIn(string, map[string]string) bool { return true }

func (unverified) StatusArgs() []string { return nil }

func (unverified) LoggedInStatus(string, error) bool { return true }
