package server_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"

	"github.com/jiaweisky/harbor/internal/config"
	"github.com/jiaweisky/harbor/internal/server"
)

// TestRESTBinding verifies the HTTP+JSON (A2A REST) binding drives a task.
func TestRESTBinding(t *testing.T) {
	srv := newTestServer(t, testConfig(t))
	defer srv.Close()

	ctx := context.Background()
	client, err := a2aclient.NewFromEndpoints(ctx, []*a2a.AgentInterface{{
		URL:             srv.URL + "/agents/cb/rest",
		ProtocolBinding: a2a.TransportProtocolHTTPJSON,
		ProtocolVersion: a2a.Version,
	}}, a2aclient.WithRESTTransport(nil))
	if err != nil {
		t.Fatalf("NewFromEndpoints: %v", err)
	}
	res, err := client.SendMessage(ctx, &a2a.SendMessageRequest{
		Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi")),
	})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	task, ok := res.(*a2a.Task)
	if !ok || task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("result = %+v, want a completed task", res)
	}
}

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	script, err := filepath.Abs(filepath.Join("..", "..", "testdata", "fake-cli", "fake-codebuddy.sh"))
	if err != nil {
		t.Fatal(err)
	}
	return &config.Config{
		Version: 1,
		Server: config.ServerConfig{
			Listen:  "127.0.0.1:0",
			BaseURL: "http://example.test",
		},
		Agents: []config.AgentConfig{{
			ID:          "cb",
			Binary:      script,
			Integration: "stream-json",
			Args:        []string{"-p"},
		}},
	}
}

func newTestServer(t *testing.T, cfg *config.Config) *httptest.Server {
	t.Helper()
	h, err := server.NewHandler(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return httptest.NewServer(h)
}

// TestArtifactConfigWiring verifies the sandbox artifact settings reach the
// executor: a small inline limit turns a changed file into an omitted artifact.
func TestArtifactConfigWiring(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "testdata", "fake-cli", "fake-mutate.sh"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Version: 1,
		Server:  config.ServerConfig{Listen: "127.0.0.1:0", BaseURL: "http://example.test"},
		Sandbox: config.SandboxConfig{
			NonImageFileArtifacts:  true,
			ArtifactInlineMaxBytes: 4,
			NonGitChangeGlobs:      []string{"*.txt"},
		},
		Agents: []config.AgentConfig{{
			ID: "cb", Binary: script, Integration: "exec",
			Args: []string{"-p", "{{.Prompt}}"}, Workdir: dir,
		}},
	}
	srv := newTestServer(t, cfg)
	defer srv.Close()

	ctx := context.Background()
	client, err := a2aclient.NewFromEndpoints(ctx, []*a2a.AgentInterface{{
		URL:             srv.URL + "/agents/cb/rest",
		ProtocolBinding: a2a.TransportProtocolHTTPJSON,
		ProtocolVersion: a2a.Version,
	}}, a2aclient.WithRESTTransport(nil))
	if err != nil {
		t.Fatalf("NewFromEndpoints: %v", err)
	}
	res, err := client.SendMessage(ctx, &a2a.SendMessageRequest{
		Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi")),
	})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	task := res.(*a2a.Task)
	for _, a := range task.Artifacts {
		if a.Name != "file" {
			continue
		}
		if a.Metadata["omitted"] == true {
			return
		}
	}
	t.Fatalf("no omitted file artifact; artifacts = %+v", task.Artifacts)
}

func TestRoutes(t *testing.T) {
	srv := newTestServer(t, testConfig(t))
	defer srv.Close()

	cases := []struct {
		name string
		path string
	}{
		{"per-agent card", "/agents/cb/.well-known/agent-card.json"},
		{"standard header card", "/.well-known/agent-card.json"},
		{"aggregate discovery", "/.well-known/agents.json"},
		{"health", "/healthz"},
		{"ready", "/readyz"},
	}
	for _, c := range cases {
		resp, err := http.Get(srv.URL + c.path)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s (%s): status %d", c.name, c.path, resp.StatusCode)
		}
	}
}

func TestReadyzReportsAgents(t *testing.T) {
	srv := newTestServer(t, testConfig(t))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		Ready  bool `json:"ready"`
		Agents map[string]struct {
			Breaker string `json:"breaker"`
			Auth    bool   `json:"auth"`
			Ready   bool   `json:"ready"`
		} `json:"agents"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	cb := body.Agents["cb"]
	if !body.Ready || cb.Breaker != "closed" || !cb.Auth || !cb.Ready {
		t.Fatalf("body = %+v, want ready/closed/auth for cb", body)
	}
}

// TestUnauthenticatedCLIStillServed verifies Harbor does not gate an agent on
// its own credential guess: a CLI whose status reports "not logged in" still
// starts and is served (the CLI decides if it can run). Authentication is
// reported as information and does not affect readiness.
func TestUnauthenticatedCLIStillServed(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "codex")
	script := "#!/bin/sh\nif [ \"$1\" = login ] && [ \"$2\" = status ]; then echo 'Not logged in'; exit 1; fi\necho ok\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Version: 1,
		Server:  config.ServerConfig{Listen: "127.0.0.1:0", BaseURL: "http://example.test"},
		Agents:  []config.AgentConfig{{ID: "codex", Binary: bin}},
	}
	srv := newTestServer(t, cfg)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Ready  bool `json:"ready"`
		Agents map[string]struct {
			Auth  bool `json:"auth"`
			Ready bool `json:"ready"`
		} `json:"agents"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	codex := body.Agents["codex"]
	if !body.Ready || !codex.Ready {
		t.Fatalf("body = %+v, want ready", body)
	}
	if codex.Auth {
		t.Fatalf("body = %+v, want auth=false (informational only)", body)
	}
}

func TestMetricsExposition(t *testing.T) {
	srv := newTestServer(t, testConfig(t))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	for _, want := range []string{
		`harbor_tasks_total{agent="cb",outcome="completed"}`,
		`harbor_breaker_state{agent="cb"} 0`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("metrics missing %q:\n%s", want, text)
		}
	}
}

func TestAggregateDiscoveryIsArray(t *testing.T) {
	srv := newTestServer(t, testConfig(t))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/.well-known/agents.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var cards []json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&cards); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(cards) != 1 {
		t.Fatalf("cards = %d, want 1", len(cards))
	}
}

func TestUnsupportedAuthModeFailsClosed(t *testing.T) {
	cfg := testConfig(t)
	cfg.Server.Auth.Mode = "oauth2"
	if _, err := server.NewHandler(context.Background(), cfg); err == nil {
		t.Fatal("expected NewHandler to reject an unsupported auth mode")
	}
}

func TestNoAgentsFailsClosed(t *testing.T) {
	cfg := &config.Config{Version: 1, Server: config.ServerConfig{Listen: "127.0.0.1:0"}}
	if _, err := server.NewHandler(context.Background(), cfg); err == nil {
		t.Fatal("expected NewHandler to fail when no agents are configured")
	}
}

func TestAllBinariesMissingFailsClosed(t *testing.T) {
	cfg := testConfig(t)
	cfg.Agents[0].Binary = "/nonexistent/harbor-no-such-binary"
	if _, err := server.NewHandler(context.Background(), cfg); err == nil {
		t.Fatal("expected NewHandler to fail when no agent binary is usable")
	}
}
