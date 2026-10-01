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

	// ErrKIDCannotVerify indicates the token's `kid` names a key pair of this
	// node's signing-key store that may not verify now (see
	// ErrKeyPairCannotVerify). Hard-fail: the kid is cyoda-go's, so the
	// chain must not let a later validator resolve it.
	ErrKIDCannotVerify = errors.New("auth: kid names a key pair that cannot verify now")

	// ErrIssuerMismatch indicates the `iss` claim does not match the expected
	// issuer (bytewise comparison per OIDC Core 1.0 §2). The first-party
	// validator returns it after resolving the `kid`; the OIDC registry
	// returns it when, after a search of every provider, none resolves the
	// token, none failed transiently (that is ErrJWKSUnavailable), and at
	// least one active, discovered provider was rejected by issuer, whether
	// or not it publishes the `kid`. Hard-fail; the chain does NOT consult
	// subsequent validators.
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
	// resolution. Surfaces to the bearer-auth caller as the uniform 401, like
	// every other authentication failure. Hard-fail (does not silently fall
	// through to subsequent validators).
	ErrJWKSUnavailable = errors.New("auth: jwks unavailable")
)
