package adapters

import "github.com/jiaweisky/harbor/internal/agent"

func init() { Register("codebuddy", func(_ string) agent.Authenticator { return codebuddy{} }) }

// codebuddy keeps its login state in the OS Keychain and has no headless login
// command, so only environment credentials can be verified and isolated.
type codebuddy struct{}

// LoggedIn cannot verify codebuddy's Keychain-based login state, so it never
// reports a definite absence; only environment credentials are observable.
func (codebuddy) LoggedIn(_ string, _ map[string]string) bool { return true }

func (codebuddy) LoginArgs() []string { return nil }

// codebuddy has no non-interactive status command.
func (codebuddy) StatusArgs() []string { return nil }

func (codebuddy) LoggedInStatus(string, error) bool { return true }
