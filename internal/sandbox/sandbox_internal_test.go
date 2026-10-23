package sandbox

import (
	"strings"
	"testing"

	"github.com/jiaweisky/harbor/internal/core"
)

func TestCheckPlatform(t *testing.T) {
	cases := []struct {
		name    string
		goos    string
		runtime core.ContainerRuntime
		wantErr string
	}{
		{"docker on linux", "linux", core.ContainerRuntimeDocker, ""},
		{"podman on darwin", "darwin", core.ContainerRuntimePodman, ""},
		{"nerdctl on linux", "linux", core.ContainerRuntimeNerdctl, ""},
		{"docker on windows", "windows", core.ContainerRuntimeDocker, "WSL"},
		{"podman on windows", "windows", core.ContainerRuntimePodman, "WSL"},
		{"nerdctl on windows", "windows", core.ContainerRuntimeNerdctl, "only supported on Linux"},
		{"nerdctl on darwin", "darwin", core.ContainerRuntimeNerdctl, "only supported on Linux"},
		{"unknown runtime", "linux", core.ContainerRuntime("unknown"), "not supported"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkPlatform(tc.goos, tc.runtime)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("checkPlatform = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("checkPlatform = %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestLastOutputLine(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want string
	}{
		{"warnings before the error", "warning\ncannot connect\n", "cannot connect"},
		{"single line", "failed", "failed"},
		{"blank lines", "\n\n", ""},
		{"trailing blank lines", "first\nlast\n\n", "last"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := lastOutputLine([]byte(tc.out)); got != tc.want {
				t.Fatalf("lastOutputLine = %q, want %q", got, tc.want)
			}
		})
	}
}
