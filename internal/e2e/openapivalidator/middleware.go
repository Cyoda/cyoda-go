package openapivalidator

import (
	"bytes"
	"flag"
	"io"
	"net/http"
	"strings"
)

// operationalPathPrefixes are paths the test server mounts but the OpenAPI
// spec deliberately doesn't document — health checks, admin endpoints,
// Scalar docs UI, OAuth discovery, the help index and its topics. Validation
// is skipped for these so they don't appear in the conformance report as "no
// spec route matches".
//
// Path prefixes are matched against r.URL.Path after the test server's
// /api context-path mount (e.g. "/api/health" matches prefix "/api/health").
//
// Keep this list to genuinely non-spec paths only. Customer endpoints that
// are spec-declared but not yet implemented must NOT appear here — they should
// go through the validator so the conformance gate (in
// zzz_openapi_conformance_test.go) sees them; a not-yet-implemented op is
// declared not-live via its `x-cyoda-status` marker in api/openapi.yaml.
var operationalPathPrefixes = []string{
	"/api/health",
	"/api/docs",
	"/api/openapi.json",
	"/api/admin/",
	"/api/.well-known/",
	"/api/help",
}

// isOperationalPath reports whether path p is an operational/admin endpoint
// that is intentionally excluded from the customer API spec.
func isOperationalPath(p string) bool {
	for _, prefix := range operationalPathPrefixes {
		if strings.HasSuffix(prefix, "/") {
			// Prefix already ends with "/" — match if request path starts with it.
			if strings.HasPrefix(p, prefix) {
				return true
			}
			continue
		}
		// Prefix has no trailing slash — match exact equality OR prefix-with-slash.
		if p == prefix || strings.HasPrefix(p, prefix+"/") {
			return true
		}
	}
	return false
}

// captureSource lets the middleware extract the captured bytes and status
// from any teeWriter variant via a single interface check.
type captureSource interface {
	captureBytes() []byte
	captureStatus() int
}

// RunFilterActive reports whether the test runner was invoked with -run set
// to any non-empty value. Computed each call (cheap; avoids package-init
// ordering surprises during tests that toggle the flag).
func RunFilterActive() bool {
	f := flag.Lookup("test.run")
	if f == nil {
		return false
	}
	return f.Value.String() != ""
}

// isCORSPreflight reports whether r is a CORS preflight. The CORS layer
// answers it; it is not a call of an API operation.
func isCORSPreflight(r *http.Request) bool {
	return r.Method == http.MethodOptions &&
		r.Header.Get("Origin") != "" &&
		r.Header.Get("Access-Control-Request-Method") != ""
}

// UndeclaredRouteProbeHeader marks a request with which a test probes, on
// purpose, a route or method the spec does not declare (for example to
// assert a 405). The middleware removes it before the application sees the
// request and does not report the route as undeclared. A marked request
// that reaches a declared route is validated as usual.
const UndeclaredRouteProbeHeader = "X-Cyoda-E2e-Undeclared-Route-Probe"

// MarkUndeclaredRouteProbe marks req as a deliberate probe of an undeclared
// route; see UndeclaredRouteProbeHeader.
func MarkUndeclaredRouteProbe(req *http.Request) {
	req.Header.Set(UndeclaredRouteProbeHeader, "true")
}

// unmatchedOperation is the Operation of a mismatch whose request matched no
// route of the spec.
const unmatchedOperation = "<unmatched>"

// NewMiddleware returns an http.Handler middleware that:
//
//  1. Wraps the response writer with a teeWriter to capture status + body.
//  2. Calls the wrapped handler.
//  3. Synthesizes an *http.Response from the captured bytes and runs it
//     through the validator.
//  4. Appends mismatches to the package-level collector, named after the
//     requesting test (TestNameHeader; see Transport).
//  5. In -run-filtered runs: also calls Errorf on the
//     requesting test (if the request named one) so it fails. Wrapped in
//     defer recover() in case the test has exited (a callback made after
//     the test returned); the mismatch is still collected, and
//     FailOnUnreported fails the run on it.
//
// Operational paths and CORS preflights are not validated. A request marked
// with UndeclaredRouteProbeHeader is not reported for matching no route.
func NewMiddleware(v *Validator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			testName, requester := requestingTest(r)
			probe := r.Header.Get(UndeclaredRouteProbeHeader) != ""
			r.Header.Del(UndeclaredRouteProbeHeader)
			if isOperationalPath(r.URL.Path) || isCORSPreflight(r) {
				// Not an API operation: an operational endpoint outside the
				// customer API spec, or a preflight the CORS layer answers.
				// Skip validation and don't record as exercised.
				next.ServeHTTP(w, r)
				return
			}

			tw := newTeeWriter(w)
			next.ServeHTTP(tw, r)

			cs, ok := tw.(captureSource)
			if !ok {
				// Should never happen — every teeWriter variant satisfies
				// captureSource via embedded *teeWriter.
				return
			}

			// Build a synthetic *http.Response for the validator.
			resp := &http.Response{
				StatusCode: cs.captureStatus(),
				Header:     w.Header(),
				Body:       io.NopCloser(bytes.NewReader(cs.captureBytes())),
				Request:    r,
			}
			mismatches := v.Validate(r.Context(), r, resp)
			for _, m := range mismatches {
				if probe && m.Operation == unmatchedOperation {
					continue
				}
				m.TestName = "unknown"
				if testName != "" {
					m.TestName = testName
				}
				defaultCollector.append(m)

				if requester != nil && RunFilterActive() {
					func() {
						defer func() { _ = recover() }()
						requester.Errorf("openapi conformance: %s %s -> %d: %s",
							m.Method, m.Path, m.Status,
							strings.TrimSpace(m.Reason))
					}()
				}
			}
		})
	}
}
