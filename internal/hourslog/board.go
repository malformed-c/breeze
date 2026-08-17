package hourslog

import (
	"database/sql"
	"fmt"
	"time"
)

// Task is one row of the board: a task, what it has cost, and when it last moved.
type Task struct {
	Summary string
	Secs    int       // total across every entry, summed from task_log
	Entries int       // how many runs contributed
	Last    time.Time // most recent entry's end (or begin, for a running one)
	Running bool      // an open entry — hours' active timer
}

// Board returns every task that has recorded time, most recently active first.
//
// Totals are SUMMED FROM task_log rather than read from task.secs_spent, even
// though that column exists and is what hours' own UI shows. The column is
// denormalized: it is the right thing for hours to keep and the wrong thing for a
// second tool to trust, because any writer that updates one and not the other
// makes it disagree with the entries it claims to total. Summing cannot drift
// from the rows it is summing.
func Board(dbPath string) ([]Task, error) {
	db, err := open(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if err := checkSchema(db); err != nil {
		return nil, err
	}

	rows, err := db.Query(`
SELECT t.summary,
       COALESCE(SUM(l.secs_spent), 0)                AS secs,
       COUNT(l.id)                                   AS entries,
       COALESCE(MAX(COALESCE(l.end_ts, l.begin_ts)), '') AS last,
       COALESCE(MAX(l.active), 0)                    AS running
FROM task t
JOIN task_log l ON l.task_id = t.id
GROUP BY t.id, t.summary
ORDER BY last DESC`)
	if err != nil {
		return nil, fmt.Errorf("reading the board: %w", err)
	}
	defer rows.Close()

	var out []Task
	for rows.Next() {
		var t Task
		var last sql.NullString
		if err := rows.Scan(&t.Summary, &t.Secs, &t.Entries, &last, &t.Running); err != nil {
			return nil, err
		}
		if last.Valid {
			t.Last, _ = parseTS(last.String)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// parseTS reads a timestamp out of task_log, which holds TEXT in more than one
// shape and so cannot be scanned straight into a time.Time.
//
// Both shapes are real and both can be in this table:
//
//	2026-08-17 20:49:40.507341081 +0000 UTC   written through the Go driver, which
//	                                          stores time.Time's String() form —
//	                                          what hours and breeze both produce
//	2026-08-17 20:07:34                       SQLite's own datetime(), i.e. anything
//	                                          inserted by hand or by another tool
//
// An unparseable value returns the zero time rather than an error: a task whose
// last activity cannot be read still belongs on the board, in OLDER, where it is
// visible. Dropping the row would hide a task on account of a timestamp.
func parseTS(s string) (time.Time, bool) {
	for _, layout := range []string{
		"2006-01-02 15:04:05.999999999 -0700 MST",
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// Column is where a task sits on the board. Recency, not status: hours has no
// notion of "doing" or "done" — a task is just a thing time was spent on — so
// inventing states it does not track would be a board about breeze's imagination
// rather than about the data.
type Column int

const (
	ColRunning Column = iota // an open timer, right now
	ColToday
	ColWeek
	ColOlder
)

func (c Column) String() string {
	switch c {
	case ColRunning:
		return "RUNNING"
	case ColToday:
		return "TODAY"
	case ColWeek:
		return "THIS WEEK"
	default:
		return "OLDER"
	}
}

// Columns bucket the board relative to now. Local time deliberately: the columns
// are "today" and "this week" as the person reading them lives, and a UTC
// midnight would move a late-evening run into tomorrow for a reader who was
// sitting right there when it happened.
func (t Task) Column(now time.Time) Column {
	switch {
	case t.Running:
		return ColRunning
	case t.Last.IsZero():
		return ColOlder
	}
	last := t.Last.Local()
	now = now.Local()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	switch {
	case !last.Before(midnight):
		return ColToday
	case !last.Before(midnight.AddDate(0, 0, -7)):
		return ColWeek
	default:
		return ColOlder
	}
}

// Duration renders seconds the way a time tracker should: "2h 15m", "45m", "30s".
// Never "0h 0m 45s" — a board is scanned, and leading zero units are noise that
// makes the numbers that matter harder to find.
func Duration(secs int) string {
	switch {
	case secs >= 3600:
		h, m := secs/3600, (secs%3600)/60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh %dm", h, m)
	case secs >= 60:
		return fmt.Sprintf("%dm", secs/60)
	default:
		return fmt.Sprintf("%ds", secs)
	}
}
