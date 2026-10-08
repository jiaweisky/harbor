//go:build cgo && (linux || darwin || freebsd)

package plugins_test

import (
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/jiaweisky/harbor/internal/agent"
	"github.com/jiaweisky/harbor/internal/plugins"
)

// TestLoadRegistersAdapter builds the test plugin and verifies loading it
// registers the adapter it exports.
func TestLoadRegistersAdapter(t *testing.T) {
	so := filepath.Join(t.TempDir(), "register.so")
	cmd := exec.Command("go", "build", "-buildmode=plugin", "-o", so, "./testdata/register")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build plugin: %v\n%s", err, out)
	}

	if err := plugins.Load([]string{so}); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := agent.LookupAdapter("plugin-cli"); !ok {
		t.Fatal("plugin did not register its adapter")
	}
}

// TestLoadMissingFile verifies a bad plugin path returns an error.
func TestLoadMissingFile(t *testing.T) {
	if err := plugins.Load([]string{filepath.Join(t.TempDir(), "nope.so")}); err == nil {
		t.Fatal("expected an error for a missing plugin file")
	}
}
