package agent_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jiaweisky/harbor/internal/agent"
)

func TestRunnerEnvRoots(t *testing.T) {
	r := agent.NewRunner(0, true, "/base/home", nil, nil)
	if got := r.AuthBase(); got != "/base/home" {
		t.Errorf("AuthBase() = %q", got)
	}

	env := agent.EnvMap(r.Env(&agent.Invocation{}))
	rootVars := []string{
		"HOME", "XDG_DATA_HOME", "CLAUDE_CONFIG_DIR", "CODEX_HOME",
		"CODEBUDDY_CONFIG_DIR", "OPENCODE_CONFIG_DIR",
		"USERPROFILE", "APPDATA", "LOCALAPPDATA",
	}
	for _, k := range rootVars {
		if env[k] == "" {
			t.Errorf("env missing %q", k)
		}
	}
}

func TestAuthBaseFallsBackToRealHome(t *testing.T) {
	r := agent.NewRunner(0, true, "", nil, nil)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no user home")
	}
	if got := r.AuthBase(); got != filepath.Clean(home) && got != home {
		t.Errorf("AuthBase() = %q, want %q", got, home)
	}
}
