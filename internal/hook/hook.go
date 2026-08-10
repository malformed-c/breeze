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
	"slices"
	"strconv"
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
	// UnsetEnv names variables to REMOVE from the inherited environment, before Env
	// is applied. Not setting a variable is not the same as unsetting it: the child
	// inherits the parent's, and a daemon that itself runs under breeze passes its
	// own BREEZE_RUN_DIR straight through — so a stage with no scratch directory
	// received a path belonging to somebody else's run. Found only because breeze
	// runs its own test suite as a breeze stage.
	UnsetEnv []string
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
	// Nice is the CPU scheduling niceness, -20 (most favourable) to 19 (least),
	// applied via nice(1) rather than a systemd property: Nice= is an EXEC property
	// and a scope unit rejects it outright ("Unknown assignment: Nice=10", exit 1),
	// because a scope adopts processes someone else started. nice(1) composes
	// cleanly inside the scope and — unlike the cgroup knobs — is inherited by every
	// grandchild, which is what makes it useful for a build that forks compilers.
	//
	// A POINTER so that 0 is expressible. nice = 0 is a meaningful value (normal
	// priority) and distinct from "unset": with a plain int, a stage that wanted to
	// undo a machine-wide nice = 10 would write nice = 0 and silently inherit 10.
	//
	// A NEGATIVE value needs privilege. A non-root nice(1) asked for -5 prints
	// "cannot set niceness: Permission denied", exits 0, and runs at 0 — accepted,
	// ineffective, and reported as success, exactly like the io controller. See
	// NicenessApplicable.
	Nice *int
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

// IsZero reports whether no limit at all is set, including niceness — an all-empty
// block behaves exactly like no block.
func (rl *ResourceLimits) IsZero() bool {
	return rl == nil || (rl.CPUQuota == "" && rl.CPUWeight == 0 && rl.MemoryMax == "" &&
		rl.MemoryHigh == "" && rl.TasksMax == 0 && rl.Nice == nil && !rl.UsesIO())
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

// filterEnv drops the named variables from env. Removing an inherited variable is
// the only way to express "this does not exist" — appending nothing leaves whatever
// the parent had, which is confidently wrong rather than absent.
func filterEnv(env, unset []string) []string {
	if len(unset) == 0 {
		return env
	}
	out := env[:0:0]
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(unset, name) {
			out = append(out, kv)
		}
	}
	return out
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
	if tmpl.ResourceLimits != nil {
		// Nice first, so the niced process is INSIDE the scope rather than the
		// scope being started by a niced systemd-run.
		path, args = WrapWithNice(path, args, tmpl.ResourceLimits.Nice)
	}
	if tmpl.ResourceLimits.needsCgroup() {
		path, args = WrapWithSystemdRun(path, args, tmpl.ResourceLimits)
	}

	cmd := exec.CommandContext(ctx, path, args...)
	if len(tmpl.Stdin) > 0 {
		cmd.Stdin = bytes.NewReader(tmpl.Stdin)
	}
	cmd.Dir = Substitute(tmpl.Dir, params)
	cmd.Env = filterEnv(os.Environ(), tmpl.UnsetEnv)
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
		// Cgroup first: a script that uses job control puts its children in process
		// groups the group kill below cannot reach, and cannot move them out of the
		// cgroup. Falls back when there is no scope of our own to kill (an
		// unlimited stage shares the daemon's cgroup, where killing everything
		// would take the daemon with it).
		if KillByCgroup(cmd.Process.Pid) {
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
		// Belt-and-suspenders: cmd.Cancel above already did this on timeout, but a
		// second kill of a possibly-already-reaped group is harmless, and this
		// covers the case where Wait raced ahead of Cancel's goroutine.
		if !KillByCgroup(cmd.Process.Pid) {
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
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

// needsCgroup reports whether anything here requires the systemd-run --scope
// wrapper. Deliberately NOT the same question as IsZero: niceness is applied by
// nice(1) and needs no cgroup at all, so a nice-only block must not drag in a
// transient scope unit — which would be pure overhead at best, and on a host with
// no usable per-user systemd session would turn a working stage into a failing one.
func (rl *ResourceLimits) needsCgroup() bool {
	if rl == nil {
		return false
	}
	return rl.CPUQuota != "" || rl.CPUWeight != 0 || rl.MemoryMax != "" ||
		rl.MemoryHigh != "" || rl.TasksMax != 0 || rl.UsesIO()
}

// WrapWithNice prepends nice(1) so the command — and everything it forks — runs at
// the requested scheduling priority. Applied BEFORE the systemd-run wrapper so the
// niced process ends up inside the scope rather than outside it.
//
// nice(1) rather than systemd's Nice= because a scope unit rejects that property
// outright: `systemd-run --scope --property=Nice=10` fails with "Unknown
// assignment: Nice=10" and exit 1. Measured, not assumed.
//
// "--" separates nice's options from the command, so a command path that begins
// with a dash cannot be read as a flag.
func WrapWithNice(path string, args []string, nice *int) (string, []string) {
	if nice == nil {
		return path, args
	}
	out := append([]string{"-n", strconv.Itoa(*nice), "--", path}, args...)
	return "nice", out
}

// NicenessApplicable reports whether the requested niceness can actually take
// effect, and if not, why. Lowering niceness (a negative value, i.e. asking for MORE
// CPU) requires privilege; a non-root caller gets a warning on stderr, an exit
// status of 0, and a process running at its original priority.
//
// Same shape as the io controller: accepted, ineffective, reported as success. The
// difference is worth stating precisely — a POSITIVE nice always works, so this is
// only ever a problem for the one direction that asks for more resources rather
// than fewer.
func NicenessApplicable(nice *int) (bool, string) {
	if nice == nil || *nice >= 0 {
		return true, ""
	}
	if os.Geteuid() == 0 {
		return true, ""
	}
	return false, fmt.Sprintf("nice = %d asks for HIGHER priority than default, which requires privilege — "+
		"a non-root daemon's nice(1) reports \"cannot set niceness: Permission denied\", exits 0, and runs at the original priority. "+
		"Positive values (lower priority) work without privilege", *nice)
}

// KillByCgroup kills every process in pid's cgroup, returning false if it declined.
//
// Why this exists, measured rather than theorised: a stage that timed out left five
// linkers running twenty minutes later, in FIVE DISTINCT PROCESS GROUPS, none of
// them the runner's — the script had `set -m`, and job control gives every
// background job its own process group, so the very option added to make a build
// killable as a tree is what exempted it from a process-group kill. Every survivor
// was still inside the stage's own scope cgroup.
//
// That is the general property and it decides the approach: a stage script CAN move
// its children out of the process group it was started in, and it CANNOT move them
// out of the cgroup. Killing by process group depends on the script's cooperation
// and fails silently when it does not cooperate; killing by cgroup does not.
// (Diagnosed by platform, who produced the pgid measurement that showed my
// group-kill could never have reached them.)
//
// THE GUARDS ARE THE WHOLE RISK HERE. Writing cgroup.kill to the wrong cgroup kills
// the daemon, or the user's whole session. So this refuses unless the target is a
// transient .scope, is not our own cgroup, and is not an ANCESTOR of our own — that
// last one is the dangerous case, because an ancestor contains us and looks like an
// ordinary different path. A stage running without resource limits shares the
// daemon's cgroup and is correctly declined here, falling back to the group kill.
func KillByCgroup(pid int) bool {
	own, err := ownCgroupDir()
	if err != nil {
		return false
	}
	theirs, err := cgroupDirOf(pid)
	if err != nil || theirs == "" {
		return false
	}
	// Only ever a transient scope: that is what systemd-run --scope creates for us,
	// and it is the only shape we have any business killing wholesale.
	if !strings.HasSuffix(theirs, ".scope") {
		return false
	}
	// Not us, and not anything containing us.
	if theirs == own || strings.HasPrefix(own, theirs+"/") {
		return false
	}
	return os.WriteFile(filepath.Join(theirs, "cgroup.kill"), []byte("1"), 0) == nil
}

// cgroupDirOf resolves pid's cgroup v2 directory, or an error if it has no unified
// entry (cgroup v1, or the process is gone).
func cgroupDirOf(pid int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(line, "0::"); ok {
			return filepath.Join("/sys/fs/cgroup", strings.TrimSpace(rest)), nil
		}
	}
	return "", fmt.Errorf("no unified (0::) cgroup entry for pid %d", pid)
}

// CgroupStats reports what a running command's own cgroup knows about its memory:
// the high-water mark, and how many times it was THROTTLED against memory_high.
// ok is false when the process has no scope of its own (an unlimited stage shares
// the daemon's cgroup, whose numbers say nothing about the stage) or the kernel
// does not expose these files.
//
// This exists because memory_high degrades instead of failing. A stage 3 GB over a
// 4 GB soft ceiling does not error — it throttles and reclaims, and the symptom is a
// run that takes 25 minutes and produces nothing, which reads as a slow build rather
// than as a limit. The kernel counts every one of those throttling events in
// memory.events, and nobody knew to look: two people asked for this on the same
// evening, in the same words — the counter exists, and "high is non-zero" is a
// one-word answer to "why is this taking so long".
func CgroupStats(pid int) (peak, highEvents uint64, ok bool) {
	dir, err := cgroupDirOf(pid)
	if err != nil || !strings.HasSuffix(dir, ".scope") {
		return 0, 0, false
	}
	if own, err := ownCgroupDir(); err == nil && (dir == own || strings.HasPrefix(own, dir+"/")) {
		return 0, 0, false // the daemon's own cgroup: its numbers are not this stage's
	}
	peak = readCgroupUint(filepath.Join(dir, "memory.peak"))
	if data, err := os.ReadFile(filepath.Join(dir, "memory.events")); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if v, found := strings.CutPrefix(line, "high "); found {
				highEvents, _ = strconv.ParseUint(strings.TrimSpace(v), 10, 64)
			}
		}
	}
	return peak, highEvents, true
}

func readCgroupUint(path string) uint64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	v, _ := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	return v
}

// ParseSize converts a systemd byte size ("512M", "2G", "1.5Gi", "8000") into bytes.
// ok is false for "infinity", an empty string, or anything it cannot parse — callers
// must then export NOTHING rather than a guess, because a wrong integer is worse
// than an absent one.
//
// This exists so breeze can hand a stage the number instead of the notation. A
// script that needs to divide a memory ceiling by a per-build figure was doing shell
// arithmetic on "16G" and getting 16, which produced "7 concurrent builds x 2
// threads" from a 16 GB budget — and it was caught only because that script printed
// its derived value next to its inputs, where 16/7 being 2 was visible. One parser
// here beats N consumers each getting the suffix right; the first of them did not.
func ParseSize(s string) (uint64, bool) {
	s = strings.TrimSpace(s)
	if s == "" || s == "infinity" {
		return 0, false
	}
	// Strip systemd's optional B / iB tail, remembering that "i" means binary.
	unit := uint64(1)
	body := s
	for suffix, mult := range map[string]uint64{
		"K": 1 << 10, "M": 1 << 20, "G": 1 << 30, "T": 1 << 40, "P": 1 << 50, "E": 1 << 60,
	} {
		for _, tail := range []string{suffix, suffix + "B", suffix + "iB"} {
			if rest, found := strings.CutSuffix(body, tail); found && rest != "" {
				unit, body = mult, rest
				goto parsed
			}
		}
	}
	body = strings.TrimSuffix(body, "B")
parsed:
	f, err := strconv.ParseFloat(strings.TrimSpace(body), 64)
	if err != nil || f < 0 {
		return 0, false
	}
	return uint64(f * float64(unit)), true
}

// ParsePercent converts a systemd CPU quota ("1400%", "12.5%") into whole percent.
// Same contract as ParseSize: no guess when it cannot parse. A script sizing its own
// parallelism has to do arithmetic on this, and "1400%" is not a number in shell.
func ParsePercent(s string) (uint64, bool) {
	s = strings.TrimSpace(s)
	if s == "" || s == "infinity" {
		return 0, false
	}
	body, found := strings.CutSuffix(s, "%")
	if !found {
		return 0, false
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(body), 64)
	if err != nil || f < 0 {
		return 0, false
	}
	return uint64(f), true
}
