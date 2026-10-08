package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jiaweisky/harbor/internal/agent"
	"github.com/jiaweisky/harbor/internal/jsonrpc"
)

// serveStartTimeout bounds how long to wait for a spawned server to listen.
const serveStartTimeout = 30 * time.Second

// connIDHeader names the ACP connection on every HTTP request.
const connIDHeader = "acp-connection-id"

// HTTPConfig configures an ACP client over a CLI's HTTP/SSE ACP endpoint (for
// example `codebuddy --serve`, which serves ACP at /api/v1/acp). When BaseURL is
// empty the CLI is spawned with Args and its listening URL is read from stdout.
type HTTPConfig struct {
	Name            string
	Binary          string
	Args            []string
	BaseURL         string
	Env             []string
	Headers         http.Header // extra request headers (for example a CSRF header)
	Auth            agent.Authenticator
	InheritEnv      bool
	CLIHome         string
	VersionArgs     []string
	VersionOverride string
	Lifecycle       context.Context
}

// HTTPLauncher drives a CLI's ACP-over-HTTP endpoint. Unlike the stdio launcher
// it does not stop the CLI per task: the server is spawned once and reused,
// while each task opens its own ACP connection.
type HTTPLauncher struct {
	cfg      HTTPConfig
	runner   *agent.Runner
	endpoint agent.ServerEndpoint
	client   *http.Client
}

// NewHTTPLauncher builds an ACP-over-HTTP launcher.
func NewHTTPLauncher(cfg HTTPConfig) *HTTPLauncher {
	if cfg.Lifecycle == nil {
		cfg.Lifecycle = context.Background()
	}
	return &HTTPLauncher{
		cfg:    cfg,
		runner: agent.NewRunner(1, cfg.InheritEnv, cfg.CLIHome),
		client: &http.Client{},
	}
}

func (l *HTTPLauncher) Name() string              { return l.cfg.Name }
func (l *HTTPLauncher) Auth() agent.Authenticator { return l.cfg.Auth }

// AuthReady reports the CLI's best-effort authentication status.
func (l *HTTPLauncher) AuthReady(ctx context.Context) bool {
	return agent.AuthReadyFor(ctx, l.cfg.Binary, l.runner, l.cfg.Env, l.cfg.Auth)
}

func (l *HTTPLauncher) Capabilities() agent.Capabilities { return acpCapabilities() }

// Probe reports the binary path and version.
func (l *HTTPLauncher) Probe(ctx context.Context) agent.Info {
	return agent.ProbeBinary(ctx, l.cfg.Name, l.cfg.Binary, l.cfg.VersionArgs, l.cfg.VersionOverride)
}

// Start opens an ACP connection and runs one turn against it. The connection
// lives for the turn; the server process is reused across tasks.
func (l *HTTPLauncher) Start(ctx context.Context, req *agent.Request) (agent.Session, error) {
	base, err := l.endpoint.URL(ctx, l.cfg.BaseURL, l.spawn)
	if err != nil {
		return nil, err
	}
	connID, err := l.connect(ctx, base)
	if err != nil {
		return nil, err
	}
	return l.establish(ctx, base, connID, req)
}

// establish builds the ACP link for one turn: it starts the notification
// subscription, runs the handshake, and dispatches the prompt. On any failure
// the link is torn down.
func (l *HTTPLauncher) establish(ctx context.Context, base, connID string, req *agent.Request) (*session, error) {
	ch := make(chan agent.Event, 16)
	linkCtx, stopLink := context.WithCancel(context.Background())
	var once sync.Once
	stop := func() {
		once.Do(func() {
			stopLink()
			l.disconnect(base, connID)
			close(ch)
		})
	}

	s := &session{events: ch, kill: stop}
	w := &postWriter{ctx: linkCtx, client: l.client, url: base + "/api/v1/acp", connID: connID, headers: l.cfg.Headers}
	c := &conn{Conn: jsonrpc.New(w), s: s}
	w.handle = c.Handle
	s.conn = c
	c.SetHandler(c)
	// Drop events until the handshake completes so a resume's replayed history
	// is not delivered as this turn's output.
	c.setEmit(func(agent.Event) {})

	// The server pushes unsolicited notifications on the subscription stream; it
	// must be established before the handshake so nothing is missed.
	ready := make(chan error, 1)
	go l.readInbound(linkCtx, base, connID, c, stop, ready)
	if err := <-ready; err != nil {
		stop()
		return nil, err
	}

	cwd, err := absWorkDir(req.WorkDir)
	if err != nil {
		stop()
		return nil, err
	}
	sessionID, err := handshakeHTTP(ctx, c, cwd, req.SessionID)
	if err != nil {
		stop()
		return nil, err
	}
	c.setEmit(func(e agent.Event) { ch <- e })
	c.emitEvent(agent.Event{Kind: agent.EventSession, SessionID: sessionID})
	s.startPrompt(ctx, sessionID, req.Prompt)
	return s, nil
}

// spawn starts the CLI's HTTP server and returns its base URL.
func (l *HTTPLauncher) spawn(ctx context.Context) (string, error) {
	inv := &agent.Invocation{
		Bin:  l.cfg.Binary,
		Args: append([]string(nil), l.cfg.Args...),
		Env:  append([]string(nil), l.cfg.Env...),
	}
	return agent.SpawnServer(ctx, l.cfg.Lifecycle, l.runner, l.cfg.Name+":server", inv, serveStartTimeout)
}

// connect opens an ACP connection and returns its id.
func (l *HTTPLauncher) connect(ctx context.Context, base string) (string, error) {
	var out struct {
		ConnectionID string `json:"connectionId"`
	}
	if err := agent.PostJSON(ctx, l.client, base+"/api/v1/acp/connect", "acp connect", l.cfg.Headers, map[string]any{}, &out); err != nil {
		return "", err
	}
	if out.ConnectionID == "" {
		return "", errors.New("acp connect: no connection id")
	}
	return out.ConnectionID, nil
}

// disconnect closes the ACP connection.
func (l *HTTPLauncher) disconnect(base, connID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, base+"/api/v1/acp", nil)
	if err != nil {
		return
	}
	applyHeaders(req, l.cfg.Headers)
	req.Header.Set(connIDHeader, connID)
	resp, err := l.client.Do(req)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

// readInbound subscribes to the server's notification stream, reports readiness
// on ready once the subscription is established, then feeds inbound frames into
// the connection until the link is stopped.
func (l *HTTPLauncher) readInbound(ctx context.Context, base, connID string, c *conn, stop func(), ready chan<- error) {
	defer stop()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/acp", nil)
	if err != nil {
		ready <- err
		return
	}
	req.Header.Set("accept", "text/event-stream")
	applyHeaders(req, l.cfg.Headers)
	req.Header.Set(connIDHeader, connID)
	resp, err := l.client.Do(req)
	if err != nil {
		ready <- err
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		ready <- fmt.Errorf("acp subscribe: %s", resp.Status)
		return
	}
	ready <- nil
	feedSSE(resp.Body, c.Handle)
}

// handshakeHTTP runs the ACP setup over HTTP. When resumeID is set it loads that
// session, otherwise it creates a new one.
func handshakeHTTP(ctx context.Context, c *conn, cwd, resumeID string) (string, error) {
	if err := initialize(ctx, c); err != nil {
		return "", err
	}
	if resumeID == "" {
		return openSession(ctx, c, cwd)
	}
	res, err := c.Conn.Request(ctx, "session/load", map[string]any{
		"sessionId": resumeID, "cwd": cwd, "mcpServers": []any{},
	})
	if err != nil {
		return "", fmt.Errorf("acp session/load: %w", err)
	}
	var ns newSessionResponse
	if json.Unmarshal(res, &ns) == nil && ns.SessionID != "" {
		c.setSessionID(ns.SessionID)
		return ns.SessionID, nil
	}
	// session/load need not echo the id; keep the requested one.
	c.setSessionID(resumeID)
	return resumeID, nil
}

// postWriter sends each outbound JSON-RPC frame as an HTTP POST. The command's
// response and its related notifications stream back on the POST reply, which is
// read asynchronously so the write path is not blocked.
type postWriter struct {
	ctx     context.Context
	client  *http.Client
	url     string
	connID  string
	headers http.Header
	handle  func([]byte)
}

func (w *postWriter) Write(p []byte) (int, error) {
	body := bytes.TrimSpace(p)
	req, err := http.NewRequestWithContext(w.ctx, http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "application/json, text/event-stream")
	applyHeaders(req, w.headers)
	req.Header.Set(connIDHeader, w.connID)
	resp, err := w.client.Do(req)
	if err != nil {
		return 0, err
	}
	if resp.StatusCode/100 != 2 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		return 0, fmt.Errorf("acp http: %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	go w.readReply(resp.Body)
	return len(p), nil
}

// readReply reads a POST reply's SSE stream and feeds it to the connection.
func (w *postWriter) readReply(body io.ReadCloser) {
	defer body.Close()
	feedSSE(body, w.handle)
}

// feedSSE delivers each JSON payload of an SSE stream to handle.
func feedSSE(r io.Reader, handle func([]byte)) {
	_ = agent.ReadSSE(r, func(_, data string) bool {
		handle([]byte(data))
		return true
	})
}

// applyHeaders adds every header entry to req.
func applyHeaders(req *http.Request, h http.Header) {
	for k, vs := range h {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
}
