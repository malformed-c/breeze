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

	cmd := exec.CommandContext(ctx, tmpl.Path, args...)
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
