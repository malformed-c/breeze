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
