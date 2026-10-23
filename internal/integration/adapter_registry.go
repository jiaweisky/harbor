package integration

import (
	"context"
	"errors"
	"sort"
	"sync"

	"github.com/jiaweisky/harbor/internal/agent"
	"github.com/jiaweisky/harbor/internal/core"
)

// ErrUnhandled signals that an AdapterBuilder does not handle the agent's
// integration, so the caller should fall back to the built-in construction.
var ErrUnhandled = errors.New("adapter does not handle this agent")

// AdapterOptions carries the deployment environment a launcher is built in. It
// mirrors the runner-affecting settings, not the agent's own configuration
// (which is passed separately as the AgentSpec).
type AdapterOptions struct {
	InheritEnv bool
	CLIHome    string
	Container  *agent.ContainerSpec
	// Runtime supplies orphan tracking and raw stream logging (may be nil).
	Runtime *agent.Runtime
	// Lifecycle bounds a spawned long-running server process (integration
	// "server"); it is nil for per-task launchers.
	Lifecycle context.Context
}

// AdapterBuilder builds a Launcher for one configured agent. It is the
// compile-time extension point for a CLI whose integration needs dedicated
// handling (a handshake, a bespoke transport, multi-turn stdin): register it
// with RegisterAdapter from the adapter's init, so adding a CLI needs no change
// to a central switch. Return ErrUnhandled for integrations it does not handle.
type AdapterBuilder func(ac core.AgentSpec, opts AdapterOptions) (agent.Launcher, error)

var (
	adapterMu       sync.RWMutex
	adapterBuilders = map[string]AdapterBuilder{}
)

// RegisterAdapter associates a builder with a CLI binary base name. Registering
// the same name again replaces the earlier builder.
func RegisterAdapter(binary string, b AdapterBuilder) {
	adapterMu.Lock()
	defer adapterMu.Unlock()
	adapterBuilders[binary] = b
}

// LookupAdapter returns the builder registered for a CLI binary base name.
func LookupAdapter(binary string) (AdapterBuilder, bool) {
	adapterMu.RLock()
	defer adapterMu.RUnlock()
	b, ok := adapterBuilders[binary]
	return b, ok
}

// AdapterNames lists the registered CLI binary base names, sorted.
func AdapterNames() []string {
	adapterMu.RLock()
	defer adapterMu.RUnlock()
	names := make([]string, 0, len(adapterBuilders))
	for name := range adapterBuilders {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
