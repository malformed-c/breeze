package main

import "testing"

// Three things now trigger shutdown — OpStop, OpRestart and a signal — and
// close() of an already-closed channel PANICS. A SIGTERM arriving while an
// OpStop is in flight is not exotic: it is `breeze stop` racing a session
// teardown, which is how a shell exits.
func TestShutdownCanBeTriggeredMoreThanOnce(t *testing.T) {
	d := &daemonServer{stop: make(chan struct{})}

	d.beginShutdown()
	d.beginShutdown() // would panic without the sync.Once
	d.beginShutdown()

	select {
	case <-d.stop:
	default:
		t.Fatal("the first call must actually close the channel")
	}
}

// Concurrent triggers are the real shape: the signal goroutine and a request
// handler run independently. Runs under -race in CI.
func TestConcurrentShutdownTriggersDoNotPanic(t *testing.T) {
	d := &daemonServer{stop: make(chan struct{})}
	done := make(chan struct{})
	for range 8 {
		go func() {
			d.beginShutdown()
			done <- struct{}{}
		}()
	}
	for range 8 {
		<-done
	}
	select {
	case <-d.stop:
	default:
		t.Fatal("stop should be closed after concurrent triggers")
	}
}
