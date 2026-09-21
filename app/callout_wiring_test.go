package app

import (
	"os"
	"regexp"
	"testing"
)

// The owner's loop is the one ExternalProcessingService app.go builds, in both
// modes. Reviewed rather than exercised: app.New starts listeners and a
// scheduler, and what it hands the engine is not observable from outside
// without a hook — so this pins the source, the way
// internal/txgate/suspend_call_sites_test.go pins the Suspend call sites.
func TestAppWiresTheOwnersLoopInBothModes(t *testing.T) {
	src, err := os.ReadFile("app.go")
	if err != nil {
		t.Fatal(err)
	}
	for name, re := range map[string]*regexp.Regexp{
		"the Coordinator is built":                                                      regexp.MustCompile(`extProc = callout\.New\(`),
		"the tracing decorator wraps the outside":                                       regexp.MustCompile(`(?s)extProc = callout\.New\(.*extProc = observability\.NewTracingExternalProcessingService\(extProc,`),
		"the peer router is held as the interface, so a single node has an untyped nil": regexp.MustCompile(`var peers callout\.PeerRouter`),
		"one lock registry per process":                                                 regexp.MustCompile(`(?s)a\.txGate = txgate\.New\(\).*a\.fence = fence\.New\(a\.txGate\)`),
	} {
		if !re.Match(src) {
			t.Errorf("app.go: %s — pattern %s not found", name, re)
		}
	}
	for name, re := range map[string]*regexp.Regexp{
		"ClusterDispatcher":                     regexp.MustCompile(`ClusterDispatcher`),
		"the dispatcher used as the service":    regexp.MustCompile(`extProc = localDispatcher`),
		"the retired pass lifetime is read":     regexp.MustCompile(`TxTokenTTL`),
		"a typed nil router would not be nil":   regexp.MustCompile(`var peers \*clusterdispatch\.PeerRouter`),
		"the temporary one-try fencing wrapper": regexp.MustCompile(`onceFenced`),
	} {
		if re.Match(src) {
			t.Errorf("app.go still has: %s (%s)", name, re)
		}
	}
	// One arbiter and one lock registry per process: the owner's loop, the join
	// layer and the callback doors judge a callback by the same state.
	for _, once := range []*regexp.Regexp{
		regexp.MustCompile(`fence\.New\(`),
		regexp.MustCompile(`txgate\.New\(\)`),
	} {
		if n := len(once.FindAll(src, -1)); n != 1 {
			t.Errorf("app.go builds %s %d times, want exactly 1", once, n)
		}
	}
}
