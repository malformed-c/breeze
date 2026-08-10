// Package hook implements breeze's single command-execution primitive. Every stage
// main command, deploy command, and pre-gate/post-action hook runs through Run — this
// is the only exec.CommandContext call site in breeze.
package hook

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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
	// Stdin, when non-empty, is written to the command's standard input and the
	// pipe is then closed, so a filter-shaped command (one that reads to EOF and
	// writes a result) works without needing a temp file. Nothing else in breeze
	// gives a command structured input — argv placeholders can carry a commit sha
	// but not a stage's captured output — which is what a transform needs.
	Stdin []byte
	// Script is an inline program body, as an alternative to Path+Args. It's written
	// to a temp file and run by Interpreter (default /bin/sh, or the script's own
	// "#!" line if it has one and Interpreter is unset), so a transform can be three
	// lines of python or jq written where it's used instead of a checked-in script
	// nobody can see from the pipeline definition.
	//
	// {placeholder} substitution deliberately does NOT apply inside Script — that is
	// what keeps breeze's "no shell ever interprets a param" guarantee true: a
	// commit sha spliced into a shell script body would be exactly the injection
	// argv construction exists to prevent. A script gets its context from stdin
	// (which for a transform is the whole point) and from Env, both of which stay
	// inert data. Braces in the script are left completely alone, so jq's `{commit}`
	// object shorthand and python f-strings mean what they normally mean.
	Script string
	// Interpreter is the argv prefix a Script is appended to, e.g. ["python3"],
	// ["jq", "-rf"], ["awk", "-f"]. Empty means /bin/sh, or direct execution when
	// the script carries a shebang.
	Interpreter []string
	// ResourceLimits, when set, wraps this command's execution in a transient
	// systemd scope so a runaway build/test/deploy can't starve the host or
	// other concurrent work. See ResourceLimits and WrapWithSystemdRun.
	ResourceLimits *ResourceLimits
	// OnStart, when set, is called with the started process's PID as soon as it is
	// running. Callers use it to record which OS process owns a stage, so a daemon
	// that comes back after a crash can tell a runner that died with the machine
	// from one that outlived a hard kill of the daemon.
	OnStart func(pid int)
	// OutputDir, when set, sends the command's stdout and stderr straight to files
	// in that directory instead of into this process's memory. That is what lets a
	// run OUTLIVE the daemon: the default capture hands the child a pipe whose read
	// end belongs to this process image, so the moment a restart re-execs, the child
	// writes into a pipe nobody holds — EPIPE, output lost, and quite possibly a
	// dead child. A file descriptor doesn't care that its parent was replaced.
	//
	// Result still carries the output (read back after the command exits, capped the
	// same way the in-memory path caps it), so callers see no difference in the
	// normal case; the difference only shows up when someone else has to recover the
	// output later.
	OutputDir string
}

// Output file names inside Template.OutputDir. Fixed rather than generated so a
// daemon that restarts — or a human with a shell — can find a run's output knowing
// only the directory.
const (
	StdoutFile = "stdout"
	StderrFile = "stderr"
)

// ResourceLimits bounds a command's cgroup footprint via systemd-run --scope.
// All fields are optional; only limits actually set are passed through as
// systemd unit properties. The string-valued fields follow systemd's own syntax
// (e.g. "200%", "512M", "2G", "infinity"); breeze validates their SHAPE at
// pipeline-registration time (see engine.validateResourceLimits) so a typo fails
// at `breeze apply` rather than halfway through a pipeline run, but it does not
// otherwise reinterpret them.
//
// Two kinds of limit, and the difference matters when the host is shared: a
// CAP (CPUQuota, MemoryMax, TasksMax) applies unconditionally, even on an
// otherwise idle machine, so a build capped at 4 cores leaves 24 idle whether or
// not anything else wants them. A PRIORITY (CPUWeight, IOWeight) only bites
// under actual contention — the command gets everything that's free and yields
// when something else needs it. For "CI must not starve the control plane
// sharing this box" the priority knobs are usually what's wanted, alone or
// alongside a generous cap; for "this build must never exceed what I budgeted"
// the caps are. MemoryHigh sits between: a soft ceiling that throttles and
// reclaims rather than killing, so it degrades instead of failing the way
// MemoryMax's OOM kill does.
type ResourceLimits struct {
	CPUQuota   string // systemd CPUQuota=, e.g. "200%" for 2 cores — a hard cap
	CPUWeight  int    // systemd CPUWeight=, 1-10000 (default 100); 0 = unset — relative share under contention
	MemoryMax  string // systemd MemoryMax=, e.g. "512M", "2G" — hard cap, OOM-kills past it
	MemoryHigh string // systemd MemoryHigh=, same syntax — soft cap: throttle + reclaim, no kill
	TasksMax   int    // systemd TasksMax=; 0 = unset
	IOWeight   int    // systemd IOWeight=, 1-10000; 0 = unset — relative share under contention
	// The IO CAPS, the counterpart to IOWeight in the same way CPUQuota is to
	// CPUWeight: a weight only decides who yields under contention and costs
	// nothing on an idle disk, a cap applies always. Each takes systemd's own
	// device-qualified syntax — "PATH VALUE", where PATH is a block device node
	// or any file whose backing device systemd resolves ("/var/lib 50M" is as
	// valid as "/dev/sda 50M"). Empty = unset.
	//
	// Read IMPORTANT: on a typical desktop/server the io controller is NOT
	// delegated to the per-user systemd manager, so every one of these — and
	// IOWeight, which shipped before them — is accepted, reported back by
	// `systemctl show` as if in force, and silently does nothing. See
	// IOControllerAvailable.
	IOReadBandwidthMax  string // systemd IOReadBandwidthMax=, e.g. "/dev/sda 50M"
	IOWriteBandwidthMax string // systemd IOWriteBandwidthMax=
	IOReadIOPSMax       string // systemd IOReadIOPSMax=, e.g. "/dev/sda 1000"
	IOWriteIOPSMax      string // systemd IOWriteIOPSMax=
}

// ioProperties pairs each IO-cap field with its systemd property name, so the
// wrapper, the validator and the "is any IO limit set" check cannot drift apart by
// someone adding a fifth field to one of them.
func (rl *ResourceLimits) ioProperties() []struct{ Property, Value string } {
	if rl == nil {
		return nil
	}
	return []struct{ Property, Value string }{
		{"IOReadBandwidthMax", rl.IOReadBandwidthMax},
		{"IOWriteBandwidthMax", rl.IOWriteBandwidthMax},
		{"IOReadIOPSMax", rl.IOReadIOPSMax},
		{"IOWriteIOPSMax", rl.IOWriteIOPSMax},
	}
}

// UsesIO reports whether rl asks for anything the io cgroup controller has to
// provide — the caps above or the older IOWeight. Callers use it to decide whether
// IOControllerAvailable's answer is worth telling anyone about: on a host with no
// io limits configured, an undelegated io controller is not a problem.
func (rl *ResourceLimits) UsesIO() bool {
	if rl == nil {
		return false
	}
	if rl.IOWeight > 0 {
		return true
	}
	for _, p := range rl.ioProperties() {
		if p.Value != "" {
			return true
		}
	}
	return false
}

// IsZero reports whether no limit at all is set — used to decide whether a
// command needs the systemd-run wrapper, so an all-empty block behaves exactly
// like no block.
func (rl *ResourceLimits) IsZero() bool {
	return rl == nil || (rl.CPUQuota == "" && rl.CPUWeight == 0 && rl.MemoryMax == "" &&
		rl.MemoryHigh == "" && rl.TasksMax == 0 && !rl.UsesIO())
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
	if rl.CPUWeight > 0 {
		sdArgs = append(sdArgs, fmt.Sprintf("--property=CPUWeight=%d", rl.CPUWeight))
	}
	if rl.MemoryMax != "" {
		sdArgs = append(sdArgs, "--property=MemoryMax="+rl.MemoryMax)
	}
	if rl.MemoryHigh != "" {
		sdArgs = append(sdArgs, "--property=MemoryHigh="+rl.MemoryHigh)
	}
	if rl.TasksMax > 0 {
		sdArgs = append(sdArgs, fmt.Sprintf("--property=TasksMax=%d", rl.TasksMax))
	}
	if rl.IOWeight > 0 {
		sdArgs = append(sdArgs, fmt.Sprintf("--property=IOWeight=%d", rl.IOWeight))
	}
	for _, p := range rl.ioProperties() {
		if p.Value != "" {
			sdArgs = append(sdArgs, "--property="+p.Property+"="+p.Value)
		}
	}
	sdArgs = append(sdArgs, "--", path)
	sdArgs = append(sdArgs, args...)
	return "systemd-run", sdArgs
}

// writeScript materializes an inline Script as a private temp file. 0700 and a
// per-run file rather than a stable path: two concurrent stages must never share
// (or race to overwrite) one script, and nothing but this user should be able to
// read — or, worse, replace — the body between writing it and executing it.
func writeScript(body string) (path string, cleanup func(), err error) {
	f, err := os.CreateTemp("", "breeze-script-*")
	if err != nil {
		return "", func() {}, fmt.Errorf("writing inline script: %w", err)
	}
	cleanup = func() { os.Remove(f.Name()) }
	if err := f.Chmod(0o700); err != nil {
		f.Close()
		cleanup()
		return "", func() {}, fmt.Errorf("writing inline script: %w", err)
	}
	if _, err := f.WriteString(body); err != nil {
		f.Close()
		cleanup()
		return "", func() {}, fmt.Errorf("writing inline script: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("writing inline script: %w", err)
	}
	return f.Name(), cleanup, nil
}

// scriptArgv decides how to invoke a materialized script: an explicit Interpreter
// wins, then a shebang (executed directly, which is what the author asked for by
// writing one), then /bin/sh as the least surprising default.
func scriptArgv(tmpl Template, scriptPath string) (string, []string) {
	if len(tmpl.Interpreter) > 0 {
		return tmpl.Interpreter[0], append(append([]string(nil), tmpl.Interpreter[1:]...), scriptPath)
	}
	if strings.HasPrefix(tmpl.Script, "#!") {
		return scriptPath, nil
	}
	return "/bin/sh", []string{scriptPath}
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
	if tmpl.Script != "" {
		scriptPath, cleanup, err := writeScript(tmpl.Script)
		if err != nil {
			return Result{Err: err}
		}
		defer cleanup()
		path, args = scriptArgv(tmpl, scriptPath)
	}
	if !tmpl.ResourceLimits.IsZero() {
		path, args = WrapWithSystemdRun(path, args, tmpl.ResourceLimits)
	}

	cmd := exec.CommandContext(ctx, path, args...)
	if len(tmpl.Stdin) > 0 {
		cmd.Stdin = bytes.NewReader(tmpl.Stdin)
	}
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
	var outFiles *outputFiles
	if tmpl.OutputDir != "" {
		of, err := openOutputFiles(tmpl.OutputDir)
		if err != nil {
			return Result{Err: err}
		}
		defer of.close()
		outFiles = of
		cmd.Stdout, cmd.Stderr = of.stdout, of.stderr
	} else {
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
	}

	start := time.Now()
	err := cmd.Start()
	if err != nil {
		return Result{Err: err, Duration: time.Since(start)}
	}
	if tmpl.OnStart != nil && cmd.Process != nil {
		tmpl.OnStart(cmd.Process.Pid)
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
	if outFiles != nil {
		res.Stdout, res.Stderr = outFiles.read()
	}
	if exitErr, ok := waitErr.(*exec.ExitError); ok {
		res.ExitCode = exitErr.ExitCode()
	} else if waitErr != nil && !timedOut {
		res.Err = waitErr
	}
	return res
}

// outputFiles holds the two files a run writes directly into. The child gets these
// descriptors as its own stdout/stderr, so nothing in this process sits between the
// command and its output — which is the whole point: there is no pipe to break when
// this process is replaced.
type outputFiles struct{ stdout, stderr *os.File }

func openOutputFiles(dir string) (*outputFiles, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("run output dir: %w", err)
	}
	out, err := os.Create(filepath.Join(dir, StdoutFile))
	if err != nil {
		return nil, fmt.Errorf("run output: %w", err)
	}
	errf, err := os.Create(filepath.Join(dir, StderrFile))
	if err != nil {
		out.Close()
		return nil, fmt.Errorf("run output: %w", err)
	}
	return &outputFiles{stdout: out, stderr: errf}, nil
}

func (o *outputFiles) close() {
	o.stdout.Close()
	o.stderr.Close()
}

func (o *outputFiles) read() ([]byte, []byte) {
	return ReadCapped(o.stdout.Name()), ReadCapped(o.stderr.Name())
}

// ReadCapped reads at most maxCaptured bytes from path, matching the in-memory
// capture's cap so a run's recorded output is the same size either way. Exported
// because recovering a run's output after a restart happens outside this package.
func ReadCapped(path string) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	buf := make([]byte, maxCaptured)
	n, _ := io.ReadFull(f, buf)
	return buf[:n]
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

// IOControllerAvailable reports whether the io cgroup controller is actually
// usable for the scopes this process creates, and if not, why.
//
// This exists because the failure it detects is invisible. On a typical
// desktop/server the io controller is NOT delegated to the per-user systemd
// manager — `user@.service` gets `cpu memory pids` and nothing else — so an IO
// limit set through `systemd-run --user` is accepted, exits 0, is echoed back by
// `systemctl show` as if in force, and does nothing at all. Measured on the
// machine this was written for:
//
//	memory.max  536870912        <- MemoryMax applied
//	io.max      (no such file)   <- the io controller is not in the cgroup
//	systemctl show ... IOReadBandwidthMax=/ 10000000   <- reported anyway
//
// So every check that would normally catch a bad limit passes. Only reading the
// cgroup itself tells the truth, which is what this does.
//
// A controller cannot appear in a child that its parent does not have, so the
// controllers available at our own cgroup bound what any scope we create can get.
// Running as root is the exception worth handling: those scopes go to the SYSTEM
// manager, where io is normally present even though this process's own cgroup is a
// user scope — so root's answer comes from the root cgroup instead.
func IOControllerAvailable() (bool, string) {
	path := "/sys/fs/cgroup/cgroup.controllers"
	where := "the system manager's root cgroup"
	if os.Geteuid() != 0 {
		own, err := ownCgroupDir()
		if err != nil {
			return false, "could not determine this process's cgroup (" + err.Error() + "), so whether io limits apply is unknown"
		}
		path = filepath.Join(own, "cgroup.controllers")
		where = "this daemon's own cgroup"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false, "could not read " + path + " (" + err.Error() + "), so whether io limits apply is unknown"
	}
	for _, c := range strings.Fields(string(data)) {
		if c == "io" {
			return true, ""
		}
	}
	return false, "the io cgroup controller is not available in " + where + " (" + path +
		" lists: " + strings.Join(strings.Fields(string(data)), " ") + ")"
}

// ownCgroupDir resolves this process's cgroup v2 directory under /sys/fs/cgroup.
// Only the unified hierarchy is handled: the "0::" line. On a cgroup v1 host there
// is no such line and this reports an error, which the caller renders as "unknown"
// rather than as "unavailable" — a thing breeze cannot determine must not be
// announced as a thing breeze has determined.
func ownCgroupDir() (string, error) {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(line, "0::"); ok {
			return filepath.Join("/sys/fs/cgroup", rest), nil
		}
	}
	return "", fmt.Errorf("no unified (0::) entry in /proc/self/cgroup — cgroup v2 is required for resource limits")
}
