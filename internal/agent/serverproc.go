package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ServerEndpoint resolves and caches a CLI server's base URL: the configured URL
// when set, otherwise the result of spawning the server on first use.
type ServerEndpoint struct {
	mu      sync.Mutex
	baseURL string
}

// URL returns the server base URL, spawning via spawn on first use when
// configured is empty. spawn is invoked at most once and its result is cached.
func (e *ServerEndpoint) URL(
	ctx context.Context, configured string, spawn func(context.Context) (string, error),
) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.baseURL != "" {
		return e.baseURL, nil
	}
	if configured != "" {
		e.baseURL = strings.TrimRight(configured, "/")
		return e.baseURL, nil
	}
	base, err := spawn(ctx)
	if err != nil {
		return "", err
	}
	e.baseURL = base
	return base, nil
}

// SpawnServer starts a long-running server process via runner and waits until it
// reports its listening URL on stdout (see ListenURL). A failed or timed-out
// wait kills the process so a retry does not leak it. lifecycle bounds the
// process; ctx bounds only this wait. taskID identifies the process in runner.
func SpawnServer(
	ctx, lifecycle context.Context, runner *Runner, taskID string, inv *Invocation, timeout time.Duration,
) (string, error) {
	events, err := runner.Start(lifecycle, taskID, inv, NewLineParser())
	if err != nil {
		return "", err
	}
	urlCh := make(chan string, 1)
	go func() {
		// Drain the server's output for its lifetime, capturing the first URL.
		sent := false
		for ev := range events {
			if sent || ev.Kind != EventTextDelta {
				continue
			}
			if url, ok := ListenURL(ev.Text); ok {
				sent = true
				urlCh <- url
			}
		}
		if !sent {
			close(urlCh)
		}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case url, ok := <-urlCh:
		if !ok {
			return "", fmt.Errorf("%s exited before listening", inv.Bin)
		}
		return url, nil
	case <-timer.C:
		runner.Kill(taskID)
		return "", fmt.Errorf("%s did not listen within %s", inv.Bin, timeout)
	case <-ctx.Done():
		runner.Kill(taskID)
		return "", ctx.Err()
	}
}
