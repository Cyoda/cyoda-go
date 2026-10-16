package openapivalidator

import (
	"context"
	"net/http"
	"sync"
)

// Reporter is the part of *testing.T the middleware reports a mismatch to.
type Reporter interface {
	Name() string
	Errorf(format string, args ...any)
}

// TestNameHeader carries the requesting test's name from the e2e client to
// the validator middleware. A context value cannot cross the TCP connection
// between the test and its httptest server; a header can. The middleware
// removes it before the application sees the request.
const TestNameHeader = "X-Cyoda-E2e-Test"

type testTKey struct{}

// reporters maps a test name sent in TestNameHeader to the test that sent
// it. Transport fills it at send time; the middleware reads it.
var reporters sync.Map

// WithTestT attaches the requesting test to ctx. A request made with ctx
// through a client whose RoundTripper is Transport names the test to the
// validator middleware, which reports a -run-filtered enforce-mode mismatch
// to it.
func WithTestT(ctx context.Context, t Reporter) context.Context {
	return context.WithValue(ctx, testTKey{}, t)
}

// TestTFromContext returns the test attached via WithTestT, or nil if none
// is present.
func TestTFromContext(ctx context.Context) Reporter {
	t, _ := ctx.Value(testTKey{}).(Reporter)
	return t
}

// Transport returns a RoundTripper that sends, in TestNameHeader, the name
// of the test attached to the request's context. Requests without one go
// out unchanged.
func Transport(base http.RoundTripper) http.RoundTripper {
	return attributingTransport{base: base}
}

type attributingTransport struct{ base http.RoundTripper }

func (a attributingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t := TestTFromContext(req.Context())
	if t == nil {
		return a.base.RoundTrip(req)
	}
	reporters.Store(t.Name(), t)
	// A RoundTripper must not modify the caller's request.
	req = req.Clone(req.Context())
	req.Header.Set(TestNameHeader, t.Name())
	return a.base.RoundTrip(req)
}

// requestingTest removes TestNameHeader from r and returns the test name it
// carries ("" when none) and the test registered under that name (nil when
// none is).
func requestingTest(r *http.Request) (string, Reporter) {
	name := r.Header.Get(TestNameHeader)
	if name == "" {
		return "", nil
	}
	r.Header.Del(TestNameHeader)
	t, _ := reporters.Load(name)
	rep, _ := t.(Reporter)
	return name, rep
}
