package hourslog

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// schemaV1 is hours' initial schema, reproduced verbatim from its InitDB so these
// tests fail if breeze's assumptions drift from the real shape. hours' own
// comment says these statements cannot change once released; only migrations can
// be added. If a future hours adds one, checkSchema refuses and this fixture is
// where the new shape gets pinned.
const schemaV1 = `
CREATE TABLE db_versions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    version INTEGER NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE task (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    summary TEXT NOT NULL,
    secs_spent INTEGER NOT NULL DEFAULT 0,
    active BOOLEAN NOT NULL DEFAULT true,
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE task_log (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id INTEGER,
    begin_ts TIMESTAMP NOT NULL,
    end_ts TIMESTAMP,
    secs_spent INTEGER NOT NULL DEFAULT 0,
    comment TEXT,
    active BOOLEAN NOT NULL,
    FOREIGN KEY(task_id) REFERENCES task(id)
);
CREATE TRIGGER prevent_duplicate_active_insert
BEFORE INSERT ON task_log
BEGIN
    SELECT CASE
        WHEN EXISTS (SELECT 1 FROM task_log WHERE active = 1)
        THEN RAISE(ABORT, 'Only one row with active=1 is allowed')
    END;
END;
INSERT INTO db_versions (version, created_at) VALUES (1, CURRENT_TIMESTAMP);
`

func newDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hours.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(schemaV1); err != nil {
		t.Fatalf("schema: %v", err)
	}
	return path
}

func entry(task string, secs int) Entry {
	begin := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	return Entry{Task: task, Comment: "deploy 5e1d2ab to local", Begin: begin, End: begin.Add(time.Duration(secs) * time.Second)}
}

func TestRecordCreatesTaskAndEntry(t *testing.T) {
	path := newDB(t)
	if err := Record(path, entry("breeze/breeze/deploy", 300)); err != nil {
		t.Fatalf("Record: %v", err)
	}

	db, _ := sql.Open("sqlite", path)
	defer db.Close()

	var summary string
	var taskSecs int
	if err := db.QueryRow(`SELECT summary, secs_spent FROM task`).Scan(&summary, &taskSecs); err != nil {
		t.Fatalf("task row: %v", err)
	}
	if summary != "breeze/breeze/deploy" || taskSecs != 300 {
		t.Errorf("task = %q/%ds, want breeze/breeze/deploy/300s", summary, taskSecs)
	}

	var logSecs, active int
	var comment string
	if err := db.QueryRow(`SELECT secs_spent, comment, active FROM task_log`).Scan(&logSecs, &comment, &active); err != nil {
		t.Fatalf("log row: %v", err)
	}
	if logSecs != 300 || comment != "deploy 5e1d2ab to local" {
		t.Errorf("log = %ds %q, want 300s with the brief", logSecs, comment)
	}
	// active=0 or the next insert of ANY kind is aborted by hours' trigger — a
	// stray active row here would wedge the user's own TUI, not just breeze.
	if active != 0 {
		t.Errorf("a finished run must be recorded inactive, got active=%d", active)
	}
}

// task.secs_spent is denormalized and hours' UI reads it. A second run under the
// same task must ADD to the total and reuse the task rather than duplicating it.
func TestRecordAccumulatesOntoOneTask(t *testing.T) {
	path := newDB(t)
	for _, secs := range []int{300, 120} {
		if err := Record(path, entry("breeze/breeze/test", secs)); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	db, _ := sql.Open("sqlite", path)
	defer db.Close()

	var tasks, logs, total int
	db.QueryRow(`SELECT COUNT(*) FROM task`).Scan(&tasks)
	db.QueryRow(`SELECT COUNT(*) FROM task_log`).Scan(&logs)
	db.QueryRow(`SELECT secs_spent FROM task`).Scan(&total)
	if tasks != 1 || logs != 2 {
		t.Errorf("want 1 task and 2 logs, got %d and %d", tasks, logs)
	}
	if total != 420 {
		t.Errorf("task total = %d, want 420 — the denormalized total must match the sum of its entries", total)
	}
}

// The constraint that decides whether this integration is trustworthy. hours'
// trigger has no condition on NEW.active, so it aborts EVERY insert while a timer
// runs — including a finished one. Recorded as a named error so the caller can
// say so, rather than a stage run vanishing while its author watches a timer.
func TestRecordReportsAnActiveTimerRatherThanFailingQuietly(t *testing.T) {
	path := newDB(t)
	db, _ := sql.Open("sqlite", path)
	if _, err := db.Exec(
		`INSERT INTO task_log (task_id, begin_ts, secs_spent, comment, active) VALUES (1, ?, 0, 'user is tracking something', 1)`,
		time.Now().UTC()); err != nil {
		t.Fatalf("seeding an active timer: %v", err)
	}
	db.Close()

	err := Record(path, entry("breeze/breeze/deploy", 300))
	if !errors.Is(err, ErrActiveTimer) {
		t.Fatalf("want ErrActiveTimer so the caller can report it, got %v", err)
	}
}

// A clock that steps backwards mid-stage must not write a negative duration into
// a column other rows are summed with.
func TestNegativeDurationsFloorAtZero(t *testing.T) {
	e := entry("breeze/breeze/build", 0)
	e.End = e.Begin.Add(-time.Hour)
	if got := e.Secs(); got != 0 {
		t.Errorf("Secs() = %d, want 0 for an end before its begin", got)
	}
}

// Writing into a schema breeze does not recognize is worse than not writing:
// the rows are accepted and wrong.
func TestRefusesAnUnknownSchemaVersion(t *testing.T) {
	path := newDB(t)
	db, _ := sql.Open("sqlite", path)
	db.Exec(`INSERT INTO db_versions (version, created_at) VALUES (2, CURRENT_TIMESTAMP)`)
	db.Close()

	err := Record(path, entry("breeze/breeze/deploy", 300))
	if err == nil {
		t.Fatal("a migrated hours database must be refused, not written into")
	}
}

// breeze must not create the database: hours initializes it, and a
// half-initialized file that hours later tries to migrate is a worse failure
// than an absent one.
func TestMissingDatabaseIsReportedNotCreated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope.db")
	if err := Record(path, entry("breeze/breeze/deploy", 300)); err == nil {
		t.Fatal("want an error for a missing database")
	}
}
