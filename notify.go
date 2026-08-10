package main

import (
	"bytes"
	"context"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"

	"breeze/internal/engine"
	"breeze/internal/hook"
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
//
// undelivered describes WHAT was being delivered, in the operator's terms ("the
// verify-guards outcome to \"claude-verify\""), so the alert below can name the
// specific thing nobody was told rather than only reporting that the channel is
// unwell. Empty for sends that are themselves alerts.
func runMessBestEffort(messPath, undelivered string, args ...string) {
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
	newlyFailing := reason != "" && !messHealth.failing
	switch {
	case newlyFailing:
		messHealth.failing, messHealth.reason = true, reason
		log.Printf("mess notifications are failing: %s — stage outcomes are unaffected, but nobody is being told about them", reason)
	case reason != "":
		messHealth.reason = reason
	case messHealth.failing:
		messHealth.failing, messHealth.reason = false, ""
		log.Printf("mess notifications are working again")
	}
	messHealth.mu.Unlock()

	if newlyFailing {
		alertHumanNotifierBroken(messPath, reason, undelivered)
	}
}

// alertHumanNotifierBroken tells the operator, once per failure transition, that
// notifications have stopped — through mess itself, to "user", the one recipient
// that is a human mailbox rather than an agent that may never have registered.
//
// The health flag alone was not enough. A pipeline spent a day notifying an identity
// that had never existed ("no such agent \"claude-verify\""), so every stage outcome
// went unannounced; the daemon reported it accurately in `breeze status`, which is
// the place you look when you are ALREADY suspicious. It was found by an agent who
// only ran status because something else had gone wrong. A broken notifier must not
// depend on someone thinking to ask whether the notifier is broken.
//
// Deliberately fired only on the transition, so a permanently-dead target costs one
// message rather than one per stage. If this send fails too, nothing recurses: the
// health flag is already set, so its own failure takes the non-transition branch —
// which is also why alerts pass undelivered="" and never alert about themselves.
//
// It names the outcome, not just the channel, because the target is derived PER RUN
// from whoever holds the next stage's role: the same daemon failed on "claude-verify"
// and ninety minutes later on "opus-inflight". "Notifications are failing" is
// therefore true only intermittently, while specific outcomes go unannounced — and a
// health flag that flaps tells you less than a message naming what was lost.
// Diagnosed by trail-main, who re-ran status instead of pasting the line they had.
func alertHumanNotifierBroken(messPath, reason, undelivered string) {
	msg := "breeze: a notification could not be delivered — " + reason
	if undelivered != "" {
		msg += "; nobody was told about " + undelivered
	}
	msg += " (the stage outcome itself is unaffected; `breeze status` shows the live state)"
	go runMessBestEffort(messPath, "", "send", "--as", messSender, "user", msg)
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
			runMessBestEffort(messPath, oneLineMess(message)+" (to "+identity+")", args...)
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
		runMessBestEffort(messPath, oneLineMess(message)+" (to topic "+topic+")", args...)
	}()
}

// ioLimitStatus reports, for `breeze status`, that this daemon has an IO limit
// configured which cannot possibly take effect — empty when there is nothing to say.
//
// It is checked and reported rather than silently tolerated because every ordinary
// signal says the limit worked: systemd-run accepts the property and exits 0, and
// `systemctl show` reads it back verbatim. Only the cgroup itself disagrees, and
// nobody looks there. Measured on the host this was written for, the io controller
// is delegated to the system manager and NOT to the per-user one, so every IO limit
// a non-root breeze sets is inert — including io_weight, which shipped long before
// the caps and had been quietly doing nothing.
//
// Deliberately silent when no IO limit is configured: an undelegated controller is
// not a problem for a daemon that never asks for it, and a warning that fires when
// nothing is wrong is how a warning stops being read.
func ioLimitStatus(eng *engine.Engine) string {
	rl := eng.DefaultResourceLimits()
	if !rl.UsesIO() {
		return ""
	}
	if ok, why := hook.IOControllerAvailable(); !ok {
		return why + " — the limit is accepted by systemd and reported back by `systemctl show`, but nothing enforces it. " +
			"Fix by delegating the controller to your user manager: a drop-in for user@.service with `Delegate=cpu io memory pids`, then re-login"
	}
	return ""
}
