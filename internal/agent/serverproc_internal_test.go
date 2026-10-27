package agent

import (
	"context"
	"testing"
	"time"

	"github.com/jiaweisky/harbor/internal/fakeclitest"
)

// TestSpawnServerReadsStderrEndpoint verifies a server that prints its listening
// address to stderr (e.g. codebuddy's streamable-http endpoint) is discovered.
func TestSpawnServerReadsStderrEndpoint(t *testing.T) {
	script := fakeclitest.Path(t, "fake-server-stderr")
	r := NewRunner(0, true, "", nil, nil)
	defer r.Kill("t")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	url, err := SpawnServer(ctx, context.Background(), r, "t", &Invocation{Bin: script}, 5*time.Second)
	if err != nil {
		t.Fatalf("SpawnServer: %v", err)
	}
	if want := "http://127.0.0.1:12345/api/v1/acp"; url != want {
		t.Fatalf("url = %q, want %q", url, want)
	}
}
