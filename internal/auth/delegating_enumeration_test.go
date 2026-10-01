package auth_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/auth"
)

// TestDelegatingAuthenticator_ErrorsAreWrappedAsAuthFailed is the regression
// test for the uniform-failure contract. All four Authenticate failure
// branches must:
//   - return the unwrapped sentinel `ErrAuthenticationFailed` so that
//     err.Error() is exactly "authentication failed" — no per-branch suffix
//     leaks to a probing client and lets them distinguish "no token sent"
//     from "token is wrong" (user-enumeration mitigation).
//   - errors.Is(err, auth.ErrAuthenticationFailed) returns true for every
//     branch so callers can detect auth failure generically.
//
// The specific failure reason is now carried by the structured slog.Warn
// record emitted from Authenticate (see TestDelegatingAuthenticator_LogsStructuredReason).
func TestDelegatingAuthenticator_ErrorsAreWrappedAsAuthFailed(t *testing.T) {
	validator := auth.NewValidatorFromSource(staticKeySource{}, "cyoda")
	authn := auth.NewDelegatingAuthenticator(validator)

	cases := []struct {
		name      string
		setHeader func(r *http.Request)
	}{
		{
			name:      "missing Authorization header",
			setHeader: func(r *http.Request) {},
		},
		{
			name: "non-Bearer scheme",
			setHeader: func(r *http.Request) {
				r.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
			},
		},
		{
			name: "empty bearer token",
			setHeader: func(r *http.Request) {
				r.Header.Set("Authorization", "Bearer ")
			},
		},
		{
			name: "invalid token value",
			setHeader: func(r *http.Request) {
				r.Header.Set("Authorization", "Bearer not.a.real.token")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
			tc.setHeader(req)

			_, err := authn.Authenticate(context.Background(), req)
			if err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
			if !errors.Is(err, auth.ErrAuthenticationFailed) {
				t.Errorf("errors.Is(err, ErrAuthenticationFailed) = false; err = %v", err)
			}
			if err.Error() != "authentication failed" {
				t.Errorf("err.Error() = %q; want exactly \"authentication failed\" so the message is safe to surface and carries no enumeration signal", err.Error())
			}
		})
	}
}
