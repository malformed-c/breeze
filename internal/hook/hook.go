// Package hook implements breeze's single command-execution primitive. Every stage
// main command, deploy command, and pre-gate/post-action hook runs through Run — this
// is the only exec.CommandContext call site in breeze.
package hook

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"time"
)

const maxCaptured = 64 * 1024

type Template struct {
	Path    string
	Args    []string
	Env     []string
	Dir     string
	Timeout time.Duration
	// ResourceLimits, when set, wraps this command's execution in a transient
	// systemd scope so a runaway build/test/deploy can't starve the host or
	// other concurrent work. See ResourceLimits and WrapWithSystemdRun.
	ResourceLimits *ResourceLimits
}

// ResourceLimits bounds a command's cgroup footprint via systemd-run --scope.
// All fields are optional; only limits actually set are passed through as
// systemd unit properties. CPUQuota/MemoryMax follow systemd's own syntax
// (e.g. "200%", "512M", "2G", "infinity") — breeze does not reinterpret them,
// so a malformed value surfaces as a systemd-run error captured in the
// command's own output, not a breeze-side validation error.
type ResourceLimits struct {
	CPUQuota  string // systemd CPUQuota=, e.g. "200%" for 2 cores
	MemoryMax string // systemd MemoryMax=, e.g. "512M", "2G"
	TasksMax  int    // systemd TasksMax=; 0 = unset
	IOWeight  int    // systemd IOWeight=, 1-10000; 0 = unset
}

// WrapWithSystemdRun rewrites (path, args) into a systemd-run invocation that
// runs the original command inside a new transient scope unit with rl's
// properties applied — still zero shell involvement, since systemd-run's own
// argv is one literal element per slice entry, exec'd directly like the
// unwrapped case. "--scope" execve()s directly into the target command in
// place (confirmed live: the PID systemd-run starts as IS the target's own
// PID, never a supervisor process still holding that PID), so Run's existing
// process-group timeout-kill and exit-code handling below work unchanged
// through the wrapper. "--quiet" suppresses systemd-run's own "Running as
// unit ..." notice so it never pollutes captured stdout/stderr; "--collect"
// unloads the transient unit once it exits so a long-running daemon doesn't
// accumulate one unit per run. A non-root caller needs "--user" (talks to the
// per-user systemd instance) — an unprivileged caller is denied a system-bus
// scope outright; root callers go straight to the system manager.
func WrapWithSystemdRun(path string, args []string, rl *ResourceLimits) (string, []string) {
	sdArgs := []string{"--scope", "--quiet", "--collect"}
	if os.Geteuid() != 0 {
		sdArgs = append(sdArgs, "--user")
	}
	if rl.CPUQuota != "" {
		sdArgs = append(sdArgs, "--property=CPUQuota="+rl.CPUQuota)
	}
	if rl.MemoryMax != "" {
		sdArgs = append(sdArgs, "--property=MemoryMax="+rl.MemoryMax)
	}
	if rl.TasksMax > 0 {
		sdArgs = append(sdArgs, fmt.Sprintf("--property=TasksMax=%d", rl.TasksMax))
	}
	if rl.IOWeight > 0 {
		sdArgs = append(sdArgs, fmt.Sprintf("--property=IOWeight=%d", rl.IOWeight))
	}
	sdArgs = append(sdArgs, "--", path)
	sdArgs = append(sdArgs, args...)
	return "systemd-run", sdArgs
}

// Params are substituted into argv/env placeholders. Values are attacker/agent
// controlled (e.g. a commit sha) but are NEVER shell-interpreted — see Run.
type Params map[string]string

type Result struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
	Duration time.Duration
	TimedOut bool
	Err      error // process-start failure, distinct from a nonzero exit
}

var placeholderRe = regexp.MustCompile(`\{([a-zA-Z_][a-zA-Z0-9_]*)\}`)

// Substitute replaces every {name} placeholder in s with params[name]. Whole-string
// substitution within a single argv element or env value — never concatenation into a
// shell command line, so there is nothing for a param value to "break out" of.
func Substitute(s string, params Params) string {
	return placeholderRe.ReplaceAllStringFunc(s, func(match string) string {
		name := match[1 : len(match)-1]
		if v, ok := params[name]; ok {
			return v
		}
		return match // unknown placeholders are a registration-time validation error, not a run-time no-op
	})
}

// Placeholders returns every distinct {name} referenced in s.
func Placeholders(s string) []string {
	matches := placeholderRe.FindAllStringSubmatch(s, -1)
	seen := make(map[string]bool, len(matches))
	var out []string
	for _, m := range matches {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	return out
}

// Run executes tmpl with params substituted into every argv element and every
// declared env entry, via exec.CommandContext with an explicit argv slice — never a
// shell — so shell metacharacters in a param value (e.g. "; rm -rf /", "$(whoami)")
// are inert, just literal bytes in one argv/env slot. On timeout the whole process
// group is killed (not just the direct child) to catch spawned grandchildren.
func Run(ctx context.Context, tmpl Template, params Params) Result {
	if tmpl.Timeout <= 0 {
		return Result{Err: fmt.Errorf("hook timeout must be > 0")}
	}
	ctx, cancel := context.WithTimeout(ctx, tmpl.Timeout)
	defer cancel()

	args := make([]string, len(tmpl.Args))
	for i, a := range tmpl.Args {
		args[i] = Substitute(a, params)
	}

	path := tmpl.Path
	if tmpl.ResourceLimits != nil {
		path, args = WrapWithSystemdRun(path, args, tmpl.ResourceLimits)
	}

	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir = Substitute(tmpl.Dir, params)
	cmd.Env = os.Environ()
	for _, e := range tmpl.Env {
		cmd.Env = append(cmd.Env, Substitute(e, params))
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// cmd.Wait() blocks until every process holding the inherited stdout/stderr pipe
	// fds exits — including a backgrounded grandchild the hook script spawned, even
	// after the direct child is killed. Two things are needed to actually bound this:
	// (1) cmd.Cancel fires the moment ctx times out (not after Wait returns) and must
	// kill the whole PROCESS GROUP, not just the direct child (Go's default Cancel
	// only kills cmd.Process); (2) cmd.WaitDelay forcibly closes the pipes and makes
	// Wait return if some fd is still held past a short grace period, rather than
	// hanging indefinitely.
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 2 * time.Second

	var stdout, stderr capBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	err := cmd.Start()
	if err != nil {
		return Result{Err: err, Duration: time.Since(start)}
	}

	waitErr := cmd.Wait()
	duration := time.Since(start)

	timedOut := ctx.Err() == context.DeadlineExceeded
	if timedOut && cmd.Process != nil {
		// Belt-and-suspenders: cmd.Cancel above already sent this on timeout, but a
		// second SIGKILL to a possibly-already-reaped group is harmless, and this
		// covers the case where Wait raced ahead of Cancel's goroutine.
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}

	res := Result{
		Stdout:   stdout.Bytes(),
		Stderr:   stderr.Bytes(),
		Duration: duration,
		TimedOut: timedOut,
	}
	if exitErr, ok := waitErr.(*exec.ExitError); ok {
		res.ExitCode = exitErr.ExitCode()
	} else if waitErr != nil && !timedOut {
		res.Err = waitErr
	}
	return res
}

// OutputTail returns up to n bytes from the end of combined stdout+stderr, for
// surfacing in an RPC-level gate-failure error without dumping the whole capture.
func (r Result) OutputTail(n int) string {
	combined := string(r.Stdout) + string(r.Stderr)
	if len(combined) <= n {
		return combined
	}
	return combined[len(combined)-n:]
}

// capBuffer caps captured output at maxCaptured; writes past the cap are silently
// dropped (return (len(p), nil) so the child's write calls never block on a full
// buffer — it just stops being recorded, the process still runs to completion or
// timeout uninterrupted).
type capBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (c *capBuffer) Write(p []byte) (int, error) {
	if c.limit == 0 {
		c.limit = maxCaptured
	}
	remaining := c.limit - c.buf.Len()
	if remaining > 0 {
		if remaining > len(p) {
			remaining = len(p)
		}
		c.buf.Write(p[:remaining])
	}
	return len(p), nil
}

func (c *capBuffer) Bytes() []byte { return c.buf.Bytes() }

// ValidateArgs checks every {placeholder} in args/env/dir against a known set of
// names (system context keys + admin-declared params) — a typo-catching correctness
// check performed at pipeline-registration time, NOT the security boundary (the
// security boundary is simply "no shell involved," enforced unconditionally by Run).
func ValidateArgs(tmpl Template, known map[string]bool) error {
	var unknown []string
	check := func(s string) {
		for _, ph := range Placeholders(s) {
			if !known[ph] {
				unknown = append(unknown, ph)
			}
		}
	}
	for _, a := range tmpl.Args {
		check(a)
	}
	for _, e := range tmpl.Env {
		check(e)
	}
	check(tmpl.Dir)
	if len(unknown) > 0 {
		return fmt.Errorf("unknown placeholder(s) %s (known: %s)", strings.Join(unknown, ", "), knownKeys(known))
	}
	return nil
}

func knownKeys(known map[string]bool) string {
	keys := make([]string, 0, len(known))
	for k := range known {
		keys = append(keys, k)
	}
	return strings.Join(keys, ", ")
}

// EnvFor builds the BREEZE_* context env vars documented in the design: system
// scalars plus BREEZE_PARAM_<NAME> per caller-declared param, plus a BREEZE_CONTEXT_JSON
// escape hatch (populated by the caller, not here, since it needs the full domain
// object which this package deliberately doesn't know about).
func EnvFor(event, actor, pipeline, stage, commit, environment string, params Params) []string {
	env := []string{
		"BREEZE_EVENT=" + event,
		"BREEZE_ACTOR=" + actor,
		"BREEZE_PIPELINE=" + pipeline,
		"BREEZE_STAGE=" + stage,
		"BREEZE_COMMIT_SHA=" + commit,
		"BREEZE_ENVIRONMENT=" + environment,
	}
	for k, v := range params {
		env = append(env, "BREEZE_PARAM_"+strings.ToUpper(k)+"="+v)
	}
	return env
}
