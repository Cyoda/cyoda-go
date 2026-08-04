// Package goroutinesafety holds a static guard over the E2E suite's own test
// sources: a *testing.T must not be used from a goroutine other than the one
// running the test, because Fatal/FailNow/Skip stop only the calling goroutine.
//
// The guard is syntactic and covers `go` statements in package
// github.com/cyoda-platform/cyoda-go/internal/e2e only. See the doc comment on
// TestNoTestingTUseInGoroutines for the exact scope and its known blind spots
// — a green run means "no `go` statement hands over a t", not "no goroutine
// anywhere can reach a t".
//
// It lives in its own package (rather than in internal/e2e) so it runs under
// `go test -short`, which the E2E TestMain exits early from — the guard is
// pure source analysis and needs neither Docker nor a database.
package goroutinesafety
