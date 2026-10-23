package integration

import (
	"net/http"

	"github.com/jiaweisky/harbor/internal/acp"
	"github.com/jiaweisky/harbor/internal/agent"
	"github.com/jiaweisky/harbor/internal/cli"
	"github.com/jiaweisky/harbor/internal/codebuddy"
	"github.com/jiaweisky/harbor/internal/codex"
	"github.com/jiaweisky/harbor/internal/core"
	"github.com/jiaweisky/harbor/internal/opencode"
)

// init registers the built-in dedicated adapters. Each handles one CLI's complex
// integrations (acp, server) and returns cli.ErrUnhandled for the generic
// framing (stream-json, exec) handled by New. External CLIs register their
// builder with cli.AttachBuilder (known binary) or cli.Register (new binary)
// from their own package's init, without editing this file.
func init() {
	cli.AttachBuilder("opencode", buildOpencode)
	cli.AttachBuilder("codebuddy", buildCodebuddy)
	cli.AttachBuilder("codex", buildCodex)
}

// buildOpencode builds the dedicated launchers for the opencode CLI: ACP over
// stdio and its HTTP server.
func buildOpencode(ac core.AgentSpec, opts cli.Options) (agent.Launcher, error) {
	switch ac.Integration {
	case core.IntegrationACP:
		return acp.NewLauncher(acp.Config{
			Name: ac.ID, Binary: ac.Binary, Args: ac.Args, Env: envSlice(ac.Env),
			Auth: authenticator(ac), MaxConcurrent: ac.Limits.MaxConcurrent,
			InheritEnv: opts.InheritEnv, CLIHome: opts.CLIHome, Container: opts.Container, Runtime: opts.Runtime,
			VersionArgs: profileVersionArgs(ac), VersionOverride: ac.Version,
		}), nil
	case core.IntegrationServer:
		return opencode.NewServerLauncher(opts.Ctx, opencode.Config{
			Name: ac.ID, Binary: ac.Binary, BaseURL: ac.ServerURL,
			Env: envSlice(ac.Env), InheritEnv: opts.InheritEnv, CLIHome: opts.CLIHome,
			Container: opts.Container, Runtime: opts.Runtime, Auth: authenticator(ac),
			VersionArgs: profileVersionArgs(ac), VersionOverride: ac.Version,
		}), nil
	default:
		return nil, cli.ErrUnhandled
	}
}

// buildCodebuddy builds the dedicated launchers for the codebuddy CLI: ACP over
// stdio, the HTTP API server, and ACP over HTTP (--serve or the standalone
// streamable-http transport).
func buildCodebuddy(ac core.AgentSpec, opts cli.Options) (agent.Launcher, error) {
	switch ac.Integration {
	case core.IntegrationACP:
		return acp.NewLauncher(acp.Config{
			Name: ac.ID, Binary: ac.Binary, Args: ac.Args, Env: envSlice(ac.Env),
			Auth: authenticator(ac), MaxConcurrent: ac.Limits.MaxConcurrent,
			InheritEnv: opts.InheritEnv, CLIHome: opts.CLIHome, Container: opts.Container, Runtime: opts.Runtime,
			VersionArgs: profileVersionArgs(ac), VersionOverride: ac.Version,
		}), nil
	case core.IntegrationServer:
		if ac.ServerProtocol == core.ServerProtocolACP ||
			ac.ServerProtocol == core.ServerProtocolACPStreamableHTTP {
			// Both protocols expose the same ACP-over-HTTP endpoint; they differ
			// only in how the server is started.
			args := []string{"--serve", "--host", "127.0.0.1", "--auth", "none"}
			if ac.ServerProtocol == core.ServerProtocolACPStreamableHTTP {
				args = []string{"--acp", "--acp-transport", "streamable-http"}
			}
			return acp.NewHTTPLauncher(acp.HTTPConfig{
				Name: ac.ID, Binary: ac.Binary, Args: args, BaseURL: ac.ServerURL,
				Env:     envSlice(ac.Env),
				Headers: http.Header{"X-CodeBuddy-Request": {"1"}},
				Auth:    authenticator(ac), InheritEnv: opts.InheritEnv, CLIHome: opts.CLIHome,
				Container: opts.Container, Runtime: opts.Runtime, VersionArgs: profileVersionArgs(ac),
				VersionOverride: ac.Version, Lifecycle: opts.Ctx,
			}), nil
		}
		return codebuddy.NewServerLauncher(opts.Ctx, codebuddy.Config{
			Name: ac.ID, Binary: ac.Binary, BaseURL: ac.ServerURL,
			Env: envSlice(ac.Env), InheritEnv: opts.InheritEnv, CLIHome: opts.CLIHome,
			Container: opts.Container, Runtime: opts.Runtime, Auth: authenticator(ac),
			VersionArgs: profileVersionArgs(ac), VersionOverride: ac.Version,
		}), nil
	default:
		return nil, cli.ErrUnhandled
	}
}

// buildCodex builds the dedicated app-server launcher for the codex CLI.
func buildCodex(ac core.AgentSpec, opts cli.Options) (agent.Launcher, error) {
	if ac.Integration != core.IntegrationServer {
		return nil, cli.ErrUnhandled
	}
	return codex.NewServerLauncher(opts.Ctx, codex.Config{
		Name: ac.ID, Binary: ac.Binary,
		Env: envSlice(ac.Env), InheritEnv: opts.InheritEnv, CLIHome: opts.CLIHome,
		Container: opts.Container, Runtime: opts.Runtime, Auth: authenticator(ac),
		VersionArgs: profileVersionArgs(ac), VersionOverride: ac.Version,
	}), nil
}

// profileVersionArgs returns the version flag(s) for a known CLI, for the
// dedicated launchers to probe with.
func profileVersionArgs(ac core.AgentSpec) []string {
	if p, ok := cli.Lookup(ac.Binary); ok {
		return p.VersionArgs
	}
	return nil
}
