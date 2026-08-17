package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"breeze/internal/hourslog"
)

// cmdBoard renders the hours tasks breeze has been recording into, as a board.
//
// Reads the database directly rather than going through the daemon: it is a local
// file the user owns, the daemon has nothing to add, and a wire op would make a
// read-only view of somebody's own time tracker depend on a running daemon.
func cmdBoard(p paths, args []string) error {
	f := parseFlags(args)
	if handled, err := f.rejectUnknownFlags("breeze board [--json]"); handled {
		return err
	}
	db := hoursDBFor(p)
	if db == "" {
		return fmt.Errorf("no hours database configured — set hours_db in %s (or the machine-wide defaults) to the path `hours` uses, and breeze will record finished stage runs into it", p.defaults)
	}
	tasks, err := hourslog.Board(db)
	if err != nil {
		return err
	}
	if f.jsonOut {
		printJSON(tasks)
		return nil
	}
	if len(tasks) == 0 {
		fmt.Printf("no time recorded yet in %s — it fills up as stages finish\n", db)
		return nil
	}
	renderBoard(os.Stdout, tasks, time.Now())
	return nil
}

const (
	boardColWidth = 34 // one card's text width; four of these fit an 140-col terminal
	boardCols     = 4
)

// renderBoard lays the tasks out in columns by recency.
//
// Columns rather than a sorted list because that is the question a board answers:
// not "what exists" (`hours log` already tells you that) but "what is warm". An
// empty column is still printed — a board where RUNNING silently disappears when
// nothing is running reads as though the column were never there, and then its
// absence carries no information the one time it matters.
func renderBoard(w *os.File, tasks []hourslog.Task, now time.Time) {
	cols := make([][]hourslog.Task, boardCols)
	totals := make([]int, boardCols)
	for _, t := range tasks {
		c := int(t.Column(now))
		cols[c] = append(cols[c], t)
		totals[c] += t.Secs
	}

	var header, rule strings.Builder
	for i := range boardCols {
		title := hourslog.Column(i).String()
		if totals[i] > 0 {
			title += "  " + hourslog.Duration(totals[i])
		}
		fmt.Fprintf(&header, "%-*s", boardColWidth+2, title)
		fmt.Fprintf(&rule, "%-*s", boardColWidth+2, strings.Repeat("─", boardColWidth))
	}
	fmt.Fprintln(w, strings.TrimRight(header.String(), " "))
	fmt.Fprintln(w, strings.TrimRight(rule.String(), " "))

	depth := 0
	for _, c := range cols {
		depth = max(depth, len(c))
	}
	for row := range depth {
		// Each card is two lines: what it is, then what it cost. Kept together in
		// one pass so the columns stay aligned even when they have different depths.
		var line1, line2 strings.Builder
		for i := range boardCols {
			name, cost := "", ""
			if row < len(cols[i]) {
				t := cols[i][row]
				name = elide(t.Summary, boardColWidth)
				cost = fmt.Sprintf("  %s · %s", hourslog.Duration(t.Secs), plural(t.Entries, "run"))
				// A running entry has accrued no secs_spent yet — hours only writes
				// that when the timer stops. Printing its stored 0 would make the
				// one task that IS being worked on look like the idle one, so show
				// what it has actually been running for.
				if t.Running && !t.Last.IsZero() {
					cost = fmt.Sprintf("  %s so far", hourslog.Duration(int(now.Sub(t.Last).Seconds())))
				}
			}
			fmt.Fprintf(&line1, "%-*s", boardColWidth+2, name)
			fmt.Fprintf(&line2, "%-*s", boardColWidth+2, cost)
		}
		fmt.Fprintln(w, strings.TrimRight(line1.String(), " "))
		if s := strings.TrimRight(line2.String(), " "); s != "" {
			fmt.Fprintln(w, s)
		}
	}
}

// elide keeps the TAIL of an over-long task name, not the head. breeze's own
// tasks are "breeze: <pipeline>/<stage>", so the distinguishing part is at the
// end — truncating the other way gives a column of identical prefixes.
func elide(s string, width int) string {
	if len(s) <= width {
		return s
	}
	return "…" + s[len(s)-(width-1):]
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}
