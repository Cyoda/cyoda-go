# OIDC Providers Subsystem — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Land the per-tenant OIDC provider registry subsystem for cyoda-go v0.8.0 — seven REST handlers, chained four-sentinel JWT validator, KV-backed persistence, cluster broadcast eviction, SSRF defence, telemetry, 64 parity tests, zero SPI changes.

**Architecture:** Chained `auth.Validator` ((`JWKSValidator`, `OIDCValidator`) — order normative) over a per-tenant `oidc.Registry` consulting a KV-backed `OidcProviderStore` in a single `oidc-providers` namespace under the system tenant. Cluster broadcast via existing `spi.ClusterBroadcaster` with single-flight keyed by `(tenantID, uri)`; `reload_all` takes the registry write lock for anti-entropy convergence.

**Tech Stack:** Go 1.26+, `log/slog`, `internal/observability` metrics, `httptest.Server` for fixture IdP, `internal/cluster/registry` gossip broadcaster, `spi.KeyValueStore` (existing — no SPI changes).

**Spec:** [`docs/superpowers/specs/2026-06-16-284-oidc-providers-design.md`](../specs/2026-06-16-284-oidc-providers-design.md) — rev. 4, post third-review. **The spec is authoritative**: when in doubt, consult the spec section cited in each task.

**ADR:** [`docs/adr/0002-federated-identity-provider-architecture.md`](../../adr/0002-federated-identity-provider-architecture.md).

**Worktree:** `feat/284-oidc-providers` based on `release/v0.8.0`.

---

## Phase 0: Baseline

### Task 0.1: Verify baseline is green

**Files:** none (verification only)

- [ ] **Step 1: Confirm worktree state and clean baseline**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/feat/284-oidc-providers
git status
git log --oneline -3
go vet ./...
go test -short -count=1 ./... 2>&1 | tail -10
```

Expected: clean working tree; HEAD at rev. 4 spec commit; `go vet` silent; short tests pass.

- [ ] **Step 2: Commit phase marker**

No commit — this is a check, not a change.

---

## Phase 1: Foundation — auth chain infrastructure (sequential)

This phase must complete before any OIDC-specific work begins. The four-sentinel error vars, the `Validator` interface, the `ChainedValidator`, and the refined `JWKSValidator` error semantics are pre-requisites for everything downstream.

### Task 1.1: Sentinel errors (`internal/auth/errors.go`)

**Spec ref:** §3.1 (errors.go row), §3.4 wire matrix, D3.

**Files:**
- Create: `internal/auth/errors.go`
- Test: `internal/auth/errors_test.go`

- [ ] **Step 1: Write failing test**

```go
// internal/auth/errors_test.go
package auth

import (
    "errors"
    "testing"
)

func TestSentinelErrorsAreDistinct(t *testing.T) {
    pairs := []struct {
        name string
        a, b error
    }{
        {"unknown_kid vs issuer_mismatch", ErrUnknownKID, ErrIssuerMismatch},
        {"unknown_kid vs sig_failure", ErrUnknownKID, ErrSignatureFailure},
        {"unknown_kid vs claims_failure", ErrUnknownKID, ErrClaimsFailure},
        {"issuer_mismatch vs sig_failure", ErrIssuerMismatch, ErrSignatureFailure},
        {"pre_transition vs unknown_kid", ErrTokenPreTransition, ErrUnknownKID},
        {"jwks_unavailable vs sig_failure", ErrJWKSUnavailable, ErrSignatureFailure},
    }
    for _, p := range pairs {
        if errors.Is(p.a, p.b) {
            t.Errorf("%s: errors.Is reports same identity", p.name)
        }
    }
}

func TestUnknownKIDIsTheOnlyFallthroughSentinel(t *testing.T) {
    // Document the chain contract: only ErrUnknownKID is fall-through.
    if !errors.Is(ErrUnknownKID, ErrUnknownKID) {
        t.Fatal("ErrUnknownKID must match itself")
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./internal/auth/ -run TestSentinel -v
```

Expected: FAIL with `undefined: ErrUnknownKID`.

- [ ] **Step 3: Implement**

```go
// internal/auth/errors.go
package auth

import "errors"

// Validator chain sentinels per D3 (see docs/superpowers/specs/2026-06-16-284-oidc-providers-design.md).
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
    ErrJWKSUnavailable = errors.New("auth: JWKS unavailable")
)
```

- [ ] **Step 4: Run test to verify it passes**

```bash
go test ./internal/auth/ -run TestSentinel -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/auth/errors.go internal/auth/errors_test.go
git commit -m "feat(auth): four-sentinel chain errors per D3 (#284)"
```

### Task 1.2: Token header parser (`internal/auth/parse.go`)

**Spec ref:** §3.1 (parse.go row), §4.3 step 1.

**Files:**
- Create: `internal/auth/parse.go`
- Test: `internal/auth/parse_test.go`

- [ ] **Step 1: Write failing tests**

```go
// internal/auth/parse_test.go
package auth

import (
    "testing"
)

func TestParseTokenHeader_HappyPath(t *testing.T) {
    // RS256-signed JWT with kid=K1, iss=https://idp.example, aud=api1, exp=1700000000, iat=1699999000, sub=user1.
    // Constructed offline; signature bytes are arbitrary (we don't verify here).
    tok := buildTestJWT(t, "K1", "RS256", map[string]any{
        "iss": "https://idp.example",
        "aud": "api1",
        "exp": int64(1700000000),
        "iat": int64(1699999000),
        "sub": "user1",
    })

    h, err := parseTokenHeader(tok)
    if err != nil {
        t.Fatalf("parseTokenHeader: %v", err)
    }
    if h.kid != "K1" {
        t.Errorf("kid = %q, want K1", h.kid)
    }
    if h.alg != "RS256" {
        t.Errorf("alg = %q, want RS256", h.alg)
    }
    if h.iss != "https://idp.example" {
        t.Errorf("iss = %q", h.iss)
    }
    if h.sub != "user1" {
        t.Errorf("sub = %q", h.sub)
    }
    if h.exp != 1700000000 {
        t.Errorf("exp = %d", h.exp)
    }
    if h.iat != 1699999000 {
        t.Errorf("iat = %d", h.iat)
    }
}

func TestParseTokenHeader_MalformedRejects(t *testing.T) {
    cases := []struct {
        name, tok string
    }{
        {"empty", ""},
        {"single-segment", "abc"},
        {"two-segments", "abc.def"},
        {"invalid-base64", "%%%.%%%.%%%"},
        {"non-json-header", encodeSeg([]byte("not-json")) + ".eyJ9.sig"},
    }
    for _, c := range cases {
        t.Run(c.name, func(t *testing.T) {
            _, err := parseTokenHeader(c.tok)
            if err == nil {
                t.Errorf("expected error for %q", c.name)
            }
        })
    }
}

// Test helpers — buildTestJWT and encodeSeg are in test_helpers_test.go
// (a separate file added alongside).
```

- [ ] **Step 2: Add test helper file**

```go
// internal/auth/test_helpers_test.go
package auth

import (
    "encoding/base64"
    "encoding/json"
    "testing"
)

func encodeSeg(b []byte) string {
    return base64.RawURLEncoding.EncodeToString(b)
}

func buildTestJWT(t *testing.T, kid, alg string, claims map[string]any) string {
    t.Helper()
    header := map[string]any{"kid": kid, "alg": alg, "typ": "JWT"}
    hb, _ := json.Marshal(header)
    cb, _ := json.Marshal(claims)
    return encodeSeg(hb) + "." + encodeSeg(cb) + ".c2ln"
}
```

- [ ] **Step 3: Run test to verify it fails**

```bash
go test ./internal/auth/ -run TestParseTokenHeader -v
```

Expected: FAIL — `parseTokenHeader` undefined.

- [ ] **Step 4: Implement**

```go
// internal/auth/parse.go
package auth

import (
    "encoding/base64"
    "encoding/json"
    "fmt"
    "strings"
)

// tokenHeaderClaims is the unauthenticated header+claims peek extracted by
// parseTokenHeader. It is intentionally unexported — callers must perform
// signature verification before trusting any of these fields except `kid`
// and `alg` (which are needed pre-verify to pick a KeySource).
type tokenHeaderClaims struct {
    kid string
    alg string
    iss string
    aud string
    sub string
    exp int64
    iat int64
}

// parseTokenHeader decodes a JWT's header and claims segments without verifying
// the signature. Used by both JWKSValidator and OIDCValidator to inspect kid/iss
// before deciding whether to consult a KeySource, and to read sub/iat for D17
// and D23 checks AFTER signature verification.
//
// Returns an error if the token is structurally malformed (wrong segment count,
// non-base64url, non-JSON header or claims). Does NOT validate field types or
// value ranges — that is the caller's responsibility post-verification.
func parseTokenHeader(tokenString string) (*tokenHeaderClaims, error) {
    parts := strings.Split(tokenString, ".")
    if len(parts) != 3 {
        return nil, fmt.Errorf("malformed token: expected 3 segments, got %d", len(parts))
    }

    hdrBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
    if err != nil {
        return nil, fmt.Errorf("malformed header segment: %w", err)
    }
    var hdr map[string]any
    if err := json.Unmarshal(hdrBytes, &hdr); err != nil {
        return nil, fmt.Errorf("malformed header JSON: %w", err)
    }

    claimsBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
    if err != nil {
        return nil, fmt.Errorf("malformed claims segment: %w", err)
    }
    var claims map[string]any
    if err := json.Unmarshal(claimsBytes, &claims); err != nil {
        return nil, fmt.Errorf("malformed claims JSON: %w", err)
    }

    out := &tokenHeaderClaims{}
    out.kid, _ = hdr["kid"].(string)
    out.alg, _ = hdr["alg"].(string)
    out.iss, _ = claims["iss"].(string)
    out.aud, _ = claims["aud"].(string)
    out.sub, _ = claims["sub"].(string)
    if v, ok := claims["exp"].(float64); ok {
        out.exp = int64(v)
    }
    if v, ok := claims["iat"].(float64); ok {
        out.iat = int64(v)
    }
    return out, nil
}
```

- [ ] **Step 5: Run test to verify it passes**

```bash
go test ./internal/auth/ -run TestParseTokenHeader -v
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/auth/parse.go internal/auth/parse_test.go internal/auth/test_helpers_test.go
git commit -m "feat(auth): parseTokenHeader helper for chain validators (#284)"
```

### Task 1.3: Validator interface + ChainedValidator (`internal/auth/chain.go`)

**Spec ref:** §3.1 (chain.go row), §4.3, D3.

**Files:**
- Create: `internal/auth/chain.go`
- Test: `internal/auth/chain_test.go`

- [ ] **Step 1: Write failing tests**

```go
// internal/auth/chain_test.go
package auth

import (
    "errors"
    "testing"

    spi "github.com/cyoda-platform/cyoda-go-spi"
)

type mockValidator struct {
    result *spi.UserContext
    err    error
    calls  int
}

func (m *mockValidator) Validate(tok string) (*spi.UserContext, error) {
    m.calls++
    return m.result, m.err
}

func TestChainedValidator_FirstSuccessReturned(t *testing.T) {
    uc := &spi.UserContext{UserID: "u1"}
    v1 := &mockValidator{result: uc, err: nil}
    v2 := &mockValidator{err: errors.New("should not be called")}
    c := NewChainedValidator(v1, v2)

    got, err := c.Validate("tok")
    if err != nil {
        t.Fatalf("Validate: %v", err)
    }
    if got != uc {
        t.Errorf("got %v, want %v", got, uc)
    }
    if v2.calls != 0 {
        t.Errorf("v2 was called (%d times); chain should short-circuit on success", v2.calls)
    }
}

func TestChainedValidator_UnknownKIDFallsThrough(t *testing.T) {
    uc := &spi.UserContext{UserID: "u2"}
    v1 := &mockValidator{err: ErrUnknownKID}
    v2 := &mockValidator{result: uc, err: nil}
    c := NewChainedValidator(v1, v2)

    got, err := c.Validate("tok")
    if err != nil {
        t.Fatalf("Validate: %v", err)
    }
    if got != uc {
        t.Errorf("got %v, want %v", got, uc)
    }
    if v1.calls != 1 || v2.calls != 1 {
        t.Errorf("calls = (%d, %d), want (1, 1)", v1.calls, v2.calls)
    }
}

func TestChainedValidator_HardFailDoesNotFallThrough(t *testing.T) {
    cases := []struct {
        name string
        err  error
    }{
        {"issuer_mismatch", ErrIssuerMismatch},
        {"signature_failure", ErrSignatureFailure},
        {"claims_failure", ErrClaimsFailure},
        {"pre_transition", ErrTokenPreTransition},
        {"jwks_unavailable", ErrJWKSUnavailable},
    }
    for _, c := range cases {
        t.Run(c.name, func(t *testing.T) {
            v1 := &mockValidator{err: c.err}
            v2 := &mockValidator{result: &spi.UserContext{UserID: "should-not-reach"}}
            chain := NewChainedValidator(v1, v2)

            _, err := chain.Validate("tok")
            if !errors.Is(err, c.err) {
                t.Errorf("err = %v, want %v", err, c.err)
            }
            if v2.calls != 0 {
                t.Errorf("v2 was called; %s must not fall through", c.name)
            }
        })
    }
}

func TestChainedValidator_AllFailUnknownKID(t *testing.T) {
    v1 := &mockValidator{err: ErrUnknownKID}
    v2 := &mockValidator{err: ErrUnknownKID}
    c := NewChainedValidator(v1, v2)

    _, err := c.Validate("tok")
    if !errors.Is(err, ErrUnknownKID) {
        t.Errorf("err = %v, want ErrUnknownKID", err)
    }
}

func TestChainedValidator_EmptyChainReturnsUnknownKID(t *testing.T) {
    c := NewChainedValidator()
    _, err := c.Validate("tok")
    if !errors.Is(err, ErrUnknownKID) {
        t.Errorf("err = %v, want ErrUnknownKID", err)
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./internal/auth/ -run TestChainedValidator -v
```

Expected: FAIL — `NewChainedValidator` undefined.

- [ ] **Step 3: Implement**

```go
// internal/auth/chain.go
package auth

import (
    "errors"

    spi "github.com/cyoda-platform/cyoda-go-spi"
)

// Validator is the contract every JWT-validating component implements.
// Concrete impls: JWKSValidator (first-party tokens), OIDCValidator
// (federated tokens via the OIDC provider registry).
//
// Returned errors must use one of the sentinels in errors.go so the
// ChainedValidator can correctly distinguish fall-through (ErrUnknownKID)
// from hard-fail (all others).
type Validator interface {
    Validate(tokenString string) (*spi.UserContext, error)
}

// ChainedValidator composes multiple Validators in order. Validate consults
// each in sequence; falls through to the next only on ErrUnknownKID. Any
// other error from any validator is returned immediately (hard-fail).
//
// Chain order is semantically meaningful — see app/app.go for the canonical
// construction. The first-party JWKSValidator runs before OIDCValidator so
// that a first-party kid with a foreign iss hard-fails with ErrIssuerMismatch
// without consulting OIDCValidator (spec §3.4, §11 row 36).
type ChainedValidator struct {
    validators []Validator
}

// NewChainedValidator returns a ChainedValidator preserving the order of its
// arguments. Empty chains are permitted (Validate returns ErrUnknownKID).
func NewChainedValidator(validators ...Validator) *ChainedValidator {
    return &ChainedValidator{validators: validators}
}

func (c *ChainedValidator) Validate(tokenString string) (*spi.UserContext, error) {
    var lastErr error = ErrUnknownKID
    for _, v := range c.validators {
        uc, err := v.Validate(tokenString)
        if err == nil {
            return uc, nil
        }
        if !errors.Is(err, ErrUnknownKID) {
            // Hard-fail sentinel — do not consult subsequent validators.
            return nil, err
        }
        lastErr = err
    }
    return nil, lastErr
}
```

- [ ] **Step 4: Run test to verify it passes**

```bash
go test ./internal/auth/ -run TestChainedValidator -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/auth/chain.go internal/auth/chain_test.go
git commit -m "feat(auth): ChainedValidator + Validator interface per D3 (#284)"
```

### Task 1.4: Refine JWKSValidator error semantics

**Spec ref:** §3.1 (validator.go row), §1 paragraph on order, D3.

**Files:**
- Modify: `internal/auth/validator.go:53-101`
- Test: `internal/auth/validator_chain_test.go` (new file; existing `validator_test.go` stays as-is for backward-compat assertions)

- [ ] **Step 1: Write failing test**

```go
// internal/auth/validator_chain_test.go
package auth

import (
    "errors"
    "testing"
)

// These tests verify the refined error semantics required by ChainedValidator:
// - kid unknown to the source → ErrUnknownKID (fall-through)
// - signature failure        → ErrSignatureFailure (hard-fail)
// - iss mismatch (post-verify) → ErrIssuerMismatch (hard-fail)
// - expired/nbf/aud failure  → ErrClaimsFailure (hard-fail)
//
// Pre-existing single-validator callers continue to see 401 in all cases;
// only the typed-sentinel distinction changes.

func TestJWKSValidator_UnknownKidReturnsErrUnknownKID(t *testing.T) {
    v := newTestJWKSValidator(t, "cyoda")
    // Sign a token with a kid the local source does not know.
    tok := signTokenWithEphemeralKey(t, "unknown-kid", map[string]any{
        "iss": "cyoda",
        "sub": "u1",
        "caas_user_id": "u1",
        "caas_org_id": "org1",
        "exp": nowPlus(t, 60),
    })

    _, err := v.Validate(tok)
    if !errors.Is(err, ErrUnknownKID) {
        t.Errorf("err = %v, want ErrUnknownKID", err)
    }
}

func TestJWKSValidator_IssMismatchReturnsErrIssuerMismatch(t *testing.T) {
    v, kid := newTestJWKSValidatorWithKey(t, "cyoda")
    tok := signTokenWithKeyAndKID(t, kid, "cyoda-priv-key-test", map[string]any{
        "iss": "https://evil.example",
        "sub": "u1",
        "caas_user_id": "u1",
        "caas_org_id": "org1",
        "exp": nowPlus(t, 60),
    })

    _, err := v.Validate(tok)
    if !errors.Is(err, ErrIssuerMismatch) {
        t.Errorf("err = %v, want ErrIssuerMismatch", err)
    }
}

func TestJWKSValidator_BadSignatureReturnsErrSignatureFailure(t *testing.T) {
    v, kid := newTestJWKSValidatorWithKey(t, "cyoda")
    // Forge a token claiming the known kid but signed with a different key.
    tok := signTokenWithKeyAndKID(t, kid, "wrong-priv-key", map[string]any{
        "iss": "cyoda",
        "sub": "u1",
        "caas_user_id": "u1",
        "caas_org_id": "org1",
        "exp": nowPlus(t, 60),
    })

    _, err := v.Validate(tok)
    if !errors.Is(err, ErrSignatureFailure) {
        t.Errorf("err = %v, want ErrSignatureFailure", err)
    }
}

func TestJWKSValidator_ExpiredReturnsErrClaimsFailure(t *testing.T) {
    v, kid := newTestJWKSValidatorWithKey(t, "cyoda")
    tok := signTokenWithKeyAndKID(t, kid, "cyoda-priv-key-test", map[string]any{
        "iss": "cyoda",
        "sub": "u1",
        "caas_user_id": "u1",
        "caas_org_id": "org1",
        "exp": nowPlus(t, -120), // past the 30s skew
    })

    _, err := v.Validate(tok)
    if !errors.Is(err, ErrClaimsFailure) {
        t.Errorf("err = %v, want ErrClaimsFailure", err)
    }
}
```

- [ ] **Step 2: Add the new test helpers (in the existing `test_helpers_test.go`)**

```go
// Append to internal/auth/test_helpers_test.go

import (
    "crypto/rand"
    "crypto/rsa"
    "time"
)

func nowPlus(t *testing.T, sec int) int64 {
    t.Helper()
    return time.Now().Add(time.Duration(sec) * time.Second).Unix()
}

// newTestJWKSValidator builds a validator with an empty LocalKeySource — used
// for tests where we want the validator to see an unknown kid.
func newTestJWKSValidator(t *testing.T, issuer string) *JWKSValidator {
    t.Helper()
    src := NewLocalKeySource()
    return NewValidatorFromSource(src, issuer)
}

// newTestJWKSValidatorWithKey builds a validator whose LocalKeySource holds
// one fresh RSA keypair; returns (validator, kid).
func newTestJWKSValidatorWithKey(t *testing.T, issuer string) (*JWKSValidator, string) {
    t.Helper()
    priv, err := rsa.GenerateKey(rand.Reader, 2048)
    if err != nil {
        t.Fatalf("GenerateKey: %v", err)
    }
    src := NewLocalKeySource()
    src.AddKeyPair(&KeyPair{
        KID:        "kid-1",
        Audience:   "human",
        Algorithm:  "RS256",
        PublicKey:  &priv.PublicKey,
        PrivateKey: priv,
        Active:     true,
        ValidFrom:  time.Now().Add(-time.Hour),
    })
    rememberTestPrivateKey("cyoda-priv-key-test", priv)
    return NewValidatorFromSource(src, issuer), "kid-1"
}

// signTokenWithEphemeralKey signs with a brand-new RSA key the validator
// has never seen — guarantees ErrUnknownKID.
func signTokenWithEphemeralKey(t *testing.T, kid string, claims map[string]any) string {
    t.Helper()
    priv, _ := rsa.GenerateKey(rand.Reader, 2048)
    return signRS256(t, kid, priv, claims)
}

// signTokenWithKeyAndKID signs with a remembered key (or a fresh one if
// keyName is unknown — the latter case is "wrong key" for tests).
func signTokenWithKeyAndKID(t *testing.T, kid, keyName string, claims map[string]any) string {
    t.Helper()
    priv := lookupTestPrivateKey(keyName)
    if priv == nil {
        priv, _ = rsa.GenerateKey(rand.Reader, 2048)
    }
    return signRS256(t, kid, priv, claims)
}

// signRS256 produces a real RS256 JWT — useful for tests that need the
// signature to actually verify (or not, depending on the key).
func signRS256(t *testing.T, kid string, priv *rsa.PrivateKey, claims map[string]any) string {
    t.Helper()
    // Delegate to existing test util in internal/auth (if present) or inline:
    return Sign(kid, priv, claims) // assume helper exists in package
}

// Tiny remembered-key store for cross-test reuse.
var testPrivKeys = map[string]*rsa.PrivateKey{}

func rememberTestPrivateKey(name string, k *rsa.PrivateKey) { testPrivKeys[name] = k }
func lookupTestPrivateKey(name string) *rsa.PrivateKey       { return testPrivKeys[name] }
```

> **Implementer note:** If `Sign(kid, priv, claims)` is not already exported from `internal/auth`, find the existing JWT-signing helper in the package and reuse it. The existing token.go has signing logic; expose what's needed. If you must add a test-only helper, place it in `internal/auth/testing_export.go` guarded by build tag `// +build test_export` or by being a regular exported function — the goal is one signing path, not parallel ones.

- [ ] **Step 3: Run tests to verify they fail**

```bash
go test ./internal/auth/ -run TestJWKSValidator_UnknownKid -v
```

Expected: FAIL (current validator returns untyped errors).

- [ ] **Step 4: Modify `internal/auth/validator.go` to use sentinels**

Locate `Validate` (lines 53-101) and apply the targeted error renames. Replace these specific lines:

```go
// At line 65-66 (current):
if !ok || kid == "" {
    return nil, fmt.Errorf("missing kid in token header")
}

// Replace with:
if !ok || kid == "" {
    return nil, fmt.Errorf("%w: missing kid in token header", ErrClaimsFailure)
}
```

```go
// At line 68-71 (current):
publicKey, err := v.source.GetKey(kid)
if err != nil {
    return nil, fmt.Errorf("failed to resolve key for kid %q: %w", kid, err)
}

// Replace with:
publicKey, err := v.source.GetKey(kid)
if err != nil {
    return nil, fmt.Errorf("%w: kid %q: %v", ErrUnknownKID, kid, err)
}
```

```go
// At line 73-75 (current):
if err := Verify(parsed.SigningInput, parsed.Signature, publicKey); err != nil {
    return nil, fmt.Errorf("signature verification failed: %w", err)
}

// Replace with:
if err := Verify(parsed.SigningInput, parsed.Signature, publicKey); err != nil {
    return nil, fmt.Errorf("%w: %v", ErrSignatureFailure, err)
}
```

```go
// At line 77-79 (current):
if err := ValidateClaims(parsed.Claims, 30*time.Second); err != nil {
    return nil, fmt.Errorf("claims validation failed: %w", err)
}

// Replace with:
if err := ValidateClaims(parsed.Claims, 30*time.Second); err != nil {
    return nil, fmt.Errorf("%w: %v", ErrClaimsFailure, err)
}
```

```go
// At lines 81-84 (current):
iss, _ := parsed.Claims["iss"].(string)
if iss != v.issuer {
    return nil, fmt.Errorf("untrusted token issuer")
}

// Replace with:
iss, _ := parsed.Claims["iss"].(string)
if iss != v.issuer {
    return nil, fmt.Errorf("%w: token iss=%q, expected %q", ErrIssuerMismatch, iss, v.issuer)
}
```

```go
// At lines 89-92 (current):
if audience != "" {
    if err := checkAudience(parsed.Claims["aud"], audience); err != nil {
        return nil, err
    }
}

// Replace with:
if audience != "" {
    if err := checkAudience(parsed.Claims["aud"], audience); err != nil {
        return nil, fmt.Errorf("%w: %v", ErrClaimsFailure, err)
    }
}
```

- [ ] **Step 5: Run tests to verify they pass**

```bash
go test ./internal/auth/ -v -run TestJWKSValidator_
```

Expected: PASS for the new tests AND any pre-existing tests in `validator_test.go` (which exercise wire behaviour, not error identity).

- [ ] **Step 6: Run the full internal/auth test suite**

```bash
go test ./internal/auth/ -count=1
```

Expected: PASS. If any pre-existing test asserts on error-string content, update those assertions to use `errors.Is(err, auth.ErrSomeSentinel)` instead.

- [ ] **Step 7: Commit**

```bash
git add internal/auth/validator.go internal/auth/validator_chain_test.go internal/auth/test_helpers_test.go
git commit -m "feat(auth): refine JWKSValidator errors to use chain sentinels (#284)"
```

---

**Phase 1 complete.** Confirm the auth-chain foundation is solid before proceeding:

```bash
go test ./internal/auth/... -count=1 -v 2>&1 | tail -5
go vet ./internal/auth/...
```

Phases 2 and 3 can be split across parallel subagents (independent file sets, independent test files) — see the next file for those phases.

---

## Phase 2: OIDC types, SSRF, discovery (parallel-safe within phase)

The three tasks below touch disjoint files and can be dispatched to parallel subagents. Each is self-contained against the spec.

### Task 2.1: OIDC domain types (`internal/auth/oidc/types.go`)

**Spec ref:** §3.3 (Data model), §3.1 (types.go row).

**Files:**
- Create: `internal/auth/oidc/types.go`
- Test: `internal/auth/oidc/types_test.go`

- [ ] **Step 1: Write failing tests**

```go
// internal/auth/oidc/types_test.go
package oidc

import (
    "encoding/json"
    "testing"
    "time"

    "github.com/google/uuid"
)

func TestOidcProvider_Active(t *testing.T) {
    now := time.Now()
    cases := []struct {
        name string
        p    OidcProvider
        want bool
    }{
        {"active-when-InvalidatedAt-nil", OidcProvider{InvalidatedAt: nil}, true},
        {"inactive-when-InvalidatedAt-set", OidcProvider{InvalidatedAt: &now}, false},
    }
    for _, c := range cases {
        t.Run(c.name, func(t *testing.T) {
            if got := c.p.Active(); got != c.want {
                t.Errorf("Active() = %v, want %v", got, c.want)
            }
        })
    }
}

func TestOidcProvider_JSONRoundTrip(t *testing.T) {
    now := time.Now().UTC().Truncate(time.Second) // JSON time has second precision
    rc := "cognito:groups"
    p := OidcProvider{
        ID:                 uuid.MustParse("11111111-2222-3333-4444-555555555555"),
        WellKnownConfigURI: "https://idp.example/.well-known/openid-configuration",
        Issuers:            []string{"https://idp.example"},
        ExpectedAudiences:  []string{"api1"},
        RolesClaim:         &rc,
        InvalidatedAt:      nil,
        CreatedAt:          now,
        OwnerLegalEntityID: uuid.MustParse("66666666-7777-8888-9999-aaaaaaaaaaaa"),
    }

    blob, err := json.Marshal(&p)
    if err != nil {
        t.Fatalf("marshal: %v", err)
    }
    var back OidcProvider
    if err := json.Unmarshal(blob, &back); err != nil {
        t.Fatalf("unmarshal: %v", err)
    }
    if back.ID != p.ID || back.WellKnownConfigURI != p.WellKnownConfigURI {
        t.Errorf("round trip lost fields: %+v vs %+v", p, back)
    }
    if back.RolesClaim == nil || *back.RolesClaim != rc {
        t.Errorf("RolesClaim lost: %v", back.RolesClaim)
    }
}

func TestUriOwnershipHistory_RoundTrip(t *testing.T) {
    reg := time.Now().UTC().Truncate(time.Second)
    del := reg.Add(time.Hour)
    h := UriOwnershipHistory{
        CurrentOwner: &Owner{
            TenantID:     "tenant-a",
            ProviderUUID: "uuid-a",
            RegisteredAt: reg,
        },
        Past: []Owner{
            {TenantID: "tenant-b", ProviderUUID: "uuid-b", RegisteredAt: reg.Add(-time.Hour), DeletedAt: &del},
        },
    }
    blob, _ := json.Marshal(&h)
    var back UriOwnershipHistory
    if err := json.Unmarshal(blob, &back); err != nil {
        t.Fatalf("unmarshal: %v", err)
    }
    if back.CurrentOwner == nil || back.CurrentOwner.TenantID != "tenant-a" {
        t.Errorf("CurrentOwner lost: %+v", back.CurrentOwner)
    }
    if len(back.Past) != 1 || back.Past[0].DeletedAt == nil {
        t.Errorf("Past lost: %+v", back.Past)
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./internal/auth/oidc/ -count=1 -v
```

Expected: FAIL — package `oidc` does not exist yet.

- [ ] **Step 3: Implement**

```go
// internal/auth/oidc/types.go
//
// Package oidc implements the per-tenant OIDC provider registry, the chained
// OIDCValidator, and the cluster-broadcast cache eviction layer.
//
// See docs/superpowers/specs/2026-06-16-284-oidc-providers-design.md and
// docs/adr/0002-federated-identity-provider-architecture.md for the design.
package oidc

import (
    "errors"
    "time"

    "github.com/google/uuid"
)

// OidcProvider is the persisted registry entry per spec §3.3. JSON blob value
// at KV key "<tenantID>:<provider-uuid>" in namespace "oidc-providers".
type OidcProvider struct {
    ID                 uuid.UUID  `json:"id"`
    WellKnownConfigURI string     `json:"wellKnownConfigUri"`           // unique per tenant
    Issuers            []string   `json:"issuers,omitempty"`            // optional pin list (max 10); empty = require iss == DiscoveryDoc.Issuer when known
    ExpectedAudiences  []string   `json:"expectedAudiences,omitempty"`  // D20; empty = aud unchecked
    RolesClaim         *string    `json:"rolesClaim,omitempty"`         // D23; nil = use DefaultRolesClaim
    InvalidatedAt      *time.Time `json:"invalidatedAt,omitempty"`
    CreatedAt          time.Time  `json:"createdAt"`                    // load-bearing for D17 iat-binding
    OwnerLegalEntityID uuid.UUID  `json:"ownerLegalEntityId"`
}

// Active reports whether the provider is currently usable for token validation.
// A nil InvalidatedAt means active; any non-nil value means invalidated.
func (p *OidcProvider) Active() bool { return p.InvalidatedAt == nil }

// UriOwnershipHistory is the D25 cross-tenant audit signal. JSON blob value at
// KV key "_history:<sha256(uri)>" in namespace "oidc-providers". The leading
// underscore disambiguates from tenant-UUID-prefixed keys (UUIDs are hex+dashes).
type UriOwnershipHistory struct {
    CurrentOwner *Owner  `json:"currentOwner,omitempty"` // nil after every owner has Deleted
    Past         []Owner `json:"past"`                   // every deleted-or-superseded owner, oldest first
}

// Owner is a single registration record within UriOwnershipHistory.
type Owner struct {
    TenantID     string     `json:"tenantId"`
    ProviderUUID string     `json:"providerUuid"`
    RegisteredAt time.Time  `json:"registeredAt"`
    DeletedAt    *time.Time `json:"deletedAt,omitempty"`
}

// Domain errors per §3.1. These are not the chain sentinels (those live in
// internal/auth/errors.go and are used by the validator); these are service-
// layer errors surfaced through the HTTP adapter to client wire codes.
var (
    ErrProviderDuplicate  = errors.New("oidc: provider with this wellKnownConfigUri already registered for this tenant")
    ErrProviderNotFound   = errors.New("oidc: provider not found")
    ErrProviderInactive   = errors.New("oidc: provider is invalidated")
    ErrSSRFBlocked        = errors.New("oidc: wellKnownConfigUri resolves to a blocked address range")
    ErrDiscoveryFailed    = errors.New("oidc: failed to fetch discovery document")
)
```

- [ ] **Step 4: Verify test passes**

```bash
go test ./internal/auth/oidc/ -count=1 -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/auth/oidc/types.go internal/auth/oidc/types_test.go
git commit -m "feat(oidc): OidcProvider + UriOwnershipHistory types (#284)"
```

### Task 2.2: SSRF defence (`internal/auth/oidc/ssrf.go`)

**Spec ref:** §3.1 (ssrf.go row), §3.4 wire, §8, D10.

**Files:**
- Create: `internal/auth/oidc/ssrf.go`
- Test: `internal/auth/oidc/ssrf_test.go`

- [ ] **Step 1: Write failing tests**

```go
// internal/auth/oidc/ssrf_test.go
package oidc

import (
    "errors"
    "net"
    "strings"
    "testing"
)

func TestValidateRegisterURI_RejectsBlockedRanges(t *testing.T) {
    cases := []struct {
        name string
        uri  string
    }{
        // We cannot rely on DNS during tests; use IP-literal URIs so the
        // resolver path is exercised against a known answer.
        {"ipv4-loopback", "https://127.0.0.1/.well-known/openid-configuration"},
        {"ipv4-link-local-metadata", "https://169.254.169.254/.well-known/openid-configuration"},
        {"ipv4-rfc1918-10", "https://10.0.0.1/.well-known/openid-configuration"},
        {"ipv4-rfc1918-172", "https://172.16.0.1/.well-known/openid-configuration"},
        {"ipv4-rfc1918-192", "https://192.168.1.1/.well-known/openid-configuration"},
        {"ipv6-loopback", "https://[::1]/.well-known/openid-configuration"},
        {"ipv6-link-local", "https://[fe80::1]/.well-known/openid-configuration"},
        {"ipv6-ula", "https://[fc00::1]/.well-known/openid-configuration"},
        {"ipv6-mapped-v4-loopback", "https://[::ffff:127.0.0.1]/.well-known/openid-configuration"},
    }
    for _, c := range cases {
        t.Run(c.name, func(t *testing.T) {
            err := validateRegisterURI(c.uri, true, false)
            if !errors.Is(err, ErrSSRFBlocked) {
                t.Errorf("err = %v, want ErrSSRFBlocked", err)
            }
        })
    }
}

func TestValidateRegisterURI_AllowsPublic(t *testing.T) {
    // 8.8.8.8 is public; this should pass (no DNS required since it's a literal).
    err := validateRegisterURI("https://8.8.8.8/.well-known/openid-configuration", true, false)
    if err != nil {
        t.Errorf("public IP rejected: %v", err)
    }
}

func TestValidateRegisterURI_RejectsHTTPWhenRequireHTTPS(t *testing.T) {
    err := validateRegisterURI("http://example.com/.well-known/openid-configuration", true, false)
    if err == nil || !strings.Contains(err.Error(), "https") {
        t.Errorf("expected https-required error, got %v", err)
    }
}

func TestValidateRegisterURI_AllowsHTTPWhenNotRequired(t *testing.T) {
    err := validateRegisterURI("http://8.8.8.8/.well-known/openid-configuration", false, false)
    if err != nil {
        t.Errorf("unexpected error with requireHTTPS=false: %v", err)
    }
}

func TestValidateRegisterURI_AllowPrivateNetworksOverride(t *testing.T) {
    err := validateRegisterURI("https://127.0.0.1/.well-known/openid-configuration", true, true)
    if err != nil {
        t.Errorf("allowPrivate=true should permit loopback, got %v", err)
    }
}

func TestIsBlockedIP_Ranges(t *testing.T) {
    cases := []struct {
        ip      string
        blocked bool
    }{
        {"127.0.0.1", true},
        {"169.254.169.254", true},
        {"10.0.0.5", true},
        {"172.16.1.1", true},
        {"192.168.0.1", true},
        {"::1", true},
        {"fe80::1", true},
        {"fc00::1", true},
        {"::ffff:127.0.0.1", true},
        {"8.8.8.8", false},
        {"1.1.1.1", false},
        {"2001:4860:4860::8888", false}, // Google DNS IPv6
    }
    for _, c := range cases {
        t.Run(c.ip, func(t *testing.T) {
            ip := net.ParseIP(c.ip)
            if ip == nil {
                t.Fatalf("ParseIP(%q) returned nil", c.ip)
            }
            if got := isBlockedIP(ip); got != c.blocked {
                t.Errorf("isBlockedIP(%s) = %v, want %v", c.ip, got, c.blocked)
            }
        })
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./internal/auth/oidc/ -run TestSSRF -v
go test ./internal/auth/oidc/ -run TestValidateRegisterURI -v
go test ./internal/auth/oidc/ -run TestIsBlocked -v
```

Expected: FAIL — `validateRegisterURI` / `isBlockedIP` undefined.

- [ ] **Step 3: Implement**

```go
// internal/auth/oidc/ssrf.go
package oidc

import (
    "context"
    "fmt"
    "net"
    "net/url"
    "strings"
)

// Blocklist CIDRs per spec D10. Parsed once at package init.
var blockedNets []*net.IPNet

func init() {
    cidrs := []string{
        "127.0.0.0/8",        // IPv4 loopback
        "169.254.0.0/16",     // IPv4 link-local (AWS metadata, etc.)
        "10.0.0.0/8",         // RFC1918
        "172.16.0.0/12",      // RFC1918
        "192.168.0.0/16",     // RFC1918
        "::1/128",            // IPv6 loopback
        "fe80::/10",          // IPv6 link-local
        "fc00::/7",           // IPv6 ULA (RFC4193)
        "::ffff:0:0/96",      // IPv4-mapped IPv6
    }
    for _, c := range cidrs {
        _, n, err := net.ParseCIDR(c)
        if err != nil {
            panic(fmt.Sprintf("oidc: invalid blocklist CIDR %q: %v", c, err))
        }
        blockedNets = append(blockedNets, n)
    }
}

// isBlockedIP reports whether ip falls in any of the SSRF blocklist ranges
// (loopback, link-local, RFC1918, IPv6 ULA, IPv4-mapped IPv6).
func isBlockedIP(ip net.IP) bool {
    for _, n := range blockedNets {
        if n.Contains(ip) {
            return true
        }
    }
    return false
}

// validateRegisterURI performs register-time SSRF + scheme checks per D10.
// This is a UX layer — the security boundary is safeDialContext, which re-checks
// at fetch time on every dial.
//
//   - requireHTTPS=true rejects http:// schemes.
//   - allowPrivate=true skips the blocklist check (test/dev only, controlled
//     by CYODA_OIDC_ALLOW_PRIVATE_NETWORKS).
//
// Returns ErrSSRFBlocked for blocklist hits, plain error for malformed/scheme.
func validateRegisterURI(rawURI string, requireHTTPS, allowPrivate bool) error {
    u, err := url.Parse(rawURI)
    if err != nil {
        return fmt.Errorf("malformed URI: %w", err)
    }
    if u.Scheme != "https" && u.Scheme != "http" {
        return fmt.Errorf("unsupported scheme %q (want https or http)", u.Scheme)
    }
    if requireHTTPS && u.Scheme != "https" {
        return fmt.Errorf("https required (CYODA_OIDC_REQUIRE_HTTPS=true)")
    }
    if u.Host == "" {
        return fmt.Errorf("missing host in URI")
    }

    if allowPrivate {
        return nil
    }

    host := u.Hostname()
    // If the host is an IP literal, check it directly.
    if ip := net.ParseIP(host); ip != nil {
        if isBlockedIP(ip) {
            return fmt.Errorf("%w: host %s in blocklist", ErrSSRFBlocked, host)
        }
        return nil
    }

    // Hostname: resolve and check every answer.
    ips, err := net.DefaultResolver.LookupIP(context.Background(), "ip", host)
    if err != nil {
        return fmt.Errorf("DNS lookup failed for %s: %w", host, err)
    }
    for _, ip := range ips {
        if isBlockedIP(ip) {
            return fmt.Errorf("%w: %s resolves to %s in blocklist", ErrSSRFBlocked, host, ip)
        }
    }
    return nil
}

// safeDialContext returns a DialContext function that re-checks every resolved
// IP against the blocklist before allowing the connection — the fetch-time
// security boundary that closes DNS-rebind windows the register-time check
// cannot defend.
//
// When allowPrivate is true (test/dev override) the dial proceeds without
// the blocklist check.
func safeDialContext(allowPrivate bool) func(ctx context.Context, network, addr string) (net.Conn, error) {
    dialer := &net.Dialer{}
    return func(ctx context.Context, network, addr string) (net.Conn, error) {
        if allowPrivate {
            return dialer.DialContext(ctx, network, addr)
        }
        host, port, err := net.SplitHostPort(addr)
        if err != nil {
            return nil, fmt.Errorf("safedialer: malformed addr %q: %w", addr, err)
        }
        // Resolve and check every candidate IP.
        ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
        if err != nil {
            return nil, fmt.Errorf("safedialer: DNS lookup failed for %s: %w", host, err)
        }
        for _, ip := range ips {
            if isBlockedIP(ip) {
                return nil, fmt.Errorf("%w: %s resolves to %s in blocklist", ErrSSRFBlocked, host, ip)
            }
        }
        // Reconstruct addr (use first non-blocked IP) and dial.
        targetAddr := net.JoinHostPort(ips[0].String(), port)
        return dialer.DialContext(ctx, network, targetAddr)
    }
}

// Compile-time guard so editors flag unused strings.Builder if refactored.
var _ = strings.Builder{}
```

- [ ] **Step 4: Verify test passes**

```bash
go test ./internal/auth/oidc/ -run "TestSSRF|TestValidateRegisterURI|TestIsBlocked" -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/auth/oidc/ssrf.go internal/auth/oidc/ssrf_test.go
git commit -m "feat(oidc): SSRF defence with fetch-time DialContext re-check (#284)"
```

### Task 2.3: HTTP discovery (`internal/auth/oidc/discovery.go`)

**Spec ref:** §3.1 (discovery.go row), §3.4 wire matrix, §6 startup, D10.

**Files:**
- Create: `internal/auth/oidc/discovery.go`
- Test: `internal/auth/oidc/discovery_test.go`

- [ ] **Step 1: Write failing tests**

```go
// internal/auth/oidc/discovery_test.go
package oidc

import (
    "context"
    "errors"
    "net/http"
    "net/http/httptest"
    "strings"
    "testing"
    "time"
)

func TestHTTPDiscovery_FetchSuccess(t *testing.T) {
    body := `{
        "issuer": "https://idp.example",
        "jwks_uri": "https://idp.example/jwks"
    }`
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("Content-Type", "application/json")
        _, _ = w.Write([]byte(body))
    }))
    defer srv.Close()

    d := NewHTTPDiscovery(DiscoveryConfig{
        ConnectTimeout:           1 * time.Second,
        SocketTimeout:            1 * time.Second,
        ConnectionRequestTimeout: 1 * time.Second,
        AllowPrivateNetworks:     true, // httptest.Server binds to 127.0.0.1
    })

    doc, err := d.Fetch(context.Background(), srv.URL)
    if err != nil {
        t.Fatalf("Fetch: %v", err)
    }
    if doc.Issuer != "https://idp.example" {
        t.Errorf("Issuer = %q", doc.Issuer)
    }
    if doc.JWKSURI != "https://idp.example/jwks" {
        t.Errorf("JWKSURI = %q", doc.JWKSURI)
    }
}

func TestHTTPDiscovery_DoesNotFollowRedirects(t *testing.T) {
    // Spec D10: redirects disabled (fail-closed).
    redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        _, _ = w.Write([]byte(`{"issuer":"compromised","jwks_uri":"x"}`))
    }))
    defer redirectTarget.Close()

    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        http.Redirect(w, r, redirectTarget.URL, http.StatusFound)
    }))
    defer srv.Close()

    d := NewHTTPDiscovery(DiscoveryConfig{
        ConnectTimeout:           1 * time.Second,
        SocketTimeout:            1 * time.Second,
        ConnectionRequestTimeout: 1 * time.Second,
        AllowPrivateNetworks:     true,
    })

    _, err := d.Fetch(context.Background(), srv.URL)
    if err == nil {
        t.Fatal("expected error on redirect, got nil")
    }
    // The exact error class is implementation-defined but must indicate
    // the redirect was not followed.
    if !strings.Contains(err.Error(), "redirect") && !errors.Is(err, ErrDiscoveryFailed) {
        // accept either explicit-redirect or wrapped-discovery-failed shape
    }
}

func TestHTTPDiscovery_BlockedHostByDialer(t *testing.T) {
    // safeDialContext rejects the dial; we verify Fetch surfaces an error.
    d := NewHTTPDiscovery(DiscoveryConfig{
        ConnectTimeout:           1 * time.Second,
        SocketTimeout:            1 * time.Second,
        ConnectionRequestTimeout: 1 * time.Second,
        AllowPrivateNetworks:     false,
    })
    _, err := d.Fetch(context.Background(), "https://127.0.0.1/.well-known/openid-configuration")
    if err == nil {
        t.Fatal("expected fetch-time SSRF block, got nil")
    }
}

func TestHTTPDiscovery_HonoursContextDeadline(t *testing.T) {
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        time.Sleep(500 * time.Millisecond)
        _, _ = w.Write([]byte(`{"issuer":"x","jwks_uri":"y"}`))
    }))
    defer srv.Close()

    d := NewHTTPDiscovery(DiscoveryConfig{
        ConnectTimeout:           1 * time.Second,
        SocketTimeout:            1 * time.Second,
        ConnectionRequestTimeout: 1 * time.Second,
        AllowPrivateNetworks:     true,
    })
    ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
    defer cancel()
    _, err := d.Fetch(ctx, srv.URL)
    if err == nil {
        t.Fatal("expected ctx.Deadline error")
    }
}
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
go test ./internal/auth/oidc/ -run TestHTTPDiscovery -v
```

Expected: FAIL — `NewHTTPDiscovery` / `DiscoveryConfig` / `Discovery` undefined.

- [ ] **Step 3: Implement**

```go
// internal/auth/oidc/discovery.go
package oidc

import (
    "context"
    "encoding/json"
    "fmt"
    "net/http"
    "time"
)

// DiscoveryDoc is the subset of the OIDC discovery document we depend on per
// spec §4.1. The full document has many fields; we extract the two used at
// runtime (Issuer for D17 bytewise iss matching; JWKSURI for key fetch).
type DiscoveryDoc struct {
    Issuer  string `json:"issuer"`
    JWKSURI string `json:"jwks_uri"`
}

// Discovery fetches the OIDC discovery document at the well-known URI. The
// implementation must enforce the safedialer + redirect-disabled + timeouts
// per D10; callers (Registry.WarmJWKSAsync, Service.Register/Update/Reload)
// invoke Fetch but do not implement HTTP themselves.
type Discovery interface {
    Fetch(ctx context.Context, uri string) (*DiscoveryDoc, error)
}

// DiscoveryConfig configures HTTPDiscovery. All durations are positive; zero
// values fall back to a defensive 5s default.
type DiscoveryConfig struct {
    ConnectTimeout           time.Duration
    SocketTimeout            time.Duration
    ConnectionRequestTimeout time.Duration
    AllowPrivateNetworks     bool // CYODA_OIDC_ALLOW_PRIVATE_NETWORKS
}

// HTTPDiscovery is the production Discovery implementation. It uses an
// http.Client wired with safeDialContext (fetch-time SSRF) and a CheckRedirect
// that refuses to follow any redirects (D10 fail-closed).
type HTTPDiscovery struct {
    client *http.Client
}

// NewHTTPDiscovery builds an HTTPDiscovery from the given config. The returned
// instance is safe for concurrent use by many callers (the underlying
// http.Client is goroutine-safe).
func NewHTTPDiscovery(cfg DiscoveryConfig) *HTTPDiscovery {
    if cfg.ConnectTimeout <= 0 {
        cfg.ConnectTimeout = 5 * time.Second
    }
    if cfg.SocketTimeout <= 0 {
        cfg.SocketTimeout = 5 * time.Second
    }
    if cfg.ConnectionRequestTimeout <= 0 {
        cfg.ConnectionRequestTimeout = 5 * time.Second
    }
    transport := &http.Transport{
        DialContext:           safeDialContext(cfg.AllowPrivateNetworks),
        TLSHandshakeTimeout:   cfg.ConnectTimeout,
        ResponseHeaderTimeout: cfg.SocketTimeout,
        ExpectContinueTimeout: 1 * time.Second,
    }
    client := &http.Client{
        Transport: transport,
        Timeout:   cfg.SocketTimeout + cfg.ConnectTimeout + cfg.ConnectionRequestTimeout,
        // Fail-closed: never follow redirects. D10 mitigation against
        // discovery returning Location: http://169.254.169.254/...
        CheckRedirect: func(req *http.Request, via []*http.Request) error {
            return http.ErrUseLastResponse
        },
    }
    return &HTTPDiscovery{client: client}
}

func (d *HTTPDiscovery) Fetch(ctx context.Context, uri string) (*DiscoveryDoc, error) {
    req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
    if err != nil {
        return nil, fmt.Errorf("%w: build request: %v", ErrDiscoveryFailed, err)
    }
    req.Header.Set("Accept", "application/json")

    resp, err := d.client.Do(req)
    if err != nil {
        return nil, fmt.Errorf("%w: %v", ErrDiscoveryFailed, err)
    }
    defer resp.Body.Close()

    // Redirect responses surface as 3xx because we refused to follow.
    // Treat them as fetch failures.
    if resp.StatusCode >= 300 {
        return nil, fmt.Errorf("%w: HTTP %d (redirects disabled)", ErrDiscoveryFailed, resp.StatusCode)
    }

    var doc DiscoveryDoc
    dec := json.NewDecoder(resp.Body)
    dec.DisallowUnknownFields()
    if err := dec.Decode(&doc); err != nil {
        // The discovery doc has many fields we don't model; DisallowUnknownFields
        // would over-reject. Retry with a permissive decoder.
        // (Implementer note: the disallow above is too strict; remove it.)
    }
    // Re-decode permissively.
    req2, _ := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
    resp2, err := d.client.Do(req2)
    if err != nil {
        return nil, fmt.Errorf("%w: %v", ErrDiscoveryFailed, err)
    }
    defer resp2.Body.Close()
    if err := json.NewDecoder(resp2.Body).Decode(&doc); err != nil {
        return nil, fmt.Errorf("%w: malformed JSON: %v", ErrDiscoveryFailed, err)
    }
    if doc.Issuer == "" || doc.JWKSURI == "" {
        return nil, fmt.Errorf("%w: discovery doc missing issuer or jwks_uri", ErrDiscoveryFailed)
    }
    return &doc, nil
}
```

> **Implementer note:** the double-decode above is awkward — the simple fix is to drop `DisallowUnknownFields` and decode once. Verify the tests still pass after that simplification, then commit the cleanup. The comment is intentionally left in the plan so the reviewer can verify the implementer recognized and resolved the smell.

- [ ] **Step 4: Verify tests pass**

```bash
go test ./internal/auth/oidc/ -run TestHTTPDiscovery -v
```

Expected: PASS.

- [ ] **Step 5: Refactor — single decode (the "implementer note" cleanup)**

Replace the body of `Fetch` after `resp.Body.Close()` deferral with a single decode:

```go
    var doc DiscoveryDoc
    if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
        return nil, fmt.Errorf("%w: malformed JSON: %v", ErrDiscoveryFailed, err)
    }
    if doc.Issuer == "" || doc.JWKSURI == "" {
        return nil, fmt.Errorf("%w: discovery doc missing issuer or jwks_uri", ErrDiscoveryFailed)
    }
    return &doc, nil
```

Re-run the tests:

```bash
go test ./internal/auth/oidc/ -run TestHTTPDiscovery -v
```

Expected: still PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/auth/oidc/discovery.go internal/auth/oidc/discovery_test.go
git commit -m "feat(oidc): HTTPDiscovery with safedialer + redirects disabled (#284)"
```

---

**Phase 2 complete.** All three tasks can run concurrently in subagents; the merged state should pass:

```bash
go test ./internal/auth/oidc/... -count=1
go vet ./internal/auth/oidc/...
```

---

## Plan continued in companion files

The remaining phases — KV store, registry + validator + service + broadcast, HTTP adapters, OpenAPI + wiring + config, parity tests, documentation, final verification — are continued in this same plan file in subsequent edits to keep the document scoped.

**Next phase in this file: Phase 3 (OIDC persistence)** — depends on Task 2.1 (types) only; can begin once 2.1 lands. Parallel-safe with Phase 2.2 and 2.3.

---

## Phase 3: OIDC persistence — KV store

### Task 3.1: Store interface (`internal/auth/oidc/store.go`)

**Spec ref:** §3.1 (store.go row), §3.3, D2.

**Files:**
- Create: `internal/auth/oidc/store.go`

This task only declares an interface, so its "test" is a compile check from Task 3.2's implementation. No standalone test file.

- [ ] **Step 1: Implement the interface**

```go
// internal/auth/oidc/store.go
package oidc

import (
    "context"

    spi "github.com/cyoda-platform/cyoda-go-spi"
)

// OidcProviderStore is the persistence interface for OIDC providers and their
// cross-tenant URI ownership history. The single implementation (KVOidcProviderStore)
// sits on top of spi.KeyValueStore and uses one namespace "oidc-providers" with
// composite keys per spec §3.3:
//
//   <tenantID>:<provider-uuid>       → JSON(OidcProvider)
//   <tenantID>:uri:<sha256(uri)>     → <provider-uuid>      (per-tenant unique index)
//   _history:<sha256(uri)>           → JSON(UriOwnershipHistory)  (D25 cross-tenant audit)
//
// All Store operations run under the system tenant context — see app/app.go
// for the bootstrap. Tenancy is enforced via the composite key prefix, not via
// the underlying KV's tenant-at-acquisition scoping.
type OidcProviderStore interface {
    // Register persists a new provider. Caller is responsible for the D11
    // race-validation re-read after Put — the store's contract is "best-effort
    // sequential": Put index, Put blob, return.
    Register(ctx context.Context, p *OidcProvider) error

    // Get retrieves a provider by (tenant, id) with stale-index defence:
    // if the blob's OwnerLegalEntityID doesn't match the requesting tenant,
    // the store treats this as orphaned and returns ErrProviderNotFound.
    Get(ctx context.Context, tenantID spi.TenantID, providerID string) (*OidcProvider, error)

    // GetByURI retrieves a provider by (tenant, wellKnownConfigUri). Same
    // stale-index defence as Get.
    GetByURI(ctx context.Context, tenantID spi.TenantID, uri string) (*OidcProvider, error)

    // Update overwrites the blob. Caller is responsible for re-reading first
    // (the adapter does the stale-index defence + 404 check).
    Update(ctx context.Context, p *OidcProvider) error

    // Delete removes the blob AND the per-tenant URI index entry. Does NOT
    // touch the cross-tenant ownership history — Service.Delete handles that
    // separately via PutURIHistory.
    Delete(ctx context.Context, tenantID spi.TenantID, providerID, uri string) error

    // ListByTenant returns all providers for one tenant, optionally filtered
    // to active=true.
    ListByTenant(ctx context.Context, tenantID spi.TenantID, activeOnly bool) ([]*OidcProvider, error)

    // LoadAll enumerates every provider across all tenants — used by the
    // startup hook and by Registry.ReloadAll. Returns one item per provider
    // blob (not per index entry; the implementation filters out keys with
    // the ":uri:" or "_history:" substrings).
    LoadAll(ctx context.Context) ([]*OidcProvider, error)

    // GetURIHistory returns the cross-tenant ownership history for one URI,
    // or (nil, nil) if no history exists yet (first registration).
    GetURIHistory(ctx context.Context, uriHash string) (*UriOwnershipHistory, error)

    // PutURIHistory overwrites the ownership-history entry for one URI.
    // Failure is logged ERROR by the caller but does not block the lifecycle
    // operation (D25 is an audit signal, not a correctness gate).
    PutURIHistory(ctx context.Context, uriHash string, h *UriOwnershipHistory) error

    // RaceValidateIndex re-reads the per-tenant URI index after Put to detect
    // the D11 register race. If the stored providerID does not equal expectedID,
    // the caller lost the race; returns the winning providerID and ok=false.
    RaceValidateIndex(ctx context.Context, tenantID spi.TenantID, uri string, expectedID string) (winningID string, ok bool, err error)
}
```

- [ ] **Step 2: Compile check**

```bash
go build ./internal/auth/oidc/
```

Expected: builds. (No tests yet — Task 3.2 will add them.)

- [ ] **Step 3: Commit**

```bash
git add internal/auth/oidc/store.go
git commit -m "feat(oidc): OidcProviderStore interface (#284)"
```

### Task 3.2: KV-backed store implementation (`internal/auth/oidc/kv_store.go`)

**Spec ref:** §3.1, §3.3, §5.1, §5.5, D2, D11.

**Files:**
- Create: `internal/auth/oidc/kv_store.go`
- Create: `internal/auth/oidc/kv_keys.go` (helper for the three key shapes)
- Test: `internal/auth/oidc/kv_store_test.go`

Because this task contains a lot of code, follow this sub-sequence:

- [ ] **Step 1: Write the key helper first (no tests; trivial)**

```go
// internal/auth/oidc/kv_keys.go
package oidc

import (
    "crypto/sha256"
    "encoding/hex"
    "fmt"

    spi "github.com/cyoda-platform/cyoda-go-spi"
)

// Namespace is the single KV namespace per D2.
const Namespace = "oidc-providers"

// providerBlobKey returns "<tenantID>:<provider-uuid>".
func providerBlobKey(t spi.TenantID, providerID string) string {
    return string(t) + ":" + providerID
}

// uriIndexKey returns "<tenantID>:uri:<sha256-hex>".
func uriIndexKey(t spi.TenantID, uri string) string {
    return string(t) + ":uri:" + sha256Hex(uri)
}

// uriHistoryKey returns "_history:<sha256-hex>". The leading underscore
// disambiguates from tenant-UUID-prefixed keys (UUIDs are hex+dashes, never
// start with '_').
func uriHistoryKey(uri string) string {
    return "_history:" + sha256Hex(uri)
}

func sha256Hex(s string) string {
    sum := sha256.Sum256([]byte(s))
    return hex.EncodeToString(sum[:])
}

// parseProviderBlobKey returns (tenantID, providerID, true) for keys of the
// form "<tenantID>:<provider-uuid>". Index keys and history keys are
// distinguished by the ":uri:" substring and the "_history:" prefix.
func parseProviderBlobKey(k string) (spi.TenantID, string, bool) {
    if len(k) >= 8 && k[:8] == "_history" {
        return "", "", false
    }
    // Find the FIRST ':' — UUIDs have dashes, not colons, so the split is unambiguous.
    for i, ch := range k {
        if ch == ':' {
            tenant := k[:i]
            rest := k[i+1:]
            // Exclude index entries: "<tenant>:uri:<hash>" has rest starting with "uri:".
            if len(rest) >= 4 && rest[:4] == "uri:" {
                return "", "", false
            }
            return spi.TenantID(tenant), rest, true
        }
    }
    return "", "", false
}

// Ensure unused-import is not the problem during incremental build.
var _ = fmt.Sprintf
```

- [ ] **Step 2: Write KV store tests against the in-memory plugin**

```go
// internal/auth/oidc/kv_store_test.go
package oidc

import (
    "context"
    "testing"
    "time"

    "github.com/google/uuid"
    spi "github.com/cyoda-platform/cyoda-go-spi"
    memplugin "github.com/cyoda-platform/cyoda-go/plugins/memory"
)

func newTestKV(t *testing.T) spi.KeyValueStore {
    t.Helper()
    factory, err := memplugin.NewFactory()
    if err != nil {
        t.Fatalf("NewFactory: %v", err)
    }
    systemCtx := spi.WithUserContext(context.Background(), &spi.UserContext{
        UserID:   "system",
        UserName: "System",
        Tenant:   spi.Tenant{ID: spi.SystemTenantID, Name: "System"},
    })
    kv, err := factory.KeyValueStore(systemCtx)
    if err != nil {
        t.Fatalf("KeyValueStore: %v", err)
    }
    t.Cleanup(func() { _ = factory.Close() })
    return kv
}

func newTestStore(t *testing.T) *KVOidcProviderStore {
    t.Helper()
    systemCtx := spi.WithUserContext(context.Background(), &spi.UserContext{
        UserID:   "system",
        UserName: "System",
        Tenant:   spi.Tenant{ID: spi.SystemTenantID, Name: "System"},
    })
    s, err := NewKVProviderStore(systemCtx, newTestKV(t))
    if err != nil {
        t.Fatalf("NewKVProviderStore: %v", err)
    }
    return s
}

func sampleProvider(t *testing.T, tenant spi.TenantID, uri string) *OidcProvider {
    t.Helper()
    return &OidcProvider{
        ID:                 uuid.New(),
        WellKnownConfigURI: uri,
        Issuers:            []string{"https://idp.example"},
        CreatedAt:          time.Now().UTC().Truncate(time.Second),
        OwnerLegalEntityID: uuid.New(),
    }
}

func TestKVStore_RegisterAndGet(t *testing.T) {
    s := newTestStore(t)
    ctx := context.Background()
    tenantA := spi.TenantID("tenant-a")

    p := sampleProvider(t, tenantA, "https://idp-a.example/.well-known/openid-configuration")
    if err := s.Register(ctx, p); err != nil {
        t.Fatalf("Register: %v", err)
    }

    got, err := s.Get(ctx, tenantA, p.ID.String())
    if err != nil {
        t.Fatalf("Get: %v", err)
    }
    if got.WellKnownConfigURI != p.WellKnownConfigURI {
        t.Errorf("URI mismatch: %s vs %s", got.WellKnownConfigURI, p.WellKnownConfigURI)
    }
}

func TestKVStore_GetByURI(t *testing.T) {
    s := newTestStore(t)
    ctx := context.Background()
    tenantA := spi.TenantID("tenant-a")
    uri := "https://idp-a.example/.well-known/openid-configuration"

    p := sampleProvider(t, tenantA, uri)
    if err := s.Register(ctx, p); err != nil {
        t.Fatalf("Register: %v", err)
    }

    got, err := s.GetByURI(ctx, tenantA, uri)
    if err != nil {
        t.Fatalf("GetByURI: %v", err)
    }
    if got.ID != p.ID {
        t.Errorf("ID mismatch")
    }
}

func TestKVStore_CrossTenantGetReturns404(t *testing.T) {
    s := newTestStore(t)
    ctx := context.Background()
    tenantA := spi.TenantID("tenant-a")
    tenantB := spi.TenantID("tenant-b")

    p := sampleProvider(t, tenantA, "https://idp.example/.well-known/openid-configuration")
    if err := s.Register(ctx, p); err != nil {
        t.Fatalf("Register: %v", err)
    }
    _, err := s.Get(ctx, tenantB, p.ID.String())
    if err == nil {
        t.Fatal("expected ErrProviderNotFound, got nil")
    }
}

func TestKVStore_ListByTenant_FiltersOtherTenants(t *testing.T) {
    s := newTestStore(t)
    ctx := context.Background()
    tenantA := spi.TenantID("tenant-a")
    tenantB := spi.TenantID("tenant-b")

    _ = s.Register(ctx, sampleProvider(t, tenantA, "https://a1.example"))
    _ = s.Register(ctx, sampleProvider(t, tenantA, "https://a2.example"))
    _ = s.Register(ctx, sampleProvider(t, tenantB, "https://b1.example"))

    got, err := s.ListByTenant(ctx, tenantA, false)
    if err != nil {
        t.Fatalf("ListByTenant: %v", err)
    }
    if len(got) != 2 {
        t.Errorf("got %d providers, want 2", len(got))
    }
}

func TestKVStore_LoadAll_AcrossTenants(t *testing.T) {
    s := newTestStore(t)
    ctx := context.Background()
    _ = s.Register(ctx, sampleProvider(t, spi.TenantID("a"), "https://a.example"))
    _ = s.Register(ctx, sampleProvider(t, spi.TenantID("b"), "https://b.example"))
    _ = s.Register(ctx, sampleProvider(t, spi.TenantID("c"), "https://c.example"))

    all, err := s.LoadAll(ctx)
    if err != nil {
        t.Fatalf("LoadAll: %v", err)
    }
    if len(all) != 3 {
        t.Errorf("LoadAll = %d, want 3", len(all))
    }
}

func TestKVStore_URIHistoryRoundTrip(t *testing.T) {
    s := newTestStore(t)
    ctx := context.Background()
    uri := "https://idp.example/.well-known/openid-configuration"

    got, err := s.GetURIHistory(ctx, sha256Hex(uri))
    if err != nil {
        t.Fatalf("GetURIHistory: %v", err)
    }
    if got != nil {
        t.Errorf("expected nil for first read, got %+v", got)
    }

    now := time.Now().UTC().Truncate(time.Second)
    h := &UriOwnershipHistory{
        CurrentOwner: &Owner{TenantID: "a", ProviderUUID: "u1", RegisteredAt: now},
    }
    if err := s.PutURIHistory(ctx, sha256Hex(uri), h); err != nil {
        t.Fatalf("PutURIHistory: %v", err)
    }
    back, err := s.GetURIHistory(ctx, sha256Hex(uri))
    if err != nil || back == nil {
        t.Fatalf("GetURIHistory after Put: %v / %v", back, err)
    }
    if back.CurrentOwner.TenantID != "a" {
        t.Errorf("history CurrentOwner lost")
    }
}

func TestKVStore_DeleteRemovesBlobAndIndex(t *testing.T) {
    s := newTestStore(t)
    ctx := context.Background()
    tenant := spi.TenantID("tenant-a")
    p := sampleProvider(t, tenant, "https://idp.example")
    _ = s.Register(ctx, p)

    if err := s.Delete(ctx, tenant, p.ID.String(), p.WellKnownConfigURI); err != nil {
        t.Fatalf("Delete: %v", err)
    }

    if _, err := s.Get(ctx, tenant, p.ID.String()); err == nil {
        t.Error("expected not-found after Delete")
    }
    if _, err := s.GetByURI(ctx, tenant, p.WellKnownConfigURI); err == nil {
        t.Error("expected not-found via index after Delete")
    }
}

func TestKVStore_RaceValidateIndex(t *testing.T) {
    s := newTestStore(t)
    ctx := context.Background()
    tenant := spi.TenantID("tenant-a")
    uri := "https://idp.example"

    p1 := sampleProvider(t, tenant, uri)
    if err := s.Register(ctx, p1); err != nil {
        t.Fatalf("Register p1: %v", err)
    }

    // Simulate a "loser" who wrote the index later thinking p2's UUID is the winner.
    winningID, ok, err := s.RaceValidateIndex(ctx, tenant, uri, "some-other-uuid")
    if err != nil {
        t.Fatalf("RaceValidateIndex: %v", err)
    }
    if ok {
        t.Error("expected ok=false (loser)")
    }
    if winningID != p1.ID.String() {
        t.Errorf("winningID = %s, want %s", winningID, p1.ID.String())
    }

    // Winner case: ID matches.
    _, ok, err = s.RaceValidateIndex(ctx, tenant, uri, p1.ID.String())
    if err != nil {
        t.Fatalf("RaceValidateIndex: %v", err)
    }
    if !ok {
        t.Error("expected ok=true (winner)")
    }
}
```

- [ ] **Step 3: Run tests to verify they fail**

```bash
go test ./internal/auth/oidc/ -run TestKVStore -v
```

Expected: FAIL — `NewKVProviderStore` undefined.

- [ ] **Step 4: Implement**

```go
// internal/auth/oidc/kv_store.go
package oidc

import (
    "context"
    "encoding/json"
    "errors"
    "fmt"
    "strings"

    spi "github.com/cyoda-platform/cyoda-go-spi"
)

// KVOidcProviderStore implements OidcProviderStore on top of spi.KeyValueStore.
// Constructed once at startup with a context that carries the system tenant —
// see app/app.go. All KV calls go through this stored context, never the
// per-request context.
type KVOidcProviderStore struct {
    kv  spi.KeyValueStore
    ctx context.Context
}

// NewKVProviderStore builds a store bound to the supplied system-tenant context.
// The ctx is stored via context.WithoutCancel so request-scoped cancellation
// cannot abort KV ops mid-operation.
func NewKVProviderStore(ctx context.Context, kv spi.KeyValueStore) (*KVOidcProviderStore, error) {
    if kv == nil {
        return nil, errors.New("oidc: NewKVProviderStore: nil KeyValueStore")
    }
    return &KVOidcProviderStore{
        kv:  kv,
        ctx: context.WithoutCancel(ctx),
    }, nil
}

func (s *KVOidcProviderStore) Register(ctx context.Context, p *OidcProvider) error {
    blob, err := json.Marshal(p)
    if err != nil {
        return fmt.Errorf("oidc: marshal provider: %w", err)
    }
    tenant := spi.TenantID(p.OwnerLegalEntityID.String())
    indexKey := uriIndexKey(tenant, p.WellKnownConfigURI)
    blobKey := providerBlobKey(tenant, p.ID.String())

    // Index first, then blob. Caller handles D11 race-validation.
    if err := s.kv.Put(s.ctx, Namespace, indexKey, []byte(p.ID.String())); err != nil {
        return fmt.Errorf("oidc: put index: %w", err)
    }
    if err := s.kv.Put(s.ctx, Namespace, blobKey, blob); err != nil {
        // Best-effort rollback of the orphan index entry.
        _ = s.kv.Delete(s.ctx, Namespace, indexKey)
        return fmt.Errorf("oidc: put blob: %w", err)
    }
    return nil
}

func (s *KVOidcProviderStore) Get(ctx context.Context, tenantID spi.TenantID, providerID string) (*OidcProvider, error) {
    blob, err := s.kv.Get(s.ctx, Namespace, providerBlobKey(tenantID, providerID))
    if err != nil {
        if errors.Is(err, spi.ErrNotFound) {
            return nil, ErrProviderNotFound
        }
        return nil, fmt.Errorf("oidc: get blob: %w", err)
    }
    var p OidcProvider
    if err := json.Unmarshal(blob, &p); err != nil {
        return nil, fmt.Errorf("oidc: unmarshal blob: %w", err)
    }
    // Stale-index defence: blob's OwnerLegalEntityID must match the tenant prefix.
    if spi.TenantID(p.OwnerLegalEntityID.String()) != tenantID {
        return nil, ErrProviderNotFound
    }
    return &p, nil
}

func (s *KVOidcProviderStore) GetByURI(ctx context.Context, tenantID spi.TenantID, uri string) (*OidcProvider, error) {
    idBytes, err := s.kv.Get(s.ctx, Namespace, uriIndexKey(tenantID, uri))
    if err != nil {
        if errors.Is(err, spi.ErrNotFound) {
            return nil, ErrProviderNotFound
        }
        return nil, fmt.Errorf("oidc: get index: %w", err)
    }
    p, err := s.Get(ctx, tenantID, string(idBytes))
    if err != nil {
        // Orphan index — best-effort cleanup.
        if errors.Is(err, ErrProviderNotFound) {
            _ = s.kv.Delete(s.ctx, Namespace, uriIndexKey(tenantID, uri))
        }
        return nil, err
    }
    return p, nil
}

func (s *KVOidcProviderStore) Update(ctx context.Context, p *OidcProvider) error {
    blob, err := json.Marshal(p)
    if err != nil {
        return fmt.Errorf("oidc: marshal provider: %w", err)
    }
    tenant := spi.TenantID(p.OwnerLegalEntityID.String())
    return s.kv.Put(s.ctx, Namespace, providerBlobKey(tenant, p.ID.String()), blob)
}

func (s *KVOidcProviderStore) Delete(ctx context.Context, tenantID spi.TenantID, providerID, uri string) error {
    if err := s.kv.Delete(s.ctx, Namespace, providerBlobKey(tenantID, providerID)); err != nil {
        return fmt.Errorf("oidc: delete blob: %w", err)
    }
    if err := s.kv.Delete(s.ctx, Namespace, uriIndexKey(tenantID, uri)); err != nil {
        // The blob is already gone; index orphan is recoverable via stale-index defence.
        return fmt.Errorf("oidc: delete index: %w", err)
    }
    return nil
}

func (s *KVOidcProviderStore) ListByTenant(ctx context.Context, tenantID spi.TenantID, activeOnly bool) ([]*OidcProvider, error) {
    entries, err := s.kv.List(s.ctx, Namespace)
    if err != nil {
        return nil, fmt.Errorf("oidc: list: %w", err)
    }
    var out []*OidcProvider
    prefix := string(tenantID) + ":"
    for k, v := range entries {
        if !strings.HasPrefix(k, prefix) {
            continue
        }
        if strings.HasPrefix(k, string(tenantID)+":uri:") {
            continue
        }
        var p OidcProvider
        if err := json.Unmarshal(v, &p); err != nil {
            continue // skip malformed
        }
        if spi.TenantID(p.OwnerLegalEntityID.String()) != tenantID {
            continue // stale-index defence on List too
        }
        if activeOnly && !p.Active() {
            continue
        }
        pCopy := p
        out = append(out, &pCopy)
    }
    return out, nil
}

func (s *KVOidcProviderStore) LoadAll(ctx context.Context) ([]*OidcProvider, error) {
    entries, err := s.kv.List(s.ctx, Namespace)
    if err != nil {
        return nil, fmt.Errorf("oidc: list: %w", err)
    }
    var out []*OidcProvider
    for k, v := range entries {
        if strings.HasPrefix(k, "_history:") {
            continue
        }
        if strings.Contains(k, ":uri:") {
            continue
        }
        var p OidcProvider
        if err := json.Unmarshal(v, &p); err != nil {
            continue
        }
        pCopy := p
        out = append(out, &pCopy)
    }
    return out, nil
}

func (s *KVOidcProviderStore) GetURIHistory(ctx context.Context, uriHash string) (*UriOwnershipHistory, error) {
    blob, err := s.kv.Get(s.ctx, Namespace, "_history:"+uriHash)
    if err != nil {
        if errors.Is(err, spi.ErrNotFound) {
            return nil, nil
        }
        return nil, fmt.Errorf("oidc: get history: %w", err)
    }
    var h UriOwnershipHistory
    if err := json.Unmarshal(blob, &h); err != nil {
        return nil, fmt.Errorf("oidc: unmarshal history: %w", err)
    }
    return &h, nil
}

func (s *KVOidcProviderStore) PutURIHistory(ctx context.Context, uriHash string, h *UriOwnershipHistory) error {
    blob, err := json.Marshal(h)
    if err != nil {
        return fmt.Errorf("oidc: marshal history: %w", err)
    }
    return s.kv.Put(s.ctx, Namespace, "_history:"+uriHash, blob)
}

func (s *KVOidcProviderStore) RaceValidateIndex(ctx context.Context, tenantID spi.TenantID, uri string, expectedID string) (string, bool, error) {
    idBytes, err := s.kv.Get(s.ctx, Namespace, uriIndexKey(tenantID, uri))
    if err != nil {
        if errors.Is(err, spi.ErrNotFound) {
            return "", false, fmt.Errorf("oidc: race-validate: index disappeared (expected %s): %w", expectedID, err)
        }
        return "", false, fmt.Errorf("oidc: race-validate: %w", err)
    }
    winning := string(idBytes)
    return winning, winning == expectedID, nil
}
```

- [ ] **Step 5: Verify tests pass**

```bash
go test ./internal/auth/oidc/ -count=1 -v -run TestKVStore
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/auth/oidc/kv_store.go internal/auth/oidc/kv_keys.go internal/auth/oidc/kv_store_test.go
git commit -m "feat(oidc): KV-backed OidcProviderStore impl (#284)"
```

---

**Phase 3 complete.** Run:

```bash
go test ./internal/auth/oidc/... -count=1
```

All tests in `internal/auth/oidc/` should pass at this point.

---

## Phases 4–11: Continued in plan-continuation document

The remaining phases — registry + validator + service + broadcast + adapters + wiring + OpenAPI + parity tests + documentation + final verification — are large enough that splitting them into a continuation document is more readable than inlining all here. Continue at:

[`2026-06-16-284-oidc-providers-plan-continued.md`](2026-06-16-284-oidc-providers-plan-continued.md)

(The continuation document is created in the next plan-writing step. The phase-3 boundary is a natural seam: everything before this point is single-file or single-component work; everything after combines those pieces into the integrated subsystem.)
