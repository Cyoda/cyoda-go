// Package servervalidation holds a static guard over the E2E suite's own test
// sources: every server the suite starts that serves the cyoda API sits behind
// the OpenAPI conformance validator (internal/e2e's validated helper), so that
// every response the suite receives is checked against the spec.
//
// It lives in its own package (rather than in internal/e2e) so it runs under
// `go test -short`, which the E2E TestMain exits early from — the guard is
// pure source analysis and needs neither Docker nor a database.
package servervalidation
