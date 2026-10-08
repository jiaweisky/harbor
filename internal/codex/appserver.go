// Package codex integrates codex's app-server (newline-delimited JSON-RPC over
// stdio) as a long-running A2A backend. Harbor starts `codex app-server` once,
// opens a thread per A2A context, and streams assistant deltas from a turn.
package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jiaweisky/harbor/internal/agent"
	"github.com/jiaweisky/harbor/internal/jsonrpc"
)

// Config configures a codex app-server launcher.
type Config struct {
	Name            string
	Binary          string
	Env             []string
	InheritEnv      bool
	CLIHome         string
	Container       *agent.ContainerSpec
	Auth            agent.Authenticator
	VersionArgs     []string
	VersionOverride string
}

// Launcher drives codex through its app-server.
type Launcher struct {
	cfg       Config
	runner    *agent.Runner
	lifecycle context.Context

	mu   sync.Mutex
	conn *conn
}

// NewServerLauncher builds an app-server launcher. lifecycle bounds the server
// process: canceling it stops the server.
func NewServerLauncher(lifecycle context.Context, cfg Config) *Launcher {
	if lifecycle == nil {
		lifecycle = context.Background()
	}
	return &Launcher{
		cfg:       cfg,
		runner:    agent.NewRunner(1, cfg.InheritEnv, cfg.CLIHome, cfg.Container),
		lifecycle: lifecycle,
	}
}

func (l *Launcher) Name() string { return l.cfg.Name }

// Capabilities describes the app-server mode: token streaming over JSON-RPC,
// with approvals disabled up front so a turn never blocks on a prompt.
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

// Start opens (or resumes) a thread and runs one turn. The thread id is
// reported as the session id so later turns of the same context resume it.
func (l *Launcher) Start(ctx context.Context, req *agent.Request) (agent.Session, error) {
	c, err := l.ensureServer(ctx)
	if err != nil {
		return nil, err
	}
	threadID, err := c.thread(ctx, req.SessionID, req.WorkDir)
	if err != nil {
		return nil, err
	}
	s := &appSession{
		threadID: threadID,
		conn:     c,
		events:   make(chan agent.Event, 64),
		deltas:   make(chan string, 128),
		done:     make(chan turnResult, 1),
		stopped:  make(chan struct{}),
	}
	c.add(threadID, s)
	go s.run(ctx, c, req.Prompt)
	return s, nil
}

// ensureServer spawns the app-server and initializes it on first use.
func (l *Launcher) ensureServer(ctx context.Context) (*conn, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conn != nil {
		return l.conn, nil
	}
	taskID := l.cfg.Name + ":app-server"
	d := &dispatcher{}
	inv := &agent.Invocation{
		Bin: l.cfg.Binary, Args: []string{"app-server"}, Env: l.cfg.Env,
		KeepStdinOpen: true, // JSON-RPC requests are written to the server's stdin
	}
	// The dispatcher routes each stdout record to the connection, so the
	// runner's event channel is unused.
	if _, err := l.runner.Start(l.lifecycle, taskID, inv, d); err != nil {
		return nil, err
	}
	c := &conn{Conn: jsonrpc.New(l.runner.Stdin(taskID)), sessions: map[string]*appSession{}}
	c.SetHandler(c)
	d.setConn(c)
	if err := c.initialize(ctx); err != nil {
		l.runner.Kill(taskID)
		return nil, err
	}
	l.conn = c
	return c, nil
}

// --- JSON-RPC connection ---

// conn is the JSON-RPC peer over the app-server's stdin/stdout. The transport
// is shared; this type owns the routing of notifications to sessions.
type conn struct {
	*jsonrpc.Conn
	mu       sync.Mutex
	sessions map[string]*appSession
}

func (c *conn) add(threadID string, s *appSession) {
	c.mu.Lock()
	c.sessions[threadID] = s
	c.mu.Unlock()
}

func (c *conn) remove(threadID string) {
	c.mu.Lock()
	delete(c.sessions, threadID)
	c.mu.Unlock()
}

func (c *conn) session(threadID string) *appSession {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessions[threadID]
}

// initialize performs the one-time server handshake.
func (c *conn) initialize(ctx context.Context) error {
	params := map[string]any{"clientInfo": map[string]any{"name": "harbor", "version": "0.1"}}
	if _, err := c.Conn.Request(ctx, "initialize", params); err != nil {
		return fmt.Errorf("codex initialize: %w", err)
	}
	return nil
}

// thread opens a new thread or resumes an existing one, returning its id.
func (c *conn) thread(ctx context.Context, resumeID, cwd string) (string, error) {
	var res json.RawMessage
	var err error
	if resumeID != "" {
		res, err = c.Conn.Request(ctx, "thread/resume", map[string]any{"threadId": resumeID})
	} else {
		params := map[string]any{"approvalPolicy": "never"} // never prompt; run unattended
		if cwd != "" {
			params["cwd"] = cwd
		}
		res, err = c.Conn.Request(ctx, "thread/start", params)
	}
	if err != nil {
		return "", fmt.Errorf("codex thread: %w", err)
	}
	var r struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(res, &r); err != nil || r.Thread.ID == "" {
		return "", errors.New("codex thread: missing thread id")
	}
	return r.Thread.ID, nil
}

// Notification routes a server notification to the owning session.
func (c *conn) Notification(method string, params json.RawMessage) {
	switch method {
	case "item/agentMessage/delta":
		var p struct {
			ThreadID string `json:"threadId"`
			Delta    string `json:"delta"`
		}
		if json.Unmarshal(params, &p) == nil && p.Delta != "" {
			if s := c.session(p.ThreadID); s != nil {
				s.deliverDelta(p.Delta)
			}
		}
	case "turn/completed":
		var p struct {
			ThreadID string `json:"threadId"`
			Turn     struct {
				Status string     `json:"status"`
				Error  *turnError `json:"error"`
			} `json:"turn"`
		}
		if json.Unmarshal(params, &p) == nil {
			if s := c.session(p.ThreadID); s != nil {
				s.deliverDone(p.Turn.Status, p.Turn.Error)
			}
		}
	case "error":
		var p struct {
			ThreadID  string `json:"threadId"`
			WillRetry bool   `json:"willRetry"`
			Error     struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(params, &p) == nil && !p.WillRetry {
			if s := c.session(p.ThreadID); s != nil {
				s.deliverDone("failed", &turnError{Message: p.Error.Message})
			}
		}
	}
}

// Request refuses server-initiated requests: approvals are disabled up front,
// so none is expected, and refusing keeps the server from waiting.
func (c *conn) Request(id json.RawMessage, method string, _ json.RawMessage) {
	_ = c.RespondError(id, -32601, "method not found: "+method)
}

// dispatcher routes each stdout record to the connection. It implements
// agent.Parser.
type dispatcher struct {
	mu sync.Mutex
	c  *conn
}

func (d *dispatcher) setConn(c *conn) {
	d.mu.Lock()
	d.c = c
	d.mu.Unlock()
}

func (d *dispatcher) Parse(rec []byte, _ func(agent.Event)) error {
	d.mu.Lock()
	c := d.c
	d.mu.Unlock()
	if c != nil {
		c.Handle(rec)
	}
	return nil
}

func (d *dispatcher) Flush(func(agent.Event)) error { return nil }

// --- session ---

// turnError is the error object on a failed turn or a non-retrying error
// notification.
type turnError struct {
	Message string `json:"message"`
}

// turnResult is the terminal outcome of a turn.
type turnResult struct {
	status string
	err    string
}

// appSession streams one turn of one thread as A2A events.
type appSession struct {
	threadID string
	conn     *conn

	events  chan agent.Event
	deltas  chan string
	done    chan turnResult
	stopped chan struct{}

	turnMu sync.Mutex
	turnID string
	cancel context.CancelFunc
	closed atomic.Bool
}

func (s *appSession) Events() <-chan agent.Event { return s.events }

func (s *appSession) Send(context.Context, agent.Input) error { return agent.ErrUnsupported }

func (s *appSession) Canceled() bool { return s.closed.Load() }

// Cancel interrupts the running turn and stops streaming.
func (s *appSession) Cancel() error {
	s.closed.Store(true)
	s.turnMu.Lock()
	turnID := s.turnID
	cancel := s.cancel
	s.turnMu.Unlock()
	if turnID != "" {
		ctx, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		_, _ = s.conn.Conn.Request(ctx, "turn/interrupt", map[string]any{"threadId": s.threadID, "turnId": turnID})
	}
	if cancel != nil {
		cancel()
	}
	return nil
}

// deliverDelta forwards a text delta, unless the run has already ended.
func (s *appSession) deliverDelta(delta string) {
	select {
	case s.deltas <- delta:
	case <-s.stopped:
	}
}

// deliverDone signals the turn's terminal outcome.
func (s *appSession) deliverDone(status string, turnErr *turnError) {
	res := turnResult{status: status}
	if turnErr != nil {
		res.err = turnErr.Message
	}
	select {
	case s.done <- res:
	case <-s.stopped:
	}
}

// run starts the turn and streams its events until it ends. It owns the events
// channel's lifetime.
func (s *appSession) run(ctx context.Context, c *conn, prompt string) {
	ctx, cancel := context.WithCancel(ctx)
	s.turnMu.Lock()
	s.cancel = cancel
	s.turnMu.Unlock()
	defer cancel()
	defer close(s.events)
	defer close(s.stopped)
	defer c.remove(s.threadID)

	s.events <- agent.Event{Kind: agent.EventSession, SessionID: s.threadID}

	turnID, err := c.startTurn(ctx, s.threadID, prompt)
	if err != nil {
		if ctx.Err() == nil {
			s.events <- agent.Event{Kind: agent.EventError, Err: err}
		}
		return
	}
	s.turnMu.Lock()
	s.turnID = turnID
	s.turnMu.Unlock()

	for {
		select {
		case delta := <-s.deltas:
			s.events <- agent.Event{Kind: agent.EventTextDelta, Text: delta}
		case res := <-s.done:
			// The connection enqueues every delta before the terminal, so drain
			// the buffer first: a random select could otherwise drop text.
			s.flushDeltas()
			if res.status == "failed" || res.err != "" {
				msg := agent.FirstNonEmpty(res.err, "codex turn failed")
				s.events <- agent.Event{Kind: agent.EventError, Err: errors.New(msg)}
				return
			}
			s.events <- agent.Event{Kind: agent.EventDone}
			return
		case <-ctx.Done():
			return
		}
	}
}

// flushDeltas emits any text deltas still buffered, without blocking.
func (s *appSession) flushDeltas() {
	for {
		select {
		case delta := <-s.deltas:
			s.events <- agent.Event{Kind: agent.EventTextDelta, Text: delta}
		default:
			return
		}
	}
}

// startTurn sends the prompt and returns the turn id.
func (c *conn) startTurn(ctx context.Context, threadID, prompt string) (string, error) {
	params := map[string]any{
		"threadId": threadID,
		"input":    []map[string]any{{"type": "text", "text": prompt}},
	}
	res, err := c.Conn.Request(ctx, "turn/start", params)
	if err != nil {
		return "", err
	}
	var r struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if err := json.Unmarshal(res, &r); err != nil || r.Turn.ID == "" {
		return "", errors.New("codex turn/start: missing turn id")
	}
	return r.Turn.ID, nil
}
