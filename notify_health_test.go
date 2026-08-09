package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeMess installs a stand-in `mess` that appends its argv to a log and exits with
// code, so a test can see exactly what the daemon tried to send.
func fakeMess(t *testing.T, code int, stderr string) (path, logFile string) {
	t.Helper()
	dir := t.TempDir()
	path = filepath.Join(dir, "mess")
	logFile = filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + logFile + "\n" +
		"echo " + stderr + " >&2\nexit " + strconv.Itoa(code) + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path, logFile
}

func readCalls(t *testing.T, logFile string) string {
	t.Helper()
	b, err := os.ReadFile(logFile)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatal(err)
	}
	return string(b)
}

func resetMessHealth() {
	messHealth.mu.Lock()
	messHealth.failing, messHealth.reason = false, ""
	messHealth.mu.Unlock()
}

// A pipeline notified an identity that had never registered, so every stage outcome
// for a whole day went unannounced. The daemon knew — it set the health flag and said
// so in `breeze status`, which is the command you run once you are ALREADY suspicious.
// It was found by someone who ran status for an unrelated reason. So the failure has
// to reach the operator through the channel that still works, rather than waiting to
// be asked.
func TestBrokenNotifierAlertsTheHuman(t *testing.T) {
	resetMessHealth()
	t.Cleanup(resetMessHealth)
	messPath, logFile := fakeMess(t, 1, "no-such-agent")

	runMessBestEffort(messPath, "the ci/build outcome", "send", "--as", messSender, "claude-verify", "breeze: ci/build -> failed")

	if notifierStatus() == "" {
		t.Fatal("a failing send must mark the notifier unhealthy")
	}
	waitFor(t, func() bool { return strings.Contains(readCalls(t, logFile), " user ") },
		"expected an alert addressed to the human mailbox \"user\"")

	calls := readCalls(t, logFile)
	if !strings.Contains(calls, "no-such-agent") {
		t.Errorf("the alert must carry the reason, got:\n%s", calls)
	}
	// The target is derived per run, so "notifications are failing" is true only
	// intermittently while specific outcomes go unannounced. The alert has to name
	// WHICH one nobody was told about, or it sends the reader back to a status line
	// that may already have flapped healthy again.
	if !strings.Contains(calls, "the ci/build outcome") {
		t.Errorf("the alert must name the undelivered outcome, got:\n%s", calls)
	}

	// A permanently dead target must cost ONE alert, not one per stage — and the
	// alert's own failure must not re-trigger the alert.
	before := strings.Count(readCalls(t, logFile), " user ")
	runMessBestEffort(messPath, "the ci/build outcome", "send", "--as", messSender, "claude-verify", "breeze: ci/test -> failed")
	time.Sleep(100 * time.Millisecond)
	if after := strings.Count(readCalls(t, logFile), " user "); after != before {
		t.Errorf("alert fired %d extra times while already failing; must fire once per transition", after-before)
	}
}

// The flip side: a healthy notifier must not page anyone.
func TestHealthyNotifierAlertsNobody(t *testing.T) {
	resetMessHealth()
	t.Cleanup(resetMessHealth)
	messPath, logFile := fakeMess(t, 0, "")

	runMessBestEffort(messPath, "the ci/build outcome", "send", "--as", messSender, "reviewer", "breeze: ci/build -> succeeded")

	if s := notifierStatus(); s != "" {
		t.Fatalf("a successful send must leave the notifier healthy, got %q", s)
	}
	time.Sleep(100 * time.Millisecond)
	if calls := readCalls(t, logFile); strings.Contains(calls, " user ") {
		t.Errorf("no alert should be sent when nothing is wrong, got:\n%s", calls)
	}
}

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(msg)
}
