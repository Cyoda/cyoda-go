# #281 — OpenAPI Conformance for `/oauth/keys/*` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the 10 stub `/oauth/keys/*` handlers with OpenAPI-conformant adapters routed through chi, remove the legacy `/oauth/keys/` prefix mux entry, add per-tenant scoping for trusted keys, and reconcile behaviour with Cyoda Cloud where reachable.

**Architecture:** Two new adapter files (`keys_adapter.go`, `trusted_adapter.go`) in `internal/domain/account/` host the 10 generated `ServerInterface` methods, calling into extended `KeyStore`/`TrustedKeyStore` interfaces. Stores gain audience partitioning (keypair) and tenant scoping (trusted key); both gain lazy `ValidTo` filtering for grace-period invalidation. Existing `internal/auth/{keys,trusted}.go` HTTP handlers and the `mux.Handle("/oauth/keys/", ...)` entry are removed. `internal/auth/jwt.go` is NOT modified — RS256-only signing stays; the 9 other algorithm enum values are rejected at adapter boundary (deferred to v0.8.1).

**Tech Stack:** Go 1.26, `chi` router, `oapi-codegen` v2.7.0, `log/slog`, `crypto/rsa` + `crypto/x509`, RFC 9457 ProblemDetail wire shape via `common.WriteError`, `golang-jwt` v5.

**Spec:** `docs/superpowers/specs/2026-06-04-281-oauth-keys-openapi-design.md` (rev4).

---

## File Map

**New files:**
- `internal/auth/keyvalidation.go` — moved validators (`trustedKIDPattern`, `parseRSAPublicKeyFromJWK`, `validateRSAPublicExponent`)
- `internal/auth/iam_config.go` — `IAMConfig` value struct
- `internal/auth/keypair_signing_test.go` — RS256 sign+verify; all other algorithms rejected at adapter
- `internal/auth/config_validation_test.go` — env var validation tests
- `internal/domain/account/keys_adapter.go` — 5 keypair handler methods
- `internal/domain/account/keys_adapter_test.go`
- `internal/domain/account/trusted_adapter.go` — 5 trusted-key handler methods
- `internal/domain/account/trusted_adapter_test.go`
- `internal/domain/account/io.go` — `boundedJSONDecode` helper
- `internal/domain/account/io_test.go`
- `internal/e2e/oauth_keys_test.go` — 10-operation E2E scenarios + grace + persistence + tenant + flag + token-exchange
- `cmd/cyoda/help/content/errors/FEATURE_DISABLED.md`
- `cmd/cyoda/help/content/errors/UNSUPPORTED_ALGORITHM.md`
- `cmd/cyoda/help/content/errors/UNSUPPORTED_KEY_TYPE.md`
- `cmd/cyoda/help/content/errors/KEY_OWNED_BY_DIFFERENT_TENANT.md`
- `cmd/cyoda/help/content/errors/KEYPAIR_NOT_FOUND.md`
- `cmd/cyoda/help/content/errors/TRUSTED_KEY_CAP_REACHED.md`

**Deleted files:**
- `internal/auth/keys.go` (entire file)
- `internal/auth/trusted.go` (entire file; validators move to `keyvalidation.go`)

**Modified files:**
- `internal/auth/store.go` — type extensions, interface signatures, RotateOptions, in-memory store updates
- `internal/auth/kv_trusted_store.go` — tenant-scoped key shape, schema, cache invariant, loadAll WARN
- `internal/auth/service.go` — adminMux entries removed, `AuthConfig.IAM` field, `TrustedKeyStore()` accessor, bootstrap key wiring
- `internal/auth/admin_guard.go` — `requireAdmin` → exported `RequireAdmin`
- `internal/auth/m2m.go` — update to new `RequireAdmin` name
- `internal/auth/token.go` — switch to in-package `getTrustedKeyByKID` helper
- `internal/auth/integration_test.go` — drop relocated body-size sub-test for trusted-key
- `internal/auth/store_test.go` — extended for new behaviours
- `internal/auth/kv_trusted_store_test.go` — extended for tenant scoping
- `internal/auth/token_test.go` — extended for `getTrustedKeyByKID`
- `internal/common/error_codes.go` — 6 new codes (5 + UNSUPPORTED_KEY_TYPE)
- `internal/domain/account/handler.go` — Handler struct fields, `account.New` signature, remove the 10 stub methods (move to adapter files)
- `app/config.go` — IAMConfig env-var parsing
- `app/app.go` — wiring: `account.New` callsite, remove `mux.Handle("/oauth/keys/", ...)`, bootstrap key save with new signature/audience/algorithm/validTo, JWKS source switch
- `api/openapi.yaml` — 501 removal, ErrorResponseDto→ProblemDetail, SUPER_USER→ROLE_ADMIN, JWK schema fix, ReactivateKeyRequestDto schema, default-behaviour prose
- `api/generated.go` — regenerated
- `cmd/cyoda/help/content/config/auth.md` — new env vars + IAM features subsection + JWT signing keypair rotation subsection
- `cmd/cyoda/help/content/errors.md` — 6 new codes in catalogue
- `cmd/cyoda/help/content/errors/TRUSTED_KEY_NOT_FOUND.md` — cross-tenant 404 note
- `cmd/cyoda/help/content/errors/NOT_FOUND.md` — KEYPAIR_NOT_FOUND cross-ref
- `cmd/cyoda/help/content/openapi.md` — line 96 clarification
- `README.md` — 5 new env vars in config table
- `docs/superpowers/audits/2026-04-29-openapi-conformance-audit.md` — 10 row dispositions updated

---

## Phase 1: Foundations (error codes, IAMConfig, validators move, type extensions)

### Task 1: Add 6 new error codes

**Files:**
- Modify: `internal/common/error_codes.go`

- [ ] **Step 1: Inspect existing error_codes.go to find insertion point**

Run: `grep -n "ErrCodeTrustedKeyNotFound\|ErrCodeNotFound\|ErrCodeForbidden" internal/common/error_codes.go`

Expected: locate `ErrCodeTrustedKeyNotFound` so the new codes sit alphabetically nearby.

- [ ] **Step 2: Add the 6 new codes**

Edit `internal/common/error_codes.go` — add inside the existing `const (...)` block (alphabetical placement):

```go
ErrCodeFeatureDisabled           = "FEATURE_DISABLED"
ErrCodeKeyOwnedByDifferentTenant = "KEY_OWNED_BY_DIFFERENT_TENANT"
ErrCodeKeypairNotFound           = "KEYPAIR_NOT_FOUND"
ErrCodeTrustedKeyCapReached      = "TRUSTED_KEY_CAP_REACHED"
ErrCodeUnsupportedAlgorithm      = "UNSUPPORTED_ALGORITHM"
ErrCodeUnsupportedKeyType        = "UNSUPPORTED_KEY_TYPE"
```

- [ ] **Step 3: Verify build**

Run: `go build ./internal/common/...`
Expected: PASS (no errors).

- [ ] **Step 4: Commit**

```bash
git add internal/common/error_codes.go
git commit -m "feat(common): add 6 error codes for /oauth/keys/* conformance

FEATURE_DISABLED, KEY_OWNED_BY_DIFFERENT_TENANT, KEYPAIR_NOT_FOUND,
TRUSTED_KEY_CAP_REACHED, UNSUPPORTED_ALGORITHM, UNSUPPORTED_KEY_TYPE.

Refs #281."
```

---

### Task 2: Add IAMConfig struct + AuthConfig.IAM field

**Files:**
- Create: `internal/auth/iam_config.go`
- Modify: `internal/auth/service.go` (add `IAM IAMConfig` field on `AuthConfig`)

- [ ] **Step 1: Create iam_config.go**

Write `internal/auth/iam_config.go`:

```go
package auth

// IAMConfig bundles IAM-feature configuration consumed by the
// /oauth/keys/* adapter surface and bootstrap wiring.
//
// All fields are read-only after construction. Value-copied into the
// account.Handler at startup (see internal/domain/account/handler.go).
type IAMConfig struct {
	// TrustedKeyRegistrationEnabled gates all 5 trusted-key endpoints.
	// When false, every trusted-key adapter returns 404 FEATURE_DISABLED.
	// Env: CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED. Default: false.
	TrustedKeyRegistrationEnabled bool

	// TrustedKeyMaxPerTenant caps registered trusted keys per tenant.
	// Counts only currently-valid keys (Active && (ValidTo == nil || now < ValidTo)).
	// 0 means unbounded.
	// Env: CYODA_IAM_TRUSTED_KEY_MAX_PER_TENANT. Default: 10.
	TrustedKeyMaxPerTenant int

	// TrustedKeyMaxValidityDays is the DEFAULT validTo for trusted keys
	// when the registration request omits validTo. No clamp on user-supplied
	// validTo values (cloud parity).
	// Env: CYODA_IAM_TRUSTED_KEY_MAX_VALIDITY_DAYS. Default: 365.
	TrustedKeyMaxValidityDays int

	// TrustedKeyMaxJWKProperties caps the number of properties in the
	// JWK object on register. Exceeding returns 400 BAD_REQUEST.
	// Env: CYODA_IAM_TRUSTED_KEY_MAX_JWK_PROPERTIES. Default: 20.
	TrustedKeyMaxJWKProperties int

	// KeypairDefaultValidityDays is the default validTo for both the
	// bootstrap key and runtime-issued keypairs. Applied at issue time.
	// Env: CYODA_IAM_KEYPAIR_DEFAULT_VALIDITY_DAYS. Default: 365.
	KeypairDefaultValidityDays int

	// BootstrapAudience is the audience assigned to the bootstrap
	// PEM-derived signing key. Must be "human" or "client".
	// Env: CYODA_JWT_BOOTSTRAP_AUDIENCE. Default: "client".
	BootstrapAudience string
}

// DefaultIAMConfig returns the cyoda-go defaults. Used when no env vars are set.
func DefaultIAMConfig() IAMConfig {
	return IAMConfig{
		TrustedKeyRegistrationEnabled: false,
		TrustedKeyMaxPerTenant:        10,
		TrustedKeyMaxValidityDays:     365,
		TrustedKeyMaxJWKProperties:    20,
		KeypairDefaultValidityDays:    365,
		BootstrapAudience:             "client",
	}
}

// Validate checks IAMConfig fields against the rules documented in the spec.
// Returns nil on success, an error suitable for startup logging on failure.
// Called once at boot from app/config.go.
func (c IAMConfig) Validate() error {
	if c.BootstrapAudience != "human" && c.BootstrapAudience != "client" {
		return fmtErrf("CYODA_JWT_BOOTSTRAP_AUDIENCE must be 'human' or 'client', got %q", c.BootstrapAudience)
	}
	if c.TrustedKeyMaxPerTenant < 0 {
		return fmtErrf("CYODA_IAM_TRUSTED_KEY_MAX_PER_TENANT must be >= 0, got %d", c.TrustedKeyMaxPerTenant)
	}
	if c.TrustedKeyMaxValidityDays <= 0 {
		return fmtErrf("CYODA_IAM_TRUSTED_KEY_MAX_VALIDITY_DAYS must be > 0, got %d", c.TrustedKeyMaxValidityDays)
	}
	if c.TrustedKeyMaxJWKProperties <= 0 {
		return fmtErrf("CYODA_IAM_TRUSTED_KEY_MAX_JWK_PROPERTIES must be > 0, got %d", c.TrustedKeyMaxJWKProperties)
	}
	if c.KeypairDefaultValidityDays <= 0 {
		return fmtErrf("CYODA_IAM_KEYPAIR_DEFAULT_VALIDITY_DAYS must be > 0, got %d", c.KeypairDefaultValidityDays)
	}
	return nil
}

// fmtErrf is a tiny shim to avoid importing fmt at the top of the file when
// Validate is the only consumer. Replace with fmt.Errorf at implementation
// time — Go's standard "import 'fmt'" + fmt.Errorf is the conventional shape.
func fmtErrf(format string, args ...any) error {
	// Replaced inline at edit time with fmt.Errorf.
	return nil
}
```

Replace `fmtErrf` with `fmt.Errorf` and add `"fmt"` to the imports — this is purely a write-time idiom so the skeleton compiles before adding the import.

- [ ] **Step 2: Add `IAM IAMConfig` field to AuthConfig**

Edit `internal/auth/service.go` lines 13-18 (the `AuthConfig` struct):

```go
type AuthConfig struct {
	SigningKeyPEM   string          // PEM-encoded RSA private key
	Issuer          string          // e.g., "cyoda"
	ExpirySeconds   int             // e.g., 3600
	TrustedKeyStore TrustedKeyStore // optional: externally-provided persistent store; if nil, uses in-memory
	IAM             IAMConfig       // IAM feature configuration; see iam_config.go
}
```

- [ ] **Step 3: Verify build**

Run: `go build ./internal/auth/...`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/auth/iam_config.go internal/auth/service.go
git commit -m "feat(auth): add IAMConfig struct + AuthConfig.IAM field

Six fields for IAM-feature configuration (trusted-key flag, caps,
keypair default validity, bootstrap audience). DefaultIAMConfig()
returns cyoda-go defaults. Validate() enforces invariants at boot.

Refs #281."
```

---

### Task 3: Move JWK validators from trusted.go to keyvalidation.go

**Files:**
- Create: `internal/auth/keyvalidation.go`
- Modify: `internal/auth/trusted.go` (delete the functions moved out)

- [ ] **Step 1: Create keyvalidation.go with the moved functions**

Write `internal/auth/keyvalidation.go`:

```go
package auth

import (
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"regexp"
)

// trustedKIDPattern is the character whitelist enforced on trusted-key
// identifiers across every lifecycle endpoint (register, delete, invalidate,
// reactivate). Allowed: ASCII alphanumerics plus '-', '_', '.', length 1..128.
var trustedKIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// parseRSAPublicKeyFromJWK parses an RSA public key from a JWK JSON object.
// Returns the rsa.PublicKey or an error describing the validation failure.
//
// Callers MUST check kty separately if they need a specific 400 error code;
// this function rejects non-RSA kty with a generic error suitable for 400 BAD_REQUEST.
// For UNSUPPORTED_KEY_TYPE responses, inspect kty before calling.
func parseRSAPublicKeyFromJWK(jwkData json.RawMessage) (*rsa.PublicKey, error) {
	if len(jwkData) == 0 {
		return nil, fmt.Errorf("empty JWK")
	}
	var jwk struct {
		Kty string `json:"kty"`
		N   string `json:"n"`
		E   string `json:"e"`
	}
	if err := json.Unmarshal(jwkData, &jwk); err != nil {
		return nil, fmt.Errorf("failed to parse JWK: %w", err)
	}
	if jwk.Kty != "RSA" {
		return nil, fmt.Errorf("unsupported key type: %s (only RSA supported)", jwk.Kty)
	}
	if jwk.N == "" || jwk.E == "" {
		return nil, fmt.Errorf("missing n or e in JWK")
	}

	nBytes, err := decodeBase64URL(jwk.N)
	if err != nil {
		return nil, fmt.Errorf("invalid n value: %w", err)
	}
	eBytes, err := decodeBase64URL(jwk.E)
	if err != nil {
		return nil, fmt.Errorf("invalid e value: %w", err)
	}

	n := new(big.Int).SetBytes(nBytes)
	eBig := new(big.Int).SetBytes(eBytes)
	e, err := validateRSAPublicExponent(eBig)
	if err != nil {
		return nil, err
	}

	return &rsa.PublicKey{N: n, E: e}, nil
}

// validateRSAPublicExponent enforces the integrity invariants on an RSA
// public-key exponent: positive, fits in int, and odd.
func validateRSAPublicExponent(e *big.Int) (int, error) {
	if e.Sign() <= 0 {
		return 0, fmt.Errorf("rsa exponent must be positive")
	}
	if !e.IsInt64() {
		return 0, fmt.Errorf("rsa exponent does not fit in int64")
	}
	v := e.Int64()
	if v > int64(math.MaxInt) {
		return 0, fmt.Errorf("rsa exponent does not fit in int")
	}
	if v&1 == 0 {
		return 0, fmt.Errorf("rsa exponent must be odd")
	}
	return int(v), nil
}
```

- [ ] **Step 2: Delete the moved declarations from trusted.go**

Edit `internal/auth/trusted.go` — DELETE:
- `trustedKIDPattern` (lines 26)
- `parseRSAPublicKeyFromJWK` function
- `validateRSAPublicExponent` function

Keep the rest of `trusted.go` for now (the HTTP handlers will be deleted in Task 25).

- [ ] **Step 3: Verify build (no duplicate declarations)**

Run: `go build ./internal/auth/...`
Expected: PASS.

- [ ] **Step 4: Run existing tests to confirm no regression**

Run: `go test ./internal/auth/... -short`
Expected: PASS (all existing tests continue to work with the moved validators).

- [ ] **Step 5: Commit**

```bash
git add internal/auth/keyvalidation.go internal/auth/trusted.go
git commit -m "refactor(auth): move JWK validators to keyvalidation.go

trustedKIDPattern, parseRSAPublicKeyFromJWK, validateRSAPublicExponent
moved out of trusted.go so they survive trusted.go's eventual deletion.
No behaviour change; existing tests unchanged.

Refs #281."
```

---

### Task 4: Extend KeyPair and TrustedKey types + RotateOptions

**Files:**
- Modify: `internal/auth/store.go` (KeyPair, TrustedKey type definitions)

- [ ] **Step 1: Update KeyPair struct**

Edit `internal/auth/store.go` lines 19-26. Replace with:

```go
// KeyPair holds an RSA key pair with metadata.
type KeyPair struct {
	KID        string
	Audience   string // "human" | "client"; selects the partition for GetActive
	Algorithm  string // RS256 only in v0.8.0; other enum values rejected at adapter
	PublicKey  *rsa.PublicKey
	PrivateKey *rsa.PrivateKey
	Active     bool
	ValidFrom  time.Time
	ValidTo    *time.Time // nil = no expiry; set on invalidate to now+grace
}
```

Note: `ValidFrom` is the rename of `CreatedAt`.

- [ ] **Step 2: Update TrustedKey struct**

Edit `internal/auth/store.go` lines 28-37. Replace with:

```go
// TrustedKey holds a trusted external public key, tenant-scoped.
type TrustedKey struct {
	KID       string
	TenantID  spi.TenantID    // owning tenant; derived server-side from caller's UserContext
	JWK       map[string]any  // original JWK for verbatim round-trip on read
	PublicKey *rsa.PublicKey
	Audience  string
	Issuers   []string
	Active    bool
	ValidFrom time.Time
	ValidTo   *time.Time
}
```

Add the `spi` import to the file's import block if not present:

```go
spi "github.com/cyoda-platform/cyoda-go-spi"
```

- [ ] **Step 3: Add RotateOptions type**

Edit `internal/auth/store.go` — append after the M2MClient struct (around line 46):

```go
// RotateOptions carries the invalidate-siblings option for both Save (keypair)
// and Register (trusted). The store atomically inserts the new entry and
// flips siblings in the same partition (keypair: same Audience; trusted:
// same TenantID) to Active=false, ValidTo=now+GracePeriodSec, when
// Invalidate=true. Grace=0 means immediate.
//
// DTO field names differ on the wire (invalidateCurrent vs invalidatePrevious);
// the store layer uses a single shape.
type RotateOptions struct {
	Invalidate     bool
	GracePeriodSec int64
}
```

- [ ] **Step 4: Verify build (will fail until store interface updated in Task 5; that's expected)**

Run: `go build ./internal/auth/...`
Expected: FAIL with errors about the in-memory store not matching the (yet-unchanged) interface, OR PASS if interface still uses old shape. Either is fine — Task 5 fixes it.

- [ ] **Step 5: Commit**

```bash
git add internal/auth/store.go
git commit -m "feat(auth): extend KeyPair and TrustedKey types; add RotateOptions

KeyPair: rename CreatedAt -> ValidFrom; add Audience, Algorithm,
ValidTo *time.Time.
TrustedKey: add TenantID, JWK map[string]any.
RotateOptions{Invalidate, GracePeriodSec} folds atomic sibling-flip
into Save/Register call signatures.

Refs #281."
```

---

## Phase 2: Store layer refactor (TDD)

### Task 5: KeyStore interface + InMemoryKeyStore (TDD)

**Files:**
- Modify: `internal/auth/store.go` (KeyStore interface + InMemoryKeyStore methods)
- Modify: `internal/auth/store_test.go` (new test cases)

- [ ] **Step 1: Write failing tests for the new KeyStore behaviours**

Add to `internal/auth/store_test.go`:

```go
func TestKeyStore_GetActive_AudiencePartition(t *testing.T) {
	s := auth.NewInMemoryKeyStore()
	now := time.Now()
	human := &auth.KeyPair{KID: "h1", Audience: "human", Algorithm: "RS256", Active: true, ValidFrom: now}
	client := &auth.KeyPair{KID: "c1", Audience: "client", Algorithm: "RS256", Active: true, ValidFrom: now}
	if err := s.Save(human, auth.RotateOptions{}); err != nil {
		t.Fatalf("save human: %v", err)
	}
	if err := s.Save(client, auth.RotateOptions{}); err != nil {
		t.Fatalf("save client: %v", err)
	}
	got, err := s.GetActive("human")
	if err != nil || got.KID != "h1" {
		t.Fatalf("GetActive(human): got=%+v err=%v", got, err)
	}
	got, err = s.GetActive("client")
	if err != nil || got.KID != "c1" {
		t.Fatalf("GetActive(client): got=%+v err=%v", got, err)
	}
	if _, err := s.GetActive("robot"); err == nil {
		t.Fatal("GetActive(robot) should return error")
	}
}

func TestKeyStore_GetActive_MaxValidFrom(t *testing.T) {
	s := auth.NewInMemoryKeyStore()
	older := &auth.KeyPair{KID: "old", Audience: "client", Algorithm: "RS256", Active: true, ValidFrom: time.Now().Add(-1 * time.Hour)}
	newer := &auth.KeyPair{KID: "new", Audience: "client", Algorithm: "RS256", Active: true, ValidFrom: time.Now()}
	_ = s.Save(older, auth.RotateOptions{})
	_ = s.Save(newer, auth.RotateOptions{})
	got, err := s.GetActive("client")
	if err != nil || got.KID != "new" {
		t.Fatalf("expected newer ValidFrom selected, got %+v err=%v", got, err)
	}
}

func TestKeyStore_Save_InvalidateFlipsSiblings(t *testing.T) {
	s := auth.NewInMemoryKeyStore()
	now := time.Now()
	existing := &auth.KeyPair{KID: "e1", Audience: "client", Algorithm: "RS256", Active: true, ValidFrom: now}
	_ = s.Save(existing, auth.RotateOptions{})
	fresh := &auth.KeyPair{KID: "f1", Audience: "client", Algorithm: "RS256", Active: true, ValidFrom: now.Add(1 * time.Second)}
	if err := s.Save(fresh, auth.RotateOptions{Invalidate: true, GracePeriodSec: 60}); err != nil {
		t.Fatalf("save with rotate: %v", err)
	}
	old, _ := s.Get("e1")
	if old.Active {
		t.Error("expected e1 Active=false")
	}
	if old.ValidTo == nil {
		t.Error("expected e1 ValidTo set")
	}
}

func TestKeyStore_ListForVerification_LazyFilter(t *testing.T) {
	s := auth.NewInMemoryKeyStore()
	now := time.Now()
	past := now.Add(-1 * time.Hour)
	active := &auth.KeyPair{KID: "active", Audience: "client", Algorithm: "RS256", Active: true, ValidFrom: now}
	expired := &auth.KeyPair{KID: "expired", Audience: "client", Algorithm: "RS256", Active: false, ValidFrom: past, ValidTo: &past}
	_ = s.Save(active, auth.RotateOptions{})
	_ = s.Save(expired, auth.RotateOptions{})
	got := s.ListForVerification()
	if len(got) != 1 || got[0].KID != "active" {
		t.Fatalf("expected only active, got %+v", got)
	}
}

func TestKeyStore_Reactivate_RequiresFreshWindow(t *testing.T) {
	s := auth.NewInMemoryKeyStore()
	now := time.Now()
	past := now.Add(-1 * time.Hour)
	expired := &auth.KeyPair{KID: "e", Audience: "client", Algorithm: "RS256", Active: false, ValidFrom: past, ValidTo: &past}
	_ = s.Save(expired, auth.RotateOptions{})
	if err := s.Reactivate("e", now, now.Add(24*time.Hour)); err != nil {
		t.Fatalf("reactivate failed: %v", err)
	}
	got, _ := s.Get("e")
	if !got.Active {
		t.Error("expected Active=true")
	}
	if got.ValidTo == nil || !got.ValidTo.After(now) {
		t.Errorf("expected ValidTo > now, got %v", got.ValidTo)
	}
}
```

- [ ] **Step 2: Run tests to confirm they fail (interface mismatch or missing methods)**

Run: `go test ./internal/auth/ -run TestKeyStore -v`
Expected: FAIL — compile errors (Save signature mismatch, Reactivate signature mismatch, missing ListForVerification, GetActive doesn't take audience).

- [ ] **Step 3: Update the KeyStore interface in store.go**

Replace the existing `KeyStore` interface (around line 50-59) with:

```go
type KeyStore interface {
	Save(kp *KeyPair, opts RotateOptions) error
	Get(kid string) (*KeyPair, error)
	// GetActive returns the active keypair for the given audience with the
	// maximum ValidFrom. Returns an error if no active key exists for the audience.
	GetActive(audience string) (*KeyPair, error)
	List() []*KeyPair
	// ListForVerification returns active keypairs that pass the lazy ValidTo
	// filter (used by JWKS endpoint to publish grace-period keys).
	ListForVerification() []*KeyPair
	Delete(kid string) error
	Invalidate(kid string, gracePeriodSec int64) error
	// Reactivate requires a fresh validity window. validTo must be > now and > validFrom.
	// The store enforces validTo > now; the caller is responsible for validFrom default
	// (typically now if not supplied).
	Reactivate(kid string, validFrom, validTo time.Time) error
}
```

- [ ] **Step 4: Update InMemoryKeyStore methods to match the new interface**

Replace `Save`, `GetActive`, add `ListForVerification`, replace `Invalidate`, replace `Reactivate` in `store.go`. Sketch:

```go
func (s *InMemoryKeyStore) Save(kp *KeyPair, opts RotateOptions) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if opts.Invalidate {
		now := time.Now()
		expiry := now.Add(time.Duration(opts.GracePeriodSec) * time.Second)
		for _, existing := range s.keys {
			if existing.Audience == kp.Audience && existing.Active && existing.KID != kp.KID {
				existing.Active = false
				e := expiry
				existing.ValidTo = &e
			}
		}
	}
	s.keys[kp.KID] = kp
	return nil
}

func (s *InMemoryKeyStore) GetActive(audience string) (*KeyPair, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var best *KeyPair
	for _, kp := range s.keys {
		if kp.Audience != audience || !kp.Active {
			continue
		}
		if kp.ValidTo != nil && !time.Now().Before(*kp.ValidTo) {
			continue
		}
		if best == nil || kp.ValidFrom.After(best.ValidFrom) {
			best = kp
		}
	}
	if best == nil {
		return nil, fmt.Errorf("no active key pair for audience %q", audience)
	}
	return best, nil
}

func (s *InMemoryKeyStore) ListForVerification() []*KeyPair {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := time.Now()
	out := make([]*KeyPair, 0, len(s.keys))
	for _, kp := range s.keys {
		if kp.ValidTo == nil || now.Before(*kp.ValidTo) {
			out = append(out, kp)
		}
	}
	return out
}

func (s *InMemoryKeyStore) Invalidate(kid string, gracePeriodSec int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	kp, ok := s.keys[kid]
	if !ok {
		return fmt.Errorf("key pair not found: %s", kid)
	}
	expiry := time.Now().Add(time.Duration(gracePeriodSec) * time.Second)
	kp.Active = false
	kp.ValidTo = &expiry
	return nil
}

func (s *InMemoryKeyStore) Reactivate(kid string, validFrom, validTo time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	kp, ok := s.keys[kid]
	if !ok {
		return fmt.Errorf("key pair not found: %s", kid)
	}
	if !validTo.After(time.Now()) {
		return fmt.Errorf("validTo must be in the future")
	}
	if !validTo.After(validFrom) {
		return fmt.Errorf("validTo must be after validFrom")
	}
	kp.Active = true
	kp.ValidFrom = validFrom
	vt := validTo
	kp.ValidTo = &vt
	return nil
}
```

Also delete the now-obsolete previous `Save`, `GetActive`, `Invalidate`, `Reactivate` implementations.

- [ ] **Step 5: Run new tests + existing keystore tests**

Run: `go test ./internal/auth/ -run "TestKeyStore|TestInMemoryKeyStore" -v`
Expected: PASS (all new tests pass; existing tests may need updates if they assume the old API — fix them inline by passing `RotateOptions{}` to `Save` and updating `Reactivate` call sites).

- [ ] **Step 6: Run full auth-package tests**

Run: `go test ./internal/auth/ -short -v`
Expected: PASS (compile errors elsewhere will be addressed in subsequent tasks; if a non-test file fails to compile, fix the call site by passing `RotateOptions{}` and the new `Reactivate` args).

- [ ] **Step 7: Commit**

```bash
git add internal/auth/store.go internal/auth/store_test.go
git commit -m "feat(auth): KeyStore audience partition + RotateOptions + lazy ValidTo

KeyStore.Save accepts RotateOptions{Invalidate, GracePeriodSec} for
atomic sibling-flip across same-audience partition. GetActive(audience)
selects max-ValidFrom. ListForVerification applies lazy ValidTo filter.
Reactivate requires fresh validity window (validTo > now, > validFrom).

Refs #281."
```

---

### Task 6: TrustedKeyStore interface + InMemoryTrustedKeyStore (TDD)

**Files:**
- Modify: `internal/auth/store.go` (TrustedKeyStore interface + InMemoryTrustedKeyStore methods)
- Modify: `internal/auth/store_test.go` (new test cases)

- [ ] **Step 1: Write failing tests**

Append to `internal/auth/store_test.go`:

```go
func TestTrustedKeyStore_TenantIsolation(t *testing.T) {
	s := auth.NewInMemoryTrustedKeyStore()
	tenantA := spi.TenantID("tenant-a")
	tenantB := spi.TenantID("tenant-b")
	kA := &auth.TrustedKey{KID: "k1", TenantID: tenantA, PublicKey: testRSAPubKey(t), Audience: "human", Active: true, ValidFrom: time.Now()}
	if err := s.Register(kA, auth.RotateOptions{}); err != nil {
		t.Fatalf("register tenantA: %v", err)
	}
	if _, err := s.Get(tenantB, "k1"); err == nil {
		t.Error("tenantB.Get(k1) should return not-found")
	}
	if err := s.Delete(tenantB, "k1"); err == nil {
		t.Error("tenantB.Delete(k1) should return not-found")
	}
}

func TestTrustedKeyStore_Register_CrossTenantCollision(t *testing.T) {
	s := auth.NewInMemoryTrustedKeyStore()
	tenantA := spi.TenantID("tenant-a")
	tenantB := spi.TenantID("tenant-b")
	kA := &auth.TrustedKey{KID: "shared", TenantID: tenantA, PublicKey: testRSAPubKey(t), Audience: "human", Active: true, ValidFrom: time.Now()}
	kB := &auth.TrustedKey{KID: "shared", TenantID: tenantB, PublicKey: testRSAPubKey(t), Audience: "human", Active: true, ValidFrom: time.Now()}
	_ = s.Register(kA, auth.RotateOptions{})
	err := s.Register(kB, auth.RotateOptions{})
	if err == nil {
		t.Fatal("expected cross-tenant collision error")
	}
	// Implementation returns an AppError with KEY_OWNED_BY_DIFFERENT_TENANT code.
}

func TestTrustedKeyStore_Register_CapReached(t *testing.T) {
	s := auth.NewInMemoryTrustedKeyStoreWithCap(2)
	tenant := spi.TenantID("t")
	mk := func(kid string) *auth.TrustedKey {
		return &auth.TrustedKey{KID: kid, TenantID: tenant, PublicKey: testRSAPubKey(t), Audience: "human", Active: true, ValidFrom: time.Now()}
	}
	_ = s.Register(mk("k1"), auth.RotateOptions{})
	_ = s.Register(mk("k2"), auth.RotateOptions{})
	err := s.Register(mk("k3"), auth.RotateOptions{})
	if err == nil {
		t.Fatal("expected cap-reached error")
	}
}

func TestTrustedKeyStore_Register_CapCountsValidOnly(t *testing.T) {
	s := auth.NewInMemoryTrustedKeyStoreWithCap(2)
	tenant := spi.TenantID("t")
	past := time.Now().Add(-1 * time.Hour)
	expired := &auth.TrustedKey{KID: "old", TenantID: tenant, PublicKey: testRSAPubKey(t), Audience: "human", Active: false, ValidFrom: past, ValidTo: &past}
	active := &auth.TrustedKey{KID: "new", TenantID: tenant, PublicKey: testRSAPubKey(t), Audience: "human", Active: true, ValidFrom: time.Now()}
	_ = s.Register(expired, auth.RotateOptions{})
	_ = s.Register(active, auth.RotateOptions{})
	// Cap is 2; expired no longer counts; should accept a third
	third := &auth.TrustedKey{KID: "third", TenantID: tenant, PublicKey: testRSAPubKey(t), Audience: "human", Active: true, ValidFrom: time.Now()}
	if err := s.Register(third, auth.RotateOptions{}); err != nil {
		t.Fatalf("expected accept (expired excluded from count), got %v", err)
	}
}

func TestTrustedKeyStore_Reactivate_RequiresFreshWindow(t *testing.T) {
	s := auth.NewInMemoryTrustedKeyStore()
	tenant := spi.TenantID("t")
	now := time.Now()
	past := now.Add(-1 * time.Hour)
	expired := &auth.TrustedKey{KID: "e", TenantID: tenant, PublicKey: testRSAPubKey(t), Audience: "human", Active: false, ValidFrom: past, ValidTo: &past}
	_ = s.Register(expired, auth.RotateOptions{})
	if err := s.Reactivate(tenant, "e", now, now.Add(24*time.Hour)); err != nil {
		t.Fatalf("reactivate failed: %v", err)
	}
	if err := s.Reactivate(tenant, "e", now, past); err == nil {
		t.Error("expected reactivate to reject past validTo")
	}
}

// testRSAPubKey returns a freshly-generated RSA public key for tests.
func testRSAPubKey(t *testing.T) *rsa.PublicKey {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	return &priv.PublicKey
}
```

Add required imports: `"crypto/rand"`, `"crypto/rsa"`, `spi "github.com/cyoda-platform/cyoda-go-spi"`.

- [ ] **Step 2: Run tests to confirm they fail**

Run: `go test ./internal/auth/ -run TestTrustedKeyStore -v`
Expected: FAIL (interface mismatch + missing methods).

- [ ] **Step 3: Update TrustedKeyStore interface**

Replace in `store.go`:

```go
type TrustedKeyStore interface {
	// Register inserts a new trusted key. Returns:
	//  - KEY_OWNED_BY_DIFFERENT_TENANT (409) on cross-tenant keyId collision.
	//  - TRUSTED_KEY_CAP_REACHED (400) when the tenant's currently-valid count >= cap.
	//  - Silent upsert for same-tenant same-kid (preserves existing behaviour).
	// When opts.Invalidate is true, atomically flips other Active keys for the
	// same TenantID to Active=false, ValidTo=now+grace.
	Register(tk *TrustedKey, opts RotateOptions) error
	// Get returns the trusted key for (tenantID, kid). Cross-tenant or missing -> error.
	Get(tenantID spi.TenantID, kid string) (*TrustedKey, error)
	List(tenantID spi.TenantID) []*TrustedKey
	// ListForVerification returns all-tenant keys passing lazy ValidTo filter.
	// Used by the in-package getTrustedKeyByKID helper in token.go.
	ListForVerification() []*TrustedKey
	Delete(tenantID spi.TenantID, kid string) error
	Invalidate(tenantID spi.TenantID, kid string, gracePeriodSec int64) error
	Reactivate(tenantID spi.TenantID, kid string, validFrom, validTo time.Time) error
}
```

- [ ] **Step 4: Update InMemoryTrustedKeyStore + add cap constructor**

In `store.go`, replace the `InMemoryTrustedKeyStore` methods. Sketch (use existing `common` errors for the AppError shape):

```go
import "github.com/cyoda-platform/cyoda-go/internal/common"
import "net/http"

type InMemoryTrustedKeyStore struct {
	mu             sync.RWMutex
	keys           map[string]*TrustedKey
	maxPerTenant   int
}

func NewInMemoryTrustedKeyStore() *InMemoryTrustedKeyStore {
	return NewInMemoryTrustedKeyStoreWithCap(0)
}

func NewInMemoryTrustedKeyStoreWithCap(cap int) *InMemoryTrustedKeyStore {
	return &InMemoryTrustedKeyStore{keys: make(map[string]*TrustedKey), maxPerTenant: cap}
}

func (s *InMemoryTrustedKeyStore) Register(tk *TrustedKey, opts RotateOptions) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Cross-tenant collision check.
	if existing, ok := s.keys[tk.KID]; ok && existing.TenantID != tk.TenantID {
		return common.Operational(http.StatusConflict, common.ErrCodeKeyOwnedByDifferentTenant, "key with this keyId belongs to a different tenant")
	}

	// Cap (counts only currently-valid keys for this tenant).
	if s.maxPerTenant > 0 {
		now := time.Now()
		count := 0
		for _, k := range s.keys {
			if k.TenantID != tk.TenantID || k.KID == tk.KID {
				continue
			}
			if !k.Active {
				continue
			}
			if k.ValidTo != nil && !now.Before(*k.ValidTo) {
				continue
			}
			count++
		}
		if count >= s.maxPerTenant {
			return common.Operational(http.StatusBadRequest, common.ErrCodeTrustedKeyCapReached, "trusted-key cap reached for tenant")
		}
	}

	// Atomic sibling-flip.
	if opts.Invalidate {
		now := time.Now()
		expiry := now.Add(time.Duration(opts.GracePeriodSec) * time.Second)
		for _, k := range s.keys {
			if k.TenantID == tk.TenantID && k.Active && k.KID != tk.KID {
				k.Active = false
				e := expiry
				k.ValidTo = &e
			}
		}
	}

	s.keys[tk.KID] = tk
	return nil
}

func (s *InMemoryTrustedKeyStore) Get(tenantID spi.TenantID, kid string) (*TrustedKey, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	k, ok := s.keys[kid]
	if !ok || k.TenantID != tenantID {
		return nil, fmt.Errorf("trusted key not found")
	}
	return k, nil
}

func (s *InMemoryTrustedKeyStore) List(tenantID spi.TenantID) []*TrustedKey {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*TrustedKey, 0)
	for _, k := range s.keys {
		if k.TenantID == tenantID {
			out = append(out, k)
		}
	}
	return out
}

func (s *InMemoryTrustedKeyStore) ListForVerification() []*TrustedKey {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := time.Now()
	out := make([]*TrustedKey, 0, len(s.keys))
	for _, k := range s.keys {
		if k.ValidTo == nil || now.Before(*k.ValidTo) {
			out = append(out, k)
		}
	}
	return out
}

func (s *InMemoryTrustedKeyStore) Delete(tenantID spi.TenantID, kid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[kid]
	if !ok || k.TenantID != tenantID {
		return fmt.Errorf("trusted key not found")
	}
	delete(s.keys, kid)
	return nil
}

func (s *InMemoryTrustedKeyStore) Invalidate(tenantID spi.TenantID, kid string, gracePeriodSec int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[kid]
	if !ok || k.TenantID != tenantID {
		return fmt.Errorf("trusted key not found")
	}
	expiry := time.Now().Add(time.Duration(gracePeriodSec) * time.Second)
	k.Active = false
	k.ValidTo = &expiry
	return nil
}

func (s *InMemoryTrustedKeyStore) Reactivate(tenantID spi.TenantID, kid string, validFrom, validTo time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[kid]
	if !ok || k.TenantID != tenantID {
		return fmt.Errorf("trusted key not found")
	}
	if !validTo.After(time.Now()) {
		return fmt.Errorf("validTo must be in the future")
	}
	if !validTo.After(validFrom) {
		return fmt.Errorf("validTo must be after validFrom")
	}
	k.Active = true
	k.ValidFrom = validFrom
	vt := validTo
	k.ValidTo = &vt
	return nil
}
```

Delete the previous `InMemoryTrustedKeyStore` method implementations.

- [ ] **Step 5: Run tests**

Run: `go test ./internal/auth/ -run TestTrustedKeyStore -v`
Expected: PASS.

- [ ] **Step 6: Run all auth tests to surface call-site breakages**

Run: `go test ./internal/auth/ -short`
Expected: Either PASS, or compile errors in `service.go` / `trusted.go` / `kv_trusted_store.go` / `token.go`. Fix call sites mechanically:
- `Register(tk)` → `Register(tk, auth.RotateOptions{})`
- `Get(kid)` → `Get(tenantID, kid)` — for `trusted.go` (deleted later in Task 25), the file is doomed; if it fails to build, that's acceptable here, but easier to keep it compiling by passing the user's tenant. For now, comment-out the failing trusted.go body (since Task 25 deletes it) or add `tenantID := spi.GetUserContext(r.Context()).Tenant.ID` then call `Get(tenantID, ...)`.

Practical: edit `internal/auth/trusted.go` to fully comment-out the file body (keep package declaration only) — it's being deleted in Task 25 anyway, so this minimal stub keeps the build green here.

- [ ] **Step 7: Commit**

```bash
git add internal/auth/store.go internal/auth/store_test.go internal/auth/trusted.go
git commit -m "feat(auth): TrustedKeyStore tenant scoping + cap + lazy ValidTo

Register returns 409 KEY_OWNED_BY_DIFFERENT_TENANT on cross-tenant
collision; 400 TRUSTED_KEY_CAP_REACHED on cap (counts only
currently-valid keys per cloud). Atomic sibling-flip on Invalidate.
Get/List/Delete/Invalidate/Reactivate tenant-scoped.
trusted.go body commented pending Task 25 deletion.

Refs #281."
```


---

### Task 7: KVTrustedKeyStore — tenant-scoped key shape + schema migration

**Files:**
- Modify: `internal/auth/kv_trusted_store.go`
- Modify: `internal/auth/kv_trusted_store_test.go`

- [ ] **Step 1: Read current `kv_trusted_store.go` to understand key encoding, record schema, and `loadAll`**

Run: `wc -l internal/auth/kv_trusted_store.go && grep -n "func \|kvKey\|trustedkey:\|Marshal\|loadAll\|loadOne\|persist\b" internal/auth/kv_trusted_store.go`

Identify: the `kvKey(kid string) string` helper, the `trustedKeyRecord` struct, the `loadAll(ctx)` method, and the `loadOne(ctx, kid)` method. Note their line ranges before editing.

- [ ] **Step 2: Write failing tests for tenant-scoped key shape + cache-pollution guard**

Add to `internal/auth/kv_trusted_store_test.go`:

```go
func TestKVTrustedKeyStore_TenantScopedKeyShape(t *testing.T) {
	mem := memory.NewKeyValueStore()
	ctx := systemCtx()
	store, err := auth.NewKVTrustedKeyStore(ctx, mem)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	tenantA := spi.TenantID("tenant-a")
	tk := &auth.TrustedKey{KID: "k1", TenantID: tenantA, PublicKey: testRSAPubKey(t), Audience: "human", Active: true, ValidFrom: time.Now()}
	if err := store.Register(tk, auth.RotateOptions{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	// Verify the KV entry lives at trustedkey:<tenantID>:<kid> shape.
	keys := mem.ListNamespaceKeys()
	found := false
	for _, k := range keys {
		if k == "trustedkey:tenant-a:k1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected key 'trustedkey:tenant-a:k1' in KV, got: %v", keys)
	}
}

func TestKVTrustedKeyStore_NoCrossTenantCachePollution(t *testing.T) {
	mem := memory.NewKeyValueStore()
	ctx := systemCtx()
	store, _ := auth.NewKVTrustedKeyStore(ctx, mem)
	tenantA := spi.TenantID("tenant-a")
	tenantB := spi.TenantID("tenant-b")
	tk := &auth.TrustedKey{KID: "k1", TenantID: tenantA, PublicKey: testRSAPubKey(t), Audience: "human", Active: true, ValidFrom: time.Now()}
	_ = store.Register(tk, auth.RotateOptions{})
	// tenantB.Get(k1) must not surface tenantA's key.
	if _, err := store.Get(tenantB, "k1"); err == nil {
		t.Error("tenantB.Get returned a key it should not see")
	}
	// tenantA.Get(k1) still works.
	if _, err := store.Get(tenantA, "k1"); err != nil {
		t.Errorf("tenantA.Get failed: %v", err)
	}
}
```

Adjust `memory.NewKeyValueStore` and `ListNamespaceKeys` to match the actual `plugins/memory` API; if `ListNamespaceKeys` doesn't exist, walk `mem.List(ctx, "")` or whatever the in-memory adapter exposes.

- [ ] **Step 3: Run tests to confirm failure**

Run: `go test ./internal/auth/ -run TestKVTrustedKeyStore -v`
Expected: FAIL — either compile error (new method signatures) or wrong key shape.

- [ ] **Step 4: Update `kvKey` helper to include tenantID**

Replace the single-arg `kvKey(kid string) string` with:

```go
// kvKey returns the KV-store key for a (tenantID, kid) pair. Layout
// "trustedkey:<tenantID>:<kid>" makes tenant isolation a storage-layer
// invariant — cross-tenant access cannot succeed even if a caller bypasses
// the public API's tenant-scoped methods.
func kvKey(tenantID spi.TenantID, kid string) string {
	return "trustedkey:" + string(tenantID) + ":" + kid
}
```

- [ ] **Step 5: Update `trustedKeyRecord` schema to include `TenantID` and `JWK`**

Add the two new fields to the existing struct, and update Marshal/Unmarshal sites:

```go
type trustedKeyRecord struct {
	KID       string           `json:"kid"`
	TenantID  string           `json:"tenantID"` // NEW
	JWK       map[string]any   `json:"jwk"`      // NEW
	Audience  string           `json:"audience"`
	Issuers   []string         `json:"issuers,omitempty"`
	Active    bool             `json:"active"`
	ValidFrom time.Time        `json:"validFrom"`
	ValidTo   *time.Time       `json:"validTo,omitempty"`
	N         string           `json:"n"` // existing RSA modulus
	E         string           `json:"e"` // existing RSA exponent
}
```

Update `serializeTrustedKey` and `deserializeTrustedKey` accordingly. On read, `TenantID == ""` → treat as old-format entry (skip in `loadAll`).

- [ ] **Step 6: Update `loadAll(ctx)` to skip old-shape entries with one-shot WARN**

Sketch:

```go
func (s *KVTrustedKeyStore) loadAll(ctx context.Context) error {
	keys, err := s.kv.List(ctx, "trustedkey:")
	if err != nil {
		return fmt.Errorf("list trusted keys: %w", err)
	}
	oldShapeCount := 0
	for _, k := range keys {
		// New shape: trustedkey:<tenantID>:<kid>
		// Old shape: trustedkey:<kid> (one colon total)
		if strings.Count(k, ":") < 2 {
			oldShapeCount++
			continue
		}
		raw, err := s.kv.Get(ctx, k)
		if err != nil {
			continue
		}
		var rec trustedKeyRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			continue
		}
		if rec.TenantID == "" {
			oldShapeCount++
			continue
		}
		tk, err := deserializeTrustedKey(rec)
		if err != nil {
			continue
		}
		s.keys[tk.KID] = tk
	}
	if oldShapeCount > 0 {
		slog.Warn("pre-v0.8.0 trusted-key entries found in KV; not loaded under new key shape — re-register via /oauth/keys/trusted; see v0.8.0 release notes",
			"pkg", "auth", "count", oldShapeCount)
	}
	return nil
}
```

- [ ] **Step 7: Update Register/Get/List/Delete/Invalidate/Reactivate to new signatures**

Mirror the in-memory implementation but with `persist(ctx, kvKey(tenantID, kid), serializeTrustedKey(*tk))` calls. **Critical:** in `Register` with `opts.Invalidate=true`, perform all `s.kv.Put` calls for siblings FIRST, then do the in-memory map flip LAST. This ensures partial KV failure leaves the in-memory cache untouched (rollback safety, per spec §3.1 #7).

Tenant-scoped lookup pattern in `Get`:

```go
func (s *KVTrustedKeyStore) Get(tenantID spi.TenantID, kid string) (*TrustedKey, error) {
	s.mu.RLock()
	cached, ok := s.keys[kid]
	s.mu.RUnlock()
	if ok && cached.TenantID == tenantID {
		// Lazy ValidTo filter
		if cached.ValidTo == nil || time.Now().Before(*cached.ValidTo) {
			return cached, nil
		}
	}
	// Fall through to KV (handles cache miss + cache-stale-after-restart).
	raw, err := s.kv.Get(s.ctx, kvKey(tenantID, kid))
	if err != nil {
		return nil, fmt.Errorf("trusted key not found")
	}
	var rec trustedKeyRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	tk, err := deserializeTrustedKey(rec)
	if err != nil {
		return nil, err
	}
	if tk.TenantID != tenantID {
		// Defense in depth: should not happen given key encoding.
		return nil, fmt.Errorf("trusted key not found")
	}
	s.mu.Lock()
	s.keys[kid] = tk
	s.mu.Unlock()
	return tk, nil
}
```

- [ ] **Step 8: Run tests**

Run: `go test ./internal/auth/ -run TestKVTrustedKeyStore -v`
Expected: PASS.

- [ ] **Step 9: Run all auth tests**

Run: `go test ./internal/auth/ -short`
Expected: PASS (or compile errors in token.go / service.go — fix call sites with tenant arg).

- [ ] **Step 10: Commit**

```bash
git add internal/auth/kv_trusted_store.go internal/auth/kv_trusted_store_test.go
git commit -m "feat(auth): KVTrustedKeyStore tenant-scoped key shape + schema

KV key encoding: trustedkey:<tenantID>:<kid> (was trustedkey:<kid>).
trustedKeyRecord gains tenantID + jwk fields.
loadAll skips old-shape entries with one-shot startup WARN (count only,
no per-entry log; cyoda-go has no production users on this surface).
Cache invariant: tenant-scoped methods verify cached.TenantID matches
caller after both cache hit and post-loadOne re-cache.
Rotation atomicity: in-memory map mutation runs LAST so partial KV
failure leaves cache untouched.

Refs #281."
```

---

### Task 8: `getTrustedKeyByKID` in-package verification helper

**Files:**
- Modify: `internal/auth/store.go` (or a new file `internal/auth/verification.go`)
- Modify: `internal/auth/token_test.go` (new test)

- [ ] **Step 1: Write failing test**

Append to `internal/auth/token_test.go` (create the file if it doesn't exist):

```go
func TestGetTrustedKeyByKID_ReturnsAcrossTenants(t *testing.T) {
	s := auth.NewInMemoryTrustedKeyStore()
	tenantA := spi.TenantID("tenant-a")
	tk := &auth.TrustedKey{KID: "k1", TenantID: tenantA, PublicKey: testRSAPubKey(t), Audience: "human", Active: true, ValidFrom: time.Now()}
	_ = s.Register(tk, auth.RotateOptions{})
	got, err := auth.GetTrustedKeyByKIDForTesting(s, "k1")
	if err != nil || got.KID != "k1" {
		t.Fatalf("unexpected: got=%+v err=%v", got, err)
	}
}

func TestGetTrustedKeyByKID_LazyValidTo(t *testing.T) {
	s := auth.NewInMemoryTrustedKeyStore()
	past := time.Now().Add(-1 * time.Hour)
	tk := &auth.TrustedKey{KID: "expired", TenantID: "t", PublicKey: testRSAPubKey(t), Audience: "human", Active: false, ValidFrom: past, ValidTo: &past}
	_ = s.Register(tk, auth.RotateOptions{})
	if _, err := auth.GetTrustedKeyByKIDForTesting(s, "expired"); err == nil {
		t.Error("expected past-ValidTo key to be filtered out")
	}
}
```

The `ForTesting` suffix is an idiom for exposing unexported helpers via a small test-only wrapper.

- [ ] **Step 2: Run test (expected FAIL)**

Run: `go test ./internal/auth/ -run TestGetTrustedKeyByKID -v`
Expected: FAIL — `GetTrustedKeyByKIDForTesting` undefined.

- [ ] **Step 3: Implement the helper**

Append to `internal/auth/store.go` (or new file `internal/auth/verification.go`):

```go
// getTrustedKeyByKID resolves a trusted key by kid without tenant scoping.
//
// CALLER CONTRACT: this helper is for the token-exchange / JWT-bearer
// assertion verification path only (internal/auth/token.go). It iterates
// ListForVerification() so the lazy ValidTo filter is applied; past-ValidTo
// entries are excluded. The caller is responsible for downstream tenant
// invariant checks (client.TenantID == subOrgID at token.go:203).
//
// Unexported on purpose — same package as both the store and token.go.
func getTrustedKeyByKID(store TrustedKeyStore, kid string) (*TrustedKey, error) {
	for _, tk := range store.ListForVerification() {
		if tk.KID == kid {
			return tk, nil
		}
	}
	return nil, fmt.Errorf("trusted key not found")
}

// GetTrustedKeyByKIDForTesting exposes getTrustedKeyByKID for cross-package
// tests in internal/auth_test. Do NOT use from production code paths.
func GetTrustedKeyByKIDForTesting(store TrustedKeyStore, kid string) (*TrustedKey, error) {
	return getTrustedKeyByKID(store, kid)
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/auth/ -run TestGetTrustedKeyByKID -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/auth/store.go internal/auth/token_test.go
git commit -m "feat(auth): in-package getTrustedKeyByKID verification helper

Resolves a trusted key by kid without tenant scoping, for the
token-exchange verification path only. Iterates ListForVerification
so lazy ValidTo filter applies. Documented bypass; caller responsible
for downstream tenant invariant checks (existing token.go:203 line).

Refs #281."
```

---

## Phase 3: OpenAPI surgery + codegen regen

### Task 9: OpenAPI spec edits — schema fixes, prose, ReactivateKeyRequestDto, ProblemDetail

**Files:**
- Modify: `api/openapi.yaml`

This is one large task because the edits are all in one file and only meaningful together.

- [ ] **Step 1: Add `ReactivateKeyRequestDto` schema**

Edit `api/openapi.yaml` — under `components.schemas`, add (alphabetical placement near `InvalidateKeyRequestDto`):

```yaml
ReactivateKeyRequestDto:
  type: object
  properties:
    validFrom:
      type: string
      format: date-time
      description: Optional; defaults to now if absent.
    validTo:
      type: string
      format: date-time
      description: Required. Must be > now and > validFrom.
  required:
    - validTo
```

- [ ] **Step 2: Add request body + 400 response to both reactivate operations**

For `reactivateJwtKeyPair` (around line 4280–4310) and `reactivateTrustedKey` (around line 4591–4621), add:

```yaml
requestBody:
  content:
    application/json:
      schema:
        $ref: "#/components/schemas/ReactivateKeyRequestDto"
  required: true
```

Add a `"400"` response entry:

```yaml
"400":
  description: Error response
  content:
    application/problem+json:
      schema:
        $ref: "#/components/schemas/ProblemDetail"
```

- [ ] **Step 3: Fix the JWK schema (drop `additionalProperties: { type: object }`)**

In `api/openapi.yaml`:
- Around line 8413-8416 (`TrustedKeyResponseDto.jwk`): change `additionalProperties: { type: object }` to `additionalProperties: true`.
- Around line 8457-8460 (`RegisterTrustedKeyRequestDto.jwk`): same change.

- [ ] **Step 4: Remove 501 declarations from all 10 operations**

For each of the 10 operations (`issueJwtKeyPair`, `getCurrentJwtKeyPair`, `deleteJwtKeyPair`, `invalidateJwtKeyPair`, `reactivateJwtKeyPair`, `listTrustedKeys`, `registerTrustedKey`, `deleteTrustedKey`, `invalidateTrustedKey`, `reactivateTrustedKey`), DELETE the `"501": $ref: "#/components/responses/NotImplemented"` block.

- [ ] **Step 5: Switch every 4xx/5xx ref from `ErrorResponseDto` to `ProblemDetail`**

For each of the 10 operations, every response with status 400/401/403/404/409/500 that currently references `ErrorResponseDto`, change to:

```yaml
"NNN":
  description: Error response
  content:
    application/problem+json:
      schema:
        $ref: "#/components/schemas/ProblemDetail"
```

- [ ] **Step 6: Replace SUPER_USER with ROLE_ADMIN in the 10 operation descriptions**

Search for `SUPER_USER` in the descriptions of the 10 operations and replace with `ROLE_ADMIN`. Verify with:

```
grep -n "SUPER_USER" api/openapi.yaml
```

Expected: 0 hits inside the 10 operation descriptions (other operations may still reference SUPER_USER; leave those alone).

- [ ] **Step 7: Add default-behaviour prose**

Add brief prose to descriptions of:
- `issueJwtKeyPair`: append "If `algorithm` is omitted, defaults to RS256. If `validTo` is omitted, defaults to `validFrom + 365 days`."
- `registerTrustedKey`: append "If `validTo` is omitted, defaults to `validFrom + 365 days`. If `validFrom` is omitted, defaults to now."
- `invalidateJwtKeyPair` and `invalidateTrustedKey`: append "If `gracePeriodSec` is omitted or zero, invalidation is immediate."

- [ ] **Step 8: Verify YAML is well-formed**

Run: `go run sigs.k8s.io/yaml/cmd/yamllint ./api/openapi.yaml 2>/dev/null || python3 -c "import yaml; yaml.safe_load(open('api/openapi.yaml'))"`
Expected: no parse error.

- [ ] **Step 9: Commit**

```bash
git add api/openapi.yaml
git commit -m "feat(openapi): conform /oauth/keys/* operations + JWK schema fix

- Add ReactivateKeyRequestDto schema; attach as required body on both
  reactivate operations; add 400 response decls.
- Fix JWK schema: additionalProperties: true (was { type: object })
  so generated Go type becomes map[string]any (was nested map).
- Remove 501 declarations from the 10 /oauth/keys/* operations.
- Switch every 4xx/5xx from ErrorResponseDto to ProblemDetail with
  application/problem+json content-type (matches wire reality per
  audit pattern).
- Replace SUPER_USER -> ROLE_ADMIN in the 10 operation descriptions.
- Add default-behaviour prose for algorithm, validTo, gracePeriodSec.

Refs #281."
```

---

### Task 10: Regenerate `api/generated.go`

**Files:**
- Modify: `api/generated.go` (regenerated)

- [ ] **Step 1: Run codegen**

Run: `go generate ./api/...`
Expected: regenerates `api/generated.go` with the new ReactivateKeyRequestDto type, new request-body params on reactivate methods, and `Jwk map[string]interface{}` (was `map[string]map[string]interface{}`).

- [ ] **Step 2: Verify the regen output**

Run:
```
grep -n "ReactivateKeyRequestDto\|Jwk\s\+map\|ReactivateJwtKeyPairJSONRequestBody" api/generated.go | head -20
```

Expected output includes:
- A `type ReactivateKeyRequestDto struct {...}`.
- `Jwk map[string]interface{}` (single map, not nested).
- New `ReactivateJwtKeyPairJSONRequestBody` and `ReactivateTrustedKeyJSONRequestBody` types.
- Updated `ServerInterface.ReactivateJwtKeyPair` and `ReactivateTrustedKey` signatures taking a JSON-decoded payload.

- [ ] **Step 3: Build to confirm chain compiles**

Run: `go build ./...`
Expected: build failures in `internal/domain/account/handler.go` because the stub methods don't match the new ServerInterface signatures. This is expected — Task 11 onward fixes it.

- [ ] **Step 4: Commit**

```bash
git add api/generated.go
git commit -m "chore(api): regenerate generated.go after openapi.yaml edits

Picks up ReactivateKeyRequestDto, new reactivate request-body params,
and Jwk type fix (map[string]interface{} instead of nested map).

Refs #281."
```


---

## Phase 4: Handler infrastructure

### Task 11: Export RequireAdmin + update in-package callsites

**Files:**
- Modify: `internal/auth/admin_guard.go`
- Modify: `internal/auth/m2m.go` (callsite)
- Modify: `internal/auth/keys.go` (callsite — file is about to be deleted in Task 25, but keep it building meanwhile)

- [ ] **Step 1: Rename `requireAdmin` → `RequireAdmin` in admin_guard.go**

Edit `internal/auth/admin_guard.go` line 22: change `func requireAdmin(...)` to `func RequireAdmin(...)`. Update the function's doc-comment to drop the lowercase form.

- [ ] **Step 2: Update in-package callsites**

```
grep -rln "requireAdmin(" internal/auth/
```

For every match, replace `requireAdmin(` with `RequireAdmin(`.

- [ ] **Step 3: Verify build**

Run: `go build ./internal/auth/...`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/auth/admin_guard.go internal/auth/m2m.go internal/auth/keys.go
git commit -m "refactor(auth): export RequireAdmin for cross-package use

Required by the new /oauth/keys/* adapters in internal/domain/account/.
No behaviour change; rename + in-package callsite updates.

Refs #281."
```

---

### Task 12: Add `TrustedKeyStore()` accessor on AuthService

**Files:**
- Modify: `internal/auth/service.go`

- [ ] **Step 1: Add the accessor**

Edit `internal/auth/service.go` — near the existing `KeyStore()` method (around line 122), add:

```go
// TrustedKeyStore returns the configured trusted-key store. Used by the
// /oauth/keys/trusted/* adapters in internal/domain/account/.
func (s *AuthService) TrustedKeyStore() TrustedKeyStore {
	return s.trustedStore
}
```

- [ ] **Step 2: Verify build**

Run: `go build ./internal/auth/...`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/auth/service.go
git commit -m "feat(auth): expose AuthService.TrustedKeyStore() accessor

Mirror of KeyStore(); consumed by the /oauth/keys/trusted/* adapters.

Refs #281."
```

---

### Task 13: `boundedJSONDecode` helper (TDD)

**Files:**
- Create: `internal/domain/account/io.go`
- Create: `internal/domain/account/io_test.go`

- [ ] **Step 1: Write failing tests**

Write `internal/domain/account/io_test.go`:

```go
package account_test

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/domain/account"
)

func TestBoundedJSONDecode_Happy(t *testing.T) {
	type dst struct{ X int `json:"x"` }
	r := httptest.NewRequest("POST", "/", bytes.NewReader([]byte(`{"x":7}`)))
	w := httptest.NewRecorder()
	var d dst
	if err := account.BoundedJSONDecodeForTesting(w, r, 1<<10, &d); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if d.X != 7 {
		t.Errorf("x=%d want 7", d.X)
	}
}

func TestBoundedJSONDecode_OverSize(t *testing.T) {
	type dst struct{ X string `json:"x"` }
	big := strings.Repeat("a", 1<<20+1)
	r := httptest.NewRequest("POST", "/", bytes.NewReader([]byte(`{"x":"`+big+`"}`)))
	w := httptest.NewRecorder()
	var d dst
	if err := account.BoundedJSONDecodeForTesting(w, r, 1<<20, &d); err == nil {
		t.Fatal("expected error on oversized body")
	}
}

func TestBoundedJSONDecode_BadJSON(t *testing.T) {
	type dst struct{}
	r := httptest.NewRequest("POST", "/", bytes.NewReader([]byte(`not-json`)))
	w := httptest.NewRecorder()
	var d dst
	if err := account.BoundedJSONDecodeForTesting(w, r, 1<<10, &d); err == nil {
		t.Fatal("expected error on bad json")
	}
}
```

- [ ] **Step 2: Run tests (expected FAIL)**

Run: `go test ./internal/domain/account/ -run TestBoundedJSONDecode -v`
Expected: FAIL — undefined.

- [ ] **Step 3: Write the helper**

Write `internal/domain/account/io.go`:

```go
package account

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// boundedJSONDecode wraps http.MaxBytesReader + json.Decoder.Decode. It
// returns a non-nil error when the body exceeds max bytes or fails to
// parse as JSON. Callers translate the error to 400 BAD_REQUEST via
// common.WriteError at the adapter boundary.
//
// All 4 POST /oauth/keys/* adapters use this helper with max=1<<20 (1 MB).
func boundedJSONDecode(w http.ResponseWriter, r *http.Request, max int64, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, max)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("decode body: %w", err)
	}
	return nil
}

// BoundedJSONDecodeForTesting exposes boundedJSONDecode for external tests.
func BoundedJSONDecodeForTesting(w http.ResponseWriter, r *http.Request, max int64, dst any) error {
	return boundedJSONDecode(w, r, max, dst)
}
```

- [ ] **Step 4: Run tests (expected PASS)**

Run: `go test ./internal/domain/account/ -run TestBoundedJSONDecode -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/domain/account/io.go internal/domain/account/io_test.go
git commit -m "feat(account): boundedJSONDecode helper for /oauth/keys/* POSTs

Shared helper wrapping http.MaxBytesReader + json.Decoder.Decode.
Returns error for oversize or malformed JSON; adapters translate
to 400 BAD_REQUEST.

Refs #281."
```

---

### Task 14: Handler struct fields + `account.New` signature update

**Files:**
- Modify: `internal/domain/account/handler.go`
- Modify: `app/app.go` (the `account.New(...)` callsite)
- Modify: `internal/domain/account/handler_test.go` (callsite update)

- [ ] **Step 1: Update Handler struct + New() signature**

Replace `internal/domain/account/handler.go` lines 14-21 with:

```go
type Handler struct {
	authSvc         contract.AuthenticationService
	authzSvc        contract.AuthorizationService
	keyStore        auth.KeyStore
	trustedKeyStore auth.TrustedKeyStore
	iam             auth.IAMConfig
}

func New(authSvc contract.AuthenticationService, authzSvc contract.AuthorizationService,
	keyStore auth.KeyStore, trustedKeyStore auth.TrustedKeyStore, iam auth.IAMConfig) *Handler {
	return &Handler{
		authSvc:         authSvc,
		authzSvc:        authzSvc,
		keyStore:        keyStore,
		trustedKeyStore: trustedKeyStore,
		iam:             iam,
	}
}
```

Add the import:

```go
"github.com/cyoda-platform/cyoda-go/internal/auth"
```

- [ ] **Step 2: Update test callsite**

Edit `internal/domain/account/handler_test.go` — the test `TestNewHandler` and `TestAccountGet` use `account.New(nil, nil)`. Update to `account.New(nil, nil, nil, nil, auth.IAMConfig{})`. (For tests that don't exercise the new dependencies, nil interfaces + zero IAMConfig is fine.)

- [ ] **Step 3: Update app.go callsite**

Search:
```
grep -n "account.New(" app/app.go
```

Update the call to pass `authSvc.KeyStore()`, `authSvc.TrustedKeyStore()`, and `cfg.Auth.IAM` (the IAMConfig from config; if `cfg.Auth.IAM` doesn't exist yet, Task 16 wires it — for now, pass `auth.IAMConfig{}` and TODO-mark the line):

```go
accountHandler := account.New(authSvc, authzSvc, authSvc.KeyStore(), authSvc.TrustedKeyStore(), cfg.Auth.IAM)
```

If `cfg.Auth.IAM` doesn't yet exist, temporarily pass `auth.DefaultIAMConfig()` so the build stays green; Task 16 wires real config.

- [ ] **Step 4: Verify build + tests**

Run: `go build ./...`
Expected: PASS.

Run: `go test ./internal/domain/account/ -short -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/domain/account/handler.go internal/domain/account/handler_test.go app/app.go
git commit -m "feat(account): Handler struct gains keyStore/trustedKeyStore/iam

account.New() signature accepts the three new dependencies. app.go
callsite updated to wire them from AuthService.KeyStore(),
AuthService.TrustedKeyStore(), and DefaultIAMConfig() (real config
wiring follows in app/config.go change).

Refs #281."
```

---

### Task 15: Wire `IAMConfig` env vars in `app/config.go`

**Files:**
- Modify: `app/config.go`
- Modify: `app/app.go` (use `cfg.Auth.IAM` once wired)
- Create: `internal/auth/config_validation_test.go`

- [ ] **Step 1: Inspect current config wiring**

Run:
```
grep -n "AuthConfig\|TrustedKeyStore\|getEnv\|envOrDefault" app/config.go | head -20
```

Identify the project's env-var-parsing helpers (e.g. `getenvOr`, `parseIntEnv`). Use them consistently.

- [ ] **Step 2: Add IAM env-var parsing to DefaultConfig/Load**

Add to `app/config.go` inside the existing config-load function (the one that populates `cfg.Auth`):

```go
iam := auth.DefaultIAMConfig()
iam.TrustedKeyRegistrationEnabled = getBoolEnv("CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED", iam.TrustedKeyRegistrationEnabled)
iam.TrustedKeyMaxPerTenant = getIntEnv("CYODA_IAM_TRUSTED_KEY_MAX_PER_TENANT", iam.TrustedKeyMaxPerTenant)
iam.TrustedKeyMaxValidityDays = getIntEnv("CYODA_IAM_TRUSTED_KEY_MAX_VALIDITY_DAYS", iam.TrustedKeyMaxValidityDays)
iam.TrustedKeyMaxJWKProperties = getIntEnv("CYODA_IAM_TRUSTED_KEY_MAX_JWK_PROPERTIES", iam.TrustedKeyMaxJWKProperties)
iam.KeypairDefaultValidityDays = getIntEnv("CYODA_IAM_KEYPAIR_DEFAULT_VALIDITY_DAYS", iam.KeypairDefaultValidityDays)
iam.BootstrapAudience = getStringEnv("CYODA_JWT_BOOTSTRAP_AUDIENCE", iam.BootstrapAudience)
if err := iam.Validate(); err != nil {
	return cfg, fmt.Errorf("invalid IAM config: %w", err)
}
cfg.Auth.IAM = iam
```

Replace `getBoolEnv`/`getIntEnv`/`getStringEnv` with whatever helpers the project already uses.

- [ ] **Step 3: Write validation tests**

Write `internal/auth/config_validation_test.go`:

```go
package auth_test

import (
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/auth"
)

func TestIAMConfig_Validate(t *testing.T) {
	good := auth.DefaultIAMConfig()
	if err := good.Validate(); err != nil {
		t.Errorf("default should be valid: %v", err)
	}

	cases := []struct {
		name string
		mut  func(*auth.IAMConfig)
	}{
		{"BootstrapAudience invalid", func(c *auth.IAMConfig) { c.BootstrapAudience = "robot" }},
		{"BootstrapAudience empty", func(c *auth.IAMConfig) { c.BootstrapAudience = "" }},
		{"TrustedKeyMaxPerTenant negative", func(c *auth.IAMConfig) { c.TrustedKeyMaxPerTenant = -1 }},
		{"TrustedKeyMaxValidityDays zero", func(c *auth.IAMConfig) { c.TrustedKeyMaxValidityDays = 0 }},
		{"TrustedKeyMaxJWKProperties zero", func(c *auth.IAMConfig) { c.TrustedKeyMaxJWKProperties = 0 }},
		{"KeypairDefaultValidityDays zero", func(c *auth.IAMConfig) { c.KeypairDefaultValidityDays = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := auth.DefaultIAMConfig()
			tc.mut(&c)
			if err := c.Validate(); err == nil {
				t.Errorf("expected error for %s", tc.name)
			}
		})
	}

	// TrustedKeyMaxPerTenant=0 is the explicit "unbounded" case; allowed.
	c := auth.DefaultIAMConfig()
	c.TrustedKeyMaxPerTenant = 0
	if err := c.Validate(); err != nil {
		t.Errorf("MaxPerTenant=0 should be valid (unbounded), got %v", err)
	}
}
```

- [ ] **Step 4: Run tests + full build**

Run: `go test ./internal/auth/ -run TestIAMConfig -v`
Expected: PASS.

Run: `go build ./...`
Expected: PASS.

- [ ] **Step 5: Update app.go to use `cfg.Auth.IAM`**

Replace the temporary `auth.DefaultIAMConfig()` from Task 14 with `cfg.Auth.IAM` in the `account.New(...)` callsite.

Also ensure the AuthService construction passes through the IAM config — `auth.NewAuthService(cfg.Auth)` carries it via the embedded `IAM` field already.

- [ ] **Step 6: Commit**

```bash
git add app/config.go internal/auth/config_validation_test.go app/app.go
git commit -m "feat(config): wire IAMConfig env vars + boot-time validation

Six env vars (TRUSTED_KEY_REGISTRATION_ENABLED, MAX_PER_TENANT,
MAX_VALIDITY_DAYS, MAX_JWK_PROPERTIES, KEYPAIR_DEFAULT_VALIDITY_DAYS,
BOOTSTRAP_AUDIENCE). IAMConfig.Validate() runs at boot; invalid
values fail startup. Plumbed through AuthConfig.IAM into the account
Handler.

Refs #281."
```

---

## Phase 5: Keypair adapters (5 ops, TDD)

Each of the 5 keypair adapter tasks follows the same TDD shape: write failing test → implement → green → commit. Adapters import the generated types from `genapi "github.com/cyoda-platform/cyoda-go/api"`.

**Shared adapter scaffolding** (Task 16 sets it up; later tasks build on it).

### Task 16: `keys_adapter.go` skeleton + `IssueJwtKeyPair` adapter

**Files:**
- Create: `internal/domain/account/keys_adapter.go`
- Create: `internal/domain/account/keys_adapter_test.go`
- Modify: `internal/domain/account/handler.go` (remove `IssueJwtKeyPair` stub)

- [ ] **Step 1: Write failing test**

Write `internal/domain/account/keys_adapter_test.go`:

```go
package account_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/cyoda-platform/cyoda-go/internal/domain/account"
)

func adminCtx() *spi.UserContext {
	return &spi.UserContext{
		UserID:   "u",
		UserName: "u",
		Tenant:   spi.Tenant{ID: "t1", Name: "t1"},
		Roles:    []string{"ROLE_ADMIN"},
	}
}

func TestIssueJwtKeyPair_Happy(t *testing.T) {
	keyStore := auth.NewInMemoryKeyStore()
	trustedStore := auth.NewInMemoryTrustedKeyStore()
	iam := auth.DefaultIAMConfig()
	h := account.New(nil, nil, keyStore, trustedStore, iam)

	body, _ := json.Marshal(genapi.IssueJwtKeyPairRequestDto{
		Algorithm: "RS256",
		Audience:  "client",
	})
	req := httptest.NewRequest("POST", "/oauth/keys/keypair", bytes.NewReader(body))
	req = req.WithContext(spi.WithUserContext(req.Context(), adminCtx()))
	w := httptest.NewRecorder()

	h.IssueJwtKeyPair(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var resp genapi.JwtKeyPairResponseDto
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode resp: %v", err)
	}
	if resp.Algorithm != "RS256" {
		t.Errorf("algorithm=%s want RS256", resp.Algorithm)
	}
	if resp.KeyId == "" {
		t.Error("keyId empty")
	}
	// publicKey must be base64-DER, not PEM.
	if _, err := base64.StdEncoding.DecodeString(resp.PublicKey); err != nil {
		t.Errorf("publicKey not base64: %v", err)
	}
	if !time.Time(resp.ValidFrom).Before(time.Now().Add(2 * time.Second)) {
		t.Error("validFrom not set or in future")
	}
}

func TestIssueJwtKeyPair_RejectsNonRS256(t *testing.T) {
	keyStore := auth.NewInMemoryKeyStore()
	h := account.New(nil, nil, keyStore, auth.NewInMemoryTrustedKeyStore(), auth.DefaultIAMConfig())
	body, _ := json.Marshal(genapi.IssueJwtKeyPairRequestDto{
		Algorithm: "ES256",
		Audience:  "client",
	})
	req := httptest.NewRequest("POST", "/oauth/keys/keypair", bytes.NewReader(body))
	req = req.WithContext(spi.WithUserContext(req.Context(), adminCtx()))
	w := httptest.NewRecorder()
	h.IssueJwtKeyPair(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	// Body should be ProblemDetail with errorCode UNSUPPORTED_ALGORITHM.
	var pd map[string]any
	json.Unmarshal(w.Body.Bytes(), &pd)
	if pd["errorCode"] != "UNSUPPORTED_ALGORITHM" {
		t.Errorf("errorCode=%v", pd["errorCode"])
	}
}

func TestIssueJwtKeyPair_RejectsBadAudience(t *testing.T) {
	keyStore := auth.NewInMemoryKeyStore()
	h := account.New(nil, nil, keyStore, auth.NewInMemoryTrustedKeyStore(), auth.DefaultIAMConfig())
	// Bypass the typed enum: send raw JSON with audience: "robot".
	body := []byte(`{"algorithm":"RS256","audience":"robot"}`)
	req := httptest.NewRequest("POST", "/oauth/keys/keypair", bytes.NewReader(body))
	req = req.WithContext(spi.WithUserContext(req.Context(), adminCtx()))
	w := httptest.NewRecorder()
	h.IssueJwtKeyPair(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}
```

- [ ] **Step 2: Run test (expected FAIL)**

Run: `go test ./internal/domain/account/ -run TestIssueJwtKeyPair -v`
Expected: FAIL — compile error from stub still in handler.go (mismatched signature) and `IssueJwtKeyPair` adapter not yet implemented.

- [ ] **Step 3: Remove the stub method from handler.go**

Edit `internal/domain/account/handler.go` — delete the `IssueJwtKeyPair` method (lines around 85-87).

- [ ] **Step 4: Write the adapter implementation**

Write `internal/domain/account/keys_adapter.go`:

```go
package account

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"
	spi "github.com/cyoda-platform/cyoda-go-spi"

	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// --- IssueJwtKeyPair ---

func (h *Handler) IssueJwtKeyPair(w http.ResponseWriter, r *http.Request) {
	if !auth.RequireAdmin(w, r) {
		return
	}

	var req genapi.IssueJwtKeyPairRequestDto
	if err := boundedJSONDecode(w, r, 1<<20, &req); err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid request body"))
		return
	}

	// Algorithm: RS256 only in v0.8.0.
	if string(req.Algorithm) != "RS256" {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeUnsupportedAlgorithm, "only RS256 supported in this version"))
		return
	}

	// Audience: validated via the generated enum.
	if !isValidKeyPairAudience(string(req.Audience)) {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid audience"))
		return
	}

	// validFrom default = now; validTo default = validFrom + KeypairDefaultValidityDays.
	now := time.Now().UTC()
	validFrom := now
	if req.ValidFrom != nil {
		validFrom = time.Time(*req.ValidFrom)
	}
	validTo := validFrom.Add(time.Duration(h.iam.KeypairDefaultValidityDays) * 24 * time.Hour)
	if req.ValidTo != nil {
		validTo = time.Time(*req.ValidTo)
	}
	if !validTo.After(validFrom) {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "validTo must be > validFrom"))
		return
	}

	// gracePeriodSec validation (only meaningful when invalidateCurrent=true).
	var grace int64
	if req.InvalidateGracePeriodSec != nil {
		grace = *req.InvalidateGracePeriodSec
		if grace < 0 {
			common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "gracePeriodSec must be >= 0"))
			return
		}
	}
	invalidate := false
	if req.InvalidateCurrent != nil {
		invalidate = *req.InvalidateCurrent
	}

	// Generate RSA keypair (RS256).
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		common.WriteError(w, r, common.Internal("rsa.GenerateKey", err))
		return
	}
	kidBytes := make([]byte, 16)
	if _, err := rand.Read(kidBytes); err != nil {
		common.WriteError(w, r, common.Internal("rand.Read", err))
		return
	}
	kid := hex.EncodeToString(kidBytes)

	vt := validTo
	kp := &auth.KeyPair{
		KID:        kid,
		Audience:   string(req.Audience),
		Algorithm:  "RS256",
		PublicKey:  &privateKey.PublicKey,
		PrivateKey: privateKey,
		Active:     true,
		ValidFrom:  validFrom,
		ValidTo:    &vt,
	}
	if err := h.keyStore.Save(kp, auth.RotateOptions{Invalidate: invalidate, GracePeriodSec: grace}); err != nil {
		common.WriteError(w, r, common.Internal("keyStore.Save", err))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(toJwtKeyPairResponse(kp))
}

// --- helpers ---

func isValidKeyPairAudience(s string) bool {
	return s == "human" || s == "client"
}

func toJwtKeyPairResponse(kp *auth.KeyPair) genapi.JwtKeyPairResponseDto {
	derBytes, err := x509.MarshalPKIXPublicKey(kp.PublicKey)
	pubKey := ""
	if err == nil {
		pubKey = base64.StdEncoding.EncodeToString(derBytes)
	}
	resp := genapi.JwtKeyPairResponseDto{
		KeyId:     kp.KID,
		Algorithm: genapi.JwtKeyPairResponseDtoAlgorithm(kp.Algorithm),
		PublicKey: pubKey,
		ValidFrom: openapi_types.Date{Time: kp.ValidFrom},
	}
	if kp.ValidTo != nil {
		vt := openapi_types.Date{Time: *kp.ValidTo}
		resp.ValidTo = &vt
	}
	return resp
}

// tenantFromCtx returns the caller's tenant from the request context.
// Falls back to "" — callers should treat empty as a programming error
// because RequireAdmin upstream guarantees UserContext present.
func tenantFromCtx(r *http.Request) spi.TenantID {
	uc := spi.GetUserContext(r.Context())
	if uc == nil {
		return ""
	}
	return uc.Tenant.ID
}

// silence "imported and not used" for errors when the package compiles.
var _ = errors.New
```

(Adjust `openapi_types.Date` usage if the generated DTOs use a different timestamp type. If `ValidFrom` is typed `time.Time` directly on the generated DTO, just assign `kp.ValidFrom`.)

- [ ] **Step 5: Run tests**

Run: `go test ./internal/domain/account/ -run TestIssueJwtKeyPair -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/domain/account/keys_adapter.go internal/domain/account/keys_adapter_test.go internal/domain/account/handler.go
git commit -m "feat(account): IssueJwtKeyPair adapter

RS256-only signing (v0.8.0); other algorithms rejected with 400
UNSUPPORTED_ALGORITHM. Audience enum validated. validFrom defaults
to now; validTo defaults to validFrom + KeypairDefaultValidityDays.
gracePeriodSec rejected if negative. publicKey returned as base64-DER.
RotateOptions{Invalidate, GracePeriodSec} flows into KeyStore.Save.

Refs #281."
```

---

### Task 17: `GetCurrentJwtKeyPair` adapter

**Files:**
- Modify: `internal/domain/account/keys_adapter.go`
- Modify: `internal/domain/account/keys_adapter_test.go`
- Modify: `internal/domain/account/handler.go` (remove stub)

- [ ] **Step 1: Write failing test**

Append to `internal/domain/account/keys_adapter_test.go`:

```go
func TestGetCurrentJwtKeyPair_Happy(t *testing.T) {
	keyStore := auth.NewInMemoryKeyStore()
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	kp := &auth.KeyPair{KID: "k1", Audience: "client", Algorithm: "RS256", PublicKey: &priv.PublicKey, PrivateKey: priv, Active: true, ValidFrom: time.Now()}
	_ = keyStore.Save(kp, auth.RotateOptions{})
	h := account.New(nil, nil, keyStore, auth.NewInMemoryTrustedKeyStore(), auth.DefaultIAMConfig())

	req := httptest.NewRequest("GET", "/oauth/keys/keypair/current?audience=client", nil)
	req = req.WithContext(spi.WithUserContext(req.Context(), adminCtx()))
	w := httptest.NewRecorder()
	params := genapi.GetCurrentJwtKeyPairParams{Audience: "client"}
	h.GetCurrentJwtKeyPair(w, req, params)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetCurrentJwtKeyPair_404_WhenNoKeyForAudience(t *testing.T) {
	keyStore := auth.NewInMemoryKeyStore()
	h := account.New(nil, nil, keyStore, auth.NewInMemoryTrustedKeyStore(), auth.DefaultIAMConfig())
	req := httptest.NewRequest("GET", "/oauth/keys/keypair/current?audience=human", nil)
	req = req.WithContext(spi.WithUserContext(req.Context(), adminCtx()))
	w := httptest.NewRecorder()
	params := genapi.GetCurrentJwtKeyPairParams{Audience: "human"}
	h.GetCurrentJwtKeyPair(w, req, params)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
	var pd map[string]any
	json.Unmarshal(w.Body.Bytes(), &pd)
	if pd["errorCode"] != "KEYPAIR_NOT_FOUND" {
		t.Errorf("errorCode=%v", pd["errorCode"])
	}
}
```

Add imports: `"crypto/rand"`, `"crypto/rsa"`.

- [ ] **Step 2: Run (expected FAIL — stub still present)**

Run: `go test ./internal/domain/account/ -run TestGetCurrentJwtKeyPair -v`
Expected: FAIL.

- [ ] **Step 3: Remove stub + write adapter**

Delete the `GetCurrentJwtKeyPair` stub in `handler.go`. Append to `keys_adapter.go`:

```go
func (h *Handler) GetCurrentJwtKeyPair(w http.ResponseWriter, r *http.Request, params genapi.GetCurrentJwtKeyPairParams) {
	if !auth.RequireAdmin(w, r) {
		return
	}
	if !isValidKeyPairAudience(string(params.Audience)) {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid audience"))
		return
	}
	kp, err := h.keyStore.GetActive(string(params.Audience))
	if err != nil {
		common.WriteError(w, r, common.Operational(http.StatusNotFound, common.ErrCodeKeypairNotFound, "no active key pair for audience"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(toJwtKeyPairResponse(kp))
}
```

- [ ] **Step 4: Run + Commit**

Run: `go test ./internal/domain/account/ -run TestGetCurrentJwtKeyPair -v`
Expected: PASS.

```bash
git add internal/domain/account/keys_adapter.go internal/domain/account/keys_adapter_test.go internal/domain/account/handler.go
git commit -m "feat(account): GetCurrentJwtKeyPair adapter

Selects via KeyStore.GetActive(audience); 404 KEYPAIR_NOT_FOUND when
no active key for audience. Refs #281."
```

---

### Task 18: `DeleteJwtKeyPair`, `InvalidateJwtKeyPair`, `ReactivateJwtKeyPair` adapters

These three lifecycle ops are small enough to land in one task.

**Files:**
- Modify: `internal/domain/account/keys_adapter.go`
- Modify: `internal/domain/account/keys_adapter_test.go`
- Modify: `internal/domain/account/handler.go` (remove 3 stubs)

- [ ] **Step 1: Write failing tests**

Append to test file:

```go
func TestDeleteJwtKeyPair(t *testing.T) {
	ks := auth.NewInMemoryKeyStore()
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	kp := &auth.KeyPair{KID: "k", Audience: "client", Algorithm: "RS256", PublicKey: &priv.PublicKey, PrivateKey: priv, Active: true, ValidFrom: time.Now()}
	_ = ks.Save(kp, auth.RotateOptions{})
	h := account.New(nil, nil, ks, auth.NewInMemoryTrustedKeyStore(), auth.DefaultIAMConfig())
	req := httptest.NewRequest("DELETE", "/", nil)
	req = req.WithContext(spi.WithUserContext(req.Context(), adminCtx()))
	w := httptest.NewRecorder()
	h.DeleteJwtKeyPair(w, req, "k")
	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", w.Code)
	}
	if _, err := ks.Get("k"); err == nil {
		t.Error("expected key deleted")
	}
}

func TestDeleteJwtKeyPair_404(t *testing.T) {
	h := account.New(nil, nil, auth.NewInMemoryKeyStore(), auth.NewInMemoryTrustedKeyStore(), auth.DefaultIAMConfig())
	req := httptest.NewRequest("DELETE", "/", nil)
	req = req.WithContext(spi.WithUserContext(req.Context(), adminCtx()))
	w := httptest.NewRecorder()
	h.DeleteJwtKeyPair(w, req, "missing")
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestInvalidateJwtKeyPair_WithGracePeriod(t *testing.T) {
	ks := auth.NewInMemoryKeyStore()
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	kp := &auth.KeyPair{KID: "k", Audience: "client", Algorithm: "RS256", PublicKey: &priv.PublicKey, PrivateKey: priv, Active: true, ValidFrom: time.Now()}
	_ = ks.Save(kp, auth.RotateOptions{})
	h := account.New(nil, nil, ks, auth.NewInMemoryTrustedKeyStore(), auth.DefaultIAMConfig())
	body, _ := json.Marshal(genapi.InvalidateKeyRequestDto{GracePeriodSec: ptrInt64(60)})
	req := httptest.NewRequest("POST", "/", bytes.NewReader(body))
	req = req.WithContext(spi.WithUserContext(req.Context(), adminCtx()))
	w := httptest.NewRecorder()
	h.InvalidateJwtKeyPair(w, req, "k")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	got, _ := ks.Get("k")
	if got.Active {
		t.Error("expected Active=false")
	}
	if got.ValidTo == nil {
		t.Error("expected ValidTo set")
	}
}

func TestReactivateJwtKeyPair_RequiresValidTo(t *testing.T) {
	ks := auth.NewInMemoryKeyStore()
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	past := time.Now().Add(-1 * time.Hour)
	kp := &auth.KeyPair{KID: "k", Audience: "client", Algorithm: "RS256", PublicKey: &priv.PublicKey, PrivateKey: priv, Active: false, ValidFrom: past, ValidTo: &past}
	_ = ks.Save(kp, auth.RotateOptions{})
	h := account.New(nil, nil, ks, auth.NewInMemoryTrustedKeyStore(), auth.DefaultIAMConfig())
	// Missing validTo -> 400
	req := httptest.NewRequest("POST", "/", bytes.NewReader([]byte(`{}`)))
	req = req.WithContext(spi.WithUserContext(req.Context(), adminCtx()))
	w := httptest.NewRecorder()
	h.ReactivateJwtKeyPair(w, req, "k")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 (missing validTo), got %d", w.Code)
	}
	// Past validTo -> 400
	body, _ := json.Marshal(genapi.ReactivateKeyRequestDto{ValidTo: openapi_types.Date{Time: past}})
	req = httptest.NewRequest("POST", "/", bytes.NewReader(body))
	req = req.WithContext(spi.WithUserContext(req.Context(), adminCtx()))
	w = httptest.NewRecorder()
	h.ReactivateJwtKeyPair(w, req, "k")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 (past validTo), got %d", w.Code)
	}
	// Fresh validTo -> 200
	body, _ = json.Marshal(genapi.ReactivateKeyRequestDto{ValidTo: openapi_types.Date{Time: time.Now().Add(24 * time.Hour)}})
	req = httptest.NewRequest("POST", "/", bytes.NewReader(body))
	req = req.WithContext(spi.WithUserContext(req.Context(), adminCtx()))
	w = httptest.NewRecorder()
	h.ReactivateJwtKeyPair(w, req, "k")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func ptrInt64(v int64) *int64 { return &v }
```

- [ ] **Step 2: Run (expected FAIL)**

Run: `go test ./internal/domain/account/ -run "TestDeleteJwtKeyPair|TestInvalidateJwtKeyPair|TestReactivateJwtKeyPair" -v`
Expected: FAIL.

- [ ] **Step 3: Remove stubs + write adapters**

Delete the 3 stub methods in `handler.go`. Append to `keys_adapter.go`:

```go
func (h *Handler) DeleteJwtKeyPair(w http.ResponseWriter, r *http.Request, keyId string) {
	if !auth.RequireAdmin(w, r) {
		return
	}
	if err := h.keyStore.Delete(keyId); err != nil {
		common.WriteError(w, r, common.Operational(http.StatusNotFound, common.ErrCodeKeypairNotFound, "key pair not found"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) InvalidateJwtKeyPair(w http.ResponseWriter, r *http.Request, keyId string) {
	if !auth.RequireAdmin(w, r) {
		return
	}
	var grace int64
	// Body is optional; if present, parse it.
	if r.ContentLength != 0 {
		var req genapi.InvalidateKeyRequestDto
		if err := boundedJSONDecode(w, r, 1<<20, &req); err != nil {
			common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid request body"))
			return
		}
		if req.GracePeriodSec != nil {
			grace = *req.GracePeriodSec
			if grace < 0 {
				common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "gracePeriodSec must be >= 0"))
				return
			}
		}
	}
	if err := h.keyStore.Invalidate(keyId, grace); err != nil {
		common.WriteError(w, r, common.Operational(http.StatusNotFound, common.ErrCodeKeypairNotFound, "key pair not found"))
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) ReactivateJwtKeyPair(w http.ResponseWriter, r *http.Request, keyId string) {
	if !auth.RequireAdmin(w, r) {
		return
	}
	var req genapi.ReactivateKeyRequestDto
	if err := boundedJSONDecode(w, r, 1<<20, &req); err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid request body"))
		return
	}
	// validTo required and must be > now and > validFrom.
	validFrom := time.Now()
	if req.ValidFrom != nil {
		validFrom = time.Time(*req.ValidFrom)
	}
	validTo := time.Time(req.ValidTo)
	if !validTo.After(time.Now()) {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "validTo must be in the future"))
		return
	}
	if !validTo.After(validFrom) {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "validTo must be > validFrom"))
		return
	}
	if err := h.keyStore.Reactivate(keyId, validFrom, validTo); err != nil {
		common.WriteError(w, r, common.Operational(http.StatusNotFound, common.ErrCodeKeypairNotFound, "key pair not found"))
		return
	}
	w.WriteHeader(http.StatusOK)
}
```

- [ ] **Step 4: Run + commit**

Run: `go test ./internal/domain/account/ -v`
Expected: PASS.

```bash
git add internal/domain/account/keys_adapter.go internal/domain/account/keys_adapter_test.go internal/domain/account/handler.go
git commit -m "feat(account): Delete/Invalidate/Reactivate JwtKeyPair adapters

Delete/Invalidate return 404 KEYPAIR_NOT_FOUND on missing kid (no
path-param regex; matches cloud). Invalidate body optional;
gracePeriodSec defaults to 0; negative rejected. Reactivate requires
ReactivateKeyRequestDto body with validTo > now and > validFrom.

Refs #281."
```


---

## Phase 6: Trusted-key adapters (5 ops, TDD)

### Task 19: `trusted_adapter.go` skeleton + `RegisterTrustedKey` adapter

**Files:**
- Create: `internal/domain/account/trusted_adapter.go`
- Create: `internal/domain/account/trusted_adapter_test.go`
- Modify: `internal/domain/account/handler.go` (remove `RegisterTrustedKey` stub)

- [ ] **Step 1: Write failing tests**

Write `internal/domain/account/trusted_adapter_test.go`:

```go
package account_test

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"

	openapi_types "github.com/oapi-codegen/runtime/types"
	spi "github.com/cyoda-platform/cyoda-go-spi"
	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/cyoda-platform/cyoda-go/internal/domain/account"
)

func rsaJWK(t *testing.T, kid string) map[string]interface{} {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	n := base64.RawURLEncoding.EncodeToString(priv.PublicKey.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(priv.PublicKey.E)).Bytes())
	return map[string]interface{}{"kty": "RSA", "kid": kid, "n": n, "e": e}
}

func TestRegisterTrustedKey_Happy_FlagEnabled(t *testing.T) {
	ks := auth.NewInMemoryKeyStore()
	ts := auth.NewInMemoryTrustedKeyStore()
	iam := auth.DefaultIAMConfig()
	iam.TrustedKeyRegistrationEnabled = true
	h := account.New(nil, nil, ks, ts, iam)
	body, _ := json.Marshal(genapi.RegisterTrustedKeyRequestDto{
		KeyId:    "k1",
		Jwk:      rsaJWK(t, "k1"),
		Audience: "human",
	})
	req := httptest.NewRequest("POST", "/oauth/keys/trusted", bytes.NewReader(body))
	req = req.WithContext(spi.WithUserContext(req.Context(), adminCtx()))
	w := httptest.NewRecorder()
	h.RegisterTrustedKey(w, req)
	if w.Code != http.StatusCreated && w.Code != http.StatusOK {
		t.Fatalf("expected 201/200, got %d: %s", w.Code, w.Body.String())
	}
	var resp genapi.TrustedKeyResponseDto
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.KeyId != "k1" {
		t.Errorf("keyId=%s", resp.KeyId)
	}
	if resp.LegalEntityId != "t1" {
		t.Errorf("legalEntityId=%s want t1", resp.LegalEntityId)
	}
	if resp.Jwk["kty"] != "RSA" {
		t.Errorf("jwk.kty=%v", resp.Jwk["kty"])
	}
}

func TestRegisterTrustedKey_404_FlagDisabled(t *testing.T) {
	ts := auth.NewInMemoryTrustedKeyStore()
	iam := auth.DefaultIAMConfig() // flag default false
	h := account.New(nil, nil, auth.NewInMemoryKeyStore(), ts, iam)
	body, _ := json.Marshal(genapi.RegisterTrustedKeyRequestDto{KeyId: "k1", Jwk: rsaJWK(t, "k1"), Audience: "human"})
	req := httptest.NewRequest("POST", "/oauth/keys/trusted", bytes.NewReader(body))
	req = req.WithContext(spi.WithUserContext(req.Context(), adminCtx()))
	w := httptest.NewRecorder()
	h.RegisterTrustedKey(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
	var pd map[string]any
	json.Unmarshal(w.Body.Bytes(), &pd)
	if pd["errorCode"] != "FEATURE_DISABLED" {
		t.Errorf("errorCode=%v", pd["errorCode"])
	}
}

func TestRegisterTrustedKey_KidKeyIdMismatch(t *testing.T) {
	ts := auth.NewInMemoryTrustedKeyStore()
	iam := auth.DefaultIAMConfig()
	iam.TrustedKeyRegistrationEnabled = true
	h := account.New(nil, nil, auth.NewInMemoryKeyStore(), ts, iam)
	// JWK has kid "evil" but request keyId is "good".
	jwk := rsaJWK(t, "evil")
	body, _ := json.Marshal(genapi.RegisterTrustedKeyRequestDto{KeyId: "good", Jwk: jwk, Audience: "human"})
	req := httptest.NewRequest("POST", "/oauth/keys/trusted", bytes.NewReader(body))
	req = req.WithContext(spi.WithUserContext(req.Context(), adminCtx()))
	w := httptest.NewRecorder()
	h.RegisterTrustedKey(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestRegisterTrustedKey_RejectsNonRSA(t *testing.T) {
	ts := auth.NewInMemoryTrustedKeyStore()
	iam := auth.DefaultIAMConfig()
	iam.TrustedKeyRegistrationEnabled = true
	h := account.New(nil, nil, auth.NewInMemoryKeyStore(), ts, iam)
	// EC JWK
	jwk := map[string]interface{}{"kty": "EC", "kid": "k1", "crv": "P-256", "x": "abc", "y": "def"}
	body, _ := json.Marshal(genapi.RegisterTrustedKeyRequestDto{KeyId: "k1", Jwk: jwk, Audience: "human"})
	req := httptest.NewRequest("POST", "/oauth/keys/trusted", bytes.NewReader(body))
	req = req.WithContext(spi.WithUserContext(req.Context(), adminCtx()))
	w := httptest.NewRecorder()
	h.RegisterTrustedKey(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	var pd map[string]any
	json.Unmarshal(w.Body.Bytes(), &pd)
	if pd["errorCode"] != "UNSUPPORTED_KEY_TYPE" {
		t.Errorf("errorCode=%v", pd["errorCode"])
	}
}

func TestRegisterTrustedKey_CrossTenantCollision_409(t *testing.T) {
	ts := auth.NewInMemoryTrustedKeyStore()
	iam := auth.DefaultIAMConfig()
	iam.TrustedKeyRegistrationEnabled = true
	// pre-register from tenant-a
	pre := &auth.TrustedKey{KID: "shared", TenantID: "tenant-a", PublicKey: testRSAPubKey(t), Audience: "human", Active: true, ValidFrom: time.Now()}
	_ = ts.Register(pre, auth.RotateOptions{})
	h := account.New(nil, nil, auth.NewInMemoryKeyStore(), ts, iam)
	// tenant-b register
	uc := &spi.UserContext{UserID: "u", UserName: "u", Tenant: spi.Tenant{ID: "tenant-b", Name: "B"}, Roles: []string{"ROLE_ADMIN"}}
	body, _ := json.Marshal(genapi.RegisterTrustedKeyRequestDto{KeyId: "shared", Jwk: rsaJWK(t, "shared"), Audience: "human"})
	req := httptest.NewRequest("POST", "/oauth/keys/trusted", bytes.NewReader(body))
	req = req.WithContext(spi.WithUserContext(req.Context(), uc))
	w := httptest.NewRecorder()
	h.RegisterTrustedKey(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", w.Code)
	}
	var pd map[string]any
	json.Unmarshal(w.Body.Bytes(), &pd)
	if pd["errorCode"] != "KEY_OWNED_BY_DIFFERENT_TENANT" {
		t.Errorf("errorCode=%v", pd["errorCode"])
	}
}
```

Add helper `testRSAPubKey` if not already imported from another test file in the same package; copy from Phase 2 if needed.

- [ ] **Step 2: Run tests (expected FAIL)**

Run: `go test ./internal/domain/account/ -run TestRegisterTrustedKey -v`
Expected: FAIL.

- [ ] **Step 3: Remove the stub + write adapter**

Delete `RegisterTrustedKey` stub from `handler.go`. Write `internal/domain/account/trusted_adapter.go`:

```go
package account

import (
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"

	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// gateTrustedKeyFeature returns false if the feature flag is off; the
// adapter has already written a 404 FEATURE_DISABLED response.
func (h *Handler) gateTrustedKeyFeature(w http.ResponseWriter, r *http.Request) bool {
	if !h.iam.TrustedKeyRegistrationEnabled {
		common.WriteError(w, r, common.Operational(http.StatusNotFound, common.ErrCodeFeatureDisabled, "trusted-key registration is disabled"))
		return false
	}
	return true
}

// --- RegisterTrustedKey ---

func (h *Handler) RegisterTrustedKey(w http.ResponseWriter, r *http.Request) {
	if !auth.RequireAdmin(w, r) {
		return
	}
	if !h.gateTrustedKeyFeature(w, r) {
		return
	}

	var req genapi.RegisterTrustedKeyRequestDto
	if err := boundedJSONDecode(w, r, 1<<20, &req); err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid request body"))
		return
	}

	// keyId regex
	if !auth.MatchesTrustedKIDPatternForTesting(req.KeyId) {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid keyId format"))
		return
	}

	// JWK content checks: kty=RSA, kid==keyId, ≤ MaxJWKProperties
	pubKey, jwkErr := parseTrustedJWK(req.Jwk, req.KeyId, h.iam.TrustedKeyMaxJWKProperties)
	if jwkErr != nil {
		var keyTypeErr *unsupportedKeyTypeError
		if errors.As(jwkErr, &keyTypeErr) {
			common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeUnsupportedKeyType, jwkErr.Error()))
			return
		}
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, jwkErr.Error()))
		return
	}

	if !isValidKeyPairAudience(string(req.Audience)) {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid audience"))
		return
	}

	now := time.Now()
	validFrom := now
	if req.ValidFrom != nil {
		validFrom = time.Time(*req.ValidFrom)
	}
	validTo := validFrom.Add(time.Duration(h.iam.TrustedKeyMaxValidityDays) * 24 * time.Hour)
	if req.ValidTo != nil {
		validTo = time.Time(*req.ValidTo)
	}
	if !validTo.After(validFrom) {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "validTo must be > validFrom"))
		return
	}

	var grace int64
	if req.InvalidateGracePeriodSec != nil {
		grace = *req.InvalidateGracePeriodSec
		if grace < 0 {
			common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "gracePeriodSec must be >= 0"))
			return
		}
	}
	invalidate := false
	if req.InvalidatePrevious != nil {
		invalidate = *req.InvalidatePrevious
	}

	var issuers []string
	if req.Issuers != nil {
		issuers = *req.Issuers
	}

	vt := validTo
	tk := &auth.TrustedKey{
		KID:       req.KeyId,
		TenantID:  tenantFromCtx(r),
		JWK:       req.Jwk, // store the request JWK verbatim for round-trip
		PublicKey: pubKey,
		Audience:  string(req.Audience),
		Issuers:   issuers,
		Active:    true,
		ValidFrom: validFrom,
		ValidTo:   &vt,
	}
	if err := h.trustedKeyStore.Register(tk, auth.RotateOptions{Invalidate: invalidate, GracePeriodSec: grace}); err != nil {
		// Translate AppError (409 cross-tenant, 400 cap reached) verbatim.
		var ae *common.AppError
		if errors.As(err, &ae) {
			common.WriteError(w, r, ae)
			return
		}
		common.WriteError(w, r, common.Internal("trustedKeyStore.Register", err))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(toTrustedKeyResponse(tk))
}

// --- JWK parsing with structured errors ---

type unsupportedKeyTypeError struct{ msg string }

func (e *unsupportedKeyTypeError) Error() string { return e.msg }

// parseTrustedJWK enforces all four content checks (kty, kid match, max
// properties, RSA-parseable). Returns a *unsupportedKeyTypeError when the
// reason is a non-RSA kty so the caller can map to UNSUPPORTED_KEY_TYPE.
func parseTrustedJWK(jwk map[string]interface{}, keyId string, maxProps int) (*rsa.PublicKey, error) {
	if len(jwk) > maxProps {
		return nil, fmt.Errorf("jwk has too many properties (%d > %d)", len(jwk), maxProps)
	}
	kty, _ := jwk["kty"].(string)
	if kty == "" {
		return nil, fmt.Errorf("jwk missing kty")
	}
	if kty != "RSA" {
		return nil, &unsupportedKeyTypeError{msg: "only RSA JWKs supported (v0.8.0)"}
	}
	if rawKid, ok := jwk["kid"]; ok {
		if s, _ := rawKid.(string); s != keyId {
			return nil, fmt.Errorf("jwk.kid (%q) must equal keyId (%q)", s, keyId)
		}
	}
	n, _ := jwk["n"].(string)
	e, _ := jwk["e"].(string)
	if n == "" || e == "" {
		return nil, fmt.Errorf("jwk missing n or e")
	}
	// Build via parseRSAPublicKeyFromJWK in package auth.
	raw, _ := json.Marshal(jwk)
	pk, err := auth.ParseRSAPublicKeyFromJWKForTesting(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid jwk: %w", err)
	}
	_ = big.NewInt(0) // silence import if not otherwise needed
	return pk, nil
}

// --- response helpers ---

func toTrustedKeyResponse(tk *auth.TrustedKey) genapi.TrustedKeyResponseDto {
	resp := genapi.TrustedKeyResponseDto{
		KeyId:         tk.KID,
		LegalEntityId: string(tk.TenantID),
		Jwk:           tk.JWK,
		Audience:      genapi.TrustedKeyResponseDtoAudience(tk.Audience),
		ValidFrom:     openapi_types.Date{Time: tk.ValidFrom},
	}
	if tk.Issuers != nil {
		issuers := tk.Issuers
		resp.Issuers = &issuers
	}
	if tk.ValidTo != nil {
		vt := openapi_types.Date{Time: *tk.ValidTo}
		resp.ValidTo = &vt
	}
	return resp
}
```

Also add to `internal/auth/keyvalidation.go`:

```go
// MatchesTrustedKIDPatternForTesting exposes trustedKIDPattern for adapter callers.
func MatchesTrustedKIDPatternForTesting(kid string) bool {
	return trustedKIDPattern.MatchString(kid)
}

// ParseRSAPublicKeyFromJWKForTesting exposes parseRSAPublicKeyFromJWK for adapter callers.
func ParseRSAPublicKeyFromJWKForTesting(jwk json.RawMessage) (*rsa.PublicKey, error) {
	return parseRSAPublicKeyFromJWK(jwk)
}
```

(The `ForTesting` suffix is wrong here — these are real adapter callers. Rename to `MatchesTrustedKIDPattern` and `ParseRSAPublicKeyFromJWK` — exported.)

- [ ] **Step 4: Run tests + commit**

Run: `go test ./internal/domain/account/ -run TestRegisterTrustedKey -v`
Expected: PASS.

```bash
git add internal/domain/account/trusted_adapter.go internal/domain/account/trusted_adapter_test.go internal/domain/account/handler.go internal/auth/keyvalidation.go
git commit -m "feat(account): RegisterTrustedKey adapter

Feature flag (404 FEATURE_DISABLED when off). keyId regex. JWK
content checks: kty=RSA only (else 400 UNSUPPORTED_KEY_TYPE),
kid==keyId, max properties. Audience enum. validTo default
validFrom+365d; validTo<validFrom rejected. RotateOptions for atomic
sibling-flip. Cross-tenant collision -> 409 KEY_OWNED_BY_DIFFERENT_TENANT.
Cap reached -> 400 TRUSTED_KEY_CAP_REACHED.

Refs #281."
```

---

### Task 20: ListTrustedKeys + Delete/Invalidate/Reactivate TrustedKey adapters

**Files:**
- Modify: `internal/domain/account/trusted_adapter.go`
- Modify: `internal/domain/account/trusted_adapter_test.go`
- Modify: `internal/domain/account/handler.go` (remove 4 stubs)

- [ ] **Step 1: Write failing tests**

Append to `trusted_adapter_test.go`:

```go
func TestListTrustedKeys_TenantScoped(t *testing.T) {
	ts := auth.NewInMemoryTrustedKeyStore()
	mine := &auth.TrustedKey{KID: "mine", TenantID: "t1", PublicKey: testRSAPubKey(t), Audience: "human", Active: true, ValidFrom: time.Now(), JWK: map[string]any{"kty": "RSA", "kid": "mine"}}
	theirs := &auth.TrustedKey{KID: "theirs", TenantID: "other", PublicKey: testRSAPubKey(t), Audience: "human", Active: true, ValidFrom: time.Now(), JWK: map[string]any{"kty": "RSA", "kid": "theirs"}}
	_ = ts.Register(mine, auth.RotateOptions{})
	_ = ts.Register(theirs, auth.RotateOptions{})
	iam := auth.DefaultIAMConfig()
	iam.TrustedKeyRegistrationEnabled = true
	h := account.New(nil, nil, auth.NewInMemoryKeyStore(), ts, iam)
	req := httptest.NewRequest("GET", "/oauth/keys/trusted", nil)
	req = req.WithContext(spi.WithUserContext(req.Context(), adminCtx()))
	w := httptest.NewRecorder()
	h.ListTrustedKeys(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp []genapi.TrustedKeyResponseDto
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp) != 1 || resp[0].KeyId != "mine" {
		t.Fatalf("expected only 'mine', got %+v", resp)
	}
}

func TestDeleteTrustedKey_CrossTenant_404(t *testing.T) {
	ts := auth.NewInMemoryTrustedKeyStore()
	tk := &auth.TrustedKey{KID: "k", TenantID: "other", PublicKey: testRSAPubKey(t), Audience: "human", Active: true, ValidFrom: time.Now()}
	_ = ts.Register(tk, auth.RotateOptions{})
	iam := auth.DefaultIAMConfig()
	iam.TrustedKeyRegistrationEnabled = true
	h := account.New(nil, nil, auth.NewInMemoryKeyStore(), ts, iam)
	req := httptest.NewRequest("DELETE", "/", nil)
	req = req.WithContext(spi.WithUserContext(req.Context(), adminCtx()))
	w := httptest.NewRecorder()
	h.DeleteTrustedKey(w, req, "k")
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 (cross-tenant should not leak existence), got %d", w.Code)
	}
}

func TestInvalidateTrustedKey_WithGrace(t *testing.T) {
	ts := auth.NewInMemoryTrustedKeyStore()
	tk := &auth.TrustedKey{KID: "k", TenantID: "t1", PublicKey: testRSAPubKey(t), Audience: "human", Active: true, ValidFrom: time.Now(), JWK: map[string]any{"kty": "RSA", "kid": "k"}}
	_ = ts.Register(tk, auth.RotateOptions{})
	iam := auth.DefaultIAMConfig()
	iam.TrustedKeyRegistrationEnabled = true
	h := account.New(nil, nil, auth.NewInMemoryKeyStore(), ts, iam)
	body, _ := json.Marshal(genapi.InvalidateKeyRequestDto{GracePeriodSec: ptrInt64(60)})
	req := httptest.NewRequest("POST", "/", bytes.NewReader(body))
	req = req.WithContext(spi.WithUserContext(req.Context(), adminCtx()))
	w := httptest.NewRecorder()
	h.InvalidateTrustedKey(w, req, "k")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	got, _ := ts.Get("t1", "k")
	if got.Active || got.ValidTo == nil {
		t.Errorf("expected invalidated with grace, got %+v", got)
	}
}

func TestReactivateTrustedKey_RequiresValidTo(t *testing.T) {
	ts := auth.NewInMemoryTrustedKeyStore()
	past := time.Now().Add(-1 * time.Hour)
	tk := &auth.TrustedKey{KID: "k", TenantID: "t1", PublicKey: testRSAPubKey(t), Audience: "human", Active: false, ValidFrom: past, ValidTo: &past, JWK: map[string]any{"kty": "RSA", "kid": "k"}}
	_ = ts.Register(tk, auth.RotateOptions{})
	iam := auth.DefaultIAMConfig()
	iam.TrustedKeyRegistrationEnabled = true
	h := account.New(nil, nil, auth.NewInMemoryKeyStore(), ts, iam)
	body, _ := json.Marshal(genapi.ReactivateKeyRequestDto{ValidTo: openapi_types.Date{Time: time.Now().Add(24 * time.Hour)}})
	req := httptest.NewRequest("POST", "/", bytes.NewReader(body))
	req = req.WithContext(spi.WithUserContext(req.Context(), adminCtx()))
	w := httptest.NewRecorder()
	h.ReactivateTrustedKey(w, req, "k")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}
```

- [ ] **Step 2: Run (expected FAIL)**

Run: `go test ./internal/domain/account/ -run "TestListTrustedKeys|TestDeleteTrustedKey|TestInvalidateTrustedKey|TestReactivateTrustedKey" -v`
Expected: FAIL.

- [ ] **Step 3: Remove stubs + write adapters**

Delete `ListTrustedKeys`, `DeleteTrustedKey`, `InvalidateTrustedKey`, `ReactivateTrustedKey` stubs in `handler.go`. Append to `trusted_adapter.go`:

```go
func (h *Handler) ListTrustedKeys(w http.ResponseWriter, r *http.Request) {
	if !auth.RequireAdmin(w, r) {
		return
	}
	if !h.gateTrustedKeyFeature(w, r) {
		return
	}
	tenantID := tenantFromCtx(r)
	keys := h.trustedKeyStore.List(tenantID)
	out := make([]genapi.TrustedKeyResponseDto, 0, len(keys))
	for _, k := range keys {
		out = append(out, toTrustedKeyResponse(k))
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (h *Handler) DeleteTrustedKey(w http.ResponseWriter, r *http.Request, keyId string) {
	if !auth.RequireAdmin(w, r) {
		return
	}
	if !h.gateTrustedKeyFeature(w, r) {
		return
	}
	if !auth.MatchesTrustedKIDPattern(keyId) {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid keyId format"))
		return
	}
	tenantID := tenantFromCtx(r)
	if err := h.trustedKeyStore.Delete(tenantID, keyId); err != nil {
		common.WriteError(w, r, common.Operational(http.StatusNotFound, common.ErrCodeTrustedKeyNotFound, "trusted key not found"))
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) InvalidateTrustedKey(w http.ResponseWriter, r *http.Request, keyId string) {
	if !auth.RequireAdmin(w, r) {
		return
	}
	if !h.gateTrustedKeyFeature(w, r) {
		return
	}
	if !auth.MatchesTrustedKIDPattern(keyId) {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid keyId format"))
		return
	}
	var grace int64
	if r.ContentLength != 0 {
		var req genapi.InvalidateKeyRequestDto
		if err := boundedJSONDecode(w, r, 1<<20, &req); err != nil {
			common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid request body"))
			return
		}
		if req.GracePeriodSec != nil {
			grace = *req.GracePeriodSec
			if grace < 0 {
				common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "gracePeriodSec must be >= 0"))
				return
			}
		}
	}
	tenantID := tenantFromCtx(r)
	if err := h.trustedKeyStore.Invalidate(tenantID, keyId, grace); err != nil {
		common.WriteError(w, r, common.Operational(http.StatusNotFound, common.ErrCodeTrustedKeyNotFound, "trusted key not found"))
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) ReactivateTrustedKey(w http.ResponseWriter, r *http.Request, keyId string) {
	if !auth.RequireAdmin(w, r) {
		return
	}
	if !h.gateTrustedKeyFeature(w, r) {
		return
	}
	if !auth.MatchesTrustedKIDPattern(keyId) {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid keyId format"))
		return
	}
	var req genapi.ReactivateKeyRequestDto
	if err := boundedJSONDecode(w, r, 1<<20, &req); err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid request body"))
		return
	}
	validFrom := time.Now()
	if req.ValidFrom != nil {
		validFrom = time.Time(*req.ValidFrom)
	}
	validTo := time.Time(req.ValidTo)
	if !validTo.After(time.Now()) {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "validTo must be in the future"))
		return
	}
	if !validTo.After(validFrom) {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "validTo must be > validFrom"))
		return
	}
	tenantID := tenantFromCtx(r)
	if err := h.trustedKeyStore.Reactivate(tenantID, keyId, validFrom, validTo); err != nil {
		common.WriteError(w, r, common.Operational(http.StatusNotFound, common.ErrCodeTrustedKeyNotFound, "trusted key not found"))
		return
	}
	w.WriteHeader(http.StatusOK)
}
```

- [ ] **Step 4: Run + commit**

Run: `go test ./internal/domain/account/ -v`
Expected: PASS.

```bash
git add internal/domain/account/trusted_adapter.go internal/domain/account/trusted_adapter_test.go internal/domain/account/handler.go
git commit -m "feat(account): List/Delete/Invalidate/Reactivate TrustedKey adapters

All gate the feature flag (404 FEATURE_DISABLED when off). All gate
ROLE_ADMIN. KeyId regex on path-param. Cross-tenant access returns
404 TRUSTED_KEY_NOT_FOUND uniformly (no existence leak). Reactivate
requires ReactivateKeyRequestDto with validTo > now and > validFrom.

Refs #281."
```


---

## Phase 7: Cleanup — remove legacy handlers + mux + body-size relocation

### Task 21: Delete `internal/auth/keys.go` and `internal/auth/trusted.go`

**Files:**
- Delete: `internal/auth/keys.go`
- Delete: `internal/auth/trusted.go`

- [ ] **Step 1: Delete the files**

```bash
git rm internal/auth/keys.go internal/auth/trusted.go
```

- [ ] **Step 2: Verify build (should still build because validators moved to keyvalidation.go and adapters own the HTTP shape)**

Run: `go build ./...`
Expected: PASS — if it fails, the error points to a leftover caller in `service.go` (the `NewKeysHandler`/`NewTrustedKeysHandler` construction); delete those construction lines (Task 22 handles adminMux entries; finish them together if surfaced now).

- [ ] **Step 3: Run unit tests**

Run: `go test ./internal/auth/ -short`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add -A
git commit -m "refactor(auth): delete keys.go and trusted.go HTTP handlers

The 10 OpenAPI operations are now served by adapters in
internal/domain/account/{keys,trusted}_adapter.go. Validators moved to
keyvalidation.go in Task 3. RotateOptions atomic sibling-flip lives in
store.go.

Refs #281."
```

---

### Task 22: Remove `/oauth/keys/*` adminMux entries in `service.go`

**Files:**
- Modify: `internal/auth/service.go`

- [ ] **Step 1: Locate the adminMux block**

Run: `grep -n "adminMux\|/oauth/keys/keypair\|/oauth/keys/trusted" internal/auth/service.go`

Identify lines 84-91 (per the earlier exploration).

- [ ] **Step 2: Delete the 4 entries**

Edit `internal/auth/service.go` — delete the lines:

```go
adminMux.Handle("/oauth/keys/keypair/", keysHandler)
adminMux.Handle("/oauth/keys/keypair", keysHandler)
adminMux.Handle("/oauth/keys/trusted/", trustedHandler)
adminMux.Handle("/oauth/keys/trusted", trustedHandler)
```

Also delete the construction of `keysHandler` and `trustedHandler` if they're no longer referenced.

Leave the `/account/m2m` entries intact (they belong to #194-B).

- [ ] **Step 3: Verify build + tests**

Run: `go build ./... && go test ./internal/auth/ -short`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/auth/service.go
git commit -m "refactor(auth): remove /oauth/keys/* adminMux entries

The 4 entries are now served by chi via the account adapters.
/account/m2m entries remain (handled by #194-B).

Refs #281."
```

---

### Task 23: Remove `mux.Handle("/oauth/keys/", ...)` in `app/app.go`

**Files:**
- Modify: `app/app.go`

- [ ] **Step 1: Find the line**

Run: `grep -n 'mux.Handle("/oauth/keys/"' app/app.go`

Expected: line 482 (or thereabouts).

- [ ] **Step 2: Delete the line**

Remove `mux.Handle("/oauth/keys/", authMW(authSvc.AdminHandler()))` only. Keep `/account/m2m/` and `/account/m2m` mux entries — they belong to #194-B.

- [ ] **Step 3: Verify build + run integration tests**

Run: `go build ./... && go test ./internal/auth/ -short`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add app/app.go
git commit -m "refactor(app): remove /oauth/keys/* prefix mux entry

Chi router (mounted at /) now owns all /oauth/keys/* paths via the
10 ServerInterface methods on the account Handler.

Refs #281."
```

---

### Task 24: Bootstrap key wiring — audience + algorithm + validTo

**Files:**
- Modify: `app/app.go` (or wherever bootstrap-key save happens; around lines 207-239)
- Modify: `internal/auth/service.go` if bootstrap save happens there

- [ ] **Step 1: Find the bootstrap save**

Run: `grep -rn "ParseRSAPrivateKeyFromPEM\|keyStore.Save\|signingKID\b" internal/auth/ app/`

The bootstrap PEM-derived key is saved during `NewAuthService` (in `service.go`) — the deterministic-KID derivation is around the "Derive KID deterministically" comment.

- [ ] **Step 2: Update the bootstrap KeyPair construction**

Where the bootstrap `KeyPair` is built (in `service.go`'s `NewAuthService`), set:

```go
now := time.Now().UTC()
validTo := now.Add(time.Duration(config.IAM.KeypairDefaultValidityDays) * 24 * time.Hour)
kp := &KeyPair{
	KID:        signingKID,
	Audience:   config.IAM.BootstrapAudience, // default "client"
	Algorithm:  "RS256",
	PublicKey:  &privateKey.PublicKey,
	PrivateKey: privateKey,
	Active:     true,
	ValidFrom:  now,
	ValidTo:    &validTo,
}
if err := keyStore.Save(kp, RotateOptions{}); err != nil {
	return nil, fmt.Errorf("save bootstrap key: %w", err)
}
```

If `config.IAM.KeypairDefaultValidityDays == 0`, use 365 as a defensive default (the validation gate ensures it's nonzero in production; this guards tests that pass empty AuthConfig).

- [ ] **Step 3: Add startup WARN for short-lived bootstrap keys**

After the save, add:

```go
if validTo.Sub(now) < 30*24*time.Hour {
	slog.Warn("bootstrap signing key expires soon", "pkg", "auth", "kid", signingKID, "validTo", validTo.Format(time.RFC3339))
}
```

- [ ] **Step 4: Update existing tests that constructed AuthService with empty IAM config**

Run: `go test ./internal/auth/ -short`

Failures will surface where existing tests don't populate `AuthConfig.IAM`. Fix by inserting `IAM: auth.DefaultIAMConfig()` into the AuthConfig literal in each failing test, OR have `NewAuthService` apply `DefaultIAMConfig()` when fields are zero.

Recommended: in `NewAuthService`, apply defaults at the top of the function:

```go
if config.IAM.KeypairDefaultValidityDays == 0 {
	config.IAM = DefaultIAMConfig()
}
```

That preserves backwards compat for test fixtures while keeping production validation strict (the strict path runs at boot via `app/config.go`).

- [ ] **Step 5: Run tests**

Run: `go test ./internal/auth/ -short -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/auth/service.go
git commit -m "feat(auth): bootstrap signing key honors IAMConfig defaults

Bootstrap key gets Audience = config.IAM.BootstrapAudience (default
client), Algorithm = RS256, ValidTo = now + KeypairDefaultValidityDays
(default 365). Startup WARN if ValidTo < 30 days away.

Behaviour change: bootstrap key now has finite validity (was nil).
Documented in v0.8.0 release notes.

Refs #281."
```

---

### Task 25: Switch `token.go` to `getTrustedKeyByKID`

**Files:**
- Modify: `internal/auth/token.go`

- [ ] **Step 1: Find the existing trusted-key lookup**

Run: `grep -n "trustedKeyStore.Get\|trustedStore.Get" internal/auth/token.go`

Around line 137 (per earlier exploration).

- [ ] **Step 2: Update the lookup**

Replace `h.trustedStore.Get(kid)` (single-arg) with `getTrustedKeyByKID(h.trustedStore, kid)` (in-package helper).

Note: the existing `client.TenantID != subOrgID` check at line 203 is NOT removed — it remains the principal-tenant invariant per spec §7.1.

- [ ] **Step 3: Switch JWKS endpoint source**

Search:

```
grep -n "keyStore.List\|h.keyStore.List" internal/auth/service.go internal/auth/jwt.go
```

Find the JWKS-publishing call (likely in `service.go` or a JWKS handler file). Replace `keyStore.List()` with `keyStore.ListForVerification()` so grace-period keys remain published until ValidTo.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/auth/ -short -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/auth/token.go internal/auth/service.go
git commit -m "feat(auth): token-exchange uses getTrustedKeyByKID; JWKS lazy-filtered

token.go switches from trustedStore.Get(kid) to in-package
getTrustedKeyByKID helper (matches cloud: no new check on trustedKey
tenant; existing client.TenantID == subOrgID check at line 203 remains).
JWKS endpoint sources from KeyStore.ListForVerification so grace-period
keys remain published until ValidTo.

Refs #281."
```

---

### Task 26: M2M signing path — switch to `GetActive("client")`

**Files:**
- Modify: `internal/auth/token.go` (or `internal/auth/jwt.go` — wherever the M2M signing flow calls GetActive)

- [ ] **Step 1: Find every `KeyStore.GetActive(` call**

Run:

```
grep -rn "keyStore.GetActive\|KeyStore.GetActive\|h.keyStore.GetActive" internal/auth/
```

- [ ] **Step 2: Pass `"client"` to every callsite**

The M2M token-issuance path (`/oauth/token` `client_credentials` grant) needs the client-audience key. Pass `"client"`.

If any callsite signs human-audience tokens — STOP and surface to the user. Per spec §3.1 #9, the pre-merge verification step is "grep all `KeyStore.GetActive()` call sites and confirm none implicitly sign human-audience tokens today; if any exist, this default needs revisiting." If found, that's a real concern.

- [ ] **Step 3: Run tests**

Run: `go test ./internal/auth/ -short`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/auth/
git commit -m "feat(auth): M2M signing path uses GetActive(client)

The token-issuance path (POST /oauth/token client_credentials) now
explicitly requests the client-audience key. No existing path signs
human-audience tokens (verified by grep).

Refs #281."
```

---

### Task 27: Remove relocated body-size sub-test

**Files:**
- Modify: `internal/auth/integration_test.go`

- [ ] **Step 1: Delete the trusted-key sub-test**

Edit `internal/auth/integration_test.go` — delete the `t.Run("trusted key register endpoint rejects oversized body", ...)` block (lines 287-301 per earlier exploration).

Keep the M2M sub-test (handled by #194-B).

- [ ] **Step 2: Run integration tests**

Run: `go test ./internal/auth/ -run TestIntegration_JWTMode_RequestBodySizeLimit -v`
Expected: PASS (with the trusted-key sub-test gone; the M2M sub-test still runs).

- [ ] **Step 3: Commit**

```bash
git add internal/auth/integration_test.go
git commit -m "test(auth): drop relocated trusted-key body-size sub-test

The assertion is replaced by boundedJSONDecode + the new adapter-level
test in account/io_test.go and the E2E suite. The M2M sub-test stays
(belongs to #194-B).

Refs #281."
```

---

## Phase 8: E2E tests

### Task 28: E2E happy-path for the 10 operations

**Files:**
- Create: `internal/e2e/oauth_keys_test.go`

- [ ] **Step 1: Inspect existing E2E scaffolding**

Run: `head -80 internal/e2e/e2e_test.go` (or whatever the existing E2E setup file is — find via `grep -l TestMain internal/e2e/`).

Identify: how the test server is started, how a bootstrap M2M token is obtained, how authenticated requests are made.

- [ ] **Step 2: Write the happy-path E2E**

Write `internal/e2e/oauth_keys_test.go`:

```go
package e2e_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// Helper: obtain a Bearer token from POST /oauth/token using the bootstrap
// M2M client. Reuse whatever helper the existing E2E suite already exposes;
// if there isn't one, factor it out from e2e_test.go into a small helper.
func adminBearer(t *testing.T) string {
	t.Helper()
	// REPLACE this with the actual bootstrap-M2M-credentials -> /oauth/token
	// flow used elsewhere in internal/e2e. The skeleton is illustrative.
	return obtainBootstrapM2MToken(t)
}

func TestE2E_IssueAndGetCurrentKeyPair(t *testing.T) {
	srv := startTestServer(t)
	defer srv.Close()
	token := adminBearer(t)

	// Issue
	body, _ := json.Marshal(map[string]any{"algorithm": "RS256", "audience": "client"})
	req, _ := http.NewRequest("POST", srv.URL+"/oauth/keys/keypair", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("issue status=%d", resp.StatusCode)
	}
	resp.Body.Close()

	// Get current
	req, _ = http.NewRequest("GET", srv.URL+"/oauth/keys/keypair/current?audience=client", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("getCurrent status=%d", resp.StatusCode)
	}
	resp.Body.Close()
}

// Similar helpers for the other 8 operations follow the same pattern;
// keep them in this file as TestE2E_<Operation>_HappyPath. Each test is a
// single round-trip through the full chi -> adapter -> store stack.
```

Add the remaining 8 ops (Delete/Invalidate/Reactivate of keypair; List/Register/Delete/Invalidate/Reactivate of trusted with flag enabled).

- [ ] **Step 3: Run**

Run: `go test ./internal/e2e/ -run TestE2E_Issue -v`
Expected: PASS (the helper bindings need to match the actual e2e_test.go scaffolding — adjust as needed).

- [ ] **Step 4: Commit**

```bash
git add internal/e2e/oauth_keys_test.go
git commit -m "test(e2e): happy-path E2E for the 10 /oauth/keys/* ops

Round-trip each operation through chi router with bootstrap-M2M Bearer.
Asserts status + DTO shape. Trusted-key tests run with
TrustedKeyRegistrationEnabled=true; flag-off case is a separate test.

Refs #281."
```

---

### Task 29: E2E grace-period round-trip + persistence

**Files:**
- Modify: `internal/e2e/oauth_keys_test.go`

- [ ] **Step 1: Add grace-period test**

Append to `oauth_keys_test.go`:

```go
func TestE2E_GracePeriodRoundTrip(t *testing.T) {
	srv := startTestServer(t)
	defer srv.Close()
	token := adminBearer(t)

	// Issue A
	body, _ := json.Marshal(map[string]any{"algorithm": "RS256", "audience": "client"})
	respA := postJSON(t, srv.URL+"/oauth/keys/keypair", body, token)
	var keyA struct{ KeyId string `json:"keyId"` }
	_ = json.NewDecoder(respA.Body).Decode(&keyA)
	respA.Body.Close()

	// Issue B with invalidateCurrent + grace=2
	body, _ = json.Marshal(map[string]any{
		"algorithm":               "RS256",
		"audience":                "client",
		"invalidateCurrent":       true,
		"invalidateGracePeriodSec": 2,
	})
	respB := postJSON(t, srv.URL+"/oauth/keys/keypair", body, token)
	respB.Body.Close()

	// Immediately: A still in JWKS
	if !jwksContainsKID(t, srv.URL, keyA.KeyId) {
		t.Error("A's kid should still be in JWKS during grace")
	}

	// Wait past grace
	time.Sleep(3 * time.Second)

	if jwksContainsKID(t, srv.URL, keyA.KeyId) {
		t.Error("A's kid should be excluded from JWKS after grace")
	}
}

func postJSON(t *testing.T, url string, body []byte, token string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("postJSON: %v", err)
	}
	return resp
}

func jwksContainsKID(t *testing.T, baseURL, kid string) bool {
	t.Helper()
	resp, err := http.Get(baseURL + "/.well-known/jwks.json")
	if err != nil {
		t.Fatalf("jwks: %v", err)
	}
	defer resp.Body.Close()
	var data struct {
		Keys []struct {
			Kid string `json:"kid"`
		} `json:"keys"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&data)
	for _, k := range data.Keys {
		if k.Kid == kid {
			return true
		}
	}
	return false
}
```

- [ ] **Step 2: Add persistence test**

Append:

```go
func TestE2E_TrustedKeyPersistence(t *testing.T) {
	srv1, factory := startTestServerWithFactory(t)
	token := adminBearer(t)
	body := registerTrustedKeyBody(t, "persistent")
	resp := postJSON(t, srv1.URL+"/oauth/keys/trusted", body, token)
	resp.Body.Close()
	srv1.Close()

	// Restart with the same store factory
	srv2 := restartTestServerWithFactory(t, factory)
	defer srv2.Close()
	token2 := adminBearer(t)
	req, _ := http.NewRequest("GET", srv2.URL+"/oauth/keys/trusted", nil)
	req.Header.Set("Authorization", "Bearer "+token2)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	defer resp.Body.Close()
	var list []struct{ KeyId string `json:"keyId"` }
	_ = json.NewDecoder(resp.Body).Decode(&list)
	found := false
	for _, k := range list {
		if k.KeyId == "persistent" {
			found = true
		}
	}
	if !found {
		t.Error("registered trusted key should survive restart")
	}
}
```

The `startTestServerWithFactory` / `restartTestServerWithFactory` helpers need to share the underlying `KeyValueStore` instance between the two `app.New` invocations. Match the project's existing pattern; if no such pattern exists, factor a small `e2e_helpers.go` from `e2e_test.go`.

- [ ] **Step 3: Run + commit**

Run: `go test ./internal/e2e/ -run "TestE2E_GracePeriod|TestE2E_TrustedKeyPersistence" -v`
Expected: PASS.

```bash
git add internal/e2e/
git commit -m "test(e2e): grace-period round-trip + trusted-key persistence

Grace test: issue A, issue B with invalidateCurrent+grace=2s, assert
A in JWKS immediately, sleep(3s), assert A excluded.
Persistence test: register trusted key, restart server with shared
store factory, list returns the key.

Refs #281."
```

---

### Task 30: E2E cross-tenant + feature-flag + token-exchange tenant invariant

**Files:**
- Modify: `internal/e2e/oauth_keys_test.go`

- [ ] **Step 1: Cross-tenant isolation test**

Append:

```go
func TestE2E_CrossTenantIsolation(t *testing.T) {
	srv := startTestServerWithTwoTenants(t)
	defer srv.Close()
	tokenA := bearerForTenant(t, srv, "tenant-a")
	tokenB := bearerForTenant(t, srv, "tenant-b")

	body := registerTrustedKeyBody(t, "shared-kid")
	respA := postJSON(t, srv.URL+"/oauth/keys/trusted", body, tokenA)
	if respA.StatusCode != http.StatusCreated {
		t.Fatalf("A register status=%d", respA.StatusCode)
	}
	respA.Body.Close()

	// Tenant B registering same kid -> 409
	respB := postJSON(t, srv.URL+"/oauth/keys/trusted", body, tokenB)
	if respB.StatusCode != http.StatusConflict {
		t.Fatalf("B should get 409, got %d", respB.StatusCode)
	}
	respB.Body.Close()

	// Tenant B listing -> empty
	req, _ := http.NewRequest("GET", srv.URL+"/oauth/keys/trusted", nil)
	req.Header.Set("Authorization", "Bearer "+tokenB)
	respList, _ := http.DefaultClient.Do(req)
	defer respList.Body.Close()
	var list []struct{ KeyId string `json:"keyId"` }
	_ = json.NewDecoder(respList.Body).Decode(&list)
	if len(list) != 0 {
		t.Errorf("tenant B should see empty list, got %d entries", len(list))
	}
}
```

- [ ] **Step 2: Feature-flag test**

Append:

```go
func TestE2E_TrustedKeyFlagDisabled(t *testing.T) {
	srv := startTestServerWithFlag(t, false) // TrustedKeyRegistrationEnabled=false
	defer srv.Close()
	token := adminBearer(t)

	for _, path := range []string{
		"/oauth/keys/trusted",       // list
	} {
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, _ := http.DefaultClient.Do(req)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: expected 404, got %d", path, resp.StatusCode)
		}
		resp.Body.Close()
	}

	// Keypair endpoints still work
	req, _ := http.NewRequest("GET", srv.URL+"/oauth/keys/keypair/current?audience=client", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode == http.StatusNotFound && resp.Header.Get("Content-Type") == "application/problem+json" {
		// 404 KEYPAIR_NOT_FOUND is acceptable if no key issued yet; flag is unrelated.
		resp.Body.Close()
		return
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("getCurrent: expected 200 or 404 KEYPAIR_NOT_FOUND, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}
```

- [ ] **Step 3: Token-exchange tenant-invariant test**

Append a test that registers a trusted key for tenant A, mints a subject token signed by that key with `caas_org_id=A`, calls `POST /oauth/token` with grant `urn:ietf:params:oauth:grant-type:token-exchange` as tenant A's M2M client → success; then repeats with tenant B's client → 4xx rejection.

Use the existing `signSubjectToken(t, priv, claims)` helper if one exists, or factor it. Mark the test `t.Skip` with a clear TODO if the test infrastructure for token-exchange isn't ready in v0.8.0 — the unit-level coverage in `internal/auth/token_test.go` is the primary safety net.

- [ ] **Step 4: Run + commit**

Run: `go test ./internal/e2e/ -run "TestE2E_CrossTenantIsolation|TestE2E_TrustedKeyFlagDisabled|TestE2E_TokenExchange" -v`
Expected: PASS.

```bash
git add internal/e2e/
git commit -m "test(e2e): cross-tenant + feature-flag + token-exchange invariant

Locks the documented divergences and security properties:
- Cross-tenant register collision returns 409; B's list is empty.
- TrustedKeyRegistrationEnabled=false: trusted-key paths return 404
  FEATURE_DISABLED; keypair paths unaffected.
- Token-exchange: A's M2M client + subject signed by A's trusted key
  -> success; B's M2M client claiming caas_org_id=A -> rejected.

Refs #281."
```


---

## Phase 9: Per-divergence regression tests

### Task 31: Per-divergence regression-lock tests in adapter layer

**Files:**
- Modify: `internal/domain/account/keys_adapter_test.go`
- Modify: `internal/domain/account/trusted_adapter_test.go`

The spec §3.2 documents 10 divergences from cloud. Each gets one regression-lock test so a future contributor "fixing toward cloud" hits a failing test.

- [ ] **Step 1: Add regression-locks (one test each)**

Append to `keys_adapter_test.go` and `trusted_adapter_test.go` the following short assertions. Each is named `TestRegression_<DivergenceSlug>`:

```go
// §3.2 #2 — ROLE_ADMIN only (not ADMIN ∨ SUPER_USER).
func TestRegression_RoleGate_RoleAdminOnly(t *testing.T) {
	h := account.New(nil, nil, auth.NewInMemoryKeyStore(), auth.NewInMemoryTrustedKeyStore(), auth.DefaultIAMConfig())
	uc := &spi.UserContext{UserID: "u", UserName: "u", Tenant: spi.Tenant{ID: "t1"}, Roles: []string{"SUPER_USER"}}
	req := httptest.NewRequest("GET", "/", nil).WithContext(spi.WithUserContext(httptest.NewRequest("GET", "/", nil).Context(), uc))
	w := httptest.NewRecorder()
	params := genapi.GetCurrentJwtKeyPairParams{Audience: "client"}
	h.GetCurrentJwtKeyPair(w, req, params)
	if w.Code != http.StatusForbidden {
		t.Errorf("SUPER_USER should not be admitted; want 403, got %d", w.Code)
	}
}

// §3.2 #3 — cross-tenant lifecycle returns 404 (not 403).
// Already covered by TestDeleteTrustedKey_CrossTenant_404; no extra test needed here.

// §3.2 #4 — trusted-key audience honored on round-trip (cloud always returns human).
func TestRegression_TrustedAudienceRoundTrip(t *testing.T) {
	ts := auth.NewInMemoryTrustedKeyStore()
	iam := auth.DefaultIAMConfig()
	iam.TrustedKeyRegistrationEnabled = true
	h := account.New(nil, nil, auth.NewInMemoryKeyStore(), ts, iam)
	body, _ := json.Marshal(genapi.RegisterTrustedKeyRequestDto{KeyId: "k1", Jwk: rsaJWK(t, "k1"), Audience: "client"})
	req := httptest.NewRequest("POST", "/", bytes.NewReader(body)).WithContext(spi.WithUserContext(httptest.NewRequest("POST", "/", nil).Context(), adminCtx()))
	w := httptest.NewRecorder()
	h.RegisterTrustedKey(w, req)
	var resp genapi.TrustedKeyResponseDto
	json.Unmarshal(w.Body.Bytes(), &resp)
	if string(resp.Audience) != "client" {
		t.Errorf("audience should round-trip 'client', got %q (cloud would coerce to human)", resp.Audience)
	}
}

// §3.2 #5 — reactivate requires fresh validTo (cloud clears to nil).
// Already covered by TestReactivateJwtKeyPair_RequiresValidTo.

// §3.2 #6 — strict validation: validTo<validFrom, gracePeriodSec<0 rejected.
func TestRegression_StrictValidation_GracePeriodNegative(t *testing.T) {
	ks := auth.NewInMemoryKeyStore()
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	kp := &auth.KeyPair{KID: "k", Audience: "client", Algorithm: "RS256", PublicKey: &priv.PublicKey, PrivateKey: priv, Active: true, ValidFrom: time.Now()}
	_ = ks.Save(kp, auth.RotateOptions{})
	h := account.New(nil, nil, ks, auth.NewInMemoryTrustedKeyStore(), auth.DefaultIAMConfig())
	body := []byte(`{"gracePeriodSec":-5}`)
	req := httptest.NewRequest("POST", "/", bytes.NewReader(body)).WithContext(spi.WithUserContext(httptest.NewRequest("POST", "/", nil).Context(), adminCtx()))
	w := httptest.NewRecorder()
	h.InvalidateJwtKeyPair(w, req, "k")
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for negative grace (cloud accepts silently), got %d", w.Code)
	}
}

// §3.2 #7 — gracePeriodSec default 0 (cloud defaults 3600).
func TestRegression_GraceDefaultZero(t *testing.T) {
	ks := auth.NewInMemoryKeyStore()
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	now := time.Now()
	kp := &auth.KeyPair{KID: "k", Audience: "client", Algorithm: "RS256", PublicKey: &priv.PublicKey, PrivateKey: priv, Active: true, ValidFrom: now}
	_ = ks.Save(kp, auth.RotateOptions{})
	h := account.New(nil, nil, ks, auth.NewInMemoryTrustedKeyStore(), auth.DefaultIAMConfig())
	// Empty body -> grace=0 -> immediate invalidation
	req := httptest.NewRequest("POST", "/", bytes.NewReader([]byte(`{}`))).WithContext(spi.WithUserContext(httptest.NewRequest("POST", "/", nil).Context(), adminCtx()))
	w := httptest.NewRecorder()
	h.InvalidateJwtKeyPair(w, req, "k")
	got, _ := ks.Get("k")
	if got.ValidTo == nil || !got.ValidTo.Before(now.Add(2*time.Second)) {
		t.Errorf("expected ValidTo ~now (grace=0), got %v (cloud default 3600s would be future)", got.ValidTo)
	}
}

// §3.2 #8 — same-tenant silent upsert (cloud delete-and-replace).
func TestRegression_SameTenantSilentUpsert(t *testing.T) {
	ts := auth.NewInMemoryTrustedKeyStore()
	iam := auth.DefaultIAMConfig()
	iam.TrustedKeyRegistrationEnabled = true
	h := account.New(nil, nil, auth.NewInMemoryKeyStore(), ts, iam)
	for i := 0; i < 2; i++ {
		body, _ := json.Marshal(genapi.RegisterTrustedKeyRequestDto{KeyId: "k", Jwk: rsaJWK(t, "k"), Audience: "human"})
		req := httptest.NewRequest("POST", "/", bytes.NewReader(body)).WithContext(spi.WithUserContext(httptest.NewRequest("POST", "/", nil).Context(), adminCtx()))
		w := httptest.NewRecorder()
		h.RegisterTrustedKey(w, req)
		if w.Code != http.StatusCreated && w.Code != http.StatusOK {
			t.Errorf("re-register %d: status=%d (expected silent upsert)", i, w.Code)
		}
	}
}

// §3.2 #10 — non-RS256 algorithm rejected (cloud signs all enum values).
// Already covered by TestIssueJwtKeyPair_RejectsNonRS256.
```

- [ ] **Step 2: Run + commit**

Run: `go test ./internal/domain/account/ -run TestRegression -v`
Expected: PASS.

```bash
git add internal/domain/account/keys_adapter_test.go internal/domain/account/trusted_adapter_test.go
git commit -m "test(account): regression-lock tests for §3.2 divergences

One assertion per documented divergence so a future contributor
'fixing toward cloud' hits a failing test:
- ROLE_ADMIN only (SUPER_USER not admitted)
- Trusted-key audience round-trip (not coerced to human)
- Negative gracePeriodSec rejected (cloud accepts silently)
- gracePeriodSec default 0 (cloud default 3600)
- Same-tenant silent upsert (cloud delete-and-replace)

Refs #281."
```

---

## Phase 10: Documentation

### Task 32: Six new error help topics

**Files:**
- Create: 6 files in `cmd/cyoda/help/content/errors/`

- [ ] **Step 1: Inspect template**

Run: `cat cmd/cyoda/help/content/errors/TRUSTED_KEY_NOT_FOUND.md`

- [ ] **Step 2: Write each topic — `FEATURE_DISABLED.md`**

```markdown
---
topic: errors.FEATURE_DISABLED
title: "FEATURE_DISABLED — feature is not enabled in this deployment"
stability: stable
see_also:
  - errors
  - config.auth
---

# errors.FEATURE_DISABLED

## NAME

FEATURE_DISABLED — the requested operation belongs to an optional feature that is not enabled in this deployment.

## SYNOPSIS

HTTP: `404` `Not Found`. Retryable: `no`.

## DESCRIPTION

Returned by trusted-key admin endpoints when `cyoda.security.web.iam.trustedKeyRegistrationEnabled` is `false` (the default):

- `GET /oauth/keys/trusted`
- `POST /oauth/keys/trusted`
- `DELETE /oauth/keys/trusted/{keyId}`
- `POST /oauth/keys/trusted/{keyId}/invalidate`
- `POST /oauth/keys/trusted/{keyId}/reactivate`

To enable, set `CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED=true` and restart. Keypair endpoints (`/oauth/keys/keypair/*`) are unaffected.

Not retryable. Operator action required.

## SEE ALSO

- errors
- config.auth
```

- [ ] **Step 3: Write `UNSUPPORTED_ALGORITHM.md`**

```markdown
---
topic: errors.UNSUPPORTED_ALGORITHM
title: "UNSUPPORTED_ALGORITHM — requested JWT algorithm is not supported"
stability: stable
see_also:
  - errors
---

# errors.UNSUPPORTED_ALGORITHM

## NAME

UNSUPPORTED_ALGORITHM — the requested JWT signing algorithm is not implemented in this version.

## SYNOPSIS

HTTP: `400` `Bad Request`. Retryable: `no`.

## DESCRIPTION

cyoda-go v0.8.0 supports only `RS256` for JWT signing keypairs. Other enum values declared in the OpenAPI spec (`RS384`, `RS512`, `PS256`, `PS384`, `PS512`, `ES256`, `ES384`, `ES512`, `EdDSA`) are not yet implemented; requests specifying them return 400 `UNSUPPORTED_ALGORITHM`.

Cyoda Cloud supports the full enum. Parity with cloud for the remaining algorithms is tracked in a v0.8.1 follow-up.

Not retryable. Use `algorithm: RS256` or omit the field (defaults to RS256).

## SEE ALSO

- errors
```

- [ ] **Step 4: Write `UNSUPPORTED_KEY_TYPE.md`**

```markdown
---
topic: errors.UNSUPPORTED_KEY_TYPE
title: "UNSUPPORTED_KEY_TYPE — JWK key type is not supported"
stability: stable
see_also:
  - errors
  - errors.UNSUPPORTED_ALGORITHM
---

# errors.UNSUPPORTED_KEY_TYPE

## NAME

UNSUPPORTED_KEY_TYPE — the JWK `kty` is not supported by this cyoda-go version.

## SYNOPSIS

HTTP: `400` `Bad Request`. Retryable: `no`.

## DESCRIPTION

`POST /oauth/keys/trusted` accepts only RSA public keys (`kty: "RSA"`) in v0.8.0. Cloud also supports `kty: "EC"` and `kty: "OKP"` (EdDSA); cyoda-go parity is tracked in a v0.8.1 follow-up.

Not retryable. Convert the public key to RSA, or wait for the v0.8.1 release.

## SEE ALSO

- errors
- errors.UNSUPPORTED_ALGORITHM
```

- [ ] **Step 5: Write `KEY_OWNED_BY_DIFFERENT_TENANT.md`**

```markdown
---
topic: errors.KEY_OWNED_BY_DIFFERENT_TENANT
title: "KEY_OWNED_BY_DIFFERENT_TENANT — trusted-key registration collides with another tenant"
stability: stable
see_also:
  - errors
  - errors.TRUSTED_KEY_NOT_FOUND
---

# errors.KEY_OWNED_BY_DIFFERENT_TENANT

## NAME

KEY_OWNED_BY_DIFFERENT_TENANT — the requested `keyId` is already registered by another tenant.

## SYNOPSIS

HTTP: `409` `Conflict`. Retryable: `no`.

## DESCRIPTION

Trusted keys are tenant-scoped: each tenant has its own namespace for `keyId`. When `POST /oauth/keys/trusted` is called with a `keyId` that already belongs to a different tenant, the request is rejected with `409`.

Not retryable. Pick a fresh `keyId` (the caller cannot see or affect another tenant's keys).

## SEE ALSO

- errors
- errors.TRUSTED_KEY_NOT_FOUND
```

- [ ] **Step 6: Write `KEYPAIR_NOT_FOUND.md`**

```markdown
---
topic: errors.KEYPAIR_NOT_FOUND
title: "KEYPAIR_NOT_FOUND — referenced signing keypair does not exist"
stability: stable
see_also:
  - errors
  - errors.NOT_FOUND
---

# errors.KEYPAIR_NOT_FOUND

## NAME

KEYPAIR_NOT_FOUND — the requested JWT signing keypair is not present.

## SYNOPSIS

HTTP: `404` `Not Found`. Retryable: `no`.

## DESCRIPTION

Returned by:

- `DELETE /oauth/keys/keypair/{keyId}` — keyId not present.
- `POST /oauth/keys/keypair/{keyId}/invalidate` — keyId not present.
- `POST /oauth/keys/keypair/{keyId}/reactivate` — keyId not present.
- `GET /oauth/keys/keypair/current?audience=X` — no active key for audience X.

Not retryable. Verify via the issue endpoint or check the bootstrap-key audience configuration (`CYODA_JWT_BOOTSTRAP_AUDIENCE`).

## SEE ALSO

- errors
- errors.NOT_FOUND
```

- [ ] **Step 7: Write `TRUSTED_KEY_CAP_REACHED.md`**

```markdown
---
topic: errors.TRUSTED_KEY_CAP_REACHED
title: "TRUSTED_KEY_CAP_REACHED — tenant trusted-key cap exhausted"
stability: stable
see_also:
  - errors
  - config.auth
---

# errors.TRUSTED_KEY_CAP_REACHED

## NAME

TRUSTED_KEY_CAP_REACHED — the tenant has registered the maximum allowed trusted keys.

## SYNOPSIS

HTTP: `400` `Bad Request`. Retryable: `no`.

## DESCRIPTION

`POST /oauth/keys/trusted` enforces a per-tenant cap (default 10, configurable via `CYODA_IAM_TRUSTED_KEY_MAX_PER_TENANT`). The cap counts only currently-valid keys (Active and not past `validTo`).

To make room, delete or invalidate older keys, or raise the cap via the env var and restart.

Not retryable.

## SEE ALSO

- errors
- config.auth
```

- [ ] **Step 8: Commit**

```bash
git add cmd/cyoda/help/content/errors/FEATURE_DISABLED.md cmd/cyoda/help/content/errors/UNSUPPORTED_ALGORITHM.md cmd/cyoda/help/content/errors/UNSUPPORTED_KEY_TYPE.md cmd/cyoda/help/content/errors/KEY_OWNED_BY_DIFFERENT_TENANT.md cmd/cyoda/help/content/errors/KEYPAIR_NOT_FOUND.md cmd/cyoda/help/content/errors/TRUSTED_KEY_CAP_REACHED.md
git commit -m "docs(help): 6 new error topics for /oauth/keys/* conformance

FEATURE_DISABLED, UNSUPPORTED_ALGORITHM, UNSUPPORTED_KEY_TYPE,
KEY_OWNED_BY_DIFFERENT_TENANT, KEYPAIR_NOT_FOUND, TRUSTED_KEY_CAP_REACHED.

Refs #281."
```

---

### Task 33: Update errors.md catalogue + existing TRUSTED_KEY_NOT_FOUND + NOT_FOUND

**Files:**
- Modify: `cmd/cyoda/help/content/errors.md`
- Modify: `cmd/cyoda/help/content/errors/TRUSTED_KEY_NOT_FOUND.md`
- Modify: `cmd/cyoda/help/content/errors/NOT_FOUND.md`

- [ ] **Step 1: Append 6 codes to the catalogue table in `errors.md`**

Run: `grep -n "TRUSTED_KEY_NOT_FOUND\|UNAUTHORIZED" cmd/cyoda/help/content/errors.md | head`

Identify the catalogue table block. Add (alphabetical):

```
| FEATURE_DISABLED | 404 | Optional feature is disabled in this deployment |
| KEY_OWNED_BY_DIFFERENT_TENANT | 409 | Trusted-key registration collides with another tenant |
| KEYPAIR_NOT_FOUND | 404 | Referenced signing keypair does not exist |
| TRUSTED_KEY_CAP_REACHED | 400 | Per-tenant trusted-key cap exhausted |
| UNSUPPORTED_ALGORITHM | 400 | Requested JWT algorithm not supported |
| UNSUPPORTED_KEY_TYPE | 400 | JWK kty not supported |
```

- [ ] **Step 2: Update `TRUSTED_KEY_NOT_FOUND.md` DESCRIPTION**

Edit — append to the existing DESCRIPTION section:

```markdown
Returned uniformly for kids that don't exist AND kids owned by another tenant; the response does not distinguish — by design, to prevent cross-tenant existence enumeration.
```

- [ ] **Step 3: Update `NOT_FOUND.md` SEE ALSO**

Add `errors.KEYPAIR_NOT_FOUND` to the see_also block of `cmd/cyoda/help/content/errors/NOT_FOUND.md` for keypair-specific 404s.

- [ ] **Step 4: Commit**

```bash
git add cmd/cyoda/help/content/errors.md cmd/cyoda/help/content/errors/TRUSTED_KEY_NOT_FOUND.md cmd/cyoda/help/content/errors/NOT_FOUND.md
git commit -m "docs(help): wire 6 new error codes into errors.md catalogue

TRUSTED_KEY_NOT_FOUND DESCRIPTION updated to document the cross-tenant
404 case (no existence leakage). NOT_FOUND.md gets cross-ref to
KEYPAIR_NOT_FOUND for keypair-specific 404s.

Refs #281."
```

---

### Task 34: `config/auth.md` updates — 5 new env vars + IAM features subsection

**Files:**
- Modify: `cmd/cyoda/help/content/config/auth.md`

- [ ] **Step 1: Add `CYODA_JWT_BOOTSTRAP_AUDIENCE` under JWT mode**

After the existing JWT mode env vars (around line 53), add:

```markdown
- `CYODA_JWT_BOOTSTRAP_AUDIENCE` — audience assigned to the bootstrap signing
  key derived from `CYODA_JWT_SIGNING_KEY`. Must be `client` or `human`.
  The M2M token-issuance path (`POST /oauth/token`) always uses the
  client-audience key. (default: `client`)
```

- [ ] **Step 2: Add new "IAM features" section after the JWT mode section**

```markdown
### IAM features

These environment variables tune the IAM admin endpoints under `/oauth/keys/*`.

- `CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED` — gates all 5 endpoints under
  `/oauth/keys/trusted/*`. When `false`, every trusted-key endpoint returns
  `404 FEATURE_DISABLED`. (default: `false`)
- `CYODA_IAM_TRUSTED_KEY_MAX_PER_TENANT` — per-tenant cap on registered
  trusted keys. Counts only currently-valid keys (active and not past
  `validTo`). `0` means unbounded. (default: `10`)
- `CYODA_IAM_TRUSTED_KEY_MAX_VALIDITY_DAYS` — default validity for trusted
  keys when the registration request omits `validTo`. No clamp on
  user-supplied `validTo` values. (default: `365`)
- `CYODA_IAM_TRUSTED_KEY_MAX_JWK_PROPERTIES` — caps the number of properties
  in a registered JWK to guard against absurdly large payloads. (default: `20`)
- `CYODA_IAM_KEYPAIR_DEFAULT_VALIDITY_DAYS` — default validity for both the
  bootstrap signing key and runtime-issued keypairs via
  `POST /oauth/keys/keypair`. The startup banner emits a `WARN` if the
  active bootstrap key expires within 30 days. (default: `365`)
```

- [ ] **Step 3: Add "JWT signing keypair rotation" subsection**

```markdown
### JWT signing keypair rotation

The bootstrap signing key derived from `CYODA_JWT_SIGNING_KEY` (or
`CYODA_JWT_SIGNING_KEY_FILE`) is the default signing key for the
`POST /oauth/token` flow. Its KID is deterministic across nodes sharing
the same PEM (SHA-256 of the public key).

Operators can rotate signing keys at runtime via
`POST /oauth/keys/keypair` (with `algorithm: RS256` and `audience: client`),
optionally setting `invalidateCurrent: true` and
`invalidateGracePeriodSec: N` to overlap the old and new keys.

v0.8.0 limitation: runtime-issued keypairs are held in memory only;
they do not survive process restart. The bootstrap key survives because
its KID is derived deterministically from the PEM input. Persisted
signing-key storage is tracked in a follow-up issue.

Bootstrap keys are saved with a finite `validTo` (default 365 days).
After expiry the M2M token-issuance path will return `404 KEYPAIR_NOT_FOUND`
for `getCurrentJwtKeyPair?audience=client`. Operators should monitor
the startup WARN and rotate before expiry.
```

- [ ] **Step 4: Append EXAMPLES block for the trusted-key flag**

```markdown
**With trusted-key registration enabled:**

```
CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED=true
CYODA_IAM_TRUSTED_KEY_MAX_PER_TENANT=10
```
```

- [ ] **Step 5: Commit**

```bash
git add cmd/cyoda/help/content/config/auth.md
git commit -m "docs(help): config/auth.md — IAM features section + 5 new env vars

CYODA_JWT_BOOTSTRAP_AUDIENCE under JWT mode; new IAM features section
with the 4 trusted-key knobs + KEYPAIR_DEFAULT_VALIDITY_DAYS; new
JWT signing keypair rotation subsection (bootstrap vs runtime; v0.8.0
persistence limitation; finite validity).

Refs #281."
```

---

### Task 35: `openapi.md` line 96 + audit table + README

**Files:**
- Modify: `cmd/cyoda/help/content/openapi.md`
- Modify: `docs/superpowers/audits/2026-04-29-openapi-conformance-audit.md`
- Modify: `README.md`

- [ ] **Step 1: `openapi.md:96`**

Find line 96 (the IAM bullet) and update to:

```
- **IAM** — OAuth token, key management under `/oauth/`. As of v0.8.0, the 10 `/oauth/keys/*` admin operations (keypair + trusted-key lifecycle) are conformant. OIDC providers under `/oauth/providers/*` remain 501 until v0.9.0.
```

- [ ] **Step 2: Audit table — 10 row dispositions**

Edit `docs/superpowers/audits/2026-04-29-openapi-conformance-audit.md` lines 122-131. For each row, change the disposition column from:

```
out-of-scope-not-implemented (#194)
```

to:

```
match
```

Leave the commit-hash column to be filled at merge time (it'll be the merge commit).

- [ ] **Step 3: README config table**

Find the env-var table in `README.md` (likely under a "Configuration" heading). Add 5 rows (alphabetical):

```
| `CYODA_IAM_KEYPAIR_DEFAULT_VALIDITY_DAYS` | `365` | Default validity for runtime + bootstrap keypairs |
| `CYODA_IAM_TRUSTED_KEY_MAX_JWK_PROPERTIES` | `20` | Max property count on registered JWKs |
| `CYODA_IAM_TRUSTED_KEY_MAX_PER_TENANT` | `10` | Per-tenant cap on currently-valid trusted keys (0 = unbounded) |
| `CYODA_IAM_TRUSTED_KEY_MAX_VALIDITY_DAYS` | `365` | Default validity for trusted keys |
| `CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED` | `false` | Gate all `/oauth/keys/trusted/*` endpoints |
| `CYODA_JWT_BOOTSTRAP_AUDIENCE` | `client` | Audience for the bootstrap signing key |
```

- [ ] **Step 4: Commit**

```bash
git add cmd/cyoda/help/content/openapi.md docs/superpowers/audits/2026-04-29-openapi-conformance-audit.md README.md
git commit -m "docs: openapi.md clarification + audit dispositions + README config

openapi.md: clarify that the 10 /oauth/keys/* ops are conformant in
v0.8.0 (OIDC providers still 501).
audit table: 10 rows -> 'match' (merge commit hash to be added at
merge time).
README: 6 new env-var rows.

Refs #281."
```

---

### Task 36: Release notes additions

**Files:**
- Modify: `CHANGELOG.md` (or wherever release notes live — confirm via `ls`)

- [ ] **Step 1: Identify the release-notes file**

Run: `ls CHANGELOG* RELEASE* docs/RELEASE_NOTES* 2>/dev/null && grep -rn "v0.8.0" CHANGELOG.md docs/ 2>/dev/null | head -5`

- [ ] **Step 2: Append the 5 v0.8.0 entries**

Under the v0.8.0 section (or create one if missing), add:

```markdown
### Changes

- **IAM admin endpoints conform to OpenAPI** — the 10 `/oauth/keys/*` operations (keypair + trusted-key lifecycle) now respond with the OpenAPI-declared DTOs through the chi router. The legacy `/oauth/keys/` prefix mux is removed.
- **Algorithm scope (v0.8.0)** — only `RS256` signs/verifies. Other algorithm enum values (`RS384`, `RS512`, `PS256`, `PS384`, `PS512`, `ES256`, `ES384`, `ES512`, `EdDSA`) are accepted by the OpenAPI surface but rejected with `400 UNSUPPORTED_ALGORITHM`. Trusted-key registration accepts only `kty=RSA` JWKs (`kty=EC`/`OKP` rejected with `400 UNSUPPORTED_KEY_TYPE`). A follow-up issue tracks adding the rest.
- **Trusted-key registration is disabled by default** — set `CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED=true` to enable. Customers using `/oauth/keys/trusted/*` through the legacy mux must opt in.
- **Bootstrap signing key now has finite validity** — defaults to 365 days (configurable via `CYODA_IAM_KEYPAIR_DEFAULT_VALIDITY_DAYS`). Long-running deployments must rotate before expiry; the startup banner emits a `WARN` if the active key expires within 30 days.
- **Reactivate semantics changed** — `POST /oauth/keys/{keypair,trusted}/{keyId}/reactivate` now requires a `ReactivateKeyRequestDto` body with `validTo` (must be > now and > validFrom). Cyoda Cloud's behaviour of clearing `validTo` to nil (zombie key) is intentionally not adopted; see the spec for details.

### Known limitations

- **Runtime-issued signing keypairs are lost on process restart.** The bootstrap key survives (its KID is deterministic per PEM). Persistent signing-key storage is tracked in a follow-up.
- **Pre-v0.8.0 trusted-key KV entries are orphaned.** They use a different key prefix (`trustedkey:<kid>`); v0.8.0 uses `trustedkey:<tenantID>:<kid>` and does not query the old shape. Operators must re-register affected keys. Inspection: `grep "^trustedkey:[^:]*$" <kvdump>`. cyoda-go has no known production users on this surface.
- **v0.8.0 → pre-v0.8.0 rollback hazard.** Trusted keys created under v0.8.0 are visible to pre-v0.8.0 binaries as mangled-kid entries (`<tenantID>:<kid>` treated as kid). Purge out-of-band before rollback if visibility matters.
```

- [ ] **Step 3: Commit**

```bash
git add CHANGELOG.md
git commit -m "docs(release): v0.8.0 release notes — /oauth/keys/* conformance

Five operational notes plus three known limitations. Covers algorithm
scope, trusted-key flag default, bootstrap key finite validity,
reactivate semantics, runtime-key restart loss, KV orphan story,
rollback hazard.

Refs #281."
```


---

## Phase 11: Final verification + follow-up

### Task 37: Full test suite + vet

**Files:** none modified.

- [ ] **Step 1: Run unit tests**

Run: `go test -short ./... -v`
Expected: PASS — every package green. Failures must be triaged before continuing.

- [ ] **Step 2: Run E2E**

Run: `go test ./internal/e2e/... -v`
Expected: PASS (requires Docker for the testcontainers PostgreSQL fixture). If Docker isn't available locally, document the gap and run on CI.

- [ ] **Step 3: Run vet**

Run: `go vet ./...`
Expected: clean output.

- [ ] **Step 4: Run plugin tests**

Run: `make test-all`
Expected: PASS — root + `plugins/memory|sqlite|postgres`.

If any plugin test fails (e.g. the parity registry surfaces a new test that fails on Cassandra), triage:
- If the failure is a real regression caused by this PR, fix.
- If it's pre-existing, document and continue.

- [ ] **Step 5: Commit if any fixups landed**

```bash
git add -A
git commit -m "test: fixups from full-suite verification"
```

---

### Task 38: Race detector single run (pre-PR sanity)

**Files:** none modified.

- [ ] **Step 1: Run race detector**

Run: `go test -race ./...`
Expected: PASS, no race reports.

Per `.claude/rules/race-testing.md`, this is the one-shot pre-PR sanity check, NOT a per-step gate. If races surface, fix and re-run; don't sprinkle `-race` across earlier task verifications.

- [ ] **Step 2: Commit fixups if needed**

```bash
git add -A
git commit -m "fix: race detector fixups from pre-PR run"
```

---

### Task 39: File v0.8.1 follow-up issue

**Files:** none modified.

- [ ] **Step 1: Compose follow-up issue body**

Title: `feat(iam): multi-algorithm JWT signing + non-RSA JWK kty support — v0.8.1`

Body (target as Markdown; convert per project convention before posting):

```markdown
## Goal

Extend cyoda-go's JWT signing and trusted-key verification surface to the full
algorithm enum declared in `api/openapi.yaml`:
- Sign + verify: `RS384`, `RS512`, `PS256`, `PS384`, `PS512`, `ES256`, `ES384`,
  `ES512`, `EdDSA` (in addition to existing `RS256`).
- Trusted-key registration: `kty=EC` and `kty=OKP` JWKs.

## Context

#281 (released in v0.8.0) ships the 10 `/oauth/keys/*` operations as
OpenAPI-conformant adapters but rejects every algorithm and JWK kty other than
RS256/RSA at the adapter boundary (400 `UNSUPPORTED_ALGORITHM` /
`UNSUPPORTED_KEY_TYPE`). This was a deliberate scope deferral: `internal/auth/jwt.go`
was untouched; multi-algorithm dispatch is real new code (per-algorithm
signing-method registry, ECDSA + Ed25519 generators, JWK encoding/decoding for
EC and OKP curves, golden-vector test fixtures per algorithm).

Cyoda Cloud supports the full enum — see `JwtKeyPairUtil` and
`TrustedKeyRegistrationService` in the Kotlin reference. Parity is the goal.

## In scope

- `internal/auth/jwt.go` — replace the RS256-hardcoded Sign/Verify with a
  signing-method registry keyed on `KeyPair.Algorithm`. Validation surface
  (`EnsureAlgRS256` -> generic alg check) updated.
- `internal/auth/keys.go` (will be reintroduced or absorbed) or a new
  `internal/auth/keygen.go` — ECDSA P-256/P-384/P-521 + Ed25519 generators.
- `internal/auth/keyvalidation.go` — JWK decode for `kty=EC` (crv/x/y) and
  `kty=OKP` (crv/x) following RFC 7517 / RFC 8037.
- Adapter changes: remove the `UNSUPPORTED_ALGORITHM` and `UNSUPPORTED_KEY_TYPE`
  early-reject branches; the store accepts the broader algorithm/kty space.
- Help topic updates: `UNSUPPORTED_ALGORITHM.md` and `UNSUPPORTED_KEY_TYPE.md`
  mark as deprecated (kept for backwards compat in error responses if any
  configuration variant still rejects).
- Golden-vector tests per algorithm (sign + verify + JWK round-trip).

## Acceptance

- All 10 algorithm enum values produce a valid signing key when issued; round-trip
  signs a sample claim and verifies via JWKS.
- All 3 JWK `kty` values (`RSA`, `EC`, `OKP`) register and verify.
- E2E `TestE2E_TokenExchange` extended with EC and Ed25519 trusted keys.
- `UNSUPPORTED_ALGORITHM` and `UNSUPPORTED_KEY_TYPE` error responses are no
  longer emitted by the adapters under normal operation.
```

- [ ] **Step 2: File the issue via `gh`**

```bash
gh issue create --title "feat(iam): multi-algorithm JWT signing + non-RSA JWK kty support — v0.8.1" --body-file /tmp/issue-body.md --milestone v0.8.1
```

Where `/tmp/issue-body.md` contains the body above (write it to a tmp file first).

- [ ] **Step 3: Note the issue number in the spec**

After the issue is created, add a one-line note to the spec (rev4) under §12 "Out of scope" referencing the issue number.

```bash
git add docs/superpowers/specs/2026-06-04-281-oauth-keys-openapi-design.md
git commit -m "docs(spec): record v0.8.1 multi-algorithm follow-up issue number"
```

---

## Self-Review Checklist

After all 39 tasks land, run through this:

- [ ] Every spec §3.1 in-scope item has at least one task that implements it.
- [ ] Every spec §3.2 divergence has a regression-lock test (Task 31).
- [ ] Every spec §5.1 new error code has a help topic (Task 32).
- [ ] Every spec §5.3 input-validation row is exercised by an adapter test.
- [ ] No leftover `requireAdmin` (lowercase) callsites.
- [ ] No leftover `Save(kp)` (single-arg) or `Reactivate(kid)` (single-arg) callsites.
- [ ] No leftover `mux.Handle("/oauth/keys/", ...)`.
- [ ] No `// TODO(#281)` comments left in source.
- [ ] `api/generated.go` is the regenerated version (matches `api/openapi.yaml`).
- [ ] `go test -race ./...` passes once (pre-PR).
- [ ] The PR description includes the spec rev4 link, the audit-table update, and the follow-up issue link.

---

## Out-of-scope (do not implement in this PR)

- ES* / EdDSA signing + verification (follow-up issue from Task 39).
- `kty=EC` / `kty=OKP` JWK acceptance (same follow-up).
- Persistent signing-key storage (#194 §3.5 follow-up).
- M2M client-store persistence (#194 §3.6, picked up by #194-B).
- `/clients` OpenAPI conformance (#194-B).
- `accountSubscriptionsGet` (#194-C).
- OIDC providers subsystem (#194-D, v0.9.0+).
- Periodic prune of past-`ValidTo` trusted-key entries.
- Cleanup of orphan pre-v0.8.0 `trustedkey:<kid>` KV entries.
- Same-tenant idempotent re-register atomic delete-and-replace (cyoda-go preserves silent upsert).
