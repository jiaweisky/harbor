// Package adapters holds per-CLI implementations of Harbor's CLI-specific
// behavior. Each supported coding-agent CLI lives in its own file and registers
// itself by binary base name, so the rest of Harbor stays CLI-agnostic.
package adapters

import (
	"path/filepath"
	"sync"

	"github.com/jiaweisky/harbor/internal/agent"
)

// Factory builds the authenticator for one CLI version. The version lets an
// adapter branch on a breaking change (e.g. a status output format change)
// without touching any other package.
type Factory func(version string) agent.Authenticator

var (
	mu       sync.RWMutex
	registry = map[string]Factory{}
)

// Register associates a CLI binary base name with its authenticator factory. It
// is called from each adapter's init.
func Register(binary string, f Factory) {
	mu.Lock()
	registry[binary] = f
	mu.Unlock()
}

// AuthFor returns the authenticator for a CLI binary path or name at the given
// version.
func AuthFor(binary, version string) (agent.Authenticator, bool) {
	mu.RLock()
	f, ok := registry[filepath.Base(binary)]
	mu.RUnlock()
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
