package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"breeze/internal/engine"
)

// cmdAudit reads this daemon's audit log.
//
// The log has existed all along and there was NO WAY TO ASK IT ANYTHING: no verb,
// no noun, nothing in help. Its only route was knowing that a JSONL file lives at
// <state-dir>/audit.jsonl and opening it by hand. On 2026-08-19 two of us went
// looking for who had granted an agent the admin role on a shared daemon and
// neither could answer — one because the events were never emitted, the other
// because there was nothing to query even if they had been. Provenance that
// exists and is unreachable is the same shape as provenance that does not exist,
// which is why both halves shipped together.
//
// Reads the file directly rather than through the daemon: it is a local file the
// daemon appends to, and routing a read-only history through a wire op would make
// the record unavailable exactly when the daemon is unhealthy — which is when
// someone is most likely to be asking what happened.
func cmdAudit(p paths, args []string) error {
	f := parseFlags(args)
	if handled, err := f.only("breeze audit [--kind K] [--as NAME] [--limit N] [--since D] [--json]"); handled {
		return err
	}

	events, err := readAudit(p.audit)
	if err != nil {
		return err
	}
	events, err = filterAudit(events, f)
	if err != nil {
		return err
	}
	if f.jsonOut {
		printJSON(events)
		return nil
	}
	if len(events) == 0 {
		fmt.Printf("no audit events match (log: %s)\n", p.audit)
		return nil
	}
	// Oldest first: an audit log is read as a narrative, and --limit already
	// selects the recent tail, so reversing here would show the tail backwards.
	for _, ev := range events {
		fmt.Printf("%s  %-22s  %-16s  %s\n",
			ev.Time.Format("2006-01-02 15:04:05"), ev.Kind, ev.Actor, ev.Detail)
	}
	return nil
}

// readAudit parses the log, skipping unreadable lines rather than failing.
//
// A truncated final line is normal — the daemon appends, and a reader can arrive
// mid-write — and refusing to show 9,000 good events because the 9,001st is half
// written would make the tool useless exactly when it is being used during an
// incident.
func readAudit(path string) ([]engine.AuditEvent, error) {
	fh, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no audit log at %s — this daemon has not recorded anything yet", path)
		}
		return nil, err
	}
	defer fh.Close()

	var out []engine.AuditEvent
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // a stage detail can be long
	for sc.Scan() {
		var ev engine.AuditEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			continue
		}
		out = append(out, ev)
	}
	return out, sc.Err()
}

func filterAudit(events []engine.AuditEvent, f flagSet) ([]engine.AuditEvent, error) {
	var since time.Time
	if f.since != "" {
		d, err := time.ParseDuration(f.since)
		if err != nil {
			return nil, fmt.Errorf("--since %q: %w (try 2h, 30m, 72h)", f.since, err)
		}
		since = time.Now().Add(-d)
	}
	limit := 0
	if f.limit != "" {
		n, err := strconv.Atoi(f.limit)
		if err != nil {
			return nil, fmt.Errorf("--limit %q: %w", f.limit, err)
		}
		limit = n
	}

	out := events[:0:0]
	for _, ev := range events {
		// PREFIX match, so `--kind role` finds role.assigned and role.revoked and
		// `--kind stage` finds the whole family. Exact matching would make the
		// useful queries the ones you must already know the answer to.
		if f.auditKind != "" && !strings.HasPrefix(ev.Kind, f.auditKind) {
			continue
		}
		if f.as != "" && ev.Actor != f.as {
			continue
		}
		if !since.IsZero() && ev.Time.Before(since) {
			continue
		}
		out = append(out, ev)
	}
	// The TAIL, because a log is queried for what happened recently.
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}
