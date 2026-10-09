// Package server assembles the HTTP handler that exposes agents over A2A.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2agrpc/v1"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/push"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

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
		Network: s.Container.Network,
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

// Servers bundles the HTTP handler and any configured gRPC servers. The gRPC
// servers are built but not yet serving; the caller listens and serves them.
type Servers struct {
	HTTP http.Handler
	GRPC []*GRPCServer

	closers []io.Closer
}

// Close releases resources held by the servers, such as SQLite stores.
func (s *Servers) Close() error { return closeAll(s.closers) }

// GRPCServer is one agent's gRPC server (A2A gRPC binding) on its address.
type GRPCServer struct {
	Addr   string
	Server *grpc.Server
}

// New builds the HTTP handler and the gRPC servers for the configured agents.
// Each agent with a grpc_listen address gets its own gRPC server.
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

	b := &agentBuilder{
		cfg:            cfg,
		auth:           auth,
		bearer:         cfg.Server.Auth.Mode == "bearer",
		rl:             newRateLimiter(cfg.Server.RateLimit, cfg.Server.Auth.Mode == "bearer"),
		taskLookup:     taskLookupFunc(taskStore),
		mux:            http.NewServeMux(),
		authReady:      make(map[string]bool, len(cfg.Agents)),
		handlerOptions: handlerOptions(pushStore, pushSender, taskStore),
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
	return &Servers{HTTP: httpHandler, GRPC: b.grpc, closers: b.closers}, nil
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
	pushStore push.ConfigStore, pushSender push.Sender, ts taskstore.Store,
) []a2asrv.RequestHandlerOption {
	return []a2asrv.RequestHandlerOption{
		a2asrv.WithLogger(slog.Default()),
		a2asrv.WithAgentInactivityTimeout(10 * time.Minute),
		a2asrv.WithPushNotifications(pushStore, pushSender),
		a2asrv.WithCapabilityChecks(&a2a.AgentCapabilities{Streaming: true, PushNotifications: true}),
		a2asrv.WithTaskStore(ts),
	}
}

// agentBuilder assembles the per-agent runtime, card, routes, and optional gRPC
// server, accumulating them for the assembled Servers.
type agentBuilder struct {
	cfg            *config.Config
	auth           func(http.Handler) http.Handler
	bearer         bool
	rl             *rateLimiter
	taskLookup     func(context.Context, a2a.TaskID) (*a2a.Task, bool)
	mux            *http.ServeMux
	handlerOptions []a2asrv.RequestHandlerOption

	cards     []*a2a.AgentCard
	execs     []*bridge.CLIExecutor
	authReady map[string]bool
	grpc      []*GRPCServer
	closers   []io.Closer
}

// addAgent builds one agent's runtime and, on success, registers its card,
// routes, and gRPC server. A build error or missing binary disables the agent
// with a warning rather than failing startup.
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
	exec.SetFileArtifacts(b.cfg.Sandbox.FileArtifacts)
	exec.SetTaskLookup(b.taskLookup)
	handler := a2asrv.NewHandler(exec, b.handlerOptions...)

	path := AgentPath(ac.ID)
	card := bridge.BuildCard(ln, info, EndpointURL(b.cfg.Server.BaseURL, ac.ID), b.bearer)
	if ac.GRPCListen != "" {
		card.SupportedInterfaces = append(card.SupportedInterfaces, &a2a.AgentInterface{
			URL: ac.GRPCListen, ProtocolBinding: a2a.TransportProtocolGRPC, ProtocolVersion: a2a.Version,
		})
		b.grpc = append(b.grpc, newGRPCServer(handler, ac, b.cfg.Server.Auth, b.rl))
	}
	b.mux.Handle(path, b.auth(b.rl.middleware(ac.ID, a2asrv.NewJSONRPCHandler(handler))))
	// The HTTP+JSON (A2A REST) binding, mounted under the agent's base path.
	rest := http.StripPrefix(path+bridge.RESTMount, a2asrv.NewRESTHandler(handler))
	b.mux.Handle(path+bridge.RESTPath, b.auth(b.rl.middleware(ac.ID, rest)))
	b.mux.Handle(path+".well-known/agent-card.json", a2asrv.NewStaticAgentCardHandler(card))

	b.cards = append(b.cards, card)
	b.execs = append(b.execs, exec)
	b.authReady[ac.ID] = ln.AuthReady(ctx)
	slog.Info("agent ready", "id", ac.ID, "integration", ac.Integration, "endpoint", path)
}

// newGRPCServer builds one agent's gRPC server.
func newGRPCServer(
	handler a2asrv.RequestHandler, ac config.AgentConfig, auth config.AuthConfig, rl *rateLimiter,
) *GRPCServer {
	gs := grpc.NewServer(grpcServerOptions(auth, rl, ac.ID)...)
	a2agrpc.NewHandler(handler).RegisterWith(gs)
	slog.Info("agent grpc ready", "id", ac.ID, "addr", ac.GRPCListen)
	return &GRPCServer{Addr: ac.GRPCListen, Server: gs}
}

// traceMiddleware extracts an incoming W3C trace-context so the task span joins
// the caller's distributed trace.
func traceMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// grpcServerOptions returns server options that propagate trace context and, when
// configured, enforce bearer auth on the gRPC endpoints.
func grpcServerOptions(auth config.AuthConfig, rl *rateLimiter, agentID string) []grpc.ServerOption {
	unary := []grpc.UnaryServerInterceptor{traceUnaryInterceptor}
	stream := []grpc.StreamServerInterceptor{traceStreamInterceptor}
	// Authenticate before rate limiting so the limiter only sees validated
	// callers (mirroring the HTTP order).
	if auth.Mode == "bearer" {
		unary = append(unary, bearerUnaryInterceptor(auth.Tokens))
		stream = append(stream, bearerStreamInterceptor(auth.Tokens))
	}
	if rl != nil {
		unary = append(unary, rl.unaryInterceptor(agentID))
		stream = append(stream, rl.streamInterceptor(agentID))
	}
	return []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(unary...),
		grpc.ChainStreamInterceptor(stream...),
	}
}

func (l *rateLimiter) unaryInterceptor(agentID string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if !l.allow(agentID + "\x00" + l.grpcCallerKey(ctx)) {
			return nil, status.Error(codes.ResourceExhausted, "rate limit exceeded")
		}
		return handler(ctx, req)
	}
}

func (l *rateLimiter) streamInterceptor(agentID string) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if !l.allow(agentID + "\x00" + l.grpcCallerKey(ss.Context())) {
			return status.Error(codes.ResourceExhausted, "rate limit exceeded")
		}
		return handler(srv, ss)
	}
}

// grpcCallerKey identifies a gRPC caller. As with the HTTP limiter, the bearer
// token is only used when authentication validated it ahead of the limiter;
// otherwise the peer address identifies the caller.
func (l *rateLimiter) grpcCallerKey(ctx context.Context) string {
	if l.authEnabled {
		if md, ok := metadata.FromIncomingContext(ctx); ok {
			if v := md.Get("authorization"); len(v) > 0 {
				if tok, ok := bridge.BearerToken(v[0]); ok {
					return tokenKey(tok)
				}
			}
		}
	}
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		return p.Addr.String()
	}
	return "unknown"
}

func traceUnaryInterceptor(
	ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler,
) (any, error) {
	return handler(extractGRPCTrace(ctx), req)
}

func traceStreamInterceptor(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	return handler(srv, tracedStream{ServerStream: ss, ctx: extractGRPCTrace(ss.Context())})
}

// tracedStream overrides the stream context with the extracted trace context.
type tracedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s tracedStream) Context() context.Context { return s.ctx }

// extractGRPCTrace extracts the W3C trace-context from incoming gRPC metadata.
func extractGRPCTrace(ctx context.Context) context.Context {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ctx
	}
	return otel.GetTextMapPropagator().Extract(ctx, mdCarrier(md))
}

// mdCarrier adapts gRPC metadata to a propagation.TextMapCarrier.
type mdCarrier metadata.MD

func (c mdCarrier) Get(key string) string {
	if v := metadata.MD(c).Get(key); len(v) > 0 {
		return v[0]
	}
	return ""
}

func (c mdCarrier) Set(key, value string) { metadata.MD(c).Set(key, value) }

func (c mdCarrier) Keys() []string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	return keys
}

func bearerUnaryInterceptor(tokens []string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if !grpcAuthorized(ctx, tokens) {
			return nil, status.Error(codes.Unauthenticated, "missing or invalid bearer token")
		}
		return handler(ctx, req)
	}
}

func bearerStreamInterceptor(tokens []string) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if !grpcAuthorized(ss.Context(), tokens) {
			return status.Error(codes.Unauthenticated, "missing or invalid bearer token")
		}
		return handler(srv, ss)
	}
}

// grpcAuthorized reports whether the incoming gRPC metadata carries a valid
// bearer token.
func grpcAuthorized(ctx context.Context, tokens []string) bool {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return false
	}
	for _, v := range md.Get("authorization") {
		tok, ok := bridge.BearerToken(v)
		if !ok {
			continue
		}
		for _, t := range tokens {
			if subtle.ConstantTimeCompare([]byte(tok), []byte(t)) == 1 {
				return true
			}
		}
	}
	return false
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
