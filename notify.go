package main

import (
	"context"
	"os/exec"
	"time"
)

// runMessBestEffort shells out `mess <args...>` with a short timeout, swallowing
// any error (unknown agent, mess not running, timeout) — a latency optimization,
// not a guarantee; breeze's correctness never depends on a mess call actually
// landing. Shared by notifyViaMess, notifyViaMessTopic, and mess_listener.go's
// chat-command reply — every fire-and-forget `mess` shellout in breeze.
func runMessBestEffort(messPath string, args ...string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	exec.CommandContext(ctx, messPath, args...).Run()
}

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
			args := []string{"send", identity, message}
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
		args := []string{"pub", topic, message}
		if thread != "" {
			args = append(args, "--thread", thread)
		}
		runMessBestEffort(messPath, args...)
	}()
}
