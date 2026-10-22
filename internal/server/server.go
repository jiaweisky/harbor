// Package server assembles the HTTP handler that exposes agents over A2A.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/push"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	"github.com/jiaweisky/harbor/internal/agent"
	"github.com/jiaweisky/harbor/internal/bridge"
	"github.com/jiaweisky/harbor/internal/config"
	"github.com/jiaweisky/harbor/internal/integration"
	"github.com/jiaweisky/harbor/internal/session"
	"github.com/jiaweisky/harbor/internal/store"
	"github.com/jiaweisky/harbor/internal/workspace"
)

// DiscoveryPath is the aggregate discovery endpoint. It returns every agent
// card as a JSON array, the shape CodeBuddy's remote-agents discovery expects
// (the A2A-standard well-known path returns a single card). It is always served:
// the cards are already public individually, so aggregating them adds no
// information; it exists so CodeBuddy works out of the box.
const DiscoveryPath = "/.well-known/agents.json"

// sessionStore returns a per-agent session store: a SQLite store under dir when
// persistence is enabled (so CLI session continuity survives a restart),
// otherwise an in-memory one.
func sessionStore(sc config.ServerConfig, agentID string) (bridge.SessionStore, error) {
	if sc.Store != config.StoreSQLite || sc.SessionStoreDir == "" {
		return session.New(), nil
	}
	return session.NewSQLiteStore(filepath.Join(sc.SessionStoreDir, agentID+".db"))
}

// containerSpec builds the runner's container sandbox from the configuration, or
// nil when no container is configured. The agent's workdir, the isolation root,
// and the CLI home are bind-mounted at their host paths so paths inside the
// container match the host.
func containerSpec(s config.SandboxConfig, workdir string) *agent.ContainerSpec {
	if s.Container == nil {
		return nil
	}
	var mounts []string
	for _, m := range []string{workdir, s.IsolationRoot, s.CLIHome} {
		if m != "" {
			mounts = append(mounts, m)
		}
	}
	return &agent.ContainerSpec{
		Runtime: s.Container.Runtime,
		Image:   s.Container.Image,
		Network: string(s.Container.Network),
		Mounts:  mounts,
	}
}

// NewHandler builds the HTTP handler that exposes every configured agent:
// a public agent card per agent, a JSON-RPC endpoint per agent, the standard
// first-agent card, and the aggregate discovery endpoint. The auth middleware,
// when enabled, protects only the JSON-RPC endpoints.
func NewHandler(ctx context.Context, cfg *config.Config) (http.Handler, error) {
	servers, err := New(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return servers.HTTP, nil
}

// Servers bundles the HTTP handler and the resources the caller must release.
type Servers struct {
	HTTP http.Handler

	closers []io.Closer
}

// Close releases resources held by the servers, such as SQLite stores.
func (s *Servers) Close() error { return closeAll(s.closers) }

// New builds the HTTP handler for the configured agents.
func New(ctx context.Context, cfg *config.Config) (*Servers, error) {
	auth, err := authMiddleware(cfg.Server.Auth)
	if err != nil {
		return nil, err
	}
	taskStore, err := newTaskStore(cfg.Server)
	if err != nil {
		return nil, err
	}
	var closers []io.Closer
	if c, ok := taskStore.(io.Closer); ok {
		closers = append(closers, c)
	}

	pushStore := push.NewInMemoryStore()
	pushSender := push.NewHTTPPushSender(&push.HTTPSenderConfig{
		Timeout:              10 * time.Second,
		AllowPrivateNetworks: cfg.Server.PushAllowPrivateNetworks,
	})
	inactivity := inactivityTimeout(cfg.Server)

	b := &agentBuilder{
		cfg:            cfg,
		auth:           auth,
		bearer:         cfg.Server.Auth.Mode == "bearer",
		rl:             newRateLimiter(cfg.Server.RateLimit, cfg.Server.Auth.Mode == "bearer"),
		taskLookup:     taskLookupFunc(taskStore),
		mux:            http.NewServeMux(),
		authReady:      make(map[string]bool, len(cfg.Agents)),
		handlerOptions: handlerOptions(pushStore, pushSender, taskStore, inactivity),
		keepalive:      inactivity / 2,
		closers:        closers,
	}
	for i := range cfg.Agents {
		b.addAgent(ctx, cfg.Agents[i])
	}
	if len(b.cards) == 0 {
		_ = closeAll(b.closers)
		return nil, fmt.Errorf("no agents available; check the agent config and that their binaries exist")
	}

	mux := b.mux
	mux.Handle("/.well-known/agent-card.json", a2asrv.NewStaticAgentCardHandler(b.cards[0]))
	mux.Handle(DiscoveryPath, bridge.NewCardsHandler(b.cards))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/readyz", readinessHandler(b.execs, b.authReady))
	mux.HandleFunc("/metrics", metricsHandler(b.execs))

	httpHandler := traceMiddleware(bridge.CORSMiddleware(cfg.Server.CORSAllowedOrigins, mux))
	return &Servers{HTTP: httpHandler, closers: b.closers}, nil
}

// closeAll closes every closer, returning the first error.
func closeAll(closers []io.Closer) error {
	var err error
	for _, c := range closers {
		if e := c.Close(); e != nil && err == nil {
			err = e
		}
	}
	return err
}

// newTaskStore returns the configured task store: a SQLite store when
// server.store is "sqlite" and a path is set, otherwise in-memory. The executor
// also reads it, for idempotent sends and durable interactions.
func newTaskStore(sc config.ServerConfig) (taskstore.Store, error) {
	if sc.Store != config.StoreSQLite || sc.StorePath == "" {
		return taskstore.NewInMemory(nil), nil
	}
	ts, err := store.NewSQLiteStore(sc.StorePath)
	if err != nil {
		return nil, fmt.Errorf("task store: %w", err)
	}
	return ts, nil
}

// taskLookupFunc adapts a task store to the executor's lookup.
func taskLookupFunc(ts taskstore.Store) func(context.Context, a2a.TaskID) (*a2a.Task, bool) {
	return func(ctx context.Context, id a2a.TaskID) (*a2a.Task, bool) {
		st, err := ts.Get(ctx, id)
		if err != nil || st == nil || st.Task == nil {
			return nil, false
		}
		return st.Task, true
	}
}

// handlerOptions returns the shared A2A request-handler options.
func handlerOptions(
	pushStore push.ConfigStore, pushSender push.Sender, ts taskstore.Store, inactivity time.Duration,
) []a2asrv.RequestHandlerOption {
	return []a2asrv.RequestHandlerOption{
		a2asrv.WithLogger(slog.Default()),
		a2asrv.WithAgentInactivityTimeout(inactivity),
		a2asrv.WithPushNotifications(pushStore, pushSender),
		a2asrv.WithCapabilityChecks(&a2a.AgentCapabilities{Streaming: true, PushNotifications: true}),
		a2asrv.WithTaskStore(ts),
	}
}

// inactivityTimeout returns the configured agent inactivity timeout, or the
// default when unset.
func inactivityTimeout(sc config.ServerConfig) time.Duration {
	if sc.AgentInactivityTimeout != nil {
		return *sc.AgentInactivityTimeout
	}
	return 10 * time.Minute
}

// agentBuilder assembles the per-agent runtime, card, routes, and optional gRPC
// server, accumulating them for the assembled Servers.
type agentBuilder struct {
	cfg            *config.Config
	auth           func(http.Handler) http.Handler
	bearer         bool
	rl             *rateLimiter
	taskLookup     func(context.Context, a2a.TaskID) (*a2a.Task, bool)
	keepalive      time.Duration
	mux            *http.ServeMux
	handlerOptions []a2asrv.RequestHandlerOption

	cards     []*a2a.AgentCard
	execs     []*bridge.CLIExecutor
	authReady map[string]bool
	closers   []io.Closer
}

// addAgent builds one agent's runtime and, on success, registers its card and
// routes. A build error or missing binary disables the agent with a warning
// rather than failing startup.
func (b *agentBuilder) addAgent(ctx context.Context, ac config.AgentConfig) {
	ln, info, err := integration.Build(ctx, ac, integration.Options{
		InheritEnv: b.cfg.Sandbox.InheritEnvEnabled(),
		CLIHome:    b.cfg.Sandbox.CLIHome,
		Container:  containerSpec(b.cfg.Sandbox, ac.Workdir),
	})
	if err != nil {
		slog.Warn("agent disabled", "id", ac.ID, "err", err)
		return
	}
	if !info.Enabled {
		slog.Warn("binary missing", "id", ac.ID, "err", info.Err)
		return
	}
	sessions, err := sessionStore(b.cfg.Server, ac.ID)
	if err != nil {
		slog.Warn("agent disabled", "id", ac.ID, "err", err)
		return
	}
	if c, ok := sessions.(io.Closer); ok {
		b.closers = append(b.closers, c)
	}
	resolver := workspace.NewResolverWithIsolation(ac.Workdir,
		b.cfg.Sandbox.Isolation, b.cfg.Sandbox.IsolationRoot)
	exec := bridge.NewCLIExecutor(ln, sessions, agent.NewLiveSessions(), resolver, ac.Limits)
	exec.SetNonImageFileArtifacts(b.cfg.Sandbox.NonImageFileArtifacts)
	exec.SetArtifactInlineMaxBytes(b.cfg.Sandbox.ArtifactInlineMaxBytes)
	exec.SetDiffInlineMaxBytes(b.cfg.Sandbox.DiffInlineMaxBytes)
	exec.SetNonGitChangeGlobs(b.cfg.Sandbox.NonGitChangeGlobs)
	exec.SetTaskLookup(b.taskLookup)
	// Enable the keepalive only when the agent also has a Harbor-side bound, so
	// that suppressing the server watchdog for a long silent step does not leave
	// a genuinely hung CLI running with no timeout to reap it.
	if ac.Limits.Timeout > 0 || ac.Limits.IdleTimeout > 0 {
		exec.SetKeepaliveInterval(b.keepalive)
	}
	handler := a2asrv.NewHandler(exec, b.handlerOptions...)

	path := AgentPath(ac.ID)
	card := bridge.BuildCard(ln, info, EndpointURL(b.cfg.Server.BaseURL, ac.ID), b.bearer)
	limit := b.cfg.Server.MaxRequestBytes
	b.mux.Handle(path, b.auth(b.rl.middleware(ac.ID, maxBytes(limit, a2asrv.NewJSONRPCHandler(handler)))))
	// The HTTP+JSON (A2A REST) binding, mounted under the agent's base path.
	rest := http.StripPrefix(path+bridge.RESTMount, a2asrv.NewRESTHandler(handler))
	b.mux.Handle(path+bridge.RESTPath, b.auth(b.rl.middleware(ac.ID, maxBytes(limit, rest))))
	b.mux.Handle(path+".well-known/agent-card.json", a2asrv.NewStaticAgentCardHandler(card))

	b.cards = append(b.cards, card)
	b.execs = append(b.execs, exec)
	b.authReady[ac.ID] = ln.AuthReady(ctx)
	slog.Info("agent ready", "id", ac.ID, "integration", ac.Integration, "endpoint", path)
}

// traceMiddleware extracts an incoming W3C trace-context so the task span joins
// the caller's distributed trace.
func traceMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// agentReadiness is one agent's readiness, reported on /readyz.
type agentReadiness struct {
	Breaker string `json:"breaker"`
	Auth    bool   `json:"auth"`
	Ready   bool   `json:"ready"`
}

// readinessHandler reports service readiness as JSON. An agent is ready when
// its circuit breaker is closed; the CLI's authentication is reported separately
// as information and never affects readiness (the CLI decides if it can run).
func readinessHandler(execs []*bridge.CLIExecutor, authReady map[string]bool) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		agents := make(map[string]agentReadiness, len(execs))
		ready := false
		for _, e := range execs {
			name := e.Name()
			rdy := e.Ready()
			agents[name] = agentReadiness{Breaker: e.BreakerState(), Auth: authReady[name], Ready: rdy}
			if rdy {
				ready = true
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if !ready {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ready": ready, "agents": agents})
	}
}

// maxBytes bounds an incoming request body to n bytes; 0 disables the bound.
// Oversized bodies fail with 413 before being buffered.
func maxBytes(n int64, next http.Handler) http.Handler {
	if n <= 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, n)
		next.ServeHTTP(w, r)
	})
}

// metricsHandler exposes per-agent task counters and breaker state in the
// Prometheus text exposition format.
func metricsHandler(execs []*bridge.CLIExecutor) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		snaps := make([]bridge.MetricsSnapshot, 0, len(execs))
		for _, e := range execs {
			snaps = append(snaps, e.Snapshot())
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		bridge.WritePrometheus(w, snaps)
	}
}

// authMiddleware returns the JSON-RPC authentication middleware for the
// configured mode. Unknown modes are rejected (fail closed) rather than
// silently serving without auth.
func authMiddleware(cfg config.AuthConfig) (func(http.Handler) http.Handler, error) {
	switch cfg.Mode {
	case "", "none":
		return func(h http.Handler) http.Handler { return h }, nil
	case "bearer":
		return func(h http.Handler) http.Handler { return bridge.BearerMiddleware(cfg.Tokens, h) }, nil
	default:
		return nil, fmt.Errorf("unsupported auth mode %q", cfg.Mode)
	}
}

// AgentPath is the JSON-RPC mount path for an agent. It ends in a slash so the
// card and RPC paths stay consistent (clients POST to this exact path).
func AgentPath(id string) string {
	return "/agents/" + id + "/"
}

// EndpointURL is the absolute URL advertised in an agent's card.
func EndpointURL(baseURL, id string) string {
	return strings.TrimRight(baseURL, "/") + AgentPath(id)
}
