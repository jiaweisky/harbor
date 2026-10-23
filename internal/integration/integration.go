// Package integration builds a Launcher for an agent configuration entry.
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jiaweisky/harbor/internal/acp"
	"github.com/jiaweisky/harbor/internal/agent"
	"github.com/jiaweisky/harbor/internal/cli"
	"github.com/jiaweisky/harbor/internal/core"
	"github.com/jiaweisky/harbor/internal/parser"
	versionutil "github.com/jiaweisky/harbor/internal/version"
)

// Options controls how a launcher's processes are run.
type Options = cli.Options

// AgentSpecSource is implemented by config.AgentConfig and core.AgentSpec.
type AgentSpecSource interface {
	Spec() core.AgentSpec
}

// Build probes the CLI and constructs a launcher. A missing binary yields a
// disabled Info with no error; an unsupported version yields an error so the
// agent is refused rather than run against an unknown binary.
func Build(ctx context.Context, src AgentSpecSource, opts Options) (agent.Launcher, agent.Info, error) {
	ac := src.Spec()
	path, err := exec.LookPath(ac.Binary)
	if err != nil {
		return nil, agent.Info{Name: ac.ID, DisplayName: ac.DisplayName, Enabled: false, Err: err}, nil
	}

	profile, known := cli.LookupProfile(ac.Binary)
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
	opts.Ctx = ctx
	ln, err := New(ac, opts)
	if err != nil {
		return nil, agent.Info{}, err
	}
	// Harbor does not gate on its own credential guess: the CLI decides whether
	// it can run, so a CLI that starts standalone is served as-is.
	return ln, agent.Info{
		Name: ac.ID, DisplayName: ac.DisplayName, Description: ac.Description,
		Skills: infoSkills(ac), Path: path, Enabled: true, Version: ver,
	}, nil
}

// interactionLauncher overrides an agent's advertised interaction capability
// without changing how the CLI is driven.
type interactionLauncher struct {
	agent.Launcher
	caps agent.Capabilities
}

// Capabilities reports the overridden interaction capability.
func (l interactionLauncher) Capabilities() agent.Capabilities { return l.caps }

// applyInteractionMode applies the configured interaction mode to a launcher's
// capabilities. auto (or unset) keeps the integration default; interactive
// forces prompts to be surfaced; non-interactive drops them so the card does not
// advertise the interaction extension and the executor fails if the CLI still
// prompts.
func applyInteractionMode(ln agent.Launcher, mode core.Interaction) (agent.Launcher, error) {
	caps := ln.Capabilities()
	switch mode {
	case "", core.InteractionAuto:
		return ln, nil
	case core.InteractionInteractive:
		caps.Permission = agent.PermInteractive
		caps.InputRequiredResume = true
	case core.InteractionNonInteractive:
		caps.Permission = agent.PermFlagOnly
		caps.InputRequiredResume = false
		caps.PlanApproval = false
		caps.AskQuestion = false
	default:
		return nil, fmt.Errorf("unknown interaction mode %q", mode)
	}
	return interactionLauncher{Launcher: ln, caps: caps}, nil
}

// infoSkills maps the configured card skills onto the agent package type.
func infoSkills(ac core.AgentSpec) []agent.Skill {
	if len(ac.Skills) == 0 {
		return nil
	}
	out := make([]agent.Skill, 0, len(ac.Skills))
	for _, s := range ac.Skills {
		out = append(out, agent.Skill{
			ID: s.ID, Name: s.Name, Description: s.Description,
			Tags: s.Tags, Examples: s.Examples, InputModes: s.InputModes, OutputModes: s.OutputModes,
		})
	}
	return out
}

// New constructs a Launcher for the given spec source and applies the
// configured interaction mode to its capabilities. When the integration is
// empty it is resolved from the CLI's known profile.
func New(src AgentSpecSource, opts Options) (agent.Launcher, error) {
	ac := src.Spec()
	ln, err := buildLauncher(ac, opts)
	if err != nil {
		return nil, err
	}
	return applyInteractionMode(ln, ac.Interaction)
}

// buildLauncher builds the integration-specific launcher without the
// interaction override.
func buildLauncher(ac core.AgentSpec, opts Options) (agent.Launcher, error) {
	profile, known := cli.LookupProfile(ac.Binary)

	integ := ac.Integration
	if known {
		switch {
		case integ == "":
			integ = profile.Integration
		case !profile.Supports(integ):
			return nil, fmt.Errorf("binary %q does not support integration %q (supported: %s)",
				ac.Binary, integ, core.IntegrationNames(profile.Supported))
		}
	}
	ac.Integration = integ // a dedicated adapter reads the resolved integration

	var versionArgs []string
	if known {
		versionArgs = profile.VersionArgs
	}
	// The agent's input images default to the CLI profile's list, so a launcher
	// built without config defaults still advertises the CLI's capabilities.
	inputImage := ac.InputImage
	if len(inputImage) == 0 {
		inputImage = profile.InputImage
	}
	auth := authenticator(ac)

	// A dedicated adapter registered for this CLI (see adapters.go, or any
	// package's init) takes precedence for the integrations it handles.
	if desc, ok := cli.LookupDescriptor(filepath.Base(ac.Binary)); ok && desc.Build != nil {
		ln, err := desc.Build(ac, opts)
		if !errors.Is(err, cli.ErrUnhandled) {
			return ln, err
		}
	}

	switch integ {
	case core.IntegrationStreamJSON:
		caps := streamJSONCaps(inputImage, profile.OutputImage)
		caps.SessionFork = ac.Session.ForkFlag != ""
		return headlessLauncher(ac, opts, auth, versionArgs, caps, headlessSpec{
			newParser: func() agent.Parser { return parser.NewStreamJSON() },
			invoke:    streamJSONInvoke(ac),
		}), nil
	case core.IntegrationExec:
		newParser, err := execParserFactory(ac)
		if err != nil {
			return nil, err
		}
		return headlessLauncher(ac, opts, auth, versionArgs, execCaps(ac, inputImage, profile.OutputImage),
			headlessSpec{newParser: newParser, invoke: textInvoke(ac)}), nil
	case core.IntegrationACP:
		return acpLauncher(ac, opts, auth, versionArgs), nil
	case core.IntegrationServer:
		// The dedicated opencode/codebuddy/codex adapters are registered by
		// binary name and handle these before this switch; reaching here means an
		// unsupported binary.
		return nil, fmt.Errorf(
			"integration %q is implemented for opencode, codebuddy and codex; got %q",
			integ, filepath.Base(ac.Binary),
		)
	default:
		if known {
			return nil, fmt.Errorf("binary %q supports %s", ac.Binary, core.IntegrationNames(profile.Supported))
		}
		return nil, fmt.Errorf("unknown integration %q; set one of %s", integ, core.IntegrationList())
	}
}

// headlessSpec carries the per-integration pieces of a headless launcher. At
// most one of parser and newParser is set.
type headlessSpec struct {
	invoke    agent.InvokeFunc
	parser    agent.Parser
	newParser func() agent.Parser
}

// headlessLauncher builds a headless (stdin/stdout) launcher with the shared
// runner settings and the given per-integration pieces.
func headlessLauncher(
	ac core.AgentSpec, opts Options, auth agent.Authenticator, versionArgs []string,
	caps agent.Capabilities, spec headlessSpec,
) agent.Launcher {
	return agent.NewHeadlessLauncher(agent.LauncherConfig{
		Name: ac.ID, Binary: ac.Binary, Capabilities: caps,
		Parser: spec.parser, NewParser: spec.newParser, Invoke: spec.invoke,
		Env: envSlice(ac.Env), Auth: auth, MaxConcurrent: ac.Limits.MaxConcurrent,
		InheritEnv: opts.InheritEnv, CLIHome: opts.CLIHome, Container: opts.Container, Runtime: opts.Runtime,
		VersionArgs: versionArgs, VersionOverride: ac.Version,
	})
}

// execCaps derives a one-shot CLI's capabilities from its configured attachment
// and structured-output support.
func execCaps(ac core.AgentSpec, inputImage, outputImage []string) agent.Capabilities {
	caps := textCaps(outputImage)
	caps.OutputData = len(ac.OutputSchemaArgs) > 0 // structured output configured
	caps.AttachFiles = len(ac.FileArgs) > 0        // files attached via CLI args
	caps.InputImage = inputImage                   // images accepted (advertised)
	caps.AttachImages = len(ac.ImageArgs) > 0      // images attached via CLI args
	caps.SessionFork = ac.Session.ForkFlag != ""   // session fork flag configured
	return caps
}

// acpLauncher builds an ACP-over-stdio launcher.
func acpLauncher(
	ac core.AgentSpec, opts Options, auth agent.Authenticator, versionArgs []string,
) agent.Launcher {
	return acp.NewLauncher(acp.Config{
		Name: ac.ID, Binary: ac.Binary, Args: ac.Args, Env: envSlice(ac.Env),
		Auth: auth, MaxConcurrent: ac.Limits.MaxConcurrent,
		InheritEnv: opts.InheritEnv, CLIHome: opts.CLIHome, Container: opts.Container, Runtime: opts.Runtime,
		VersionArgs: versionArgs, VersionOverride: ac.Version,
	})
}

// authenticator returns the CLI's authenticator: a built-in adapter for a known
// CLI, or an unverifiable one for a generic CLI. The probed version lets the
// adapter branch on version-specific behavior.
func authenticator(ac core.AgentSpec) agent.Authenticator {
	return cli.AuthFor(ac.Binary, ac.Version)
}

// streamJSONCaps describes a CLI driven over its structured, duplex stream mode.
// inputImage lists the image MIME types the CLI accepts in an image content
// block (empty when it does not accept images on this channel); outputImage
// lists the image MIME types it can produce.
func streamJSONCaps(inputImage, outputImage []string) agent.Capabilities {
	return agent.Capabilities{
		InputText:           true,
		InputImage:          inputImage,
		InlineImages:        len(inputImage) > 0,
		OutputImage:         outputImage,
		InputFile:           true,
		InputPrompt:         true,
		OutputText:          true,
		OutputData:          true, // --json-schema structured output
		Permission:          agent.PermInteractive,
		PlanApproval:        true, // ExitPlanMode -> plan approval
		InputRequiredResume: true,
		SessionResume:       true,
		Streaming:           agent.StreamToken,
	}
}

// textCaps describes a CLI driven by one-shot invocation with plain output.
// outputImage lists the image MIME types the CLI can produce.
func textCaps(outputImage []string) agent.Capabilities {
	return agent.Capabilities{
		InputText:     true,
		InputFile:     true,
		OutputImage:   outputImage,
		InputPrompt:   true,
		OutputText:    true,
		Permission:    agent.PermFlagOnly,
		SessionResume: true,
		Streaming:     agent.StreamTurn,
	}
}

// execParserFactory validates the configured exec parser once and returns a
// constructor. Each task gets its own parser instance because parsers may carry
// per-task state and tasks run concurrently.
func execParserFactory(ac core.AgentSpec) (func() agent.Parser, error) {
	name := ac.Parser
	if name == "" {
		name = core.ParserText
	}
	pattern := ""
	if ac.Regex != nil {
		pattern = ac.Regex.Text
	}
	if _, err := parser.Build(name, pattern); err != nil {
		return nil, err
	}
	return func() agent.Parser {
		p, err := parser.Build(name, pattern)
		if err != nil {
			return parser.NewText()
		}
		return p
	}, nil
}

// streamJSONInvoke builds a duplex invocation: the prompt is written to stdin
// and the pipe stays open for later turns.
func streamJSONInvoke(ac core.AgentSpec) agent.InvokeFunc {
	return func(req *agent.Request) (*agent.Invocation, error) {
		args := append([]string{}, ac.Args...)
		args = append(args, sessionArgs(ac, req)...)
		args = appendCommonFlags(args, ac, req)
		stdin := agent.EncodeStreamJSONBlocks(req.Blocks, req.Images)
		if ac.SDKCaps {
			// Declare SDK capabilities first so the CLI routes AskUserQuestion
			// through can_use_tool.
			stdin = agent.EncodeControlInitialize() + stdin
		}
		return &agent.Invocation{
			Bin:           ac.Binary,
			Args:          args,
			Dir:           req.WorkDir,
			Env:           envSlice(ac.Env),
			Stdin:         strings.NewReader(stdin),
			KeepStdinOpen: true,
		}, nil
	}
}

// textInvoke builds a one-shot invocation: the prompt is substituted into the
// configured argument template. When the request carries an output schema and
// the agent configures output_schema_args, the schema is written to a file and
// the args (with {{.SchemaFile}}/{{.Schema}}) are appended.
func textInvoke(ac core.AgentSpec) agent.InvokeFunc {
	return func(req *agent.Request) (*agent.Invocation, error) {
		args := make([]string, 0, len(ac.Args)+len(ac.OutputSchemaArgs))
		for _, a := range ac.Args {
			args = append(args, substitute(a, req))
		}
		args = append(args, sessionArgs(ac, req)...)
		if req.OutputSchema != nil && len(ac.OutputSchemaArgs) > 0 {
			schemaArgs, err := schemaArgs(ac.OutputSchemaArgs, req.OutputSchema)
			if err != nil {
				return nil, err
			}
			args = append(args, schemaArgs...)
		}
		args = append(args, fileArgs(ac.FileArgs, req.Files)...)
		args = append(args, imageArgs(ac.ImageArgs, req.ImageFiles)...)
		return &agent.Invocation{
			Bin:  ac.Binary,
			Args: args,
			Dir:  req.WorkDir,
			Env:  envSlice(ac.Env),
		}, nil
	}
}

// fileArgs expands the file-attachment args once per file path.
func fileArgs(tmpl, files []string) []string {
	return expandArgs(tmpl, "{{.File}}", files)
}

// imageArgs expands the image-attachment args once per image path.
func imageArgs(tmpl, images []string) []string {
	return expandArgs(tmpl, "{{.Image}}", images)
}

// expandArgs expands an argument template once per value, replacing placeholder
// with the value. It returns nil when the template or values are empty.
func expandArgs(tmpl []string, placeholder string, values []string) []string {
	if len(tmpl) == 0 || len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(tmpl)*len(values))
	for _, v := range values {
		for _, a := range tmpl {
			out = append(out, strings.ReplaceAll(a, placeholder, v))
		}
	}
	return out
}

// schemaArgs writes the schema to a temp file and expands the configured args,
// substituting {{.SchemaFile}} with the file path and {{.Schema}} with the
// compact schema JSON.
func schemaArgs(tmpl []string, schema map[string]any) ([]string, error) {
	raw, err := json.Marshal(schema)
	if err != nil {
		return nil, fmt.Errorf("marshal output schema: %w", err)
	}
	f, err := os.CreateTemp("", "harbor-schema-*.json")
	if err != nil {
		return nil, fmt.Errorf("write output schema: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(raw); err != nil {
		return nil, fmt.Errorf("write output schema: %w", err)
	}
	out := make([]string, len(tmpl))
	for i, a := range tmpl {
		a = strings.ReplaceAll(a, "{{.SchemaFile}}", f.Name())
		out[i] = strings.ReplaceAll(a, "{{.Schema}}", string(raw))
	}
	return out, nil
}

// sessionArgs returns the flags that continue, resume, or fork a CLI session.
func sessionArgs(ac core.AgentSpec, req *agent.Request) []string {
	if req.SessionID != "" {
		var args []string
		if flag := agent.FirstNonEmpty(ac.Session.ResumeFlag, ac.Session.IDFlag); flag != "" {
			args = append(args, flag, req.SessionID)
		}
		if req.Fork && ac.Session.ForkFlag != "" {
			args = append(args, ac.Session.ForkFlag)
		}
		return args
	}
	if req.Continue && ac.Session.ContinueFlag != "" {
		return []string{ac.Session.ContinueFlag}
	}
	return nil
}

// appendCommonFlags adds the model, system-prompt, structured-output, and
// dangerous-mode flags shared by the Claude-family CLIs; other integrations use
// argument templates.
func appendCommonFlags(args []string, ac core.AgentSpec, req *agent.Request) []string {
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if req.SystemExtra != "" {
		args = append(args, "--append-system-prompt", req.SystemExtra)
	}
	if req.OutputSchema != nil {
		if b, err := json.Marshal(req.OutputSchema); err == nil {
			args = append(args, "--json-schema", string(b))
		}
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
