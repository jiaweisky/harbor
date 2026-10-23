// Package codebuddy integrates codebuddy's HTTP server (`codebuddy --serve`) as
// a long-running A2A backend. Unlike the headless modes, Harbor starts the
// server once and drives its REST "runs" API: a task POSTs a run and reads the
// run's SSE stream, mapping the final message to the artifact. Multi-turn
// continuity is keyed by the run's conversation id, reported as the session id.
package codebuddy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jiaweisky/harbor/internal/agent"
	"github.com/jiaweisky/harbor/internal/sandbox"
)

// serverStartTimeout bounds how long to wait for a spawned server to listen.
const serverStartTimeout = 30 * time.Second

// csrfHeader is codebuddy's CSRF guard, required on every /api/v1 request.
const csrfHeader = "X-CodeBuddy-Request"

// Config configures a codebuddy server launcher.
type Config struct {
	Name            string
	Binary          string
	BaseURL         string // connect to an existing server; empty spawns `codebuddy --serve`
	Env             []string
	InheritEnv      bool
	CLIHome         string
	Container       *sandbox.Container
	Runtime         *agent.Runtime
	Auth            agent.Authenticator
	VersionArgs     []string
	VersionOverride string
}

// Launcher drives codebuddy through its HTTP server.
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

// Capabilities describes the server mode: turn-level output (the runs stream
// delivers the final message, not token deltas), with permissions handled by the
// CLI's own configuration.
func (l *Launcher) Capabilities() agent.Capabilities {
	return agent.Capabilities{
		InputText:     true,
		InputPrompt:   true,
		OutputText:    true,
		Permission:    agent.PermFlagOnly,
		SessionResume: true,
		Streaming:     agent.StreamTurn,
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

// Start runs one prompt against the server. The request's session id is used as
// the run's conversation id, so later turns of the same A2A context continue the
// same codebuddy conversation.
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

// spawn starts `codebuddy --serve` and waits for it to report its listening URL.
// The server binds loopback and disables its own HTTP auth: Harbor already runs
// the CLI as the same user, and the server is not exposed beyond localhost.
func (l *Launcher) spawn(ctx context.Context) (string, error) {
	inv := &agent.Invocation{
		Bin:  l.cfg.Binary,
		Args: []string{"--serve", "--host", "127.0.0.1", "--auth", "none"},
		Env:  l.cfg.Env,
	}
	return agent.SpawnServer(ctx, l.lifecycle, l.runner, l.cfg.Name+":server", inv, serverStartTimeout)
}

// startRun initiates an asynchronous agent run and returns its id.
func (l *Launcher) startRun(ctx context.Context, base, conversationID, prompt string) (string, error) {
	body := map[string]any{
		"id":   newID(),
		"type": "message",
		"source": map[string]any{
			"platform":     "generic",
			"sender":       map[string]any{"id": "harbor", "name": "Harbor"},
			"conversation": map[string]any{"id": conversationID, "type": "direct"},
		},
		"payload": map[string]any{"text": prompt},
	}
	var out struct {
		Data struct {
			RunID string `json:"runId"`
		} `json:"data"`
	}
	if err := l.post(ctx, base+"/api/v1/runs", body, &out); err != nil {
		return "", err
	}
	if out.Data.RunID == "" {
		return "", errors.New("codebuddy server: run create returned no id")
	}
	return out.Data.RunID, nil
}

// cancelRun asks the server to stop generating for a run.
func (l *Launcher) cancelRun(base, runID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = l.post(ctx, base+"/api/v1/runs/"+runID+"/cancel", map[string]any{}, nil)
}

// post sends a JSON request with the required CSRF header.
func (l *Launcher) post(ctx context.Context, url string, body, out any) error {
	header := http.Header{csrfHeader: {"1"}}
	return agent.PostJSON(ctx, l.client, url, "codebuddy server", header, body, out)
}

// serverSession streams one run's turn over the HTTP API.
type serverSession struct {
	*agent.ServerSession
}

// messageEvent is the payload of a `message` server-sent event: the agent's
// reply text, plus a status that may mark the run as failed.
type messageEvent struct {
	Status  string `json:"status"`
	Content struct {
		Markdown string `json:"markdown"`
	} `json:"content"`
}

// run performs one turn: start the run, read its stream, and emit the result.
// It owns the events channel's lifetime.
func (s *serverSession) run(ctx context.Context, l *Launcher, base string, req *agent.Request) {
	defer s.Close()

	conversationID := req.SessionID
	if conversationID == "" {
		conversationID = newID()
		s.Emit(agent.Event{Kind: agent.EventSession, SessionID: conversationID})
	}

	runID, err := l.startRun(ctx, base, conversationID, req.Prompt)
	if err != nil {
		s.EmitError(ctx, err)
		return
	}
	s.SetAbort(func() { l.cancelRun(base, runID) })

	sawMessage, err := s.stream(ctx, l, base, runID)
	if err != nil {
		s.EmitError(ctx, err)
		return
	}
	if !sawMessage {
		s.EmitError(ctx, errors.New("codebuddy server: run stream ended without a result"))
		return
	}
	s.Emit(agent.Event{Kind: agent.EventDone})
}

// stream opens the run's SSE stream and emits reply text until the run
// terminates. It reports whether any reply text was seen.
func (s *serverSession) stream(ctx context.Context, l *Launcher, base, runID string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/runs/"+runID+"/stream", nil)
	if err != nil {
		return false, err
	}
	req.Header.Set(csrfHeader, "1")
	req.Header.Set("accept", "text/event-stream")
	resp, err := l.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return false, fmt.Errorf("codebuddy server: stream %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	return s.consume(resp.Body)
}

// consume reads the run's event stream, forwarding reply text and stopping at
// the terminal `done` event.
func (s *serverSession) consume(r io.Reader) (bool, error) {
	sawMessage := false
	var handlerErr error
	readErr := agent.ReadSSE(r, func(event, data string) bool {
		sawText, done, err := s.handleEvent(event, data)
		sawMessage = sawMessage || sawText
		if err != nil {
			handlerErr = err
			return false
		}
		return !done
	})
	if handlerErr != nil {
		return sawMessage, handlerErr
	}
	return sawMessage, readErr
}

// handleEvent consumes one server-sent event. It reports whether the event
// carried reply text and whether it terminated the run.
func (s *serverSession) handleEvent(event, data string) (sawText, done bool, err error) {
	switch event {
	case "done":
		return false, true, nil
	case "message":
		var m messageEvent
		if json.Unmarshal([]byte(data), &m) != nil {
			return false, false, nil
		}
		if m.Status == "failed" || m.Status == "error" {
			return false, false, errors.New("codebuddy server: run failed")
		}
		if m.Content.Markdown == "" {
			return false, false, nil
		}
		s.Emit(agent.Event{Kind: agent.EventTextDelta, Text: m.Content.Markdown})
		return true, false, nil
	}
	return false, false, nil
}

// newID returns a random 128-bit hex id for run message and conversation ids.
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
