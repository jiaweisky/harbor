package agent_test

import (
	"slices"
	"testing"

	"github.com/jiaweisky/harbor/internal/agent"
)

// TestOutputModesIncludeImages verifies the advertised output MIME modes include
// the images a CLI can produce.
func TestOutputModesIncludeImages(t *testing.T) {
	caps := agent.Capabilities{OutputText: true, OutputImage: []string{"image/png", "image/jpeg"}}
	modes := caps.OutputModes()
	if !slices.Contains(modes, "text/plain") || !slices.Contains(modes, "image/png") {
		t.Fatalf("output modes = %v, want text/plain and image/png", modes)
	}
}
