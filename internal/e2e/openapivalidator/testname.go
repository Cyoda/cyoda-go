package openapivalidator

import (
	"context"
	"testing"
)

type testTKey struct{}

// WithTestT attaches a *testing.T to ctx so the validator middleware
// can call t.Errorf when a -run-filtered enforce-mode validation fails.
//
// E2E tests should construct their requests via the e2e.NewRequest helper
// in helpers_test.go, which calls this function and attaches the request's
// context to the http.Request.
func WithTestT(ctx context.Context, t *testing.T) context.Context {
	return context.WithValue(ctx, testTKey{}, t)
}

// TestTFromContext returns the *testing.T attached via WithTestT, or nil
// if none is present (e.g. requests issued without going through the
// helper, or after the test has exited).
func TestTFromContext(ctx context.Context) *testing.T {
	t, _ := ctx.Value(testTKey{}).(*testing.T)
	return t
}
