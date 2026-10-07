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
	// LoginInvoke builds the invocation that starts an interactive login; nil
	// means the CLI has no headless login.
	LoginInvoke InvokeFunc
	// Env is the adapter's environment, applied to auth probes and login
	// commands as well as task invocations.
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
	invoke          InvokeFunc
	loginInvoke     InvokeFunc
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
		invoke:          cfg.Invoke,
		loginInvoke:     cfg.LoginInvoke,
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

// authEnv is the environment a CLI's auth commands run with.
func (l *HeadlessLauncher) authEnv() []string { return l.runner.Env(&Invocation{Env: l.env}) }

// AuthReady reports whether the CLI currently holds a usable ticket.
func (l *HeadlessLauncher) AuthReady(ctx context.Context) bool {
	return Authenticated(ctx, l.binary, l.runner.AuthBase(), l.authEnv(), l.auth)
}

// EnsureLogin verifies the CLI holds a ticket, attempting its login once when
// none is present. A failure reports the agent as unavailable.
func (l *HeadlessLauncher) EnsureLogin(ctx context.Context) error {
	return EnsureLogin(ctx, l.name, l.binary, l.runner.AuthBase(), l.authEnv(), l.auth)
}

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
	inv, err := l.invocation(req)
	if err != nil {
		return nil, err
	}
	parser := l.parser
	if req.Login {
		parser = lineParser{}
	}
	events, err := l.runner.Start(ctx, TaskIDFrom(ctx), inv, parser)
	if err != nil {
		return nil, err
	}
	return &lineSession{
		events: events,
		stdin:  l.runner.Stdin(TaskIDFrom(ctx)),
		duplex: l.caps.InputRequiredResume && !req.Login,
		kill:   func() { l.runner.Kill(TaskIDFrom(ctx)) },
	}, nil
}

// invocation builds the CLI invocation for a request: the login command for a
// login task, otherwise the adapter's normal invocation.
func (l *HeadlessLauncher) invocation(req *Request) (*Invocation, error) {
	fn := l.invoke
	if req.Login {
		fn = l.loginInvoke
		if fn == nil {
			return nil, ErrNoLogin
		}
	}
	return fn(req)
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

// NewLineParser returns a parser that emits each raw line as a text event.
// Login commands print human-readable output rather than structured events, so
// a login task reads them with this parser.
func NewLineParser() Parser { return lineParser{} }

// lineParser emits each raw line as a text delta.
type lineParser struct{}

func (lineParser) Parse(rec []byte, out func(Event)) error {
	out(Event{Kind: EventTextDelta, Text: string(rec) + "\n", Raw: rec})
	return nil
}

func (lineParser) Flush(func(Event)) error { return nil }

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
