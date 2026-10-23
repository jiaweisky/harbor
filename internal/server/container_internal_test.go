package server

import (
	"reflect"
	"testing"

	"github.com/jiaweisky/harbor/internal/config"
	"github.com/jiaweisky/harbor/internal/sandbox"
	"github.com/jiaweisky/harbor/internal/workspace"
)

// TestContainerSpecFromSandbox verifies the CLI home and container settings are
// carried over, and which roots are mounted for each isolation strategy.
func TestContainerSpecFromSandbox(t *testing.T) {
	cases := []struct {
		name      string
		isolation workspace.Isolation
		want      []string
	}{
		{"shared", workspace.IsolationShared, []string{"/srv/work", "/srv/iso", "/srv/home"}},
		{"worktree", workspace.IsolationWorktree, []string{"/srv/work", "/srv/iso", "/srv/home"}},
		{"copy", workspace.IsolationCopy, []string{"/srv/iso", "/srv/home"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := config.SandboxConfig{
				Isolation:     tc.isolation,
				IsolationRoot: "/srv/iso",
				CLIHome:       "/srv/home",
				Container: &config.ContainerConfig{
					Runtime: "docker", Image: "img:latest",
					ExtraArgs: []string{"--read-only"},
				},
			}

			got := containerSpec(s, "/srv/work",
				workspace.NewResolverWithIsolation(
					"/srv/work", tc.isolation, "/srv/iso", s.CopyStrategy,
				).NeedsSourceMount())
			want := &sandbox.Container{
				Runtime: "docker", Image: "img:latest",
				Mounts:    tc.want,
				ExtraArgs: []string{"--read-only"},
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("containerSpec = %+v, want %+v", got, want)
			}
		})
	}
}

// TestContainerSpecNilWhenUnset verifies no container produces a nil spec.
func TestContainerSpecNilWhenUnset(t *testing.T) {
	if got := containerSpec(config.SandboxConfig{}, "/srv/work", true); got != nil {
		t.Fatalf("containerSpec = %+v, want nil", got)
	}
}
