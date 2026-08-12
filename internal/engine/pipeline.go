package engine

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"breeze/internal/hook"
)

// knownParams is the fixed system placeholder set every {name} in a command/hook
// template is validated against at registration time (plus nothing else — breeze's
// finalized data model has no separate per-stage "declared params" list; every
// context key a command might need is one of these system fields).
var knownParams = map[string]bool{
	"commit": true, "environment": true, "pipeline": true,
	"stage": true, "target": true, "actor": true,
}

// RegisterPipeline validates and stores p, an idempotent upsert by name (re-registering
// an existing pipeline name replaces its definition — this is what `breeze apply`
// relies on for its diff-and-upsert behavior).
func (e *Engine) RegisterPipeline(p Pipeline, createdBy string) error {
	if err := validatePipeline(&p); err != nil {
		return err
	}
	p.CreatedBy = createdBy
	p.CreatedAt = e.now()

	e.mu.Lock()
	defer e.mu.Unlock()
	e.pipelines[p.Name] = &p
	e.changed()
	return nil
}

func validatePipeline(p *Pipeline) error {
	if p.Name == "" {
		return fmt.Errorf("pipeline name required")
	}
	if len(p.Stages) == 0 {
		return fmt.Errorf("pipeline %q: at least one stage required", p.Name)
	}

	seen := make(map[string]int, len(p.Stages))
	for i, s := range p.Stages {
		if s.Name == "" {
			return fmt.Errorf("pipeline %q: stage with empty name", p.Name)
		}
		if _, dup := seen[s.Name]; dup {
			return fmt.Errorf("pipeline %q: duplicate stage name %q", p.Name, s.Name)
		}
		seen[s.Name] = i
	}
	if err := validateStageNeeds(p, seen); err != nil {
		return err
	}

	if p.FanOutAt < 0 || p.FanOutAt > len(p.Stages) {
		return fmt.Errorf("pipeline %q: fanOutAt %d out of range [0,%d]", p.Name, p.FanOutAt, len(p.Stages))
	}
	fansOut := p.FanOutAt < len(p.Stages)
	if fansOut && len(p.Environments) == 0 {
		return fmt.Errorf("pipeline %q: fanOutAt < len(stages) requires at least one environment", p.Name)
	}
	if !fansOut && len(p.Environments) > 0 {
		return fmt.Errorf("pipeline %q: environments declared but fanOutAt has no fan-out point", p.Name)
	}
	for _, de := range p.DebugEnvironments {
		if !slices.Contains(p.Environments, de) {
			return fmt.Errorf("pipeline %q: debugEnvironments references undeclared environment %q", p.Name, de)
		}
	}
	for env := range p.EnvironmentOwners {
		if !slices.Contains(p.Environments, env) {
			return fmt.Errorf("pipeline %q: environmentOwners references undeclared environment %q", p.Name, env)
		}
	}

	for i, s := range p.Stages {
		switch s.Type {
		case StageCommand:
			if s.CommandPolicy == nil {
				return fmt.Errorf("pipeline %q stage %q: command stage requires CommandPolicy", p.Name, s.Name)
			}
		case StageApproval:
			if s.ApprovalPolicy == nil {
				return fmt.Errorf("pipeline %q stage %q: approval stage requires ApprovalPolicy", p.Name, s.Name)
			}
			// An approval executes nothing, so there is no work to serialize and the
			// requirement would silently never be enforced. Refused here rather than
			// ignored at run time: a lock requirement that quietly does nothing is
			// worse than no lock requirement, because the config says it is protected.
			if s.RequiresLock != "" {
				return fmt.Errorf("pipeline %q stage %q: requires_lock is not meaningful on an approval stage — approving runs no command, so there is nothing to serialize; put it on the command or deploy stage that does the work", p.Name, s.Name)
			}
			// Same shape: an approval runs no command, so a declared value would have
			// nothing to reach and the gate would be pure ceremony — while the config
			// reads as though the declaration is enforced somewhere that matters.
			if len(s.RequiresEnv) > 0 {
				return fmt.Errorf("pipeline %q stage %q: requires_env is not meaningful on an approval stage — approving runs no command, so the value would reach nothing; put it on the command or deploy stage that does the work", p.Name, s.Name)
			}
			if s.ApprovalPolicy.RequiredApprovals < 1 {
				return fmt.Errorf("pipeline %q stage %q: requiredApprovals must be >= 1", p.Name, s.Name)
			}
		case StageDeploy:
			if s.DeployPolicy == nil {
				return fmt.Errorf("pipeline %q stage %q: deploy stage requires DeployPolicy", p.Name, s.Name)
			}
			// Deploy is inherently environment-keyed — rejected at registration if
			// placed before the fan-out point, not discovered at run time.
			if i < p.FanOutAt {
				return fmt.Errorf("pipeline %q stage %q: deploy-type stage must be at index >= fanOutAt (%d), got %d", p.Name, s.Name, p.FanOutAt, i)
			}
		default:
			return fmt.Errorf("pipeline %q stage %q: unknown stage type %q", p.Name, s.Name, s.Type)
		}

		if s.Type == StageCommand || s.Type == StageDeploy {
			if s.Timeout <= 0 {
				return fmt.Errorf("pipeline %q stage %q: timeout required", p.Name, s.Name)
			}
			if err := validateTemplatePlaceholders(s.Command); err != nil {
				return fmt.Errorf("pipeline %q stage %q: %w", p.Name, s.Name, err)
			}
		}
		for _, h := range s.PreGate {
			if err := validateHook(h); err != nil {
				return fmt.Errorf("pipeline %q stage %q preGate: %w", p.Name, s.Name, err)
			}
		}
		for _, h := range s.PostAction {
			if err := validateHook(h); err != nil {
				return fmt.Errorf("pipeline %q stage %q postAction: %w", p.Name, s.Name, err)
			}
		}
	}

	// mess reserves "/" for addressing — "room/topic" and "room/agent" — so a topic
	// containing one is now rejected by mess itself. Caught at registration rather
	// than at notification time: a bad topic would otherwise surface only as
	// notifications that never arrive, and "the notifier is broken" is a much harder
	// thing to trace back to a config typo than "breeze apply refused this".
	for _, t := range []struct{ field, value string }{
		{"notify_topic", p.NotifyTopic},
		{"command_topic", p.CommandTopic},
	} {
		if strings.Contains(t.value, "/") {
			return fmt.Errorf("pipeline %q: %s %q must not contain %q — mess reserves it for addressing a room (\"room/topic\"), and would reject the topic", p.Name, t.field, t.value, "/")
		}
	}

	if err := validateEnvironmentDeps(p.Environments, p.EnvironmentDeps); err != nil {
		return fmt.Errorf("pipeline %q: %w", p.Name, err)
	}

	return nil
}

// validateStageNeeds checks the stage graph at registration time: every name in a
// stage's Needs must be a declared stage, and must be declared EARLIER than the
// stage needing it. That single backward-reference rule is what makes the graph
// acyclic by construction — so unlike EnvironmentDeps there is no cycle DFS here,
// and no cyclic config can ever reach a run-time gate check. index maps stage name
// to declaration index (built by the caller's duplicate-name pass).
func validateStageNeeds(p *Pipeline, index map[string]int) error {
	for i, s := range p.Stages {
		switch s.Convergence {
		case "", ConvergeAll, ConvergeAny:
		default:
			return fmt.Errorf("pipeline %q stage %q: unknown convergence %q (must be %q or %q)", p.Name, s.Name, s.Convergence, ConvergeAll, ConvergeAny)
		}
		seenNeed := make(map[string]bool, len(s.Needs))
		for _, name := range s.Needs {
			j, ok := index[name]
			if !ok {
				return fmt.Errorf("pipeline %q stage %q: needs unknown stage %q", p.Name, s.Name, name)
			}
			if j == i {
				return fmt.Errorf("pipeline %q stage %q: cannot need itself", p.Name, s.Name)
			}
			if j > i {
				return fmt.Errorf("pipeline %q stage %q: needs %q, which is declared later — a stage may only need stages declared before it", p.Name, s.Name, name)
			}
			if seenNeed[name] {
				return fmt.Errorf("pipeline %q stage %q: duplicate need %q", p.Name, s.Name, name)
			}
			seenNeed[name] = true
		}
	}
	return nil
}

func validateHook(h Hook) error {
	if h.Timeout <= 0 {
		return fmt.Errorf("hook timeout required")
	}
	return validateTemplatePlaceholders(h.Command)
}

func validateTemplatePlaceholders(tmpl CommandTemplate) error {
	switch {
	case tmpl.Path == "" && tmpl.Script == "":
		return fmt.Errorf("command required: give either command = [...] or an inline script")
	case tmpl.Path != "" && tmpl.Script != "":
		return fmt.Errorf("command and script are mutually exclusive — a stage runs one or the other, and accepting both would silently ignore one of them")
	case tmpl.Script != "" && len(tmpl.Args) > 0:
		return fmt.Errorf("args cannot be combined with an inline script (a script's input is stdin, not argv); use interpreter = [...] to control how it's invoked")
	}
	if err := validateResourceLimits(tmpl.ResourceLimits); err != nil {
		return err
	}
	return hook.ValidateArgs(hook.Template{
		Path: tmpl.Path, Args: tmpl.Args, Env: tmpl.Env, Dir: tmpl.Dir,
	}, knownParams)
}

// cpuQuotaRe and memorySizeRe pin the SHAPE of systemd's own syntax for the
// string-valued limits: a percentage for CPUQuota ("200%", "12.5%"), and a byte
// count with an optional K/M/G/T/P/E suffix (systemd accepts a bare or "B"/"iB"
// -suffixed form of each) for the memory limits. "infinity" is systemd's explicit
// no-limit value and is accepted for all three.
var (
	cpuQuotaRe   = regexp.MustCompile(`^\d+(\.\d+)?%$`)
	memorySizeRe = regexp.MustCompile(`^\d+(\.\d+)?([KMGTPE](i?B)?|B)?$`)
	ioPSRe       = regexp.MustCompile(`^\d+$`)
)

// validateResourceLimits rejects a malformed limit at registration time rather
// than letting it surface as a systemd-run error partway through a pipeline run.
// This is a shape check, not a semantic one — breeze still doesn't reinterpret
// what the values MEAN, it just refuses the typos that would otherwise only be
// discovered by a stage failing in a way that looks like the build broke:
// `cpu_quota = "1400"` (missing %), `memory_max = "8GB "`, `memory_max = "8 G"`.
// Registration is the right place because it's the moment an admin is looking at
// the config and can fix it, and because breeze's whole premise is that a gate
// rejection should be legible before anything runs.
func validateResourceLimits(rl *hook.ResourceLimits) error {
	if rl == nil {
		return nil
	}
	if rl.CPUQuota != "" && rl.CPUQuota != "infinity" && !cpuQuotaRe.MatchString(rl.CPUQuota) {
		return fmt.Errorf("resource_limits: cpu_quota %q must be a percentage like \"200%%\" (200%% = 2 cores) or \"infinity\"", rl.CPUQuota)
	}
	if rl.CPUWeight != 0 && (rl.CPUWeight < 1 || rl.CPUWeight > 10000) {
		return fmt.Errorf("resource_limits: cpu_weight must be between 1 and 10000 (systemd's default is 100)")
	}
	for _, m := range []struct{ name, value string }{
		{"memory_max", rl.MemoryMax},
		{"memory_high", rl.MemoryHigh},
	} {
		if m.value != "" && m.value != "infinity" && !memorySizeRe.MatchString(m.value) {
			return fmt.Errorf("resource_limits: %s %q must be a byte count like \"512M\", \"2G\" or \"infinity\"", m.name, m.value)
		}
	}
	if rl.IOWeight != 0 && (rl.IOWeight < 1 || rl.IOWeight > 10000) {
		return fmt.Errorf("resource_limits: io_weight must be between 1 and 10000")
	}
	// The IO caps take systemd's device-qualified form, "PATH VALUE" — a shape that
	// is easy to get wrong in a way that reads fine ("50M" alone, or "/dev/sda,50M"),
	// and whose failure is otherwise invisible: an unparseable property makes the
	// scope fail to start partway through a run, and on a host without the io
	// controller it does not even do that.
	for _, m := range []struct {
		name, value string
		iops        bool
	}{
		{"io_read_bandwidth_max", rl.IOReadBandwidthMax, false},
		{"io_write_bandwidth_max", rl.IOWriteBandwidthMax, false},
		{"io_read_iops_max", rl.IOReadIOPSMax, true},
		{"io_write_iops_max", rl.IOWriteIOPSMax, true},
	} {
		if err := validateIOLimit(m.name, m.value, m.iops); err != nil {
			return err
		}
	}
	if rl.Nice != nil && (*rl.Nice < -20 || *rl.Nice > 19) {
		return fmt.Errorf("resource_limits: nice must be between -20 (most favourable) and 19 (least), got %d", *rl.Nice)
	}
	if rl.TasksMax < 0 {
		return fmt.Errorf("resource_limits: tasks_max must be >= 0")
	}
	return nil
}

// validateIOLimit checks one device-qualified IO cap. systemd wants a path and a
// value separated by whitespace, where the path may be a block device node or any
// file whose backing device systemd resolves ("/var/lib 50M" is as valid as
// "/dev/sda 50M"). Shape only, as everywhere else here: breeze does not go looking
// for the device or decide whether the number is sensible.
func validateIOLimit(name, value string, iops bool) error {
	if value == "" {
		return nil
	}
	fields := strings.Fields(value)
	if len(fields) != 2 {
		return fmt.Errorf("resource_limits: %s %q must be a device and a value separated by a space, e.g. %q — systemd applies an IO limit per device, so a bare value has nothing to apply to",
			name, value, ioLimitExample(name, iops))
	}
	if !strings.HasPrefix(fields[0], "/") {
		return fmt.Errorf("resource_limits: %s %q: %q is not a path — give a block device (\"/dev/sda\") or any file on the device you mean (\"/var/lib\"), which systemd resolves to its backing device",
			name, value, fields[0])
	}
	if fields[1] == "max" || fields[1] == "infinity" {
		return nil
	}
	if iops {
		if !ioPSRe.MatchString(fields[1]) {
			return fmt.Errorf("resource_limits: %s %q: %q must be a whole number of operations per second, or \"max\"", name, value, fields[1])
		}
		return nil
	}
	if !memorySizeRe.MatchString(fields[1]) {
		return fmt.Errorf("resource_limits: %s %q: %q must be a bytes-per-second rate like \"50M\", or \"max\"", name, value, fields[1])
	}
	return nil
}

func ioLimitExample(name string, iops bool) string {
	if iops {
		return "/dev/sda 1000"
	}
	return "/dev/sda 50M"
}

func (e *Engine) Pipeline(name string) (*Pipeline, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	p, ok := e.pipelines[name]
	if !ok {
		return nil, false
	}
	cp := *p
	return &cp, true
}

func (e *Engine) Pipelines() []Pipeline {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Pipeline, 0, len(e.pipelines))
	for _, p := range e.pipelines {
		out = append(out, *p)
	}
	slices.SortFunc(out, func(a, b Pipeline) int {
		if a.Name < b.Name {
			return -1
		}
		if a.Name > b.Name {
			return 1
		}
		return 0
	})
	return out
}
