package sandbox_test

import (
	"context"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/jiaweisky/harbor/internal/core"
	"github.com/jiaweisky/harbor/internal/sandbox"
)

func TestMounts(t *testing.T) {
	cases := []struct {
		name        string
		needsSource bool
		want        []string
	}{
		{"source needed", true, []string{"/srv/work", "/srv/iso", "/srv/home"}},
		{"self-contained copy", false, []string{"/srv/iso", "/srv/home"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sandbox.Mounts("/srv/work", tc.needsSource, "/srv/iso", "/srv/home")
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Mounts = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCheckRuntimeMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	err := sandbox.CheckRuntime(context.Background(), core.ContainerRuntimeDocker)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v, want a not-found error", err)
	}
}

func TestCheckRuntimePlatform(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("nerdctl is allowed on Linux; the info probe covers the rest")
	}
	err := sandbox.CheckRuntime(context.Background(), core.ContainerRuntimeNerdctl)
	if err == nil || !strings.Contains(err.Error(), "only supported on Linux") {
		t.Fatalf("err = %v, want a platform error", err)
	}
}
