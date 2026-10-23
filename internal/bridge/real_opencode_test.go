//go:build realoc

package bridge_test

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/jiaweisky/harbor/internal/agent"
	"github.com/jiaweisky/harbor/internal/bridge"
	"github.com/jiaweisky/harbor/internal/config"
	"github.com/jiaweisky/harbor/internal/core"
	"github.com/jiaweisky/harbor/internal/integration"
	"github.com/jiaweisky/harbor/internal/session"
	"github.com/jiaweisky/harbor/internal/workspace"
)

// opencodeHandler builds a handler for a real opencode run with no workspace
// confinement. The context bounds a spawned `opencode serve` process in server
// mode.
func opencodeHandler(t *testing.T, ctx context.Context, ac config.AgentConfig) a2asrv.RequestHandler {
	return opencodeHandlerWith(t, ctx, nil, 60*time.Second, ac)
}

// opencodeHandlerWith builds an opencode handler with a workspace resolver and a
// task timeout.
func opencodeHandlerWith(
	t *testing.T, ctx context.Context, ws *workspace.Resolver, timeout time.Duration, ac config.AgentConfig,
) a2asrv.RequestHandler {
	t.Helper()
	ln, err := integration.New(ac, integration.Options{InheritEnv: true, Ctx: ctx})
	if err != nil {
		t.Fatalf("integration.New: %v", err)
	}
	exec := bridge.NewCLIExecutor(ln, session.New(), agent.NewLiveSessions(), ws,
		core.Limits{Timeout: timeout})
	return a2asrv.NewHandler(exec, a2asrv.WithCapabilityChecks(&a2a.AgentCapabilities{Streaming: true}))
}

func requireOpencode(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Skip("opencode not installed")
	}
}

func TestRealOpencodeExec(t *testing.T) {
	requireOpencode(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h := opencodeHandler(t, ctx, config.AgentConfig{
		ID: "oc", Binary: "opencode", Integration: core.IntegrationExec,
		Args:    []string{"run", "{{.Prompt}}"},
		Session: config.SessionConfig{IDFlag: "--session", ContinueFlag: "--continue"},
	})
	sendAndExpect(t, h, message("reply with the single word: pong"), "pong")
}

func TestRealOpencodeServer(t *testing.T) {
	requireOpencode(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h := opencodeHandler(t, ctx, config.AgentConfig{
		ID: "oc", Binary: "opencode", Integration: core.IntegrationServer,
	})
	sendAndExpect(t, h, message("reply with the single word: pong"), "pong")
}

func TestRealOpencodeACP(t *testing.T) {
	requireOpencode(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h := opencodeHandler(t, ctx, config.AgentConfig{
		ID: "oc", Binary: "opencode", Integration: core.IntegrationACP,
		Args: []string{"acp"},
	})
	sendAndExpect(t, h, message("reply with the single word: pong"), "pong")
}
