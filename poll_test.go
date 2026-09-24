package main

import (
	"context"
	"errors"
	"math"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apsis-io/velocity/resilience"
)

// The question these answer: a poll that ends EARLY looks exactly like a poll whose
// condition was met at the same instant — both stop, both hand back a give-up, and
// the outcome alone cannot say which happened.
//
// My first attempt asserted a two-sided band, floor(budget/interval) <= probes <=
// ceil(budget/interval)+1, and wm-eval was right that the lower end is unsound. A
// cycle costs the interval PLUS the probe, so a budget buys floor(budget / (interval
// + probeCost)) probes, not floor(budget/interval) — and under load the probe is not
// free. That lower bound assumes the arithmetic survives a scheduler, which is the
// same mistake as the attempts=floor(budget/interval)+1 I had just reported, in mirror
// image: an idealised division standing in for a measurement.
//
// The fix is not a looser band, it is to use only the bounds that ARE robust, and the
// asymmetry is the useful part:
//
//   - An UPPER bound on probe count is robust. Every completed cycle sleeps at least
//     the interval, so load can only make cycles LONGER and probes FEWER. The most
//     probes a budget can buy is floor(budget/interval)+1, and that holds however
//     badly the scheduler behaves.
//   - A LOWER bound is not robust, for the same reason inverted: it assumes the probe
//     costs nothing, so it is the end that flakes under load.
//
// Which is why the "ends early" case needs a RELATIVE assertion rather than an
// absolute one. Comparing a run against another run of the same shape cancels out
// both probe cost and machine load, which is the only way to say "this poll used the
// budget it was given" without hard-coding a number the machine gets to choose.
func TestPollUntilNeverProbesMoreThanTheBudgetBuys(t *testing.T) {
	for _, c := range []struct{ budget, interval time.Duration }{
		{250 * time.Millisecond, 100 * time.Millisecond},
		{100 * time.Millisecond, 50 * time.Millisecond},
		{60 * time.Millisecond, 20 * time.Millisecond},
		{45 * time.Millisecond, 15 * time.Millisecond},
		{300 * time.Millisecond, 100 * time.Millisecond},
	} {
		got := probesFor(t, c.budget, c.interval)
		// +1 for the probe in flight when the deadline cuts the last sleep.
		if want := int64(math.Floor(float64(c.budget)/float64(c.interval))) + 1; got > want {
			t.Errorf("budget=%s interval=%s: probed %d times, more than the %d the budget can buy", c.budget, c.interval, got, want)
		}
	}
}

// The under-wait case. A relative probe count cannot catch it — comparing a 200ms
// poll against a 1000ms one passes even if BOTH silently use half their budget,
// because scaling preserves the ordering — so this asserts the thing directly, in
// time rather than in iterations.
//
// And time is the robust direction, for the same reason the probe upper bound is: a
// cycle sleeps at least the interval, so probe cost and machine load can only ever
// make a poll run LONGER. Every completed sleep must have happened, so the poll cannot
// have finished before (floor(budget/interval) x interval). A budget silently halved
// finishes well short of that; a loaded box finishes late and still passes.
//
// Which is the generalisation both of us were reaching for and neither had: because
// probe cost and load only ever subtract from what a budget affords, EVERY sound
// bound on a bounded loop points the same way — at least this much work happened, at
// most this many iterations. The unsound bounds are all the mirror image, and they
// are the ones that read as "at least N probes".
func TestPollUntilUsesTheBudgetItWasGiven(t *testing.T) {
	for _, c := range []struct{ budget, interval time.Duration }{
		{1000 * time.Millisecond, 100 * time.Millisecond},
		{600 * time.Millisecond, 50 * time.Millisecond},
		{450 * time.Millisecond, 150 * time.Millisecond},
	} {
		start := time.Now()
		probesFor(t, c.budget, c.interval)
		elapsed := time.Since(start)
		want := time.Duration(math.Floor(float64(c.budget)/float64(c.interval))) * c.interval
		if elapsed < want {
			t.Errorf("budget=%s interval=%s: gave up after %s, before the %d whole intervals its budget owes — it is ending early, and the give-up alone looks exactly like a condition met on time",
				c.budget, c.interval, elapsed.Round(time.Millisecond), int(math.Floor(float64(c.budget)/float64(c.interval))))
		}
	}
}

// The sound minimal lower bound, and why it is the only one: probes >= 2 whenever the
// budget exceeds one interval, because the first sleep cannot be cut short by a
// deadline that has not arrived yet. A timer firing late (which is what load causes)
// only makes this safer. Asserting more than this would be asserting that the probe is
// free.
func TestPollUntilKeepsPollingWhileTheBudgetLasts(t *testing.T) {
	if got := probesFor(t, 500*time.Millisecond, 100*time.Millisecond); got < 2 {
		t.Errorf("probed %d times over a budget five intervals long; the first sleep alone should have completed", got)
	}
}

// probesFor runs a poll whose condition never holds, and returns how many times the
// probe ran. Asserting the give-up here as well keeps every count in this file honest:
// a count from a run that did not give up is not a count about the budget.
func probesFor(t *testing.T, budget, interval time.Duration) int64 {
	t.Helper()
	var probes atomic.Int64
	_, err := pollUntil(context.Background(), budget, interval, func(context.Context) (struct{}, bool, error) {
		probes.Add(1)
		return struct{}{}, false, nil
	})
	if !errors.Is(err, resilience.ErrGaveUp) {
		t.Fatalf("budget=%s interval=%s: a condition that never held must give up, got %v", budget, interval, err)
	}
	return probes.Load()
}

// Every give-up is one class, whichever bound ended it — so "did my poll give up?"
// is a single question a caller can ask instead of a type switch over *RetryError
// and context.DeadlineExceeded.
func TestPollUntilGivesUpAsOneErrorClass(t *testing.T) {
	_, err := pollUntil(context.Background(), 80*time.Millisecond, 20*time.Millisecond, func(context.Context) (struct{}, bool, error) {
		return struct{}{}, false, nil
	})
	if !errors.Is(err, resilience.ErrGaveUp) {
		t.Fatalf("a poll that ran out of budget must satisfy errors.Is(err, ErrGaveUp), got %v", err)
	}
	// Never a bare nil: a poll that never succeeded reporting success is the exact
	// defect the previous API invited, and it is unrepresentable now only because a
	// give-up always carries a cause.
	if err == nil {
		t.Fatal("a poll whose condition never held returned no error at all")
	}
}

// A probe error is a REASON TO STOP, never a "not yet": flockWithRetry depends on
// this to tell "someone else holds the lock" (worth waiting out) from "this fd is
// broken" (not worth 15 seconds of anyone's time).
func TestPollUntilStopsOnAProbeErrorWithoutRetrying(t *testing.T) {
	broken := errors.New("EBADF")
	var probes atomic.Int64
	_, err := pollUntil(context.Background(), time.Second, 10*time.Millisecond, func(context.Context) (struct{}, bool, error) {
		probes.Add(1)
		return struct{}{}, false, broken
	})
	if !errors.Is(err, broken) {
		t.Fatalf("a probe error must come back untouched, got %v", err)
	}
	if got := probes.Load(); got != 1 {
		t.Errorf("a probe error must not be retried, got %d probes", got)
	}
}

func TestPollUntilReturnsTheProbeValue(t *testing.T) {
	var probes atomic.Int64
	// Satisfied on the second probe, against a budget of ten intervals: enough room
	// that load cannot cost the poll its second probe, which is the only thing this
	// needs to assert. The BOUNDARY case — satisfied on the final probe the budget
	// allows — is a property of RetryUntil, not of this helper, it is pinned twice on
	// velocity's side, and asserting it here would mean hard-coding an exact probe
	// count, which is the fragile thing the rest of this file deliberately avoids.
	got, err := pollUntil(context.Background(), time.Second, 100*time.Millisecond, func(context.Context) (string, bool, error) {
		if probes.Add(1) < 2 {
			return "", false, nil
		}
		return "connected", true, nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "connected" {
		t.Errorf("got %q, want the probe's value", got)
	}
	if probes.Load() != 2 {
		t.Errorf("expected to stop at the second probe, got %d", probes.Load())
	}
}

// A cancelled context stops the poll immediately — the capability the four hand-rolled
// loops did not have at all. Nothing in the CLI passes a cancellable ctx yet (see
// pollUntil's doc), so this asserts the mechanism is genuinely wired rather than
// merely available.
func TestPollUntilStopsOnACancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var probes atomic.Int64
	_, err := pollUntil(ctx, time.Minute, 10*time.Millisecond, func(context.Context) (struct{}, bool, error) {
		probes.Add(1)
		return struct{}{}, false, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled context must stop the poll, got %v", err)
	}
	if probes.Load() != 0 {
		t.Errorf("nothing should be probed after cancellation, got %d", probes.Load())
	}
}

// An interval longer than the budget is how you ask for exactly one probe. It used to
// be a silent trap: the derived attempt count collapsed to 1, and the poll then
// reported "the condition was never met" as though that were a fact about the world
// rather than about the caller's arithmetic. Now it is one probe and an honest
// give-up, which is a supported request rather than a bug.
func TestAnIntervalLongerThanTheBudgetProbesOnceAndGivesUp(t *testing.T) {
	var probes atomic.Int64
	_, err := pollUntil(context.Background(), 50*time.Millisecond, 100*time.Millisecond, func(context.Context) (struct{}, bool, error) {
		probes.Add(1)
		return struct{}{}, false, nil
	})
	if !errors.Is(err, resilience.ErrGaveUp) {
		t.Fatalf("expected an honest give-up, got %v", err)
	}
	if probes.Load() != 1 {
		t.Errorf("expected exactly one probe, got %d", probes.Load())
	}
}
