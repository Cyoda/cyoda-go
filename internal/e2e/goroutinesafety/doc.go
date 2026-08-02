// Package goroutinesafety holds a static guard over the E2E suite's own test
// sources: a *testing.T must not be used from a goroutine other than the one
// running the test.
//
// It lives in its own package (rather than in internal/e2e) so it runs under
// `go test -short`, which the E2E TestMain exits early from — the guard is
// pure source analysis and needs neither Docker nor a database.
package goroutinesafety
