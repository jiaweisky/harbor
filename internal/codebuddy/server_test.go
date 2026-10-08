package codebuddy_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jiaweisky/harbor/internal/agent"
	"github.com/jiaweisky/harbor/internal/codebuddy"
)

// runRequest is the subset of the gateway message Harbor sends.
type runRequest struct {
	Source struct {
		Conversation struct {
			ID string `json:"id"`
		} `json:"conversation"`
	} `json:"source"`
	Payload struct {
		Text string `json:"text"`
	} `json:"payload"`
}

// fakeServer records run requests and streams a fixed reply.
type fakeServer struct {
	mu   sync.Mutex
	reqs []runRequest
	head bool // every request carried X-CodeBuddy-Request
}

func (f *fakeServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/runs", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Header.Get("X-CodeBuddy-Request") == "1" {
			f.head = true
		}
		var req runRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.reqs = append(f.reqs, req)
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"runId":"run-1","status":"accepted"}}`))
	})
	mux.HandleFunc("/api/v1/runs/run-1/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		_, _ = w.Write([]byte("event: message\n" +
			`data: {"status":"completed","content":{"markdown":"pong"}}` + "\n\n"))
		_, _ = w.Write([]byte("event: done\ndata: {}\n\n"))
	})
	mux.HandleFunc("/api/v1/runs/run-1/cancel", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

func TestServerLauncherTurn(t *testing.T) {
	f := &fakeServer{}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	ln := codebuddy.NewServerLauncher(context.Background(), codebuddy.Config{Name: "cb", BaseURL: srv.URL})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sess, err := ln.Start(ctx, &agent.Request{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	var sessionID, text string
	var done bool
	for ev := range sess.Events() {
		switch ev.Kind {
		case agent.EventSession:
			sessionID = ev.SessionID
		case agent.EventTextDelta:
			text += ev.Text
		case agent.EventDone:
			done = true
		case agent.EventError:
			t.Fatalf("event error: %v", ev.Err)
		}
	}
	if !done {
		t.Fatal("no EventDone")
	}
	if text != "pong" {
		t.Fatalf("text = %q, want pong", text)
	}
	if sessionID == "" {
		t.Fatal("no session id reported for a new conversation")
	}
	if !f.head {
		t.Error("request missing X-CodeBuddy-Request header")
	}

	// A second turn with the reported session id must reuse it as the
	// conversation id so the CLI continues the same conversation.
	sess2, err := ln.Start(ctx, &agent.Request{Prompt: "again", SessionID: sessionID})
	if err != nil {
		t.Fatalf("Start 2: %v", err)
	}
	for range sess2.Events() {
	}
	if len(f.reqs) != 2 {
		t.Fatalf("got %d run requests, want 2", len(f.reqs))
	}
	if got := f.reqs[1].Source.Conversation.ID; got != sessionID {
		t.Errorf("second run conversation = %q, want %q", got, sessionID)
	}
	if f.reqs[1].Payload.Text != "again" {
		t.Errorf("second run prompt = %q, want again", f.reqs[1].Payload.Text)
	}
}

func TestServerLauncherStreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/stream"):
			w.Header().Set("content-type", "text/event-stream")
			_, _ = w.Write([]byte("event: message\ndata: {\"status\":\"failed\"}\n\n"))
		default:
			_, _ = w.Write([]byte(`{"data":{"runId":"run-1","status":"accepted"}}`))
		}
	}))
	defer srv.Close()

	ln := codebuddy.NewServerLauncher(context.Background(), codebuddy.Config{Name: "cb", BaseURL: srv.URL})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sess, err := ln.Start(ctx, &agent.Request{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	var failed bool
	for ev := range sess.Events() {
		if ev.Kind == agent.EventError {
			failed = true
		}
	}
	if !failed {
		t.Fatal("a failed run must emit an error event")
	}
}
