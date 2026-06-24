# #281 — OpenAPI Conformance for `/oauth/keys/*` Implementation Plan (rev5)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the 10 stub `/oauth/keys/*` handlers with OpenAPI-conformant adapters routed through chi, add per-tenant scoping for trusted keys, and reconcile behaviour with Cyoda Cloud where reachable.

**Architecture:** Two new adapter files (`keys_adapter.go`, `trusted_adapter.go`) in `internal/domain/account/` host the 10 generated `ServerInterface` methods. Stores gain audience partitioning (keypair) and tenant scoping (trusted key) with lazy `ValidTo` filtering. Existing `internal/auth/{keys,trusted}.go` HTTP handlers and the `mux.Handle("/oauth/keys/", ...)` entry are removed. `internal/auth/jwt.go` is NOT modified — RS256-only signing stays; the 9 other algorithm enum values are rejected at adapter boundary (deferred to v0.8.1).

**Tech Stack:** Go 1.26, chi router, oapi-codegen v2.7.0 (std-http-server), `log/slog`, `crypto/rsa` + `crypto/x509`, RFC 9457 ProblemDetail wire shape via `common.WriteError`.

**Spec:** `docs/superpowers/specs/2026-06-04-281-oauth-keys-openapi-design.md` (rev4).

**Plan revision:** rev5 — folds in 34 findings from four fresh-context reviews of rev1.

---

## Ground truth the executor must accept

These are non-negotiable facts about the existing codebase. Do not invent alternatives.

1. **Existing `app.IAMConfig`** at `app/config.go:67-79` carries IAM mode + JWT settings. To avoid name collision, this plan introduces **`auth.IAMFeatures`** (not `auth.IAMConfig`) for the new fields.
2. **Env helper names**: `envString`, `envInt`, `envBool`, `envDuration` (in `app/config.go`). NOT `getStringEnv`/`getIntEnv` etc.
3. **ProblemDetail wire shape**: `errorCode` lives under `properties` in the JSON body. Use `commontest.ExpectErrorCode(t, resp, "CODE")` from `internal/common/commontest/problemdetail.go`. NEVER `pd["errorCode"]` on a raw map.
4. **SPI KeyValueStore API**: `List(ctx, namespace string) (map[string][]byte, error)`; `Get(ctx, namespace, key string) ([]byte, error)`; `Put(ctx, namespace, key, value)`; `Delete(ctx, namespace, key)`. The existing namespace is `trustedKeysNamespace = "trusted-keys"`. Tenant scoping is achieved by changing the KEY (not the namespace) from `<kid>` to `<tenantID>:<kid>`.
5. **Memory KV construction**: `factory := memory.NewStoreFactory(memory.Config{}); kv, _ := factory.KeyValueStore(ctx)`. NOT `memory.NewKeyValueStore()`.
6. **Generated DTO timestamp shapes**: `format: date-time` → `time.Time` (required) or `*time.Time` (optional). NOT `openapi_types.Date{Time: t}`. `openapi_types.Date` is for `format: date` only (no time component).
7. **std-http-server is body-agnostic**: adding `requestBody` to a path in OpenAPI does NOT change the `ServerInterface` method signature. Reactivate handlers stay `(w, r, keyId)`; the body is decoded in-adapter via `boundedJSONDecode`.
8. **JWKS source**: `internal/auth/jwks.go:42` does `h.keyStore.List()`. That's the only site that needs to switch to `ListForVerification()`.
9. **Bootstrap key save**: lives at `internal/auth/service.go:61-70`, inside `NewAuthService`. NOT in `app/app.go`.
10. **`auth.AuthService.TrustedKeyStore()` accessor already exists** at `service.go:127`. Do NOT re-add.
11. **`auth.IAMFeatures` is value-copied, not interface-shared.** Per `.claude/rules/ownership-mutability.md`, immutable config is value-copied; stores are shared via interfaces.

---

## File Map

**New files:**
- `internal/auth/iam_features.go` — `IAMFeatures` value struct + Default + Validate
- `internal/auth/iam_features_test.go`
- `internal/auth/keyvalidation.go` — moved validators with exported names
- `internal/auth/verification.go` — `getTrustedKeyByKID` in-package helper
- `internal/auth/keypair_signing_test.go` — RS256 sign+verify table-driven; all 9 other enum values rejected at adapter
- `internal/domain/account/keys_adapter.go` — 5 keypair adapter methods + helpers
- `internal/domain/account/keys_adapter_test.go`
- `internal/domain/account/trusted_adapter.go` — 5 trusted-key adapter methods + helpers
- `internal/domain/account/trusted_adapter_test.go`
- `internal/domain/account/io.go` — `boundedJSONDecode`
- `internal/domain/account/io_test.go`
- `internal/e2e/oauth_keys_test.go`
- 6 new help topics under `cmd/cyoda/help/content/errors/`

**Deleted files (in Phase 2):**
- `internal/auth/keys.go`
- `internal/auth/trusted.go` (validators moved to `keyvalidation.go` first)

**Modified files** (highlights — full list in each task):
- `internal/auth/{store,service,admin_guard,m2m,token,jwks,kv_trusted_store,integration_test,delegating_test}.go`
- `internal/common/error_codes.go`
- `internal/domain/account/handler.go`
- `app/config.go` (extend existing `IAMConfig`)
- `app/app.go` (wiring)
- `api/openapi.yaml` (10 ops conformance + JWK schema fix + ReactivateKeyRequestDto)
- `api/generated.go` (regenerated)
- `cmd/cyoda/help/content/{config/auth.md, errors.md, errors/{TRUSTED_KEY_NOT_FOUND.md, NOT_FOUND.md}, openapi.md}`
- `docs/superpowers/audits/2026-04-29-openapi-conformance-audit.md` (10 row dispositions)
- `CHANGELOG.md` (Keep-a-Changelog v0.8.0 section)

**Intentionally NOT modified:** `README.md` (no env-var config table exists; configuration is documented in cyoda help).

---

## Phase 1: Foundations

### Task 1: 6 new error codes

**Files:** Modify `internal/common/error_codes.go`.

- [ ] **Step 1:** Edit `internal/common/error_codes.go` — add inside the existing `const (...)` block (alphabetical):

```go
ErrCodeFeatureDisabled           = "FEATURE_DISABLED"
ErrCodeKeyOwnedByDifferentTenant = "KEY_OWNED_BY_DIFFERENT_TENANT"
ErrCodeKeypairNotFound           = "KEYPAIR_NOT_FOUND"
ErrCodeTrustedKeyCapReached      = "TRUSTED_KEY_CAP_REACHED"
ErrCodeUnsupportedAlgorithm      = "UNSUPPORTED_ALGORITHM"
ErrCodeUnsupportedKeyType        = "UNSUPPORTED_KEY_TYPE"
```

- [ ] **Step 2:** `go build ./internal/common/...` → PASS.
- [ ] **Step 3:** Commit: `feat(common): add 6 error codes for /oauth/keys/* conformance` (refs #281).

---

### Task 2: `IAMFeatures` value struct (RED-first)

**Files:** Create `internal/auth/iam_features.go` + `internal/auth/iam_features_test.go`.

- [ ] **Step 1: Write RED tests first.** Write `internal/auth/iam_features_test.go`:

```go
package auth_test

import (
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/auth"
)

func TestIAMFeatures_Validate_Defaults(t *testing.T) {
	if err := auth.DefaultIAMFeatures().Validate(); err != nil {
		t.Fatalf("default should validate, got %v", err)
	}
}

func TestIAMFeatures_Validate_Rejections(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*auth.IAMFeatures)
	}{
		{"BootstrapAudience invalid", func(c *auth.IAMFeatures) { c.BootstrapAudience = "robot" }},
		{"BootstrapAudience empty", func(c *auth.IAMFeatures) { c.BootstrapAudience = "" }},
		{"TrustedKeyMaxPerTenant negative", func(c *auth.IAMFeatures) { c.TrustedKeyMaxPerTenant = -1 }},
		{"TrustedKeyMaxValidityDays zero", func(c *auth.IAMFeatures) { c.TrustedKeyMaxValidityDays = 0 }},
		{"TrustedKeyMaxJWKProperties zero", func(c *auth.IAMFeatures) { c.TrustedKeyMaxJWKProperties = 0 }},
		{"KeypairDefaultValidityDays zero", func(c *auth.IAMFeatures) { c.KeypairDefaultValidityDays = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := auth.DefaultIAMFeatures()
			tc.mut(&c)
			if err := c.Validate(); err == nil {
				t.Errorf("expected error")
			}
		})
	}
}

func TestIAMFeatures_Validate_MaxPerTenantZeroIsUnbounded(t *testing.T) {
	c := auth.DefaultIAMFeatures()
	c.TrustedKeyMaxPerTenant = 0
	if err := c.Validate(); err != nil {
		t.Errorf("MaxPerTenant=0 means unbounded; got %v", err)
	}
}
```

- [ ] **Step 2:** `go test ./internal/auth/ -run TestIAMFeatures -v` → FAIL (undefined).

- [ ] **Step 3:** Write `internal/auth/iam_features.go`:

```go
package auth

import "fmt"

// IAMFeatures bundles IAM-feature configuration consumed by the
// /oauth/keys/* adapter surface and the bootstrap-key wiring.
// Named "Features" (not "Config") to avoid name collision with
// app.IAMConfig (app/config.go) which carries IAM mode + JWT settings.
type IAMFeatures struct {
	TrustedKeyRegistrationEnabled bool   // env CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED, default false
	TrustedKeyMaxPerTenant        int    // env CYODA_IAM_TRUSTED_KEY_MAX_PER_TENANT, default 10; 0=unbounded
	TrustedKeyMaxValidityDays     int    // env CYODA_IAM_TRUSTED_KEY_MAX_VALIDITY_DAYS, default 365
	TrustedKeyMaxJWKProperties    int    // env CYODA_IAM_TRUSTED_KEY_MAX_JWK_PROPERTIES, default 20
	KeypairDefaultValidityDays    int    // env CYODA_IAM_KEYPAIR_DEFAULT_VALIDITY_DAYS, default 365
	BootstrapAudience             string // env CYODA_JWT_BOOTSTRAP_AUDIENCE, default "client"
}

func DefaultIAMFeatures() IAMFeatures {
	return IAMFeatures{
		TrustedKeyMaxPerTenant:     10,
		TrustedKeyMaxValidityDays:  365,
		TrustedKeyMaxJWKProperties: 20,
		KeypairDefaultValidityDays: 365,
		BootstrapAudience:          "client",
	}
}

func (c IAMFeatures) Validate() error {
	if c.BootstrapAudience != "human" && c.BootstrapAudience != "client" {
		return fmt.Errorf("CYODA_JWT_BOOTSTRAP_AUDIENCE must be 'human' or 'client', got %q", c.BootstrapAudience)
	}
	if c.TrustedKeyMaxPerTenant < 0 {
		return fmt.Errorf("CYODA_IAM_TRUSTED_KEY_MAX_PER_TENANT must be >= 0, got %d", c.TrustedKeyMaxPerTenant)
	}
	if c.TrustedKeyMaxValidityDays <= 0 {
		return fmt.Errorf("CYODA_IAM_TRUSTED_KEY_MAX_VALIDITY_DAYS must be > 0, got %d", c.TrustedKeyMaxValidityDays)
	}
	if c.TrustedKeyMaxJWKProperties <= 0 {
		return fmt.Errorf("CYODA_IAM_TRUSTED_KEY_MAX_JWK_PROPERTIES must be > 0, got %d", c.TrustedKeyMaxJWKProperties)
	}
	if c.KeypairDefaultValidityDays <= 0 {
		return fmt.Errorf("CYODA_IAM_KEYPAIR_DEFAULT_VALIDITY_DAYS must be > 0, got %d", c.KeypairDefaultValidityDays)
	}
	return nil
}
```

- [ ] **Step 4:** `go test ./internal/auth/ -run TestIAMFeatures -v` → PASS.
- [ ] **Step 5:** Commit: `feat(auth): IAMFeatures value struct + RED-first Validate` (refs #281).

---

### Task 3: Move JWK validators to `keyvalidation.go`

**Files:** Create `internal/auth/keyvalidation.go`; Modify `internal/auth/trusted.go`.

- [ ] **Step 1:** Write `internal/auth/keyvalidation.go`:

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

var trustedKIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// MatchesTrustedKIDPattern is the exported form for adapter consumption.
func MatchesTrustedKIDPattern(kid string) bool { return trustedKIDPattern.MatchString(kid) }

// ParseRSAPublicKeyFromJWK is the exported form for adapter consumption.
// Returns a generic error on non-RSA kty; callers needing the specific
// UNSUPPORTED_KEY_TYPE response should check kty before calling.
func ParseRSAPublicKeyFromJWK(jwkData json.RawMessage) (*rsa.PublicKey, error) {
	return parseRSAPublicKeyFromJWK(jwkData)
}

func parseRSAPublicKeyFromJWK(jwkData json.RawMessage) (*rsa.PublicKey, error) {
	if len(jwkData) == 0 {
		return nil, fmt.Errorf("empty JWK")
	}
	var jwk struct{ Kty, N, E string }
	if err := json.Unmarshal(jwkData, &jwk); err != nil {
		return nil, fmt.Errorf("parse JWK: %w", err)
	}
	if jwk.Kty != "RSA" {
		return nil, fmt.Errorf("unsupported key type: %s", jwk.Kty)
	}
	if jwk.N == "" || jwk.E == "" {
		return nil, fmt.Errorf("missing n or e")
	}
	nBytes, err := decodeBase64URL(jwk.N)
	if err != nil {
		return nil, fmt.Errorf("invalid n: %w", err)
	}
	eBytes, err := decodeBase64URL(jwk.E)
	if err != nil {
		return nil, fmt.Errorf("invalid e: %w", err)
	}
	e, err := validateRSAPublicExponent(new(big.Int).SetBytes(eBytes))
	if err != nil {
		return nil, err
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(nBytes), E: e}, nil
}

func validateRSAPublicExponent(e *big.Int) (int, error) {
	if e.Sign() <= 0 {
		return 0, fmt.Errorf("exponent must be positive")
	}
	if !e.IsInt64() {
		return 0, fmt.Errorf("exponent too large")
	}
	v := e.Int64()
	if v > int64(math.MaxInt) {
		return 0, fmt.Errorf("exponent too large")
	}
	if v&1 == 0 {
		return 0, fmt.Errorf("exponent must be odd")
	}
	return int(v), nil
}
```

- [ ] **Step 2:** Delete from `internal/auth/trusted.go`: the `trustedKIDPattern` declaration, `parseRSAPublicKeyFromJWK`, and `validateRSAPublicExponent`. Keep the rest of `trusted.go` intact for now.

- [ ] **Step 3:** `go build ./internal/auth/... && go test ./internal/auth/ -short` → PASS.

- [ ] **Step 4:** Commit: `refactor(auth): move JWK validators to keyvalidation.go with exported names` (refs #281).


---

## Phase 2: Store layer refactor

### Task 4: KeyPair types + KeyStore interface + InMemoryKeyStore + callsite sweep (RED-first, no broken-build commit)

**Files:** Modify `internal/auth/store.go`, `internal/auth/store_test.go`, `internal/auth/service.go`, `internal/auth/keys.go` (briefly, deleted in Task 5), `internal/auth/token.go`, `internal/auth/jwks.go`, `internal/auth/delegating_test.go`, `internal/auth/integration_test.go`.

This task bundles types + interface change + impl + every callsite mechanical fix so the commit lands with a green tree.

- [ ] **Step 1: Inventory existing callsites.**

```
grep -rn "\.CreatedAt\b" internal/auth/ app/
grep -rn "keyStore.GetActive\|h.keyStore.GetActive\|s.keyStore.GetActive\|KeyStore().GetActive\|GetActive()" internal/auth/ app/
grep -rn "keyStore.Save\|h.keyStore.Save\|s.keyStore.Save\|KeyStore().Save" internal/auth/ app/
```

Expected hits to update mechanically (Step 7):
- `internal/auth/service.go:61-70` — bootstrap KeyPair literal (`CreatedAt` → `ValidFrom`; add `Audience`, `Algorithm`); `Save(kp)` → `Save(kp, RotateOptions{})`.
- `internal/auth/keys.go` — handler will be deleted in Task 5; for this commit, update its `Save(kp)` calls to `Save(kp, RotateOptions{})` AND add `Audience: "client", Algorithm: "RS256"` to its KeyPair literal so it stays green.
- `internal/auth/jwks.go:42` — STAYS `List()` here (Task 4b swaps to `ListForVerification()`).
- `internal/auth/token.go` — `GetActive()` → `GetActive("client")` (M2M signing path).
- `internal/auth/store_test.go` — `kp.CreatedAt` → `kp.ValidFrom`; `s.GetActive()` → `s.GetActive("client")`.
- `internal/auth/delegating_test.go:128` — same.
- `internal/auth/integration_test.go` — grep for any `.CreatedAt`/no-arg `GetActive()` and patch.

- [ ] **Step 2: Write failing tests (RED).** Append to `internal/auth/store_test.go`:

```go
func TestKeyStore_GetActive_AudiencePartition(t *testing.T) {
	s := auth.NewInMemoryKeyStore()
	priv := testRSAPriv(t)
	now := time.Now()
	human := &auth.KeyPair{KID: "h1", Audience: "human", Algorithm: "RS256", PublicKey: &priv.PublicKey, PrivateKey: priv, Active: true, ValidFrom: now}
	client := &auth.KeyPair{KID: "c1", Audience: "client", Algorithm: "RS256", PublicKey: &priv.PublicKey, PrivateKey: priv, Active: true, ValidFrom: now}
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
	if _, err := s.GetActive("robot"); err == nil {
		t.Fatal("GetActive(robot) should error")
	}
}

func TestKeyStore_GetActive_MaxValidFrom(t *testing.T) {
	s := auth.NewInMemoryKeyStore()
	priv := testRSAPriv(t)
	older := &auth.KeyPair{KID: "old", Audience: "client", Algorithm: "RS256", PublicKey: &priv.PublicKey, PrivateKey: priv, Active: true, ValidFrom: time.Now().Add(-1 * time.Hour)}
	newer := &auth.KeyPair{KID: "new", Audience: "client", Algorithm: "RS256", PublicKey: &priv.PublicKey, PrivateKey: priv, Active: true, ValidFrom: time.Now()}
	_ = s.Save(older, auth.RotateOptions{})
	_ = s.Save(newer, auth.RotateOptions{})
	got, _ := s.GetActive("client")
	if got.KID != "new" {
		t.Errorf("expected newer ValidFrom selected, got %s", got.KID)
	}
}

func TestKeyStore_Save_RotateInvalidatesSiblings(t *testing.T) {
	s := auth.NewInMemoryKeyStore()
	priv := testRSAPriv(t)
	now := time.Now()
	existing := &auth.KeyPair{KID: "e1", Audience: "client", Algorithm: "RS256", PublicKey: &priv.PublicKey, PrivateKey: priv, Active: true, ValidFrom: now}
	_ = s.Save(existing, auth.RotateOptions{})
	fresh := &auth.KeyPair{KID: "f1", Audience: "client", Algorithm: "RS256", PublicKey: &priv.PublicKey, PrivateKey: priv, Active: true, ValidFrom: now.Add(1 * time.Second)}
	if err := s.Save(fresh, auth.RotateOptions{Invalidate: true, GracePeriodSec: 60}); err != nil {
		t.Fatalf("save: %v", err)
	}
	old, _ := s.Get("e1")
	if old.Active {
		t.Error("expected e1.Active=false")
	}
	if old.ValidTo == nil {
		t.Error("expected e1.ValidTo set")
	}
}

func TestKeyStore_Save_RotateNoOp(t *testing.T) {
	s := auth.NewInMemoryKeyStore()
	priv := testRSAPriv(t)
	fresh := &auth.KeyPair{KID: "alone", Audience: "client", Algorithm: "RS256", PublicKey: &priv.PublicKey, PrivateKey: priv, Active: true, ValidFrom: time.Now()}
	if err := s.Save(fresh, auth.RotateOptions{Invalidate: true, GracePeriodSec: 60}); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, _ := s.Get("alone")
	if !got.Active {
		t.Error("solo Save with Invalidate=true should still leave new key active")
	}
}

func TestKeyStore_Save_ConcurrentRotateExactlyOneActive(t *testing.T) {
	s := auth.NewInMemoryKeyStore()
	priv := testRSAPriv(t)
	baseline := &auth.KeyPair{KID: "base", Audience: "client", Algorithm: "RS256", PublicKey: &priv.PublicKey, PrivateKey: priv, Active: true, ValidFrom: time.Now()}
	_ = s.Save(baseline, auth.RotateOptions{})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			kp := &auth.KeyPair{KID: fmt.Sprintf("c%d", i), Audience: "client", Algorithm: "RS256", PublicKey: &priv.PublicKey, PrivateKey: priv, Active: true, ValidFrom: time.Now().Add(time.Duration(i+1) * time.Millisecond)}
			_ = s.Save(kp, auth.RotateOptions{Invalidate: true, GracePeriodSec: 1})
		}()
	}
	wg.Wait()
	active := 0
	for _, kid := range []string{"base", "c0", "c1"} {
		if kp, err := s.Get(kid); err == nil && kp.Active {
			active++
		}
	}
	if active != 1 {
		t.Errorf("expected exactly 1 active client-audience key, got %d", active)
	}
}

func TestKeyStore_ListForVerification_LazyFilter(t *testing.T) {
	s := auth.NewInMemoryKeyStore()
	priv := testRSAPriv(t)
	now := time.Now()
	past := now.Add(-1 * time.Hour)
	active := &auth.KeyPair{KID: "active", Audience: "client", Algorithm: "RS256", PublicKey: &priv.PublicKey, PrivateKey: priv, Active: true, ValidFrom: now}
	expired := &auth.KeyPair{KID: "expired", Audience: "client", Algorithm: "RS256", PublicKey: &priv.PublicKey, PrivateKey: priv, Active: false, ValidFrom: past, ValidTo: &past}
	_ = s.Save(active, auth.RotateOptions{})
	_ = s.Save(expired, auth.RotateOptions{})
	got := s.ListForVerification()
	if len(got) != 1 || got[0].KID != "active" {
		t.Fatalf("expected only active, got %+v", got)
	}
}

func TestKeyStore_Reactivate_FreshWindow(t *testing.T) {
	s := auth.NewInMemoryKeyStore()
	priv := testRSAPriv(t)
	now := time.Now()
	past := now.Add(-1 * time.Hour)
	expired := &auth.KeyPair{KID: "e", Audience: "client", Algorithm: "RS256", PublicKey: &priv.PublicKey, PrivateKey: priv, Active: false, ValidFrom: past, ValidTo: &past}
	_ = s.Save(expired, auth.RotateOptions{})
	// Past validTo: reject
	if err := s.Reactivate("e", now, past); err == nil {
		t.Error("expected past-validTo to reject")
	}
	// Fresh validTo: accept
	if err := s.Reactivate("e", now, now.Add(24*time.Hour)); err != nil {
		t.Fatalf("reactivate: %v", err)
	}
	got, _ := s.Get("e")
	if !got.Active {
		t.Error("expected Active=true after reactivate")
	}
}

func TestKeyStore_Reactivate_IdempotentOnActive(t *testing.T) {
	s := auth.NewInMemoryKeyStore()
	priv := testRSAPriv(t)
	kp := &auth.KeyPair{KID: "k", Audience: "client", Algorithm: "RS256", PublicKey: &priv.PublicKey, PrivateKey: priv, Active: true, ValidFrom: time.Now()}
	_ = s.Save(kp, auth.RotateOptions{})
	newWindow := time.Now().Add(48 * time.Hour)
	if err := s.Reactivate("k", time.Now(), newWindow); err != nil {
		t.Fatalf("reactivate on already-active: %v", err)
	}
	got, _ := s.Get("k")
	if !got.Active {
		t.Error("expected Active=true (idempotent)")
	}
	if got.ValidTo == nil || !got.ValidTo.Equal(newWindow) {
		t.Errorf("expected ValidTo updated to %v, got %v", newWindow, got.ValidTo)
	}
}

func testRSAPriv(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	return priv
}
```

Add imports: `"crypto/rand"`, `"crypto/rsa"`, `"fmt"`, `"sync"`.

- [ ] **Step 3:** `go test ./internal/auth/ -run TestKeyStore -v` → FAIL (compile errors).

- [ ] **Step 4: Update types in `store.go`.**

Replace `KeyPair`:

```go
type KeyPair struct {
	KID        string
	Audience   string // "human" | "client"
	Algorithm  string // RS256 only in v0.8.0
	PublicKey  *rsa.PublicKey
	PrivateKey *rsa.PrivateKey
	Active     bool
	ValidFrom  time.Time
	ValidTo    *time.Time
}
```

Replace `TrustedKey`:

```go
type TrustedKey struct {
	KID       string
	TenantID  spi.TenantID
	JWK       map[string]any
	PublicKey *rsa.PublicKey
	Audience  string
	Issuers   []string
	Active    bool
	ValidFrom time.Time
	ValidTo   *time.Time
}
```

Add `spi "github.com/cyoda-platform/cyoda-go-spi"` to imports if not present.

Add after the type declarations:

```go
type RotateOptions struct {
	Invalidate     bool
	GracePeriodSec int64
}
```

- [ ] **Step 5: Update `KeyStore` interface:**

```go
type KeyStore interface {
	Save(kp *KeyPair, opts RotateOptions) error
	Get(kid string) (*KeyPair, error)
	GetActive(audience string) (*KeyPair, error)
	List() []*KeyPair
	ListForVerification() []*KeyPair
	Delete(kid string) error
	Invalidate(kid string, gracePeriodSec int64) error
	Reactivate(kid string, validFrom, validTo time.Time) error
}
```

- [ ] **Step 6: Update `InMemoryKeyStore` methods.** Delete the old impls; add:

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
	now := time.Now()
	var best *KeyPair
	for _, kp := range s.keys {
		if kp.Audience != audience || !kp.Active {
			continue
		}
		if kp.ValidTo != nil && !now.Before(*kp.ValidTo) {
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

- [ ] **Step 7: Sweep every callsite from Step 1.** Make the bootstrap KeyPair (`internal/auth/service.go:61-70`) read:

```go
kp := &KeyPair{
	KID:        signingKID,
	Audience:   "client", // bootstrap audience config will land in Task 14
	Algorithm:  "RS256",
	PublicKey:  &privateKey.PublicKey,
	PrivateKey: privateKey,
	Active:     true,
	ValidFrom:  time.Now().UTC(),
	// ValidTo: Task 14 sets this from IAMFeatures.KeypairDefaultValidityDays
}
if err := keyStore.Save(kp, RotateOptions{}); err != nil { ... }
```

Update `internal/auth/keys.go` similarly: `issueKeyPair` constructs KeyPair → add `Audience: "client", Algorithm: "RS256"`; change `Save(kp)` → `Save(kp, RotateOptions{})`. (`keys.go` is deleted in Task 5.)

Update `internal/auth/token.go` — every `GetActive()` (no-arg) → `GetActive("client")`. (Verify via grep that no human-audience signing path exists today before swapping.)

Update `internal/auth/store_test.go` — bulk replace `CreatedAt` → `ValidFrom`; `s.GetActive()` → `s.GetActive("client")`.

Update `internal/auth/delegating_test.go:128` — `svc.KeyStore().GetActive()` → `svc.KeyStore().GetActive("client")`.

Update `internal/auth/integration_test.go` — same sweep.

- [ ] **Step 8:** `go build ./... && go test ./internal/auth/ -short -v` → PASS.

- [ ] **Step 9:** Commit:

```bash
git add internal/auth/store.go internal/auth/store_test.go internal/auth/service.go internal/auth/keys.go internal/auth/token.go internal/auth/delegating_test.go internal/auth/integration_test.go
git commit -m "feat(auth): KeyPair audience partition + RotateOptions + ValidFrom rename

- KeyPair: rename CreatedAt to ValidFrom; add Audience, Algorithm, ValidTo *time.Time.
- TrustedKey: add TenantID, JWK map[string]any (interface use comes in Task 6).
- RotateOptions{Invalidate, GracePeriodSec} folds atomic sibling-flip into Save.
- GetActive(audience) partitions; max-ValidFrom when multiple active; lazy ValidTo filter.
- ListForVerification for JWKS/grace-period publishing.
- Reactivate requires fresh validity window (validTo > now and > validFrom);
  idempotent on already-active keys.
- All in-tree callsites swept (service.go bootstrap, keys.go, token.go, jwks.go,
  test files including delegating_test.go and integration_test.go). M2M signing
  path explicitly GetActive('client') so the audience switch is contractual.
- Includes concurrent-rotate (exactly-one-active) and no-op-flip tests per spec.

Refs #281."
```


---

### Task 5: JWKS source switch + delete `internal/auth/keys.go`

**Files:** Modify `internal/auth/jwks.go`; delete `internal/auth/keys.go`; modify `internal/auth/service.go` (drop adminMux entries).

- [ ] **Step 1:** Edit `internal/auth/jwks.go:42` — `h.keyStore.List()` → `h.keyStore.ListForVerification()`.

- [ ] **Step 2:** Delete `internal/auth/keys.go`:

```bash
git rm internal/auth/keys.go
```

- [ ] **Step 3:** Edit `internal/auth/service.go`:
- Delete the `keysHandler := NewKeysHandler(...)` construction line.
- Delete the two `adminMux.Handle("/oauth/keys/keypair...", keysHandler)` entries.

After this commit the `/oauth/keys/keypair/*` paths are unrouted until the adapters land in Task 14. No HTTP test currently hits them.

- [ ] **Step 4:** `go build ./... && go test ./internal/auth/ -short` → PASS.

- [ ] **Step 5:** Commit:

```bash
git add internal/auth/jwks.go internal/auth/keys.go internal/auth/service.go
git commit -m "refactor(auth): delete keys.go + switch JWKS to ListForVerification

JWKS endpoint now publishes grace-period-invalidated keys until ValidTo
passes. The 5 keypair operations move to chi-routed adapters in
Task 14+. service.go adminMux entries for /oauth/keys/keypair* dropped.

Refs #281."
```

---

### Task 6: TrustedKeyStore interface + InMemoryTrustedKeyStore + delete `trusted.go` (RED-first)

**Files:** Modify `internal/auth/store.go`, `internal/auth/store_test.go`; delete `internal/auth/trusted.go`; modify `internal/auth/service.go` (drop adminMux entries); modify `internal/auth/token.go` (transitional stub).

- [ ] **Step 1: Write failing tests (RED).** Append to `store_test.go`:

```go
func TestTrustedKeyStore_TenantIsolation(t *testing.T) {
	s := auth.NewInMemoryTrustedKeyStore()
	priv := testRSAPriv(t)
	tA := spi.TenantID("tenant-a")
	tB := spi.TenantID("tenant-b")
	tk := &auth.TrustedKey{KID: "k1", TenantID: tA, PublicKey: &priv.PublicKey, Audience: "human", Active: true, ValidFrom: time.Now()}
	if err := s.Register(tk, auth.RotateOptions{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := s.Get(tB, "k1"); err == nil {
		t.Error("B.Get(k1) leaked")
	}
	if err := s.Delete(tB, "k1"); err == nil {
		t.Error("B.Delete(k1) leaked")
	}
	if err := s.Invalidate(tB, "k1", 0); err == nil {
		t.Error("B.Invalidate(k1) leaked")
	}
}

func TestTrustedKeyStore_CrossTenantCollision_409(t *testing.T) {
	s := auth.NewInMemoryTrustedKeyStore()
	priv := testRSAPriv(t)
	tA := spi.TenantID("tenant-a")
	tB := spi.TenantID("tenant-b")
	kA := &auth.TrustedKey{KID: "shared", TenantID: tA, PublicKey: &priv.PublicKey, Audience: "human", Active: true, ValidFrom: time.Now()}
	_ = s.Register(kA, auth.RotateOptions{})
	kB := &auth.TrustedKey{KID: "shared", TenantID: tB, PublicKey: &priv.PublicKey, Audience: "human", Active: true, ValidFrom: time.Now()}
	err := s.Register(kB, auth.RotateOptions{})
	if err == nil {
		t.Fatal("expected cross-tenant error")
	}
	var ae *common.AppError
	if !errors.As(err, &ae) || ae.Code != common.ErrCodeKeyOwnedByDifferentTenant {
		t.Errorf("expected KEY_OWNED_BY_DIFFERENT_TENANT, got %v", err)
	}
}

func TestTrustedKeyStore_CapReached(t *testing.T) {
	s := auth.NewInMemoryTrustedKeyStoreWithCap(2)
	priv := testRSAPriv(t)
	tID := spi.TenantID("t")
	mk := func(kid string) *auth.TrustedKey {
		return &auth.TrustedKey{KID: kid, TenantID: tID, PublicKey: &priv.PublicKey, Audience: "human", Active: true, ValidFrom: time.Now()}
	}
	_ = s.Register(mk("k1"), auth.RotateOptions{})
	_ = s.Register(mk("k2"), auth.RotateOptions{})
	err := s.Register(mk("k3"), auth.RotateOptions{})
	if err == nil {
		t.Fatal("expected cap-reached error")
	}
	var ae *common.AppError
	if !errors.As(err, &ae) || ae.Code != common.ErrCodeTrustedKeyCapReached {
		t.Errorf("expected TRUSTED_KEY_CAP_REACHED, got %v", err)
	}
}

func TestTrustedKeyStore_CapCountsValidOnly(t *testing.T) {
	s := auth.NewInMemoryTrustedKeyStoreWithCap(2)
	priv := testRSAPriv(t)
	tID := spi.TenantID("t")
	past := time.Now().Add(-1 * time.Hour)
	expired := &auth.TrustedKey{KID: "old", TenantID: tID, PublicKey: &priv.PublicKey, Audience: "human", Active: false, ValidFrom: past, ValidTo: &past}
	active := &auth.TrustedKey{KID: "new", TenantID: tID, PublicKey: &priv.PublicKey, Audience: "human", Active: true, ValidFrom: time.Now()}
	_ = s.Register(expired, auth.RotateOptions{})
	_ = s.Register(active, auth.RotateOptions{})
	third := &auth.TrustedKey{KID: "third", TenantID: tID, PublicKey: &priv.PublicKey, Audience: "human", Active: true, ValidFrom: time.Now()}
	if err := s.Register(third, auth.RotateOptions{}); err != nil {
		t.Fatalf("expected accept; expired excluded from count; got %v", err)
	}
}

func TestTrustedKeyStore_RotateInvalidatesSameTenant(t *testing.T) {
	s := auth.NewInMemoryTrustedKeyStore()
	priv := testRSAPriv(t)
	tID := spi.TenantID("t")
	a := &auth.TrustedKey{KID: "a", TenantID: tID, PublicKey: &priv.PublicKey, Audience: "human", Active: true, ValidFrom: time.Now()}
	_ = s.Register(a, auth.RotateOptions{})
	b := &auth.TrustedKey{KID: "b", TenantID: tID, PublicKey: &priv.PublicKey, Audience: "human", Active: true, ValidFrom: time.Now().Add(1 * time.Second)}
	if err := s.Register(b, auth.RotateOptions{Invalidate: true, GracePeriodSec: 60}); err != nil {
		t.Fatalf("register: %v", err)
	}
	gotA, _ := s.Get(tID, "a")
	if gotA.Active || gotA.ValidTo == nil {
		t.Errorf("expected a invalidated with ValidTo; got %+v", gotA)
	}
}

func TestTrustedKeyStore_Reactivate_RequiresFreshWindow(t *testing.T) {
	s := auth.NewInMemoryTrustedKeyStore()
	priv := testRSAPriv(t)
	tID := spi.TenantID("t")
	now := time.Now()
	past := now.Add(-1 * time.Hour)
	expired := &auth.TrustedKey{KID: "e", TenantID: tID, PublicKey: &priv.PublicKey, Audience: "human", Active: false, ValidFrom: past, ValidTo: &past}
	_ = s.Register(expired, auth.RotateOptions{})
	if err := s.Reactivate(tID, "e", now, past); err == nil {
		t.Error("expected past validTo rejected")
	}
	if err := s.Reactivate(tID, "e", now, now.Add(24*time.Hour)); err != nil {
		t.Fatalf("reactivate: %v", err)
	}
}
```

Add imports: `"errors"`, `spi "github.com/cyoda-platform/cyoda-go-spi"`, `"github.com/cyoda-platform/cyoda-go/internal/common"`.

- [ ] **Step 2:** `go test ./internal/auth/ -run TestTrustedKeyStore -v` → FAIL.

- [ ] **Step 3: Update `TrustedKeyStore` interface** in `store.go`:

```go
type TrustedKeyStore interface {
	Register(tk *TrustedKey, opts RotateOptions) error
	Get(tenantID spi.TenantID, kid string) (*TrustedKey, error)
	List(tenantID spi.TenantID) []*TrustedKey
	ListForVerification() []*TrustedKey
	Delete(tenantID spi.TenantID, kid string) error
	Invalidate(tenantID spi.TenantID, kid string, gracePeriodSec int64) error
	Reactivate(tenantID spi.TenantID, kid string, validFrom, validTo time.Time) error
}
```

- [ ] **Step 4: Update `InMemoryTrustedKeyStore`.** Replace the struct + methods:

```go
type InMemoryTrustedKeyStore struct {
	mu           sync.RWMutex
	keys         map[string]*TrustedKey
	maxPerTenant int
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
	if existing, ok := s.keys[tk.KID]; ok && existing.TenantID != tk.TenantID {
		return common.Operational(http.StatusConflict, common.ErrCodeKeyOwnedByDifferentTenant, "key with this keyId belongs to a different tenant")
	}
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

Add imports: `"net/http"`, `"github.com/cyoda-platform/cyoda-go/internal/common"`.

- [ ] **Step 5: Delete `trusted.go` + adminMux entries.**

```bash
git rm internal/auth/trusted.go
```

Edit `internal/auth/service.go` — delete the `trustedHandler := NewTrustedKeysHandler(...)` line and both `adminMux.Handle("/oauth/keys/trusted...", ...)` entries.

- [ ] **Step 6: Replace `token.go` lookup with a transitional stub.**

Edit `internal/auth/token.go` — at line 137, replace `trustedKey, err := h.trustedKeyStore.Get(kid)` with `trustedKey, err := findTrustedKeyByKIDStub(h.trustedKeyStore, kid)`. Add at the bottom of `token.go`:

```go
// findTrustedKeyByKIDStub is a transitional helper for Task 6 → Task 7.
// Iterates ListForVerification() applying lazy ValidTo filter. Replaced by
// the canonical getTrustedKeyByKID in Task 7.
func findTrustedKeyByKIDStub(store TrustedKeyStore, kid string) (*TrustedKey, error) {
	for _, tk := range store.ListForVerification() {
		if tk.KID == kid {
			return tk, nil
		}
	}
	return nil, fmt.Errorf("trusted key not found")
}
```

- [ ] **Step 7:** `go test ./internal/auth/ -short -v && go build ./...` → PASS.

- [ ] **Step 8:** Commit:

```bash
git add internal/auth/
git commit -m "feat(auth): TrustedKeyStore tenant scoping + cap + atomic rotate; delete trusted.go

- Register returns 409 KEY_OWNED_BY_DIFFERENT_TENANT on cross-tenant
  collision; 400 TRUSTED_KEY_CAP_REACHED at cap (counts only currently
  valid keys per cloud parity).
- All CRUD methods tenant-scoped; cross-tenant access returns not-found.
- RotateOptions{Invalidate} atomically flips siblings in same-tenant
  partition.
- trusted.go HTTP handler deleted (5 trusted-key ops move to chi adapters
  in Task 14+); service.go adminMux entries dropped.
- token.go transitional findTrustedKeyByKIDStub; Task 7 replaces with
  canonical helper.

Refs #281."
```

---

### Task 7: `getTrustedKeyByKID` canonical helper + replace stub

**Files:** Create `internal/auth/verification.go`; create `internal/auth/token_test.go` (in-package test); modify `internal/auth/token.go`.

- [ ] **Step 1: Write RED tests.** Write `internal/auth/token_test.go`:

```go
package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func TestGetTrustedKeyByKID_Found(t *testing.T) {
	s := NewInMemoryTrustedKeyStore()
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	tk := &TrustedKey{KID: "k1", TenantID: spi.TenantID("ta"), PublicKey: &priv.PublicKey, Audience: "human", Active: true, ValidFrom: time.Now()}
	_ = s.Register(tk, RotateOptions{})
	got, err := getTrustedKeyByKID(s, "k1")
	if err != nil || got.KID != "k1" {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}

func TestGetTrustedKeyByKID_PastValidTo_NotFound(t *testing.T) {
	s := NewInMemoryTrustedKeyStore()
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	past := time.Now().Add(-1 * time.Hour)
	tk := &TrustedKey{KID: "expired", TenantID: spi.TenantID("t"), PublicKey: &priv.PublicKey, Audience: "human", Active: false, ValidFrom: past, ValidTo: &past}
	_ = s.Register(tk, RotateOptions{})
	if _, err := getTrustedKeyByKID(s, "expired"); err == nil {
		t.Error("past-ValidTo should not surface")
	}
}

func TestGetTrustedKeyByKID_Missing(t *testing.T) {
	s := NewInMemoryTrustedKeyStore()
	if _, err := getTrustedKeyByKID(s, "missing"); err == nil {
		t.Error("expected not-found")
	}
}
```

- [ ] **Step 2:** `go test ./internal/auth/ -run TestGetTrustedKeyByKID -v` → FAIL.

- [ ] **Step 3: Write `verification.go`:**

```go
package auth

import "fmt"

// getTrustedKeyByKID resolves a trusted key by kid without tenant scoping.
// Used exclusively by token.go's token-exchange / JWT-bearer-assertion grant.
// Iterates ListForVerification() so lazy ValidTo filter applies; past-ValidTo
// entries are excluded. Bypasses tenant scoping by design — caller (token.go)
// enforces principal-tenant invariant at the existing client.TenantID == subOrgID
// check (spec §7.1).
func getTrustedKeyByKID(store TrustedKeyStore, kid string) (*TrustedKey, error) {
	for _, tk := range store.ListForVerification() {
		if tk.KID == kid {
			return tk, nil
		}
	}
	return nil, fmt.Errorf("trusted key not found")
}
```

- [ ] **Step 4: Replace the stub.** Edit `internal/auth/token.go`:
- Delete `findTrustedKeyByKIDStub` function.
- Replace the line that calls it (from Task 6 Step 6) with `getTrustedKeyByKID(h.trustedKeyStore, kid)`.

- [ ] **Step 5:** `go test ./internal/auth/ -short -v && go build ./...` → PASS.

- [ ] **Step 6:** Commit: `feat(auth): canonical getTrustedKeyByKID; remove transitional stub` (refs #281).


---

### Task 8: KVTrustedKeyStore tenant-scoped key + record schema (RED-first)

**Files:** Modify `internal/auth/kv_trusted_store.go`, `internal/auth/kv_trusted_store_test.go`.

- [ ] **Step 1: Inspect current KV store.**

```
grep -n "^func\|trustedKeysNamespace\|s\.kv\.\|trustedKeyRecord\|serializeTrustedKey\|deserializeTrustedKey\|loadAll\|loadOne" internal/auth/kv_trusted_store.go
```

Expected: namespace is `"trusted-keys"`; `loadAll() error` (no ctx); record struct; serialize/deserialize helpers; KV calls take `(s.ctx, trustedKeysNamespace, kid)`. There is NO existing `kvKey` helper — Task 8 introduces `trustedKeyKey(tenantID, kid)`.

- [ ] **Step 2: Write RED tests.** Append to `internal/auth/kv_trusted_store_test.go`:

```go
func TestKVTrustedKeyStore_TenantScopedKeyEncoding(t *testing.T) {
	ctx := systemCtx()
	kv := mustNewMemoryKV(t, ctx)
	store, err := auth.NewKVTrustedKeyStore(ctx, kv)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	tID := spi.TenantID("tenant-a")
	tk := &auth.TrustedKey{KID: "k1", TenantID: tID, PublicKey: &priv.PublicKey, JWK: map[string]any{"kty": "RSA", "kid": "k1"}, Audience: "human", Active: true, ValidFrom: time.Now()}
	if err := store.Register(tk, auth.RotateOptions{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	all, err := kv.List(ctx, "trusted-keys")
	if err != nil {
		t.Fatalf("kv list: %v", err)
	}
	if _, ok := all["tenant-a:k1"]; !ok {
		t.Errorf("expected key 'tenant-a:k1' in KV; keys: %v", mapKeys(all))
	}
}

func TestKVTrustedKeyStore_NoCrossTenantCachePollution(t *testing.T) {
	ctx := systemCtx()
	kv := mustNewMemoryKV(t, ctx)
	store, _ := auth.NewKVTrustedKeyStore(ctx, kv)
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	tA := spi.TenantID("tenant-a")
	tB := spi.TenantID("tenant-b")
	tk := &auth.TrustedKey{KID: "k1", TenantID: tA, PublicKey: &priv.PublicKey, JWK: map[string]any{"kty": "RSA", "kid": "k1"}, Audience: "human", Active: true, ValidFrom: time.Now()}
	_ = store.Register(tk, auth.RotateOptions{})
	if _, err := store.Get(tA, "k1"); err != nil {
		t.Fatalf("A.Get: %v", err)
	}
	if _, err := store.Get(tB, "k1"); err == nil {
		t.Error("B.Get(k1) leaked A's key")
	}
	if _, err := store.Get(tA, "k1"); err != nil {
		t.Errorf("A.Get post-B failure: %v", err)
	}
}

func TestKVTrustedKeyStore_RoundTripsTenantIDAndJWK(t *testing.T) {
	ctx := systemCtx()
	kv := mustNewMemoryKV(t, ctx)
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	tID := spi.TenantID("t1")
	originalJWK := map[string]any{"kty": "RSA", "kid": "k", "extra": "field"}
	tk := &auth.TrustedKey{KID: "k", TenantID: tID, PublicKey: &priv.PublicKey, JWK: originalJWK, Audience: "human", Active: true, ValidFrom: time.Now()}
	store, _ := auth.NewKVTrustedKeyStore(ctx, kv)
	_ = store.Register(tk, auth.RotateOptions{})
	store2, err := auth.NewKVTrustedKeyStore(ctx, kv)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got, err := store2.Get(tID, "k")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.TenantID != tID {
		t.Errorf("TenantID lost: %q", got.TenantID)
	}
	if got.JWK["extra"] != "field" {
		t.Errorf("JWK 'extra' lost: %+v", got.JWK)
	}
}

func mapKeys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func mustNewMemoryKV(t *testing.T, ctx context.Context) spi.KeyValueStore {
	t.Helper()
	factory := memory.NewStoreFactory(memory.Config{})
	kv, err := factory.KeyValueStore(ctx)
	if err != nil {
		t.Fatalf("memory KV: %v", err)
	}
	return kv
}
```

Add imports: `"context"`, `"crypto/rand"`, `"crypto/rsa"`, `spi "github.com/cyoda-platform/cyoda-go-spi"`, `"github.com/cyoda-platform/cyoda-go/internal/auth"`, `"github.com/cyoda-platform/cyoda-go/plugins/memory"`.

Verify `memory.NewStoreFactory(memory.Config{})` is the actual signature with `head -40 plugins/memory/factory.go`; adjust if needed.

- [ ] **Step 3:** `go test ./internal/auth/ -run TestKVTrustedKeyStore -v` → FAIL (signature mismatch).

- [ ] **Step 4: Add `trustedKeyKey` helper + update `trustedKeyRecord`:**

```go
// trustedKeyKey returns the KV key (within trustedKeysNamespace) for a (tenantID, kid).
// Layout "<tenantID>:<kid>" makes tenant isolation a storage-layer invariant.
func trustedKeyKey(tenantID spi.TenantID, kid string) string {
	return string(tenantID) + ":" + kid
}

type trustedKeyRecord struct {
	KID       string         `json:"kid"`
	TenantID  string         `json:"tenantID,omitempty"`
	JWK       map[string]any `json:"jwk,omitempty"`
	Audience  string         `json:"audience"`
	Issuers   []string       `json:"issuers,omitempty"`
	Active    bool           `json:"active"`
	ValidFrom time.Time      `json:"validFrom"`
	ValidTo   *time.Time     `json:"validTo,omitempty"`
	N         string         `json:"n"`
	E         string         `json:"e"`
}
```

Update serialize / deserialize to round-trip the new fields:

```go
func serializeTrustedKey(tk *TrustedKey) ([]byte, error) {
	rec := trustedKeyRecord{
		KID: tk.KID, TenantID: string(tk.TenantID), JWK: tk.JWK,
		Audience: tk.Audience, Issuers: tk.Issuers, Active: tk.Active,
		ValidFrom: tk.ValidFrom, ValidTo: tk.ValidTo,
		N: base64.RawURLEncoding.EncodeToString(tk.PublicKey.N.Bytes()),
		E: base64.RawURLEncoding.EncodeToString(big.NewInt(int64(tk.PublicKey.E)).Bytes()),
	}
	return json.Marshal(rec)
}

func deserializeTrustedKey(data []byte) (*TrustedKey, error) {
	var rec trustedKeyRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	nb, err := decodeBase64URL(rec.N)
	if err != nil {
		return nil, fmt.Errorf("n: %w", err)
	}
	eb, err := decodeBase64URL(rec.E)
	if err != nil {
		return nil, fmt.Errorf("e: %w", err)
	}
	pub := &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: int(new(big.Int).SetBytes(eb).Int64())}
	return &TrustedKey{
		KID: rec.KID, TenantID: spi.TenantID(rec.TenantID), JWK: rec.JWK,
		PublicKey: pub, Audience: rec.Audience, Issuers: rec.Issuers,
		Active: rec.Active, ValidFrom: rec.ValidFrom, ValidTo: rec.ValidTo,
	}, nil
}
```

- [ ] **Step 5: Update `KVTrustedKeyStore` struct + methods.** Add field if missing:

```go
type KVTrustedKeyStore struct {
	ctx          context.Context
	kv           spi.KeyValueStore
	mu           sync.RWMutex
	keys         map[string]*TrustedKey
	maxPerTenant int // wired via KVTrustedKeyStoreOption (Task 14)
}
```

Replace public methods (KV writes FIRST, in-memory mutation LAST for rollback safety):

```go
func (s *KVTrustedKeyStore) Register(tk *TrustedKey, opts RotateOptions) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.keys[tk.KID]; ok && existing.TenantID != tk.TenantID {
		return common.Operational(http.StatusConflict, common.ErrCodeKeyOwnedByDifferentTenant, "key with this keyId belongs to a different tenant")
	}
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
	// Stage sibling-flip clones in memory; write KV for each, then commit cache.
	updates := make([]*TrustedKey, 0)
	if opts.Invalidate {
		now := time.Now()
		expiry := now.Add(time.Duration(opts.GracePeriodSec) * time.Second)
		for _, k := range s.keys {
			if k.TenantID == tk.TenantID && k.Active && k.KID != tk.KID {
				clone := *k
				clone.Active = false
				e := expiry
				clone.ValidTo = &e
				data, err := serializeTrustedKey(&clone)
				if err != nil {
					return fmt.Errorf("serialize sibling %s: %w", k.KID, err)
				}
				if err := s.kv.Put(s.ctx, trustedKeysNamespace, trustedKeyKey(clone.TenantID, clone.KID), data); err != nil {
					return fmt.Errorf("persist sibling %s: %w", k.KID, err)
				}
				updates = append(updates, &clone)
			}
		}
	}
	data, err := serializeTrustedKey(tk)
	if err != nil {
		return fmt.Errorf("serialize: %w", err)
	}
	if err := s.kv.Put(s.ctx, trustedKeysNamespace, trustedKeyKey(tk.TenantID, tk.KID), data); err != nil {
		return fmt.Errorf("persist: %w", err)
	}
	// Mutate cache LAST (rollback safety on partial KV failure above).
	for _, u := range updates {
		s.keys[u.KID] = u
	}
	s.keys[tk.KID] = tk
	return nil
}

func (s *KVTrustedKeyStore) Get(tenantID spi.TenantID, kid string) (*TrustedKey, error) {
	s.mu.RLock()
	cached, ok := s.keys[kid]
	s.mu.RUnlock()
	if ok && cached.TenantID == tenantID {
		return cached, nil
	}
	// Cache miss OR cache hit from different tenant -> KV fallback (tenant-keyed).
	data, err := s.kv.Get(s.ctx, trustedKeysNamespace, trustedKeyKey(tenantID, kid))
	if err != nil || data == nil {
		return nil, fmt.Errorf("trusted key not found")
	}
	tk, err := deserializeTrustedKey(data)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if c, ok := s.keys[kid]; !ok || c.TenantID == tenantID {
		s.keys[kid] = tk
	}
	s.mu.Unlock()
	return tk, nil
}

func (s *KVTrustedKeyStore) List(tenantID spi.TenantID) []*TrustedKey {
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

func (s *KVTrustedKeyStore) ListForVerification() []*TrustedKey {
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

func (s *KVTrustedKeyStore) Delete(tenantID spi.TenantID, kid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[kid]
	if !ok || k.TenantID != tenantID {
		return fmt.Errorf("trusted key not found")
	}
	if err := s.kv.Delete(s.ctx, trustedKeysNamespace, trustedKeyKey(tenantID, kid)); err != nil {
		return fmt.Errorf("kv delete: %w", err)
	}
	delete(s.keys, kid)
	return nil
}

func (s *KVTrustedKeyStore) Invalidate(tenantID spi.TenantID, kid string, gracePeriodSec int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[kid]
	if !ok || k.TenantID != tenantID {
		return fmt.Errorf("trusted key not found")
	}
	clone := *k
	expiry := time.Now().Add(time.Duration(gracePeriodSec) * time.Second)
	clone.Active = false
	clone.ValidTo = &expiry
	data, err := serializeTrustedKey(&clone)
	if err != nil {
		return err
	}
	if err := s.kv.Put(s.ctx, trustedKeysNamespace, trustedKeyKey(tenantID, kid), data); err != nil {
		return err
	}
	s.keys[kid] = &clone
	return nil
}

func (s *KVTrustedKeyStore) Reactivate(tenantID spi.TenantID, kid string, validFrom, validTo time.Time) error {
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
	clone := *k
	clone.Active = true
	clone.ValidFrom = validFrom
	vt := validTo
	clone.ValidTo = &vt
	data, err := serializeTrustedKey(&clone)
	if err != nil {
		return err
	}
	if err := s.kv.Put(s.ctx, trustedKeysNamespace, trustedKeyKey(tenantID, kid), data); err != nil {
		return err
	}
	s.keys[kid] = &clone
	return nil
}
```

- [ ] **Step 6: Update `loadAll`:**

```go
func (s *KVTrustedKeyStore) loadAll() error {
	all, err := s.kv.List(s.ctx, trustedKeysNamespace)
	if err != nil {
		return fmt.Errorf("kv list: %w", err)
	}
	oldShapeCount := 0
	for key, data := range all {
		if !strings.Contains(key, ":") {
			oldShapeCount++
			continue
		}
		tk, err := deserializeTrustedKey(data)
		if err != nil {
			slog.Warn("kv trusted-key entry malformed; skipping", "pkg", "auth", "key", key, "err", err)
			continue
		}
		if tk.TenantID == "" {
			oldShapeCount++
			continue
		}
		s.keys[tk.KID] = tk
	}
	if oldShapeCount > 0 {
		slog.Warn("pre-v0.8.0 trusted-key entries found in KV; not loaded under new key shape — re-register via /oauth/keys/trusted; see v0.8.0 release notes", "pkg", "auth", "count", oldShapeCount)
	}
	return nil
}
```

Add `"strings"`, `"log/slog"` imports if missing.

- [ ] **Step 7:** `go test ./internal/auth/ -short -v` → PASS.

- [ ] **Step 8:** Commit:

```bash
git add internal/auth/kv_trusted_store.go internal/auth/kv_trusted_store_test.go
git commit -m "feat(auth): KVTrustedKeyStore tenant-scoped key + schema

- KV key (within 'trusted-keys' namespace) changes from '<kid>' to
  '<tenantID>:<kid>' so tenant isolation is a storage-layer invariant.
- trustedKeyRecord gains tenantID + jwk (omitempty).
- All public methods tenant-scoped. Cache reads verify cached.TenantID
  matches caller; cross-tenant cache hit falls through to KV.
- Atomicity: Register writes KV first, mutates cache LAST so partial
  KV failure leaves cache untouched.
- loadAll skips legacy <kid>-only entries; emits one-shot startup WARN
  with count.
- Legacy 409-on-registry-full path replaced by TRUSTED_KEY_CAP_REACHED.

Refs #281."
```

---

### Task 9: KV old-shape WARN + partial-failure rollback tests

**Files:** Modify `internal/auth/kv_trusted_store.go` (add testing helper); modify `internal/auth/kv_trusted_store_test.go`.

- [ ] **Step 1: Expose key helper for tests.** Append to `kv_trusted_store.go`:

```go
// TrustedKeyKVKeyForTesting exposes trustedKeyKey for cross-package tests
// that need to predict KV keys (e.g. for injection mocks).
func TrustedKeyKVKeyForTesting(tenantID spi.TenantID, kid string) string {
	return trustedKeyKey(tenantID, kid)
}
```

- [ ] **Step 2: Old-shape WARN test.** Append to `kv_trusted_store_test.go`:

```go
func TestKVTrustedKeyStore_LoadAll_SkipsOldShape_EmitsWARN(t *testing.T) {
	ctx := systemCtx()
	kv := mustNewMemoryKV(t, ctx)
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	rec := map[string]any{
		"kid": "old1", "audience": "human", "active": true, "validFrom": time.Now(),
		"n": base64.RawURLEncoding.EncodeToString(priv.PublicKey.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(priv.PublicKey.E)).Bytes()),
	}
	data, _ := json.Marshal(rec)
	if err := kv.Put(ctx, "trusted-keys", "old1", data); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := kv.Put(ctx, "trusted-keys", "old2", data); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)
	store, err := auth.NewKVTrustedKeyStore(ctx, kv)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if got := store.List(spi.TenantID("any")); len(got) != 0 {
		t.Errorf("legacy entries should not surface; got %d", len(got))
	}
	if !strings.Contains(buf.String(), "pre-v0.8.0 trusted-key entries") {
		t.Errorf("expected WARN; got: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "count=2") {
		t.Errorf("expected count=2; got: %s", buf.String())
	}
}
```

Add imports: `"bytes"`, `"encoding/base64"`, `"encoding/json"`, `"log/slog"`, `"math/big"`, `"strings"`.

- [ ] **Step 3: Partial-failure rollback test.** Append:

```go
type failingKV struct {
	spi.KeyValueStore
	failOn string
}

func (f *failingKV) Put(ctx context.Context, ns, key string, value []byte) error {
	if key == f.failOn {
		return fmt.Errorf("injected failure")
	}
	return f.KeyValueStore.Put(ctx, ns, key, value)
}

func TestKVTrustedKeyStore_PartialKVFailureLeavesCacheUntouched(t *testing.T) {
	ctx := systemCtx()
	mem := mustNewMemoryKV(t, ctx)
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	tID := spi.TenantID("t")
	pre, _ := auth.NewKVTrustedKeyStore(ctx, mem)
	a := &auth.TrustedKey{KID: "a", TenantID: tID, PublicKey: &priv.PublicKey, JWK: map[string]any{"kty": "RSA", "kid": "a"}, Audience: "human", Active: true, ValidFrom: time.Now()}
	_ = pre.Register(a, auth.RotateOptions{})

	// Inject failure on the SIBLING flip-write (key 't:a').
	kv := &failingKV{KeyValueStore: mem, failOn: auth.TrustedKeyKVKeyForTesting(tID, "a")}
	store, _ := auth.NewKVTrustedKeyStore(ctx, kv)
	b := &auth.TrustedKey{KID: "b", TenantID: tID, PublicKey: &priv.PublicKey, JWK: map[string]any{"kty": "RSA", "kid": "b"}, Audience: "human", Active: true, ValidFrom: time.Now().Add(1 * time.Second)}
	err := store.Register(b, auth.RotateOptions{Invalidate: true, GracePeriodSec: 60})
	if err == nil {
		t.Fatal("expected register failure")
	}
	gotA, _ := store.Get(tID, "a")
	if gotA == nil || !gotA.Active {
		t.Errorf("a should still be active after rollback; got %+v", gotA)
	}
	if _, err := store.Get(tID, "b"); err == nil {
		t.Error("b should not be visible after rollback")
	}
}
```

Add import: `"fmt"`.

- [ ] **Step 4:** `go test ./internal/auth/ -run TestKVTrustedKeyStore -v` → PASS.

- [ ] **Step 5:** Commit: `test(auth): KV legacy WARN + partial-failure rollback assertions` (refs #281).


---

## Phase 3: OpenAPI surgery + codegen regen

### Task 10: OpenAPI spec edits

**Files:** Modify `api/openapi.yaml`.

- [ ] **Step 1: Add `ReactivateKeyRequestDto` schema.** Edit `api/openapi.yaml`, alphabetical placement near `InvalidateKeyRequestDto` (line 8509):

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

- [ ] **Step 2: Add request body to both reactivate ops.** For `reactivateJwtKeyPair` (~ line 4280-4310) and `reactivateTrustedKey` (~ line 4591-4621), add:

```yaml
requestBody:
  content:
    application/json:
      schema:
        $ref: "#/components/schemas/ReactivateKeyRequestDto"
  required: true
```

And add a `"400"` response (alphabetical-by-status; both responses live in `responses:` map):

```yaml
"400":
  description: Error response
  content:
    application/problem+json:
      schema:
        $ref: "#/components/schemas/ProblemDetail"
```

- [ ] **Step 3: Fix JWK schema (`additionalProperties: { type: object }` → `additionalProperties: true`)** at:
- `api/openapi.yaml:8413-8416` (TrustedKeyResponseDto.jwk)
- `api/openapi.yaml:8457-8460` (RegisterTrustedKeyRequestDto.jwk)

After:

```yaml
jwk:
  type: object
  additionalProperties: true
  description: "..."
```

- [ ] **Step 4: Remove 501 declarations.** For each of the 10 ops (`issueJwtKeyPair`, `getCurrentJwtKeyPair`, `deleteJwtKeyPair`, `invalidateJwtKeyPair`, `reactivateJwtKeyPair`, `listTrustedKeys`, `registerTrustedKey`, `deleteTrustedKey`, `invalidateTrustedKey`, `reactivateTrustedKey`), DELETE:

```yaml
"501":
  $ref: "#/components/responses/NotImplemented"
```

After all 10, verify only the 10 are touched:

```
grep -c "501" api/openapi.yaml
```

Should be 12 less than before (10 ops × 1 line each + the spec-wide 2 entries that stay).

- [ ] **Step 5: Switch every 4xx/5xx to `ProblemDetail`.** For each of the 10 ops, every response with status 400/401/403/404/409/500 that currently references `ErrorResponseDto`, replace with:

```yaml
"NNN":
  description: Error response
  content:
    application/problem+json:
      schema:
        $ref: "#/components/schemas/ProblemDetail"
```

- [ ] **Step 6: Replace SUPER_USER with ROLE_ADMIN in the 5 TRUSTED ops only.** (The 5 keypair ops never used "SUPER_USER" in their descriptions.) Verify with:

```
grep -n "SUPER_USER" api/openapi.yaml
```

After editing the 5 trusted ops, the only remaining `SUPER_USER` references should be outside the `/oauth/keys/*` block.

- [ ] **Step 7: Add default-behaviour prose.** Append one line to each of these op descriptions:
- `issueJwtKeyPair`: "If `algorithm` is omitted, defaults to RS256. If `validTo` is omitted, defaults to `validFrom + 365 days`."
- `registerTrustedKey`: "If `validTo` is omitted, defaults to `validFrom + 365 days`. If `validFrom` is omitted, defaults to now."
- `invalidateJwtKeyPair` and `invalidateTrustedKey`: "If `gracePeriodSec` is omitted or zero, invalidation is immediate."

- [ ] **Step 8: Verify YAML well-formedness.**

Run: `python3 -c "import yaml; yaml.safe_load(open('api/openapi.yaml'))"`
Expected: no parse error.

- [ ] **Step 9:** Commit: `feat(openapi): conform /oauth/keys/* operations + JWK schema fix` (refs #281).

---

### Task 11: Regenerate `api/generated.go`

**Files:** Modify `api/generated.go` (regenerated).

- [ ] **Step 1: Run codegen.**

```
go generate ./api/...
```

Expected: regenerates `api/generated.go`. Note: `api/generate.go:3` declares `//go:generate go tool oapi-codegen --config=config.yaml openapi.yaml`.

- [ ] **Step 2: Verify the regen output.**

```
grep -n "ReactivateKeyRequestDto\|Jwk\s\+map\[string\]" api/generated.go | head -20
```

Expected:
- `type ReactivateKeyRequestDto struct { ... ValidTo time.Time ... ValidFrom *time.Time ... }`.
- `Jwk map[string]interface{}` (single map, NOT nested) in both `RegisterTrustedKeyRequestDto` and `TrustedKeyResponseDto`.
- Type aliases `ReactivateJwtKeyPairJSONRequestBody = ReactivateKeyRequestDto` and `ReactivateTrustedKeyJSONRequestBody = ReactivateKeyRequestDto`.
- **`ServerInterface` reactivate signatures STAY `(w, r, keyId)`** — std-http-server is body-agnostic. Don't expect new payload parameters.

- [ ] **Step 3:** `go build ./...` → expect failures in `internal/domain/account/handler.go` because the existing stubs still match the (unchanged) ServerInterface signatures, AND because Task 4 hasn't introduced the Handler struct fields yet. That's expected; subsequent tasks address it.

- [ ] **Step 4:** Commit: `chore(api): regenerate generated.go` (refs #281). Use `-n` (dry-run) first to inspect diff scope.


---

## Phase 4: Handler infrastructure

### Task 12: Export `RequireAdmin`

**Files:** Modify `internal/auth/admin_guard.go`, `internal/auth/m2m.go`.

- [ ] **Step 1: Rename `requireAdmin` to `RequireAdmin`** in `admin_guard.go`. Update its doc comment to use the exported name.

- [ ] **Step 2: Update in-package callsites.**

```
grep -rln "requireAdmin(" internal/auth/
```

For every match (now just `m2m.go` since keys.go/trusted.go are deleted), replace `requireAdmin(` with `RequireAdmin(`. Also sweep doc-comment occurrences for hygiene.

- [ ] **Step 3:** `go build ./... && go test ./internal/auth/ -short` → PASS.

- [ ] **Step 4:** Commit: `refactor(auth): export RequireAdmin for cross-package use` (refs #281).

---

### Task 13: `boundedJSONDecode` helper (RED-first)

**Files:** Create `internal/domain/account/io.go`, `internal/domain/account/io_test.go`.

- [ ] **Step 1: Write RED tests.**

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
		t.Errorf("got x=%d", d.X)
	}
}

func TestBoundedJSONDecode_OverSize(t *testing.T) {
	type dst struct{ X string `json:"x"` }
	big := strings.Repeat("a", 1<<20+1)
	r := httptest.NewRequest("POST", "/", bytes.NewReader([]byte(`{"x":"`+big+`"}`)))
	w := httptest.NewRecorder()
	var d dst
	if err := account.BoundedJSONDecodeForTesting(w, r, 1<<20, &d); err == nil {
		t.Fatal("expected error")
	}
}

func TestBoundedJSONDecode_BadJSON(t *testing.T) {
	type dst struct{}
	r := httptest.NewRequest("POST", "/", bytes.NewReader([]byte(`not-json`)))
	w := httptest.NewRecorder()
	var d dst
	if err := account.BoundedJSONDecodeForTesting(w, r, 1<<10, &d); err == nil {
		t.Fatal("expected error")
	}
}
```

- [ ] **Step 2:** `go test ./internal/domain/account/ -run TestBoundedJSONDecode -v` → FAIL.

- [ ] **Step 3: Write `io.go`:**

```go
package account

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// boundedJSONDecode wraps http.MaxBytesReader + json.Decoder.Decode.
// Returns non-nil error on oversize body or JSON parse failure. Callers
// translate to 400 BAD_REQUEST via common.WriteError.
//
// All 4 POST /oauth/keys/* adapters use this helper with max = 1<<20 (1 MiB).
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

- [ ] **Step 4:** `go test ./internal/domain/account/ -run TestBoundedJSONDecode -v` → PASS.

- [ ] **Step 5:** Commit: `feat(account): boundedJSONDecode helper for /oauth/keys/* POSTs` (refs #281).

---

### Task 14: Handler struct + `account.New` signature + IAM wiring

**Files:** Modify `internal/domain/account/handler.go`, `internal/domain/account/handler_test.go`, `app/config.go`, `app/app.go`, `internal/auth/service.go`.

This task is meaningful in size (handler + config wiring + bootstrap key wiring all touch the same plumbing) but stays under one task to avoid intermediate broken builds.

- [ ] **Step 1: Inspect existing handler_test.go callsites.**

```
grep -n "account.New(" internal/domain/account/handler_test.go
```

Expected: 4 occurrences (`TestNewHandler`, `TestAccountGet`, plus 2 more).

- [ ] **Step 2: Extend `app.IAMConfig` with 6 new fields and add env wiring + Validate hook.**

Edit `app/config.go:67-79`:

```go
type IAMConfig struct {
	Mode           string
	MockUserID     string
	MockUserName   string
	MockTenantID   string
	MockTenantName string
	MockRoles      []string
	JWTSigningKey  string
	JWTIssuer      string
	JWTAudience    string
	JWTExpiry      int
	RequireJWT     bool

	// NEW: IAM feature surface for /oauth/keys/* — passed through to auth.IAMFeatures.
	TrustedKeyRegistrationEnabled bool
	TrustedKeyMaxPerTenant        int
	TrustedKeyMaxValidityDays     int
	TrustedKeyMaxJWKProperties    int
	KeypairDefaultValidityDays    int
	BootstrapAudience             string
}
```

In `DefaultConfig()` (around line 156), inside the `IAM: IAMConfig{...}` literal, append after `RequireJWT`:

```go
TrustedKeyRegistrationEnabled: envBool("CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED", false),
TrustedKeyMaxPerTenant:        envInt("CYODA_IAM_TRUSTED_KEY_MAX_PER_TENANT", 10),
TrustedKeyMaxValidityDays:     envInt("CYODA_IAM_TRUSTED_KEY_MAX_VALIDITY_DAYS", 365),
TrustedKeyMaxJWKProperties:    envInt("CYODA_IAM_TRUSTED_KEY_MAX_JWK_PROPERTIES", 20),
KeypairDefaultValidityDays:    envInt("CYODA_IAM_KEYPAIR_DEFAULT_VALIDITY_DAYS", 365),
BootstrapAudience:             envString("CYODA_JWT_BOOTSTRAP_AUDIENCE", "client"),
```

Add a tiny helper at the bottom of `app/config.go` (or extend the existing `ValidateIAM` at line 440):

```go
// AuthIAMFeatures projects the new IAM-feature fields out of IAMConfig
// into the auth-package value struct.
func (c IAMConfig) AuthIAMFeatures() auth.IAMFeatures {
	return auth.IAMFeatures{
		TrustedKeyRegistrationEnabled: c.TrustedKeyRegistrationEnabled,
		TrustedKeyMaxPerTenant:        c.TrustedKeyMaxPerTenant,
		TrustedKeyMaxValidityDays:     c.TrustedKeyMaxValidityDays,
		TrustedKeyMaxJWKProperties:    c.TrustedKeyMaxJWKProperties,
		KeypairDefaultValidityDays:    c.KeypairDefaultValidityDays,
		BootstrapAudience:             c.BootstrapAudience,
	}
}
```

Extend the existing `ValidateIAM` function to also call `c.AuthIAMFeatures().Validate()` and propagate any error.

Add `"github.com/cyoda-platform/cyoda-go/internal/auth"` to imports.

- [ ] **Step 3: Update Handler struct + New signature.**

Edit `internal/domain/account/handler.go`:

```go
type Handler struct {
	authSvc         contract.AuthenticationService
	authzSvc        contract.AuthorizationService
	keyStore        auth.KeyStore
	trustedKeyStore auth.TrustedKeyStore
	iam             auth.IAMFeatures
}

func New(authSvc contract.AuthenticationService, authzSvc contract.AuthorizationService,
	keyStore auth.KeyStore, trustedKeyStore auth.TrustedKeyStore, iam auth.IAMFeatures) *Handler {
	return &Handler{
		authSvc:         authSvc,
		authzSvc:        authzSvc,
		keyStore:        keyStore,
		trustedKeyStore: trustedKeyStore,
		iam:             iam,
	}
}
```

Add `"github.com/cyoda-platform/cyoda-go/internal/auth"` to imports.

- [ ] **Step 4: Delete the 10 stub methods from `handler.go`.** The stubs being deleted: `IssueJwtKeyPair`, `GetCurrentJwtKeyPair`, `DeleteJwtKeyPair`, `InvalidateJwtKeyPair`, `ReactivateJwtKeyPair`, `ListTrustedKeys`, `RegisterTrustedKey`, `DeleteTrustedKey`, `InvalidateTrustedKey`, `ReactivateTrustedKey`. The keep the `stub` helper for any remaining 501 stubs in the file.

After deletion, `handler.go` no longer satisfies `genapi.ServerInterface`. That's expected — the adapter files (Tasks 15-19) re-implement them.

- [ ] **Step 5: Update `account.New(...)` callsites in tests.**

Edit `internal/domain/account/handler_test.go` — change every `account.New(nil, nil)` (4 occurrences) to:

```go
account.New(nil, nil, nil, nil, auth.IAMFeatures{})
```

Add `"github.com/cyoda-platform/cyoda-go/internal/auth"` to imports.

- [ ] **Step 6: Update `app/app.go` callsite.**

```
grep -n "account.New(" app/app.go
```

Replace with:

```go
accountHandler := account.New(authSvc, authzSvc, authSvc.KeyStore(), authSvc.TrustedKeyStore(), cfg.IAM.AuthIAMFeatures())
```

- [ ] **Step 7: Wire bootstrap key with IAMFeatures defaults.**

Edit `internal/auth/service.go:61-70` — bootstrap KeyPair literal becomes:

```go
now := time.Now().UTC()
validTo := now.Add(time.Duration(config.IAMFeatures.KeypairDefaultValidityDays) * 24 * time.Hour)
kp := &KeyPair{
	KID:        signingKID,
	Audience:   config.IAMFeatures.BootstrapAudience,
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
if validTo.Sub(now) < 30*24*time.Hour {
	slog.Warn("bootstrap signing key expires within 30 days; rotate before expiry", "pkg", "auth", "kid", signingKID, "validTo", validTo.Format(time.RFC3339))
}
```

Add `IAMFeatures IAMFeatures` field to `AuthConfig` (around line 13-18):

```go
type AuthConfig struct {
	SigningKeyPEM   string
	Issuer          string
	ExpirySeconds   int
	TrustedKeyStore TrustedKeyStore
	IAMFeatures     IAMFeatures // NEW
}
```

Also: if `config.IAMFeatures.KeypairDefaultValidityDays` is 0 (zero value), apply defaults at the top of `NewAuthService` to keep tests with empty AuthConfig green:

```go
if config.IAMFeatures.KeypairDefaultValidityDays == 0 {
	config.IAMFeatures = DefaultIAMFeatures()
}
```

Add `"log/slog"` import if missing.

- [ ] **Step 8: Plumb config through to `AuthConfig.IAMFeatures` in `app/app.go`.**

Find `auth.NewAuthService(auth.AuthConfig{...})` call (search in `app/app.go` around lines 230-260 typically). Add:

```go
IAMFeatures: cfg.IAM.AuthIAMFeatures(),
```

to the AuthConfig literal.

- [ ] **Step 9: Add bootstrap-key WARN test.**

Create `internal/auth/service_bootstrap_test.go`:

```go
package auth_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/auth"
)

func TestNewAuthService_BootstrapKey_AudienceClient(t *testing.T) {
	svc, err := auth.NewAuthService(auth.AuthConfig{
		SigningKeyPEM: generateTestPEM(t),
		Issuer:        "cyoda",
		ExpirySeconds: 3600,
		IAMFeatures:   auth.DefaultIAMFeatures(), // BootstrapAudience="client"
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	kp, err := svc.KeyStore().GetActive("client")
	if err != nil {
		t.Fatalf("GetActive(client): %v", err)
	}
	if kp.Audience != "client" || kp.Algorithm != "RS256" {
		t.Errorf("bootstrap key wrong: %+v", kp)
	}
	if kp.ValidTo == nil {
		t.Error("bootstrap key should have ValidTo set")
	}
}

func TestNewAuthService_BootstrapKey_WARN_NearExpiry(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)

	feats := auth.DefaultIAMFeatures()
	feats.KeypairDefaultValidityDays = 7 // < 30 days -> WARN expected
	_, err := auth.NewAuthService(auth.AuthConfig{
		SigningKeyPEM: generateTestPEM(t),
		Issuer:        "cyoda",
		ExpirySeconds: 3600,
		IAMFeatures:   feats,
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if !strings.Contains(buf.String(), "bootstrap signing key expires within 30 days") {
		t.Errorf("expected WARN; got: %s", buf.String())
	}
}
```

(`generateTestPEM(t)` already exists in `internal/auth/integration_test.go` and is package-private; if the new file is in `auth_test` package, copy the helper here or expose via `service_test_helpers.go`.)

- [ ] **Step 10:** `go build ./... && go test ./... -short` → PASS.

- [ ] **Step 11:** Commit:

```bash
git add internal/auth/service.go internal/auth/service_bootstrap_test.go internal/domain/account/handler.go internal/domain/account/handler_test.go app/config.go app/app.go
git commit -m "feat(account+auth): Handler keyStore/trustedKeyStore/iam fields + IAMFeatures wiring

- account.Handler gains keyStore (KeyStore), trustedKeyStore (TrustedKeyStore),
  iam (IAMFeatures) fields. New signature: account.New(authSvc, authzSvc,
  keyStore, trustedKeyStore, iam).
- app.IAMConfig extended with 6 new fields (TrustedKeyRegistrationEnabled,
  3 caps, KeypairDefaultValidityDays, BootstrapAudience). Default values via
  env vars CYODA_IAM_*. AuthIAMFeatures() projects into auth.IAMFeatures.
- auth.AuthConfig gains IAMFeatures field. Bootstrap key saved with
  Audience=BootstrapAudience, Algorithm=RS256, ValidTo=ValidFrom+KeypairDefaultValidityDays.
  Startup WARN if ValidTo within 30 days.
- Tests: bootstrap key configured correctly + WARN emission near expiry.
- 10 stub methods deleted from handler.go (adapter files re-implement starting
  Task 15). handler_test.go callsites updated (4 occurrences).

Refs #281."
```


---

## Phase 5: Keypair adapters

### Task 15: `keys_adapter.go` scaffolding + IssueJwtKeyPair (RED-first)

**Files:** Create `internal/domain/account/keys_adapter.go`, `internal/domain/account/keys_adapter_test.go`.

- [ ] **Step 1: Write RED tests.** Write `internal/domain/account/keys_adapter_test.go`:

```go
package account_test

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/cyoda-platform/cyoda-go/internal/common/commontest"
	"github.com/cyoda-platform/cyoda-go/internal/domain/account"
)

func adminUC() *spi.UserContext {
	return &spi.UserContext{
		UserID: "u", UserName: "u",
		Tenant: spi.Tenant{ID: "t1", Name: "t1"},
		Roles:  []string{"ROLE_ADMIN"},
	}
}

func adminReq(t *testing.T, method, path string, body []byte) *http.Request {
	t.Helper()
	var br *bytes.Reader
	if body != nil {
		br = bytes.NewReader(body)
	}
	var req *http.Request
	if br == nil {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, br)
	}
	return req.WithContext(spi.WithUserContext(req.Context(), adminUC()))
}

// resultResp wraps *http.Response from a ResponseRecorder so we can use
// commontest.ExpectErrorCode (which takes *http.Response).
func resultResp(w *httptest.ResponseRecorder) *http.Response { return w.Result() }

func newHandler(t *testing.T) (*account.Handler, auth.KeyStore, auth.TrustedKeyStore) {
	t.Helper()
	ks := auth.NewInMemoryKeyStore()
	ts := auth.NewInMemoryTrustedKeyStore()
	h := account.New(nil, nil, ks, ts, auth.DefaultIAMFeatures())
	return h, ks, ts
}

func TestIssueJwtKeyPair_Happy(t *testing.T) {
	h, _, _ := newHandler(t)
	body, _ := json.Marshal(genapi.IssueJwtKeyPairRequestDto{Algorithm: "RS256", Audience: "client"})
	req := adminReq(t, "POST", "/oauth/keys/keypair", body)
	w := httptest.NewRecorder()
	h.IssueJwtKeyPair(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp genapi.JwtKeyPairResponseDto
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(resp.Algorithm) != "RS256" || resp.KeyId == "" {
		t.Errorf("resp: %+v", resp)
	}
	if _, err := base64.StdEncoding.DecodeString(resp.PublicKey); err != nil {
		t.Errorf("publicKey not base64-DER: %v", err)
	}
	if resp.ValidFrom.After(time.Now().Add(2 * time.Second)) {
		t.Error("validFrom in future")
	}
}

func TestIssueJwtKeyPair_RejectsNonRS256(t *testing.T) {
	h, _, _ := newHandler(t)
	body, _ := json.Marshal(genapi.IssueJwtKeyPairRequestDto{Algorithm: "ES256", Audience: "client"})
	req := adminReq(t, "POST", "/oauth/keys/keypair", body)
	w := httptest.NewRecorder()
	h.IssueJwtKeyPair(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", w.Code)
	}
	commontest.ExpectErrorCode(t, resultResp(w), "UNSUPPORTED_ALGORITHM")
	if ct := w.Header().Get("Content-Type"); !contains(ct, "application/problem+json") {
		t.Errorf("Content-Type=%q want application/problem+json", ct)
	}
}

func TestIssueJwtKeyPair_RejectsBadAudience(t *testing.T) {
	h, _, _ := newHandler(t)
	// audience: "robot" passes JSON decode for an unknown enum string but our
	// adapter rejects it. The generated typed enum is `string`-based so this works.
	body := []byte(`{"algorithm":"RS256","audience":"robot"}`)
	req := adminReq(t, "POST", "/oauth/keys/keypair", body)
	w := httptest.NewRecorder()
	h.IssueJwtKeyPair(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", w.Code)
	}
}

func TestIssueJwtKeyPair_401_NoAuth(t *testing.T) {
	h, _, _ := newHandler(t)
	body, _ := json.Marshal(genapi.IssueJwtKeyPairRequestDto{Algorithm: "RS256", Audience: "client"})
	req := httptest.NewRequest("POST", "/oauth/keys/keypair", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.IssueJwtKeyPair(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status=%d want 401", w.Code)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// rsa helpers (used here and in subsequent adapter tests).
func mkRSAKeyPair(t *testing.T, audience string) *auth.KeyPair {
	t.Helper()
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	return &auth.KeyPair{KID: "k", Audience: audience, Algorithm: "RS256", PublicKey: &priv.PublicKey, PrivateKey: priv, Active: true, ValidFrom: time.Now()}
}

func mkRSAPub(t *testing.T) *rsa.PublicKey {
	t.Helper()
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	return &priv.PublicKey
}
```

- [ ] **Step 2:** `go test ./internal/domain/account/ -run TestIssueJwtKeyPair -v` → FAIL.

- [ ] **Step 3: Write `keys_adapter.go`:**

```go
package account

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

func (h *Handler) IssueJwtKeyPair(w http.ResponseWriter, r *http.Request) {
	if !auth.RequireAdmin(w, r) {
		return
	}
	var req genapi.IssueJwtKeyPairRequestDto
	if err := boundedJSONDecode(w, r, 1<<20, &req); err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid request body"))
		return
	}
	if string(req.Algorithm) != "RS256" {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeUnsupportedAlgorithm, "only RS256 supported in this version"))
		return
	}
	if !isValidKeyPairAudience(string(req.Audience)) {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid audience"))
		return
	}
	now := time.Now().UTC()
	validFrom := now
	if req.ValidFrom != nil {
		validFrom = *req.ValidFrom
	}
	validTo := validFrom.Add(time.Duration(h.iam.KeypairDefaultValidityDays) * 24 * time.Hour)
	if req.ValidTo != nil {
		validTo = *req.ValidTo
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
	if req.InvalidateCurrent != nil {
		invalidate = *req.InvalidateCurrent
	}
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
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
		KID: kid, Audience: string(req.Audience), Algorithm: "RS256",
		PublicKey: &priv.PublicKey, PrivateKey: priv,
		Active: true, ValidFrom: validFrom, ValidTo: &vt,
	}
	if err := h.keyStore.Save(kp, auth.RotateOptions{Invalidate: invalidate, GracePeriodSec: grace}); err != nil {
		common.WriteError(w, r, common.Internal("keyStore.Save", err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(toJwtKeyPairResponse(kp))
}

func isValidKeyPairAudience(s string) bool { return s == "human" || s == "client" }

func toJwtKeyPairResponse(kp *auth.KeyPair) genapi.JwtKeyPairResponseDto {
	der, _ := x509.MarshalPKIXPublicKey(kp.PublicKey)
	resp := genapi.JwtKeyPairResponseDto{
		KeyId:     kp.KID,
		Algorithm: genapi.JwtKeyPairResponseDtoAlgorithm(kp.Algorithm),
		PublicKey: base64.StdEncoding.EncodeToString(der),
		ValidFrom: kp.ValidFrom,
	}
	if kp.ValidTo != nil {
		vt := *kp.ValidTo
		resp.ValidTo = &vt
	}
	return resp
}

func tenantFromCtx(r *http.Request) spi.TenantID {
	uc := spi.GetUserContext(r.Context())
	if uc == nil {
		return ""
	}
	return uc.Tenant.ID
}
```

- [ ] **Step 4:** `go test ./internal/domain/account/ -run TestIssueJwtKeyPair -v` → PASS.

- [ ] **Step 5:** Commit: `feat(account): IssueJwtKeyPair adapter (RS256-only, body-validated)` (refs #281).

---

### Task 16: `keypair_signing_test.go` — all 9 non-RS256 algorithms rejected (table-driven)

**Files:** Create `internal/auth/keypair_signing_test.go`.

- [ ] **Step 1: Write the test.**

```go
package auth_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/cyoda-platform/cyoda-go/internal/common/commontest"
	"github.com/cyoda-platform/cyoda-go/internal/domain/account"
)

// Ensures the v0.8.0 algorithm scope is locked: RS256 accepted; the 9 other
// algorithm enum values declared in OpenAPI all rejected with 400
// UNSUPPORTED_ALGORITHM at the adapter boundary. Multi-algorithm support
// is deferred to v0.8.1 (follow-up issue).
func TestAdapter_AlgorithmEnum_Coverage(t *testing.T) {
	uc := &spi.UserContext{UserID: "u", UserName: "u", Tenant: spi.Tenant{ID: "t1"}, Roles: []string{"ROLE_ADMIN"}}
	rejected := []string{"RS384", "RS512", "PS256", "PS384", "PS512", "ES256", "ES384", "ES512", "EdDSA"}
	for _, alg := range rejected {
		alg := alg
		t.Run(alg, func(t *testing.T) {
			h := account.New(nil, nil, auth.NewInMemoryKeyStore(), auth.NewInMemoryTrustedKeyStore(), auth.DefaultIAMFeatures())
			body, _ := json.Marshal(genapi.IssueJwtKeyPairRequestDto{Algorithm: genapi.IssueJwtKeyPairRequestDtoAlgorithm(alg), Audience: "client"})
			req := httptest.NewRequest("POST", "/oauth/keys/keypair", bytes.NewReader(body))
			req = req.WithContext(spi.WithUserContext(req.Context(), uc))
			w := httptest.NewRecorder()
			h.IssueJwtKeyPair(w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("%s: status=%d want 400", alg, w.Code)
			}
			commontest.ExpectErrorCode(t, w.Result(), "UNSUPPORTED_ALGORITHM")
		})
	}
}

func TestAdapter_AlgorithmEnum_RS256_HappyPath(t *testing.T) {
	uc := &spi.UserContext{UserID: "u", UserName: "u", Tenant: spi.Tenant{ID: "t1"}, Roles: []string{"ROLE_ADMIN"}}
	h := account.New(nil, nil, auth.NewInMemoryKeyStore(), auth.NewInMemoryTrustedKeyStore(), auth.DefaultIAMFeatures())
	body, _ := json.Marshal(genapi.IssueJwtKeyPairRequestDto{Algorithm: "RS256", Audience: "client"})
	req := httptest.NewRequest("POST", "/oauth/keys/keypair", bytes.NewReader(body))
	req = req.WithContext(spi.WithUserContext(req.Context(), uc))
	w := httptest.NewRecorder()
	h.IssueJwtKeyPair(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}
```

- [ ] **Step 2:** `go test ./internal/auth/ -run TestAdapter_AlgorithmEnum -v` → PASS (the adapter already rejects non-RS256; this test locks the scope).

- [ ] **Step 3:** Commit: `test(auth): keypair signing table — all 9 non-RS256 algorithms rejected` (refs #281).

---

### Task 17: GetCurrentJwtKeyPair adapter

**Files:** Modify `internal/domain/account/keys_adapter.go`, `internal/domain/account/keys_adapter_test.go`.

- [ ] **Step 1:** Append failing tests:

```go
func TestGetCurrentJwtKeyPair_Happy(t *testing.T) {
	h, ks, _ := newHandler(t)
	_ = ks.Save(mkRSAKeyPair(t, "client"), auth.RotateOptions{})
	req := adminReq(t, "GET", "/oauth/keys/keypair/current?audience=client", nil)
	w := httptest.NewRecorder()
	h.GetCurrentJwtKeyPair(w, req, genapi.GetCurrentJwtKeyPairParams{Audience: "client"})
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestGetCurrentJwtKeyPair_404_NoKeyForAudience(t *testing.T) {
	h, _, _ := newHandler(t)
	req := adminReq(t, "GET", "/oauth/keys/keypair/current?audience=human", nil)
	w := httptest.NewRecorder()
	h.GetCurrentJwtKeyPair(w, req, genapi.GetCurrentJwtKeyPairParams{Audience: "human"})
	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d", w.Code)
	}
	commontest.ExpectErrorCode(t, w.Result(), "KEYPAIR_NOT_FOUND")
}
```

- [ ] **Step 2:** `go test ./internal/domain/account/ -run TestGetCurrentJwtKeyPair -v` → FAIL.

- [ ] **Step 3: Append to `keys_adapter.go`:**

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

- [ ] **Step 4:** PASS + commit: `feat(account): GetCurrentJwtKeyPair adapter` (refs #281).

---

### Task 18: Delete + Invalidate + Reactivate JwtKeyPair adapters

**Files:** Modify `internal/domain/account/keys_adapter.go`, `internal/domain/account/keys_adapter_test.go`.

- [ ] **Step 1: Write RED tests.**

```go
func TestDeleteJwtKeyPair(t *testing.T) {
	h, ks, _ := newHandler(t)
	kp := mkRSAKeyPair(t, "client")
	_ = ks.Save(kp, auth.RotateOptions{})
	w := httptest.NewRecorder()
	h.DeleteJwtKeyPair(w, adminReq(t, "DELETE", "/", nil), "k")
	if w.Code != http.StatusNoContent {
		t.Fatalf("status=%d", w.Code)
	}
	if _, err := ks.Get("k"); err == nil {
		t.Error("expected deleted")
	}
}

func TestDeleteJwtKeyPair_404(t *testing.T) {
	h, _, _ := newHandler(t)
	w := httptest.NewRecorder()
	h.DeleteJwtKeyPair(w, adminReq(t, "DELETE", "/", nil), "missing")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d", w.Code)
	}
	commontest.ExpectErrorCode(t, w.Result(), "KEYPAIR_NOT_FOUND")
}

func TestInvalidateJwtKeyPair_GraceDefaultZero(t *testing.T) {
	h, ks, _ := newHandler(t)
	now := time.Now()
	kp := mkRSAKeyPair(t, "client")
	_ = ks.Save(kp, auth.RotateOptions{})
	w := httptest.NewRecorder()
	h.InvalidateJwtKeyPair(w, adminReq(t, "POST", "/", []byte(`{}`)), "k")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	got, _ := ks.Get("k")
	if got.ValidTo == nil || !got.ValidTo.Before(now.Add(2*time.Second)) {
		t.Errorf("expected ValidTo near now (grace=0); got %v", got.ValidTo)
	}
}

func TestInvalidateJwtKeyPair_NegativeGraceRejected(t *testing.T) {
	h, ks, _ := newHandler(t)
	_ = ks.Save(mkRSAKeyPair(t, "client"), auth.RotateOptions{})
	w := httptest.NewRecorder()
	h.InvalidateJwtKeyPair(w, adminReq(t, "POST", "/", []byte(`{"gracePeriodSec":-5}`)), "k")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", w.Code)
	}
}

func TestReactivateJwtKeyPair_RequiresFreshValidTo(t *testing.T) {
	h, ks, _ := newHandler(t)
	past := time.Now().Add(-1 * time.Hour)
	priv := &auth.KeyPair{KID: "k", Audience: "client", Algorithm: "RS256", PublicKey: mkRSAPub(t), Active: false, ValidFrom: past, ValidTo: &past}
	_ = ks.Save(priv, auth.RotateOptions{})

	// Missing validTo
	w := httptest.NewRecorder()
	h.ReactivateJwtKeyPair(w, adminReq(t, "POST", "/", []byte(`{}`)), "k")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing validTo: status=%d", w.Code)
	}

	// Past validTo
	body, _ := json.Marshal(genapi.ReactivateKeyRequestDto{ValidTo: past})
	w = httptest.NewRecorder()
	h.ReactivateJwtKeyPair(w, adminReq(t, "POST", "/", body), "k")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("past validTo: status=%d", w.Code)
	}

	// Fresh validTo
	future := time.Now().Add(24 * time.Hour)
	body, _ = json.Marshal(genapi.ReactivateKeyRequestDto{ValidTo: future})
	w = httptest.NewRecorder()
	h.ReactivateJwtKeyPair(w, adminReq(t, "POST", "/", body), "k")
	if w.Code != http.StatusOK {
		t.Fatalf("fresh validTo: status=%d body=%s", w.Code, w.Body.String())
	}
}
```

- [ ] **Step 2:** Append to `keys_adapter.go`:

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
	if req.ValidTo.IsZero() {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "validTo required"))
		return
	}
	validFrom := time.Now()
	if req.ValidFrom != nil {
		validFrom = *req.ValidFrom
	}
	validTo := req.ValidTo
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

- [ ] **Step 3:** PASS + commit: `feat(account): Delete/Invalidate/Reactivate JwtKeyPair adapters` (refs #281).


---

## Phase 6: Trusted-key adapters

### Task 19: `trusted_adapter.go` scaffolding + RegisterTrustedKey

**Files:** Create `internal/domain/account/trusted_adapter.go`, `internal/domain/account/trusted_adapter_test.go`.

- [ ] **Step 1: Write RED tests.**

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
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/cyoda-platform/cyoda-go/internal/common/commontest"
	"github.com/cyoda-platform/cyoda-go/internal/domain/account"
)

func rsaJWK(t *testing.T, kid string) map[string]interface{} {
	t.Helper()
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	n := base64.RawURLEncoding.EncodeToString(priv.PublicKey.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(priv.PublicKey.E)).Bytes())
	return map[string]interface{}{"kty": "RSA", "kid": kid, "n": n, "e": e}
}

func enabledHandler(t *testing.T) *account.Handler {
	t.Helper()
	feats := auth.DefaultIAMFeatures()
	feats.TrustedKeyRegistrationEnabled = true
	return account.New(nil, nil, auth.NewInMemoryKeyStore(), auth.NewInMemoryTrustedKeyStore(), feats)
}

func TestRegisterTrustedKey_Happy(t *testing.T) {
	h := enabledHandler(t)
	body, _ := json.Marshal(genapi.RegisterTrustedKeyRequestDto{KeyId: "k1", Jwk: rsaJWK(t, "k1"), Audience: "human"})
	req := adminReq(t, "POST", "/oauth/keys/trusted", body)
	w := httptest.NewRecorder()
	h.RegisterTrustedKey(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp genapi.TrustedKeyResponseDto
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.KeyId != "k1" || resp.LegalEntityId != "t1" || resp.Jwk["kty"] != "RSA" {
		t.Errorf("resp: %+v", resp)
	}
}

func TestRegisterTrustedKey_FlagDisabled_404(t *testing.T) {
	feats := auth.DefaultIAMFeatures() // default false
	h := account.New(nil, nil, auth.NewInMemoryKeyStore(), auth.NewInMemoryTrustedKeyStore(), feats)
	body, _ := json.Marshal(genapi.RegisterTrustedKeyRequestDto{KeyId: "k1", Jwk: rsaJWK(t, "k1"), Audience: "human"})
	w := httptest.NewRecorder()
	h.RegisterTrustedKey(w, adminReq(t, "POST", "/", body))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d", w.Code)
	}
	commontest.ExpectErrorCode(t, w.Result(), "FEATURE_DISABLED")
}

func TestRegisterTrustedKey_KidKeyIdMismatch_400(t *testing.T) {
	h := enabledHandler(t)
	jwk := rsaJWK(t, "evil")
	body, _ := json.Marshal(genapi.RegisterTrustedKeyRequestDto{KeyId: "good", Jwk: jwk, Audience: "human"})
	w := httptest.NewRecorder()
	h.RegisterTrustedKey(w, adminReq(t, "POST", "/", body))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", w.Code)
	}
}

func TestRegisterTrustedKey_NonRSA_400_UnsupportedKeyType(t *testing.T) {
	h := enabledHandler(t)
	body, _ := json.Marshal(genapi.RegisterTrustedKeyRequestDto{KeyId: "k1", Jwk: map[string]interface{}{"kty": "EC", "kid": "k1", "crv": "P-256", "x": "abc", "y": "def"}, Audience: "human"})
	w := httptest.NewRecorder()
	h.RegisterTrustedKey(w, adminReq(t, "POST", "/", body))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", w.Code)
	}
	commontest.ExpectErrorCode(t, w.Result(), "UNSUPPORTED_KEY_TYPE")
}

func TestRegisterTrustedKey_CrossTenantCollision_409(t *testing.T) {
	ts := auth.NewInMemoryTrustedKeyStore()
	pre := &auth.TrustedKey{KID: "shared", TenantID: spi.TenantID("tenant-a"), PublicKey: mkRSAPub(t), Audience: "human", Active: true, ValidFrom: time.Now()}
	_ = ts.Register(pre, auth.RotateOptions{})
	feats := auth.DefaultIAMFeatures()
	feats.TrustedKeyRegistrationEnabled = true
	h := account.New(nil, nil, auth.NewInMemoryKeyStore(), ts, feats)
	uc := &spi.UserContext{UserID: "u", UserName: "u", Tenant: spi.Tenant{ID: "tenant-b"}, Roles: []string{"ROLE_ADMIN"}}
	body, _ := json.Marshal(genapi.RegisterTrustedKeyRequestDto{KeyId: "shared", Jwk: rsaJWK(t, "shared"), Audience: "human"})
	req := httptest.NewRequest("POST", "/", bytes.NewReader(body)).WithContext(spi.WithUserContext(httptest.NewRequest("POST", "/", nil).Context(), uc))
	w := httptest.NewRecorder()
	h.RegisterTrustedKey(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("status=%d", w.Code)
	}
	commontest.ExpectErrorCode(t, w.Result(), "KEY_OWNED_BY_DIFFERENT_TENANT")
}
```

- [ ] **Step 2:** RED.

- [ ] **Step 3: Write `trusted_adapter.go`:**

```go
package account

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// gateTrustedKeyFeature returns false if the feature flag is off; the
// adapter has already written 404 FEATURE_DISABLED.
func (h *Handler) gateTrustedKeyFeature(w http.ResponseWriter, r *http.Request) bool {
	if !h.iam.TrustedKeyRegistrationEnabled {
		common.WriteError(w, r, common.Operational(http.StatusNotFound, common.ErrCodeFeatureDisabled, "trusted-key registration is disabled"))
		return false
	}
	return true
}

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
	if !auth.MatchesTrustedKIDPattern(req.KeyId) {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid keyId format"))
		return
	}
	pub, errCode, jwkErr := parseTrustedJWK(req.Jwk, req.KeyId, h.iam.TrustedKeyMaxJWKProperties)
	if jwkErr != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, errCode, jwkErr.Error()))
		return
	}
	if !isValidKeyPairAudience(string(req.Audience)) {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid audience"))
		return
	}
	now := time.Now()
	validFrom := now
	if req.ValidFrom != nil {
		validFrom = *req.ValidFrom
	}
	validTo := validFrom.Add(time.Duration(h.iam.TrustedKeyMaxValidityDays) * 24 * time.Hour)
	if req.ValidTo != nil {
		validTo = *req.ValidTo
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
		KID: req.KeyId, TenantID: tenantFromCtx(r), JWK: req.Jwk, PublicKey: pub,
		Audience: string(req.Audience), Issuers: issuers,
		Active: true, ValidFrom: validFrom, ValidTo: &vt,
	}
	if err := h.trustedKeyStore.Register(tk, auth.RotateOptions{Invalidate: invalidate, GracePeriodSec: grace}); err != nil {
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

// parseTrustedJWK enforces JWK content checks. Returns (pub, code, err);
// code is the error-code constant for the eventual 400 ProblemDetail.
func parseTrustedJWK(jwk map[string]any, keyId string, maxProps int) (pub *rsa.PublicKey, code string, err error) {
	if len(jwk) > maxProps {
		return nil, common.ErrCodeBadRequest, fmt.Errorf("jwk has too many properties (%d > %d)", len(jwk), maxProps)
	}
	ktyAny, ok := jwk["kty"]
	if !ok {
		return nil, common.ErrCodeBadRequest, fmt.Errorf("jwk missing kty")
	}
	kty, _ := ktyAny.(string)
	if kty == "" {
		return nil, common.ErrCodeBadRequest, fmt.Errorf("jwk kty must be a string")
	}
	if kty != "RSA" {
		return nil, common.ErrCodeUnsupportedKeyType, fmt.Errorf("only RSA JWKs supported (v0.8.0)")
	}
	if rawKid, ok := jwk["kid"]; ok {
		s, _ := rawKid.(string)
		if s != keyId {
			return nil, common.ErrCodeBadRequest, fmt.Errorf("jwk.kid (%q) must equal keyId (%q)", s, keyId)
		}
	}
	raw, err := json.Marshal(jwk)
	if err != nil {
		return nil, common.ErrCodeBadRequest, fmt.Errorf("re-marshal jwk: %w", err)
	}
	pubKey, err := auth.ParseRSAPublicKeyFromJWK(raw)
	if err != nil {
		return nil, common.ErrCodeBadRequest, fmt.Errorf("invalid jwk: %w", err)
	}
	return pubKey, "", nil
}

func toTrustedKeyResponse(tk *auth.TrustedKey) genapi.TrustedKeyResponseDto {
	resp := genapi.TrustedKeyResponseDto{
		KeyId: tk.KID, LegalEntityId: string(tk.TenantID),
		Jwk:       tk.JWK,
		Audience:  genapi.TrustedKeyResponseDtoAudience(tk.Audience),
		ValidFrom: tk.ValidFrom,
	}
	if tk.Issuers != nil {
		s := tk.Issuers
		resp.Issuers = &s
	}
	if tk.ValidTo != nil {
		vt := *tk.ValidTo
		resp.ValidTo = &vt
	}
	return resp
}
```

Add `"crypto/rsa"` to imports.

- [ ] **Step 4:** PASS + commit: `feat(account): RegisterTrustedKey adapter (flag-gated, JWK-validated)` (refs #281).

---

### Task 20: List / Delete / Invalidate / Reactivate TrustedKey adapters

**Files:** Modify `internal/domain/account/trusted_adapter.go`, `internal/domain/account/trusted_adapter_test.go`.

- [ ] **Step 1: Write RED tests.**

```go
func TestListTrustedKeys_TenantScoped(t *testing.T) {
	ts := auth.NewInMemoryTrustedKeyStore()
	mine := &auth.TrustedKey{KID: "mine", TenantID: spi.TenantID("t1"), PublicKey: mkRSAPub(t), Audience: "human", Active: true, ValidFrom: time.Now(), JWK: map[string]any{"kty": "RSA", "kid": "mine"}}
	theirs := &auth.TrustedKey{KID: "theirs", TenantID: spi.TenantID("other"), PublicKey: mkRSAPub(t), Audience: "human", Active: true, ValidFrom: time.Now(), JWK: map[string]any{"kty": "RSA", "kid": "theirs"}}
	_ = ts.Register(mine, auth.RotateOptions{})
	_ = ts.Register(theirs, auth.RotateOptions{})
	feats := auth.DefaultIAMFeatures()
	feats.TrustedKeyRegistrationEnabled = true
	h := account.New(nil, nil, auth.NewInMemoryKeyStore(), ts, feats)
	w := httptest.NewRecorder()
	h.ListTrustedKeys(w, adminReq(t, "GET", "/oauth/keys/trusted", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	var resp []genapi.TrustedKeyResponseDto
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp) != 1 || resp[0].KeyId != "mine" {
		t.Fatalf("expected only 'mine', got %+v", resp)
	}
}

func TestDeleteTrustedKey_CrossTenant_404(t *testing.T) {
	ts := auth.NewInMemoryTrustedKeyStore()
	tk := &auth.TrustedKey{KID: "k", TenantID: spi.TenantID("other"), PublicKey: mkRSAPub(t), Audience: "human", Active: true, ValidFrom: time.Now()}
	_ = ts.Register(tk, auth.RotateOptions{})
	feats := auth.DefaultIAMFeatures()
	feats.TrustedKeyRegistrationEnabled = true
	h := account.New(nil, nil, auth.NewInMemoryKeyStore(), ts, feats)
	w := httptest.NewRecorder()
	h.DeleteTrustedKey(w, adminReq(t, "DELETE", "/", nil), "k")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d", w.Code)
	}
}

func TestInvalidateTrustedKey_Grace(t *testing.T) {
	ts := auth.NewInMemoryTrustedKeyStore()
	tk := &auth.TrustedKey{KID: "k", TenantID: spi.TenantID("t1"), PublicKey: mkRSAPub(t), Audience: "human", Active: true, ValidFrom: time.Now(), JWK: map[string]any{"kty": "RSA", "kid": "k"}}
	_ = ts.Register(tk, auth.RotateOptions{})
	feats := auth.DefaultIAMFeatures()
	feats.TrustedKeyRegistrationEnabled = true
	h := account.New(nil, nil, auth.NewInMemoryKeyStore(), ts, feats)
	body, _ := json.Marshal(genapi.InvalidateKeyRequestDto{GracePeriodSec: ptrInt64(60)})
	w := httptest.NewRecorder()
	h.InvalidateTrustedKey(w, adminReq(t, "POST", "/", body), "k")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	got, _ := ts.Get(spi.TenantID("t1"), "k")
	if got.Active || got.ValidTo == nil {
		t.Errorf("expected invalidated; got %+v", got)
	}
}

func TestReactivateTrustedKey_RequiresValidTo(t *testing.T) {
	ts := auth.NewInMemoryTrustedKeyStore()
	past := time.Now().Add(-1 * time.Hour)
	tk := &auth.TrustedKey{KID: "k", TenantID: spi.TenantID("t1"), PublicKey: mkRSAPub(t), Audience: "human", Active: false, ValidFrom: past, ValidTo: &past, JWK: map[string]any{"kty": "RSA", "kid": "k"}}
	_ = ts.Register(tk, auth.RotateOptions{})
	feats := auth.DefaultIAMFeatures()
	feats.TrustedKeyRegistrationEnabled = true
	h := account.New(nil, nil, auth.NewInMemoryKeyStore(), ts, feats)
	body, _ := json.Marshal(genapi.ReactivateKeyRequestDto{ValidTo: time.Now().Add(24 * time.Hour)})
	w := httptest.NewRecorder()
	h.ReactivateTrustedKey(w, adminReq(t, "POST", "/", body), "k")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func ptrInt64(v int64) *int64 { return &v }
```

- [ ] **Step 2:** Append to `trusted_adapter.go`:

```go
func (h *Handler) ListTrustedKeys(w http.ResponseWriter, r *http.Request) {
	if !auth.RequireAdmin(w, r) {
		return
	}
	if !h.gateTrustedKeyFeature(w, r) {
		return
	}
	tID := tenantFromCtx(r)
	keys := h.trustedKeyStore.List(tID)
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
	tID := tenantFromCtx(r)
	if err := h.trustedKeyStore.Delete(tID, keyId); err != nil {
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
	tID := tenantFromCtx(r)
	if err := h.trustedKeyStore.Invalidate(tID, keyId, grace); err != nil {
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
	if req.ValidTo.IsZero() {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "validTo required"))
		return
	}
	validFrom := time.Now()
	if req.ValidFrom != nil {
		validFrom = *req.ValidFrom
	}
	validTo := req.ValidTo
	if !validTo.After(time.Now()) {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "validTo must be in the future"))
		return
	}
	if !validTo.After(validFrom) {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "validTo must be > validFrom"))
		return
	}
	tID := tenantFromCtx(r)
	if err := h.trustedKeyStore.Reactivate(tID, keyId, validFrom, validTo); err != nil {
		common.WriteError(w, r, common.Operational(http.StatusNotFound, common.ErrCodeTrustedKeyNotFound, "trusted key not found"))
		return
	}
	w.WriteHeader(http.StatusOK)
}
```

- [ ] **Step 3:** PASS + commit: `feat(account): List/Delete/Invalidate/Reactivate TrustedKey adapters` (refs #281).


---

## Phase 7: Wiring + cleanup

### Task 21: Remove `mux.Handle("/oauth/keys/", ...)` in `app/app.go`

**Files:** Modify `app/app.go`.

- [ ] **Step 1:** `grep -n 'mux.Handle("/oauth/keys/"' app/app.go` → expect line 482.

- [ ] **Step 2:** Delete the line. Keep `/account/m2m/` lines (they belong to #194-B).

- [ ] **Step 3: Verify chi routes the new adapters.** Confirm chi's `HandlerFromMux(accountHandler, ...)` (or equivalent in this codebase) mounts the 10 keypair/trusted-key paths.

```
grep -n "HandlerFromMux\|server.Account" app/app.go
```

Confirm the `accountHandler` from Task 14 is the receiver.

- [ ] **Step 4:** `go build ./... && go test ./internal/auth/ -short` → PASS.

- [ ] **Step 5:** Commit: `refactor(app): remove legacy /oauth/keys/ prefix mux entry` (refs #281).

---

### Task 22: Remove relocated body-size sub-test

**Files:** Modify `internal/auth/integration_test.go`.

- [ ] **Step 1:** Delete the `t.Run("trusted key register endpoint rejects oversized body", ...)` block (the existing trusted-key sub-test). Keep the M2M sub-test.

- [ ] **Step 2:** `go test ./internal/auth/ -run TestIntegration_JWTMode_RequestBodySizeLimit -v` → PASS.

- [ ] **Step 3:** Commit: `test(auth): drop relocated trusted-key body-size sub-test` (refs #281).

---

## Phase 8: E2E coverage

### Task 23: E2E happy-paths for all 10 ops

**Files:** Create `internal/e2e/oauth_keys_test.go`.

- [ ] **Step 1: Inspect existing E2E scaffolding.**

```
ls internal/e2e/
head -80 internal/e2e/e2e_test.go
```

Identify how `TestMain` starts the server, how a bootstrap M2M token is acquired, how authenticated requests are made. If `startTestServer`, `adminBearer` etc. don't already exist as helpers, factor them out into a small `internal/e2e/helpers_test.go` before writing Task 23.

- [ ] **Step 2:** Write `internal/e2e/oauth_keys_test.go` with one happy-path test per operation. Each test starts the test server, obtains an admin bearer token, executes the operation, asserts status + DTO shape. Trusted-key tests pass `CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED=true`. See sketches in Phase 8/9 tasks below for body shape.

- [ ] **Step 3:** `go test ./internal/e2e/ -run TestE2E -v` → PASS (Docker required for postgres testcontainers).

- [ ] **Step 4:** Commit: `test(e2e): happy-path coverage for the 10 /oauth/keys/* ops` (refs #281).

---

### Task 24: E2E grace + persistence + body-size

**Files:** Modify `internal/e2e/oauth_keys_test.go`.

- [ ] **Step 1: Grace-period test.** Issue keypair A, then issue keypair B with `invalidateCurrent=true, invalidateGracePeriodSec=2`. Assert A's kid is in `/.well-known/jwks.json` immediately; `time.Sleep(3*time.Second)`; assert A's kid no longer in JWKS.

- [ ] **Step 2: Persistence test.** Start server with a KV factory, register a trusted key, restart the in-process server against the same factory, assert `GET /oauth/keys/trusted` still returns the key.

- [ ] **Step 3: Body-size E2E.** POST > 1 MB body to `/oauth/keys/trusted` through the chi router → expect 400 (per Task 13's helper). Same for `/oauth/keys/keypair`.

- [ ] **Step 4:** Commit: `test(e2e): grace-period + persistence + body-size assertions` (refs #281).

---

### Task 25: E2E cross-tenant + feature flag + token-exchange invariant

**Files:** Modify `internal/e2e/oauth_keys_test.go`.

- [ ] **Step 1: Cross-tenant.** Bootstrap two M2M clients in distinct tenants A and B (use `CYODA_BOOTSTRAP_*` env or factor a multi-tenant test helper). Tenant A registers `keyId=shared`; tenant B tries the same → 409 `KEY_OWNED_BY_DIFFERENT_TENANT`. Tenant B `GET /oauth/keys/trusted` returns empty list.

- [ ] **Step 2: Feature flag.** Start a server with `TrustedKeyRegistrationEnabled=false`. All 5 trusted-key endpoints return 404 `FEATURE_DISABLED`. Keypair endpoints work normally.

- [ ] **Step 3: Token-exchange invariant.** Register trusted key with tenant A. Mint a subject token signed by that key with `caas_org_id=A`. Present via grant `urn:ietf:params:oauth:grant-type:token-exchange` from tenant A's M2M client → token issued. Repeat with `caas_org_id=B` claim from tenant A's client → rejected (existing line-203 check). Repeat with tenant B's M2M client claiming `caas_org_id=A` → rejected.

If the token-exchange infrastructure is hard to wire, DO NOT `t.Skip`. Instead, factor the helper so the test runs. Unit-level coverage in `internal/auth/token_test.go` (Task 7) is the safety net for the verification path itself.

- [ ] **Step 4:** Commit: `test(e2e): cross-tenant isolation + feature flag + token-exchange invariant` (refs #281).

---

## Phase 9: Regression-lock tests (per documented divergence)

### Task 26: Per-divergence regression-lock tests in adapter layer

**Files:** Modify `internal/domain/account/keys_adapter_test.go`, `internal/domain/account/trusted_adapter_test.go`.

For each documented divergence in spec §3.2, one regression-lock test that fails if a future contributor "fixes toward cloud".

- [ ] **Step 1: ROLE_ADMIN only (not SUPER_USER).**

```go
func TestRegression_RoleGate_RoleAdminOnly(t *testing.T) {
	h, _, _ := newHandler(t)
	uc := &spi.UserContext{UserID: "u", UserName: "u", Tenant: spi.Tenant{ID: "t1"}, Roles: []string{"SUPER_USER"}}
	req := httptest.NewRequest("GET", "/", nil).WithContext(spi.WithUserContext(httptest.NewRequest("GET", "/", nil).Context(), uc))
	w := httptest.NewRecorder()
	h.GetCurrentJwtKeyPair(w, req, genapi.GetCurrentJwtKeyPairParams{Audience: "client"})
	if w.Code != http.StatusForbidden {
		t.Errorf("SUPER_USER should not be admitted; want 403, got %d", w.Code)
	}
}
```

- [ ] **Step 2: Cross-tenant lifecycle 404 (covered by TestDeleteTrustedKey_CrossTenant_404).** Add explicit cross-reference comment.

- [ ] **Step 3: Trusted-key audience round-trip.**

```go
func TestRegression_TrustedAudienceRoundTrip(t *testing.T) {
	h := enabledHandler(t)
	body, _ := json.Marshal(genapi.RegisterTrustedKeyRequestDto{KeyId: "k1", Jwk: rsaJWK(t, "k1"), Audience: "client"})
	w := httptest.NewRecorder()
	h.RegisterTrustedKey(w, adminReq(t, "POST", "/", body))
	var resp genapi.TrustedKeyResponseDto
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if string(resp.Audience) != "client" {
		t.Errorf("expected audience='client' (cloud coerces to 'human'); got %q", resp.Audience)
	}
}
```

- [ ] **Step 4: Reactivate fresh validTo (already covered).** Cross-reference.

- [ ] **Step 5: Strict input — validTo<validFrom.**

```go
func TestRegression_StrictValidation_ValidToBeforeValidFrom(t *testing.T) {
	h, _, _ := newHandler(t)
	from := time.Now().Add(2 * time.Hour)
	to := time.Now().Add(1 * time.Hour)
	body, _ := json.Marshal(genapi.IssueJwtKeyPairRequestDto{Algorithm: "RS256", Audience: "client", ValidFrom: &from, ValidTo: &to})
	w := httptest.NewRecorder()
	h.IssueJwtKeyPair(w, adminReq(t, "POST", "/", body))
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for validTo<validFrom; got %d", w.Code)
	}
}
```

- [ ] **Step 6: Strict input — negative gracePeriodSec.** (Covered in Task 18 already; cross-reference.)

- [ ] **Step 7: gracePeriodSec default 0 (cloud 3600).** (Covered in Task 18 `TestInvalidateJwtKeyPair_GraceDefaultZero`; cross-reference.)

- [ ] **Step 8: Same-tenant silent upsert.**

```go
func TestRegression_SameTenantSilentUpsert(t *testing.T) {
	h := enabledHandler(t)
	for i := 0; i < 2; i++ {
		body, _ := json.Marshal(genapi.RegisterTrustedKeyRequestDto{KeyId: "k", Jwk: rsaJWK(t, "k"), Audience: "human"})
		w := httptest.NewRecorder()
		h.RegisterTrustedKey(w, adminReq(t, "POST", "/", body))
		if w.Code != http.StatusCreated && w.Code != http.StatusOK {
			t.Errorf("iteration %d: status=%d", i, w.Code)
		}
	}
}
```

- [ ] **Step 9: Non-RS256 rejected (covered by Task 16 table).** Cross-reference.

- [ ] **Step 10: JWKS-retained (covered by Task 24 grace E2E).** Cross-reference.

- [ ] **Step 11:** Commit: `test(account): per-divergence regression-lock suite` (refs #281).


---

## Phase 10: Documentation

### Task 27: Six new help-topic files

**Files:** Create 6 files under `cmd/cyoda/help/content/errors/`.

- [ ] **Step 1: Inspect template.** Open `cmd/cyoda/help/content/errors/TRUSTED_KEY_NOT_FOUND.md` to confirm frontmatter shape and section order.

- [ ] **Step 2: Write each new file.** Use rev4 spec §10.3 copy verbatim:

`FEATURE_DISABLED.md`:

```markdown
---
topic: errors.FEATURE_DISABLED
title: "FEATURE_DISABLED — feature is not enabled"
stability: stable
see_also:
  - errors
  - config.auth
---

# errors.FEATURE_DISABLED

## NAME

FEATURE_DISABLED — the operation belongs to an optional feature not enabled in this deployment.

## SYNOPSIS

HTTP: `404` `Not Found`. Retryable: `no`.

## DESCRIPTION

Returned by trusted-key endpoints when `CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED=false` (the default):

- `GET /oauth/keys/trusted`
- `POST /oauth/keys/trusted`
- `DELETE /oauth/keys/trusted/{keyId}`
- `POST /oauth/keys/trusted/{keyId}/invalidate`
- `POST /oauth/keys/trusted/{keyId}/reactivate`

Enable by setting the env var and restarting. Keypair endpoints (`/oauth/keys/keypair/*`) are unaffected.

## SEE ALSO

- errors
- config.auth
```

`UNSUPPORTED_ALGORITHM.md`:

```markdown
---
topic: errors.UNSUPPORTED_ALGORITHM
title: "UNSUPPORTED_ALGORITHM — algorithm not supported"
stability: stable
see_also:
  - errors
---

# errors.UNSUPPORTED_ALGORITHM

## NAME

UNSUPPORTED_ALGORITHM — the requested JWT algorithm is not implemented in this version.

## SYNOPSIS

HTTP: `400` `Bad Request`. Retryable: `no`.

## DESCRIPTION

cyoda-go v0.8.0 signs and verifies only `RS256`. Other enum values declared in the OpenAPI spec (`RS384`, `RS512`, `PS256`, `PS384`, `PS512`, `ES256`, `ES384`, `ES512`, `EdDSA`) are rejected with this error. Cyoda Cloud supports the full enum; parity is tracked in a v0.8.1 follow-up.

Use `algorithm: RS256` or omit the field.

## SEE ALSO

- errors
```

`UNSUPPORTED_KEY_TYPE.md`:

```markdown
---
topic: errors.UNSUPPORTED_KEY_TYPE
title: "UNSUPPORTED_KEY_TYPE — JWK kty not supported"
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

`POST /oauth/keys/trusted` accepts only `kty: "RSA"` in v0.8.0. Cloud also supports `kty: "EC"` and `kty: "OKP"`; cyoda-go parity is tracked in a v0.8.1 follow-up.

## SEE ALSO

- errors
- errors.UNSUPPORTED_ALGORITHM
```

`KEY_OWNED_BY_DIFFERENT_TENANT.md`:

```markdown
---
topic: errors.KEY_OWNED_BY_DIFFERENT_TENANT
title: "KEY_OWNED_BY_DIFFERENT_TENANT — trusted-key collision with another tenant"
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

Trusted keys are tenant-scoped. When `POST /oauth/keys/trusted` is called with a `keyId` that already belongs to a different tenant, the request is rejected with `409`. Pick a fresh `keyId` (the caller cannot see or affect the other tenant's keys).

## SEE ALSO

- errors
- errors.TRUSTED_KEY_NOT_FOUND
```

`KEYPAIR_NOT_FOUND.md`:

```markdown
---
topic: errors.KEYPAIR_NOT_FOUND
title: "KEYPAIR_NOT_FOUND — signing keypair not found"
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

Verify the keyId, or check the bootstrap-key audience configuration via `CYODA_JWT_BOOTSTRAP_AUDIENCE`.

## SEE ALSO

- errors
- errors.NOT_FOUND
```

`TRUSTED_KEY_CAP_REACHED.md`:

```markdown
---
topic: errors.TRUSTED_KEY_CAP_REACHED
title: "TRUSTED_KEY_CAP_REACHED — tenant trusted-key cap reached"
stability: stable
see_also:
  - errors
  - config.auth
---

# errors.TRUSTED_KEY_CAP_REACHED

## NAME

TRUSTED_KEY_CAP_REACHED — the tenant has reached the maximum registered trusted keys.

## SYNOPSIS

HTTP: `400` `Bad Request`. Retryable: `no`.

## DESCRIPTION

`POST /oauth/keys/trusted` enforces a per-tenant cap (default 10, configurable via `CYODA_IAM_TRUSTED_KEY_MAX_PER_TENANT`). The cap counts only currently-valid keys (Active and not past `validTo`). Delete or invalidate older keys, or raise the cap.

## SEE ALSO

- errors
- config.auth
```

- [ ] **Step 3:** Commit: `docs(help): 6 new error topics for /oauth/keys/* conformance` (refs #281).

---

### Task 28: Update `errors.md` catalogue (BULLET LIST, not table) + TRUSTED_KEY_NOT_FOUND + NOT_FOUND

**Files:** Modify `cmd/cyoda/help/content/errors.md`, `cmd/cyoda/help/content/errors/TRUSTED_KEY_NOT_FOUND.md`, `cmd/cyoda/help/content/errors/NOT_FOUND.md`.

**Important:** `errors.md` is a BULLET LIST (each entry: `- \`errors.CODE\` — \`STATUS\` — retryable-or-not — description`). NOT a table.

- [ ] **Step 1: Inspect current format.**

```
grep -n "errors\." cmd/cyoda/help/content/errors.md | head -20
```

- [ ] **Step 2: Insert 6 new bullets at alphabetical positions.** Each bullet:

```
- `errors.FEATURE_DISABLED` — `404` — not retryable — Optional feature not enabled in this deployment.
- `errors.KEY_OWNED_BY_DIFFERENT_TENANT` — `409` — not retryable — Trusted-key registration collides with another tenant.
- `errors.KEYPAIR_NOT_FOUND` — `404` — not retryable — Referenced signing keypair does not exist.
- `errors.TRUSTED_KEY_CAP_REACHED` — `400` — not retryable — Per-tenant trusted-key cap reached.
- `errors.UNSUPPORTED_ALGORITHM` — `400` — not retryable — Requested JWT algorithm not supported in this version.
- `errors.UNSUPPORTED_KEY_TYPE` — `400` — not retryable — JWK `kty` not supported in this version.
```

Insertion positions (alphabetical within existing list): around `EPOCH_MISMATCH`/`FORBIDDEN`, `IDEMPOTENCY_CONFLICT`/`INCOMPATIBLE_TYPE`, `TRANSITION_NOT_FOUND`/`TRUSTED_KEY_NOT_FOUND`, `UNAUTHORIZED`/`VALIDATION_FAILED`.

- [ ] **Step 3: Update `TRUSTED_KEY_NOT_FOUND.md` DESCRIPTION.** Append:

```markdown
Returned uniformly for kids that don't exist AND kids owned by another tenant; the response does not distinguish — by design, to prevent cross-tenant existence enumeration.
```

- [ ] **Step 4: Update `NOT_FOUND.md`.** Add `errors.KEYPAIR_NOT_FOUND` to the `see_also` frontmatter block for keypair-specific 404s.

- [ ] **Step 5:** Commit: `docs(help): wire 6 new error codes into errors.md catalogue` (refs #281).

---

### Task 29: `config/auth.md` updates

**Files:** Modify `cmd/cyoda/help/content/config/auth.md`.

- [ ] **Step 1: Add `CYODA_JWT_BOOTSTRAP_AUDIENCE` under JWT mode** (just before the HMAC section). One-line entry:

```
- `CYODA_JWT_BOOTSTRAP_AUDIENCE` — audience for the bootstrap signing key
  derived from `CYODA_JWT_SIGNING_KEY`. Must be `client` or `human`. The
  M2M token-issuance path (`POST /oauth/token`) always uses the
  client-audience key. Set to `human` only in deployments where M2M token
  issuance is disabled and the bootstrap key signs human tokens through
  an external flow. (default: `client`)
```

- [ ] **Step 2: Add new `### IAM features` subsection AFTER `### Bootstrap M2M client` (around line ~71), BEFORE `## EXAMPLES`:**

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

- [ ] **Step 3: Add new `### JWT signing keypair rotation` subsection** after IAM features:

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

v0.8.0 limitations:
- Runtime-issued keypairs are held in memory only; they do not survive
  process restart. The bootstrap key survives because its KID is derived
  deterministically from the PEM input. Persisted signing-key storage is
  tracked in a v0.8.x follow-up.
- Bootstrap keys are saved with a finite `validTo` (default 365 days).
  After expiry the M2M token-issuance path will return
  `404 KEYPAIR_NOT_FOUND` for `getCurrentJwtKeyPair?audience=client`.
  Operators should monitor the startup `WARN` and rotate before expiry.

#### Upgrading from v0.7.x

KV-backed trusted-key entries written by versions < v0.8.0 use the key
shape `trustedkey:<kid>` and are not visible to v0.8.0's OpenAPI surface
(orphaned, not deleted). Operators must re-register affected keys.
Inspection: `grep "^trustedkey:[^:]*$" <kvdump>`. cyoda-go has no known
production users on this surface.
```

- [ ] **Step 4: Add EXAMPLES sub-block** to the existing `## EXAMPLES` (after `**With bootstrap client:**`):

```markdown
**With trusted-key registration enabled:**

```
CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED=true
CYODA_IAM_TRUSTED_KEY_MAX_PER_TENANT=10
```
```

- [ ] **Step 5: Verify quickstart/helm/run.md.** Grep for JWT-bootstrap mentions; only touch if defaults/audience need clarifying:

```
grep -l "CYODA_JWT_SIGNING_KEY\|bootstrap" cmd/cyoda/help/content/quickstart.md cmd/cyoda/help/content/helm.md cmd/cyoda/help/content/run.md
```

If any reference needs updating, add a one-liner about the audience default. Otherwise leave alone.

- [ ] **Step 6:** Commit: `docs(help): config/auth.md — IAM features + JWT rotation + v0.7 upgrade note` (refs #281).

---

### Task 30: `openapi.md` line 96 + audit table

**Files:** Modify `cmd/cyoda/help/content/openapi.md`, `docs/superpowers/audits/2026-04-29-openapi-conformance-audit.md`.

- [ ] **Step 1: Edit `openapi.md:96`.** Replace the IAM bullet with:

```
- **IAM** — OAuth token and key management under `/oauth/`. As of v0.8.0, the 10 `/oauth/keys/*` admin operations (keypair + trusted-key lifecycle) are conformant. OIDC providers under `/oauth/oidc/providers/*` remain 501 until v0.9.0.
```

(NOTE: actual path is `/oauth/oidc/providers/*`, not `/oauth/providers/*`.)

- [ ] **Step 2: Audit table.** Edit `docs/superpowers/audits/2026-04-29-openapi-conformance-audit.md` lines 122-131. For each of the 10 rows, change disposition column from `out-of-scope-not-implemented (#194)` to `match`. The commit column already contains the audit-import hash `766df8b` — replace it with `pending — release/v0.8.0 merge` (per release-milestone-invariant pattern), to be updated to the actual merge commit at release time.

- [ ] **Step 3:** Commit: `docs: openapi.md clarification + audit-table dispositions` (refs #281).

---

### Task 31: v0.8.0 CHANGELOG entry (Keep-a-Changelog format)

**Files:** Modify `CHANGELOG.md`.

- [ ] **Step 1: Inspect existing CHANGELOG structure.**

```
head -80 CHANGELOG.md
```

Note the existing section headings (typically `### ⚠️ Breaking changes`, `### Added`, `### Changed`, `### Storage / SPI`, etc.). Match the established style.

- [ ] **Step 2: Add a v0.8.0 section (or extend `## [Unreleased]` if it's the staging area):**

```markdown
### Added

- `/oauth/keys/keypair/*` and `/oauth/keys/trusted/*` — 10 admin endpoints now conform to the OpenAPI surface via chi-routed adapters in `internal/domain/account/`. (#281, sub-issue of #194)
- 6 new error codes: `FEATURE_DISABLED`, `KEY_OWNED_BY_DIFFERENT_TENANT`, `KEYPAIR_NOT_FOUND`, `TRUSTED_KEY_CAP_REACHED`, `UNSUPPORTED_ALGORITHM`, `UNSUPPORTED_KEY_TYPE`.
- 5 new env vars: `CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED`, `CYODA_IAM_TRUSTED_KEY_MAX_PER_TENANT`, `CYODA_IAM_TRUSTED_KEY_MAX_VALIDITY_DAYS`, `CYODA_IAM_TRUSTED_KEY_MAX_JWK_PROPERTIES`, `CYODA_IAM_KEYPAIR_DEFAULT_VALIDITY_DAYS`. Plus `CYODA_JWT_BOOTSTRAP_AUDIENCE`.

### Changed

- Legacy `/oauth/keys/` prefix mux entry removed from `app/app.go`; chi router now owns all `/oauth/keys/*` paths.
- JWKS endpoint (`/.well-known/jwks.json`) now publishes grace-period-invalidated keys until their `validTo` passes.
- `KVTrustedKeyStore` KV-key encoding within the `trusted-keys` namespace changed from `<kid>` to `<tenantID>:<kid>`. Tenant isolation is now enforced at the storage layer.
- Trusted-key per-tenant cap counts only currently-valid keys (matches Cyoda Cloud).

### ⚠️ Breaking changes

- **Reactivate semantics changed.** `POST /oauth/keys/keypair/{keyId}/reactivate` and `POST /oauth/keys/trusted/{keyId}/reactivate` now require a `ReactivateKeyRequestDto` body with `validTo > now` (and `> validFrom` if supplied). Previously these endpoints had no request body. Cyoda Cloud's behaviour of clearing `validTo` to nil (zombie key) is intentionally not adopted; see #281 spec for rationale.
- **Trusted-key registration is disabled by default.** Set `CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED=true` to enable. Customers using `/oauth/keys/trusted/*` through the legacy mux must opt in.
- **Bootstrap signing key now has finite validity.** Defaults to 365 days (configurable via `CYODA_IAM_KEYPAIR_DEFAULT_VALIDITY_DAYS`). Long-running deployments must rotate before expiry; the startup banner emits a `WARN` if the active key expires within 30 days.
- **Algorithm scope.** cyoda-go v0.8.0 signs and verifies `RS256` only. The OpenAPI declares the full enum (`RS*`, `PS*`, `ES*`, `EdDSA`); non-`RS256` values are rejected with `400 UNSUPPORTED_ALGORITHM`. Trusted-key registration accepts only `kty=RSA` JWKs (`kty=EC`/`OKP` rejected with `400 UNSUPPORTED_KEY_TYPE`). v0.8.1 follow-up tracks multi-algorithm + non-RSA `kty` support.

### Known limitations

- **Runtime-issued signing keypairs are lost on process restart.** The bootstrap key survives (its KID is deterministic per PEM). Persistent signing-key storage is tracked in a v0.8.x follow-up.
- **Pre-v0.8.0 KV trusted-key entries are orphaned.** They use the old key shape (`trustedkey:<kid>` directly in the namespace); v0.8.0 uses `trustedkey:<tenantID>:<kid>` and does not query the old shape. Operators must re-register affected keys. Inspection: `grep "^trustedkey:[^:]*$" <kvdump>`.
- **v0.8.0 → pre-v0.8.0 rollback hazard.** Trusted keys created under v0.8.0 are visible to pre-v0.8.0 binaries as mangled-kid entries (`<tenantID>:<kid>` treated as the kid). Purge out-of-band before rollback if visibility matters.
```

- [ ] **Step 3:** Commit: `docs(release): v0.8.0 CHANGELOG — /oauth/keys/* conformance` (refs #281).

---

## Phase 11: Final verification + follow-up

### Task 32: Full test suite + vet + plugins

- [ ] **Step 1:** `go test -short ./... -v` → PASS.

- [ ] **Step 2:** `go test ./internal/e2e/... -v` → PASS (Docker required).

- [ ] **Step 3:** `go vet ./...` → clean.

- [ ] **Step 4:** `make test-all` → PASS (root + `plugins/memory|sqlite|postgres`).

- [ ] **Step 5: No-secrets-in-logs audit.** Grep new adapter files and bootstrap save:

```
grep -n "slog\.\|log\.Print" internal/domain/account/keys_adapter.go internal/domain/account/trusted_adapter.go internal/auth/service.go internal/auth/kv_trusted_store.go
```

For each call, visually verify no field contains a private key, PEM, or JWK that would expose private components. Expected slog fields: `kid`, `tenant`, `audience`, `algorithm`, `validTo`, `count`, `pkg`.

- [ ] **Step 6:** Commit any fixups: `test: fixups from full-suite verification` (refs #281).

---

### Task 33: Race detector single run

- [ ] **Step 1:** `go test -race ./...` → PASS. Per `.claude/rules/race-testing.md`, this is the one-shot pre-PR sanity check.

- [ ] **Step 2:** Fix any races; re-run; commit any fixups.

---

### Task 34: File v0.8.1 follow-up issue

- [ ] **Step 1:** Compose follow-up issue body in `/tmp/issue-281-followup.md`:

```markdown
## Goal

Extend cyoda-go's JWT signing and trusted-key verification to the full algorithm enum declared in `api/openapi.yaml`:

- Sign + verify: `RS384`, `RS512`, `PS256`, `PS384`, `PS512`, `ES256`, `ES384`, `ES512`, `EdDSA` (in addition to existing `RS256`).
- Trusted-key registration: `kty=EC` and `kty=OKP` JWKs.

## Context

#281 (v0.8.0) ships the 10 `/oauth/keys/*` operations as OpenAPI-conformant adapters but rejects every algorithm and JWK `kty` other than RS256/RSA at the adapter boundary (`UNSUPPORTED_ALGORITHM` / `UNSUPPORTED_KEY_TYPE`). The deferral was deliberate: `internal/auth/jwt.go` was untouched; multi-algorithm dispatch is meaningful new code (per-algorithm signing-method registry, ECDSA + Ed25519 generators, EC/OKP JWK encoding, golden-vector tests).

Cyoda Cloud supports the full enum — see `JwtKeyPairUtil` and `TrustedKeyRegistrationService` in the Kotlin reference.

## In scope

- `internal/auth/jwt.go` — signing-method registry keyed on `KeyPair.Algorithm`.
- ECDSA P-256/P-384/P-521 + Ed25519 generators.
- `internal/auth/keyvalidation.go` — JWK decode for `kty=EC` (crv/x/y per RFC 7518) and `kty=OKP` (crv/x per RFC 8037).
- Adapter changes: remove the early-reject `UNSUPPORTED_ALGORITHM` and `UNSUPPORTED_KEY_TYPE` branches.
- E2E coverage for the new algorithm × kty combinations.

## Acceptance

- All 10 algorithm enum values produce a valid signing key when issued; round-trip signs a sample claim and verifies via JWKS.
- All 3 JWK `kty` values (`RSA`, `EC`, `OKP`) register and verify.
- E2E `TestE2E_TokenExchange` extended with EC and Ed25519 trusted keys.
- Adapters no longer emit `UNSUPPORTED_ALGORITHM` / `UNSUPPORTED_KEY_TYPE` under normal operation.

## Out of scope

- KMS-backed private key storage (separate follow-up under #194 §3.5).

Milestone: v0.8.1.
```

- [ ] **Step 2: File via gh:**

```bash
gh issue create --title "feat(iam): multi-algorithm JWT signing + non-RSA JWK kty support" --body-file /tmp/issue-281-followup.md --milestone v0.8.1
```

- [ ] **Step 3:** Note the issue number in the spec §12 (or in this plan's "Out of scope" section). Commit: `docs(spec): record v0.8.1 multi-algorithm follow-up issue number` (refs #281).

---

## Out of scope (do NOT implement in this PR)

- Multi-algorithm JWT signing (RS384/RS512/PS*/ES*/EdDSA) — v0.8.1 follow-up (Task 34).
- Non-RSA JWK `kty` (EC/OKP) — same v0.8.1 follow-up.
- Persistent signing-key storage — #194 §3.5 follow-up.
- M2M client-store persistence — #194 §3.6, picked up by #194-B.
- `/clients` OpenAPI conformance — #194-B.
- `accountSubscriptionsGet` — #194-C.
- OIDC providers subsystem — #194-D (v0.9.0+).
- Periodic prune of past-`ValidTo` trusted-key entries.
- Cleanup of orphan pre-v0.8.0 `trustedkey:<kid>` KV entries.
- Same-tenant idempotent re-register atomic delete-and-replace (cyoda-go preserves silent upsert).
- Multi-audience signing for human-audience tokens (no current path needs it).

---

## Self-review checklist (for the executor)

After all 34 tasks land, walk this list:

- [ ] All 10 `/oauth/keys/*` operations return OpenAPI-conformant DTOs through chi.
- [ ] `mux.Handle("/oauth/keys/", ...)` removed from `app/app.go`.
- [ ] `internal/auth/keys.go` and `internal/auth/trusted.go` deleted; validators preserved in `keyvalidation.go`.
- [ ] `auth.RequireAdmin` exported; all in-package callsites updated.
- [ ] `Handler` gains `keyStore`, `trustedKeyStore`, `iam` fields; `account.New` signature updated; `app.go` callsite passes through.
- [ ] All 6 env vars implemented; boot-time validation rejects out-of-range values.
- [ ] `kty=RSA` enforced (others → 400 `UNSUPPORTED_KEY_TYPE`); `kid≡keyId` enforced.
- [ ] Tenant-scoped trusted-key store; cross-tenant 404; register-collision 409.
- [ ] `getTrustedKeyByKID` helper canonical; token-verification path uses it.
- [ ] `KVTrustedKeyStore` cache value carries `TenantID`; tenant-scoped methods verify cached TenantID; serialization round-trips `tenantID` + `jwk`; partial KV failure leaves cache untouched.
- [ ] `RotateOptions{Invalidate, GracePeriodSec}` introduced; `Save`/`Register` carry it for atomic sibling-flip.
- [ ] `GetActive(audience)` selects max-`ValidFrom`.
- [ ] Reactivate accepts `ReactivateKeyRequestDto { validFrom?, validTo }`; rejects absent/past `validTo`; idempotent on already-active.
- [ ] `publicKey` returned as base64-DER (no PEM armor).
- [ ] `api/generated.go` regenerated after OpenAPI spec changes.
- [ ] OpenAPI: 501s removed from the 10 ops; every 4xx/5xx switched to ProblemDetail + `application/problem+json`; `SUPER_USER` → `ROLE_ADMIN` in 5 trusted op descriptions; JWK schema fixed; `ReactivateKeyRequestDto` added; default-behaviour prose added.
- [ ] Body-size assertion via shared `boundedJSONDecode` on all 4 POST adapters; integration sub-test relocated; E2E asserts >1MB → 400.
- [ ] Audit table dispositions updated.
- [ ] Cyoda help: 6 new error topics, IAM features + JWT rotation + v0.7 upgrade note in `config/auth.md`, `errors.md` catalogue + `TRUSTED_KEY_NOT_FOUND.md` + `NOT_FOUND.md` updates, `openapi.md:96` clarification.
- [ ] CHANGELOG: v0.8.0 entry in Keep-a-Changelog format with Added/Changed/Breaking changes/Known limitations.
- [ ] Per-divergence regression tests cover all §3.2 items.
- [ ] Full test suite (`go test ./... -v`) + `make test-all` + `go test -race ./...` green.
- [ ] No-secrets-in-logs audit passed.
- [ ] v0.8.1 follow-up issue filed (multi-algorithm + non-RSA kty).
