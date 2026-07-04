package main

import (
	"context"
	"os/exec"
	"time"
)

// notifyViaMess is the daemon's wake-integration wiring: best-effort `mess send` for
// each identity, fired in a goroutine per identity so a slow/hung mess invocation
// never blocks the stage resolution that triggered it. Soft dependency — if `mess`
// isn't installed, this silently no-ops (checked once via exec.LookPath), mirroring
// mess's own desktopNotify graceful-degradation pattern. breeze's correctness never
// depends on this: stage.wait and status polling always see the true current state
// regardless of whether the notification actually reaches anyone.
func notifyViaMess(identities []string, message string) {
	messPath, err := exec.LookPath("mess")
	if err != nil {
		return
	}
	for _, identity := range identities {
		go func(identity string) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			// Best-effort: errors (unknown agent, mess not running, timeout) are
			// deliberately swallowed — this is a latency optimization, not a
			// guarantee, and breeze has its own poll/wait fallback regardless.
			exec.CommandContext(ctx, messPath, "send", identity, message).Run()
		}(identity)
	}
}
