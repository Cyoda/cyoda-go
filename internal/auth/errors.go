package auth

import "errors"

// Validation-failure sentinels. DelegatingAuthenticator treats any Validate
// error identically (uniform 401 to the caller); these sentinels exist so
// logAuthFailure's detail field documents *why* validation failed, and so
// tests can distinguish failure causes.
var (
	// ErrIssuerMismatch indicates the `iss` claim does not match the
	// validator's configured issuer (bytewise comparison).
	ErrIssuerMismatch = errors.New("auth: issuer mismatch")

	// ErrSignatureFailure indicates signature verification failed after a
	// successful key resolution.
	ErrSignatureFailure = errors.New("auth: signature verification failed")

	// ErrClaimsFailure indicates a standard-claims validation failure:
	// exp, nbf, aud, sub, or alg. The wrapping error message may include
	// a subcode (expired / nbf / audience / missing_sub / invalid_sub /
	// unsupported_alg) per spec §3.4 and §4.3.
	ErrClaimsFailure = errors.New("auth: claims validation failed")
)
