// Package adapters holds per-CLI implementations of Harbor's CLI-specific
// authentication behavior. Each supported coding-agent CLI lives in its own
// file; the table below is the single place that maps a binary to it.
package adapters

import (
	"path/filepath"

	"github.com/jiaweisky/harbor/internal/agent"
)

// Factory builds the authenticator for one CLI version. The version lets an
// adapter branch on a breaking change (e.g. a status output format change)
// without touching any other package.
type Factory func(version string) agent.Authenticator

var authenticators = map[string]Factory{
	"claude":    func(string) agent.Authenticator { return claude{} },
	"codebuddy": func(string) agent.Authenticator { return codebuddy{} },
	"codex":     func(string) agent.Authenticator { return codex{} },
	"opencode":  func(string) agent.Authenticator { return opencode{} },
}

// AuthFor returns the authenticator for a CLI binary path or name at the given
// version.
func AuthFor(binary, version string) (agent.Authenticator, bool) {
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
