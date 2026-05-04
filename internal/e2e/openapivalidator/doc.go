/*
Package openapivalidator validates HTTP responses captured during E2E tests
against the OpenAPI 3.1 spec embedded in github.com/cyoda-platform/cyoda-go/api.

Architecture:

  - middleware.go wraps the test server's http.Handler. Every response is
    captured via tee.go (an http.ResponseWriter that delegates Flusher,
    Hijacker, and ReaderFrom to the underlying writer).
  - validator.go runs the captured response through kin-openapi's
    openapi3filter.ValidateResponse with IncludeResponseStatus=true and
    MultiError=true.
  - collector.go appends mismatches to a process-level slice (mutex-guarded).
  - report.go writes the collector's contents to
    internal/e2e/_openapi-conformance-report.md after the suite ends.
  - testname.go plumbs the current *testing.T through request context so
    the middleware can call t.Errorf in -run-filtered enforce mode.

Streaming responses (application/x-ndjson) skip body validation but still
validate status code and headers. Detection uses the matched operation's
*declared* content-type for the actual status code, NOT the response's
Content-Type header (header-based detection is fragile against auto-sniffed
types and future SSE additions).

Mode is a compile-time constant (mode.go) — there is intentionally no env
var or runtime toggle. See docs/adr/0001-openapi-server-spec-conformance.md
for why.

This suite is incompatible with `go test -shuffle on`. The conformance test
detects shuffle and fails with an explanatory error.
*/
package openapivalidator
