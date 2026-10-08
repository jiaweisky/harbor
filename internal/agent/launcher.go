package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"sync/atomic"
)

// ErrUnsupported indicates the session cannot accept out-of-band input.
var ErrUnsupported = errors.New("operation not supported by this session")

// InvokeFunc builds a concrete Invocation from a Request.
type InvokeFunc func(req *Request) (*Invocation, error)

// LauncherConfig configures a HeadlessLauncher.
type LauncherConfig struct {
	Name         string
	Binary       string
	Capabilities Capabilities
	// Parser is a shared parser; NewParser, when set, is preferred and yields a
	// fresh parser per task so parser state cannot leak across concurrent tasks.
	Parser    Parser
	NewParser func() Parser
	Invoke    InvokeFunc
	// Env is the adapter's environment, applied to auth probes and task
	// invocations.
	Env []string
	// Auth describes how the CLI authenticates (may be nil for CLIs Harbor
	// cannot verify).
	Auth Authenticator
	// MaxConcurrent caps simultaneous processes for this launcher (<=0 unbounded).
	MaxConcurrent int
	// InheritEnv passes the parent environment to the CLI when true.
	InheritEnv bool
	// CLIHome, when set, isolates the CLI's HOME and config roots under it.
	CLIHome string
	// VersionArgs is the flag(s) used to read the CLI version; nil skips probing.
	VersionArgs []string
	// VersionOverride, when set, is used verbatim instead of probing.
	VersionOverride string
}

// HeadlessLauncher drives a CLI as a headless subprocess (stdin/stdout).
type HeadlessLauncher struct {
	name            string
	binary          string
	caps            Capabilities
	parser          Parser
	newParser       func() Parser
	invoke          InvokeFunc
	env             []string
	auth            Authenticator
	runner          *Runner
	versionArgs     []string
	versionOverride string
}

// NewHeadlessLauncher builds a headless launcher for one CLI.
func NewHeadlessLauncher(cfg LauncherConfig) *HeadlessLauncher {
	return &HeadlessLauncher{
		name:            cfg.Name,
		binary:          cfg.Binary,
		caps:            cfg.Capabilities,
		parser:          cfg.Parser,
		newParser:       cfg.NewParser,
		invoke:          cfg.Invoke,
		env:             cfg.Env,
		auth:            cfg.Auth,
		runner:          NewRunner(cfg.MaxConcurrent, cfg.InheritEnv, cfg.CLIHome),
		versionArgs:     cfg.VersionArgs,
		versionOverride: cfg.VersionOverride,
	}
}

func (l *HeadlessLauncher) Name() string               { return l.name }
func (l *HeadlessLauncher) Capabilities() Capabilities { return l.caps }
func (l *HeadlessLauncher) Auth() Authenticator        { return l.auth }

// AuthReady reports the CLI's best-effort authentication status.
func (l *HeadlessLauncher) AuthReady(ctx context.Context) bool {
	return AuthReadyFor(ctx, l.binary, l.runner, l.env, l.auth)
}

// ProbeBinary checks that a CLI binary is present/executable and best-effort
// reads its version. Version probing is skipped when versionArgs is empty.
func ProbeBinary(ctx context.Context, name, binary string, versionArgs []string, versionOverride string) Info {
	path, err := exec.LookPath(binary)
	if err != nil {
		return Info{Name: name, Enabled: false, Err: err}
	}
	version := versionOverride
	if version == "" {
		version = DetectVersion(ctx, path, versionArgs)
	}
	return Info{Name: name, Path: path, Enabled: true, Version: version}
}

func (l *HeadlessLauncher) Probe(ctx context.Context) Info {
	return ProbeBinary(ctx, l.name, l.binary, l.versionArgs, l.versionOverride)
}

func (l *HeadlessLauncher) Start(ctx context.Context, req *Request) (Session, error) {
	inv, err := l.invoke(req)
	if err != nil {
		return nil, err
	}
	p := l.parser
	if l.newParser != nil {
		p = l.newParser()
	}
	events, err := l.runner.Start(ctx, TaskIDFrom(ctx), inv, p)
	if err != nil {
		return nil, err
	}
	return &lineSession{
		events: events,
		stdin:  l.runner.Stdin(TaskIDFrom(ctx)),
		duplex: l.caps.InputRequiredResume,
		kill:   func() { l.runner.Kill(TaskIDFrom(ctx)) },
	}, nil
}

type lineSession struct {
	events   <-chan Event
	stdin    io.WriteCloser
	duplex   bool
	kill     func()
	canceled atomic.Bool
}

func (s *lineSession) Events() <-chan Event { return s.events }

func (s *lineSession) Send(_ context.Context, in Input) error {
	if !s.duplex || s.stdin == nil {
		return ErrUnsupported
	}
	// A permission or question interaction is answered with a control_response;
	// any other input is a normal user turn written as a stream-json message.
	switch in.Kind {
	case string(InteractionPermission):
		_, err := io.WriteString(s.stdin, EncodePermissionResponse(in.ID, in.Answer))
		return err
	case string(InteractionQuestion):
		_, err := io.WriteString(s.stdin, EncodeQuestionResponse(in.ID, in.Target, in.Answer))
		return err
	}
	_, err := io.WriteString(s.stdin, EncodeStreamJSONUserMessage(in.Answer))
	return err
}

func (s *lineSession) Cancel() error {
	s.canceled.Store(true)
	if s.kill != nil {
		s.kill()
	}
	return nil
}

func (s *lineSession) Canceled() bool { return s.canceled.Load() }

// NewLineParser returns a parser that emits each raw line as a text delta. It
// is used to read unstructured subprocess output such as a server's startup
// banner.
func NewLineParser() Parser { return lineParser{} }

// lineParser emits each raw line as a text delta.
type lineParser struct{}

func (lineParser) Parse(rec []byte, out func(Event)) error {
	out(Event{Kind: EventTextDelta, Text: string(rec) + "\n", Raw: rec})
	return nil
}

func (lineParser) Flush(func(Event)) error { return nil }

// EncodeStreamJSONUserMessage encodes a text-only stream-json user message.
func EncodeStreamJSONUserMessage(text string) string { return EncodeStreamJSONInput(text, nil) }

// EncodeStreamJSONInput encodes a stream-json user message with optional native
// image content blocks (base64-encoded). It is used for the initial turn and,
// with no images, for a lossy resume's injected text.
func EncodeStreamJSONInput(text string, images []Image) string {
	content := make([]map[string]any, 0, len(images)+1)
	if text != "" {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	for _, img := range images {
		content = append(content, map[string]any{
			"type": "image",
			"source": map[string]any{
				"type":       "base64",
				"media_type": img.MediaType,
				"data":       base64.StdEncoding.EncodeToString(img.Data),
			},
		})
	}
	msg := map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": content},
	}
	b, _ := json.Marshal(msg)
	return string(b) + "\n"
}
