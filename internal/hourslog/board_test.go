package hourslog

import (
	"database/sql"
	"testing"
	"time"
)

func TestDurationDropsLeadingZeroUnits(t *testing.T) {
	for _, tc := range []struct {
		secs int
		want string
	}{
		{45, "45s"},
		{60, "1m"},
		{2700, "45m"},
		{3600, "1h"}, // not "1h 0m" — a board is scanned
		{8100, "2h 15m"},
	} {
		if got := Duration(tc.secs); got != tc.want {
			t.Errorf("Duration(%d) = %q, want %q", tc.secs, got, tc.want)
		}
	}
}

// Both timestamp shapes are real and both can be in this table: the Go driver
// stores time.Time's String() form, while anything inserted by hand gets
// SQLite's datetime(). Reading only one of them would silently sort half the
// board into OLDER.
func TestParseTSHandlesBothStoredShapes(t *testing.T) {
	for _, s := range []string{
		"2026-08-17 20:49:40.507341081 +0000 UTC",
		"2026-08-17 20:07:34",
		"2026-08-17T20:07:34Z",
	} {
		got, ok := parseTS(s)
		if !ok {
			t.Errorf("parseTS(%q) failed", s)
			continue
		}
		if got.Year() != 2026 || got.Month() != time.August || got.Day() != 17 {
			t.Errorf("parseTS(%q) = %v, want 2026-08-17", s, got)
		}
	}
	// Unreadable is not fatal: the task still belongs on the board.
	if _, ok := parseTS("not a time"); ok {
		t.Error("an unparseable timestamp must report failure, not a wrong time")
	}
}

func TestColumnsBucketByRecency(t *testing.T) {
	now := time.Date(2026, 8, 17, 15, 0, 0, 0, time.Local)
	for _, tc := range []struct {
		name string
		task Task
		want Column
	}{
		{"a running timer outranks its age", Task{Running: true, Last: now.AddDate(0, 0, -30)}, ColRunning},
		{"earlier today", Task{Last: now.Add(-2 * time.Hour)}, ColToday},
		{"just after midnight today", Task{Last: time.Date(2026, 8, 17, 0, 0, 1, 0, time.Local)}, ColToday},
		{"yesterday is this week, not today", Task{Last: now.AddDate(0, 0, -1)}, ColWeek},
		{"eight days ago", Task{Last: now.AddDate(0, 0, -8)}, ColOlder},
		{"no readable timestamp", Task{}, ColOlder},
	} {
		if got := tc.task.Column(now); got != tc.want {
			t.Errorf("%s: column = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Totals are summed from task_log rather than read from task.secs_spent. That
// column is denormalized and right for hours to keep, but a second tool trusting
// it inherits any disagreement between it and the entries it claims to total —
// so this test deliberately makes them disagree.
func TestBoardSumsEntriesRatherThanTrustingTheStoredTotal(t *testing.T) {
	path := newDB(t)
	db, _ := sql.Open("sqlite", path)
	db.Exec(`INSERT INTO task (id, summary, secs_spent, created_at) VALUES (1, 'work', 99999, ?)`, time.Now().UTC())
	db.Exec(`INSERT INTO task_log (task_id, begin_ts, end_ts, secs_spent, comment, active) VALUES (1, ?, ?, 300, 'a', 0)`,
		time.Now().Add(-time.Hour).UTC(), time.Now().UTC())
	db.Exec(`INSERT INTO task_log (task_id, begin_ts, end_ts, secs_spent, comment, active) VALUES (1, ?, ?, 120, 'b', 0)`,
		time.Now().Add(-2*time.Hour).UTC(), time.Now().Add(-time.Hour).UTC())
	db.Close()

	tasks, err := Board(path)
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("want 1 task, got %d", len(tasks))
	}
	if tasks[0].Secs != 420 {
		t.Errorf("Secs = %d, want 420 (the sum of the entries, not the stored 99999)", tasks[0].Secs)
	}
	if tasks[0].Entries != 2 {
		t.Errorf("Entries = %d, want 2", tasks[0].Entries)
	}
}

// A task with no entries has cost nothing and has no place on a board about where
// time went — and would otherwise sit permanently in OLDER with a zero.
func TestBoardOmitsTasksWithNoRecordedTime(t *testing.T) {
	path := newDB(t)
	db, _ := sql.Open("sqlite", path)
	db.Exec(`INSERT INTO task (summary, created_at) VALUES ('never tracked', ?)`, time.Now().UTC())
	db.Close()

	tasks, err := Board(path)
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	if len(tasks) != 0 {
		t.Errorf("want no rows, got %+v", tasks)
	}
}

func TestBoardReportsARunningEntry(t *testing.T) {
	path := newDB(t)
	db, _ := sql.Open("sqlite", path)
	db.Exec(`INSERT INTO task (id, summary, created_at) VALUES (1, 'rolling', ?)`, time.Now().UTC())
	db.Exec(`INSERT INTO task_log (task_id, begin_ts, secs_spent, comment, active) VALUES (1, ?, 0, 'now', 1)`,
		time.Now().Add(-10*time.Minute).UTC())
	db.Close()

	tasks, err := Board(path)
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	if len(tasks) != 1 || !tasks[0].Running {
		t.Fatalf("want one running task, got %+v", tasks)
	}
	// Last falls back to begin_ts for an open entry, which is what lets the board
	// show how long it has been running instead of the stored zero.
	if tasks[0].Last.IsZero() {
		t.Error("a running entry must still report when it started")
	}
}
