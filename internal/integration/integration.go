// Package integration builds a Launcher for an agent configuration entry.
package integration

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jiaweisky/harbor/internal/acp"
	"github.com/jiaweisky/harbor/internal/agent"
	"github.com/jiaweisky/harbor/internal/config"
	"github.com/jiaweisky/harbor/internal/parser"
	versionutil "github.com/jiaweisky/harbor/internal/version"
)

// Options controls how a launcher's processes are run.
type Options struct {
	// InheritEnv passes the parent environment to the CLI when true.
	InheritEnv bool
	// CLIHome, when set, isolates the CLI's HOME and config roots under it.
	CLIHome string
}

// Build probes the CLI and constructs a launcher. A missing binary yields a
// disabled Info with no error; an unsupported version yields an error so the
// agent is refused rather than run against an unknown binary.
func Build(ctx context.Context, ac config.AgentConfig, opts Options) (agent.Launcher, agent.Info, error) {
	path, err := exec.LookPath(ac.Binary)
	if err != nil {
		return nil, agent.Info{Name: ac.ID, DisplayName: ac.DisplayName, Enabled: false, Err: err}, nil
	}

	profile, known := config.LookupProfile(ac.Binary)
	ver := ac.Version // an explicit override is kept verbatim
	if ver == "" && known {
		ver = agent.DetectVersion(ctx, path, profile.VersionArgs)
		if canonical, ok := versionutil.Parse(ver); ok {
			ver = strings.TrimPrefix(canonical, "v") // "2.1.292 (Claude Code)" -> "2.1.292"
		}
	}
	if known && profile.MinVersion != "" && ver != "" {
		if older, ok := versionutil.Less(ver, profile.MinVersion); ok && older {
			return nil, agent.Info{}, fmt.Errorf(
				"%s %s is below the minimum supported version %s; please upgrade",
				filepath.Base(ac.Binary), ver, profile.MinVersion,
			)
		}
	}

	ac.Version = ver // reuse the probed version; avoids re-probing in the launcher
	ln, err := New(ac, opts)
	if err != nil {
		return nil, agent.Info{}, err
	}
	return ln, agent.Info{Name: ac.ID, DisplayName: ac.DisplayName, Path: path, Enabled: true, Version: ver}, nil
}

// New constructs a Launcher for the given agent config. When the integration
// is empty it is resolved from the CLI's known profile.
func New(ac config.AgentConfig, opts Options) (agent.Launcher, error) {
	profile, known := config.LookupProfile(ac.Binary)

	integ := ac.Integration
	if known {
		switch {
		case integ == "":
			integ = profile.Integration
		case !profile.Supports(integ):
			return nil, fmt.Errorf("binary %q does not support integration %q (supported: %s)",
				ac.Binary, integ, config.IntegrationNames(profile.Supported))
		}
	}

	var versionArgs []string
	if known {
		versionArgs = profile.VersionArgs
	}

	switch integ {
	case config.IntegrationStreamJSON:
		return agent.NewHeadlessLauncher(agent.LauncherConfig{
			Name: ac.ID, Binary: ac.Binary, Capabilities: streamJSONCaps(),
			Parser: parser.NewStreamJSON(), Invoke: streamJSONInvoke(ac),
			MaxConcurrent: ac.Limits.MaxConcurrent, InheritEnv: opts.InheritEnv, CLIHome: opts.CLIHome,
			VersionArgs: versionArgs, VersionOverride: ac.Version,
		}), nil
	case config.IntegrationExec:
		p, err := buildExecParser(ac)
		if err != nil {
			return nil, err
		}
		return agent.NewHeadlessLauncher(agent.LauncherConfig{
			Name: ac.ID, Binary: ac.Binary, Capabilities: textCaps(),
			Parser: p, Invoke: textInvoke(ac),
			MaxConcurrent: ac.Limits.MaxConcurrent, InheritEnv: opts.InheritEnv, CLIHome: opts.CLIHome,
			VersionArgs: versionArgs, VersionOverride: ac.Version,
		}), nil
	case config.IntegrationACP:
		return acp.NewLauncher(acp.Config{
			Name: ac.ID, Binary: ac.Binary, Args: ac.Args, Env: envSlice(ac.Env),
			MaxConcurrent: ac.Limits.MaxConcurrent, InheritEnv: opts.InheritEnv, CLIHome: opts.CLIHome,
			VersionArgs: versionArgs, VersionOverride: ac.Version,
		}), nil
	case config.IntegrationServer:
		return nil, fmt.Errorf(
			"integration %q is recognized but not implemented yet; use %q for a CLI with "+
				"a structured stream, or %q for one-shot text output",
			integ, config.IntegrationStreamJSON, config.IntegrationExec,
		)
	default:
		if known {
			return nil, fmt.Errorf("binary %q supports %s", ac.Binary, config.IntegrationNames(profile.Supported))
		}
		return nil, fmt.Errorf("unknown integration %q; set one of %s", integ, config.IntegrationList())
	}
}

// streamJSONCaps describes a CLI driven over its structured, duplex stream mode.
func streamJSONCaps() agent.Capabilities {
	return agent.Capabilities{
		InputText:           true,
		InputPrompt:         true,
		OutputText:          true,
		Permission:          agent.PermInteractive,
		InputRequiredResume: true,
		SessionResume:       true,
		Streaming:           agent.StreamToken,
	}
}

// textCaps describes a CLI driven by one-shot invocation with plain output.
func textCaps() agent.Capabilities {
	return agent.Capabilities{
		InputText:     true,
		InputPrompt:   true,
		OutputText:    true,
		Permission:    agent.PermFlagOnly,
		SessionResume: true,
		Streaming:     agent.StreamTurn,
	}
}

// buildExecParser builds the parser for a one-shot (exec) invocation. The
// parser name comes from config (default "text"); "regex" also needs a pattern.
func buildExecParser(ac config.AgentConfig) (agent.Parser, error) {
	name := ac.Parser
	if name == "" {
		name = "text"
	}
	pattern := ""
	if ac.Regex != nil {
		pattern = ac.Regex.Text
	}
	return parser.Build(name, pattern)
}

// streamJSONInvoke builds a duplex invocation: the prompt is written to stdin
// and the pipe stays open for later turns.
func streamJSONInvoke(ac config.AgentConfig) agent.InvokeFunc {
	return func(req *agent.Request) (*agent.Invocation, error) {
		args := append([]string{}, ac.Args...)
		args = append(args, sessionArgs(ac, req)...)
		args = appendModelAndSystem(args, ac, req)
		return &agent.Invocation{
			Bin:           ac.Binary,
			Args:          args,
			Dir:           req.WorkDir,
			Env:           envSlice(ac.Env),
			Stdin:         strings.NewReader(agent.EncodeStreamJSONUserMessage(req.Prompt)),
			KeepStdinOpen: true,
		}, nil
	}
}

// textInvoke builds a one-shot invocation: the prompt is substituted into the
// configured argument template.
func textInvoke(ac config.AgentConfig) agent.InvokeFunc {
	return func(req *agent.Request) (*agent.Invocation, error) {
		args := make([]string, 0, len(ac.Args))
		for _, a := range ac.Args {
			args = append(args, substitute(a, req))
		}
		args = append(args, sessionArgs(ac, req)...)
		return &agent.Invocation{
			Bin:  ac.Binary,
			Args: args,
			Dir:  req.WorkDir,
			Env:  envSlice(ac.Env),
		}, nil
	}
}

// sessionArgs returns the flags that continue or resume a CLI session.
func sessionArgs(ac config.AgentConfig, req *agent.Request) []string {
	if req.SessionID != "" {
		if flag := agent.FirstNonEmpty(ac.Session.ResumeFlag, ac.Session.IDFlag); flag != "" {
			return []string{flag, req.SessionID}
		}
		return nil
	}
	if req.Continue && ac.Session.ContinueFlag != "" {
		return []string{ac.Session.ContinueFlag}
	}
	return nil
}

// appendModelAndSystem adds model and system-prompt flags when requested. These
// flags are shared by the Claude-family CLIs; other integrations use templates.
func appendModelAndSystem(args []string, ac config.AgentConfig, req *agent.Request) []string {
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if req.SystemExtra != "" {
		args = append(args, "--append-system-prompt", req.SystemExtra)
	}
	if ac.AllowDangerous {
		args = append(args, "--dangerously-skip-permissions")
	}
	return args
}

func substitute(arg string, req *agent.Request) string {
	arg = strings.ReplaceAll(arg, "{{.Prompt}}", req.Prompt)
	arg = strings.ReplaceAll(arg, "{{.Model}}", req.Model)
	arg = strings.ReplaceAll(arg, "{{.WorkDir}}", req.WorkDir)
	return arg
}

func envSlice(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}
