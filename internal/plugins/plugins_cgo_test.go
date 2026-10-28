//go:build cgo && (linux || darwin || freebsd)

package plugins_test

import (
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/jiaweisky/harbor/internal/cli"
	"github.com/jiaweisky/harbor/internal/plugins"
)

// TestLoadRegistersAdapter builds the test plugin and verifies loading it
// registers the adapter it exports.
func TestLoadRegistersAdapter(t *testing.T) {
	so := filepath.Join(t.TempDir(), "register.so")
	args := []string{"build", "-buildmode=plugin", "-o", so}
	if raceEnabled {
		args = append(args, "-race")
	}
	args = append(args, "./testdata/register")
	cmd := exec.Command("go", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build plugin: %v\n%s", err, out)
	}

	if err := plugins.Load([]string{so}); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := cli.LookupDescriptor("plugin-cli"); !ok {
		t.Fatal("plugin did not register its adapter")
	}
}

// TestLoadMissingFile verifies a bad plugin path returns an error.
func TestLoadMissingFile(t *testing.T) {
	if err := plugins.Load([]string{filepath.Join(t.TempDir(), "nope.so")}); err == nil {
		t.Fatal("expected an error for a missing plugin file")
	}
}
