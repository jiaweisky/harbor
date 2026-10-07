// Package acp integrates a coding-agent CLI that speaks the Agent Client
// Protocol (ACP) over newline-delimited JSON-RPC on its stdin/stdout.
//
// Harbor acts as the ACP client: it starts the CLI, drives a single prompt turn
// (`initialize` -> `session/new` -> `session/prompt`), streams the agent's
// `session/update` notifications as normalized events, and bridges
// `session/request_permission` to an A2A input-required pause. The process is
// stopped once the turn ends, so one A2A task maps to one ACP session lifetime.
package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/jiaweisky/harbor/internal/agent"
)

// protocolVersion is the ACP protocol version Harbor speaks.
const protocolVersion = 1

// Config configures an ACP launcher.
type Config struct {
	Name            string
	Binary          string
	Args            []string
	Env             []string // extra environment entries ("K=V"), on top of the inherited/allowlisted base
	Auth            agent.Authenticator
	MaxConcurrent   int
	InheritEnv      bool
	CLIHome         string
	VersionArgs     []string
	VersionOverride string
}

// Launcher drives a CLI in its ACP mode.
type Launcher struct {
	cfg    Config
	runner *agent.Runner
}

// NewLauncher builds an ACP launcher. The CLI is invoked with Args, which must
// select ACP mode (for example `opencode acp`).
func NewLauncher(cfg Config) *Launcher {
	return &Launcher{
		cfg:    cfg,
		runner: agent.NewRunner(cfg.MaxConcurrent, cfg.InheritEnv, cfg.CLIHome),
	}
}

func (l *Launcher) Name() string { return l.cfg.Name }

func (l *Launcher) Auth() agent.Authenticator { return l.cfg.Auth }

// authEnv is the environment a CLI's auth commands run with.
func (l *Launcher) authEnv() []string { return l.runner.Env(&agent.Invocation{Env: l.cfg.Env}) }

// AuthReady reports whether the CLI currently holds a usable ticket.
func (l *Launcher) AuthReady(ctx context.Context) bool {
	return agent.Authenticated(ctx, l.cfg.Binary, l.runner.AuthBase(), l.authEnv(), l.cfg.Auth)
}

// EnsureLogin verifies the CLI holds a ticket, attempting its login once when
// none is present.
func (l *Launcher) EnsureLogin(ctx context.Context) error {
	return agent.EnsureLogin(ctx, l.cfg.Name, l.cfg.Binary, l.runner.AuthBase(), l.authEnv(), l.cfg.Auth)
}

// Capabilities describes an ACP agent: token streaming, tool traces, and native
// interaction through permission requests, which resume within the turn.
func (l *Launcher) Capabilities() agent.Capabilities {
	return agent.Capabilities{
		InputText:           true,
		InputFile:           true,
		InputPrompt:         true,
		OutputText:          true,
		ToolTrace:           true,
		Permission:          agent.PermInteractive,
		InputRequiredResume: true,
		Streaming:           agent.StreamToken,
	}
}

// Probe reports the binary path and version. Version probing is skipped when no
// version arguments are configured; an override is reported verbatim.
func (l *Launcher) Probe(ctx context.Context) agent.Info {
	path, err := exec.LookPath(l.cfg.Binary)
	if err != nil {
		return agent.Info{Name: l.cfg.Name, Enabled: false, Err: err}
	}
	version := l.cfg.VersionOverride
	if version == "" {
		version = agent.DetectVersion(ctx, path, l.cfg.VersionArgs)
	}
	return agent.Info{Name: l.cfg.Name, Path: path, Enabled: true, Version: version}
}

// Start launches the CLI and completes the ACP handshake. The first prompt is
// dispatched asynchronously; its events are delivered on the returned Session.
func (l *Launcher) Start(ctx context.Context, req *agent.Request) (agent.Session, error) {
	cwd, err := absWorkDir(req.WorkDir)
	if err != nil {
		return nil, err
	}
	if req.Login {
		return l.startLogin(ctx, cwd)
	}

	taskID := agent.TaskIDFrom(ctx)
	inv := &agent.Invocation{
		Bin:           l.cfg.Binary,
		Args:          append([]string(nil), l.cfg.Args...),
		Dir:           cwd,
		Env:           append([]string(nil), l.cfg.Env...),
		KeepStdinOpen: true,
	}
	d := &dispatcher{}
	events, err := l.runner.Start(ctx, taskID, inv, d)
	if err != nil {
		return nil, err
	}

	s := &session{events: events, kill: func() { l.runner.Kill(taskID) }}
	c := &conn{stdin: l.runner.Stdin(taskID), pending: map[int64]chan rpcMessage{}, s: s}
	s.conn = c
	d.setConn(c)

	sessionID, err := handshake(ctx, c, cwd)
	if err != nil {
		_ = s.Cancel()
		return nil, err
	}
	s.startPrompt(ctx, sessionID, req.Prompt)
	return s, nil
}

// startLogin runs the CLI's login command and streams its output so a client
// can read a device-code URL and complete the login out of band. It does not
// speak ACP: the login command is a plain subprocess.
func (l *Launcher) startLogin(ctx context.Context, cwd string) (agent.Session, error) {
	if l.cfg.Auth == nil || len(l.cfg.Auth.LoginArgs()) == 0 {
		return nil, agent.ErrNoLogin
	}
	inv := &agent.Invocation{
		Bin:  l.cfg.Binary,
		Args: append([]string(nil), l.cfg.Auth.LoginArgs()...),
		Dir:  cwd,
		Env:  append([]string(nil), l.cfg.Env...),
	}
	taskID := agent.TaskIDFrom(ctx)
	events, err := l.runner.Start(ctx, taskID, inv, agent.NewLineParser())
	if err != nil {
		return nil, err
	}
	return &loginSession{events: events, kill: func() { l.runner.Kill(taskID) }}, nil
}

// loginSession streams a login command's output; it accepts no out-of-band
// input.
type loginSession struct {
	events   <-chan agent.Event
	kill     func()
	canceled atomic.Bool
}

func (s *loginSession) Events() <-chan agent.Event { return s.events }

func (s *loginSession) Send(context.Context, agent.Input) error { return agent.ErrUnsupported }

func (s *loginSession) Cancel() error {
	s.canceled.Store(true)
	if s.kill != nil {
		s.kill()
	}
	return nil
}

func (s *loginSession) Canceled() bool { return s.canceled.Load() }

// absWorkDir returns an absolute working directory, falling back to the process
// working directory when the request carries none.
func absWorkDir(dir string) (string, error) {
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		dir = wd
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("acp: working directory must be absolute: %q", dir)
	}
	return dir, nil
}

// handshake runs the ACP setup sequence and returns the new session id.
func handshake(ctx context.Context, c *conn, cwd string) (string, error) {
	if _, err := c.send(ctx, "initialize", map[string]any{
		"protocolVersion":    protocolVersion,
		"clientCapabilities": map[string]any{},
	}); err != nil {
		return "", fmt.Errorf("acp initialize: %w", err)
	}
	res, err := c.send(ctx, "session/new", map[string]any{"cwd": cwd, "mcpServers": []any{}})
	if err != nil {
		return "", fmt.Errorf("acp session/new: %w", err)
	}
	var ns newSessionResponse
	if err := json.Unmarshal(res, &ns); err != nil || ns.SessionID == "" {
		return "", fmt.Errorf("acp session/new: missing session id")
	}
	c.setSessionID(ns.SessionID)
	return ns.SessionID, nil
}

// session is one ACP prompt turn backed by a live CLI process.
type session struct {
	conn         *conn
	events       <-chan agent.Event
	kill         func()
	canceled     bool
	cancelPrompt context.CancelFunc
	mu           sync.Mutex
	permIDs      []json.RawMessage // permission requests awaiting an answer, oldest first
}

func (s *session) Events() <-chan agent.Event { return s.events }

// Send answers the oldest pending permission request. The answer is an ACP
// permission option id; an empty answer cancels the request.
func (s *session) Send(_ context.Context, in agent.Input) error {
	s.mu.Lock()
	if len(s.permIDs) == 0 {
		s.mu.Unlock()
		return agent.ErrUnsupported
	}
	id := s.permIDs[0]
	s.permIDs = s.permIDs[1:]
	s.mu.Unlock()

	outcome := map[string]any{"outcome": "cancelled"}
	if in.Answer != "" {
		outcome = map[string]any{"outcome": "selected", "optionId": in.Answer}
	}
	return s.conn.respond(id, map[string]any{"outcome": outcome})
}

func (s *session) Cancel() error {
	s.mu.Lock()
	s.canceled = true
	cancel := s.cancelPrompt
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if sid := s.conn.sessionID(); sid != "" {
		_ = s.conn.notify("session/cancel", map[string]any{"sessionId": sid})
	}
	if s.kill != nil {
		s.kill()
	}
	return nil
}

func (s *session) Canceled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.canceled
}

// startPrompt sends the prompt and, when the turn ends, stops the process so the
// event channel closes and the task completes.
func (s *session) startPrompt(ctx context.Context, sessionID, prompt string) {
	params := map[string]any{
		"sessionId": sessionID,
		"prompt":    []map[string]any{{"type": "text", "text": prompt}},
	}
	promptCtx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.cancelPrompt = cancel
	s.mu.Unlock()

	go func() {
		defer cancel()
		res, err := s.conn.send(promptCtx, "session/prompt", params)
		if err != nil {
			if s.Canceled() || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return
			}
			s.conn.emitEvent(agent.Event{Kind: agent.EventError, Err: err})
			return
		}
		var pr promptResponse
		_ = json.Unmarshal(res, &pr)
		if u := pr.usage(); u != nil {
			s.conn.emitEvent(agent.Event{Kind: agent.EventUsage, Usage: u})
		}
		if s.kill != nil {
			s.kill()
		}
	}()
}

// onPermission records a permission request and surfaces it as an event.
func (s *session) onPermission(id json.RawMessage, p permissionParams) {
	s.mu.Lock()
	s.permIDs = append(s.permIDs, id)
	s.mu.Unlock()
	s.conn.emitEvent(agent.Event{
		Kind:      agent.EventPermissionRequest,
		Text:      permissionText(p),
		Tool:      agent.FirstNonEmpty(p.ToolCall.Kind, p.ToolCall.Title),
		ToolInput: permissionInput(p),
		Interaction: &agent.Interaction{
			Kind:    agent.InteractionPermission,
			Action:  agent.FirstNonEmpty(p.ToolCall.Kind, p.ToolCall.Title),
			Target:  p.ToolCall.Title,
			Options: permissionOptionIDs(p.Options),
			Resume:  true,
		},
	})
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

func (d *dispatcher) Parse(rec []byte, out func(agent.Event)) error {
	d.mu.Lock()
	c := d.c
	d.mu.Unlock()
	if c != nil {
		c.handle(rec, out)
	}
	return nil
}

func (d *dispatcher) Flush(func(agent.Event)) error { return nil }

// conn is the JSON-RPC peer over the CLI's stdin/stdout.
type conn struct {
	writeMu sync.Mutex
	mu      sync.Mutex
	stdin   io.Writer
	nextID  int64
	pending map[int64]chan rpcMessage
	emit    func(agent.Event)
	s       *session
	sessID  string
}

func (c *conn) setSessionID(id string) {
	c.mu.Lock()
	c.sessID = id
	c.mu.Unlock()
}

func (c *conn) sessionID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessID
}

// emitEvent forwards a normalized event to the session's event channel.
func (c *conn) emitEvent(ev agent.Event) {
	c.mu.Lock()
	emit := c.emit
	c.mu.Unlock()
	if emit != nil {
		emit(ev)
	}
}

// send writes a request and waits for its response or context cancellation.
func (c *conn) send(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan rpcMessage, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	if err := c.write(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}); err != nil {
		c.forget(id)
		return nil, err
	}
	select {
	case m := <-ch:
		if m.Error != nil {
			return nil, m.Error
		}
		return m.Result, nil
	case <-ctx.Done():
		c.forget(id)
		return nil, ctx.Err()
	}
}

// forget drops the pending slot for a request that will not receive a response.
func (c *conn) forget(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// notify writes a notification (a request without an id).
func (c *conn) notify(method string, params any) error {
	return c.write(rpcRequest{JSONRPC: "2.0", Method: method, Params: params})
}

// respond answers an inbound request from the agent.
func (c *conn) respond(id json.RawMessage, result any) error {
	return c.write(rpcResponse{JSONRPC: "2.0", ID: id, Result: result})
}

func (c *conn) respondError(id json.RawMessage, code int, msg string) error {
	return c.write(rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}})
}

func (c *conn) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.stdin == nil {
		return errors.New("acp: connection closed")
	}
	_, err = c.stdin.Write(b)
	return err
}

// handle consumes one stdout record.
func (c *conn) handle(line []byte, emit func(agent.Event)) {
	c.mu.Lock()
	if c.emit == nil {
		c.emit = emit
	}
	c.mu.Unlock()

	var m rpcMessage
	if err := json.Unmarshal(line, &m); err != nil {
		return
	}
	switch {
	case m.Method != "" && len(m.ID) == 0:
		c.handleNotification(m)
	case m.Method != "":
		c.handleRequest(m)
	default:
		c.resolveResponse(m)
	}
}

// chunkEvent maps streaming text updates to their normalized event kind.
var chunkEvent = map[string]agent.EventKind{
	"agent_message_chunk": agent.EventTextDelta,
	"agent_thought_chunk": agent.EventThinking,
}

func (c *conn) handleNotification(m rpcMessage) {
	if m.Method != "session/update" {
		return
	}
	var p sessionUpdateParams
	if json.Unmarshal(m.Params, &p) != nil {
		return
	}
	if kind, ok := chunkEvent[p.Update.SessionUpdate]; ok {
		if t := p.Update.contentText(); t != "" {
			c.emitEvent(agent.Event{Kind: kind, Text: t})
		}
		return
	}
	switch p.Update.SessionUpdate {
	case "tool_call":
		c.emitEvent(agent.Event{Kind: agent.EventToolCall,
			Tool: agent.FirstNonEmpty(p.Update.Title, p.Update.Kind)})
	case "tool_call_update":
		// Only a finished tool call is a result; in-progress updates would be
		// misleading.
		if p.Update.Status == "completed" || p.Update.Status == "failed" {
			c.emitEvent(agent.Event{Kind: agent.EventToolResult})
		}
	}
}

func (c *conn) handleRequest(m rpcMessage) {
	switch m.Method {
	case "session/request_permission":
		var p permissionParams
		if json.Unmarshal(m.Params, &p) != nil {
			_ = c.respondError(m.ID, codeInvalidParams, "invalid params")
			return
		}
		c.s.onPermission(m.ID, p)
	default:
		// fs/* and terminal/* are not advertised, so the agent must not call
		// them; answer with a method-not-found error for a clean failure.
		_ = c.respondError(m.ID, codeMethodNotFound, "method not found: "+m.Method)
	}
}

// resolveResponse delivers a response to the goroutine waiting in send.
func (c *conn) resolveResponse(m rpcMessage) {
	id, err := parseID(m.ID)
	if err != nil {
		return
	}
	c.mu.Lock()
	ch := c.pending[id]
	delete(c.pending, id)
	c.mu.Unlock()
	if ch != nil {
		ch <- m
	}
}

// --- wire types ---

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("acp: %s (%d)", e.Message, e.Code) }

// JSON-RPC error codes used in responses to the agent.
const (
	codeInvalidParams  = -32602
	codeMethodNotFound = -32601
)

type newSessionResponse struct {
	SessionID string `json:"sessionId"`
}

type promptResponse struct {
	Usage *usageEntry `json:"usage"`
}

func (p promptResponse) usage() *agent.Usage {
	if p.Usage == nil {
		return nil
	}
	return &agent.Usage{InputTokens: p.Usage.InputTokens, OutputTokens: p.Usage.OutputTokens}
}

type usageEntry struct {
	InputTokens  int `json:"inputTokens"`
	OutputTokens int `json:"outputTokens"`
}

type sessionUpdateParams struct {
	Update sessionUpdate `json:"update"`
}

type sessionUpdate struct {
	SessionUpdate string   `json:"sessionUpdate"`
	Content       *content `json:"content"`
	Title         string   `json:"title"`
	Kind          string   `json:"kind"`
	Status        string   `json:"status"`
}

func (u sessionUpdate) contentText() string {
	if u.Content == nil || u.Content.Type != "text" {
		return ""
	}
	return u.Content.Text
}

type content struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type permissionParams struct {
	ToolCall toolCallUpdate     `json:"toolCall"`
	Options  []permissionOption `json:"options"`
}

type toolCallUpdate struct {
	ToolCallID string `json:"toolCallId"`
	Title      string `json:"title"`
	Kind       string `json:"kind"`
}

type permissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
}

// permissionText renders a human-readable prompt listing the option ids the
// client must echo back to answer.
func permissionText(p permissionParams) string {
	var b strings.Builder
	b.WriteString("permission required")
	if t := agent.FirstNonEmpty(p.ToolCall.Title, p.ToolCall.Kind); t != "" {
		b.WriteString(": ")
		b.WriteString(t)
	}
	if labels := optionLabels(p.Options); len(labels) > 0 {
		b.WriteString(" (reply with one of: ")
		b.WriteString(strings.Join(labels, ", "))
		b.WriteString(")")
	}
	return b.String()
}

// permissionInput exposes the option ids and the tool call for programmatic use.
func permissionInput(p permissionParams) map[string]any {
	return map[string]any{
		"toolCallId": p.ToolCall.ToolCallID,
		"options":    permissionOptionIDs(p.Options),
	}
}

// usableOptions drops options without an id, which the client cannot choose.
func usableOptions(options []permissionOption) []permissionOption {
	usable := make([]permissionOption, 0, len(options))
	for _, o := range options {
		if o.OptionID != "" {
			usable = append(usable, o)
		}
	}
	return usable
}

func permissionOptionIDs(options []permissionOption) []string {
	usable := usableOptions(options)
	ids := make([]string, len(usable))
	for i, o := range usable {
		ids[i] = o.OptionID
	}
	return ids
}

// optionLabels renders each option as "id" or "id (name)".
func optionLabels(options []permissionOption) []string {
	usable := usableOptions(options)
	labels := make([]string, len(usable))
	for i, o := range usable {
		labels[i] = o.OptionID
		if o.Name != "" {
			labels[i] = o.OptionID + " (" + o.Name + ")"
		}
	}
	return labels
}

func parseID(raw json.RawMessage) (int64, error) {
	return strconv.ParseInt(strings.Trim(string(raw), `"`), 10, 64)
}
