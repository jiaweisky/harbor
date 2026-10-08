package acp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jiaweisky/harbor/internal/acp"
	"github.com/jiaweisky/harbor/internal/agent"
)

// fakeACP is a minimal ACP-over-HTTP server: connect returns a connection id,
// each POST streams that command's reply as SSE, and GET is an idle subscription.
func fakeACPServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/acp/connect", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, `{"connectionId":"c1"}`)
	})
	mux.HandleFunc("/api/v1/acp", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("content-type", "text/event-stream")
			_, _ = io.WriteString(w, ":ok\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case http.MethodPost:
			var m struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			_ = json.NewDecoder(r.Body).Decode(&m)
			w.Header().Set("content-type", "text/event-stream")
			f := w.(http.Flusher)
			reply := func(data string) {
				_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", data)
				f.Flush()
			}
			switch m.Method {
			case "initialize":
				reply(`{"jsonrpc":"2.0","id":` + string(m.ID) + `,"result":{}}`)
			case "session/new":
				reply(`{"jsonrpc":"2.0","id":` + string(m.ID) + `,"result":{"sessionId":"s1"}}`)
			case "session/prompt":
				reply(`{"jsonrpc":"2.0","method":"session/update","params":` +
					`{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk",` +
					`"content":{"type":"text","text":"pong"}}}}`)
				reply(`{"jsonrpc":"2.0","id":` + string(m.ID) + `,"result":{"stopReason":"end_turn"}}`)
			}
		}
	})
	return httptest.NewServer(mux)
}

func TestHTTPLauncherTurn(t *testing.T) {
	srv := fakeACPServer(t)
	defer srv.Close()

	ln := acp.NewHTTPLauncher(acp.HTTPConfig{Name: "fake", BaseURL: srv.URL})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sess, err := ln.Start(ctx, &agent.Request{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	var sessionID, text string
	for ev := range sess.Events() {
		switch ev.Kind {
		case agent.EventSession:
			sessionID = ev.SessionID
		case agent.EventTextDelta:
			text += ev.Text
		case agent.EventError:
			t.Fatalf("event error: %v", ev.Err)
		}
	}
	if sessionID != "s1" {
		t.Errorf("session id = %q, want s1", sessionID)
	}
	if text != "pong" {
		t.Errorf("text = %q, want pong", text)
	}
}

// TestHTTPLauncherCapabilities mirrors the stdio ACP capability set.
func TestHTTPLauncherCapabilities(t *testing.T) {
	ln := acp.NewHTTPLauncher(acp.HTTPConfig{Name: "fake", BaseURL: "http://127.0.0.1:1"})
	caps := ln.Capabilities()
	if !caps.InputRequiredResume || caps.Streaming != agent.StreamToken || !caps.ToolTrace {
		t.Fatalf("unexpected capabilities: %+v", caps)
	}
}
