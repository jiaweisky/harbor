package server_test

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2acrypto"

	"github.com/jiaweisky/harbor/internal/config"
	"github.com/jiaweisky/harbor/internal/fakeclitest"
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

// TestVersionMismatch verifies both bindings reject a request that does not
// target A2A 1.0 before it reaches the executor.
func TestVersionMismatch(t *testing.T) {
	srv := newTestServer(t, testConfig(t))
	defer srv.Close()

	msg := &a2a.SendMessageRequest{
		Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi")),
	}
	restBody, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	rpcBody, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "SendMessage",
		"params":  msg,
	})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		path   string
		body   []byte
		verify func(*testing.T, *http.Response)
	}{
		{"jsonrpc", "/agents/cb/", rpcBody, verifyJSONRPCVersionError},
		{"rest", "/agents/cb/rest/message:send", restBody, verifyRESTVersionError},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, srv.URL+tc.path, bytes.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(a2a.SvcParamVersion, "0.3")

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			tc.verify(t, resp)
		})
	}
}

func verifyJSONRPCVersionError(t *testing.T, resp *http.Response) {
	t.Helper()
	var body struct {
		Error struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Code != -32009 {
		t.Fatalf("jsonrpc error code = %d, want -32009", body.Error.Code)
	}
}

func verifyRESTVersionError(t *testing.T, resp *http.Response) {
	t.Helper()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "VERSION_NOT_SUPPORTED") {
		t.Fatalf("body = %s, want VERSION_NOT_SUPPORTED", body)
	}
}

// TestVersionQueryParameter verifies A2A-Version may be supplied as a query
// parameter instead of a header.
func TestVersionQueryParameter(t *testing.T) {
	srv := newTestServer(t, testConfig(t))
	defer srv.Close()

	msg := &a2a.SendMessageRequest{
		Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi")),
	}
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "SendMessage",
		"params":  msg,
	})
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(
		srv.URL+"/agents/cb/?A2A-Version=1.0", "application/json", bytes.NewReader(body),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var out struct {
		Error any `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Error != nil {
		t.Fatalf("error = %v, want a successful response", out.Error)
	}
}

// TestCardSigning verifies a configured signing key produces a verifiable JWS
// signature on the served card.
func TestCardSigning(t *testing.T) {
	keyPath, pub := writeSigningKey(t)
	cfg := testConfig(t)
	cfg.Agents[0].Card.Signing = &config.CardSigningConfig{
		KeyFile: keyPath, KeyID: "test-kid", Algorithm: "ES256",
	}

	srv := newTestServer(t, cfg)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/agents/cb/.well-known/agent-card.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var card a2a.AgentCard
	if err := json.Unmarshal(raw, &card); err != nil {
		t.Fatalf("decode card: %v", err)
	}
	if len(card.Signatures) != 1 {
		t.Fatalf("signatures = %d, want 1", len(card.Signatures))
	}

	verifier := a2acrypto.NewVerifier(a2acrypto.VerifierConfig{
		KeyResolver: a2acrypto.KeyResolverFunc(func(_ context.Context, kid string) (crypto.PublicKey, error) {
			if kid != "test-kid" {
				t.Fatalf("kid = %q, want test-kid", kid)
			}
			return pub, nil
		}),
	})
	if err := verifier.Verify(context.Background(), raw, &card.Signatures[0]); err != nil {
		t.Fatalf("verify signature: %v", err)
	}
}

func writeSigningKey(t *testing.T) (string, *ecdsa.PublicKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "signing-key.pem")
	raw := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, &key.PublicKey
}

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	script := fakeclitest.Path(t, "fake-codebuddy")
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
	h, err := server.NewHandler(context.Background(), cfg, nil)
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
	bin := fakeclitest.Path(t, "codex")
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
	if _, err := server.NewHandler(context.Background(), cfg, nil); err == nil {
		t.Fatal("expected NewHandler to reject an unsupported auth mode")
	}
}

func TestNoAgentsFailsClosed(t *testing.T) {
	cfg := &config.Config{Version: 1, Server: config.ServerConfig{Listen: "127.0.0.1:0"}}
	if _, err := server.NewHandler(context.Background(), cfg, nil); err == nil {
		t.Fatal("expected NewHandler to fail when no agents are configured")
	}
}

func TestAllBinariesMissingFailsClosed(t *testing.T) {
	cfg := testConfig(t)
	cfg.Agents[0].Binary = "/nonexistent/harbor-no-such-binary"
	if _, err := server.NewHandler(context.Background(), cfg, nil); err == nil {
		t.Fatal("expected NewHandler to fail when no agent binary is usable")
	}
}
