package auth

import "errors"

// Validator chain sentinels per spec D3 + D17 (see
// docs/superpowers/specs/2026-06-16-284-oidc-providers-design.md).
// ChainedValidator falls through ONLY on ErrUnknownKID; all others hard-fail.
var (
	// ErrUnknownKID indicates the validator's KeySource did not recognise the
	// token's `kid` header. The ChainedValidator treats this as "not mine,
	// try the next validator." Surfaces to the bearer-auth caller as 401
	// only after chain exhaustion.
	ErrUnknownKID = errors.New("auth: unknown kid")

	// ErrIssuerMismatch indicates the validator's KeySource resolved the
	// token's `kid` but the `iss` claim does not match the validator's
	// expected issuer (bytewise comparison per OIDC Core 1.0 §2). Hard-fail;
	// the chain does NOT consult subsequent validators.
	ErrIssuerMismatch = errors.New("auth: issuer mismatch")

	// ErrSignatureFailure indicates signature verification failed after a
	// successful key resolution. Hard-fail. For OIDCValidator, the caller
	// is contractually required to invoke Registry.EvictKidEntry to self-
	// heal the kidIndex (D6).
	ErrSignatureFailure = errors.New("auth: signature verification failed")

	// ErrClaimsFailure indicates a standard-claims validation failure:
	// exp, nbf, aud, sub, or alg. The wrapping error message may include
	// a subcode (expired / nbf / audience / missing_sub / invalid_sub /
	// unsupported_alg) per spec §3.4 and §4.3.
	ErrClaimsFailure = errors.New("auth: claims validation failed")

	// ErrTokenPreTransition indicates the token's `iat` claim predates the
	// resolving provider's CreatedAt by more than the 30s clock skew (D17).
	// Closes the accidental-spillover case during cross-tenant URI
	// re-registration. Hard-fail.
	ErrTokenPreTransition = errors.New("auth: token issued before provider creation")

	// ErrJWKSUnavailable indicates a transient JWKS-endpoint failure during
	// resolution. Surfaces to the bearer-auth caller as 503 + Retry-After.
	// Hard-fail (does not silently fall through to subsequent validators).
	ErrJWKSUnavailable = errors.New("auth: jwks unavailable")
)
