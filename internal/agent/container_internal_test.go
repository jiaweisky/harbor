package agent

import (
	"reflect"
	"testing"
)

// TestContainerSpecCommand verifies the container runtime argv: flags, mounts at
// host paths, working directory, environment forwarding by name, deployer
// ExtraArgs, image, and the wrapped CLI command.
func TestContainerSpecCommand(t *testing.T) {
	c := &ContainerSpec{
		Runtime: "docker", Image: "img:latest",
		Mounts:    []string{"/srv/work", "/home/cli"},
		ExtraArgs: []string{"--read-only", "--pids-limit=512"},
	}
	inv := &Invocation{Bin: "claude", Args: []string{"-p", "hi"}, Dir: "/srv/work/task"}
	env := []string{"PATH=/bin", "PWD=/srv/work/task"}

	bin, args := c.Command(inv.Bin, inv.Args, inv.Dir, env)

	if bin != "docker" {
		t.Fatalf("bin = %q, want docker", bin)
	}
	want := []string{
		"run", "--rm", "-i",
		"-v", "/srv/work:/srv/work", "-v", "/home/cli:/home/cli",
		"-w", "/srv/work/task",
		"-e", "PATH", "-e", "PWD",
		"--read-only", "--pids-limit=512",
		"img:latest", "claude", "-p", "hi",
	}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args =\n%q\nwant\n%q", args, want)
	}
}

// TestContainerSpecCommandMinimal verifies a container run without mounts or a
// workdir emits neither mounts nor -w.
func TestContainerSpecCommandMinimal(t *testing.T) {
	c := &ContainerSpec{Runtime: "podman", Image: "img"}
	inv := &Invocation{Bin: "codex", Args: []string{"exec"}}

	bin, args := c.Command(inv.Bin, inv.Args, inv.Dir, nil)

	if bin != "podman" {
		t.Fatalf("bin = %q, want podman", bin)
	}
	want := []string{"run", "--rm", "-i", "img", "codex", "exec"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args = %q, want %q", args, want)
	}
}
