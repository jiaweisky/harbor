package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/jiaweisky/harbor/internal/config"
	"github.com/jiaweisky/harbor/internal/server"
)

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
