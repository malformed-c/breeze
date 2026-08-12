package hook

import (
	"slices"
	"strings"
	"testing"
)

func envMap(kvs []string) map[string]string {
	m := make(map[string]string, len(kvs))
	for _, kv := range kvs {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}

func TestStageContextExportsTheWorkNotJustTheResources(t *testing.T) {
	got := envMap(StageContext{
		Pipeline: "periapsis", Stage: "deploy",
		Commit: "d744a086", Environment: "engix99",
		Actor: "coordinator", Brief: "tidal reconcile + D-Bus self-heal",
	}.Env())

	for k, want := range map[string]string{
		"BREEZE_PIPELINE":    "periapsis",
		"BREEZE_STAGE":       "deploy",
		"BREEZE_COMMIT":      "d744a086",
		"BREEZE_ENVIRONMENT": "engix99",
		"BREEZE_ACTOR":       "coordinator",
		"BREEZE_BRIEF":       "tidal reconcile + D-Bus self-heal",
	} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
}

// The subtle half. A breeze daemon may itself run as a breeze stage — this repo's
// own CI does exactly that — so an OMITTED variable is not "unset" from the
// child's point of view, it is the OUTER run's value, and the stage then reports
// somebody else's commit and actor as its own. Every key must be present even
// when empty, because os/exec resolves duplicate keys to the last one: an explicit
// empty overrides an inherited value, an absent key silently inherits it.
func TestStageContextExportsEmptyValuesSoNothingIsInherited(t *testing.T) {
	// A pre-fan-out stage has no environment and may carry no brief.
	env := StageContext{Pipeline: "breeze", Stage: "build", Commit: "5e1d2ab", Actor: "breeze-main"}.Env()

	var names []string
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		names = append(names, k)
	}
	for _, k := range []string{"BREEZE_ENVIRONMENT", "BREEZE_BRIEF"} {
		if !slices.Contains(names, k) {
			t.Errorf("%s must be exported even when empty — an absent key inherits the outer run's value", k)
		}
	}
	got := envMap(env)
	if got["BREEZE_ENVIRONMENT"] != "" || got["BREEZE_BRIEF"] != "" {
		t.Errorf("empty fields must export as empty, got env=%q brief=%q", got["BREEZE_ENVIRONMENT"], got["BREEZE_BRIEF"])
	}
}
