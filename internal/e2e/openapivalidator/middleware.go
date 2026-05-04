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
// Scalar docs UI, OAuth discovery. Validation is skipped for these so they
// don't appear in the conformance report as "no spec route matches".
//
// Path prefixes are matched against r.URL.Path after the test server's
// /api context-path mount (e.g. "/api/health" matches prefix "/api/health").
//
// Keep this list to genuinely non-spec paths only. Customer endpoints that
// are spec-declared but not yet implemented (e.g. /oauth/keys/, /account/m2m)
// must NOT appear here — they should go through the validator so that
// knownUncoveredOps (in zzz_openapi_conformance_test.go) remains the single
// authoritative source of "intentionally uncovered" operations.
var operationalPathPrefixes = []string{
	"/api/health",
	"/api/docs",
	"/api/openapi.json",
	"/api/admin/",
	"/api/.well-known/",
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

// NewMiddleware returns an http.Handler middleware that:
//
//  1. Wraps the response writer with a teeWriter to capture status + body.
//  2. Calls the wrapped handler.
//  3. Synthesizes an *http.Response from the captured bytes and runs it
//     through the validator.
//  4. Appends mismatches to the package-level collector.
//  5. In ModeEnforce + -run-filtered runs: also calls t.Errorf on the
//     captured *testing.T (if any) so the requesting test fails immediately.
//     Wrapped in defer recover() in case the test has exited (fire-and-
//     forget pattern; captured *T may no longer be valid).
func NewMiddleware(v *Validator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isOperationalPath(r.URL.Path) {
				// Operational endpoint; not in the customer API spec. Skip validation
				// and don't record as exercised (these aren't operationIds).
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
				if t := TestTFromContext(r.Context()); t != nil {
					m.TestName = t.Name()
				} else {
					m.TestName = "unknown"
				}
				defaultCollector.append(m)

				if Mode == ModeEnforce && RunFilterActive() {
					func() {
						defer func() { _ = recover() }()
						if t := TestTFromContext(r.Context()); t != nil {
							t.Errorf("openapi conformance: %s %s -> %d: %s",
								m.Method, m.Path, m.Status,
								strings.TrimSpace(m.Reason))
						}
					}()
				}
			}
		})
	}
}
