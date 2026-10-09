package server

import (
	"reflect"
	"testing"

	"github.com/jiaweisky/harbor/internal/agent"
	"github.com/jiaweisky/harbor/internal/config"
)

// TestContainerSpecFromSandbox verifies the agent workdir, the isolation root,
// and the CLI home become container mounts (at identical paths), and the
// container settings are carried over.
func TestContainerSpecFromSandbox(t *testing.T) {
	s := config.SandboxConfig{
		IsolationRoot: "/srv/iso",
		CLIHome:       "/srv/home",
		Container:     &config.ContainerConfig{Runtime: "docker", Image: "img:latest", Network: "none"},
	}

	got := containerSpec(s, "/srv/work")
	want := &agent.ContainerSpec{
		Runtime: "docker", Image: "img:latest", Network: "none",
		Mounts: []string{"/srv/work", "/srv/iso", "/srv/home"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("containerSpec = %+v, want %+v", got, want)
	}
}

// TestContainerSpecNilWhenUnset verifies no container produces a nil spec.
func TestContainerSpecNilWhenUnset(t *testing.T) {
	if got := containerSpec(config.SandboxConfig{}, "/srv/work"); got != nil {
		t.Fatalf("containerSpec = %+v, want nil", got)
	}
}
