# /clients OpenAPI conformance (issue #282) — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Surface the existing M2M client management at the spec-conformant `/clients` paths with the spec-conformant DTOs, retire `/account/m2m`, and flip five OpenAPI operations from `501 NOT_IMPLEMENTED` to `match`.

**Architecture:** New per-endpoint chi adapter `internal/domain/account/m2m_adapter.go` mirrors the just-merged `trusted_adapter.go` from PR #281: each operation calls `auth.RequireAdmin`, then a feature/store gate, then forwards to the existing `auth.M2MClientStore`. A new IAM feature flag `M2MAdminRoleEnabled` plumbs through `IAMFeatures` → `IAMConfig` → `auth.IAMFeatures`. The legacy `internal/auth/m2m.go` HTTP handler and its `/account/m2m*` mux entries are deleted. `GetTechnicalUserToken` stays on the `Handler` as a defensive 500 stub for `ServerInterface` satisfaction; the public mux at `app/app.go:480` continues to serve the real `POST /oauth/token`.

**Tech Stack:** Go 1.26, `chi` v5, `oapi-codegen` v2.7.0, `golang.org/x/crypto/bcrypt`, `log/slog`, `testcontainers-go` for E2E PostgreSQL.

**Spec:** [`docs/superpowers/specs/2026-06-16-282-iam-clients-openapi-design.md`](../specs/2026-06-16-282-iam-clients-openapi-design.md) (rev. 2).

**Branch:** `feat/iam-clients-openapi-282` off `release/v0.8.0`. PR targets `release/v0.8.0` per the v0.8.0 release-branch workflow.

---

## Task 1: Add `M2MAdminRoleEnabled` feature-flag plumbing (TDD)

**Files:**
- Modify: `internal/auth/iam_features.go`
- Modify: `app/config.go:84-96` (`IAMConfig` struct), `:189-205` (`DefaultConfig`), `:496-509` (`AuthIAMFeatures`)
- Modify: `internal/auth/iam_features_test.go` (if it exists; otherwise create)
- Modify: `app/config_test.go` (find by `grep -l 'envBool.*TRUSTED_KEY_REGISTRATION' app/`)

- [ ] **Step 1.1: Write the RED test for `DefaultConfig` default-false.**

Open or create `app/config_test.go`. Add this test function:

```go
func TestDefaultConfig_M2MAdminRoleEnabled_DefaultFalse(t *testing.T) {
    t.Setenv("CYODA_IAM_M2M_ADMIN_ROLE_ENABLED", "")
    cfg := DefaultConfig()
    if cfg.IAM.M2MAdminRoleEnabled {
        t.Fatalf("expected CYODA_IAM_M2M_ADMIN_ROLE_ENABLED to default to false, got true")
    }
}

func TestDefaultConfig_M2MAdminRoleEnabled_EnvTrue(t *testing.T) {
    t.Setenv("CYODA_IAM_M2M_ADMIN_ROLE_ENABLED", "true")
    cfg := DefaultConfig()
    if !cfg.IAM.M2MAdminRoleEnabled {
        t.Fatalf("CYODA_IAM_M2M_ADMIN_ROLE_ENABLED=true should set the field to true")
    }
}

func TestAuthIAMFeatures_PropagatesM2MAdminRoleEnabled(t *testing.T) {
    c := IAMConfig{M2MAdminRoleEnabled: true}
    f := c.AuthIAMFeatures()
    if !f.M2MAdminRoleEnabled {
        t.Fatalf("AuthIAMFeatures must propagate M2MAdminRoleEnabled, got false")
    }
}
```

- [ ] **Step 1.2: Run the tests to confirm they fail at compile.**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/iam-clients-openapi
go test ./app/ -run TestDefaultConfig_M2MAdminRoleEnabled -v
```

Expected: `FAIL` with "`IAMConfig has no field or method M2MAdminRoleEnabled`" or similar.

- [ ] **Step 1.3: Add the field to `IAMFeatures`.**

Edit `internal/auth/iam_features.go`. Add the field after `BootstrapAudience`:

```go
type IAMFeatures struct {
    TrustedKeyRegistrationEnabled bool   // env CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED, default false
    TrustedKeyMaxPerTenant        int    // env CYODA_IAM_TRUSTED_KEY_MAX_PER_TENANT, default 10; 0=unbounded
    TrustedKeyMaxValidityDays     int    // env CYODA_IAM_TRUSTED_KEY_MAX_VALIDITY_DAYS, default 365
    TrustedKeyMaxJWKProperties    int    // env CYODA_IAM_TRUSTED_KEY_MAX_JWK_PROPERTIES, default 20
    KeypairDefaultValidityDays    int    // env CYODA_IAM_KEYPAIR_DEFAULT_VALIDITY_DAYS, default 365
    BootstrapAudience             string // env CYODA_JWT_BOOTSTRAP_AUDIENCE, default "client"

    // M2MAdminRoleEnabled gates POST /clients?withAdminRole=true. When false
    // (the secure default) the endpoint returns 404 FEATURE_DISABLED on that
    // request shape. env CYODA_IAM_M2M_ADMIN_ROLE_ENABLED, default false.
    M2MAdminRoleEnabled bool
}
```

No change to `DefaultIAMFeatures()` (zero value `false` is correct). No change to `Validate()` (booleans have no invalid value).

- [ ] **Step 1.4: Add the field to `app.IAMConfig`.**

Edit `app/config.go` line ~88. Add after `BootstrapAudience`:

```go
    // NEW: IAM feature surface for /oauth/keys/* — passed through to auth.IAMFeatures.
    TrustedKeyRegistrationEnabled bool
    TrustedKeyMaxPerTenant        int
    TrustedKeyMaxValidityDays     int
    TrustedKeyMaxJWKProperties    int
    KeypairDefaultValidityDays    int
    BootstrapAudience             string

    // M2MAdminRoleEnabled — see auth.IAMFeatures.M2MAdminRoleEnabled.
    // env CYODA_IAM_M2M_ADMIN_ROLE_ENABLED, default false.
    M2MAdminRoleEnabled bool
```

- [ ] **Step 1.5: Wire `envBool` for the new var in `DefaultConfig`.**

Edit `app/config.go` ~line 200. Within the `IAM: IAMConfig{ ... }` literal, after `BootstrapAudience: envString(...)`, add:

```go
            BootstrapAudience:             envString("CYODA_JWT_BOOTSTRAP_AUDIENCE", "client"),
            M2MAdminRoleEnabled:           envBool("CYODA_IAM_M2M_ADMIN_ROLE_ENABLED", false),
```

- [ ] **Step 1.6: Propagate through `AuthIAMFeatures`.**

Edit `app/config.go` ~line 498. Add the field to the projector:

```go
func (c IAMConfig) AuthIAMFeatures() auth.IAMFeatures {
    return auth.IAMFeatures{
        TrustedKeyRegistrationEnabled: c.TrustedKeyRegistrationEnabled,
        TrustedKeyMaxPerTenant:        c.TrustedKeyMaxPerTenant,
        TrustedKeyMaxValidityDays:     c.TrustedKeyMaxValidityDays,
        TrustedKeyMaxJWKProperties:    c.TrustedKeyMaxJWKProperties,
        KeypairDefaultValidityDays:    c.KeypairDefaultValidityDays,
        BootstrapAudience:             c.BootstrapAudience,
        M2MAdminRoleEnabled:           c.M2MAdminRoleEnabled,
    }
}
```

- [ ] **Step 1.7: Run the tests to confirm GREEN.**

```bash
go test ./app/ -run "TestDefaultConfig_M2MAdminRoleEnabled|TestAuthIAMFeatures_PropagatesM2MAdminRoleEnabled" -v
go test ./internal/auth/ -run TestIAMFeatures -v
```

Expected: all PASS.

- [ ] **Step 1.8: Commit.**

```bash
git add internal/auth/iam_features.go app/config.go app/config_test.go
git commit -m "feat(iam): add M2MAdminRoleEnabled flag plumbing

CYODA_IAM_M2M_ADMIN_ROLE_ENABLED env (default false) flows through
IAMConfig.AuthIAMFeatures() into auth.IAMFeatures. Used in Task 5
to gate POST /clients?withAdminRole=true.

Refs #282."
```

---

## Task 2: Extend `M2MClient` with timestamps and `spi.TenantID` (TDD)

**Files:**
- Modify: `internal/auth/store.go:52-58` (`M2MClient` struct), `:458-505` (`Create`, `CreateWithSecret`), `:546-565` (`ResetSecret`), `:85-93` (`M2MClientStore` interface)
- Modify: `internal/auth/store_test.go` (find by `grep -l 'TestM2MClient\|InMemoryM2MClientStore' internal/auth/*_test.go`)
- Modify: `app/app.go:266` (bootstrap caller — `CreateWithSecret`)

- [ ] **Step 2.1: Locate the existing M2M store tests.**

```bash
grep -ln "InMemoryM2MClientStore\|TestM2M\|M2MClientStore" internal/auth/*_test.go
```

The expected target is `internal/auth/store_test.go` (or wherever the M2M tests live). Open it.

- [ ] **Step 2.2: Write the RED tests for timestamps + tenant-type promotion.**

Append to that test file (or create it with package `auth`):

```go
func TestInMemoryM2MClientStore_Create_StampsCreatedAndUpdatedAt(t *testing.T) {
    store := NewInMemoryM2MClientStore()
    before := time.Now()
    _, err := store.Create("client-a", spi.TenantID("tenant-a"), "user-a", []string{"ROLE_M2M"})
    if err != nil {
        t.Fatalf("Create: %v", err)
    }
    after := time.Now()

    c, err := store.Get("client-a")
    if err != nil {
        t.Fatalf("Get: %v", err)
    }
    if c.CreatedAt.Before(before) || c.CreatedAt.After(after) {
        t.Errorf("CreatedAt %v outside [%v, %v]", c.CreatedAt, before, after)
    }
    if !c.UpdatedAt.Equal(c.CreatedAt) {
        t.Errorf("Create: UpdatedAt (%v) should equal CreatedAt (%v) on fresh create", c.UpdatedAt, c.CreatedAt)
    }
}

func TestInMemoryM2MClientStore_CreateWithSecret_StampsCreatedAndUpdatedAt(t *testing.T) {
    store := NewInMemoryM2MClientStore()
    before := time.Now()
    err := store.CreateWithSecret("client-b", spi.TenantID("tenant-b"), "user-b", "secret-b", []string{"ROLE_M2M"})
    if err != nil {
        t.Fatalf("CreateWithSecret: %v", err)
    }
    after := time.Now()

    c, err := store.Get("client-b")
    if err != nil {
        t.Fatalf("Get: %v", err)
    }
    if c.CreatedAt.Before(before) || c.CreatedAt.After(after) {
        t.Errorf("CreatedAt %v outside [%v, %v]", c.CreatedAt, before, after)
    }
    if !c.UpdatedAt.Equal(c.CreatedAt) {
        t.Errorf("CreateWithSecret: UpdatedAt should equal CreatedAt on fresh create")
    }
}

func TestInMemoryM2MClientStore_ResetSecret_AdvancesUpdatedAt(t *testing.T) {
    store := NewInMemoryM2MClientStore()
    if _, err := store.Create("client-c", spi.TenantID("tenant-c"), "user-c", []string{"ROLE_M2M"}); err != nil {
        t.Fatalf("Create: %v", err)
    }
    c0, _ := store.Get("client-c")
    time.Sleep(2 * time.Millisecond) // guarantee monotonic distance
    if _, err := store.ResetSecret("client-c"); err != nil {
        t.Fatalf("ResetSecret: %v", err)
    }
    c1, _ := store.Get("client-c")

    if !c1.UpdatedAt.After(c0.UpdatedAt) {
        t.Errorf("ResetSecret: UpdatedAt did not advance (%v -> %v)", c0.UpdatedAt, c1.UpdatedAt)
    }
    if !c1.CreatedAt.Equal(c0.CreatedAt) {
        t.Errorf("ResetSecret: CreatedAt must not change (%v -> %v)", c0.CreatedAt, c1.CreatedAt)
    }
}

func TestM2MClient_TenantIDIsSpiType(t *testing.T) {
    var c M2MClient
    var _ spi.TenantID = c.TenantID // compile-time check: field must be assignable from spi.TenantID
}
```

Add `"time"`, `spi "github.com/cyoda-platform/cyoda-go-spi"` to the test file imports if not already present.

- [ ] **Step 2.3: Confirm the tests fail.**

```bash
go test ./internal/auth/ -run "TestInMemoryM2MClientStore_Create_StampsCreatedAndUpdatedAt|TestInMemoryM2MClientStore_CreateWithSecret_StampsCreatedAndUpdatedAt|TestInMemoryM2MClientStore_ResetSecret_AdvancesUpdatedAt|TestM2MClient_TenantIDIsSpiType" -v
```

Expected: compile error on `c.CreatedAt`, `c.UpdatedAt`, and the `var _ spi.TenantID = c.TenantID` line (TenantID is currently `string`).

- [ ] **Step 2.4: Update the `M2MClient` struct.**

Edit `internal/auth/store.go` ~line 52:

```go
// M2MClient represents a machine-to-machine client.
type M2MClient struct {
    ClientID     string
    HashedSecret string
    TenantID     spi.TenantID
    UserID       string
    Roles        []string
    CreatedAt    time.Time // set at Create/CreateWithSecret, never advanced
    UpdatedAt    time.Time // advanced on ResetSecret; equal to CreatedAt on fresh create
}
```

If `time` is not imported, add `"time"` to the import block. The file already imports `spi "github.com/cyoda-platform/cyoda-go-spi"` (used by `TrustedKey`).

- [ ] **Step 2.5: Update the `M2MClientStore` interface signatures.**

Edit `internal/auth/store.go` ~line 85:

```go
// M2MClientStore manages machine-to-machine clients.
type M2MClientStore interface {
    Create(clientID string, tenantID spi.TenantID, userID string, roles []string) (string, error)
    CreateWithSecret(clientID string, tenantID spi.TenantID, userID, secret string, roles []string) error
    Get(clientID string) (*M2MClient, error)
    List() []*M2MClient
    Delete(clientID string) error
    ResetSecret(clientID string) (string, error)
    VerifySecret(clientID, plaintext string) (bool, error)
}
```

- [ ] **Step 2.6: Update the `InMemoryM2MClientStore` method signatures + bodies.**

Edit `internal/auth/store.go` ~line 458:

```go
func (s *InMemoryM2MClientStore) Create(clientID string, tenantID spi.TenantID, userID string, roles []string) (string, error) {
    secret, err := GenerateSecret()
    if err != nil {
        return "", fmt.Errorf("failed to generate secret: %w", err)
    }
    hashed, err := bcrypt.GenerateFromPassword([]byte(secret), bcrypt.DefaultCost)
    if err != nil {
        return "", fmt.Errorf("failed to hash secret: %w", err)
    }
    rolesCopy := make([]string, len(roles))
    copy(rolesCopy, roles)

    now := time.Now().UTC()
    s.mu.Lock()
    defer s.mu.Unlock()
    s.clients[clientID] = &M2MClient{
        ClientID:     clientID,
        HashedSecret: string(hashed),
        TenantID:     tenantID,
        UserID:       userID,
        Roles:        rolesCopy,
        CreatedAt:    now,
        UpdatedAt:    now,
    }
    return secret, nil
}

func (s *InMemoryM2MClientStore) CreateWithSecret(clientID string, tenantID spi.TenantID, userID, secret string, roles []string) error {
    hashed, err := bcrypt.GenerateFromPassword([]byte(secret), bcrypt.DefaultCost)
    if err != nil {
        return fmt.Errorf("failed to hash secret: %w", err)
    }
    rolesCopy := make([]string, len(roles))
    copy(rolesCopy, roles)

    now := time.Now().UTC()
    s.mu.Lock()
    defer s.mu.Unlock()
    s.clients[clientID] = &M2MClient{
        ClientID:     clientID,
        HashedSecret: string(hashed),
        TenantID:     tenantID,
        UserID:       userID,
        Roles:        rolesCopy,
        CreatedAt:    now,
        UpdatedAt:    now,
    }
    return nil
}
```

Update `ResetSecret` ~line 546:

```go
func (s *InMemoryM2MClientStore) ResetSecret(clientID string) (string, error) {
    secret, err := GenerateSecret()
    if err != nil {
        return "", fmt.Errorf("failed to generate secret: %w", err)
    }
    hashed, err := bcrypt.GenerateFromPassword([]byte(secret), bcrypt.DefaultCost)
    if err != nil {
        return "", fmt.Errorf("failed to hash secret: %w", err)
    }

    s.mu.Lock()
    defer s.mu.Unlock()
    c, ok := s.clients[clientID]
    if !ok {
        return "", fmt.Errorf("m2m client not found: %s", clientID)
    }
    c.HashedSecret = string(hashed)
    c.UpdatedAt = time.Now().UTC()
    return secret, nil
}
```

- [ ] **Step 2.7: Fix the bootstrap caller.**

Edit `app/app.go` ~line 266:

```go
if err := authSvc.M2MClientStore().CreateWithSecret(
    cfg.Bootstrap.ClientID,
    spi.TenantID(cfg.Bootstrap.TenantID),
    cfg.Bootstrap.UserID,
    cfg.Bootstrap.ClientSecret,
    roles,
); err != nil {
```

If `spi` is not already imported in `app/app.go`, add `spi "github.com/cyoda-platform/cyoda-go-spi"`.

- [ ] **Step 2.8: Run the affected packages and confirm everything compiles + tests pass.**

```bash
go build ./...
go test ./internal/auth/ -run "TestInMemoryM2MClientStore_Create_StampsCreatedAndUpdatedAt|TestInMemoryM2MClientStore_CreateWithSecret_StampsCreatedAndUpdatedAt|TestInMemoryM2MClientStore_ResetSecret_AdvancesUpdatedAt|TestM2MClient_TenantIDIsSpiType" -v
go test ./internal/auth/ -v
go test ./app/ -v
```

Expected: all PASS. If a string-tenant call site outside the bootstrap appears, fix it the same way (`spi.TenantID(...)` cast).

- [ ] **Step 2.9: Commit.**

```bash
git add internal/auth/store.go internal/auth/store_test.go app/app.go
git commit -m "feat(auth): M2MClient gets CreatedAt/UpdatedAt and spi.TenantID

CreatedAt + UpdatedAt back the TechnicalUserDto.creationDate +
lastUpdateDate fields. Promote TenantID from string to spi.TenantID
for parity with TrustedKey and tenantFromCtx(); the bootstrap call
in app/app.go casts.

Refs #282."
```

---

## Task 3: Add `ErrCodeM2MClientNotFound` error code

**Files:**
- Modify: `internal/common/error_codes.go`

- [ ] **Step 3.1: Add the constant.**

Edit `internal/common/error_codes.go` ~line 35 (near `ErrCodeTrustedKeyNotFound`):

```go
ErrCodeTrustedKeyCapReached      = "TRUSTED_KEY_CAP_REACHED"
ErrCodeTrustedKeyNotFound        = "TRUSTED_KEY_NOT_FOUND"
ErrCodeM2MClientNotFound         = "M2M_CLIENT_NOT_FOUND"
```

- [ ] **Step 3.2: Confirm it compiles.**

```bash
go vet ./internal/common/
```

Expected: clean (no test needed; the code is consumed by adapter tests in Task 5).

- [ ] **Step 3.3: Commit.**

```bash
git add internal/common/error_codes.go
git commit -m "feat(common): add ErrCodeM2MClientNotFound

Domain-specific 404 code mirroring TRUSTED_KEY_NOT_FOUND; emitted
by the /clients adapter on cross-tenant or unknown-id paths.

Refs #282."
```

---

## Task 4: Extend `account.Handler` constructor with M2M store

**Files:**
- Modify: `internal/domain/account/handler.go:15-32` (struct + `New`)
- Modify: `app/app.go:452` (call site)
- Modify: `internal/domain/account/handler_test.go` and `keys_adapter_test.go`, `trusted_adapter_test.go` (find by `grep -ln "account.New\\|account\\.New" internal/`)

- [ ] **Step 4.1: Add the field + constructor parameter.**

Edit `internal/domain/account/handler.go`:

```go
type Handler struct {
    authSvc         contract.AuthenticationService
    authzSvc        contract.AuthorizationService
    keyStore        auth.KeyStore
    trustedKeyStore auth.TrustedKeyStore
    m2mClientStore  auth.M2MClientStore
    iam             auth.IAMFeatures
}

func New(authSvc contract.AuthenticationService, authzSvc contract.AuthorizationService,
    keyStore auth.KeyStore, trustedKeyStore auth.TrustedKeyStore, m2mClientStore auth.M2MClientStore,
    iam auth.IAMFeatures) *Handler {
    return &Handler{
        authSvc:         authSvc,
        authzSvc:        authzSvc,
        keyStore:        keyStore,
        trustedKeyStore: trustedKeyStore,
        m2mClientStore:  m2mClientStore,
        iam:             iam,
    }
}
```

- [ ] **Step 4.2: Update the production call site.**

Edit `app/app.go` ~line 452:

```go
var accountKeyStore auth.KeyStore
var accountTrustedKeyStore auth.TrustedKeyStore
var accountM2MStore auth.M2MClientStore
if authSvc != nil {
    accountKeyStore = authSvc.KeyStore()
    accountTrustedKeyStore = authSvc.TrustedKeyStore()
    accountM2MStore = authSvc.M2MClientStore()
}
server.Account = account.New(a.authService, a.authzService, accountKeyStore, accountTrustedKeyStore, accountM2MStore, cfg.IAM.AuthIAMFeatures())
```

- [ ] **Step 4.3: Update all existing call sites in tests.**

```bash
grep -rn "account.New(" internal/ app/ | grep -v "account_test"
```

For each match outside production code, add a `nil` (or `auth.NewInMemoryM2MClientStore()` if the test exercises M2M) as the new fifth positional argument before the `iam` argument. Example pattern:

```go
// Before
h := account.New(authSvc, authzSvc, keyStore, trustedStore, feats)
// After
h := account.New(authSvc, authzSvc, keyStore, trustedStore, nil, feats)
```

- [ ] **Step 4.4: Confirm the project builds + existing tests still pass.**

```bash
go build ./...
go test ./internal/domain/account/ ./app/ ./internal/api/ -v
```

Expected: all PASS.

- [ ] **Step 4.5: Commit.**

```bash
git add internal/domain/account/handler.go app/app.go internal/domain/account/handler_test.go internal/domain/account/keys_adapter_test.go internal/domain/account/trusted_adapter_test.go
# add any other test files updated by step 4.3
git commit -m "feat(account): inject M2MClientStore into handler constructor

Adds the dependency channel for the upcoming m2m_adapter.go. No
behaviour change — the field is nil-tolerated by the existing
adapters and the new adapter (Task 5) guards on nil with
501 NOT_IMPLEMENTED.

Refs #282."
```

---

## Task 5: Create `m2m_adapter.go` skeleton + helpers

**Files:**
- Create: `internal/domain/account/m2m_adapter.go`

The actual operation methods land in Tasks 6–9. This task seeds the file with the helpers all four methods share.

- [ ] **Step 5.1: Create the file.**

Write `internal/domain/account/m2m_adapter.go`:

```go
package account

import (
    "crypto/rand"
    "encoding/base32"
    "encoding/json"
    "errors"
    "log/slog"
    "net/http"
    "regexp"
    "strings"
    "time"

    spi "github.com/cyoda-platform/cyoda-go-spi"
    genapi "github.com/cyoda-platform/cyoda-go/api"
    "github.com/cyoda-platform/cyoda-go/internal/auth"
    "github.com/cyoda-platform/cyoda-go/internal/common"
)

// clientIDPattern enforces the OpenAPI schema for path-param + generated
// clientId: ^[A-Za-z0-9]+$, length 1..100.
var clientIDPattern = regexp.MustCompile(`^[A-Za-z0-9]{1,100}$`)

// clientIDLen is the number of base32-hex characters in a generated clientId.
// 16 chars × 5 bits/char = 80 bits of entropy. Comfortably inside the
// OpenAPI maxLength=100; short enough to copy-paste.
const clientIDLen = 16

// generateClientID returns a random 16-char uppercase base32-hex string.
// Uses crypto/rand; never reuses entropy.
func generateClientID() (string, error) {
    // base32-hex with no padding: 5 bits per char. ceil(16*5/8) = 10 bytes input.
    buf := make([]byte, 10)
    if _, err := rand.Read(buf); err != nil {
        return "", err
    }
    s := base32.HexEncoding.WithPadding(base32.NoPadding).EncodeToString(buf)
    s = strings.ToUpper(s)
    if len(s) < clientIDLen {
        // base32 output is deterministic on input length; this is belt-and-suspenders.
        return "", errors.New("base32 encoder produced unexpected length")
    }
    return s[:clientIDLen], nil
}

// requireM2MStore writes 501 NOT_IMPLEMENTED and returns false when the M2M
// store is not wired (mock IAM mode). All four /clients adapters call this.
func (h *Handler) requireM2MStore(w http.ResponseWriter, r *http.Request) bool {
    if h.m2mClientStore == nil {
        common.WriteError(w, r, common.Operational(http.StatusNotImplemented,
            common.ErrCodeNotImplemented, "M2M client management requires JWT IAM mode"))
        return false
    }
    return true
}

// gateM2MAdminRole gates POST /clients?withAdminRole=true on the IAM feature
// flag. Writes 404 FEATURE_DISABLED when the flag is off; returns false.
func (h *Handler) gateM2MAdminRole(w http.ResponseWriter, r *http.Request) bool {
    if !h.iam.M2MAdminRoleEnabled {
        common.WriteError(w, r, common.Operational(http.StatusNotFound,
            common.ErrCodeFeatureDisabled, "M2M admin-role grants are disabled"))
        return false
    }
    return true
}

// validateClientID writes 400 BAD_REQUEST and returns false when the
// path-param clientId is empty or violates the OpenAPI pattern.
func validateClientID(w http.ResponseWriter, r *http.Request, clientID string) bool {
    if clientID == "" || !clientIDPattern.MatchString(clientID) {
        common.WriteError(w, r, common.Operational(http.StatusBadRequest,
            common.ErrCodeBadRequest, "invalid clientId"))
        return false
    }
    return true
}

// toTechnicalUserDto maps a store record to its OpenAPI wire shape.
// Deliberately reads only the public-facing fields — never touches HashedSecret.
func toTechnicalUserDto(c *auth.M2MClient) genapi.TechnicalUserDto {
    roles := make([]string, len(c.Roles))
    copy(roles, c.Roles)
    return genapi.TechnicalUserDto{
        ClientId:       c.ClientID,
        CreationDate:   c.CreatedAt,
        LastUpdateDate: c.UpdatedAt,
        Roles:          roles,
    }
}

// toTechnicalUserCredentialsDto wraps a freshly-issued plaintext secret in
// the spec-conformant response shape. The grant_type is always
// "client_credentials" per RFC 7591 §3.2.1. expires_at=0 means "never expires".
func toTechnicalUserCredentialsDto(clientID, plaintextSecret string, roles []string) genapi.TechnicalUserCredentialsDto {
    rolesCopy := make([]string, len(roles))
    copy(rolesCopy, roles)
    return genapi.TechnicalUserCredentialsDto{
        ClientId:              clientID,
        ClientSecret:          plaintextSecret,
        GrantType:             genapi.TechnicalUserCredentialsDtoGrantType("client_credentials"),
        ClientSecretExpiresAt: 0,
        Roles:                 rolesCopy,
    }
}

// clientBelongsToTenant returns true iff the store record's TenantID matches
// the caller's tenant. The comparison shape is direct because both sides are
// spi.TenantID after Task 2.
func clientBelongsToTenant(c *auth.M2MClient, callerTenant spi.TenantID) bool {
    return c.TenantID == callerTenant
}

// withReassertedAdminCheck is a noop placeholder for future per-adapter
// metric hooks. Currently used to silence the staticcheck warning about
// the unused slog import in the unreachable GetTechnicalUserToken stub
// while keeping the import block stable across small adapters.
// (slog is consumed by the unreachable-stub elsewhere in handler.go.)
var _ = slog.LevelInfo

// _ uses time so the unused-import linter stays quiet until the operation
// methods (Tasks 6-9) reference it directly.
var _ = time.Now
```

- [ ] **Step 5.2: Confirm the file compiles.**

```bash
go build ./internal/domain/account/
```

Expected: clean build. If `slog`/`time` placeholders cause complaints, leave them — they'll be consumed by the operation methods in subsequent tasks.

- [ ] **Step 5.3: Commit.**

```bash
git add internal/domain/account/m2m_adapter.go
git commit -m "feat(account): seed m2m_adapter helpers

Adds the per-adapter file with shared helpers (clientId generator
+ pattern, store/feature gates, DTO mappers). Operation methods
land in Tasks 6-9.

Refs #282."
```

---

## Task 6: Implement `ListTechnicalUsers` (TDD)

**Files:**
- Create: `internal/domain/account/m2m_adapter_test.go`
- Modify: `internal/domain/account/m2m_adapter.go`

- [ ] **Step 6.1: Create the test file with fixture helpers + RED tests for List.**

Write `internal/domain/account/m2m_adapter_test.go`:

```go
package account

import (
    "encoding/json"
    "net/http"
    "net/http/httptest"
    "strings"
    "testing"

    spi "github.com/cyoda-platform/cyoda-go-spi"
    genapi "github.com/cyoda-platform/cyoda-go/api"
    "github.com/cyoda-platform/cyoda-go/internal/auth"
    "github.com/cyoda-platform/cyoda-go/internal/common"
)

// --- fixture helpers ---

const (
    tenantA = "tenant-a"
    tenantB = "tenant-b"
)

func newM2MAdapterFixture(t *testing.T, flagOn bool) *Handler {
    t.Helper()
    feats := auth.DefaultIAMFeatures()
    feats.M2MAdminRoleEnabled = flagOn
    return New(nil, nil, nil, nil, auth.NewInMemoryM2MClientStore(), feats)
}

func withTenantAdminCtx(req *http.Request, tenantID string) *http.Request {
    return req.WithContext(spi.WithUserContext(req.Context(), &spi.UserContext{
        UserID:   "admin-user",
        UserName: "Admin User",
        Tenant:   spi.Tenant{ID: spi.TenantID(tenantID), Name: tenantID},
        Roles:    []string{"ROLE_ADMIN"},
    }))
}

func withTenantNonAdminCtx(req *http.Request, tenantID string) *http.Request {
    return req.WithContext(spi.WithUserContext(req.Context(), &spi.UserContext{
        UserID:   "regular-user",
        UserName: "Regular User",
        Tenant:   spi.Tenant{ID: spi.TenantID(tenantID), Name: tenantID},
        Roles:    []string{"ROLE_USER"},
    }))
}

func decodeErrCode(t *testing.T, body []byte) string {
    t.Helper()
    var env struct {
        ErrorCode string `json:"errorCode"`
    }
    if err := json.Unmarshal(body, &env); err != nil {
        t.Fatalf("decode error envelope: %v\nbody: %s", err, string(body))
    }
    return env.ErrorCode
}

// --- Case 1: List, admin, empty store ---

func TestListTechnicalUsers_AdminEmpty_Returns200EmptyArray(t *testing.T) {
    h := newM2MAdapterFixture(t, false)
    req := withTenantAdminCtx(httptest.NewRequest(http.MethodGet, "/clients", nil), tenantA)
    rr := httptest.NewRecorder()

    h.ListTechnicalUsers(rr, req)

    if rr.Code != http.StatusOK {
        t.Fatalf("status: got %d want 200, body=%s", rr.Code, rr.Body.String())
    }
    body := strings.TrimSpace(rr.Body.String())
    if body != "[]" {
        t.Errorf("body: got %q want %q", body, "[]")
    }
}

// --- Case 2: List, admin, mixed-tenant store, returns caller's tenant only ---

func TestListTechnicalUsers_AdminMixedTenant_FiltersOnCallerTenant(t *testing.T) {
    h := newM2MAdapterFixture(t, false)
    store := h.m2mClientStore.(*auth.InMemoryM2MClientStore)
    if _, err := store.Create("CLIENTAONE", spi.TenantID(tenantA), "CLIENTAONE", []string{"ROLE_M2M"}); err != nil {
        t.Fatalf("seed A1: %v", err)
    }
    if _, err := store.Create("CLIENTATWO", spi.TenantID(tenantA), "CLIENTATWO", []string{"ROLE_M2M"}); err != nil {
        t.Fatalf("seed A2: %v", err)
    }
    if _, err := store.Create("CLIENTBONE", spi.TenantID(tenantB), "CLIENTBONE", []string{"ROLE_M2M"}); err != nil {
        t.Fatalf("seed B1: %v", err)
    }

    req := withTenantAdminCtx(httptest.NewRequest(http.MethodGet, "/clients", nil), tenantA)
    rr := httptest.NewRecorder()
    h.ListTechnicalUsers(rr, req)

    if rr.Code != http.StatusOK {
        t.Fatalf("status: got %d want 200", rr.Code)
    }
    var got []genapi.TechnicalUserDto
    if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
        t.Fatalf("decode: %v", err)
    }
    if len(got) != 2 {
        t.Fatalf("got %d items, want 2: %+v", len(got), got)
    }
    seen := map[string]bool{}
    for _, c := range got {
        seen[c.ClientId] = true
        if c.CreationDate.IsZero() {
            t.Errorf("client %s: CreationDate zero", c.ClientId)
        }
    }
    if !seen["CLIENTAONE"] || !seen["CLIENTATWO"] {
        t.Errorf("missing expected clients: %v", seen)
    }
    if seen["CLIENTBONE"] {
        t.Errorf("tenant B's client leaked: %v", seen)
    }
}

// --- Case 3: List, non-admin → 403 FORBIDDEN ---

func TestListTechnicalUsers_NonAdmin_Returns403Forbidden(t *testing.T) {
    h := newM2MAdapterFixture(t, false)
    req := withTenantNonAdminCtx(httptest.NewRequest(http.MethodGet, "/clients", nil), tenantA)
    rr := httptest.NewRecorder()

    h.ListTechnicalUsers(rr, req)

    if rr.Code != http.StatusForbidden {
        t.Fatalf("status: got %d want 403", rr.Code)
    }
    if code := decodeErrCode(t, rr.Body.Bytes()); code != common.ErrCodeForbidden {
        t.Errorf("errorCode: got %q want %q", code, common.ErrCodeForbidden)
    }
}

// --- Case 4: List, no user context → 401 UNAUTHORIZED ---

func TestListTechnicalUsers_NoUserContext_Returns401Unauthorized(t *testing.T) {
    h := newM2MAdapterFixture(t, false)
    req := httptest.NewRequest(http.MethodGet, "/clients", nil) // no context
    rr := httptest.NewRecorder()

    h.ListTechnicalUsers(rr, req)

    if rr.Code != http.StatusUnauthorized {
        t.Fatalf("status: got %d want 401", rr.Code)
    }
    if code := decodeErrCode(t, rr.Body.Bytes()); code != common.ErrCodeUnauthorized {
        t.Errorf("errorCode: got %q want %q", code, common.ErrCodeUnauthorized)
    }
}

// --- Case 5: List, nil store → 501 NOT_IMPLEMENTED ---

func TestListTechnicalUsers_NilStore_Returns501NotImplemented(t *testing.T) {
    feats := auth.DefaultIAMFeatures()
    h := New(nil, nil, nil, nil, nil, feats) // explicitly nil store

    req := withTenantAdminCtx(httptest.NewRequest(http.MethodGet, "/clients", nil), tenantA)
    rr := httptest.NewRecorder()
    h.ListTechnicalUsers(rr, req)

    if rr.Code != http.StatusNotImplemented {
        t.Fatalf("status: got %d want 501", rr.Code)
    }
    if code := decodeErrCode(t, rr.Body.Bytes()); code != common.ErrCodeNotImplemented {
        t.Errorf("errorCode: got %q want %q", code, common.ErrCodeNotImplemented)
    }
}
```

- [ ] **Step 6.2: Confirm the tests fail (no `ListTechnicalUsers` on `m2m_adapter.go` yet).**

```bash
go test ./internal/domain/account/ -run TestListTechnicalUsers -v
```

Expected: compile error or unexpected fall-through; the existing stub at `handler.go:76` still answers 501. The stub will be removed in Task 10; for now, the existing stub may make some assertions accidentally pass. Delete the existing stub method (`handler.go:76-78`) **before** running the tests — this is the moment to take it out:

Edit `internal/domain/account/handler.go` and remove these methods:

```go
func (h *Handler) ListTechnicalUsers(w http.ResponseWriter, r *http.Request) {
    h.stub(w, r)
}
```

Same removal for `CreateTechnicalUser`, `DeleteTechnicalUser`, `ResetTechnicalUserSecret` (we'll redeclare them in Tasks 7-9). Leave `GetTechnicalUserToken` in place for now (rewritten in Task 10).

```bash
go build ./internal/domain/account/
```

Expected: build error — `Handler` no longer satisfies `genapi.ServerInterface`. The new methods land below.

- [ ] **Step 6.3: Add `ListTechnicalUsers` to `m2m_adapter.go`.**

Append to `internal/domain/account/m2m_adapter.go`:

```go
// ListTechnicalUsers implements GET /clients.
func (h *Handler) ListTechnicalUsers(w http.ResponseWriter, r *http.Request) {
    if !auth.RequireAdmin(w, r) {
        return
    }
    if !h.requireM2MStore(w, r) {
        return
    }
    tID := tenantFromCtx(r)
    all := h.m2mClientStore.List()
    out := make([]genapi.TechnicalUserDto, 0, len(all))
    for _, c := range all {
        if !clientBelongsToTenant(c, tID) {
            continue
        }
        out = append(out, toTechnicalUserDto(c))
    }
    w.Header().Set("Content-Type", "application/json")
    _ = json.NewEncoder(w).Encode(out)
}
```

Remove the placeholder `var _ = slog.LevelInfo` and `var _ = time.Now` lines from the top of the file (they were stubs; the operation methods consume the imports now). Drop the `"log/slog"` and `"time"` imports if they become unused; keep them if Task 7 will use them.

Actually `slog` and `time` will be referenced in later tasks (Task 7's `withAdminRole` logging, Task 10's stub). Keep both imports — but remove the placeholder lines:

```bash
# In m2m_adapter.go, delete the two lines:
# var _ = slog.LevelInfo
# var _ = time.Now
```

- [ ] **Step 6.4: Run the tests.**

```bash
go test ./internal/domain/account/ -run TestListTechnicalUsers -v
```

Expected: all 5 cases PASS.

- [ ] **Step 6.5: Commit.**

```bash
git add internal/domain/account/m2m_adapter.go internal/domain/account/m2m_adapter_test.go internal/domain/account/handler.go
git commit -m "feat(account): implement ListTechnicalUsers chi adapter

GET /clients adapter routes through requireAdmin + requireM2MStore +
tenant filter. Removes the matching stub from handler.go; the chi
ServerInterface is now satisfied by m2m_adapter.go for this op.

Refs #282."
```

---

## Task 7: Implement `CreateTechnicalUser` (TDD)

**Files:**
- Modify: `internal/domain/account/m2m_adapter.go`
- Modify: `internal/domain/account/m2m_adapter_test.go`

The OpenAPI tightening to `type: boolean` (D8 in the spec) lands in Task 12. Until then, the generated `CreateTechnicalUserParams.WithAdminRole` is `*string`. The adapter parses both shapes: `nil` → false, `"true"` → true, `"false"` → false, anything else → 400 `BAD_REQUEST`. After Task 12 the param becomes `*bool` and the parse simplifies; the test cases adjust then.

- [ ] **Step 7.1: Add RED tests for `CreateTechnicalUser`.**

Append to `internal/domain/account/m2m_adapter_test.go`:

```go
// --- Case 6: Create, admin, withAdminRole absent ---

func TestCreateTechnicalUser_AdminNoFlag_Returns200WithM2MRoleOnly(t *testing.T) {
    h := newM2MAdapterFixture(t, false)
    req := withTenantAdminCtx(httptest.NewRequest(http.MethodPost, "/clients", nil), tenantA)
    rr := httptest.NewRecorder()

    h.CreateTechnicalUser(rr, req, genapi.CreateTechnicalUserParams{})

    if rr.Code != http.StatusOK {
        t.Fatalf("status: got %d want 200, body=%s", rr.Code, rr.Body.String())
    }
    var creds genapi.TechnicalUserCredentialsDto
    if err := json.Unmarshal(rr.Body.Bytes(), &creds); err != nil {
        t.Fatalf("decode: %v", err)
    }
    if !clientIDPattern.MatchString(creds.ClientId) || len(creds.ClientId) != 16 {
        t.Errorf("clientId %q: want 16-char [A-Za-z0-9]", creds.ClientId)
    }
    if creds.ClientSecret == "" {
        t.Error("client_secret empty")
    }
    if string(creds.GrantType) != "client_credentials" {
        t.Errorf("grant_type: got %q want client_credentials", creds.GrantType)
    }
    if creds.ClientSecretExpiresAt != 0 {
        t.Errorf("client_secret_expires_at: got %d want 0", creds.ClientSecretExpiresAt)
    }

    // Store side: roles == ["ROLE_M2M"], CreatedAt == UpdatedAt, tenant == caller.
    stored, err := h.m2mClientStore.Get(creds.ClientId)
    if err != nil {
        t.Fatalf("Get(%s): %v", creds.ClientId, err)
    }
    if len(stored.Roles) != 1 || stored.Roles[0] != "ROLE_M2M" {
        t.Errorf("stored roles: got %v want [ROLE_M2M]", stored.Roles)
    }
    if stored.TenantID != spi.TenantID(tenantA) {
        t.Errorf("stored tenant: got %q want %q", stored.TenantID, tenantA)
    }
    if !stored.CreatedAt.Equal(stored.UpdatedAt) {
        t.Errorf("CreatedAt (%v) != UpdatedAt (%v)", stored.CreatedAt, stored.UpdatedAt)
    }
}

// --- Case 7: Create, admin, withAdminRole=true, flag on ---

func TestCreateTechnicalUser_AdminWithAdminRoleFlagOn_AddsAdminRole(t *testing.T) {
    h := newM2MAdapterFixture(t, true)
    val := "true"
    req := withTenantAdminCtx(httptest.NewRequest(http.MethodPost, "/clients?withAdminRole=true", nil), tenantA)
    rr := httptest.NewRecorder()

    h.CreateTechnicalUser(rr, req, genapi.CreateTechnicalUserParams{WithAdminRole: &val})

    if rr.Code != http.StatusOK {
        t.Fatalf("status: got %d want 200, body=%s", rr.Code, rr.Body.String())
    }
    var creds genapi.TechnicalUserCredentialsDto
    if err := json.Unmarshal(rr.Body.Bytes(), &creds); err != nil {
        t.Fatalf("decode: %v", err)
    }
    stored, err := h.m2mClientStore.Get(creds.ClientId)
    if err != nil {
        t.Fatalf("Get: %v", err)
    }
    want := map[string]bool{"ROLE_M2M": true, "ROLE_ADMIN": true}
    got := map[string]bool{}
    for _, r := range stored.Roles {
        got[r] = true
    }
    for k := range want {
        if !got[k] {
            t.Errorf("stored roles missing %q: %v", k, stored.Roles)
        }
    }
}

// --- Case 8: Create, admin, withAdminRole=true, flag OFF ---

func TestCreateTechnicalUser_AdminWithAdminRoleFlagOff_Returns404FeatureDisabled(t *testing.T) {
    h := newM2MAdapterFixture(t, false)
    val := "true"
    req := withTenantAdminCtx(httptest.NewRequest(http.MethodPost, "/clients?withAdminRole=true", nil), tenantA)
    rr := httptest.NewRecorder()

    h.CreateTechnicalUser(rr, req, genapi.CreateTechnicalUserParams{WithAdminRole: &val})

    if rr.Code != http.StatusNotFound {
        t.Fatalf("status: got %d want 404", rr.Code)
    }
    if code := decodeErrCode(t, rr.Body.Bytes()); code != common.ErrCodeFeatureDisabled {
        t.Errorf("errorCode: got %q want %q", code, common.ErrCodeFeatureDisabled)
    }
    // No store record should have been created.
    if len(h.m2mClientStore.List()) != 0 {
        t.Errorf("store should remain empty on FEATURE_DISABLED, got %d records", len(h.m2mClientStore.List()))
    }
}

// --- Case 9: Create, admin, withAdminRole=false (explicit) ---

func TestCreateTechnicalUser_AdminWithAdminRoleFalse_NoAdminRole(t *testing.T) {
    h := newM2MAdapterFixture(t, true)
    val := "false"
    req := withTenantAdminCtx(httptest.NewRequest(http.MethodPost, "/clients?withAdminRole=false", nil), tenantA)
    rr := httptest.NewRecorder()

    h.CreateTechnicalUser(rr, req, genapi.CreateTechnicalUserParams{WithAdminRole: &val})

    if rr.Code != http.StatusOK {
        t.Fatalf("status: got %d want 200", rr.Code)
    }
    var creds genapi.TechnicalUserCredentialsDto
    _ = json.Unmarshal(rr.Body.Bytes(), &creds)
    stored, _ := h.m2mClientStore.Get(creds.ClientId)
    for _, r := range stored.Roles {
        if r == "ROLE_ADMIN" {
            t.Errorf("ROLE_ADMIN must NOT be present when withAdminRole=false; got %v", stored.Roles)
        }
    }
}

// --- Case 10: Create, non-admin → 403 FORBIDDEN ---

func TestCreateTechnicalUser_NonAdmin_Returns403Forbidden(t *testing.T) {
    h := newM2MAdapterFixture(t, true)
    req := withTenantNonAdminCtx(httptest.NewRequest(http.MethodPost, "/clients", nil), tenantA)
    rr := httptest.NewRecorder()

    h.CreateTechnicalUser(rr, req, genapi.CreateTechnicalUserParams{})

    if rr.Code != http.StatusForbidden {
        t.Fatalf("status: got %d want 403", rr.Code)
    }
}

// --- Case 11: Invalid withAdminRole value (string mode) ---

func TestCreateTechnicalUser_InvalidWithAdminRoleValue_Returns400BadRequest(t *testing.T) {
    h := newM2MAdapterFixture(t, true)
    val := "yes" // not "true"/"false"
    req := withTenantAdminCtx(httptest.NewRequest(http.MethodPost, "/clients?withAdminRole=yes", nil), tenantA)
    rr := httptest.NewRecorder()

    h.CreateTechnicalUser(rr, req, genapi.CreateTechnicalUserParams{WithAdminRole: &val})

    if rr.Code != http.StatusBadRequest {
        t.Fatalf("status: got %d want 400", rr.Code)
    }
    if code := decodeErrCode(t, rr.Body.Bytes()); code != common.ErrCodeBadRequest {
        t.Errorf("errorCode: got %q want %q", code, common.ErrCodeBadRequest)
    }
}
```

- [ ] **Step 7.2: Confirm RED.**

```bash
go test ./internal/domain/account/ -run TestCreateTechnicalUser -v
```

Expected: compile error or `Handler.CreateTechnicalUser` not found.

- [ ] **Step 7.3: Add `CreateTechnicalUser` to `m2m_adapter.go`.**

Append to `internal/domain/account/m2m_adapter.go`:

```go
// CreateTechnicalUser implements POST /clients?withAdminRole=<bool>.
// Generates a 16-char base32-hex clientId and returns the freshly issued
// plaintext secret exactly once.
func (h *Handler) CreateTechnicalUser(w http.ResponseWriter, r *http.Request, params genapi.CreateTechnicalUserParams) {
    if !auth.RequireAdmin(w, r) {
        return
    }
    if !h.requireM2MStore(w, r) {
        return
    }

    withAdmin, ok := parseWithAdminRole(params.WithAdminRole)
    if !ok {
        common.WriteError(w, r, common.Operational(http.StatusBadRequest,
            common.ErrCodeBadRequest, "invalid withAdminRole; expected true or false"))
        return
    }
    if withAdmin && !h.gateM2MAdminRole(w, r) {
        return
    }

    // Generate clientId with one collision retry.
    var clientID string
    for attempt := 0; attempt < 2; attempt++ {
        cid, err := generateClientID()
        if err != nil {
            common.WriteError(w, r, common.Internal("generateClientID", err))
            return
        }
        if _, getErr := h.m2mClientStore.Get(cid); getErr != nil {
            clientID = cid
            break
        }
        // Collision (astronomical at 80 bits). Loop once more.
    }
    if clientID == "" {
        common.WriteError(w, r, common.Internal("generateClientID-collision",
            errors.New("clientId collision after retry")))
        return
    }

    tID := tenantFromCtx(r)
    roles := []string{"ROLE_M2M"}
    if withAdmin {
        roles = append(roles, "ROLE_ADMIN")
    }

    secret, err := h.m2mClientStore.Create(clientID, tID, clientID, roles)
    if err != nil {
        common.WriteError(w, r, common.Internal("m2mClientStore.Create", err))
        return
    }

    w.Header().Set("Content-Type", "application/json")
    _ = json.NewEncoder(w).Encode(toTechnicalUserCredentialsDto(clientID, secret, roles))
}

// parseWithAdminRole handles the transitional *string shape generated for the
// withAdminRole query param. Once Task 12 tightens the OpenAPI to type:
// boolean, this becomes a one-line bool deref.
//
// Returns (false, true) for nil/absent. Returns (true, true) for "true",
// (false, true) for "false". Returns (_, false) for anything else.
func parseWithAdminRole(p *string) (bool, bool) {
    if p == nil {
        return false, true
    }
    switch *p {
    case "true":
        return true, true
    case "false":
        return false, true
    default:
        return false, false
    }
}
```

- [ ] **Step 7.4: Run the tests.**

```bash
go test ./internal/domain/account/ -run TestCreateTechnicalUser -v
```

Expected: 6 cases PASS.

- [ ] **Step 7.5: Add the collision-retry test.**

Append to `internal/domain/account/m2m_adapter_test.go`:

```go
// --- Case 12: clientId collision → retry succeeds ---

// generateClientIDOnce/Twice exercises the retry loop by pre-seeding the
// store with a record whose id matches generateClientID's *next* output.
// We can't mock crypto/rand, so this test instead asserts the looser
// guarantee that the adapter handles a Get-success on first attempt by
// looping once. Construct the scenario by seeding the *expected* output
// space: since the random clientId is unpredictable, the actual collision
// path is unreachable from a black-box test. We instead verify the
// invariant directly: 100 sequential creates produce 100 distinct stored
// clientIds with no overwrites.

func TestCreateTechnicalUser_RepeatedCreates_NoCollisions(t *testing.T) {
    h := newM2MAdapterFixture(t, false)
    seen := map[string]bool{}
    const n = 100
    for i := 0; i < n; i++ {
        req := withTenantAdminCtx(httptest.NewRequest(http.MethodPost, "/clients", nil), tenantA)
        rr := httptest.NewRecorder()
        h.CreateTechnicalUser(rr, req, genapi.CreateTechnicalUserParams{})
        if rr.Code != http.StatusOK {
            t.Fatalf("iter %d: status %d, body=%s", i, rr.Code, rr.Body.String())
        }
        var creds genapi.TechnicalUserCredentialsDto
        _ = json.Unmarshal(rr.Body.Bytes(), &creds)
        if seen[creds.ClientId] {
            t.Fatalf("clientId collision at iter %d: %q reused", i, creds.ClientId)
        }
        seen[creds.ClientId] = true
    }
    if len(h.m2mClientStore.List()) != n {
        t.Errorf("store size: got %d want %d (some Create silently overwrote?)", len(h.m2mClientStore.List()), n)
    }
}
```

- [ ] **Step 7.6: Run the new test.**

```bash
go test ./internal/domain/account/ -run TestCreateTechnicalUser_RepeatedCreates_NoCollisions -v
```

Expected: PASS.

- [ ] **Step 7.7: Run the whole adapter test file.**

```bash
go test ./internal/domain/account/ -run TestListTechnicalUsers -v
go test ./internal/domain/account/ -run TestCreateTechnicalUser -v
```

Both PASS.

- [ ] **Step 7.8: Commit.**

```bash
git add internal/domain/account/m2m_adapter.go internal/domain/account/m2m_adapter_test.go
git commit -m "feat(account): implement CreateTechnicalUser chi adapter

POST /clients?withAdminRole=<bool> generates a 16-char base32-hex
clientId (with collision retry), accepts the M2M role plus optional
ROLE_ADMIN gated on M2MAdminRoleEnabled, returns the spec-conformant
TechnicalUserCredentialsDto.

Refs #282."
```

---

## Task 8: Implement `DeleteTechnicalUser` (TDD)

**Files:**
- Modify: `internal/domain/account/m2m_adapter.go`
- Modify: `internal/domain/account/m2m_adapter_test.go`

- [ ] **Step 8.1: Add RED tests.**

Append to `internal/domain/account/m2m_adapter_test.go`:

```go
// --- Case 13: Delete, admin, owned ---

func TestDeleteTechnicalUser_AdminOwned_Returns200AndRemoves(t *testing.T) {
    h := newM2MAdapterFixture(t, false)
    if _, err := h.m2mClientStore.Create("CLIENT1", spi.TenantID(tenantA), "CLIENT1", []string{"ROLE_M2M"}); err != nil {
        t.Fatalf("seed: %v", err)
    }
    req := withTenantAdminCtx(httptest.NewRequest(http.MethodDelete, "/clients/CLIENT1", nil), tenantA)
    rr := httptest.NewRecorder()

    h.DeleteTechnicalUser(rr, req, "CLIENT1")

    if rr.Code != http.StatusOK {
        t.Fatalf("status: got %d want 200, body=%s", rr.Code, rr.Body.String())
    }
    var resp genapi.DeleteTechnicalUser200ResponseDto
    if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
        t.Fatalf("decode: %v", err)
    }
    if resp.ClientId != "CLIENT1" {
        t.Errorf("clientId: got %q want CLIENT1", resp.ClientId)
    }
    if resp.Message == "" {
        t.Error("message empty")
    }
    if _, err := h.m2mClientStore.Get("CLIENT1"); err == nil {
        t.Error("store record still present after Delete")
    }
}

// --- Case 14: Delete, admin, cross-tenant target ---

func TestDeleteTechnicalUser_AdminCrossTenant_Returns404AndPreservesRecord(t *testing.T) {
    h := newM2MAdapterFixture(t, false)
    if _, err := h.m2mClientStore.Create("CLIENTB", spi.TenantID(tenantB), "CLIENTB", []string{"ROLE_M2M"}); err != nil {
        t.Fatalf("seed: %v", err)
    }
    req := withTenantAdminCtx(httptest.NewRequest(http.MethodDelete, "/clients/CLIENTB", nil), tenantA)
    rr := httptest.NewRecorder()

    h.DeleteTechnicalUser(rr, req, "CLIENTB")

    if rr.Code != http.StatusNotFound {
        t.Fatalf("status: got %d want 404", rr.Code)
    }
    if code := decodeErrCode(t, rr.Body.Bytes()); code != common.ErrCodeM2MClientNotFound {
        t.Errorf("errorCode: got %q want %q", code, common.ErrCodeM2MClientNotFound)
    }
    // Tenant B's record must remain untouched.
    if _, err := h.m2mClientStore.Get("CLIENTB"); err != nil {
        t.Errorf("tenant B record removed by cross-tenant DELETE: %v", err)
    }
}

// --- Case 15: Delete, admin, unknown id ---

func TestDeleteTechnicalUser_AdminUnknown_Returns404(t *testing.T) {
    h := newM2MAdapterFixture(t, false)
    req := withTenantAdminCtx(httptest.NewRequest(http.MethodDelete, "/clients/UNKNOWN", nil), tenantA)
    rr := httptest.NewRecorder()

    h.DeleteTechnicalUser(rr, req, "UNKNOWN")

    if rr.Code != http.StatusNotFound {
        t.Fatalf("status: got %d want 404", rr.Code)
    }
    if code := decodeErrCode(t, rr.Body.Bytes()); code != common.ErrCodeM2MClientNotFound {
        t.Errorf("errorCode: got %q want %q", code, common.ErrCodeM2MClientNotFound)
    }
}

// --- Case 16: Delete, admin, malformed id (hyphen) ---

func TestDeleteTechnicalUser_AdminMalformedId_Returns400(t *testing.T) {
    h := newM2MAdapterFixture(t, false)
    req := withTenantAdminCtx(httptest.NewRequest(http.MethodDelete, "/clients/bad-id", nil), tenantA)
    rr := httptest.NewRecorder()

    h.DeleteTechnicalUser(rr, req, "bad-id")

    if rr.Code != http.StatusBadRequest {
        t.Fatalf("status: got %d want 400", rr.Code)
    }
    if code := decodeErrCode(t, rr.Body.Bytes()); code != common.ErrCodeBadRequest {
        t.Errorf("errorCode: got %q want %q", code, common.ErrCodeBadRequest)
    }
}

// --- Case 17: Delete, admin, empty id (trailing slash) ---

func TestDeleteTechnicalUser_AdminEmptyId_Returns400(t *testing.T) {
    h := newM2MAdapterFixture(t, false)
    req := withTenantAdminCtx(httptest.NewRequest(http.MethodDelete, "/clients/", nil), tenantA)
    rr := httptest.NewRecorder()

    h.DeleteTechnicalUser(rr, req, "")

    if rr.Code != http.StatusBadRequest {
        t.Fatalf("status: got %d want 400", rr.Code)
    }
}

// --- Case 18: Delete, non-admin → 403 ---

func TestDeleteTechnicalUser_NonAdmin_Returns403(t *testing.T) {
    h := newM2MAdapterFixture(t, false)
    if _, err := h.m2mClientStore.Create("CLIENT1", spi.TenantID(tenantA), "CLIENT1", []string{"ROLE_M2M"}); err != nil {
        t.Fatalf("seed: %v", err)
    }
    req := withTenantNonAdminCtx(httptest.NewRequest(http.MethodDelete, "/clients/CLIENT1", nil), tenantA)
    rr := httptest.NewRecorder()

    h.DeleteTechnicalUser(rr, req, "CLIENT1")

    if rr.Code != http.StatusForbidden {
        t.Fatalf("status: got %d want 403", rr.Code)
    }
}
```

- [ ] **Step 8.2: Confirm RED.**

```bash
go test ./internal/domain/account/ -run TestDeleteTechnicalUser -v
```

Expected: compile error (`Handler.DeleteTechnicalUser` not defined).

- [ ] **Step 8.3: Add `DeleteTechnicalUser` to `m2m_adapter.go`.**

Append:

```go
// DeleteTechnicalUser implements DELETE /clients/{clientId}.
func (h *Handler) DeleteTechnicalUser(w http.ResponseWriter, r *http.Request, clientID string) {
    if !auth.RequireAdmin(w, r) {
        return
    }
    if !h.requireM2MStore(w, r) {
        return
    }
    if !validateClientID(w, r, clientID) {
        return
    }

    tID := tenantFromCtx(r)
    existing, err := h.m2mClientStore.Get(clientID)
    if err != nil || !clientBelongsToTenant(existing, tID) {
        // Identical 404 for "no such client" and "owned by another tenant"
        // — no cross-tenant existence oracle (Gate 3).
        common.WriteError(w, r, common.Operational(http.StatusNotFound,
            common.ErrCodeM2MClientNotFound, "M2M client not found"))
        return
    }
    if err := h.m2mClientStore.Delete(clientID); err != nil {
        // Race with concurrent delete: same 404 shape.
        common.WriteError(w, r, common.Operational(http.StatusNotFound,
            common.ErrCodeM2MClientNotFound, "M2M client not found"))
        return
    }

    resp := genapi.DeleteTechnicalUser200ResponseDto{
        Message:  "M2M client deleted successfully",
        ClientId: clientID,
    }
    w.Header().Set("Content-Type", "application/json")
    _ = json.NewEncoder(w).Encode(resp)
}
```

- [ ] **Step 8.4: Run the tests.**

```bash
go test ./internal/domain/account/ -run TestDeleteTechnicalUser -v
```

Expected: 6 cases PASS.

- [ ] **Step 8.5: Commit.**

```bash
git add internal/domain/account/m2m_adapter.go internal/domain/account/m2m_adapter_test.go
git commit -m "feat(account): implement DeleteTechnicalUser chi adapter

DELETE /clients/{clientId} adapter routes through requireAdmin +
requireM2MStore + clientId pattern validation + tenant-scope check.
Cross-tenant and unknown-id paths both return 404
M2M_CLIENT_NOT_FOUND.

Refs #282."
```

---

## Task 9: Implement `ResetTechnicalUserSecret` (TDD)

**Files:**
- Modify: `internal/domain/account/m2m_adapter.go`
- Modify: `internal/domain/account/m2m_adapter_test.go`

- [ ] **Step 9.1: Add RED tests.**

Append to `internal/domain/account/m2m_adapter_test.go`:

```go
// --- Case 19: Reset, admin, owned ---

func TestResetTechnicalUserSecret_AdminOwned_Returns200AndRotatesSecret(t *testing.T) {
    h := newM2MAdapterFixture(t, false)
    plaintextOld, err := h.m2mClientStore.Create("CLIENTR", spi.TenantID(tenantA), "CLIENTR", []string{"ROLE_M2M"})
    if err != nil {
        t.Fatalf("seed: %v", err)
    }

    req := withTenantAdminCtx(httptest.NewRequest(http.MethodPut, "/clients/CLIENTR/secret", nil), tenantA)
    rr := httptest.NewRecorder()

    h.ResetTechnicalUserSecret(rr, req, "CLIENTR")

    if rr.Code != http.StatusOK {
        t.Fatalf("status: got %d want 200, body=%s", rr.Code, rr.Body.String())
    }
    var creds genapi.TechnicalUserCredentialsDto
    if err := json.Unmarshal(rr.Body.Bytes(), &creds); err != nil {
        t.Fatalf("decode: %v", err)
    }
    if creds.ClientId != "CLIENTR" {
        t.Errorf("client_id: got %q want CLIENTR", creds.ClientId)
    }
    if creds.ClientSecret == plaintextOld {
        t.Error("new client_secret equals old (reset did not rotate)")
    }
    // VerifySecret accepts the new and rejects the old.
    okNew, _ := h.m2mClientStore.VerifySecret("CLIENTR", creds.ClientSecret)
    if !okNew {
        t.Error("new secret does not verify against store")
    }
    okOld, _ := h.m2mClientStore.VerifySecret("CLIENTR", plaintextOld)
    if okOld {
        t.Error("old secret still verifies after reset")
    }
    // UpdatedAt advanced.
    stored, _ := h.m2mClientStore.Get("CLIENTR")
    if !stored.UpdatedAt.After(stored.CreatedAt) {
        t.Errorf("UpdatedAt did not advance past CreatedAt (created=%v updated=%v)", stored.CreatedAt, stored.UpdatedAt)
    }
}

// --- Case 20: Reset, cross-tenant ---

func TestResetTechnicalUserSecret_AdminCrossTenant_Returns404AndPreservesSecret(t *testing.T) {
    h := newM2MAdapterFixture(t, false)
    plaintextB, _ := h.m2mClientStore.Create("CLIENTB", spi.TenantID(tenantB), "CLIENTB", []string{"ROLE_M2M"})

    req := withTenantAdminCtx(httptest.NewRequest(http.MethodPut, "/clients/CLIENTB/secret", nil), tenantA)
    rr := httptest.NewRecorder()

    h.ResetTechnicalUserSecret(rr, req, "CLIENTB")

    if rr.Code != http.StatusNotFound {
        t.Fatalf("status: got %d want 404", rr.Code)
    }
    if code := decodeErrCode(t, rr.Body.Bytes()); code != common.ErrCodeM2MClientNotFound {
        t.Errorf("errorCode: got %q want %q", code, common.ErrCodeM2MClientNotFound)
    }
    okB, _ := h.m2mClientStore.VerifySecret("CLIENTB", plaintextB)
    if !okB {
        t.Error("tenant B's secret was rotated by cross-tenant reset")
    }
}

// --- Case 21: Reset, unknown id ---

func TestResetTechnicalUserSecret_AdminUnknown_Returns404(t *testing.T) {
    h := newM2MAdapterFixture(t, false)
    req := withTenantAdminCtx(httptest.NewRequest(http.MethodPut, "/clients/UNKNOWN/secret", nil), tenantA)
    rr := httptest.NewRecorder()
    h.ResetTechnicalUserSecret(rr, req, "UNKNOWN")

    if rr.Code != http.StatusNotFound {
        t.Fatalf("status: got %d want 404", rr.Code)
    }
}

// --- Case 22: Reset, malformed id ---

func TestResetTechnicalUserSecret_AdminMalformedId_Returns400(t *testing.T) {
    h := newM2MAdapterFixture(t, false)
    req := withTenantAdminCtx(httptest.NewRequest(http.MethodPut, "/clients/bad-id/secret", nil), tenantA)
    rr := httptest.NewRecorder()
    h.ResetTechnicalUserSecret(rr, req, "bad-id")

    if rr.Code != http.StatusBadRequest {
        t.Fatalf("status: got %d want 400", rr.Code)
    }
}

// --- Case 23: Reset, empty id ---

func TestResetTechnicalUserSecret_AdminEmptyId_Returns400(t *testing.T) {
    h := newM2MAdapterFixture(t, false)
    req := withTenantAdminCtx(httptest.NewRequest(http.MethodPut, "/clients//secret", nil), tenantA)
    rr := httptest.NewRecorder()
    h.ResetTechnicalUserSecret(rr, req, "")

    if rr.Code != http.StatusBadRequest {
        t.Fatalf("status: got %d want 400", rr.Code)
    }
}

// --- Case 24: Reset, non-admin → 403 ---

func TestResetTechnicalUserSecret_NonAdmin_Returns403(t *testing.T) {
    h := newM2MAdapterFixture(t, false)
    _, _ = h.m2mClientStore.Create("CLIENT1", spi.TenantID(tenantA), "CLIENT1", []string{"ROLE_M2M"})

    req := withTenantNonAdminCtx(httptest.NewRequest(http.MethodPut, "/clients/CLIENT1/secret", nil), tenantA)
    rr := httptest.NewRecorder()
    h.ResetTechnicalUserSecret(rr, req, "CLIENT1")

    if rr.Code != http.StatusForbidden {
        t.Fatalf("status: got %d want 403", rr.Code)
    }
}
```

- [ ] **Step 9.2: Confirm RED.**

```bash
go test ./internal/domain/account/ -run TestResetTechnicalUserSecret -v
```

Expected: compile error (`Handler.ResetTechnicalUserSecret` not defined).

- [ ] **Step 9.3: Add `ResetTechnicalUserSecret` to `m2m_adapter.go`.**

Append:

```go
// ResetTechnicalUserSecret implements PUT /clients/{clientId}/secret.
func (h *Handler) ResetTechnicalUserSecret(w http.ResponseWriter, r *http.Request, clientID string) {
    if !auth.RequireAdmin(w, r) {
        return
    }
    if !h.requireM2MStore(w, r) {
        return
    }
    if !validateClientID(w, r, clientID) {
        return
    }

    tID := tenantFromCtx(r)
    existing, err := h.m2mClientStore.Get(clientID)
    if err != nil || !clientBelongsToTenant(existing, tID) {
        common.WriteError(w, r, common.Operational(http.StatusNotFound,
            common.ErrCodeM2MClientNotFound, "M2M client not found"))
        return
    }
    secret, err := h.m2mClientStore.ResetSecret(clientID)
    if err != nil {
        // Race with concurrent delete: 404. Other failures: 500.
        if strings.Contains(err.Error(), "not found") {
            common.WriteError(w, r, common.Operational(http.StatusNotFound,
                common.ErrCodeM2MClientNotFound, "M2M client not found"))
            return
        }
        common.WriteError(w, r, common.Internal("m2mClientStore.ResetSecret", err))
        return
    }

    w.Header().Set("Content-Type", "application/json")
    _ = json.NewEncoder(w).Encode(toTechnicalUserCredentialsDto(clientID, secret, existing.Roles))
}
```

- [ ] **Step 9.4: Run the tests.**

```bash
go test ./internal/domain/account/ -run TestResetTechnicalUserSecret -v
```

Expected: 6 cases PASS.

- [ ] **Step 9.5: Commit.**

```bash
git add internal/domain/account/m2m_adapter.go internal/domain/account/m2m_adapter_test.go
git commit -m "feat(account): implement ResetTechnicalUserSecret chi adapter

PUT /clients/{clientId}/secret rotates the bcrypt-hashed secret and
returns the new plaintext exactly once. Cross-tenant/unknown both
return 404 M2M_CLIENT_NOT_FOUND; race with concurrent delete maps
to the same shape.

Refs #282."
```

---

## Task 10: Rewrite `GetTechnicalUserToken` as defensive stub + add response-hygiene + token-shape tests

**Files:**
- Modify: `internal/domain/account/handler.go` (rewrite `GetTechnicalUserToken`)
- Modify: `internal/domain/account/m2m_adapter_test.go`

- [ ] **Step 10.1: Rewrite the stub.**

Edit `internal/domain/account/handler.go`. Find the existing `GetTechnicalUserToken` stub. Replace with:

```go
// GetTechnicalUserToken — defensive interface-satisfaction stub for
// POST /oauth/token. The real handler is the auth-service token handler
// mounted on the public mux at app/app.go:480, which intercepts before
// the chi router can reach this method. Arriving here means a routing
// regression — log + 500.
func (h *Handler) GetTechnicalUserToken(w http.ResponseWriter, r *http.Request, params genapi.GetTechnicalUserTokenParams) {
    slog.WarnContext(r.Context(),
        "chi /oauth/token reached — should be intercepted by public mux; routing regression?",
        "method", r.Method, "path", r.URL.Path)
    common.WriteError(w, r,
        common.Internal("getTechnicalUserToken-unreachable",
            errors.New("routing regression: chi served POST /oauth/token")))
}
```

Add `"errors"` and `"log/slog"` to `handler.go` imports if missing.

- [ ] **Step 10.2: Add response-hygiene test (Case 25 of the spec).**

Append to `internal/domain/account/m2m_adapter_test.go`:

```go
// --- Case 25: Response field hygiene ---
// Ensures no Create/Reset/List/Delete response surface ever serialises
// HashedSecret or any other private store field, by round-tripping the
// raw HTTP body through the generated DTO and asserting no leftover keys.

func TestM2MAdapter_ResponseFieldHygiene_NeverLeaksHashedSecret(t *testing.T) {
    h := newM2MAdapterFixture(t, true)

    // Create
    req := withTenantAdminCtx(httptest.NewRequest(http.MethodPost, "/clients", nil), tenantA)
    rr := httptest.NewRecorder()
    h.CreateTechnicalUser(rr, req, genapi.CreateTechnicalUserParams{})
    if rr.Code != http.StatusOK {
        t.Fatalf("Create: status %d", rr.Code)
    }
    assertNoHashedSecretLeak(t, "Create", rr.Body.Bytes())

    var creds genapi.TechnicalUserCredentialsDto
    _ = json.Unmarshal(rr.Body.Bytes(), &creds)
    clientID := creds.ClientId

    // Reset
    req = withTenantAdminCtx(httptest.NewRequest(http.MethodPut, "/clients/"+clientID+"/secret", nil), tenantA)
    rr = httptest.NewRecorder()
    h.ResetTechnicalUserSecret(rr, req, clientID)
    if rr.Code != http.StatusOK {
        t.Fatalf("Reset: status %d", rr.Code)
    }
    assertNoHashedSecretLeak(t, "Reset", rr.Body.Bytes())

    // List
    req = withTenantAdminCtx(httptest.NewRequest(http.MethodGet, "/clients", nil), tenantA)
    rr = httptest.NewRecorder()
    h.ListTechnicalUsers(rr, req)
    if rr.Code != http.StatusOK {
        t.Fatalf("List: status %d", rr.Code)
    }
    assertNoHashedSecretLeak(t, "List", rr.Body.Bytes())

    // Delete
    req = withTenantAdminCtx(httptest.NewRequest(http.MethodDelete, "/clients/"+clientID, nil), tenantA)
    rr = httptest.NewRecorder()
    h.DeleteTechnicalUser(rr, req, clientID)
    if rr.Code != http.StatusOK {
        t.Fatalf("Delete: status %d", rr.Code)
    }
    assertNoHashedSecretLeak(t, "Delete", rr.Body.Bytes())
}

func assertNoHashedSecretLeak(t *testing.T, op string, body []byte) {
    t.Helper()
    s := strings.ToLower(string(body))
    for _, bad := range []string{"hashedsecret", "hashed_secret", "hashed-secret", "bcrypt", "$2a$", "$2b$"} {
        if strings.Contains(s, bad) {
            t.Errorf("%s response contains forbidden token %q: %s", op, bad, body)
        }
    }
}
```

- [ ] **Step 10.3: Run the new tests + full file.**

```bash
go test ./internal/domain/account/ -run "TestM2MAdapter_ResponseFieldHygiene_NeverLeaksHashedSecret|TestListTechnicalUsers|TestCreateTechnicalUser|TestDeleteTechnicalUser|TestResetTechnicalUserSecret" -v
go build ./...
```

Expected: all PASS, project builds.

- [ ] **Step 10.4: Commit.**

```bash
git add internal/domain/account/handler.go internal/domain/account/m2m_adapter_test.go
git commit -m "feat(account): defensive GetTechnicalUserToken stub + hygiene test

Rewrites the unreachable chi-dispatched POST /oauth/token method to
log+500 (interface satisfaction only; public mux handles the real
request). Adds round-trip assertion that no /clients response leaks
HashedSecret or bcrypt-prefix fragments.

Refs #282."
```

---

## Task 11: Remove `/account/m2m` mux entries + retire legacy handler

**Files:**
- Modify: `app/app.go:470` (comment), `:491-492` (mux entries)
- Modify: `internal/auth/service.go:96, 106-107` (legacy wiring)
- Delete: `internal/auth/m2m.go`
- Delete: `internal/auth/m2m_test.go`
- Modify: `internal/auth/admin_authz_test.go` (M4 from spec — see Step 11.5)
- Modify: `internal/auth/integration_test.go:265-286` (delete adminSrv block)
- Modify: `app/app_test.go:198-200` (swap to /clients table rows)

- [ ] **Step 11.1: Remove the public mux entries in `app/app.go`.**

Edit `app/app.go`. Update the comment block ~line 470:

```go
    //   PUBLIC (no auth): /.well-known/jwks.json, POST /oauth/token.
    //     These are the OAuth2/OIDC discovery + token-exchange endpoints
    //     and must be reachable by unauthenticated callers by protocol.
    //
    //   ADMIN (authMW + ROLE_ADMIN): served via the chi router (account
    //     handler). The /account/m2m* legacy mux entries were retired in
    //     issue #282; see the m2m_adapter.go chi methods for /clients.
```

Delete these two lines ~line 491-492:

```go
        mux.Handle("/account/m2m/", authMW(authSvc.AdminHandler()))
        mux.Handle("/account/m2m", authMW(authSvc.AdminHandler()))
```

(Note: the commit message can use `#282`. Per project convention, the in-file *comment* should NOT name the issue number. Rewrite the comment without `#282`:)

```go
    //   ADMIN (authMW + ROLE_ADMIN): served via the chi router (account
    //     handler). The /account/m2m* legacy mux entries were retired
    //     when /clients chi adapters landed; see m2m_adapter.go.
```

- [ ] **Step 11.2: Remove the legacy wiring in `internal/auth/service.go`.**

Edit `internal/auth/service.go`. Remove line 96 (`m2mHandler := NewM2MHandler(m2mStore)`). Remove lines 106-107 (the two `adminMux.Handle("/account/m2m...", m2mHandler)`). Also remove the "M2M clients" mention from the comment on line 105 if present.

The remaining `adminMux` block should look like:

```go
    // Admin mux: key management (requires auth + ROLE_ADMIN).
    // Trusted-key endpoints moved to chi adapters in #281.
    // M2M-client endpoints moved to chi adapters (account.m2m_adapter).
    adminMux := http.NewServeMux()
```

If `adminMux` is now empty (only the comment remains), keep it — `AdminHandler()` may still be called by other tests. Verify by `grep -n adminMux internal/auth/service.go`.

- [ ] **Step 11.3: Delete `internal/auth/m2m.go`.**

```bash
git rm internal/auth/m2m.go
```

- [ ] **Step 11.4: Delete `internal/auth/m2m_test.go`.**

```bash
git rm internal/auth/m2m_test.go
```

- [ ] **Step 11.5: Update `internal/auth/admin_authz_test.go`.**

Edit `internal/auth/admin_authz_test.go`. Delete these three test functions:

- `TestM2MHandler_NonAdminForbidden` (~line 64)
- `TestM2MHandler_NoUserContextUnauthorized` (~line 83)
- `TestM2MHandler_AdminCanList` (~line 95)

Edit the two `TestRequireAdmin_*` cases (~line 131, ~line 147) — they use `NewM2MHandler(NewInMemoryM2MClientStore())` as a passthrough fixture. Replace with the keypair handler:

```go
// Before:
//   handler := NewM2MHandler(NewInMemoryM2MClientStore())
// After:
//   handler := NewKeyPairHandler(NewInMemoryKeyStore(), DefaultIAMFeatures())
```

If the keypair handler constructor differs, grep `NewKeyPairHandler` in `internal/auth/` to confirm the signature, or use `NewJWKSHandler(NewInMemoryKeyStore())` (a simpler unauthenticated handler is fine — `requireAdmin` triggers before any handler logic).

If neither constructor is straightforward, define a tiny inline handler:

```go
handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
    if !RequireAdmin(w, r) {
        return
    }
    w.WriteHeader(http.StatusOK)
})
```

- [ ] **Step 11.6: Delete the body-size assertion in `internal/auth/integration_test.go`.**

Edit `internal/auth/integration_test.go`. Find and remove the block starting around line 265:

```go
    // Admin endpoints are served on a separate handler; wrap with a test
    // middleware that supplies an admin UserContext so requireAdmin admits
    // the request and the body-size limit is the observable behaviour.
    adminSrv := httptest.NewServer(testAdminMW(svc.AdminHandler()))
    defer adminSrv.Close()

    t.Run("m2m create endpoint rejects oversized body", func(t *testing.T) {
        oversized := strings.Repeat("x", 1<<20+1)
        req, _ := http.NewRequest("POST", adminSrv.URL+"/account/m2m", strings.NewReader(oversized))
        req.Header.Set("Content-Type", "application/json")

        resp, err := http.DefaultClient.Do(req)
        if err != nil {
            t.Fatalf("POST /account/m2m: %v", err)
        }
        defer resp.Body.Close()

        if resp.StatusCode != http.StatusRequestEntityTooLarge && resp.StatusCode != http.StatusBadRequest {
            t.Errorf("expected 413 or 400, got %d", resp.StatusCode)
        }
    })
}
```

Delete the whole block. The surrounding test (`TestIntegration_*`) must still close cleanly — re-balance the braces.

If `strings` becomes an unused import after the deletion, run `goimports` or remove it manually. If `testAdminMW` becomes unused, remove it too.

- [ ] **Step 11.7: Update `app/app_test.go` route-presence table.**

Edit `app/app_test.go`. Find the test that lists routes (~line 195-205, exact location varies). Replace these three rows:

```go
{"POST", "/account/m2m"},
{"DELETE", "/account/m2m/some-client"},
{"POST", "/account/m2m/some-client/secret/reset"},
```

with these four:

```go
{"GET", "/clients"},
{"POST", "/clients"},
{"DELETE", "/clients/some-client"},
{"PUT", "/clients/some-client/secret"},
```

The test's purpose ("admin endpoint rejects unauthenticated") still holds.

- [ ] **Step 11.8: Run the affected suites.**

```bash
go test ./internal/auth/ ./app/ ./internal/domain/account/ -v
```

Expected: all PASS. Failures here mean either the `adminMux` block has an unused-variable warning (Go will complain), or a test still references `NewM2MHandler`. Fix in place.

- [ ] **Step 11.9: Commit.**

```bash
git add app/app.go internal/auth/service.go internal/auth/admin_authz_test.go internal/auth/integration_test.go app/app_test.go
git commit -m "refactor(auth): retire legacy /account/m2m surface

Removes the two mux.Handle(/account/m2m...) entries, the legacy
m2m.go HTTP handler + its tests, and the dependent route-presence
+ body-size assertions. The /clients chi adapter (Tasks 6-9) is
the supported surface. Private-cleanup only — /account/m2m was
never OpenAPI-declared.

Refs #282."
```

---

## Task 12: OpenAPI updates — strip 501s, prose flip, withAdminRole boolean

**Files:**
- Modify: `api/openapi.yaml` (operations: `listTechnicalUsers`, `createTechnicalUser`, `deleteTechnicalUser`, `resetTechnicalUserSecret`, `getTechnicalUserToken`)
- Regenerate: `api/generated.go`

- [ ] **Step 12.1: Strip `501` response declarations from the five operations.**

Open `api/openapi.yaml`. For each of the five operations, locate and delete the `"501":` block, e.g.:

```yaml
        "501":
          $ref: "#/components/responses/NotImplemented"
```

Use grep to find them:

```bash
grep -n "501\|operationId: listTechnicalUsers\|operationId: createTechnicalUser\|operationId: deleteTechnicalUser\|operationId: resetTechnicalUserSecret\|operationId: getTechnicalUserToken" api/openapi.yaml
```

Walk each operationId's block (between `operationId:` and the next path/operation) and remove the `"501":` two-line stanza.

- [ ] **Step 12.2: Flip "SUPER_USER" → "ROLE_ADMIN" in the prose of the five operations.**

Within each operation's `description:` block, replace the line:

```
        **Authorization Required:** SUPER_USER role
```

with:

```
        **Authorization Required:** ROLE_ADMIN role
```

Five occurrences total — one per operation. Use a careful one-by-one sed or interactive edit.

- [ ] **Step 12.3: Rewrite the `createTechnicalUser` disabled-flag prose.**

In the `createTechnicalUser` operation's `description:`, locate the line:

```
        This requires the `cyoda.security.web.jwt.m2m.admin-role-enabled`
        feature flag to be enabled;


        otherwise, a 401 Unauthorized response is returned.
```

Replace with:

```
        This requires the `CYODA_IAM_M2M_ADMIN_ROLE_ENABLED` env
        flag to be set to `true`;


        otherwise, a `404` response with error code `FEATURE_DISABLED` is returned.
```

- [ ] **Step 12.4: Tighten `withAdminRole` to `type: boolean`.**

Find the parameter block:

```yaml
        - name: withAdminRole
          in: query
          description: "When true, the created M2M client will additionally receive the
            ADMIN role. Requires the M2M admin role feature flag to be enabled.
            "
          required: false
          schema:
            type: string
```

Change `type: string` to `type: boolean`.

- [ ] **Step 12.5: Regenerate the API code.**

```bash
go generate ./api/...
```

This re-runs `go tool oapi-codegen --config=config.yaml openapi.yaml` per `api/generate.go`. The output `api/generated.go` changes: `CreateTechnicalUserParams.WithAdminRole` becomes `*bool` (not `*string`).

- [ ] **Step 12.6: Update the adapter parse logic for `*bool`.**

Edit `internal/domain/account/m2m_adapter.go`. Replace `parseWithAdminRole`:

```go
// parseWithAdminRole reads the *bool query param. nil/absent → false.
func parseWithAdminRole(p *bool) (bool, bool) {
    if p == nil {
        return false, true
    }
    return *p, true
}
```

The `(_, false)` "invalid" return shape stays in the signature for caller compatibility; the boolean form has no invalid value, so the second return is always `true`. The 400 BAD_REQUEST path in `CreateTechnicalUser` becomes unreachable. Update Case 11 (the test for the `"yes"` invalid value):

- Edit `internal/domain/account/m2m_adapter_test.go`. Delete `TestCreateTechnicalUser_InvalidWithAdminRoleValue_Returns400BadRequest` — the case is no longer applicable after the schema tightens to `boolean`.
- The 400-path inside `CreateTechnicalUser` can be removed too. Edit `m2m_adapter.go` and simplify:

```go
    withAdmin, _ := parseWithAdminRole(params.WithAdminRole)
    if withAdmin && !h.gateM2MAdminRole(w, r) {
        return
    }
```

The `if !ok` block goes away.

- [ ] **Step 12.7: Update Cases 7-9 to pass `*bool` instead of `*string`.**

Edit `internal/domain/account/m2m_adapter_test.go`. In Cases 7, 8, 9, replace:

```go
val := "true"
... genapi.CreateTechnicalUserParams{WithAdminRole: &val} ...
```

with:

```go
trueVal := true
... genapi.CreateTechnicalUserParams{WithAdminRole: &trueVal} ...
```

And for the `"false"` case in Case 9:

```go
falseVal := false
... genapi.CreateTechnicalUserParams{WithAdminRole: &falseVal} ...
```

- [ ] **Step 12.8: Run all affected tests.**

```bash
go build ./...
go test ./internal/domain/account/ -v
go test ./app/ -v
```

Expected: all PASS.

- [ ] **Step 12.9: Commit.**

```bash
git add api/openapi.yaml api/generated.go internal/domain/account/m2m_adapter.go internal/domain/account/m2m_adapter_test.go
git commit -m "feat(api): /clients operations match — 501 declarations off

- Strip 501 from listTechnicalUsers, createTechnicalUser,
  deleteTechnicalUser, resetTechnicalUserSecret, getTechnicalUserToken.
- Flip SUPER_USER prose to ROLE_ADMIN on those five.
- Tighten withAdminRole query param to type: boolean.
- Rewrite the disabled-flag prose to point at CYODA_IAM_M2M_ADMIN_ROLE_ENABLED
  and the 404 FEATURE_DISABLED response.

NB: withAdminRole boolean is a deliberate divergence from Cyoda Cloud's
upstream OpenAPI (which declares string) — flagged in the spec.

Refs #282."
```

---

## Task 13: E2E coverage

**Files:**
- Create: `internal/e2e/clients_test.go`
- Modify: `internal/e2e/e2e_test.go:117` (add `M2MAdminRoleEnabled = true` to harness)

- [ ] **Step 13.1: Enable the flag in the E2E harness.**

Edit `internal/e2e/e2e_test.go`. Find line 117 (`cfg.IAM.TrustedKeyRegistrationEnabled = true`). Add directly below:

```go
    cfg.IAM.TrustedKeyRegistrationEnabled = true
    cfg.IAM.M2MAdminRoleEnabled = true
```

- [ ] **Step 13.2: Create `internal/e2e/clients_test.go`.**

The pattern mirrors `internal/e2e/oauth_keys_test.go`. Inspect that file first to learn the harness helpers:

```bash
grep -n "func e2eAdmin\|func TestE2E\|baseURL\|adminBearer\|tenantBearer\|httpDo" internal/e2e/oauth_keys_test.go | head -10
```

Copy the helper invocation style from the closest existing test. The file:

```go
package e2e

import (
    "encoding/base64"
    "encoding/json"
    "fmt"
    "net/http"
    "strings"
    "testing"

    genapi "github.com/cyoda-platform/cyoda-go/api"
)

// TestE2E_Clients_ListEmpty asserts a fresh tenant sees [].
func TestE2E_Clients_ListEmpty(t *testing.T) {
    resp := adminGET(t, "/clients")
    defer resp.Body.Close()
    if resp.StatusCode != http.StatusOK {
        t.Fatalf("status: %d", resp.StatusCode)
    }
    var list []genapi.TechnicalUserDto
    decodeJSON(t, resp, &list)
    if len(list) != 0 {
        t.Errorf("len: %d, want 0", len(list))
    }
}

// TestE2E_Clients_CreateListRoundtrip creates a client, then verifies it
// appears in the list with roles=[ROLE_M2M] and CreationDate set.
func TestE2E_Clients_CreateListRoundtrip(t *testing.T) {
    resp := adminPOST(t, "/clients", nil)
    defer resp.Body.Close()
    if resp.StatusCode != http.StatusOK {
        t.Fatalf("create status: %d", resp.StatusCode)
    }
    var creds genapi.TechnicalUserCredentialsDto
    decodeJSON(t, resp, &creds)
    if creds.ClientId == "" || creds.ClientSecret == "" {
        t.Fatalf("creds: %+v", creds)
    }

    listResp := adminGET(t, "/clients")
    defer listResp.Body.Close()
    var list []genapi.TechnicalUserDto
    decodeJSON(t, listResp, &list)
    var found *genapi.TechnicalUserDto
    for i := range list {
        if list[i].ClientId == creds.ClientId {
            found = &list[i]
            break
        }
    }
    if found == nil {
        t.Fatalf("created client %s not in list", creds.ClientId)
    }
    if found.CreationDate.IsZero() {
        t.Errorf("CreationDate zero")
    }
    if !containsString(found.Roles, "ROLE_M2M") {
        t.Errorf("roles %v missing ROLE_M2M", found.Roles)
    }

    // Clean up.
    delResp := adminDELETE(t, "/clients/"+creds.ClientId)
    defer delResp.Body.Close()
    if delResp.StatusCode != http.StatusOK {
        t.Errorf("delete: %d", delResp.StatusCode)
    }
}

// TestE2E_Clients_TokenExchangeRoundtrip verifies that a freshly issued
// client_id/secret authenticates against POST /oauth/token and yields a
// JWT with sub=clientId and scopes including ROLE_M2M.
func TestE2E_Clients_TokenExchangeRoundtrip(t *testing.T) {
    resp := adminPOST(t, "/clients", nil)
    defer resp.Body.Close()
    var creds genapi.TechnicalUserCredentialsDto
    decodeJSON(t, resp, &creds)

    tokResp := basicTokenRequest(t, creds.ClientId, creds.ClientSecret)
    defer tokResp.Body.Close()
    if tokResp.StatusCode != http.StatusOK {
        b, _ := readAll(tokResp.Body)
        t.Fatalf("token: %d, body=%s", tokResp.StatusCode, b)
    }
    var tok genapi.TokenResponseDto
    decodeJSON(t, tokResp, &tok)
    if tok.AccessToken == "" {
        t.Fatalf("access_token empty")
    }
    claims := decodeJWTPayload(t, tok.AccessToken)
    if claims["sub"] != creds.ClientId {
        t.Errorf("sub: got %v want %v", claims["sub"], creds.ClientId)
    }
    scopes, _ := claims["scopes"].([]any)
    if !containsAnyString(scopes, "ROLE_M2M") {
        t.Errorf("scopes %v missing ROLE_M2M", scopes)
    }
}

// TestE2E_Clients_ResetSecretRotatesAuth verifies the old secret stops
// working after PUT /clients/{id}/secret.
func TestE2E_Clients_ResetSecretRotatesAuth(t *testing.T) {
    resp := adminPOST(t, "/clients", nil)
    defer resp.Body.Close()
    var creds genapi.TechnicalUserCredentialsDto
    decodeJSON(t, resp, &creds)

    rResp := adminPUT(t, "/clients/"+creds.ClientId+"/secret", nil)
    defer rResp.Body.Close()
    var newCreds genapi.TechnicalUserCredentialsDto
    decodeJSON(t, rResp, &newCreds)
    if newCreds.ClientSecret == creds.ClientSecret {
        t.Fatal("reset returned identical secret")
    }

    // Old secret fails.
    badResp := basicTokenRequest(t, creds.ClientId, creds.ClientSecret)
    defer badResp.Body.Close()
    if badResp.StatusCode == http.StatusOK {
        t.Errorf("old secret still authenticates: %d", badResp.StatusCode)
    }

    // New secret works.
    goodResp := basicTokenRequest(t, creds.ClientId, newCreds.ClientSecret)
    defer goodResp.Body.Close()
    if goodResp.StatusCode != http.StatusOK {
        t.Errorf("new secret should authenticate: %d", goodResp.StatusCode)
    }
}

// TestE2E_Clients_DeleteInvalidatesToken verifies that POST /oauth/token
// 401s with a deleted client's credentials.
func TestE2E_Clients_DeleteInvalidatesToken(t *testing.T) {
    resp := adminPOST(t, "/clients", nil)
    defer resp.Body.Close()
    var creds genapi.TechnicalUserCredentialsDto
    decodeJSON(t, resp, &creds)

    delResp := adminDELETE(t, "/clients/"+creds.ClientId)
    defer delResp.Body.Close()
    if delResp.StatusCode != http.StatusOK {
        t.Fatalf("delete: %d", delResp.StatusCode)
    }

    tokResp := basicTokenRequest(t, creds.ClientId, creds.ClientSecret)
    defer tokResp.Body.Close()
    if tokResp.StatusCode != http.StatusUnauthorized {
        t.Errorf("token after delete: got %d want 401", tokResp.StatusCode)
    }
}

// TestE2E_Clients_WithAdminRoleFlagOn issues a token with ROLE_ADMIN in
// scopes when withAdminRole=true and the flag is on (set by TestMain).
func TestE2E_Clients_WithAdminRoleFlagOn(t *testing.T) {
    resp := adminPOST(t, "/clients?withAdminRole=true", nil)
    defer resp.Body.Close()
    if resp.StatusCode != http.StatusOK {
        b, _ := readAll(resp.Body)
        t.Fatalf("status: %d, body=%s", resp.StatusCode, b)
    }
    var creds genapi.TechnicalUserCredentialsDto
    decodeJSON(t, resp, &creds)

    tokResp := basicTokenRequest(t, creds.ClientId, creds.ClientSecret)
    defer tokResp.Body.Close()
    var tok genapi.TokenResponseDto
    decodeJSON(t, tokResp, &tok)
    claims := decodeJWTPayload(t, tok.AccessToken)
    scopes, _ := claims["scopes"].([]any)
    if !containsAnyString(scopes, "ROLE_ADMIN") {
        t.Errorf("withAdminRole=true should include ROLE_ADMIN in scopes; got %v", scopes)
    }

    // Cleanup.
    _ = adminDELETE(t, "/clients/"+creds.ClientId).Body.Close()
}

// --- helpers (use the harness conventions from oauth_keys_test.go) ---

// adminGET, adminPOST, adminPUT, adminDELETE, decodeJSON, readAll,
// basicTokenRequest, decodeJWTPayload, containsString, containsAnyString
// are expected to exist in the e2e package already (alongside the trusted-
// keys helpers). If a helper is missing, define it below.

func basicTokenRequest(t *testing.T, clientID, secret string) *http.Response {
    t.Helper()
    body := strings.NewReader("grant_type=client_credentials")
    req, err := http.NewRequest(http.MethodPost, baseURL+"/oauth/token", body)
    if err != nil {
        t.Fatalf("new request: %v", err)
    }
    req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
    creds := base64.StdEncoding.EncodeToString([]byte(clientID + ":" + secret))
    req.Header.Set("Authorization", "Basic "+creds)
    resp, err := http.DefaultClient.Do(req)
    if err != nil {
        t.Fatalf("do: %v", err)
    }
    return resp
}

func decodeJWTPayload(t *testing.T, tokenStr string) map[string]any {
    t.Helper()
    parts := strings.Split(tokenStr, ".")
    if len(parts) != 3 {
        t.Fatalf("malformed JWT: %s", tokenStr)
    }
    raw, err := base64.RawURLEncoding.DecodeString(parts[1])
    if err != nil {
        t.Fatalf("decode payload: %v", err)
    }
    var claims map[string]any
    if err := json.Unmarshal(raw, &claims); err != nil {
        t.Fatalf("unmarshal claims: %v", err)
    }
    return claims
}

func containsString(haystack []string, needle string) bool {
    for _, s := range haystack {
        if s == needle {
            return true
        }
    }
    return false
}

func containsAnyString(haystack []any, needle string) bool {
    for _, s := range haystack {
        if str, ok := s.(string); ok && str == needle {
            return true
        }
    }
    return false
}

// Suppress unused-import linter if not all helpers fire (defensive).
var _ = fmt.Sprintf
```

Open `internal/e2e/oauth_keys_test.go` and confirm the actual helper names (`adminGET`, `adminPOST`, `decodeJSON`, etc.). If they differ in this codebase, rename the calls above to match. The harness conventions are stable across the e2e package per PR #281.

- [ ] **Step 13.3: Run the E2E suite.**

```bash
go test ./internal/e2e/ -run "TestE2E_Clients_" -v
```

Expected: all PASS. (Docker required for PostgreSQL testcontainer; the harness boots it on TestMain.)

- [ ] **Step 13.4: Commit.**

```bash
git add internal/e2e/clients_test.go internal/e2e/e2e_test.go
git commit -m "test(e2e): /clients OpenAPI conformance through full HTTP stack

Covers list/create/delete/reset roundtrips, client-credentials
token exchange, secret-rotation invalidation, delete-invalidates-
token, and the withAdminRole=true flag-on path. Flag-off case
remains in the unit suite per spec D9.

Refs #282."
```

---

## Task 14: Documentation

**Files:**
- Modify: `README.md` (config env-var table)
- Modify: `cmd/cyoda/help/content/config/auth.md` (IAM section)
- Modify: `cmd/cyoda/help/content/errors/FEATURE_DISABLED.md`
- Create: `cmd/cyoda/help/content/errors/M2M_CLIENT_NOT_FOUND.md`
- Modify: `CHANGELOG.md` (`## [0.8.0]` block)

- [ ] **Step 14.1: Add the env var to `README.md`.**

Open `README.md`. Find the configuration table that already includes `CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED`:

```bash
grep -n "TRUSTED_KEY_REGISTRATION_ENABLED" README.md
```

Add a row directly below it:

```markdown
| `CYODA_IAM_M2M_ADMIN_ROLE_ENABLED` | bool | `false` | When `true`, `POST /clients?withAdminRole=true` may grant `ROLE_ADMIN` to created M2M clients. When `false` (default), that request shape returns `404 FEATURE_DISABLED`. |
```

Adjust column count / formatting to match the surrounding rows.

- [ ] **Step 14.2: Add to `cmd/cyoda/help/content/config/auth.md`.**

Open `cmd/cyoda/help/content/config/auth.md`. Find the `CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED` block (~line 83-94 per spec recon):

```bash
grep -n "TRUSTED_KEY_REGISTRATION_ENABLED" cmd/cyoda/help/content/config/auth.md
```

Add a sibling block directly after it. Match the topic's existing prose style. Suggested text:

```markdown
- `CYODA_IAM_M2M_ADMIN_ROLE_ENABLED` — gates the `withAdminRole=true`
  query parameter on `POST /clients`. When `false` (default), that request
  shape returns `404` with error code `FEATURE_DISABLED` and no client is
  created. When `true`, the created M2M client receives both `ROLE_M2M`
  and `ROLE_ADMIN`. Toggling does not affect existing clients.
```

If the surrounding section has a worked example block ("With trusted-key registration enabled"), add an "With M2M admin-role grants enabled" sibling block following the same shape.

- [ ] **Step 14.3: Update `FEATURE_DISABLED.md`.**

Open `cmd/cyoda/help/content/errors/FEATURE_DISABLED.md`. The current DESCRIPTION lists trusted-key endpoints. Add an `/clients` paragraph after the trusted-key list:

```markdown
Also returned by `POST /clients?withAdminRole=true` when
`CYODA_IAM_M2M_ADMIN_ROLE_ENABLED=false` (the default). Enable by
setting the env var and restarting. Other `/clients` operations
(list, create-without-withAdminRole, delete, reset-secret) are
unaffected.
```

- [ ] **Step 14.4: Create `M2M_CLIENT_NOT_FOUND.md`.**

Create `cmd/cyoda/help/content/errors/M2M_CLIENT_NOT_FOUND.md`:

```markdown
---
topic: errors.M2M_CLIENT_NOT_FOUND
title: "M2M_CLIENT_NOT_FOUND — referenced technical user does not exist"
stability: stable
see_also:
  - errors
  - errors.UNAUTHORIZED
  - errors.FORBIDDEN
---

# errors.M2M_CLIENT_NOT_FOUND

## NAME

M2M_CLIENT_NOT_FOUND — an admin operation referenced a `clientId` that is not present in this tenant's M2M registry.

## SYNOPSIS

HTTP: `404` `Not Found`. Retryable: `no`.

## DESCRIPTION

Returned by `/clients` admin endpoints when the supplied `clientId` does not match any registered M2M client in the caller's tenant:

- `DELETE /clients/{clientId}` — the deletion target does not exist.
- `PUT /clients/{clientId}/secret` — the rotation target does not exist.

The detail field carries a generic `M2M client not found` message; internal store phrasing is never leaked into the response body.

Not retryable. Verify the `clientId` via `GET /clients` before retrying the operation.

Returned uniformly for `clientId`s that do not exist AND `clientId`s owned by another tenant; the response does not distinguish — by design, to prevent cross-tenant existence enumeration.

## SEE ALSO

- errors
- errors.UNAUTHORIZED
- errors.FORBIDDEN
```

- [ ] **Step 14.5: Update `CHANGELOG.md`.**

Open `CHANGELOG.md`. Find the `## [0.8.0]` block (it should already exist since PR #281 landed entries). Add entries to the existing sub-sections (create them if absent):

```markdown
### Added

- (existing entries...)
- `/clients` OpenAPI surface — `GET /clients`, `POST /clients`,
  `DELETE /clients/{clientId}`, `PUT /clients/{clientId}/secret`. M2M
  client management is now reachable at the spec-conformant paths
  with the spec-conformant DTOs.
- `CYODA_IAM_M2M_ADMIN_ROLE_ENABLED` env (default `false`) gates the
  `withAdminRole=true` query parameter on `POST /clients`. When off
  the request returns `404` with error code `FEATURE_DISABLED`.
- Error code `M2M_CLIENT_NOT_FOUND` (HTTP 404) emitted by the
  `/clients` admin operations on unknown or cross-tenant `clientId`.

### Changed

- (existing entries...)
- `withAdminRole` query parameter on `POST /clients` tightened from
  `string` to `boolean` in `api/openapi.yaml`. This is a deliberate
  divergence from the upstream Cyoda Cloud OpenAPI declaration.
- `auth.M2MClient.TenantID` promoted from `string` to `spi.TenantID`.
  `M2MClient` now carries `CreatedAt` and `UpdatedAt` timestamps.

### Removed

- Private `/account/m2m*` HTTP surface and its `internal/auth/m2m.go`
  handler. M2M client management is exclusively at `/clients` going
  forward. `/account/m2m*` was never OpenAPI-declared.
- `501 NOT_IMPLEMENTED` response declarations on `listTechnicalUsers`,
  `createTechnicalUser`, `deleteTechnicalUser`, `resetTechnicalUserSecret`,
  `getTechnicalUserToken` in `api/openapi.yaml`.

### Known limitations

- M2M clients created via `POST /clients` are held in-memory by the
  default `InMemoryM2MClientStore` and do not survive a server restart.
  Customers running with the in-memory IAM mode must re-create their
  clients on every restart. A persistence follow-up tracking storage-SPI
  backing is on the roadmap; see the v0.8.0 milestone discussion.
```

Do not insert `#282` or any other issue numbers in this body — per project convention, issue IDs belong only in PR bodies and git commit messages.

- [ ] **Step 14.6: Commit.**

```bash
git add README.md cmd/cyoda/help/content/config/auth.md cmd/cyoda/help/content/errors/FEATURE_DISABLED.md cmd/cyoda/help/content/errors/M2M_CLIENT_NOT_FOUND.md CHANGELOG.md
git commit -m "docs(iam): /clients env var, error codes, changelog

CYODA_IAM_M2M_ADMIN_ROLE_ENABLED documented in README and the
config.auth help topic. New M2M_CLIENT_NOT_FOUND help topic.
FEATURE_DISABLED help topic gains the /clients paragraph.
CHANGELOG entries for the new surface, the env var, the error
code, the /account/m2m removal, the boolean tightening, and the
restart-wipes-credentials known limitation.

Refs #282."
```

---

## Task 15: Pre-PR verification

**Files:** none (verification commands only)

- [ ] **Step 15.1: Vet the codebase.**

```bash
go vet ./...
```

Expected: clean.

- [ ] **Step 15.2: Full short test suite + plugins.**

```bash
make test-short-all
```

Expected: all PASS. (Docker required for the postgres plugin testcontainer.)

- [ ] **Step 15.3: Full E2E suite.**

```bash
go test ./internal/e2e/... -v
```

Expected: all PASS, including the new `/clients` cases.

- [ ] **Step 15.4: Race detector (end-of-deliverable per `.claude/rules/race-testing.md`).**

```bash
go test -race ./...
```

Expected: all PASS, no races detected. This step runs once before PR creation, not iteratively.

- [ ] **Step 15.5: Issue-ID scrub in shipped artefacts.**

Per project memory, no `#282`-shaped strings in code, OpenAPI text, error messages, log messages, response bodies, comments, or help-topic content.

```bash
grep -rn "#282\|issue 282\|issue/282" \
  --include="*.go" --include="*.yaml" --include="*.md" \
  --exclude-dir=docs/superpowers/specs \
  --exclude-dir=docs/superpowers/plans \
  --exclude-dir=.git \
  .
```

Expected: zero hits. (Spec + plan are exempted.)

- [ ] **Step 15.6: Audit-table flip.**

Locate the OpenAPI conformance audit (if one exists in the repo):

```bash
grep -rln "out-of-scope-not-implemented\|listTechnicalUsers" docs/superpowers/specs/
```

If a table with dispositions exists (likely in the umbrella `2026-06-04-194-decomposition-design.md` or a sibling audit file), update the five operation rows' disposition column from `out-of-scope-not-implemented` to `match`. If no such table is found, file a `// TODO(plan-locator)` note in the PR body for the human reviewer.

- [ ] **Step 15.7: Commit any audit/scrub touch-ups.**

If steps 15.5 or 15.6 produced edits:

```bash
git add -A
git commit -m "docs(audit): flip /clients operations to match disposition

Refs #282."
```

- [ ] **Step 15.8: Open the PR.**

```bash
git push -u origin feat/iam-clients-openapi-282
gh pr create --base release/v0.8.0 \
  --title "feat(iam): OpenAPI conformance for /clients (technical users)" \
  --body "$(cat <<'EOF'
Closes #282 (replaces part of #194).

## Summary

Surfaces existing M2M client management at the spec-conformant `/clients`
paths with spec-conformant DTOs. Retires `/account/m2m*`. Flips five
OpenAPI operations from `501` to `match`:

- `GET /clients` → `listTechnicalUsers`
- `POST /clients` → `createTechnicalUser` (with `withAdminRole` query param)
- `DELETE /clients/{clientId}` → `deleteTechnicalUser`
- `PUT /clients/{clientId}/secret` → `resetTechnicalUserSecret`
- `POST /oauth/token` → `getTechnicalUserToken` (already functional via public mux;
  spec only loses its `501` declaration)

## Spec & plan

- Spec: \`docs/superpowers/specs/2026-06-16-282-iam-clients-openapi-design.md\`
  (rev. 2, post-review)
- Plan: \`docs/superpowers/plans/2026-06-16-282-iam-clients-openapi.md\`

## Key decisions

- **Disabled-flag response**: `404` with `FEATURE_DISABLED`, matching the
  trusted-key precedent from #281 — not the OpenAPI's literal "401" wording.
- **`withAdminRole` schema**: tightened from `string` to `boolean` —
  a deliberate divergence from the upstream Cyoda Cloud OpenAPI, flagged for
  reviewer attention.
- **`/account/m2m*` removal**: private surface, never OpenAPI-declared;
  Gate 6 cleanup.

## Known limitation (v0.8.0 release notes)

M2M client credentials created via `POST /clients` do not survive process
restart with the default in-memory store. The persistence follow-up is
tracked separately for v0.9.0+ scheduling.

## Validation

- \`go test -race ./...\` ✓
- \`make test-short-all\` ✓
- \`go test ./internal/e2e/... -v\` ✓ (includes Docker testcontainer)
- \`go vet ./...\` ✓
- Issue-ID scrub clean.

🤖 Generated with [Claude Code](https://claude.com/claude-code)
EOF
)"
```

This is the milestone-targeting PR per the v0.8.0 release-branch workflow. Apply the v0.8.0 milestone to the PR (gh pr edit --milestone v0.8.0).

---

## Self-Review

**1. Spec coverage:**
- D1 (404 FEATURE_DISABLED) — Task 5 helper `gateM2MAdminRole`, Task 7 Case 8.
- D2 (16-char base32-hex + collision retry) — Task 5 `generateClientID`, Task 7 `CreateTechnicalUser` retry loop, Task 7 Case 12.
- D3 (delete `m2m.go`) — Task 11 (Steps 11.3, 11.4).
- D4 (delete body-size test) — Task 11 Step 11.6.
- D5 (SUPER_USER prose flip scope) — Task 12 Step 12.2.
- D6 (CreatedAt + UpdatedAt) — Task 2.
- D7 (no new SPI, use accessor) — Task 4.
- D8 (boolean schema) — Task 12 Steps 12.4-12.7.
- D9 (flag-off unit-only, E2E flag-on) — Task 7 Case 8 (unit), Task 13 Step 13.2 (E2E flag-on).
- D10 (CHANGELOG entries) — Task 14 Step 14.5.
- D11 (ROLE_M2M / ROLE_ADMIN strings) — Task 7 step 7.3, Tasks 6/7/8/9 assertions.
- D12 (real error codes, new M2M_CLIENT_NOT_FOUND) — Task 3 + every adapter task.
- D13 (TenantID → spi.TenantID) — Task 2.
- D14 (defensive 500 stub for GetTechnicalUserToken) — Task 10.
- §3.4 tenant isolation — adapter logic in Tasks 8/9, test Cases 14/20.
- §3.7 (security) — Task 10 hygiene test, Task 15.5 issue-ID scrub.
- §8 (docs) — Task 14 (all subtopics).

**2. Placeholder scan:** No "TBD" or "implement later". The audit-table location is the only fuzzy item (§4 of spec lists it as an open question); Task 15.6 handles it with a grep + a documented fallback (note in PR body).

**3. Type consistency:** `m2mClientStore` field name used consistently across handler.go, the adapter, and tests. `clientIDPattern` shared across all four operation methods. `parseWithAdminRole` returns `(bool, bool)` for both the *string and *bool versions — Task 12.6 simplifies but keeps the signature stable so call sites do not change.

**4. Plan completeness:** 15 tasks, each commit-bounded. RED tests precede implementation for every behavioural step. Final pre-PR verification is gated separately so the race-detector runs once.
