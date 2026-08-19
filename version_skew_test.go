package main

import "testing"

// The warning must fire ONLY on real skew. A warning that fires for every `go run
// .` is one people stop reading, and then it is not there when it matters.
func TestVersionSkewWarningIsQuietUnlessBothBuildsAreKnownAndDiffer(t *testing.T) {
	saved := buildTime
	defer func() { buildTime = saved }()

	cases := []struct {
		name        string
		client, dsn string
		wantWarn    bool
	}{
		{"both stamped and different", "2026-08-19T22:00:00Z", "2026-08-19T10:00:00Z", true},
		{"identical builds", "2026-08-19T22:00:00Z", "2026-08-19T22:00:00Z", false},
		{"client unstamped (go run .)", "unknown", "2026-08-19T10:00:00Z", false},
		{"daemon unstamped", "2026-08-19T22:00:00Z", "unknown", false},
		{"both unstamped", "unknown", "unknown", false},
		{"client empty", "", "2026-08-19T10:00:00Z", false},
	}
	for _, c := range cases {
		buildTime = c.client
		if got := versionSkewed(c.dsn); got != c.wantWarn {
			t.Errorf("%s: skew=%v, want %v", c.name, got, c.wantWarn)
		}
	}
}
