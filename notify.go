package main

import (
	"bytes"
	"context"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// messSender is who the DAEMON sends as. It has to be explicit: mess resolves a
// sender from --as, then a session-registered identity, then $MESS_AGENT — and a
// long-lived daemon has none of those. Without it every notification breeze has
// ever sent failed with "no identity", and the error went in the bin (see below).
// The name need not be registered; mess accepts an explicit sender.
const messSender = "breeze"

// messHealth remembers whether the last `mess` invocation worked, so a notifier
// that CANNOT work says so once instead of failing forever in silence.
var messHealth struct {
	mu      sync.Mutex
	failing bool
	reason  string
}

// notifierStatus reports the notifier's health for `breeze status` — empty when
// nothing has gone wrong.
func notifierStatus() string {
	messHealth.mu.Lock()
	defer messHealth.mu.Unlock()
	if !messHealth.failing {
		return ""
	}
	return messHealth.reason
}

// runMessBestEffort shells out `mess <args...>` with a short timeout. Delivery
// stays best-effort — a peer being offline must never affect a stage outcome, and
// breeze's correctness never depends on a notification landing.
//
// What is NOT best-effort any more is MISCONFIGURATION. This used to discard the
// error, which meant "the recipient is offline" and "this daemon has no identity
// and every notification you have ever sent failed" were indistinguishable from
// breeze's side — so the second went unnoticed indefinitely. Failure and recovery
// are now logged once per transition (not per notification, which would be noise)
// and surfaced by `breeze status`.
func runMessBestEffort(messPath string, args ...string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, messPath, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()

	reason := ""
	if err != nil {
		reason = strings.TrimSpace(stderr.String())
		if reason == "" {
			reason = err.Error()
		}
		reason = oneLineMess(reason)
	}
	messHealth.mu.Lock()
	defer messHealth.mu.Unlock()
	switch {
	case reason != "" && !messHealth.failing:
		messHealth.failing, messHealth.reason = true, reason
		log.Printf("mess notifications are failing: %s — stage outcomes are unaffected, but nobody is being told about them", reason)
	case reason != "":
		messHealth.reason = reason
	case messHealth.failing:
		messHealth.failing, messHealth.reason = false, ""
		log.Printf("mess notifications are working again")
	}
}

func oneLineMess(s string) string { return strings.Join(strings.Fields(s), " ") }

// notifyViaMess is the daemon's wake-integration wiring: best-effort `mess send` for
// each identity, fired in a goroutine per identity so a slow/hung mess invocation
// never blocks the stage resolution that triggered it. Soft dependency — if `mess`
// isn't installed, this silently no-ops (checked once via exec.LookPath), mirroring
// mess's own desktopNotify graceful-degradation pattern. breeze's correctness never
// depends on this: stage.wait and status polling always see the true current state
// regardless of whether the notification actually reaches anyone. thread (see
// engine.messThreadID), when non-empty, is passed as `--thread` so every
// notification about one (pipeline, commit) run lands in the same mess thread.
func notifyViaMess(identities []string, message, thread string) {
	messPath, err := exec.LookPath("mess")
	if err != nil {
		return
	}
	for _, identity := range identities {
		go func(identity string) {
			args := []string{"send", "--as", messSender, identity, message}
			if thread != "" {
				args = append(args, "--thread", thread)
			}
			runMessBestEffort(messPath, args...)
		}(identity)
	}
}

// notifyViaMessTopic is notifyViaMess's counterpart for Pipeline.NotifyTopic —
// `mess pub <topic> "..."` instead of a per-identity send, same best-effort,
// soft-dependency, never-blocks-the-caller, thread-aware semantics.
func notifyViaMessTopic(topic, message, thread string) {
	messPath, err := exec.LookPath("mess")
	if err != nil {
		return
	}
	go func() {
		args := []string{"pub", "--as", messSender, topic, message}
		if thread != "" {
			args = append(args, "--thread", thread)
		}
		runMessBestEffort(messPath, args...)
	}()
}
