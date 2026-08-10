package hook

import "testing"

// Not an assertion — this host's answer is a fact about the host, and the point of
// IOControllerAvailable is that it reports it rather than guessing. Run with -v to
// see what this machine actually does.
func TestReportIOControllerAvailabilityOnThisHost(t *testing.T) {
	ok, why := IOControllerAvailable()
	t.Logf("io controller available: %v | %s", ok, why)
	if !ok && why == "" {
		t.Fatal("an unavailable controller must come with a reason")
	}
	if ok && why != "" {
		t.Fatalf("an available controller must not carry a complaint: %q", why)
	}
}
