package engine

import (
	"reflect"
	"testing"

	"breeze/internal/hook"
)

// This function's comment used to promise that "more specific wins, per field" was
// "defined exactly once and cannot drift between them". It was not defined once —
// hclconfig.mergeLimits does the same job on wire types — and it drifted: nice and
// the four IO caps were added to that copy and not this one, so five of eleven
// fields silently stopped being inherited from a machine default.
//
// A sentence cannot enforce that. This does: it enumerates ResourceLimits by
// reflection and checks that a value present ONLY in the default survives the merge,
// for every field. A new field added to the struct and wired into only one merge
// fails here rather than in somebody's capacity arithmetic six weeks later.
func TestEveryResourceLimitFieldIsInherited(t *testing.T) {
	typ := reflect.TypeOf(hook.ResourceLimits{})
	if typ.NumField() == 0 {
		t.Fatal("no fields found — the reflection is broken, not the merge")
	}

	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		t.Run(f.Name, func(t *testing.T) {
			// A default that sets ONLY this field, to a non-zero value.
			def := &hook.ResourceLimits{}
			set := reflect.ValueOf(def).Elem().Field(i)
			switch f.Type.Kind() {
			case reflect.String:
				set.SetString("SENTINEL")
			case reflect.Int:
				set.SetInt(7)
			case reflect.Ptr:
				v := reflect.New(f.Type.Elem())
				v.Elem().SetInt(7)
				set.Set(v)
			default:
				t.Skipf("unhandled kind %s — extend this test rather than skipping in anger", f.Type.Kind())
			}

			// An "own" that sets a DIFFERENT field, so the merge is exercised in its
			// real shape (partially-specified stage over a machine default) rather
			// than the trivial nil case.
			own := &hook.ResourceLimits{CPUWeight: 99}
			if f.Name == "CPUWeight" {
				own = &hook.ResourceLimits{TasksMax: 99}
			}

			merged := MergeResourceLimits(own, def)
			got := reflect.ValueOf(merged).Elem().Field(i)
			if got.IsZero() {
				t.Errorf("%s is NOT inherited from the machine default — a stage that sets any other limit silently loses it", f.Name)
			}
		})
	}
}
