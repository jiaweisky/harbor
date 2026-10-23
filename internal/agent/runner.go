package agent

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/jiaweisky/harbor/internal/sandbox"
)

// ErrCLINotFound indicates the CLI binary is missing or not executable.
var ErrCLINotFound = errors.New("cli not found")

// Process tracks one running CLI process group.
type Process struct {
	cmd      *exec.Cmd
	pgid     int
	stdin    io.WriteCloser
	done     chan struct{}
	stopped  chan struct{}
	stopOnce sync.Once
	canceled atomic.Bool
	span     trace.Span
	rawOut   io.WriteCloser // raw stdout log (optional)
	rawErr   io.WriteCloser // raw stderr log (optional)
}

// stop marks the process stopped and closes stopped so blocked event sends
// abort instead of leaking. It is idempotent.
func (p *Process) stop() {
	p.canceled.Store(true)
	p.stopOnce.Do(func() { close(p.stopped) })
}

// Runner spawns and supervises CLI subprocesses.
type Runner struct {
	sem        chan struct{}
	inheritEnv bool
	cliHome    string
	container  *sandbox.Container
	procs      *ProcRegistry
	raw        *RawStream
	live       sync.Map // taskID -> *Process
}

// NewRunner creates a Runner limiting concurrency to maxConcurrent (<=0 =
// unbounded). When inheritEnv is false the child receives only an allowlisted
// base environment plus the adapter-provided variables. When cliHome is set the
// child's HOME (and XDG dirs) are redirected under it. When container is set the
// CLI runs inside a container instead of directly on the host. rt supplies
// orphan tracking and raw stream logging and may be nil.
func NewRunner(
	maxConcurrent int, inheritEnv bool, cliHome string, container *sandbox.Container, rt *Runtime,
) *Runner {
	var sem chan struct{}
	if maxConcurrent > 0 {
		sem = make(chan struct{}, maxConcurrent)
	}
	r := &Runner{sem: sem, inheritEnv: inheritEnv, cliHome: cliHome, container: container}
	if rt != nil {
		r.procs = rt.Procs
		r.raw = rt.Raw
	}
	return r
}

// Start runs the CLI and streams normalized events parsed from its stdout.
func (r *Runner) Start(ctx context.Context, taskID string, inv *Invocation, p Parser) (<-chan Event, error) {
	if err := r.acquire(ctx); err != nil {
		return nil, err
	}

	env := buildEnv(r.inheritEnv, r.cliHome, inv.Env)
	// exec sets the child's working directory but leaves the inherited PWD
	// pointing at the parent's; a CLI that reads PWD (not getcwd) would then run
	// in the wrong directory. Keep PWD consistent with Dir.
	if inv.Dir != "" {
		env = setEnv(env, "PWD", inv.Dir)
	}
	bin, args := r.command(inv, env)

	// exec.Command (not CommandContext): cancellation is handled here so the
	// whole process group is terminated, not just the direct child.
	cmd := exec.Command(bin, args...)
	cmd.Dir = inv.Dir
	cmd.Env = env
	setProcessGroup(cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		r.release()
		return nil, err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		r.release()
		return nil, err
	}
	stderr, sink := stderrSink(inv)
	cmd.Stderr = sink

	// Optional raw stream logging (best effort; failures are ignored).
	rawOut, rawErr := r.raw.open(taskID)
	if rawErr != nil {
		cmd.Stderr = io.MultiWriter(sink, rawErr)
	}

	// One span per CLI subprocess, linked to the requesting task's span via ctx.
	// It ends when the process exits (see readLoop).
	ctx, span := otel.Tracer("harbor").Start(ctx, "harbor.cli", trace.WithAttributes(
		attribute.String("process.executable.name", filepath.Base(inv.Bin)),
		attribute.String("harbor.workdir", inv.Dir),
	))

	if err := cmd.Start(); err != nil {
		span.End()
		closeRaw(rawOut)
		closeRaw(rawErr)
		r.release()
		return nil, classifyStartErr(err)
	}
	r.feedStdin(stdin, inv)

	out := make(chan Event, 64)
	proc := &Process{
		cmd:     cmd,
		pgid:    cmd.Process.Pid,
		stdin:   stdin,
		done:    make(chan struct{}),
		stopped: make(chan struct{}),
		span:    span,
		rawOut:  rawOut,
		rawErr:  rawErr,
	}
	r.live.Store(taskID, proc)
	if r.procs != nil {
		r.procs.Add(proc.pgid, proc.cmd.Path)
	}
	go r.watchCancellation(ctx, proc)
	go r.readLoop(stdout, p, out, stderr, proc, taskID)

	return out, nil
}

// stderrSink returns the writer for cmd.Stderr and the value used to report
// stderr in errors. When inv.StderrLine is set, lines are also forwarded (so a
// spawner can read a server's listening address from stderr); otherwise stderr
// is only retained, bounded, for diagnostics.
func stderrSink(inv *Invocation) (fmt.Stringer, io.Writer) {
	if inv.StderrLine != nil {
		sw := newStderrWriter(inv.StderrLine)
		return sw, sw
	}
	buf := newLimitedBuffer(64 << 10)
	return buf, buf
}

// command returns the binary and args to execute, wrapping the invocation in a
// container runtime when a container sandbox is configured.
func (r *Runner) command(inv *Invocation, env []string) (string, []string) {
	if r.container == nil {
		return inv.Bin, inv.Args
	}
	return r.container.Command(inv.Bin, inv.Args, inv.Dir, env)
}

// feedStdin copies an optional initial payload and, unless the invocation wants
// a duplex pipe, closes stdin so a one-shot CLI sees EOF. With no payload and no
// duplex need, stdin is closed immediately: otherwise a CLI that reads stdin
// (e.g. `opencode run`, `codex exec`) blocks forever on an open pipe.
func (r *Runner) feedStdin(stdin io.WriteCloser, inv *Invocation) {
	if inv.Stdin == nil {
		if !inv.KeepStdinOpen {
			_ = stdin.Close()
		}
		return
	}
	go func() {
		_, _ = io.Copy(stdin, inv.Stdin)
		if !inv.KeepStdinOpen {
			_ = stdin.Close()
		}
	}()
}

// readLoop parses stdout line by line, then waits for the process. It owns the
// event channel's lifetime and removes the process entry when it finishes.
//
// Event sends block when the buffer is full (backpressure); they abort as soon
// as the process is stopped, so a consumer that goes away never leaks this
// goroutine or its concurrency slot.
func (r *Runner) readLoop(
	stdout io.Reader, p Parser, out chan<- Event,
	stderr fmt.Stringer, proc *Process, taskID string,
) {
	defer close(out)
	defer close(proc.done)
	defer r.release()
	defer proc.span.End()
	defer closeRaw(proc.rawOut)
	defer closeRaw(proc.rawErr)
	defer r.live.CompareAndDelete(taskID, proc)
	defer func() {
		if r.procs != nil {
			r.procs.Remove(proc.pgid)
		}
	}()

	alive := true
	emit := func(e Event) {
		if !alive {
			return
		}
		select {
		case out <- e:
		case <-proc.stopped:
			alive = false
		}
	}

	rd := bufio.NewReaderSize(stdout, 64*1024)
	for alive {
		line, readErr := rd.ReadBytes('\n')
		if proc.rawOut != nil {
			_, _ = proc.rawOut.Write(line)
		}
		if rec := trimLine(line); len(rec) > 0 {
			if perr := p.Parse(rec, emit); perr != nil {
				emit(Event{Kind: EventError, Err: perr})
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				emit(Event{Kind: EventError, Err: readErr})
			}
			break
		}
	}
	_ = p.Flush(emit)

	// Wait always runs so the process is reaped even after an early stop.
	if werr := proc.cmd.Wait(); werr != nil && !proc.canceled.Load() {
		emit(Event{Kind: EventError, Err: fmt.Errorf("cli exit: %w; stderr=%s", werr, truncate(stderr.String(), 2000))})
	}
}

// watchCancellation terminates the process group if ctx is canceled before exit.
func (r *Runner) watchCancellation(ctx context.Context, proc *Process) {
	select {
	case <-ctx.Done():
		proc.stop()
		terminateGroup(proc)
		select {
		case <-proc.done:
		case <-time.After(5 * time.Second):
			killGroup(proc)
		}
	case <-proc.done:
	}
}

// Kill terminates the process group for taskID (SIGTERM, then SIGKILL).
func (r *Runner) Kill(taskID string) {
	v, ok := r.live.LoadAndDelete(taskID)
	if !ok {
		return
	}
	p := v.(*Process)
	p.stop()
	terminateGroup(p)
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		killGroup(p)
	}
}

// Stdin returns the process stdin for duplex sessions.
func (r *Runner) Stdin(taskID string) io.WriteCloser {
	if v, ok := r.live.Load(taskID); ok {
		return v.(*Process).stdin
	}
	return nil
}

func (r *Runner) acquire(ctx context.Context) error {
	if r.sem == nil {
		return nil
	}
	select {
	case r.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *Runner) release() {
	if r.sem != nil {
		<-r.sem
	}
}

// baseEnvKeys are the parent variables kept when environment inheritance is
// disabled (sandbox.inherit_env: false); every other parent variable is dropped.
// Adapter-provided variables are always added on top of this set. The Windows
// roots are included so a CLI there can still find its profile.
var baseEnvKeys = map[string]bool{
	"PATH": true, "HOME": true, "USERPROFILE": true, "APPDATA": true, "LOCALAPPDATA": true,
	"LANG": true, "LC_ALL": true, "TERM": true, "TMPDIR": true,
}

// buildEnv returns the child environment. When inherit is false only an
// allowlisted base plus the adapter extras is passed (no credential leakage).
// When cliHome is set, HOME and the CLI config roots are redirected under it;
// those redirects win over any adapter-provided values so isolation cannot be
// overridden.
func buildEnv(inherit bool, cliHome string, extra []string) []string {
	base := os.Environ()
	if !inherit {
		base = allowlistedEnv()
	}
	return append(base, append(extra, cliHomeEnv(cliHome)...)...)
}

// Env returns the full environment a child invocation would receive, so callers
// can inspect it (for example to check credentials) before starting.
func (r *Runner) Env(inv *Invocation) []string {
	return buildEnv(r.inheritEnv, r.cliHome, inv.Env)
}

// AuthBase returns the base home directory for credential checks: the
// configured CLI home, otherwise the process user's real home.
func (r *Runner) AuthBase() string {
	if r.cliHome != "" {
		return r.cliHome
	}
	if home, err := os.UserHomeDir(); err == nil {
		return home
	}
	return ""
}

// setEnv sets key=value in env, replacing an existing entry or appending.
func setEnv(env []string, key, value string) []string {
	prefix := key + "="
	for i, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}

func allowlistedEnv() []string {
	env := make([]string, 0, len(baseEnvKeys))
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); baseEnvKeys[k] {
			env = append(env, kv)
		}
	}
	return env
}

// cliHomeEnv redirects a CLI's config/home to the configured directory. It sets
// the roots each supported CLI resolves its config under, so the CLI derives the
// platform-correct subpaths itself.
func cliHomeEnv(cliHome string) []string {
	if cliHome == "" {
		return nil
	}
	home := cliHome
	return []string{
		// Unix roots.
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"XDG_DATA_HOME=" + filepath.Join(home, ".local", "share"),
		"XDG_CACHE_HOME=" + filepath.Join(home, ".cache"),
		// Windows roots: a CLI on Windows resolves its config from these
		// instead of HOME, so setting only HOME would leak the real profile.
		"USERPROFILE=" + home,
		"APPDATA=" + filepath.Join(home, "AppData", "Roaming"),
		"LOCALAPPDATA=" + filepath.Join(home, "AppData", "Local"),
		// CLI-specific config roots.
		"CLAUDE_CONFIG_DIR=" + filepath.Join(home, ".claude"),
		"CODEX_HOME=" + filepath.Join(home, ".codex"),
		"CODEBUDDY_CONFIG_DIR=" + filepath.Join(home, ".codebuddy"),
		"OPENCODE_CONFIG_DIR=" + filepath.Join(home, ".config", "opencode"),
	}
}

func classifyStartErr(err error) error {
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %w", ErrCLINotFound, err)
	}
	return err
}

func trimLine(line []byte) []byte {
	line = bytes.TrimRight(line, "\r\n")
	if len(line) == 0 {
		return nil
	}
	return append([]byte(nil), line...)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}

// stderrWriter retains a bounded copy of stderr (for error reporting) and
// forwards complete lines to onLine, so a spawner can read a server's listening
// address even when the CLI prints it to stderr.
type stderrWriter struct {
	onLine func(string)
	kept   limitedBuffer
	buf    []byte
}

func newStderrWriter(onLine func(string)) *stderrWriter {
	return &stderrWriter{onLine: onLine, kept: limitedBuffer{max: 64 << 10}}
}

func (w *stderrWriter) Write(p []byte) (int, error) {
	_, _ = w.kept.Write(p)
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		if w.onLine != nil {
			w.onLine(string(w.buf[:i]))
		}
		w.buf = w.buf[i+1:]
	}
	if len(w.buf) > 64<<10 {
		w.buf = w.buf[len(w.buf)-(64<<10):]
	}
	return len(p), nil
}

func (w *stderrWriter) String() string { return w.kept.String() }

// limitedBuffer keeps at most max bytes; extra writes are dropped.
type limitedBuffer struct {
	buf []byte
	max int
}

func newLimitedBuffer(max int) *limitedBuffer { return &limitedBuffer{max: max} }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.max - len(b.buf); room > 0 {
		if room > len(p) {
			room = len(p)
		}
		b.buf = append(b.buf, p[:room]...)
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string { return string(b.buf) }
