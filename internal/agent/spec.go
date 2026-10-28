package agent

import (
	"context"
	"io"
)

// Info describes the result of probing a CLI binary. It carries no AgentCard
// data.
type Info struct {
	AgentID    string // configured agent ID
	CLIVersion string // detected CLI version or configured override; empty when probing is skipped
	BinaryPath string // resolved executable path; empty when the binary was not found
	Available  bool   // whether the binary was found and the launcher can be served
	ProbeErr   error  // why the probe failed; nil when Available is true
}

// Request is a normalized invocation request (A2A -> adapter).
type Request struct {
	Prompt string
	// Blocks is the ordered list of text blocks (one per message part) for a
	// structured-input CLI; Prompt is their newline-joined form for templates.
	Blocks      []string
	SystemExtra string
	Model       string
	Agent       string
	WorkDir     string
	SessionID   string
	Continue    bool
	// Fork, with SessionID, forks the session instead of continuing it in place
	// (the CLI starts a new session seeded from the resumed one).
	Fork     bool
	MaxTurns int
	// OutputSchema, when set, asks the CLI to produce a schema-validated JSON
	// result, surfaced as a structured DataPart artifact.
	OutputSchema map[string]any
	// Images are native image inputs for structured (stream-json) CLIs.
	Images []Image
	// Files are materialized file paths the CLI attaches via its file args.
	Files []string
	// ImageFiles are materialized image paths the CLI attaches via its image
	// args (for CLIs that take images as arguments, not native content blocks).
	ImageFiles []string
	Meta       map[string]any
}

// Invocation is a concrete CLI invocation.
type Invocation struct {
	Bin   string
	Args  []string
	Env   []string
	Dir   string
	Stdin io.Reader // optional; headless structured input (duplex)
	// KeepStdinOpen keeps the stdin pipe open for a duplex transport that reads
	// requests from it (stream-json, ACP, app-server). When false, stdin is
	// closed after any initial Stdin payload, or immediately when there is none,
	// so a one-shot CLI sees EOF instead of blocking on an open pipe.
	KeepStdinOpen bool
	// StderrLine, when set, receives each complete line the CLI writes to
	// stderr. Some servers print their listening address there (for example
	// codebuddy's streamable-http endpoint), so a spawner can read it alongside
	// stdout.
	StderrLine func(string)
}

// Parser converts raw CLI output records into normalized events.
type Parser interface {
	Parse(rec []byte, out func(Event)) error
	Flush(out func(Event)) error
}

// StreamGranularity describes how fine-grained output streaming is.
type StreamGranularity int

const (
	StreamNone StreamGranularity = iota
	StreamTurn
	StreamToken
)

// PermissionModel describes how a CLI handles permission prompts.
type PermissionModel int

const (
	PermNone PermissionModel = iota
	PermFlagOnly
	PermInteractive
)

// Capabilities describes what a coding-agent CLI can do. It is projected onto
// the advertised agent card.
type Capabilities struct {
	InputText   bool
	InputImage  []string
	InputFile   bool
	InputData   bool
	InputPrompt bool

	OutputText  bool
	OutputFile  bool
	OutputData  bool
	OutputImage []string
	ToolTrace   bool

	// AttachFiles attaches file parts via CLI args (materialized to disk and
	// named on the command line) rather than only mentioning the path.
	AttachFiles bool
	// InlineImages sends accepted image parts as native content blocks (for a
	// structured stream transport). It is false for CLIs that take images as
	// command-line arguments instead.
	InlineImages bool
	// AttachImages materializes accepted image parts and passes them via CLI
	// args (see the agent's image_args) rather than as native content blocks.
	AttachImages bool

	Permission          PermissionModel
	AskQuestion         bool
	PlanApproval        bool
	InputRequiredResume bool

	SessionResume bool
	SessionFork   bool
	Streaming     StreamGranularity
	SubAgents     bool
	Worktree      bool
	Sandbox       bool
}

// InputModes projects input capabilities onto A2A MIME modes.
func (c Capabilities) InputModes() []string {
	modes := []string{}

	if c.InputText {
		modes = append(modes, "text/plain")
	}

	modes = append(modes, c.InputImage...)

	if c.InputFile {
		modes = append(modes, "application/octet-stream")
	}

	if c.InputData {
		modes = append(modes, "application/json")
	}

	return modes
}

// OutputModes projects output capabilities onto A2A MIME modes.
func (c Capabilities) OutputModes() []string {
	modes := []string{}

	if c.OutputText {
		modes = append(modes, "text/plain")
	}

	if c.OutputData {
		modes = append(modes, "application/json")
	}

	if c.OutputFile {
		modes = append(modes, "text/x-diff")
	}

	modes = append(modes, c.OutputImage...)

	return modes
}

// Launcher is the core, mode-agnostic contract the executor depends on.
type Launcher interface {
	Name() string
	Capabilities() Capabilities
	Probe(ctx context.Context) Info
	// Auth describes how the CLI authenticates (may be nil for CLIs Harbor
	// cannot verify).
	Auth() Authenticator
	// AuthReady reports the CLI's best-effort authentication status, for
	// readiness reporting. It never gates task execution.
	AuthReady(ctx context.Context) bool
	Start(ctx context.Context, req *Request) (Session, error)
}

// Session is one running CLI conversation.
type Session interface {
	Events() <-chan Event
	Send(ctx context.Context, in Input) error
	Cancel() error
	Canceled() bool
}

// Input is an out-of-band answer to a session (permission / clarification).
type Input struct {
	Kind    string         // interaction kind (e.g. "permission", "question")
	ID      string         // interaction id the client answered
	Action  string         // interaction action (e.g. "form")
	Target  string         // interaction target (e.g. the question text)
	Answer  string         // text answer
	Content map[string]any // form answer, keyed by field
}

type taskIDKey struct{}

// WithTaskID attaches a task id to the context so wrappers can key processes.
func WithTaskID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, taskIDKey{}, id)
}

// TaskIDFrom extracts the task id previously attached with WithTaskID.
func TaskIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(taskIDKey{}).(string)
	return id
}

// FirstNonEmpty returns the first non-empty string, or "" if none is set.
func FirstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}

	return ""
}
