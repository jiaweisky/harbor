package adapters

import (
	"path/filepath"

	"github.com/jiaweisky/harbor/internal/agent"
)

func init() { Register("opencode", func(_ string) agent.Authenticator { return opencode{} }) }

// opencode stores credentials in its data directory (XDG_DATA_HOME/opencode/
// auth.json) and also accepts provider keys and inline auth content in the
// environment.
type opencode struct{}

var opencodeCredEnv = []string{
	"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "OPENCODE_API_KEY", "OPENCODE_AUTH_CONTENT",
}

func (opencode) LoggedIn(home string, env map[string]string) bool {
	if anySet(env, opencodeCredEnv...) {
		return true
	}
	data := env["XDG_DATA_HOME"]
	if data == "" {
		data = filepath.Join(home, ".local", "share")
	}
	return fileExists(filepath.Join(data, "opencode", "auth.json"))
}

func (opencode) LoginArgs() []string { return []string{"auth", "login"} }

// opencode has no status command with a reliable exit code or stable output, so
// it is not probed for readiness.
func (opencode) StatusArgs() []string { return nil }

func (opencode) LoggedInStatus(string, error) bool { return true }
