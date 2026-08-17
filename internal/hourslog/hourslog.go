// Package hourslog records finished stage runs as time entries in an `hours`
// database (github.com/dhth/hours, MIT, © 2024-2026 Dhruv Thakur).
//
// NOT a copy of hours' code. hours keeps its persistence layer under internal/,
// which Go forbids importing from another module — verified, not assumed: a
// `replace` pointing at a local checkout (i.e. exactly what a git submodule would
// give) still fails with "use of internal package ... not allowed". So a
// submodule cannot work here, and copying the package would drag in ~2,000 lines
// of read-queries that exist to serve a TUI breeze does not have. breeze needs
// three writes, so it makes three writes.
//
// WHAT THAT COSTS, STATED PLAINLY: this file encodes hours' schema, and hours can
// change it in any release. The schema is v1 with no migrations yet
// (latestDBVersion = 1), and hours' own comment says the v1 statements "cannot be
// changed once released; only further migrations can be added" — so the tables
// below are stable by their authors' rule, and a future migration is the thing to
// watch. checkSchema fails loudly rather than writing into a shape it does not
// recognize.
package hourslog

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// ErrActiveTimer means hours has a running timer, and its schema refuses ANY
// insert while one exists.
//
// This is not a guess about their intent — the trigger is
//
//	CREATE TRIGGER prevent_duplicate_active_insert BEFORE INSERT ON task_log
//	BEGIN SELECT CASE WHEN EXISTS (SELECT 1 FROM task_log WHERE active = 1)
//	  THEN RAISE(ABORT, 'Only one row with active=1 is allowed') END; END;
//
// with no condition on NEW.active, so it fires on every insert rather than only
// on inserts that would create a second active row. Measured: inserting a fully
// FINISHED entry (active=0) while any timer runs is aborted.
//
// Surfaced as its own error because the alternative is a stage run that silently
// fails to be recorded precisely while its author is sitting in the TUI watching
// a timer — the exact "two states that render alike" shape breeze exists to
// remove. The caller reports it; nothing retries behind anyone's back.
var ErrActiveTimer = errors.New("hours has an active timer, and its schema refuses any insert while one exists")

// ErrNoDB means the database file is not there. hours creates and initializes it
// on first run; breeze deliberately does NOT, because a half-initialized database
// that hours later tries to migrate is a worse failure than an absent one.
var ErrNoDB = errors.New("no hours database (run `hours` once to create it)")

// Entry is one finished stage run, as a time entry.
type Entry struct {
	Task    string // the task summary to aggregate under, e.g. "breeze/periapsis/deploy"
	Comment string // what happened — the brief, or a generated description
	Begin   time.Time
	End     time.Time
}

// Secs is the entry's duration, floored at zero. hours stores seconds as an
// integer and its own CLI branch had to fix "end before begin", so a clock that
// steps backwards during a stage must not write a negative total into a column
// other rows are summed with.
func (e Entry) Secs() int {
	d := e.End.Sub(e.Begin)
	if d < 0 {
		return 0
	}
	return int(d.Seconds())
}

// Record writes one finished entry, creating its task if needed.
//
// Everything happens in ONE transaction, because task.secs_spent is a
// DENORMALIZED total that hours' own UI reads: inserting the log without
// updating it leaves the tool reporting a task total that disagrees with the sum
// of its own entries, which is worse than not recording at all — one is a gap,
// the other is a number that looks right.
func Record(dbPath string, e Entry) error {
	if strings.TrimSpace(e.Task) == "" {
		return errors.New("a time entry needs a task summary")
	}
	db, err := open(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	if err := checkSchema(db); err != nil {
		return err
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rolled back only if Commit didn't happen

	taskID, err := taskIDFor(tx, e.Task)
	if err != nil {
		return err
	}
	secs := e.Secs()
	_, err = tx.Exec(
		`INSERT INTO task_log (task_id, begin_ts, end_ts, secs_spent, comment, active) VALUES (?, ?, ?, ?, ?, 0)`,
		taskID, e.Begin.UTC(), e.End.UTC(), secs, e.Comment)
	if err != nil {
		if isActiveTimerAbort(err) {
			return ErrActiveTimer
		}
		return fmt.Errorf("recording the time entry: %w", err)
	}
	if _, err := tx.Exec(
		`UPDATE task SET secs_spent = secs_spent + ?, updated_at = ? WHERE id = ?`,
		secs, time.Now().UTC(), taskID); err != nil {
		return fmt.Errorf("updating the task total: %w", err)
	}
	return tx.Commit()
}

func open(dbPath string) (*sql.DB, error) {
	// _txlock=immediate takes the write lock at BEGIN rather than at first write,
	// so a concurrent writer loses at the start of the transaction instead of
	// half way through it. busy_timeout because hours' TUI holds the database
	// while it is open, and a stage resolving at that moment should wait briefly
	// rather than fail.
	db, err := sql.Open("sqlite", dbPath+"?_txlock=immediate&_pragma=busy_timeout(3000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// checkSchema refuses to write into a database whose shape is not the one this
// package was written against. The failure it prevents: hours ships a migration,
// the columns move, and breeze keeps inserting rows that are accepted and wrong.
func checkSchema(db *sql.DB) error {
	var version int
	err := db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM db_versions`).Scan(&version)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNoDB, err)
	}
	if version != 1 {
		return fmt.Errorf("hours database is at schema version %d, but breeze only knows version 1 — check what changed before recording into it", version)
	}
	return nil
}

// taskIDFor finds the task by summary, creating it if absent. Matched on the
// exact summary because that is the only stable handle hours offers — it has no
// external id — so the task name is the integration's contract.
func taskIDFor(tx *sql.Tx, summary string) (int64, error) {
	var id int64
	err := tx.QueryRow(`SELECT id FROM task WHERE summary = ?`, summary).Scan(&id)
	switch {
	case err == nil:
		return id, nil
	case !errors.Is(err, sql.ErrNoRows):
		return 0, fmt.Errorf("looking up the task: %w", err)
	}
	now := time.Now().UTC()
	res, err := tx.Exec(
		`INSERT INTO task (summary, secs_spent, active, created_at, updated_at) VALUES (?, 0, 1, ?, ?)`,
		summary, now, now)
	if err != nil {
		return 0, fmt.Errorf("creating the task: %w", err)
	}
	return res.LastInsertId()
}

func isActiveTimerAbort(err error) bool {
	return strings.Contains(err.Error(), "Only one row with active=1 is allowed")
}
