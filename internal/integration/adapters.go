package integration

import (
	"net/http"

	"github.com/jiaweisky/harbor/internal/acp"
	"github.com/jiaweisky/harbor/internal/agent"
	"github.com/jiaweisky/harbor/internal/codebuddy"
	"github.com/jiaweisky/harbor/internal/codex"
	"github.com/jiaweisky/harbor/internal/config"
	"github.com/jiaweisky/harbor/internal/opencode"
)

// init registers the built-in dedicated adapters. Each handles one CLI's complex
// integrations (acp, server) and returns agent.ErrUnhandled for the generic
// framing (stream-json, exec) handled by New. An external CLI can add itself the
// same way, from its own package's init, without editing this file.
func init() {
	agent.RegisterAdapter("opencode", buildOpencode)
	agent.RegisterAdapter("codebuddy", buildCodebuddy)
	agent.RegisterAdapter("codex", buildCodex)
}

// buildOpencode builds the dedicated launchers for the opencode CLI: ACP over
// stdio and its HTTP server.
func buildOpencode(ac config.AgentConfig, opts agent.AdapterOptions) (agent.Launcher, error) {
	switch ac.Integration {
	case config.IntegrationACP:
		return acp.NewLauncher(acp.Config{
			Name: ac.ID, Binary: ac.Binary, Args: ac.Args, Env: envSlice(ac.Env),
			Auth: authenticator(ac), MaxConcurrent: ac.Limits.MaxConcurrent,
			InheritEnv: opts.InheritEnv, CLIHome: opts.CLIHome, Container: opts.Container,
			VersionArgs: profileVersionArgs(ac), VersionOverride: ac.Version,
		}), nil
	case config.IntegrationServer:
		return opencode.NewServerLauncher(opts.Lifecycle, opencode.Config{
			Name: ac.ID, Binary: ac.Binary, BaseURL: ac.ServerURL,
			Env: envSlice(ac.Env), InheritEnv: opts.InheritEnv, CLIHome: opts.CLIHome,
			Container: opts.Container, Auth: authenticator(ac),
			VersionArgs: profileVersionArgs(ac), VersionOverride: ac.Version,
		}), nil
	default:
		return nil, agent.ErrUnhandled
	}
}

// buildCodebuddy builds the dedicated launchers for the codebuddy CLI: ACP over
// stdio, the HTTP API server, and ACP over HTTP (--serve or the standalone
// streamable-http transport).
func buildCodebuddy(ac config.AgentConfig, opts agent.AdapterOptions) (agent.Launcher, error) {
	switch ac.Integration {
	case config.IntegrationACP:
		return acp.NewLauncher(acp.Config{
			Name: ac.ID, Binary: ac.Binary, Args: ac.Args, Env: envSlice(ac.Env),
			Auth: authenticator(ac), MaxConcurrent: ac.Limits.MaxConcurrent,
			InheritEnv: opts.InheritEnv, CLIHome: opts.CLIHome, Container: opts.Container,
			VersionArgs: profileVersionArgs(ac), VersionOverride: ac.Version,
		}), nil
	case config.IntegrationServer:
		if ac.ServerProtocol == config.ServerProtocolACP ||
			ac.ServerProtocol == config.ServerProtocolACPStreamableHTTP {
			// Both protocols expose the same ACP-over-HTTP endpoint; they differ
			// only in how the server is started.
			args := []string{"--serve", "--host", "127.0.0.1", "--auth", "none"}
			if ac.ServerProtocol == config.ServerProtocolACPStreamableHTTP {
				args = []string{"--acp", "--acp-transport", "streamable-http"}
			}
			return acp.NewHTTPLauncher(acp.HTTPConfig{
				Name: ac.ID, Binary: ac.Binary, Args: args, BaseURL: ac.ServerURL,
				Env:     envSlice(ac.Env),
				Headers: http.Header{"X-CodeBuddy-Request": {"1"}},
				Auth:    authenticator(ac), InheritEnv: opts.InheritEnv, CLIHome: opts.CLIHome,
				Container: opts.Container, VersionArgs: profileVersionArgs(ac),
				VersionOverride: ac.Version, Lifecycle: opts.Lifecycle,
			}), nil
		}
		return codebuddy.NewServerLauncher(opts.Lifecycle, codebuddy.Config{
			Name: ac.ID, Binary: ac.Binary, BaseURL: ac.ServerURL,
			Env: envSlice(ac.Env), InheritEnv: opts.InheritEnv, CLIHome: opts.CLIHome,
			Container: opts.Container, Auth: authenticator(ac),
			VersionArgs: profileVersionArgs(ac), VersionOverride: ac.Version,
		}), nil
	default:
		return nil, agent.ErrUnhandled
	}
}

// buildCodex builds the dedicated app-server launcher for the codex CLI.
func buildCodex(ac config.AgentConfig, opts agent.AdapterOptions) (agent.Launcher, error) {
	if ac.Integration != config.IntegrationServer {
		return nil, agent.ErrUnhandled
	}
	return codex.NewServerLauncher(opts.Lifecycle, codex.Config{
		Name: ac.ID, Binary: ac.Binary,
		Env: envSlice(ac.Env), InheritEnv: opts.InheritEnv, CLIHome: opts.CLIHome,
		Container: opts.Container, Auth: authenticator(ac),
		VersionArgs: profileVersionArgs(ac), VersionOverride: ac.Version,
	}), nil
}

// profileVersionArgs returns the version flag(s) for a known CLI, for the
// dedicated launchers to probe with.
func profileVersionArgs(ac config.AgentConfig) []string {
	if p, ok := config.LookupProfile(ac.Binary); ok {
		return p.VersionArgs
	}
	return nil
}
