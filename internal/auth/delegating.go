package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// ErrAuthenticationFailed is the generic client-facing auth failure sentinel.
// Every Authenticate failure path returns this sentinel verbatim — without
// any per-branch suffix — so that err.Error() is always the exact same string
// "authentication failed" and a probing client cannot distinguish "no token
// sent" from "token is wrong" via the response body (issues #100, #68 item 12).
//
// The specific failure mode is logged server-side via a single slog.Warn
// record carrying a structured `reason` field (see Authenticate). This keeps
// operator observability intact without leaking enumeration signal to
// callers.
var ErrAuthenticationFailed = errors.New("authentication failed")

// Reason slugs used for the structured `reason` field of the per-failure
// slog.Warn record. Stable values — operators key alerts on these.
const (
	authReasonMissingHeader = "missing-header"
	authReasonInvalidFormat = "invalid-format"
	authReasonEmptyBearer   = "empty-bearer"
	authReasonTokenInvalid  = "token-invalid"
)

// DelegatingAuthenticator implements contract.AuthenticationService by delegating
// token validation to a Validator. The concrete validator is typically a
// *JWKSValidator (first-party-only mode) or a *ChainedValidator (OIDC mode).
type DelegatingAuthenticator struct {
	validator Validator
}

// NewDelegatingAuthenticator creates a new DelegatingAuthenticator.
func NewDelegatingAuthenticator(validator Validator) *DelegatingAuthenticator {
	return &DelegatingAuthenticator{validator: validator}
}

// Authenticate extracts a Bearer token from the Authorization header and validates it.
//
// On any failure path it returns the generic ErrAuthenticationFailed sentinel
// (no wrapped detail) so the caller-visible error string carries no
// enumeration signal, and emits a single slog.Warn record with a structured
// `reason` field plus operator-relevant context (remote address, request
// method/path). No token material or other PII is logged.
func (a *DelegatingAuthenticator) Authenticate(_ context.Context, r *http.Request) (*spi.UserContext, error) {
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		logAuthFailure(r, authReasonMissingHeader, nil)
		return nil, ErrAuthenticationFailed
	}

	if !strings.HasPrefix(authHeader, "Bearer ") {
		logAuthFailure(r, authReasonInvalidFormat, nil)
		return nil, ErrAuthenticationFailed
	}

	token := strings.TrimPrefix(authHeader, "Bearer ")
	if token == "" {
		logAuthFailure(r, authReasonEmptyBearer, nil)
		return nil, ErrAuthenticationFailed
	}

	uc, err := a.validator.Validate(token)
	if err != nil {
		logAuthFailure(r, authReasonTokenInvalid, err)
		return nil, ErrAuthenticationFailed
	}

	return uc, nil
}

// logAuthFailure emits exactly one structured slog.Warn record describing the
// authentication failure. The detail err (if any) is the validator's
// description of *why* the token failed — this never contains the raw token
// (validator errors only reference kid/issuer/aud/algorithm); see
// internal/auth/validator.go and EnsureAlgRS256.
func logAuthFailure(r *http.Request, reason string, detail error) {
	attrs := []any{
		"pkg", "auth",
		"reason", reason,
		"remote_addr", r.RemoteAddr,
		"method", r.Method,
		"path", r.URL.Path,
	}
	if detail != nil {
		attrs = append(attrs, "detail", detail.Error())
	}
	slog.Warn("authentication failed", attrs...)
}
