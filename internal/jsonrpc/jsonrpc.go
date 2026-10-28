// Package jsonrpc provides a minimal newline-delimited JSON-RPC 2.0 connection
// for CLIs that speak JSON-RPC over stdin/stdout (for example ACP and codex
// app-server).
package jsonrpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
)

// Error is a JSON-RPC error object.
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string { return fmt.Sprintf("jsonrpc: %s (%d)", e.Message, e.Code) }

// message is one inbound record.
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

type request struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Handler receives inbound requests and notifications (responses are matched
// internally to their Request).
type Handler interface {
	Notification(method string, params json.RawMessage)
	Request(id json.RawMessage, method string, params json.RawMessage)
}

// Conn is a JSON-RPC 2.0 connection over the CLI's stdin. Records read from the
// CLI's stdout are passed to Handle.
type Conn struct {
	writeMu sync.Mutex
	mu      sync.Mutex
	stdin   io.Writer
	nextID  int64
	pending map[int64]chan message
	h       Handler
}

// New creates a connection writing to stdin.
func New(stdin io.Writer) *Conn {
	return &Conn{stdin: stdin, pending: map[int64]chan message{}}
}

// SetHandler installs the handler for inbound requests and notifications.
func (c *Conn) SetHandler(h Handler) {
	c.mu.Lock()
	c.h = h
	c.mu.Unlock()
}

// Request sends a request and waits for its response or context cancellation.
func (c *Conn) Request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan message, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	if err := c.write(request{JSONRPC: "2.0", ID: id, Method: method, Params: params}); err != nil {
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

// Notify sends a notification (a request without an id).
func (c *Conn) Notify(method string, params any) error {
	return c.write(request{JSONRPC: "2.0", Method: method, Params: params})
}

// Respond answers an inbound request.
func (c *Conn) Respond(id json.RawMessage, result any) error {
	return c.write(response{JSONRPC: "2.0", ID: id, Result: result})
}

// RespondError answers an inbound request with an error.
func (c *Conn) RespondError(id json.RawMessage, code int, msg string) error {
	return c.write(response{JSONRPC: "2.0", ID: id, Error: &Error{Code: code, Message: msg}})
}

func (c *Conn) forget(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func (c *Conn) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}

	b = append(b, '\n')

	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	if c.stdin == nil {
		return errors.New("jsonrpc: connection closed")
	}

	_, err = c.stdin.Write(b)

	return err
}

// Handle consumes one inbound record and dispatches it.
func (c *Conn) Handle(line []byte) {
	var m message
	if json.Unmarshal(line, &m) != nil {
		return
	}

	c.mu.Lock()
	h := c.h
	c.mu.Unlock()

	switch {
	case m.Method != "" && len(m.ID) == 0:
		if h != nil {
			h.Notification(m.Method, m.Params)
		}
	case m.Method != "":
		if h != nil {
			h.Request(m.ID, m.Method, m.Params)
		}
	default:
		c.resolve(m)
	}
}

// resolve delivers a response to the goroutine waiting in Request.
func (c *Conn) resolve(m message) {
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

func parseID(raw json.RawMessage) (int64, error) {
	return strconv.ParseInt(strings.Trim(string(raw), `"`), 10, 64)
}
