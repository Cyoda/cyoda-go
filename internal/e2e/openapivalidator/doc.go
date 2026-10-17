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
  - testname.go names the requesting test to the middleware: the client's
    Transport sends the test attached to the request context as a header,
    so the report names the test and, in a -run-filtered run, the
    middleware fails it. FailOnUnreported (collector.go) fails a run that
    collected mismatches no test reported.

The validator checks the spec through the view of view.go (ApplyView), and
recursive.go checks the values kin-openapi accepts unchecked where a schema
recurs. The api/ tests check examples and samples the same way: value.go
(ValidateValue) runs kin-openapi and that recursive walk on one value, and
ComponentUse gives the schema a use of a component stands for in the view.

Streaming responses (application/x-ndjson) skip body validation but still
validate status code and headers. Detection uses the matched operation's
*declared* content-type for the actual status code, NOT the response's
Content-Type header (header-based detection is fragile against auto-sniffed
types and future SSE additions).

A mismatch always fails the run; there is no report-only mode. See
docs/adr/0001-openapi-server-spec-conformance.md for the validator's design.

This suite is incompatible with `go test -shuffle on`. The conformance test
detects shuffle and fails with an explanatory error.
*/
package openapivalidator
