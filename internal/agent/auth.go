package agent

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// authProbeTimeout bounds a login-status probe so startup is not blocked.
const authProbeTimeout = 10 * time.Second

// Authenticator describes a coding-agent CLI's authentication surface. Each
// supported CLI provides its own implementation. Harbor never gates task
// execution on its own credential guess: the CLI decides whether it can run, so
// a CLI that starts standalone is never rejected by Harbor. Credentials for the
// non-interactive CLIs are supplied through the environment; the authenticator's
// status is only used to report readiness.
type Authenticator interface {
	// LoggedIn reports whether a usable credential is present. home is the base
	// home directory (the configured CLI home, or the real user home) and env is
	// the effective environment, so a CLI can resolve its own config locations.
	// An implementation that cannot verify returns true.
	LoggedIn(home string, env map[string]string) bool
	// StatusArgs returns the args of the CLI's login-status command, or nil when
	// the CLI has no such command.
	StatusArgs() []string
	// LoggedInStatus interprets the login-status command's output. It is called
	// only when StatusArgs is non-nil.
	LoggedInStatus(output string, runErr error) bool
}

// CheckStatus runs the CLI's login-status command and reports whether it is
// logged in. It returns true when the CLI has no status command, so an
// unverifiable CLI never reports unready.
func CheckStatus(ctx context.Context, binary string, env []string, auth Authenticator) bool {
	if auth == nil {
		return true
	}

	args := auth.StatusArgs()
	if len(args) == 0 {
		return true
	}

	ctx, cancel := context.WithTimeout(ctx, authProbeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()

	return auth.LoggedInStatus(string(out), err)
}

// Authenticated reports whether the CLI already holds a usable ticket, using a
// live status command when the CLI offers one, otherwise a static check of the
// environment and the CLI's home. It is best-effort and used for readiness
// reporting only; it never blocks a task.
func Authenticated(ctx context.Context, binary, home string, env []string, auth Authenticator) bool {
	if auth == nil {
		return true
	}

	if len(auth.StatusArgs()) > 0 {
		return CheckStatus(ctx, binary, env, auth)
	}

	return auth.LoggedIn(home, EnvMap(env))
}

// AuthReadyFor reports whether a CLI holds a usable ticket, using its runner
// for the isolated home and the effective environment.
func AuthReadyFor(ctx context.Context, binary string, runner *Runner, env []string, auth Authenticator) bool {
	return Authenticated(ctx, binary, runner.AuthBase(), runner.Env(&Invocation{Env: env}), auth)
}

// EnvMap returns the environment as a name->value map (later entries win), so
// adapters can resolve config locations from the values a CLI will actually see.
func EnvMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k != "" {
			m[k] = v
		}
	}

	return m
}
