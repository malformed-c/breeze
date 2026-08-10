package engine

import (
	"strings"
	"testing"

	"breeze/internal/hook"
)

// A malformed limit must be refused at registration, not discovered by a stage
// failing mid-pipeline in a way that looks like the build itself broke. The
// missing-% case is the one an operator actually writes.
func TestValidateResourceLimitsRejectsMalformed(t *testing.T) {
	bad := []struct {
		name string
		rl   hook.ResourceLimits
		want string
	}{
		{"cpu_quota without %", hook.ResourceLimits{CPUQuota: "1400"}, "cpu_quota"},
		{"cpu_quota with units", hook.ResourceLimits{CPUQuota: "14 cores"}, "cpu_quota"},
		{"cpu_weight too high", hook.ResourceLimits{CPUWeight: 20000}, "cpu_weight"},
		{"cpu_weight negative", hook.ResourceLimits{CPUWeight: -1}, "cpu_weight"},
		{"memory_max spaced", hook.ResourceLimits{MemoryMax: "8 G"}, "memory_max"},
		{"memory_max trailing junk", hook.ResourceLimits{MemoryMax: "8GB "}, "memory_max"},
		{"memory_high bad", hook.ResourceLimits{MemoryHigh: "lots"}, "memory_high"},
		{"io_weight out of range", hook.ResourceLimits{IOWeight: 99999}, "io_weight"},
		{"tasks_max negative", hook.ResourceLimits{TasksMax: -5}, "tasks_max"},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			err := validateResourceLimits(&c.rl)
			if err == nil {
				t.Fatalf("expected %+v to be rejected", c.rl)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error should name the offending field %q, got %q", c.want, err)
			}
		})
	}

	good := []hook.ResourceLimits{
		{CPUQuota: "1400%"}, {CPUQuota: "12.5%"}, {CPUQuota: "infinity"},
		{MemoryMax: "512M"}, {MemoryMax: "2G"}, {MemoryMax: "2GiB"}, {MemoryMax: "1024"},
		{MemoryHigh: "infinity"}, {CPUWeight: 1}, {CPUWeight: 10000},
		{IOWeight: 100}, {TasksMax: 512}, {},
	}
	for _, rl := range good {
		if err := validateResourceLimits(&rl); err != nil {
			t.Errorf("%+v should be accepted: %v", rl, err)
		}
	}
}

// The machine floor fills in what a stage left unset, per field, and never
// overrides what a stage said — including a deliberate "infinity" opt-out, which
// is the only way to escape the floor and is visible in the config when used.
func TestEffectiveLimitsMergesMachineFloorPerField(t *testing.T) {
	e := New()
	if err := e.SetDefaultResourceLimits(&hook.ResourceLimits{
		CPUQuota: "400%", CPUWeight: 50, MemoryHigh: "4G", TasksMax: 512,
	}); err != nil {
		t.Fatalf("set defaults: %v", err)
	}

	// A stage with nothing of its own gets the floor entire.
	got := e.EffectiveLimits(nil)
	if got.CPUQuota != "400%" || got.CPUWeight != 50 || got.TasksMax != 512 {
		t.Fatalf("an unlimited stage must inherit the whole floor, got %+v", got)
	}

	// A stage that sets some fields keeps them and inherits the rest.
	got = e.EffectiveLimits(&hook.ResourceLimits{MemoryMax: "16G", TasksMax: 64})
	switch {
	case got.MemoryMax != "16G":
		t.Fatalf("stage's own memory_max must win, got %+v", got)
	case got.TasksMax != 64:
		t.Fatalf("stage's own tasks_max must win over the floor, got %+v", got)
	case got.CPUQuota != "400%" || got.CPUWeight != 50 || got.MemoryHigh != "4G":
		t.Fatalf("unset fields must fall back to the floor, got %+v", got)
	}

	// Explicit opt-out: possible, but only by saying so.
	got = e.EffectiveLimits(&hook.ResourceLimits{CPUQuota: "infinity"})
	if got.CPUQuota != "infinity" {
		t.Fatalf("an explicit infinity must override the floor, got %+v", got)
	}

	// No floor set: the stage's own limits pass through untouched, nil included.
	bare := New()
	if got := bare.EffectiveLimits(nil); got != nil {
		t.Fatalf("no floor + no stage limits must stay nil, got %+v", got)
	}
}

func TestSetDefaultResourceLimitsValidates(t *testing.T) {
	e := New()
	if err := e.SetDefaultResourceLimits(&hook.ResourceLimits{CPUQuota: "1400"}); err == nil {
		t.Fatalf("a malformed machine floor must be refused, not silently installed")
	}
	if e.DefaultResourceLimits() != nil {
		t.Fatalf("a refused floor must not be installed")
	}
	// An all-empty block is the same as none — it must not make every command
	// take the systemd-run wrapper for no reason.
	if err := e.SetDefaultResourceLimits(&hook.ResourceLimits{}); err != nil {
		t.Fatalf("an empty block should be accepted as 'no floor': %v", err)
	}
	if e.DefaultResourceLimits() != nil {
		t.Fatalf("an empty block must clear the floor, not set an empty one")
	}
}

// The floor has to reach EVERY command the engine runs, which is the whole claim
// this feature makes — a stage command that skipped it would be a silent hole.
func TestMachineFloorReachesStageCommands(t *testing.T) {
	e := New()
	if err := e.SetDefaultResourceLimits(&hook.ResourceLimits{CPUWeight: 50}); err != nil {
		t.Fatalf("set defaults: %v", err)
	}
	// toHookTemplate is the single funnel for pre-gate/post-action hooks...
	tmpl := e.toHookTemplate(Hook{Command: CommandTemplate{Path: "/bin/true"}, Timeout: minute})
	if tmpl.ResourceLimits == nil || tmpl.ResourceLimits.CPUWeight != 50 {
		t.Fatalf("hooks must inherit the machine floor, got %+v", tmpl.ResourceLimits)
	}
	// ...and EffectiveLimits is what runClaimedHook (stage AND deploy main
	// commands) passes through.
	if got := e.EffectiveLimits(&hook.ResourceLimits{MemoryMax: "1G"}); got.CPUWeight != 50 {
		t.Fatalf("stage commands must inherit the machine floor, got %+v", got)
	}
}

// One merge function serves every level of the stack — stage over pipeline over
// per-daemon over machine-wide — so "more specific wins, per field" is defined once
// and cannot drift between them.
func TestMergeResourceLimitsIsPerField(t *testing.T) {
	machine := &hook.ResourceLimits{CPUWeight: 20, MemoryHigh: "4G", TasksMax: 1024}
	perDaemon := &hook.ResourceLimits{MemoryHigh: "16G"}

	got := MergeResourceLimits(perDaemon, machine)
	switch {
	case got.MemoryHigh != "16G":
		t.Fatalf("the more specific value must win: %+v", got)
	case got.CPUWeight != 20 || got.TasksMax != 1024:
		t.Fatalf("unset fields must be inherited: %+v", got)
	}
	// Either side absent is the identity, and neither input is mutated.
	if got := MergeResourceLimits(nil, machine); got.CPUWeight != 20 {
		t.Fatalf("nil own must take the default whole: %+v", got)
	}
	if got := MergeResourceLimits(perDaemon, nil); got.MemoryHigh != "16G" || got.CPUWeight != 0 {
		t.Fatalf("nil default must leave own alone: %+v", got)
	}
	if machine.MemoryHigh != "4G" || perDaemon.CPUWeight != 0 {
		t.Fatalf("merging must not mutate its inputs")
	}
}

// The IO caps take systemd's device-qualified "PATH VALUE" form. A bare value is
// the natural mistake and it is one systemd would reject partway through a run —
// or, on a host without the io controller, would not even reject.
func TestValidateIOCapShape(t *testing.T) {
	cases := []struct {
		name    string
		rl      hook.ResourceLimits
		wantErr string
	}{
		{"bandwidth ok", hook.ResourceLimits{IOWriteBandwidthMax: "/dev/sda 50M"}, ""},
		{"file path ok", hook.ResourceLimits{IOReadBandwidthMax: "/var/lib 2G"}, ""},
		{"iops ok", hook.ResourceLimits{IOReadIOPSMax: "/dev/sda 1000"}, ""},
		{"max ok", hook.ResourceLimits{IOWriteIOPSMax: "/dev/sda max"}, ""},
		{"bare value", hook.ResourceLimits{IOWriteBandwidthMax: "50M"}, "separated by a space"},
		{"comma instead of space", hook.ResourceLimits{IOReadBandwidthMax: "/dev/sda,50M"}, "separated by a space"},
		{"relative device", hook.ResourceLimits{IOReadBandwidthMax: "sda 50M"}, "is not a path"},
		{"bandwidth not a size", hook.ResourceLimits{IOWriteBandwidthMax: "/dev/sda fast"}, "bytes-per-second"},
		{"iops not a number", hook.ResourceLimits{IOReadIOPSMax: "/dev/sda 10M"}, "whole number"},
		{"unset ok", hook.ResourceLimits{}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateResourceLimits(&c.rl)
			switch {
			case c.wantErr == "" && err != nil:
				t.Fatalf("expected %+v to be accepted, got %v", c.rl, err)
			case c.wantErr != "" && err == nil:
				t.Fatalf("expected %+v to be rejected", c.rl)
			case c.wantErr != "" && !strings.Contains(err.Error(), c.wantErr):
				t.Fatalf("error %q must explain the shape (%q)", err, c.wantErr)
			}
		})
	}
}

// An IO cap has to reach the systemd-run argv, or it is config that does nothing —
// which is the whole failure mode this feature had to be careful about.
func TestIOCapsReachTheSystemdRunArgv(t *testing.T) {
	_, args := hook.WrapWithSystemdRun("/bin/true", nil, &hook.ResourceLimits{
		IOReadBandwidthMax: "/dev/sda 50M", IOWriteIOPSMax: "/var/lib 900",
	})
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--property=IOReadBandwidthMax=/dev/sda 50M",
		"--property=IOWriteIOPSMax=/var/lib 900",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in: %s", want, joined)
		}
	}
}

// IsZero decides whether a command gets wrapped at all, so an IO-cap-only block
// that reported "zero" would be silently dropped before systemd ever saw it.
func TestIOCapAloneIsNotZero(t *testing.T) {
	rl := &hook.ResourceLimits{IOWriteBandwidthMax: "/dev/sda 50M"}
	if rl.IsZero() {
		t.Fatal("a block setting only an IO cap must still wrap the command")
	}
	if !(&hook.ResourceLimits{}).IsZero() {
		t.Fatal("an empty block must stay zero")
	}
}

// A build that reads nproc gets the host's cores, not its grant, and cannot see its
// memory ceiling at all — which is how a stage came to run seven simultaneous Go
// builds asking for ~45 GB against a 4 GB soft ceiling. breeze set those limits, so
// breeze can say what they are instead of making the script rediscover them.
func TestEffectiveLimitsReachTheStageEnvironment(t *testing.T) {
	env := limitEnv(&hook.ResourceLimits{
		CPUQuota: "1400%", MemoryHigh: "12G", MemoryMax: "16G", TasksMax: 1024,
	})
	joined := strings.Join(env, " ")
	for _, want := range []string{
		"BREEZE_CPU_QUOTA=1400%", "BREEZE_MEMORY_HIGH=12G",
		"BREEZE_MEMORY_MAX=16G", "BREEZE_TASKS_MAX=1024",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in: %s", want, joined)
		}
	}
	// An unset limit must not appear at all: an empty BREEZE_MEMORY_HIGH would read
	// as "there is a ceiling and it is nothing", which is worse than absent.
	if got := limitEnv(&hook.ResourceLimits{CPUQuota: "200%"}); len(got) != 1 {
		t.Errorf("only the limits that are set may be exported, got %v", got)
	}
	if got := limitEnv(nil); got != nil {
		t.Errorf("no limits means no variables, got %v", got)
	}
}
