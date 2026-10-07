// Package server assembles the HTTP handler that exposes agents over A2A.
package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/push"

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

// NewHandler builds the HTTP handler that exposes every configured agent:
// a public agent card per agent, a JSON-RPC endpoint per agent, the standard
// first-agent card, and the aggregate discovery endpoint. The auth middleware,
// when enabled, protects only the JSON-RPC endpoints.
func NewHandler(ctx context.Context, cfg *config.Config) (http.Handler, error) {
	auth, err := authMiddleware(cfg.Server.Auth)
	if err != nil {
		return nil, err
	}
	bearer := cfg.Server.Auth.Mode == "bearer"

	mux := http.NewServeMux()

	pushStore := push.NewInMemoryStore()
	pushSender := push.NewHTTPPushSender(&push.HTTPSenderConfig{
		Timeout:              10 * time.Second,
		AllowPrivateNetworks: cfg.Server.PushAllowPrivateNetworks,
	})

	handlerOptions := []a2asrv.RequestHandlerOption{
		a2asrv.WithLogger(slog.Default()),
		a2asrv.WithAgentInactivityTimeout(10 * time.Minute),
		a2asrv.WithPushNotifications(pushStore, pushSender),
		a2asrv.WithCapabilityChecks(&a2a.AgentCapabilities{Streaming: true, PushNotifications: true}),
	}
	if cfg.Server.TaskStoreFile != "" {
		ts, err := store.NewFileStore(cfg.Server.TaskStoreFile)
		if err != nil {
			return nil, fmt.Errorf("task store: %w", err)
		}
		handlerOptions = append(handlerOptions, a2asrv.WithTaskStore(ts))
	}

	cards := make([]*a2a.AgentCard, 0, len(cfg.Agents))
	for i := range cfg.Agents {
		ac := cfg.Agents[i]
		ln, info, err := integration.Build(ctx, ac, integration.Options{
			InheritEnv: cfg.Sandbox.InheritEnvEnabled(),
			CLIHome:    cfg.Sandbox.CLIHome,
		})
		if err != nil {
			slog.Warn("agent disabled", "id", ac.ID, "err", err)
			continue
		}
		if !info.Enabled {
			slog.Warn("binary missing", "id", ac.ID, "err", info.Err)
			continue
		}

		resolver := workspace.NewResolverWithIsolation(cfg.Sandbox.WorkdirRoot,
			cfg.Sandbox.AllowOutside, cfg.Sandbox.Isolation, cfg.Sandbox.IsolationRoot)
		exec := bridge.NewCLIExecutor(ln, session.New(), agent.NewLiveSessions(), resolver, ac.Limits)
		handler := a2asrv.NewHandler(exec, handlerOptions...)

		path := AgentPath(ac.ID)
		card := bridge.BuildCard(ln, info, EndpointURL(cfg.Server.BaseURL, ac.ID), bearer)
		cards = append(cards, card)

		mux.Handle(path, auth(a2asrv.NewJSONRPCHandler(handler)))
		mux.Handle(path+".well-known/agent-card.json", a2asrv.NewStaticAgentCardHandler(card))
		slog.Info("agent ready", "id", ac.ID, "integration", ac.Integration, "endpoint", path)
	}

	if len(cards) == 0 {
		return nil, fmt.Errorf("no agents available; check the agent config and that their binaries exist")
	}
	mux.Handle("/.well-known/agent-card.json", a2asrv.NewStaticAgentCardHandler(cards[0]))
	mux.Handle(DiscoveryPath, bridge.NewCardsHandler(cards))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	return bridge.CORSMiddleware(cfg.Server.CORSAllowedOrigins, mux), nil
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
