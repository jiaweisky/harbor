package adapters

// opencode resolves providers from its own configuration (opencode.json) and
// the environment, so it is usable without a stored ticket and readiness must
// not be gated on one.
type opencode struct{}

func (opencode) LoggedIn(string, map[string]string) bool { return true }

// opencode has no status command with a reliable exit code or stable output, so
// it is not probed for readiness.
func (opencode) StatusArgs() []string { return nil }

func (opencode) LoggedInStatus(string, error) bool { return true }
