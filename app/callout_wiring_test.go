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
		"the Coordinator is built":                regexp.MustCompile(`extProc = callout\.New\(`),
		"the tracing decorator wraps the outside": regexp.MustCompile(`(?s)extProc = callout\.New\(.*extProc = observability\.NewTracingExternalProcessingService\(extProc,`),
		"the peer router is held as the interface, so a single node has an untyped nil": regexp.MustCompile(`var peers callout\.PeerRouter`),
		"one transaction coordination per process, chosen for the backend":              regexp.MustCompile(`(?s)txcoord\.Select\(startupCtx, a\.storeFactory, func\(\) txcoord\.Coordination \{\s*return txcoord\.New\(a\.transactionManager\)\s*\}\).*a\.coord = coord`),
		"the dispatcher mints passes by the backend's location independence":            regexp.MustCompile(`NewProcessorDispatcher\([^\n]*, locationIndependent\)`),
		"the hand-over server accepts an ownerless hand-over by it":                     regexp.MustCompile(`NewDispatchHandler\(localDispatcher, peerAuth, locationIndependent\)`),
		"the owner's loop registers its callouts with it":                               regexp.MustCompile(`callout\.New\(localDispatcher, a\.memberRegistry, peers, a\.coord,`),
		"the engine opens its processor scopes in it":                                   regexp.MustCompile(`workflow\.WithCoordination\(a\.coord\)`),
		"the join layer opens its callback scopes in it":                                regexp.MustCompile(`txjoin\.NewJoiner\(a\.tokenSigner, a\.transactionManager, a\.coord,`),
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
	// One coordination per process: the owner's loop, the engine, the join
	// layer and the callback doors judge a callback by the same state. app.go
	// builds no fence or gate of its own.
	for _, n := range []struct {
		re   *regexp.Regexp
		want int
	}{
		{regexp.MustCompile(`txcoord\.New\(`), 1},
		{regexp.MustCompile(`fence\.New\(`), 0},
		{regexp.MustCompile(`txgate\.New\(\)`), 0},
	} {
		if got := len(n.re.FindAll(src, -1)); got != n.want {
			t.Errorf("app.go builds %s %d times, want %d", n.re, got, n.want)
		}
	}
}
