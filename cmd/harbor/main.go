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
	"github.com/jiaweisky/harbor/internal/cli"
	"github.com/jiaweisky/harbor/internal/sandbox"
	"github.com/jiaweisky/harbor/internal/server"
	"github.com/jiaweisky/harbor/internal/telemetry"
)

// shutdownTimeout bounds graceful shutdown after a termination signal.
const shutdownTimeout = 10 * time.Second

// version is the harbor build version, overridable at link time with
// -ldflags "-X main.version=...".
var version = "dev"

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
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return nil
	}

	cfg, err := cli.Load(*cfgPath)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// A configured container sandbox must work before the server accepts tasks.
	if ct := cfg.Sandbox.Container; ct != nil {
		if err := sandbox.CheckRuntime(ctx, ct.Runtime); err != nil {
			return fmt.Errorf("sandbox.container: %w", err)
		}
	}

	// Install OTLP trace export when configured; the returned shutdown flushes
	// pending spans on exit.
	shutdownTelemetry, err := telemetry.Init(context.Background(), telemetry.Config{
		Endpoint:    cfg.Telemetry.Endpoint,
		Insecure:    cfg.Telemetry.Insecure,
		ServiceName: cfg.Telemetry.ServiceName,
	})
	if err != nil {
		return fmt.Errorf("telemetry: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := shutdownTelemetry(shutdownCtx); err != nil {
			slog.Warn("telemetry shutdown", "err", err)
		}
	}()

	// Clean up orphaned CLIs from a previous crash, then build the runtime that
	// tracks this process's children and logs their raw streams.
	agent.SweepOrphans(cfg.Sandbox.StateDir)
	rt := agent.NewRuntime(cfg.Sandbox.StateDir, cfg.Sandbox.RawStreamDir, cfg.Sandbox.RawStreamCompress)

	servers, err := server.New(ctx, cfg, rt)
	if err != nil {
		return err
	}
	defer func() {
		if err := servers.Close(); err != nil {
			slog.Warn("server close", "err", err)
		}
	}()
	srv := &http.Server{
		Addr:    cfg.Server.Listen,
		Handler: servers.HTTP,
		// Requests derive from the root context, so shutdown cancels in-flight tasks.
		BaseContext: func(net.Listener) context.Context { return ctx },
		TLSConfig:   servers.TLSConfig,
	}

	serveErr := make(chan error, 1)
	go func() {
		if cfg.Server.TLS.CertFile != "" {
			serveErr <- srv.ListenAndServeTLS(cfg.Server.TLS.CertFile, cfg.Server.TLS.KeyFile)
			return
		}
		serveErr <- srv.ListenAndServe()
	}()
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
