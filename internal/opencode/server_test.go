package opencode_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jiaweisky/harbor/internal/adapters"
	"github.com/jiaweisky/harbor/internal/agent"
	"github.com/jiaweisky/harbor/internal/opencode"
)

// fakeServer is a minimal stand-in for `opencode serve`.
func fakeServer(t *testing.T) (*httptest.Server, *int32, *int32) {
	t.Helper()
	var creates, prompts int32
	mux := http.NewServeMux()
	mux.HandleFunc("/session", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		atomic.AddInt32(&creates, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "ses_test"})
	})
	mux.HandleFunc("/session/ses_test/message", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&prompts, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"parts": []map[string]any{{"type": "step-start"}, {"type": "text", "text": "hello"}},
		})
	})
	mux.HandleFunc("/session/ses_test/abort", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	return srv, &creates, &prompts
}

func drain(sess agent.Session) []agent.Event {
	var got []agent.Event
	for ev := range sess.Events() {
		got = append(got, ev)
	}
	return got
}

func TestServerLauncherTurn(t *testing.T) {
	srv, creates, prompts := fakeServer(t)
	defer srv.Close()

	ln := opencode.NewServerLauncher(context.Background(), opencode.Config{
		Name: "oc", Binary: "opencode", BaseURL: srv.URL, Auth: adapters.Unverified(),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sess, err := ln.Start(ctx, &agent.Request{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	evs := drain(sess)
	if len(evs) != 3 ||
		evs[0].Kind != agent.EventSession || evs[0].SessionID != "ses_test" ||
		evs[1].Kind != agent.EventTextDelta || evs[1].Text != "hello" ||
		evs[2].Kind != agent.EventDone {
		t.Fatalf("events = %+v", evs)
	}
	if *creates != 1 || *prompts != 1 {
		t.Fatalf("creates=%d prompts=%d", *creates, *prompts)
	}

	// Reusing the session must not create another one.
	sess2, err := ln.Start(ctx, &agent.Request{Prompt: "again", SessionID: "ses_test"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	evs = drain(sess2)
	if len(evs) != 2 || evs[0].Kind != agent.EventTextDelta || evs[1].Kind != agent.EventDone {
		t.Fatalf("resume events = %+v", evs)
	}
	if *creates != 1 || *prompts != 2 {
		t.Fatalf("creates=%d prompts=%d, want 1 and 2", *creates, *prompts)
	}
}

func TestServerLauncherStreamsDeltas(t *testing.T) {
	sent := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/session", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "ses_test"})
	})
	mux.HandleFunc("/event", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		const frame = "data: {\"type\":\"message.part.delta\",\"properties\":" +
			"{\"sessionID\":\"ses_test\",\"field\":\"text\",\"delta\":%q}}\n\n"
		for _, d := range []string{"he", "llo"} {
			fmt.Fprintf(w, frame, d)
			if flusher != nil {
				flusher.Flush()
			}
		}
		close(sent)
		<-r.Context().Done()
	})
	mux.HandleFunc("/session/ses_test/message", func(w http.ResponseWriter, _ *http.Request) {
		<-sent
		time.Sleep(150 * time.Millisecond) // let the client read the deltas first
		_ = json.NewEncoder(w).Encode(map[string]any{
			"parts": []map[string]any{{"type": "text", "text": "hello"}},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ln := opencode.NewServerLauncher(context.Background(), opencode.Config{
		Name: "oc", Binary: "opencode", BaseURL: srv.URL, Auth: adapters.Unverified(),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sess, err := ln.Start(ctx, &agent.Request{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	evs := drain(sess)
	if len(evs) != 4 ||
		evs[0].Kind != agent.EventSession ||
		evs[1].Text != "he" || evs[2].Text != "llo" ||
		evs[3].Kind != agent.EventDone {
		t.Fatalf("events = %+v, want streamed deltas", evs)
	}
}

func TestServerLauncherError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	ln := opencode.NewServerLauncher(context.Background(), opencode.Config{
		Name: "oc", Binary: "opencode", BaseURL: srv.URL, Auth: adapters.Unverified(),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sess, err := ln.Start(ctx, &agent.Request{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	evs := drain(sess)
	if len(evs) != 1 || evs[0].Kind != agent.EventError {
		t.Fatalf("expected one error event, got %+v", evs)
	}
}
