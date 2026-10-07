package agent

import (
	"context"
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
	Parser       Parser
	Invoke       InvokeFunc
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
	invoke          InvokeFunc
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
		invoke:          cfg.Invoke,
		runner:          NewRunner(cfg.MaxConcurrent, cfg.InheritEnv, cfg.CLIHome),
		versionArgs:     cfg.VersionArgs,
		versionOverride: cfg.VersionOverride,
	}
}

func (l *HeadlessLauncher) Name() string               { return l.name }
func (l *HeadlessLauncher) Capabilities() Capabilities { return l.caps }

// Probe checks the binary is present/executable and best-effort reads its
// version. Version probing is skipped when no version arguments are configured.
func (l *HeadlessLauncher) Probe(ctx context.Context) Info {
	path, err := exec.LookPath(l.binary)
	if err != nil {
		return Info{Name: l.name, Enabled: false, Err: err}
	}
	version := l.versionOverride
	if version == "" {
		version = DetectVersion(ctx, path, l.versionArgs)
	}
	return Info{Name: l.name, Path: path, Enabled: true, Version: version}
}

func (l *HeadlessLauncher) Start(ctx context.Context, req *Request) (Session, error) {
	taskID := TaskIDFrom(ctx)
	inv, err := l.invoke(req)
	if err != nil {
		return nil, err
	}
	events, err := l.runner.Start(ctx, taskID, inv, l.parser)
	if err != nil {
		return nil, err
	}
	return &lineSession{
		events: events,
		stdin:  l.runner.Stdin(taskID),
		duplex: l.caps.InputRequiredResume,
		kill:   func() { l.runner.Kill(taskID) },
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

// EncodeStreamJSONUserMessage encodes a text turn as a stream-json user message.
func EncodeStreamJSONUserMessage(text string) string {
	msg := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": []map[string]any{{"type": "text", "text": text}},
		},
	}
	b, _ := json.Marshal(msg)
	return string(b) + "\n"
}
