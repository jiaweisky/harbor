// Command harbor exposes coding-agent CLIs as A2A (v1.0) agents.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
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
	"github.com/jiaweisky/harbor/internal/plugins"
	"github.com/jiaweisky/harbor/internal/server"
	"github.com/jiaweisky/harbor/internal/telemetry"
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

	// Load any runtime adapter plugins before building agents.
	if err := plugins.Load(cfg.Plugins); err != nil {
		return fmt.Errorf("plugins: %w", err)
	}

	// Optional per-task raw stdout/stderr logging for replay/debugging.
	agent.SetRawStream(cfg.Sandbox.RawStreamDir, cfg.Sandbox.RawStreamCompress)

	// Clean up CLI processes left behind by a previous crash, then track ours.
	agent.SweepOrphans(cfg.Sandbox.StateDir)
	agent.InitProcs(cfg.Sandbox.StateDir)

	servers, err := server.New(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() {
		if err := servers.Close(); err != nil {
			slog.Warn("store close", "err", err)
		}
	}()
	tlsConfig, err := httpTLSConfig(cfg.Server.TLS)
	if err != nil {
		return fmt.Errorf("server tls: %w", err)
	}
	srv := &http.Server{
		Addr:    cfg.Server.Listen,
		Handler: servers.HTTP,
		// Requests derive from the root context, so shutdown cancels in-flight tasks.
		BaseContext: func(net.Listener) context.Context { return ctx },
		TLSConfig:   tlsConfig,
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

// httpTLSConfig builds the server's TLS configuration, or nil when no
// certificate is configured (plaintext).
func httpTLSConfig(cfg config.TLSConfig) (*tls.Config, error) {
	if cfg.CertFile == "" {
		return nil, nil
	}
	cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return nil, err
	}
	tlsCfg := &tls.Config{Certificates: []tls.Certificate{cert}}
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no certificates in %s", cfg.CAFile)
		}
		tlsCfg.ClientCAs = pool
	}
	if cfg.ClientAuth {
		tlsCfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return tlsCfg, nil
}
