// Package opencode integrates opencode's own HTTP server (`opencode serve`) as
// a long-running A2A backend. Unlike the exec/stream-json modes, Harbor does not
// spawn a process per task: it drives the CLI's HTTP API and reuses server-side
// sessions across the turns of an A2A context.
package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jiaweisky/harbor/internal/agent"
	"github.com/jiaweisky/harbor/internal/sandbox"
)

// serverStartTimeout bounds how long to wait for a spawned server to listen.
const serverStartTimeout = 20 * time.Second

// Config configures an opencode server launcher.
type Config struct {
	Name            string
	Binary          string
	BaseURL         string // connect to an existing server; empty spawns `opencode serve`
	Env             []string
	InheritEnv      bool
	CLIHome         string
	Container       *sandbox.Container
	Runtime         *agent.Runtime
	Auth            agent.Authenticator
	VersionArgs     []string
	VersionOverride string
}

// Launcher drives opencode through its HTTP server.
type Launcher struct {
	cfg       Config
	runner    *agent.Runner
	lifecycle context.Context
	client    *http.Client
	endpoint  agent.ServerEndpoint
}

// NewServerLauncher builds a server launcher. lifecycle bounds a spawned server
// process: canceling it stops the server.
func NewServerLauncher(lifecycle context.Context, cfg Config) *Launcher {
	if lifecycle == nil {
		lifecycle = context.Background()
	}

	return &Launcher{
		cfg:       cfg,
		runner:    agent.NewRunner(1, cfg.InheritEnv, cfg.CLIHome, cfg.Container, cfg.Runtime),
		lifecycle: lifecycle,
		client:    &http.Client{},
	}
}

func (l *Launcher) Name() string { return l.cfg.Name }

// Capabilities describes the server mode: token streaming over the server's
// event stream, with permissions handled by the CLI's own configuration.
func (l *Launcher) Capabilities() agent.Capabilities {
	return agent.Capabilities{
		InputText:     true,
		InputPrompt:   true,
		OutputText:    true,
		Permission:    agent.PermFlagOnly,
		SessionResume: true,
		Streaming:     agent.StreamToken,
	}
}

func (l *Launcher) Auth() agent.Authenticator { return l.cfg.Auth }

// AuthReady reports the CLI's best-effort authentication status.
func (l *Launcher) AuthReady(ctx context.Context) bool {
	return agent.AuthReadyFor(ctx, l.cfg.Binary, l.runner, l.cfg.Env, l.cfg.Auth)
}

// Probe checks the binary is present/executable and best-effort reads its version.
func (l *Launcher) Probe(ctx context.Context) agent.Info {
	return agent.ProbeBinary(ctx, l.cfg.Name, l.cfg.Binary, l.cfg.VersionArgs, l.cfg.VersionOverride)
}

// Start runs one prompt against the server. It reuses the request's CLI session
// when set, otherwise creates a new opencode session and reports its id.
func (l *Launcher) Start(ctx context.Context, req *agent.Request) (agent.Session, error) {
	base, err := l.endpoint.URL(ctx, l.cfg.BaseURL, l.spawn)
	if err != nil {
		return nil, err
	}

	runCtx, cancel := context.WithCancel(ctx)
	s := &serverSession{ServerSession: agent.NewServerSession(8, cancel)}
	go s.run(runCtx, l, base, req)

	return s, nil
}

// spawn starts `opencode serve` and waits for it to report its listening URL.
func (l *Launcher) spawn(ctx context.Context) (string, error) {
	inv := &agent.Invocation{
		Bin:  l.cfg.Binary,
		Args: []string{"serve", "--hostname", "127.0.0.1", "--port", "0"},
		Env:  l.cfg.Env,
	}

	return agent.SpawnServer(ctx, l.lifecycle, l.runner, l.cfg.Name+":server", inv, serverStartTimeout)
}

// sessionResponse is the server's reply to a session create.
type sessionResponse struct {
	ID string `json:"id"`
}

// messagePart is one content part of an assistant message.
type messagePart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// messageResponse is the server's reply to a prompt.
type messageResponse struct {
	Parts []messagePart `json:"parts"`
}

// createSession creates a new opencode session and returns its id.
func (l *Launcher) createSession(ctx context.Context, base string) (string, error) {
	var s sessionResponse
	if err := l.post(ctx, base+"/session", map[string]any{}, &s); err != nil {
		return "", err
	}

	if s.ID == "" {
		return "", errors.New("opencode server: session create returned no id")
	}

	return s.ID, nil
}

// sendPrompt sends a text prompt and returns the assistant's text parts.
func (l *Launcher) sendPrompt(ctx context.Context, base, sid, prompt string) (string, error) {
	body := map[string]any{"parts": []map[string]any{{"type": "text", "text": prompt}}}

	var resp messageResponse
	if err := l.post(ctx, base+"/session/"+sid+"/message", body, &resp); err != nil {
		return "", err
	}

	var b strings.Builder
	for _, p := range resp.Parts {
		if p.Type == "text" {
			b.WriteString(p.Text)
		}
	}

	return b.String(), nil
}

// abort asks the server to stop generating for a session.
func (l *Launcher) abort(base, sid string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = l.post(ctx, base+"/session/"+sid+"/abort", map[string]any{}, nil)
}

// post sends a JSON request and decodes a JSON response into out.
func (l *Launcher) post(ctx context.Context, url string, body, out any) error {
	return agent.PostJSON(ctx, l.client, url, "opencode server", nil, body, out)
}

// serverSession streams one prompt turn over the HTTP API.
type serverSession struct {
	*agent.ServerSession
}

// run performs the turn: create/reuse a session, stream the reply while the
// prompt is generating, and emit the final state. It owns the events channel's
// lifetime. Deltas come from the server's event stream; when that is
// unavailable it falls back to the prompt's blocking reply.
func (s *serverSession) run(ctx context.Context, l *Launcher, base string, req *agent.Request) {
	defer s.Close()

	sid := req.SessionID
	if sid == "" {
		id, err := l.createSession(ctx, base)
		if err != nil {
			s.EmitError(ctx, err)
			return
		}

		sid = id
		s.Emit(agent.Event{Kind: agent.EventSession, SessionID: sid})
	}

	s.SetAbort(func() { l.abort(base, sid) })

	// Subscribe before prompting so no deltas are missed.
	streamCtx, stopStream := context.WithCancel(ctx)
	defer stopStream()
	events := make(chan agent.Event, 64)
	go l.streamEvents(streamCtx, base, sid, s, events)

	type replyResult struct {
		text string
		err  error
	}

	reply := make(chan replyResult, 1)
	go func() {
		text, err := l.sendPrompt(ctx, base, sid, req.Prompt)
		reply <- replyResult{text: text, err: err}
	}()

	streamed := false
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				events = nil
				continue
			}

			if ev.Kind == agent.EventTextDelta {
				streamed = true
			}

			s.Emit(ev)
		case r := <-reply:
			if r.err != nil {
				s.EmitError(ctx, r.err)
				return
			}

			// The stream may have buffered events the select has not read yet;
			// flush them before finishing so no event is dropped.
			s.drainEvents(events)
			if !streamed && r.text != "" {
				s.Emit(agent.Event{Kind: agent.EventTextDelta, Text: r.text})
			}

			s.Emit(agent.Event{Kind: agent.EventDone})
			return
		case <-ctx.Done():
			return
		}
	}
}

// drainEvents emits any events still buffered, without blocking.
func (s *serverSession) drainEvents(events <-chan agent.Event) {
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return
			}
			s.Emit(ev)
		default:
			return
		}
	}
}

// streamEvents reads the server's event stream and forwards the events of one
// session: text and reasoning deltas, plus tool parts as tool calls and
// results. It closes out when the stream ends or the context is canceled.
func (l *Launcher) streamEvents(ctx context.Context, base, sid string, s *serverSession, out chan<- agent.Event) {
	defer close(out)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/event", nil)
	if err != nil {
		return
	}

	req.Header.Set("accept", "text/event-stream")
	resp, err := l.client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return
	}

	toolCalled := map[string]string{}
	reasoningSeen := map[string]string{}
	_ = agent.ReadSSE(resp.Body, func(_, data string) bool {
		var ev sseEvent
		if json.Unmarshal([]byte(data), &ev) != nil {
			return true
		}

		if ev.Properties.SessionID != sid {
			return true
		}

		if ev.Type == "message.part.delta" {
			var kind agent.EventKind

			switch ev.Properties.Field {
			case "text":
				kind = agent.EventTextDelta
			case "reasoning":
				kind = agent.EventThinking
			}

			if kind == 0 || ev.Properties.Delta == "" {
				return true
			}

			select {
			case out <- agent.Event{Kind: kind, Text: ev.Properties.Delta}:
				return true
			case <-ctx.Done():
				return false
			}
		}

		if ev.Type != "message.part.updated" {
			return true
		}

		var part opencodePart
		if json.Unmarshal(ev.Properties.Part, &part) != nil {
			return true
		}

		// Reasoning parts arrive as whole-part updates (no deltas), so forward
		// each changed text as a thinking event.
		if part.Type == "reasoning" && part.Text != "" && reasoningSeen[part.ID] != part.Text {
			reasoningSeen[part.ID] = part.Text
			return s.emitTo(ctx, out, agent.Event{Kind: agent.EventThinking, Text: part.Text})
		}

		if part.Type != "tool" {
			return true
		}

		switch part.State.Status {
		case "pending", "running":
			// The server re-sends the part on every state tick. The pending
			// tick carries an empty input; the call event goes out on the
			// first tick that actually carries the tool's input.
			if toolCalled[part.ID] != "" || len(part.State.Input) == 0 {
				return true
			}

			toolCalled[part.ID] = "call"

			return s.emitTo(ctx, out, agent.Event{
				Kind: agent.EventToolCall, Tool: part.Tool, ToolInput: part.State.Input,
			})
		case "completed", "error":
			// Ensure the call event exists even when no earlier tick carried
			// an input, then deliver the result once.
			if toolCalled[part.ID] == "" {
				toolCalled[part.ID] = "call"

				return s.emitTo(ctx, out, agent.Event{
					Kind: agent.EventToolCall, Tool: part.Tool, ToolInput: part.State.Input,
				})
			}

			if toolCalled[part.ID] != "done" {
				toolCalled[part.ID] = "done"

				return s.emitTo(ctx, out, agent.Event{
					Kind: agent.EventToolResult, Text: outputText(part.State.Output),
				})
			}

			return true
		}

		return true
	})
}

// emitTo sends one event to out unless the context is done first.
func (s *serverSession) emitTo(ctx context.Context, out chan<- agent.Event, ev agent.Event) bool {
	select {
	case out <- ev:
		return true
	case <-ctx.Done():
		return false
	}
}

// outputText renders a tool part's output: the string opencode sent, or the
// compact JSON of a structured one.
func outputText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}

	var buf bytes.Buffer
	if json.Compact(&buf, raw) != nil {
		return string(raw)
	}

	return buf.String()
}

// sseEvent is the subset of a server-sent event Harbor consumes.
type sseEvent struct {
	Type       string `json:"type"`
	Properties struct {
		SessionID string          `json:"sessionID"`
		Field     string          `json:"field"`
		Delta     string          `json:"delta"`
		Part      json.RawMessage `json:"part"`
	} `json:"properties"`
}

// opencodePart is the subset of a message part Harbor forwards beyond text.
type opencodePart struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Text  string `json:"text"`
	Tool  string `json:"tool"`
	State struct {
		Status string          `json:"status"`
		Input  map[string]any  `json:"input"`
		Output json.RawMessage `json:"output"`
	} `json:"state"`
}
