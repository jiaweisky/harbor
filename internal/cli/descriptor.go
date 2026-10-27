package cli

import (
	"context"
	"errors"

	"github.com/jiaweisky/harbor/internal/agent"
	"github.com/jiaweisky/harbor/internal/core"
	"github.com/jiaweisky/harbor/internal/sandbox"
)

// ErrUnhandled signals that a Builder does not handle the agent's integration,
// so the caller should fall back to the built-in generic construction.
var ErrUnhandled = errors.New("adapter does not handle this agent")

// Options carries the deployment environment a launcher is built in.
type Options struct {
	InheritEnv bool
	CLIHome    string
	Container  *sandbox.Container
	Runtime    *agent.Runtime
	// Ctx bounds a spawned long-running server process (integration "server");
	// it is nil for per-task launchers.
	Ctx context.Context
}

// Builder builds a Launcher for one configured agent. Return ErrUnhandled for
// integrations it does not handle.
type Builder func(ac core.AgentSpec, opts Options) (agent.Launcher, error)

// AuthFactory builds the authenticator for one CLI version. The version lets a
// factory branch on a breaking change without touching other packages.
type AuthFactory func(version string) agent.Authenticator

// Descriptor is the single registry entry for one CLI: its profile, auth
// factory, and optional dedicated launcher builder.
type Descriptor struct {
	Binary  string
	Profile Profile
	Auth    AuthFactory
	Build   Builder
}

// descriptors is the process-wide CLI registry. Built-ins are populated in
// init; plugins may add entries at startup. It is lookup state, not per-task
// mutable state.
var descriptors = map[string]Descriptor{}

func init() {
	for _, p := range knownProfiles {
		binary := p.Binary
		descriptors[binary] = Descriptor{
			Binary:  binary,
			Profile: p,
			Auth: func(version string) agent.Authenticator {
				if a, ok := authFor(binary, version); ok {
					return a
				}
				return Unverified()
			},
		}
	}
}

// LookupDescriptor returns the descriptor for a binary path or name.
func LookupDescriptor(binary string) (Descriptor, bool) {
	d, ok := descriptors[baseName(binary)]
	return d, ok
}

// AuthFor returns the CLI's authenticator at the given version.
func AuthFor(binary, version string) agent.Authenticator {
	if d, ok := LookupDescriptor(binary); ok && d.Auth != nil {
		return d.Auth(version)
	}

	return Unverified()
}

// AttachBuilder attaches a dedicated launcher builder to a known CLI.
func AttachBuilder(binary string, build Builder) {
	binary = baseName(binary)

	d, ok := LookupDescriptor(binary)
	if !ok {
		d = Descriptor{Binary: binary}
	}

	d.Build = build
	descriptors[d.Binary] = d
}

// Register adds or replaces a descriptor. Plugins use this to add a CLI Harbor
// does not know about.
func Register(d Descriptor) {
	d.Binary = baseName(d.Binary)
	descriptors[d.Binary] = d
}

// LookupProfile returns the profile for a binary, preferring a registered
// descriptor over the built-in table.
func LookupProfile(binary string) (Profile, bool) {
	if d, ok := LookupDescriptor(binary); ok && d.Profile.Binary != "" {
		return d.Profile, true
	}

	return Lookup(binary)
}
