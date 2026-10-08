package integration

import (
	"reflect"
	"testing"

	"github.com/jiaweisky/harbor/internal/agent"
	"github.com/jiaweisky/harbor/internal/config"
)

// TestSessionArgsFork verifies a forked resume appends the fork flag after the
// resume flag and session id.
func TestSessionArgsFork(t *testing.T) {
	ac := config.AgentConfig{Session: config.SessionConfig{ResumeFlag: "--resume", ForkFlag: "--fork-session"}}
	got := sessionArgs(ac, &agent.Request{SessionID: "s1", Fork: true})
	want := []string{"--resume", "s1", "--fork-session"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sessionArgs = %v, want %v", got, want)
	}
}

// TestSessionArgsResumeWithoutFork verifies a plain resume omits the fork flag.
func TestSessionArgsResumeWithoutFork(t *testing.T) {
	ac := config.AgentConfig{Session: config.SessionConfig{ResumeFlag: "--resume", ForkFlag: "--fork-session"}}
	got := sessionArgs(ac, &agent.Request{SessionID: "s1"})
	if !reflect.DeepEqual(got, []string{"--resume", "s1"}) {
		t.Fatalf("sessionArgs = %v, want [--resume s1]", got)
	}
}
