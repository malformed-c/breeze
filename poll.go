package main

import (
	"context"
	"time"

	"github.com/apsis-io/velocity/resilience"
)

// pollUntil probes every interval until the probe reports the condition satisfied,
// the budget expires, or ctx is done — whichever comes first. Returns the probe's
// value once it is satisfied, and a give-up error otherwise (which satisfies
// errors.Is(err, resilience.ErrGaveUp) whichever bound ended it).
//
// It is resilience.RetryUntil with one decision moved in here: a poll's bound is its
// budget, so the budget becomes a ctx deadline and MaxAttempts is left at zero. That
// is the whole helper. The version this replaced derived an attempt count —
// int(budget/interval)+1 — because Retry required one, and that single line was the
// root of three separate surprises: it truncated to 1 (a poll that never retried) when
// a caller's budget was under one interval; it ended the poll up to one interval
// SHORT of the budget, on every single call, because Retry returns on the final
// attempt without a final sleep; and rounding it up to fix that flipped the give-up
// error type from *RetryError to context.DeadlineExceeded under every caller. None of
// those can happen when there is no integer to get wrong.
//
// A condition's predicate is not inverted here. Each probe says whether the thing it
// waits for has happened — the lock is held, the socket answers, the process is gone —
// and an error from a probe is a REASON TO STOP (flockWithRetry's non-EWOULDBLOCK
// case: "this fd is broken" is not worth 15 seconds of anyone's time), not a
// "not yet" to be classified.
//
// An interval longer than the budget is now a supported way to ask for exactly one
// probe, and gets an honest give-up rather than the silent single-attempt collapse it
// used to cause. Nothing guards it because there is nothing left to guard.
//
// Every caller below passes context.Background(), and that is the remaining half of
// the story rather than an oversight: breeze's command paths carry no ctx, so these
// waits can express a DEADLINE but not CANCELLATION. velocity's "nothing waits
// outside the caller's context" is only half-available until the CLI threads one —
// worth saying plainly here rather than letting a Background-derived WithTimeout read
// as though the wait could be interrupted.
func pollUntil[T any](ctx context.Context, budget, interval time.Duration, probe func(context.Context) (T, bool, error)) (T, error) {
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	return resilience.RetryUntil(ctx, resilience.UntilPolicy{
		Backoff: func(int) time.Duration { return interval },
	}, probe)
}
