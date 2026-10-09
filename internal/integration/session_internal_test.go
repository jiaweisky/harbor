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

// TestExecParserFactoryRejectsUnknown verifies an unknown parser name fails at
// build time rather than per task.
func TestExecParserFactoryRejectsUnknown(t *testing.T) {
	if _, err := execParserFactory(config.AgentConfig{Parser: "no-such-parser"}); err == nil {
		t.Fatal("unknown parser name must error")
	}
}

// TestExecParserFactoryFreshPerTask verifies each task gets its own parser
// instance, since a stream-json parser carries per-task state.
func TestExecParserFactoryFreshPerTask(t *testing.T) {
	f, err := execParserFactory(config.AgentConfig{Parser: "stream-json"})
	if err != nil {
		t.Fatal(err)
	}
	if f() == f() {
		t.Fatal("each task must get a fresh parser instance")
	}
}
