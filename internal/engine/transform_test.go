package engine

import (
	"os/exec"
	"strings"
	"testing"
)

func transformPipeline(t *testing.T, cmd CommandTemplate, transform *Hook) *Engine {
	t.Helper()
	e := New()
	p := Pipeline{
		Name: "svc",
		Stages: []StageDef{{
			Name: "test", Type: StageCommand, Timeout: minute,
			Command: cmd, CommandPolicy: &CommandPolicy{}, Transform: transform,
		}},
		FanOutAt: 1,
	}
	if err := e.RegisterPipeline(p, "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}
	return e
}

// The point of a transform: the raw output is what happened, the summary is what it
// means. Both are kept — the summary never replaces the record.
func TestTransformSummarizesAndKeepsRawOutput(t *testing.T) {
	e := transformPipeline(t,
		CommandTemplate{Path: "/bin/sh", Args: []string{"-c", "echo 'not ok 1'; echo 'not ok 2'; echo ok 3"}},
		&Hook{Timeout: minute, Command: CommandTemplate{
			Script:      `printf '%s failing checks' "$(grep -c '^not ok' <<<"$(cat)")"`,
			Interpreter: []string{"/bin/bash"},
		}},
	)
	if _, err := exec.LookPath("/bin/bash"); err != nil {
		t.Skip("bash not available")
	}
	inst, err := e.StartCommandStage("svc", "test", "abc123", "", "ci", "")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if inst.Summary == "" {
		t.Fatalf("expected a summary, got none (status=%s)", inst.Status)
	}
	if !strings.Contains(string(inst.Stdout), "not ok 1") {
		t.Fatalf("the raw output must survive alongside the summary, got %q", inst.Stdout)
	}
}

// The transform sees the RESOLVED result — status, exit code, output — which is the
// only reason it can say anything useful about a failure.
func TestTransformReceivesTheResolvedResultAsJSON(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	e := transformPipeline(t,
		CommandTemplate{Path: "/bin/sh", Args: []string{"-c", "echo boom >&2; exit 4"}},
		&Hook{Timeout: minute, Command: CommandTemplate{
			Interpreter: []string{"python3"},
			Script: "import sys,json\nd=json.load(sys.stdin)\n" +
				"print('%s/%s %s exit=%d stderr=%s actor=%s' % (d['pipeline'],d['stage'],d['status'],d['exitCode'],d['stderr'].strip(),d['actor']))",
		}},
	)
	inst, err := e.StartCommandStage("svc", "test", "abc123", "", "ci", "")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	want := "svc/test failed exit=4 stderr=boom actor=ci"
	if strings.TrimSpace(inst.Summary) != want {
		t.Fatalf("summary = %q, want %q", inst.Summary, want)
	}
}

// A summarizer must never be able to turn a green build red — but its failure must
// not vanish either, or a missing summary is indistinguishable from a stage nobody
// configured one for.
func TestBrokenTransformCannotChangeTheOutcome(t *testing.T) {
	for _, c := range []struct{ name, script, want string }{
		{"nonzero exit", "echo trouble; exit 7", "exited 7"},
		{"writes nothing", "exit 0", "wrote nothing"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := transformPipeline(t,
				CommandTemplate{Path: "/bin/true"},
				&Hook{Timeout: minute, Command: CommandTemplate{Script: c.script}},
			)
			var audited []AuditEvent
			e.SetAuditFn(func(ev AuditEvent) { audited = append(audited, ev) })

			inst, err := e.StartCommandStage("svc", "test", "abc123", "", "ci", "")
			if err != nil {
				t.Fatalf("start: %v", err)
			}
			if inst.Status != StageSucceeded {
				t.Fatalf("a broken transform must not affect the stage: status=%s", inst.Status)
			}
			if !strings.Contains(inst.Summary, c.want) {
				t.Fatalf("summary should report the transform failure (%q), got %q", c.want, inst.Summary)
			}
			found := false
			for _, ev := range audited {
				if ev.Kind == "stage.transform.failed" {
					found = true
				}
			}
			if !found {
				t.Fatalf("a failed transform must be audited, got %+v", audited)
			}
		})
	}
}

// No transform is the overwhelmingly common case and must cost nothing.
func TestNoTransformLeavesSummaryEmpty(t *testing.T) {
	e := transformPipeline(t, CommandTemplate{Path: "/bin/true"}, nil)
	inst, err := e.StartCommandStage("svc", "test", "abc123", "", "ci", "")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if inst.Summary != "" {
		t.Fatalf("summary = %q, want empty", inst.Summary)
	}
}

func TestCommandAndScriptAreMutuallyExclusive(t *testing.T) {
	e := New()
	for _, c := range []struct {
		name, want string
		cmd        CommandTemplate
	}{
		{"neither", "command required", CommandTemplate{}},
		{"both", "mutually exclusive", CommandTemplate{Path: "/bin/true", Script: "echo hi"}},
		{"script with args", "args cannot be combined", CommandTemplate{Script: "echo hi", Args: []string{"x"}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := Pipeline{Name: "svc", FanOutAt: 1, Stages: []StageDef{{
				Name: "test", Type: StageCommand, Timeout: minute,
				Command: c.cmd, CommandPolicy: &CommandPolicy{},
			}}}
			err := e.RegisterPipeline(p, "admin")
			if err == nil {
				t.Fatalf("expected rejection")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q should explain %q", err, c.want)
			}
		})
	}
}
