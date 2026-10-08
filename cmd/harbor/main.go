// Command harbor exposes coding-agent CLIs as A2A (v1.0) agents.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jiaweisky/harbor/internal/agent"
	"github.com/jiaweisky/harbor/internal/bridge"
	"github.com/jiaweisky/harbor/internal/config"
	"github.com/jiaweisky/harbor/internal/server"
)

// shutdownTimeout bounds graceful shutdown after a termination signal.
const shutdownTimeout = 10 * time.Second

func main() {
	if err := run(); err != nil {
		slog.Error("harbor exited with error", "err", err)
		os.Exit(1)
	}
}

// run wires the server and serves until a termination signal, then shuts down
// gracefully. A single root context is shared by startup probes and every
// request, so canceling it tears down in-flight tasks and their CLI processes.
func run() error {
	cfgPath := flag.String("config", "examples/agents.yaml", "path to agents.yaml")
	flag.Parse()

	cfg := config.MustLoad(*cfgPath)
	if err := bridge.RequireAuthOnExposedBind(
		cfg.Server.Listen, cfg.Server.Auth.Mode, cfg.Server.AllowInsecure,
	); err != nil {
		return fmt.Errorf("refusing to start: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Clean up CLI processes left behind by a previous crash, then track ours.
	agent.SweepOrphans(cfg.Sandbox.StateDir)
	agent.InitProcs(cfg.Sandbox.StateDir)

	handler, err := server.NewHandler(ctx, cfg)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:    cfg.Server.Listen,
		Handler: handler,
		// Requests derive from the root context, so shutdown cancels in-flight tasks.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()
	slog.Info("harbor listening", "addr", cfg.Server.Listen, "discovery", server.DiscoveryPath)

	select {
	case <-ctx.Done():
		stop() // restore default signal handling so a second signal force-quits
		slog.Info("shutting down", "timeout", shutdownTimeout)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
