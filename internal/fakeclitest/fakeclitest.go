// Package fakeclitest builds the fakecli helper binary for tests and hands out
// per-name copies of it, so tests can launch the fake CLIs by name on any
// platform. The helper dispatches on its own base name (see internal/fakecli).
package fakeclitest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

var (
	mu       sync.Mutex
	built    string
	buildErr error
)

// Path returns the path of the fake CLI binary for name, with or without the
// .sh suffix of the shell-script era (both "fake-usage" and "fake-usage.sh"
// resolve to the same binary). The helper is built once per test process and
// copied per requested name.
func Path(t testing.TB, name string) string {
	t.Helper()

	mu.Lock()
	defer mu.Unlock()
	if built == "" && buildErr == nil {
		build()
	}
	if buildErr != nil {
		t.Fatalf("build fakecli: %v", buildErr)
	}

	name = strings.TrimSuffix(name, ".sh")
	dst := filepath.Join(t.TempDir(), name+ext())
	if err := copyFile(built, dst); err != nil {
		t.Fatalf("copy fakecli: %v", err)
	}
	return dst
}

func build() {
	dir, err := os.MkdirTemp("", "fakecli-*")
	if err != nil {
		buildErr = err
		return
	}

	out := filepath.Join(dir, "fakecli"+ext())
	cmd := exec.Command("go", "build", "-o", out,
		"github.com/jiaweisky/harbor/internal/fakecli")
	if b, err := cmd.CombinedOutput(); err != nil {
		buildErr = fmt.Errorf("%v: %s", err, b)
		return
	}
	built = out
}

func ext() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o755)
}
