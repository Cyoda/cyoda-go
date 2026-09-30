# Platform Operator Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Only a platform operator — `ROLE_ADMIN` in the hard-coded tenant `PLATFORM` — may call the endpoints that change state every tenant shares; a tenant admin gets `403 FORBIDDEN`.

**Architecture:** A stateless `auth.OperatorGuard` value, built once in `app.New` from the IAM mode, replaces `auth.RequireAdmin` on the five key-pair handlers, the OIDC reload handler and the four `/admin/*` handlers. The rule rests on tenant binding (a tenant admin cannot obtain a principal in another tenant), so no role filtering is added anywhere. Test fixtures gain a required way to mint a `PLATFORM` operator token; every existing caller of an operator endpoint moves to it before the guard lands, so the suite stays green at every task.

**Tech Stack:** Go 1.26, `net/http`, oapi-codegen (`go generate ./api`), testcontainers PostgreSQL E2E, cross-backend parity suite.

**Spec:** `docs/superpowers/specs/2026-09-29-624-platform-operator-design.md` — read it before any task. Section numbers below (§n) refer to it.

## Global Constraints

- Operator tenant id: exactly `PLATFORM` (bytewise, case-sensitive), constant `auth.PlatformTenantID`. Not configurable.
- Operator rule (JWT mode): `ROLE_ADMIN` in `UserContext.Roles` AND `UserContext.Tenant.ID == "PLATFORM"`.
- Mock rule: applies if and only if `cfg.IAM.Mode == "mock"`; it is the `ROLE_ADMIN` check alone. Never compare a tenant id with `mock-tenant`.
- The zero value `auth.OperatorGuard{}` applies the JWT rule (fails closed).
- Responses: no `UserContext` → `401 UNAUTHORIZED` "authentication failed"; any refusal → `403 FORBIDDEN` "platform operator required". No new error code, no 404.
- `CYODA_IAM_MODE` accepts only `mock` or `jwt`; anything else fails startup.
- Tenant-scoped admin endpoints (trusted keys, M2M clients, OIDC register/update/invalidate/reactivate/delete, model/workflow admin) stay on `auth.RequireAdmin`.
- HTTP only: no gRPC entry point exists for any operator endpoint (waiver recorded in §6).
- No issue ids (`#624` etc.) in shipped artefacts: code, comments, help text, OpenAPI, error messages. Commit messages and PR body only.
- Go conventions: `log/slog` only; constructor DI; no test hooks or seams in production code.
- Tests: `make test` while iterating, `make test-full` at the end; never add `-count=1`; never `-v` on E2E.

## Review Focus

1. **A tenant admin whose `ROLE_ADMIN` comes from the tenant's own OIDC provider**, with a `caas_org_id: "PLATFORM"` claim in the token: expected 403 on every operator endpoint (the tenant is the provider owner's). Pinned by the parity scenario in Task 5.
2. **A tenant admin aiming at the bootstrap key itself** (invalidate / reactivate / delete by the bootstrap key's `kid`): expected 403, and the bootstrap key keeps verifying afterwards. Pinned in the Task 5 parity scenario.
3. **Lower-case `platform`, or `PLATFORM` without `ROLE_ADMIN`**: expected 403. Pinned in Task 2 (unit) and Task 5/6 (E2E).
4. **Mock mode**: key-pair endpoints still 501, `/admin/log-level` still works for the mock admin, a mock principal without `ROLE_ADMIN` still 403. Pinned in Task 5/6 (existing `TestGated_MockIAM_All21Return501` + new unit tests).
5. **Operator access through a `PLATFORM` admin M2M client after the bootstrap key is gone** (the recovery route): expected to keep key-pair control. Pinned by `TestSigningKeys_OwnCluster` (Task 3) and a shared-server E2E (Task 5).

## Execution order

- Task 1 and Task 2 are independent.
- Tasks 3 and 4 need Task 2's constant only; they are independent of each other.
- Task 5 needs 2, 3, 4. Task 6 needs 2 and 4. Task 7 needs 5 and 6. Task 8 needs 7.

---

### Task 1: `CYODA_IAM_MODE` accepts only `mock` or `jwt`

**Files:**
- Modify: `app/config.go:1000-1023` (`ValidateIAM`)
- Test: `app/config_require_jwt_test.go`
- Modify: `cmd/cyoda/help/content/config/auth.md:26` (state that any other value fails startup)
- Modify: `CHANGELOG.md` (`## [Unreleased]` → `### Breaking`)

**Interfaces:**
- Consumes: nothing.
- Produces: `ValidateIAM` returns an error for any `iam.Mode` other than `"mock"` or `"jwt"`.

- [ ] **Step 1: Write the failing test** (append to `app/config_require_jwt_test.go`)

```go
func TestValidateIAM_RejectsUnknownMode(t *testing.T) {
	for _, mode := range []string{"JWT", "Mock", "", "none", "jwt "} {
		t.Run(fmt.Sprintf("%q", mode), func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.IAM.RequireJWT = false
			cfg.IAM.Mode = mode
			err := ValidateIAM(cfg.IAM)
			if err == nil {
				t.Fatalf("mode %q: expected an error, got nil", mode)
			}
			if !strings.Contains(err.Error(), "CYODA_IAM_MODE") {
				t.Errorf("error should name CYODA_IAM_MODE: %v", err)
			}
		})
	}
}

func TestValidateIAM_AcceptsJWTWithoutRequireJWT(t *testing.T) {
	cfg := DefaultConfig()
	cfg.IAM.RequireJWT = false
	cfg.IAM.Mode = "jwt"
	if err := ValidateIAM(cfg.IAM); err != nil {
		t.Fatalf("expected nil; got %v", err)
	}
}
```

Add `"fmt"` to the imports.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./app/ -run 'TestValidateIAM_' `
Expected: FAIL — `TestValidateIAM_RejectsUnknownMode` gets nil for every mode.

- [ ] **Step 3: Implement** — in `ValidateIAM`, directly after the `RequireJWT` check:

```go
	// Only these two modes exist. app.New wires JWT auth for "jwt" and mock
	// auth for anything else, so an unrecognised value (a typo, a different
	// case) would otherwise run every request as the mock admin.
	if iam.Mode != "mock" && iam.Mode != "jwt" {
		return fmt.Errorf("CYODA_IAM_MODE=%q is not supported (expected \"mock\" or \"jwt\")", iam.Mode)
	}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./app/ ./cmd/cyoda/`
Expected: PASS (the `cmd/cyoda` package is included because `main.go:88` is the only caller).

- [ ] **Step 5: Docs**
  - `config/auth.md:26`: "`CYODA_IAM_MODE` — authentication mode: `mock` or `jwt` (default: `mock`). Any other value, including a different case, fails startup."
  - `CHANGELOG.md` `### Breaking`, new bullet: "**`CYODA_IAM_MODE` must be `mock` or `jwt`.** Any other value used to start the server with mock authentication, so every request ran as the mock admin, and without the mock-mode warning. The server now refuses to start."

- [ ] **Step 6: Commit**

```bash
git add app/config.go app/config_require_jwt_test.go cmd/cyoda/help/content/config/auth.md CHANGELOG.md
git commit -m "fix(config): refuse a CYODA_IAM_MODE other than mock or jwt"
```

---

### Task 2: `auth.OperatorGuard` and `auth.PlatformTenantID`

**Files:**
- Create: `internal/auth/operator_guard.go`
- Test: `internal/auth/operator_guard_test.go`

**Interfaces:**
- Consumes: `spi.UserContext`, `spi.HasRole`, `common.WriteError`, `common.Operational`, `common.ErrCodeUnauthorized`, `common.ErrCodeForbidden`.
- Produces:
  - `const PlatformTenantID spi.TenantID = "PLATFORM"`
  - `type OperatorGuard struct{ /* unexported */ }` — zero value = JWT rule
  - `func MockOperatorGuard() OperatorGuard`
  - `func (g OperatorGuard) Require(w http.ResponseWriter, r *http.Request) bool`

- [ ] **Step 1: Write the failing test** — `internal/auth/operator_guard_test.go`

```go
package auth_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

func guardRequest(uc *spi.UserContext) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/x", nil)
	if uc != nil {
		r = r.WithContext(spi.WithUserContext(r.Context(), uc))
	}
	return r
}

func guardUC(tenant string, roles ...string) *spi.UserContext {
	return &spi.UserContext{UserID: "u", Tenant: spi.Tenant{ID: spi.TenantID(tenant)}, Roles: roles}
}

// guardErrorCode decodes an RFC 9457 body and returns its errorCode property
// (the same shape internal/auth/admin_authz_test.go decodes).
func guardErrorCode(t *testing.T, body []byte) string {
	t.Helper()
	var pd common.ProblemDetail
	if err := json.Unmarshal(body, &pd); err != nil {
		t.Fatalf("decode problem detail %s: %v", body, err)
	}
	code, _ := pd.Props["errorCode"].(string)
	return code
}

func TestOperatorGuard(t *testing.T) {
	cases := []struct {
		name     string
		guard    auth.OperatorGuard
		uc       *spi.UserContext
		wantOK   bool
		wantCode int
		wantErr  string
	}{
		{"jwt: no user context", auth.OperatorGuard{}, nil, false, 401, "UNAUTHORIZED"},
		{"jwt: tenant admin", auth.OperatorGuard{}, guardUC("acme", "ROLE_ADMIN"), false, 403, "FORBIDDEN"},
		{"jwt: PLATFORM admin", auth.OperatorGuard{}, guardUC("PLATFORM", "ROLE_ADMIN"), true, 0, ""},
		{"jwt: PLATFORM admin among other roles", auth.OperatorGuard{}, guardUC("PLATFORM", "ROLE_M2M", "ROLE_ADMIN"), true, 0, ""},
		{"jwt: PLATFORM without ROLE_ADMIN", auth.OperatorGuard{}, guardUC("PLATFORM", "ROLE_M2M"), false, 403, "FORBIDDEN"},
		{"jwt: lower-case platform admin", auth.OperatorGuard{}, guardUC("platform", "ROLE_ADMIN"), false, 403, "FORBIDDEN"},
		{"jwt: SYSTEM admin", auth.OperatorGuard{}, guardUC("SYSTEM", "ROLE_ADMIN"), false, 403, "FORBIDDEN"},
		{"mock: no user context", auth.MockOperatorGuard(), nil, false, 401, "UNAUTHORIZED"},
		{"mock: admin in any tenant", auth.MockOperatorGuard(), guardUC("mock-tenant", "ROLE_ADMIN"), true, 0, ""},
		{"mock: no ROLE_ADMIN", auth.MockOperatorGuard(), guardUC("mock-tenant", "ROLE_M2M"), false, 403, "FORBIDDEN"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			ok := tc.guard.Require(w, guardRequest(tc.uc))
			if ok != tc.wantOK {
				t.Fatalf("Require = %v, want %v (status %d, body %s)", ok, tc.wantOK, w.Code, w.Body.String())
			}
			if tc.wantOK {
				if w.Body.Len() != 0 {
					t.Errorf("an allowed caller must get no response written, got %d %s", w.Code, w.Body.String())
				}
				return
			}
			if w.Code != tc.wantCode {
				t.Errorf("status = %d, want %d", w.Code, tc.wantCode)
			}
			if got := guardErrorCode(t, w.Body.Bytes()); got != tc.wantErr {
				t.Errorf("errorCode = %q, want %q", got, tc.wantErr)
			}
		})
	}
}

func TestPlatformTenantID(t *testing.T) {
	if auth.PlatformTenantID != "PLATFORM" {
		t.Fatalf("PlatformTenantID = %q, want PLATFORM", auth.PlatformTenantID)
	}
}
```


- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/auth/ -run 'TestOperatorGuard|TestPlatformTenantID'`
Expected: FAIL to compile — `auth.OperatorGuard`, `auth.MockOperatorGuard`, `auth.PlatformTenantID` undefined.

- [ ] **Step 3: Implement** — `internal/auth/operator_guard.go`

```go
package auth

import (
	"net/http"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// PlatformTenantID is the tenant whose admins are platform operators.
//
// A platform operator manages state every tenant shares: the signing key
// pairs, the OIDC reload of every tenant's providers, a node's log level and
// trace sampler. The rule is safe because no token source lets a tenant
// admin act in another tenant: OIDC tokens take the provider owner's tenant,
// M2M and trusted-key tokens take the client's tenant, and only the holder of
// the signing key (`cyoda token`) can name any tenant. So only the operator
// can reach PLATFORM, and PLATFORM admins can only create principals in
// PLATFORM.
const PlatformTenantID spi.TenantID = "PLATFORM"

// OperatorGuard gates the platform-wide admin endpoints. The zero value
// applies the JWT rule: ROLE_ADMIN in PlatformTenantID. Build it once at
// wiring time and share it.
type OperatorGuard struct {
	// anyTenant drops the tenant condition. Only MockOperatorGuard sets it.
	anyTenant bool
}

// MockOperatorGuard is the guard for mock IAM mode, where one fixed principal
// serves every request: ROLE_ADMIN in any tenant passes.
func MockOperatorGuard() OperatorGuard { return OperatorGuard{anyTenant: true} }

// Require reports whether the caller is a platform operator. Otherwise it
// writes 401 UNAUTHORIZED (no UserContext: the auth middleware was bypassed)
// or 403 FORBIDDEN, and returns false.
func (g OperatorGuard) Require(w http.ResponseWriter, r *http.Request) bool {
	uc := spi.GetUserContext(r.Context())
	if uc == nil {
		common.WriteError(w, r, common.Operational(
			http.StatusUnauthorized, common.ErrCodeUnauthorized, "authentication failed"))
		return false
	}
	if !spi.HasRole(uc.Roles, "ROLE_ADMIN") || (!g.anyTenant && uc.Tenant.ID != PlatformTenantID) {
		common.WriteError(w, r, common.Operational(
			http.StatusForbidden, common.ErrCodeForbidden, "platform operator required"))
		return false
	}
	return true
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/auth/ -run 'TestOperatorGuard|TestPlatformTenantID'`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/auth/operator_guard.go internal/auth/operator_guard_test.go
git commit -m "feat(auth): OperatorGuard — ROLE_ADMIN in the PLATFORM tenant"
```

---

### Task 3: Parity fixtures mint a platform operator; parity callers use it

Everything here still passes against today's `RequireAdmin` (a `PLATFORM` admin is an admin), so this task is green on its own. It is the precondition for Task 5.

**Files:**
- Modify: `e2e/parity/fixtureutil/fixtureutil.go` (new `MintPlatformOperatorJWT`)
- Modify: `e2e/parity/fixture.go` (`BackendFixture.PlatformOperator`; fix the `Tenant.ID` doc)
- Modify: `e2e/parity/multinode/fixture.go` (`MultiNodeFixture.PlatformOperator`)
- Modify: `e2e/parity/memory/fixture.go`, `e2e/parity/sqlite/fixture.go`, `e2e/parity/postgres/fixture.go`, `e2e/parity/postgres/multinode_fixture.go`
- Modify test stubs: `e2e/parity/compute_client_test.go:11-12`, `e2e/parity/multinode/attribution_skip_test.go:17-18`, `e2e/parity/multinode/compute_client_skip_test.go:14-15`
- Modify callers: `e2e/parity/signing_keys.go`, `e2e/parity/multinode/signing_keys.go`, `e2e/parity/oidc.go`, `e2e/parity/postgres/signing_keys_cluster_test.go`
- Test: `e2e/parity/fixtureutil/platform_operator_test.go` (new)

**Interfaces:**
- Consumes: `auth.PlatformTenantID` (Task 2), `auth.Sign`, `auth.NewRSASigner`, `fixtureutil.JWTKeySet`.
- Produces:
  - `func MintPlatformOperatorJWT(t *testing.T, ks *JWTKeySet) parity.Tenant` — `ID == "PLATFORM"`, token with `user_roles: ["ROLE_ADMIN"]`.
  - `BackendFixture.PlatformOperator(t *testing.T) Tenant` and `MultiNodeFixture.PlatformOperator(t *testing.T) parity.Tenant` (both required).

- [ ] **Step 1: Write the failing test** — `e2e/parity/fixtureutil/platform_operator_test.go`

```go
package fixtureutil

import (
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/auth"
)

func TestMintPlatformOperatorJWT_IsAPlatformAdminPersonToken(t *testing.T) {
	ks, err := GenerateJWTKeySet()
	if err != nil {
		t.Fatal(err)
	}
	op := MintPlatformOperatorJWT(t, ks)
	if op.ID != string(auth.PlatformTenantID) {
		t.Fatalf("ID = %q, want %q", op.ID, auth.PlatformTenantID)
	}
	parsed, err := auth.Parse(op.Token)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := auth.Verify(parsed.SigningInput, parsed.Signature, &ks.Key.PublicKey); err != nil {
		t.Fatalf("signature: %v", err)
	}
	if got := parsed.Header["kid"]; got != ks.Kid {
		t.Errorf("kid = %v, want %s", got, ks.Kid)
	}
	if got := parsed.Claims["caas_org_id"]; got != "PLATFORM" {
		t.Errorf("caas_org_id = %v, want PLATFORM", got)
	}
	roles, _ := parsed.Claims["user_roles"].([]any)
	if len(roles) != 1 || roles[0] != "ROLE_ADMIN" {
		t.Errorf("user_roles = %v, want [ROLE_ADMIN]", parsed.Claims["user_roles"])
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./e2e/parity/fixtureutil/ -run TestMintPlatformOperatorJWT`
Expected: FAIL to compile — `MintPlatformOperatorJWT` undefined.

- [ ] **Step 3: Implement the helper** (in `fixtureutil.go`, after `MintTenantJWT`)

```go
// MintPlatformOperatorJWT mints a platform-operator token: ROLE_ADMIN in the
// PLATFORM tenant, in the shape `cyoda token --tenant PLATFORM` signs (a
// person token, roles in user_roles). PLATFORM is one shared tenant: use the
// token only on the platform-wide admin endpoints, never for tenant data.
func MintPlatformOperatorJWT(t *testing.T, ks *JWTKeySet) parity.Tenant {
	t.Helper()
	now := time.Now()
	claims := map[string]any{
		"sub":          "platform-operator",
		"iss":          ks.Issuer,
		"caas_user_id": "platform-operator",
		"caas_org_id":  string(auth.PlatformTenantID),
		"user_roles":   []string{"ROLE_ADMIN"},
		"caas_tier":    "unlimited",
		"exp":          now.Add(1 * time.Hour).Unix(),
		"iat":          now.Unix(),
		"jti":          uuid.NewString(),
	}
	token, err := auth.Sign(context.Background(), claims, auth.NewRSASigner(ks.Key), ks.Kid)
	if err != nil {
		t.Fatalf("failed to mint platform operator JWT: %v", err)
	}
	return parity.Tenant{ID: string(auth.PlatformTenantID), Token: token}
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./e2e/parity/fixtureutil/ -run TestMintPlatformOperatorJWT`
Expected: PASS.

- [ ] **Step 5: Add the fixture methods**
  - `e2e/parity/fixture.go`, in `BackendFixture` after `ComputeTenant`:

```go
	// PlatformOperator returns a platform-operator token: ROLE_ADMIN in the
	// PLATFORM tenant. The platform-wide admin endpoints (signing key pairs,
	// OIDC reload) accept only this principal. PLATFORM is one shared tenant,
	// unlike NewTenant: use it only on those endpoints, never for tenant data.
	// Implementations MUST call t.Helper() and t.Fatal on failure.
	PlatformOperator(t *testing.T) Tenant
```

  - Fix the `Tenant.ID` doc comment: the id is the tenant id the token carries in `caas_org_id` — a UUID for `NewTenant`, `PLATFORM` for `PlatformOperator`.
  - `e2e/parity/multinode/fixture.go`, in `MultiNodeFixture` after `ComputeTenant`: the same method, returning `parity.Tenant`, doc "valid against every node".
  - Each in-tree fixture (memory, sqlite, postgres, `pgMultiNode`):

```go
// PlatformOperator implements parity.BackendFixture.
func (f *memoryFixture) PlatformOperator(t *testing.T) parity.Tenant {
	t.Helper()
	return fixtureutil.MintPlatformOperatorJWT(t, f.keySet)
}
```

  (receiver and doc adjusted per file; `pgMultiNode`'s doc says `multinode.MultiNodeFixture`).
  - The three test stubs: `func (stubX) PlatformOperator(*testing.T) Tenant { return Tenant{ID: "PLATFORM"} }` (with `parity.` prefix in the `multinode` package).

- [ ] **Step 6: Build check**

Run: `go vet ./e2e/...`
Expected: no errors (proves every fixture and stub satisfies the widened interfaces).

- [ ] **Step 7: Switch the parity callers** (each keeps its tenant token for tenant setup and uses the operator only for the operator call)
  - `signing_keys.go` `RunSigningKeyPairLifecycle`: `c := client.NewClient(fixture.BaseURL(), fixture.PlatformOperator(t).Token)`; drop the now-unused `tenant`.
  - `multinode/signing_keys.go` `RunSigningKeyPairFollowsTheCluster`: add `op := client.NewClient(urls[0], fixture.PlatformOperator(t).Token)`. `IssueClientKeyPair(t, op, false)`, `op.DeleteKeyPairOnCleanup`, `op.InvalidateKeyPairRaw`, `op.ReactivateKeyPairRaw`, `op.DeleteKeyPairRaw`. `a` (tenant) stays for `newM2MClient(t, a)`; `b` stays for `JWKSKIDs` and `newM2MClient(t, b)`.
  - `oidc.go`, the four reload callers — `RunOidcD18_ReloadInvalidateSerializeLocally` (call near `:1839`), `RunOidcD18_ReloadAllSerializesWithReloadOne` (`:1910`), `RunOidcReload_PreservesTokenAcceptance` (`:3192`), `RunOidcReload_AfterReactivateKeepsTokenAcceptance` (`:3235`): add `opC := client.NewClient(fix.BaseURL(), fix.PlatformOperator(t).Token)` and call `opC.ReloadOidcProviders…` where the scenario called `adminC.ReloadOidcProviders…`. `RunOidcNonAdminReload` is unchanged.
  - `oidc.go` `RunOidcD23_PerProviderRolesClaim` (`:2316-2350`): keep the provider value from `RegisterOidcProvider` (`p, err := adminC.RegisterOidcProvider(...)`); after minting `tokenWithCustomRoles`, warm with `probeC.ProbeAuthRaw(t)` (expect 200), then replace the reload probe with `if _, err := probeC.UpdateOidcProvider(t, p.ID, map[string]any{}); err != nil { t.Errorf("rolesClaim override 'cognito:groups' not respected: UpdateOidcProvider: %v", err) }`. Update the doc comment: the probe is a no-op `PATCH`, which requires `ROLE_ADMIN` in the provider's tenant.
  - `postgres/signing_keys_cluster_test.go` `TestSigningKeys_OwnCluster`: add `op := fix.PlatformOperator(t)` and `opA := client.NewClient(urls[0], op.Token)`. Every key-pair call and `createAdminClient` use `opA` (so the admin M2M client lives in `PLATFORM`); `waitStatus` probes with `tenant.Token` stay. `bootKID` still comes from the tenant token (both are bootstrap-signed). Update the header comment: the admin client is a platform operator, the route an operator keeps after the bootstrap key is gone.

- [ ] **Step 8: Exit check** — no tenant-admin client calls an operator endpoint in parity:

Run: `grep -rn 'IssueKeyPairRaw\|CurrentKeyPairRaw\|InvalidateKeyPair\|ReactivateKeyPairRaw\|DeleteKeyPairRaw\|ReloadOidcProviders\|IssueClientKeyPair' e2e/parity --include='*.go' | grep -v '/client/'`
Expected: every hit's receiver is an operator client (`c` in `RunSigningKeyPairLifecycle`, `op`, `opA`, `opC`), except `RunOidcNonAdminReload` (non-admin, asserts 403).

- [ ] **Step 9: Run**

Run: `make test` (includes the parity suites on memory/sqlite/postgres).
Expected: PASS. Then run the multi-node scenario and the own-cluster test: `go test ./e2e/parity/postgres/ -run 'TestMultiNode/SigningKeyPairFollowsTheCluster|TestSigningKeys_OwnCluster'` — PASS.

- [ ] **Step 10: Commit**

```bash
git add e2e/parity
git commit -m "test(parity): fixtures mint a PLATFORM operator; operator endpoints use it"
```

---

### Task 4: `internal/e2e` helpers and callers use a platform operator

Green against today's `RequireAdmin`, like Task 3.

**Files:**
- Modify: `internal/e2e/helpers_test.go` (new `platformTokenRaw`, `platformToken`)
- Modify: `internal/e2e/oauth_keys_test.go` (new `operatorRequest`; key-pair tests use it)
- Modify: `internal/e2e/keys_trusted_reconciliation_test.go`, `internal/e2e/signing_keys_test.go`, `internal/e2e/cyoda_token_test.go`, `internal/e2e/oidc_providers_test.go`, `internal/e2e/admin_loglevel_test.go`, plus any file the exit grep in Step 3 shows (`auth_failures_test.go`, `cors_e2e_test.go` are candidates)
- Modify: `internal/e2e/callback_harness_test.go` (new `(*callbackHarness).platformToken`)

**Interfaces:**
- Consumes: `auth.PlatformTenantID`, `signServiceToken`, `e2eSignKey`, `e2eIssuer`, `serverURL`, `e2eNewRequest`.
- Produces (test-only, package `e2e_test`):
  - `func platformTokenRaw(roles ...string) (string, error)` — shared server, tenant `PLATFORM`, `user_roles`-free service shape like `suiteTokenRaw`
  - `func platformToken(t *testing.T) string` — `ROLE_ADMIN, ROLE_M2M` in `PLATFORM`
  - `func operatorRequest(t *testing.T, method, path string, body []byte) *http.Response` — `adminRequest` with `platformToken`
  - `func (h *callbackHarness) platformToken(t *testing.T) string` — same claims signed with `h.signKey`, issuer `cyoda-callback-test`, `h.audience`

- [ ] **Step 1: Add the helpers**

`helpers_test.go`, after `suiteToken`:

```go
// platformTokenRaw signs a token for the shared server in the PLATFORM tenant
// with roles. With ROLE_ADMIN it is a platform operator, the only principal
// the platform-wide admin endpoints accept. It never touches *testing.T.
func platformTokenRaw(roles ...string) (string, error) {
	return signServiceToken(e2eSignKey, e2eIssuer, "", "platform-operator",
		string(auth.PlatformTenantID), "platform-operator", roles)
}

// platformToken is a platform-operator token for the shared server.
func platformToken(t *testing.T) string {
	t.Helper()
	tok, err := platformTokenRaw("ROLE_ADMIN", "ROLE_M2M")
	if err != nil {
		t.Fatalf("sign platform token: %v", err)
	}
	return tok
}
```

`oauth_keys_test.go`: refactor `adminRequest` into `requestAs(t, token, method, path, body)` and define `adminRequest` (suite token) and `operatorRequest` (platform token) on top of it:

```go
// requestAs issues a request to serverURL+"/api"+path with token as bearer.
func requestAs(t *testing.T, token, method, path string, body []byte) *http.Response {
	t.Helper()
	var br io.Reader
	if body != nil {
		br = bytes.NewReader(body)
	}
	req, err := e2eNewRequest(t, method, serverURL+"/api"+path, br)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do %s %s: %v", method, path, err)
	}
	return resp
}

// adminRequest issues a request as the suite's tenant admin.
func adminRequest(t *testing.T, method, path string, body []byte) *http.Response {
	t.Helper()
	return requestAs(t, suiteToken(t), method, path, body)
}

// operatorRequest issues a request as a platform operator: the platform-wide
// admin endpoints (key pairs, OIDC reload, /admin/*) accept only this.
func operatorRequest(t *testing.T, method, path string, body []byte) *http.Response {
	t.Helper()
	return requestAs(t, platformToken(t), method, path, body)
}
```

`callback_harness_test.go`, after `fetchToken`:

```go
// platformToken signs a platform-operator token (ROLE_ADMIN, ROLE_M2M in the
// PLATFORM tenant) for this stack with h.signKey.
func (h *callbackHarness) platformToken(t *testing.T) string {
	t.Helper()
	tok, err := signServiceToken(h.signKey, "cyoda-callback-test", h.audience, "platform-operator",
		string(auth.PlatformTenantID), "platform-operator", []string{"ROLE_ADMIN", "ROLE_M2M"})
	if err != nil {
		t.Fatalf("sign platform token: %v", err)
	}
	return tok
}
```

- [ ] **Step 2: Switch the callers**
  - `oauth_keys_test.go`: every key-pair test (`/oauth/keys/keypair…` paths) uses `operatorRequest`; tests on trusted keys, clients and data endpoints keep `adminRequest`/`suiteToken`. `TestE2E_KeyPairIssuedAheadDoesNotSignYet` and the two `..._400` tests that call `unauthRequest(... suiteToken(t))` on `/api/model/` keep the suite token for that data probe.
  - `keys_trusted_reconciliation_test.go`: key-pair calls use `operatorRequest`.
  - `signing_keys_test.go`: `createKeyStackClient` posts `/api/clients?withAdminRole=true` with `h.platformToken(t)` as bearer (use `doAuthAgainst(t, h.baseURL, h.platformToken(t), http.MethodPost, "/api/clients?withAdminRole=true", "")` in place of `h.DoAuth`), so the key stack's admin M2M client lives in `PLATFORM`; `bootstrapToken` (`:187`) signs with tenant `string(auth.PlatformTenantID)`. Fix both doc comments.
  - `cyoda_token_test.go`: `operatorToken` (`:28`) mints with `Tenant: auth.PlatformTenantID` — it models `cyoda token --tenant PLATFORM`. If a test in the file asserts tenant `test-tenant` on data it wrote, give that test its own token with `test-tenant` rather than weakening the assertion.
  - `oidc_providers_test.go:119`: the reload call uses a platform-operator client/token.
  - `admin_loglevel_test.go`: the log-level requests use `doAuthAgainst(t, serverURL, platformToken(t), …)` in place of `doAuth`.

- [ ] **Step 3: Exit check** — every operator-endpoint call in `internal/e2e` carries a platform token:

Run: `grep -rn 'keys/keypair\|oidc/providers/reload\|admin/log-level\|admin/trace-sampler' internal/e2e --include='*_test.go'`
Expected: each hit is inside `operatorRequest`, a `platformToken`/`h.platformToken` call, `bootstrapToken`/`operatorToken` (now `PLATFORM`), a key-stack `keyCall` (its M2M client is now in `PLATFORM`), or a test that deliberately asserts 401/403/501 (`auth_failures_test.go`, `iam_gated_501_test.go`, `cors_e2e_test.go`). Record any other hit and switch it.

- [ ] **Step 4: Run**

Run: `go test ./internal/e2e/...` (Docker required; no `-v`).
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/e2e
git commit -m "test(e2e): platform-operator helpers; operator endpoints use them"
```

---

### Task 5: Key-pair endpoints and OIDC reload require a platform operator

**Files:**
- Modify: `internal/domain/account/handler.go` (`Handler.operator`; `New` signature)
- Modify: `internal/domain/account/keys_adapter.go` (five handlers)
- Modify: `internal/domain/account/oidc_adapter.go` (`NewOidcAdapter`/`newOidcAdapter` signature; `ReloadOidcProviders`; comment at `:433`)
- Modify: `internal/auth/admin_guard.go:10-11` (doc comment no longer names the key-pair handler)
- Modify: `app/app.go` (build the guard early in `New`; pass it at `:324` and `:582`)
- Modify every `account.New(` / `account.NewOidcAdapter(` / `newOidcAdapter(` call site: `internal/domain/account/*_test.go`, `internal/auth/keypair_signing_test.go` (find with `grep -rn 'account.New(\|NewOidcAdapter(\|newOidcAdapter(\| New(nil' --include='*.go' .`)
- Test: `internal/domain/account/operator_endpoints_test.go` (new), `internal/e2e/platform_operator_test.go` (new), `e2e/parity/platform_operator.go` (new), `e2e/parity/registry.go`, `e2e/parity/registry_count_test.go`

**Interfaces:**
- Consumes: `auth.OperatorGuard`, `auth.MockOperatorGuard`, `auth.PlatformTenantID` (Task 2); `BackendFixture.PlatformOperator` (Task 3); `operatorRequest`, `platformTokenRaw`, `requestAs` (Task 4).
- Produces:
  - `func account.New(authSvc contract.AuthenticationService, authzSvc contract.AuthorizationService, keyStore auth.KeyStore, trustedKeyStore auth.TrustedKeyStore, m2mClientStore auth.M2MClientStore, iam auth.IAMFeatures, operator auth.OperatorGuard) *Handler`
  - `func account.NewOidcAdapter(service *oidc.Service, defaultRolesClaim string, requireHTTPS, allowPrivate bool, operator auth.OperatorGuard) *OidcAdapter`
  - `func parity.RunPlatformOperatorGate(t *testing.T, fixture BackendFixture)` registered as `"PlatformOperatorGate"`.

- [ ] **Step 1: Write the failing unit tests** — `internal/domain/account/operator_endpoints_test.go` (package `account_test`)

```go
package account_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/cyoda-platform/cyoda-go/internal/domain/account"
)

func ucReq(method, path string, body []byte, uc *spi.UserContext) *http.Request {
	var r *http.Request
	if body != nil {
		r = httptest.NewRequest(method, path, bytes.NewReader(body))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	return r.WithContext(spi.WithUserContext(r.Context(), uc))
}

func operatorUC() *spi.UserContext {
	return &spi.UserContext{UserID: "op", Tenant: spi.Tenant{ID: auth.PlatformTenantID}, Roles: []string{"ROLE_ADMIN"}}
}

// keyPairCalls invokes each key-pair handler as uc. The kid is well formed;
// the guard must refuse before it is looked up.
func keyPairCalls(h *account.Handler, uc *spi.UserContext) map[string]func() *httptest.ResponseRecorder {
	const kid = "0123456789abcdef0123456789abcdef"
	run := func(f func(w http.ResponseWriter)) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		f(w)
		return w
	}
	validTo := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	return map[string]func() *httptest.ResponseRecorder{
		"issue": func() *httptest.ResponseRecorder {
			return run(func(w http.ResponseWriter) {
				h.IssueJwtKeyPair(w, ucReq("POST", "/oauth/keys/keypair", []byte(`{"algorithm":"RS256","audience":"human"}`), uc))
			})
		},
		"current": func() *httptest.ResponseRecorder {
			return run(func(w http.ResponseWriter) {
				h.GetCurrentJwtKeyPair(w, ucReq("GET", "/oauth/keys/keypair/current?audience=human", nil, uc),
					genapiCurrentParams("human"))
			})
		},
		"invalidate": func() *httptest.ResponseRecorder {
			return run(func(w http.ResponseWriter) { h.InvalidateJwtKeyPair(w, ucReq("POST", "/x", nil, uc), kid) })
		},
		"reactivate": func() *httptest.ResponseRecorder {
			return run(func(w http.ResponseWriter) {
				h.ReactivateJwtKeyPair(w, ucReq("POST", "/x", []byte(`{"validTo":"`+validTo+`"}`), uc), kid)
			})
		},
		"delete": func() *httptest.ResponseRecorder {
			return run(func(w http.ResponseWriter) { h.DeleteJwtKeyPair(w, ucReq("DELETE", "/x", nil, uc), kid) })
		},
	}
}

func TestKeyPairEndpoints_RefuseTenantAdmin(t *testing.T) {
	ks := newTestKeyStore(t)
	h := account.New(nil, nil, ks, newTestTrustedStore(t), nil, auth.DefaultIAMFeatures(), auth.OperatorGuard{})
	for name, call := range keyPairCalls(h, adminUC()) { // adminUC: ROLE_ADMIN in tenant t1
		t.Run(name, func(t *testing.T) {
			w := call()
			if w.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403; body %s", w.Code, w.Body.String())
			}
		})
	}
	// Nothing was issued: the only key is the bootstrap key.
	if _, err := ks.Current("human"); err == nil {
		t.Error("a tenant admin's refused issue left a human key pair behind")
	}
}

func TestKeyPairEndpoints_AdmitPlatformOperator(t *testing.T) {
	h := account.New(nil, nil, newTestKeyStore(t), newTestTrustedStore(t), nil, auth.DefaultIAMFeatures(), auth.OperatorGuard{})
	w := keyPairCalls(h, operatorUC())["issue"]()
	if w.Code != http.StatusOK {
		t.Fatalf("issue as operator: status = %d, body %s", w.Code, w.Body.String())
	}
}

func TestKeyPairEndpoints_MockGuard_Answer501(t *testing.T) {
	mockUC := &spi.UserContext{UserID: "mock-user-001", Tenant: spi.Tenant{ID: "mock-tenant"}, Roles: []string{"ROLE_ADMIN", "ROLE_M2M"}}
	h := account.New(nil, nil, nil, nil, nil, auth.DefaultIAMFeatures(), auth.MockOperatorGuard())
	for name, call := range keyPairCalls(h, mockUC) {
		t.Run(name, func(t *testing.T) {
			if w := call(); w.Code != http.StatusNotImplemented {
				t.Fatalf("status = %d, want 501; body %s", w.Code, w.Body.String())
			}
		})
	}
}
```

Add to the same file (and import `genapi "github.com/cyoda-platform/cyoda-go/api"`):

```go
func genapiCurrentParams(aud string) genapi.GetCurrentJwtKeyPairParams {
	return genapi.GetCurrentJwtKeyPairParams{Audience: genapi.GetCurrentJwtKeyPairParamsAudience(aud)}
}
```

Reload (in `oidc_adapter_test.go`, package `account`, next to the existing reload tests at `:696-760`):

```go
func TestReloadOidcProviders_TenantAdmin_Returns403(t *testing.T) {
	h := newOidcAdapterFixture(t)
	req := withOidcTenantAdminCtx(httptest.NewRequest(http.MethodPost, "/oauth/oidc/providers/reload", nil))
	rr := httptest.NewRecorder()
	h.ReloadOidcProviders(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d want 403, body=%s", rr.Code, rr.Body.String())
	}
}
```

and rename `TestReloadOidcProviders_AdminHappyPath_Returns200` to `TestReloadOidcProviders_PlatformOperator_Returns200`, building its request with a `PLATFORM` `ROLE_ADMIN` context (add `withPlatformOperatorCtx` beside `withOidcTenantAdminCtx`).

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/domain/account/`
Expected: FAIL to compile (`account.New` takes 6 arguments).

- [ ] **Step 3: Implement**
  - `handler.go`: add `operator auth.OperatorGuard` to `Handler`; `New(..., iam auth.IAMFeatures, operator auth.OperatorGuard)` stores it.
  - `keys_adapter.go`: in each of the five handlers replace `if !auth.RequireAdmin(w, r) { return }` with `if !h.operator.Require(w, r) { return }`.
  - `oidc_adapter.go`: `oidcAdapter` gets `operator auth.OperatorGuard`; `NewOidcAdapter(..., allowPrivate bool, operator auth.OperatorGuard)` and `newOidcAdapter` pass it through; `ReloadOidcProviders` calls `a.operator.Require(w, r)`; replace the comment at `:433` with "ReloadOidcProviders implements POST /oauth/oidc/providers/reload. It reloads every tenant's providers on every node, so it requires a platform operator." Do NOT add a guard to `Handler.ReloadOidcProviders` in `handler.go`.
  - `admin_guard.go`: the `RequireAdmin` doc names "the tenant-scoped admin endpoints: trusted keys, M2M clients, OIDC provider changes"; key pairs are gone from it.
  - `app/app.go`: at the top of the IAM wiring (before `:256`):

```go
	// The platform-wide admin endpoints accept only a platform operator. Mock
	// mode has one fixed principal, so there the operator check is the admin
	// check. ValidateIAM admits only "mock" and "jwt".
	operatorGuard := auth.OperatorGuard{}
	if cfg.IAM.Mode == "mock" {
		operatorGuard = auth.MockOperatorGuard()
	}
```

  pass `operatorGuard` as the last argument at `account.NewOidcAdapter(` (`:324`) and `account.New(` (`:582`). Keep the variable in scope for Task 6.
  - Update every other call site with `auth.OperatorGuard{}` (JWT rule). Key-pair tests in `keys_adapter_test.go`, `keys_adapter_errors_test.go`, `grace_field_name_test.go` and `internal/auth/keypair_signing_test.go` that exercise a key-pair handler as `adminUC()`/tenant `t`/`t1` switch to an operator context (`operatorUC()`, or `adminReq` variants built on it — add `operatorReq(t, method, path, body)` beside `adminReq`). Tests of trusted keys and M2M clients keep `adminUC()`.

- [ ] **Step 4: Run unit tests**

Run: `go test ./internal/domain/account/ ./internal/auth/ ./app/`
Expected: PASS.

- [ ] **Step 5: Write the E2E tests** — `internal/e2e/platform_operator_test.go`

```go
package e2e_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// operatorEndpoints are the platform-wide endpoints behind the operator
// guard. kid is well formed and never exists: every refusal here happens
// before a lookup. The GET needs ?audience= (the generated wrapper answers
// 400 for a missing one before the handler runs).
func operatorEndpoints() []struct{ name, method, path, body string } {
	const kid = "0123456789abcdef0123456789abcdef"
	validTo := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	return []struct{ name, method, path, body string }{
		{"issue", http.MethodPost, "/oauth/keys/keypair", `{"algorithm":"RS256","audience":"human"}`},
		{"current", http.MethodGet, "/oauth/keys/keypair/current?audience=human", ""},
		{"invalidate", http.MethodPost, "/oauth/keys/keypair/" + kid + "/invalidate", ""},
		{"reactivate", http.MethodPost, "/oauth/keys/keypair/" + kid + "/reactivate", `{"validTo":"` + validTo + `"}`},
		{"delete", http.MethodDelete, "/oauth/keys/keypair/" + kid, ""},
		{"reload", http.MethodPost, "/oauth/oidc/providers/reload", ""},
	}
}

func assertProblem(t *testing.T, resp *http.Response, wantStatus int, wantCode string) {
	t.Helper()
	body := readBody(t, resp)
	if resp.StatusCode != wantStatus {
		t.Fatalf("status = %d, want %d; body %s", resp.StatusCode, wantStatus, body)
	}
	var pd struct {
		Properties struct {
			ErrorCode string `json:"errorCode"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(body), &pd); err != nil || pd.Properties.ErrorCode != wantCode {
		t.Fatalf("errorCode = %q (decode err %v), want %q; body %s", pd.Properties.ErrorCode, err, wantCode, body)
	}
}

func bodyBytes(s string) []byte {
	if s == "" {
		return nil
	}
	return []byte(s)
}

func TestPlatformOperator_NoToken_401(t *testing.T) {
	for _, ep := range operatorEndpoints() {
		t.Run(ep.name, func(t *testing.T) {
			assertProblem(t, unauthRequest(t, ep.method, "/api"+ep.path, ""), http.StatusUnauthorized, "UNAUTHORIZED")
		})
	}
}

func TestPlatformOperator_TenantAdmin_403(t *testing.T) {
	for _, ep := range operatorEndpoints() {
		t.Run(ep.name, func(t *testing.T) {
			resp := requestAs(t, suiteToken(t), ep.method, ep.path, bodyBytes(ep.body))
			assertProblem(t, resp, http.StatusForbidden, "FORBIDDEN")
		})
	}
}

func TestPlatformOperator_PlatformWithoutAdmin_403(t *testing.T) {
	tok, err := platformTokenRaw("ROLE_M2M")
	if err != nil {
		t.Fatal(err)
	}
	for _, ep := range operatorEndpoints() {
		t.Run(ep.name, func(t *testing.T) {
			assertProblem(t, requestAs(t, tok, ep.method, ep.path, bodyBytes(ep.body)), http.StatusForbidden, "FORBIDDEN")
		})
	}
}

// TestPlatformOperator_AdminM2MClientInPlatform: an admin M2M client created
// in PLATFORM is a platform operator — the route an operator keeps once the
// bootstrap key is revoked.
func TestPlatformOperator_AdminM2MClientInPlatform(t *testing.T) {
	id, secret := createM2MClient(t, "PLATFORM", "platform-seed", true)
	tok := getToken(t, id, secret)
	resp := requestAs(t, tok, http.MethodPost, "/oauth/keys/keypair", []byte(`{"algorithm":"RS256","audience":"human"}`))
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("issue as a PLATFORM admin M2M client: %d %s", resp.StatusCode, body)
	}
	var kp struct {
		KeyID string `json:"keyId"`
	}
	if err := json.Unmarshal([]byte(body), &kp); err != nil || kp.KeyID == "" {
		t.Fatalf("no keyId: %s", body)
	}
	t.Cleanup(func() { operatorRequest(t, http.MethodDelete, "/oauth/keys/keypair/"+kp.KeyID, nil).Body.Close() })
}
```

The operator-success cell per key-pair endpoint is `oauth_keys_test.go`'s `TestE2E_*JwtKeyPair_Happy` tests (switched to `operatorRequest` in Task 4); reload's is `oidc_providers_test.go:119` (Task 4). The cleanup's `operatorRequest` is safe after the test ends: `e2eNewRequest` uses `e2eCtx`, which is built on `context.Background()`, not `t.Context()`.

- [ ] **Step 6: Write the parity scenario** — `e2e/parity/platform_operator.go`

```go
package parity

import (
	"net/http"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// RunPlatformOperatorGate: the platform-wide admin endpoints refuse a tenant
// admin with 403 FORBIDDEN — also when the tenant admin aims at the bootstrap
// key the fixture signs with, and when ROLE_ADMIN comes from the tenant's own
// OIDC provider with a PLATFORM tenant claim — and admit the platform
// operator. Operator success on each key-pair call is
// RunSigningKeyPairLifecycle.
func RunPlatformOperatorGate(t *testing.T, fixture BackendFixture) {
	base := fixture.BaseURL()
	tenant := fixture.NewTenant(t)
	tc := client.NewClient(base, tenant.Token)
	op := client.NewClient(base, fixture.PlatformOperator(t).Token)

	// The fixture's tokens are signed with the bootstrap key: aim at it.
	bootKID := client.TokenKID(tenant.Token)
	if bootKID == "" {
		t.Fatal("the fixture's tenant token carries no kid")
	}

	refused := func(t *testing.T, c *client.Client, who string) {
		t.Helper()
		calls := []struct {
			name string
			do   func() (int, []byte, error)
		}{
			{"issue", func() (int, []byte, error) {
				return c.IssueKeyPairRaw(t, map[string]any{"algorithm": "RS256", "audience": "human"})
			}},
			{"current", func() (int, []byte, error) { return c.CurrentKeyPairRaw(t, "human") }},
			{"invalidate", func() (int, []byte, error) { return c.InvalidateKeyPairRaw(t, bootKID) }},
			{"reactivate", func() (int, []byte, error) { return c.ReactivateKeyPairRaw(t, bootKID, time.Now().Add(time.Hour)) }},
			{"delete", func() (int, []byte, error) { return c.DeleteKeyPairRaw(t, bootKID) }},
			{"reload", func() (int, []byte, error) { return c.ReloadOidcProvidersRaw(t) }},
		}
		for _, call := range calls {
			code, body, err := call.do()
			if err != nil {
				t.Fatalf("%s %s: transport: %v", who, call.name, err)
			}
			if code != http.StatusForbidden || !containsErrorCode(body, "FORBIDDEN") {
				t.Fatalf("%s %s: %d %s, want 403 FORBIDDEN", who, call.name, code, body)
			}
		}
	}

	refused(t, tc, "tenant admin")

	// The bootstrap key survived: the tenant token still authenticates.
	if code, body, err := tc.ProbeAuthRaw(t); err != nil || code != http.StatusOK {
		t.Fatalf("bootstrap-signed token after the refused calls: %d %s %v", code, body, err)
	}

	if code, body, err := op.ReloadOidcProvidersRaw(t); err != nil || code != http.StatusOK {
		t.Fatalf("reload as operator: %d %s %v", code, body, err)
	}

	// ROLE_ADMIN from the tenant's own OIDC provider, with a PLATFORM claim.
	idp := NewParityFixtureIdP(t)
	p, err := tc.RegisterOidcProvider(t, map[string]any{"wellKnownConfigUri": idp.WellKnownURI()})
	if err != nil {
		t.Fatalf("RegisterOidcProvider: %v", err)
	}
	oidcTok := idp.MintJWTWithRolesClaim(t, idp.DefaultKid, tenant.ID, map[string]any{
		"roles":       []string{"ROLE_ADMIN"},
		"caas_org_id": "PLATFORM",
	})
	oc := client.NewClient(base, oidcTok)
	if code, body, err := oc.ProbeAuthRaw(t); err != nil || code != http.StatusOK {
		t.Fatalf("control: the OIDC token authenticates: %d %s %v", code, body, err)
	}
	// Control: its ROLE_ADMIN counts in its own tenant (a no-op PATCH).
	if _, err := oc.UpdateOidcProvider(t, p.ID, map[string]any{}); err != nil {
		t.Fatalf("control: the OIDC admin updates its own provider: %v", err)
	}
	refused(t, oc, "OIDC tenant admin")
}
```

Helpers used, all existing: `containsErrorCode(body []byte, code string) bool` (`e2e/parity/grouped_stats.go:841`); `(*client.Client).ProbeAuthRaw(t) (int, []byte, error)` (`client/oidc.go:170`); `(*client.Client).UpdateOidcProvider(t, id uuid.UUID, patch map[string]any)` (`client/oidc.go:61`; `p.ID` is a `uuid.UUID`); `(*ParityFixtureIdP).MintJWTWithRolesClaim(t, kid, tenantID string, extraClaims map[string]any)` (`oidc_fixture.go:340`).

Register it in `registry.go` directly after `{"SigningKeyPairLifecycle", RunSigningKeyPairLifecycle},`: `{"PlatformOperatorGate", RunPlatformOperatorGate},`. Bump `wantParityScenarioCount` in `registry_count_test.go` from 301 to 302 and the count in `registry.go`'s header comment.

- [ ] **Step 7: Prove RED on the E2E and parity tests** — temporarily revert only the five `keys_adapter.go` guard lines and the reload guard line to `auth.RequireAdmin` (keep everything else), run:

`go test ./internal/e2e/ -run 'TestPlatformOperator_'` and `go test ./e2e/parity/memory/ -run 'TestParity/PlatformOperatorGate'`.

Expected: `TenantAdmin_403` and `PlatformOperatorGate` FAIL (200/404/400 in place of 403). `PlatformWithoutAdmin_403` passes either way (the admin check refuses it) — that is expected. **Do not run the parity check against the shared suite with the guard reverted beyond this one scenario**: the refused `delete` would really delete the fixture's bootstrap key. Restore the guard lines.

- [ ] **Step 8: Run GREEN**

Run: `go test ./internal/e2e/ -run 'TestPlatformOperator_|TestE2E_.*KeyPair|TestGated_MockIAM'` then `make test`.
Expected: PASS (`TestGated_MockIAM_All21Return501` unchanged: mock guard passes the mock admin, then 501).

- [ ] **Step 9: Commit**

```bash
git add internal/domain/account internal/auth app/app.go internal/e2e e2e/parity
git commit -m "feat(auth)!: key-pair endpoints and OIDC reload require a platform operator"
```

---

### Task 6: `/admin/log-level` and `/admin/trace-sampler` require a platform operator

**Files:**
- Modify: `internal/api/admin.go` (handlers become methods of `AdminHandlers`)
- Modify: `internal/api/admin_test.go`
- Modify: `app/app.go:625-628`
- Test: `internal/e2e/platform_operator_test.go` (extend), `internal/e2e/admin_loglevel_test.go`

**Interfaces:**
- Consumes: `auth.OperatorGuard` (Task 2), `operatorGuard` variable in `app.New` (Task 5), `platformToken`, `platformTokenRaw`, `requestAs` (Task 4).
- Produces:
  - `type AdminHandlers struct{ operator auth.OperatorGuard }`
  - `func NewAdminHandlers(operator auth.OperatorGuard) *AdminHandlers`
  - methods `GetLogLevel`, `SetLogLevel`, `GetTraceSampler`, `SetTraceSampler` with signature `(w http.ResponseWriter, r *http.Request)`

- [ ] **Step 1: Write the failing unit tests** — in `internal/api/admin_test.go`:
  - Replace `HandleX(w, req)` calls with `NewAdminHandlers(auth.OperatorGuard{}).X(w, req)` (method names as above).
  - `adminContext` (`:17-25`) builds a `PLATFORM` `ROLE_ADMIN` context (rename to `operatorContext`).
  - The nil-context tests at `:50`, `:96`, `:109`, `:134`, `:230`, `:400` expect `401` and `errorCode` `UNAUTHORIZED` (rename `_Forbidden` → `_NoUserContext_401`).
  - Add, for each of the four methods, `…_TenantAdmin_403`: context `ROLE_ADMIN` in tenant `acme` → 403 `FORBIDDEN`, and the level/sampler is unchanged after a refused POST (read `logging.Level.Level()` / `observability.Sampler.Config()` before and after).
  - Add `TestAdminHandlers_MockGuard_AdmitsAdminInAnyTenant`: `NewAdminHandlers(auth.MockOperatorGuard()).GetLogLevel` with `ROLE_ADMIN` in `mock-tenant` → 200.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/api/ -run 'LogLevel|TraceSampler|AdminHandlers'`
Expected: FAIL to compile (`NewAdminHandlers` undefined).

- [ ] **Step 3: Implement** — in `admin.go`:

```go
// AdminHandlers serves the node's runtime controls: log level and trace
// sampler. They change process-wide state, so only a platform operator may
// call them.
type AdminHandlers struct {
	operator auth.OperatorGuard
}

// NewAdminHandlers returns the runtime-control handlers behind operator.
func NewAdminHandlers(operator auth.OperatorGuard) *AdminHandlers {
	return &AdminHandlers{operator: operator}
}
```

Each former `HandleX` becomes `func (a *AdminHandlers) X(w http.ResponseWriter, r *http.Request)`, and its inline `uc == nil || !spi.HasRole(...)` block becomes `if !a.operator.Require(w, r) { return }`. Update the doc comments ("Requires ROLE_ADMIN" → "Requires a platform operator"). Remove the now-unused `spi` import if nothing else uses it.

`app/app.go:625-628`:

```go
	adminHandlers := internalapi.NewAdminHandlers(operatorGuard)
	mux.Handle("GET /admin/log-level", authMW(http.HandlerFunc(adminHandlers.GetLogLevel)))
	mux.Handle("POST /admin/log-level", authMW(http.HandlerFunc(adminHandlers.SetLogLevel)))
	mux.Handle("GET /admin/trace-sampler", authMW(http.HandlerFunc(adminHandlers.GetTraceSampler)))
	mux.Handle("POST /admin/trace-sampler", authMW(http.HandlerFunc(adminHandlers.SetTraceSampler)))
```

If `operatorGuard` from Task 5 is not in scope at this point of `New`, move its declaration up so both uses share one value; do not build a second guard.

- [ ] **Step 4: Run unit tests**

Run: `go test ./internal/api/ ./app/`
Expected: PASS.

- [ ] **Step 5: Extend the E2E tests** — in `platform_operator_test.go`, add the four `/admin` pairs to the endpoint table used by the three refusal tests:

```go
		{"get log-level", http.MethodGet, "/admin/log-level", ""},
		{"set log-level", http.MethodPost, "/admin/log-level", `{"level":"info"}`},
		{"get trace-sampler", http.MethodGet, "/admin/trace-sampler", ""},
		{"set trace-sampler", http.MethodPost, "/admin/trace-sampler", `{"sampler":"always"}`},
```

(`/admin/*` is served under the `/api` context path — `admin_loglevel_test.go` calls `/api/admin/log-level` — so these paths go through `requestAs`, which adds `/api`, like the others.)

Add operator success for the trace sampler, which has no E2E today:

```go
// TestPlatformOperator_TraceSamplerRoundTrip: the operator reads the sampler
// and writes the same configuration back.
func TestPlatformOperator_TraceSamplerRoundTrip(t *testing.T) {
	resp := operatorRequest(t, http.MethodGet, "/admin/trace-sampler", nil)
	cfg := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET trace-sampler as operator: %d %s", resp.StatusCode, cfg)
	}
	resp = operatorRequest(t, http.MethodPost, "/admin/trace-sampler", []byte(cfg))
	if body := readBody(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("POST trace-sampler as operator: %d %s", resp.StatusCode, body)
	}
}
```

Log-level operator success is `admin_loglevel_test.go` (switched in Task 4).

Mock mode: add to `iam_gated_501_test.go` (it owns `newMockIAMServer`):

```go
// TestMockIAM_AdminLogLevelWorks: in mock mode the mock admin is the
// operator, so the runtime controls keep working for local development.
func TestMockIAM_AdminLogLevelWorks(t *testing.T) {
	base, cleanup := newMockIAMServer(t)
	defer cleanup()
	resp, err := http.Get(base + "/api/admin/log-level")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("mock mode GET /api/admin/log-level: %d, want 200", resp.StatusCode)
	}
}
```

(`TestGated_MockIAM_All21Return501` addresses the same server with `/api/...` paths.)

- [ ] **Step 6: Prove RED** — temporarily make one method use `auth.MockOperatorGuard()` in place of `a.operator` (i.e. tenant-blind), run `go test ./internal/e2e/ -run 'TestPlatformOperator_TenantAdmin_403'`: the `/admin` rows FAIL with 200. Restore.

- [ ] **Step 7: Run GREEN**

Run: `go test ./internal/e2e/ -run 'TestPlatformOperator_|TestAdminLogLevel|TestMockIAM_AdminLogLevelWorks|TestGated_MockIAM'` then `make test`.
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/api app/app.go internal/e2e
git commit -m "feat(api)!: log-level and trace-sampler controls require a platform operator"
```

---

### Task 7: Documentation, OpenAPI, CHANGELOG, COMPATIBILITY, cloud parity

**Files:** (all listed in spec §8)
- `cmd/cyoda/help/content/cli/token.md`, `config/auth.md`, `auth.md`, `auth/tokens.md`, `admin.md`, `telemetry.md`, `errors/FORBIDDEN.md`, `auth/oidc.md`
- `api/openapi.yaml` → `go generate ./api`
- `README.md`, `docs/ARCHITECTURE.md`, `CHANGELOG.md`, `COMPATIBILITY.md`
- Create: `docs/cloud-parity/platform-operator.md`; modify `docs/cloud-parity/README.md`

**Interfaces:** none (docs). Shipped text: plain, compact, present tense, no issue ids.

- [ ] **Step 1: Help topics** — one edit per item; keep each compact (actionable core plus a pointer):
  - `cli/token.md`: in DESCRIPTION, one sentence: the key-pair endpoints, OIDC reload and the runtime controls need a platform operator — `ROLE_ADMIN` in the tenant `PLATFORM` — so use `cyoda token --tenant PLATFORM` for them. In WHEN THE TOKEN IS REFUSED: `:37` "require `ROLE_ADMIN`" → "require a platform operator (`--tenant PLATFORM`)"; `:40` replace the recovery bullet with spec §4.7's three routes and the instruction "create an admin M2M client in `PLATFORM` before revoking the signing key". EXAMPLES: add `TOKEN=$(cyoda token --tenant PLATFORM)` followed by a key-pair `curl` (`POST /api/oauth/keys/keypair`).
  - `config/auth.md`: the key-pair section names the platform operator; recovery text at `:325-329`, `:387`, `:397-409` → spec §4.7 (the "admin from a federated OIDC provider" route becomes the `PLATFORM` admin M2M client; the new-signing-key route stays).
  - `auth.md`, `auth/tokens.md`: wherever they say who may manage key pairs.
  - `admin.md:28` and `telemetry.md:233`: "require a platform operator (`ROLE_ADMIN` in the tenant `PLATFORM`)".
  - `errors/FORBIDDEN.md`: add the cause "the endpoint needs a platform operator: `ROLE_ADMIN` in the tenant `PLATFORM` (signing key pairs, OIDC reload, `/admin/*`)"; fix `:24` so it no longer says role claims alone determine access; replace the "`admin` is required" example with the "platform operator required" detail.
  - `auth/oidc.md`: reload needs a platform operator; `:76` and `:187` — a tenant refreshes its own provider's keys with a `PATCH /oauth/oidc/providers/{id}` whose body is `{}`.

- [ ] **Step 2: Verify help** — Run: `go test ./cmd/cyoda/...` Expected: PASS (help-topic tests, error-code parity).

- [ ] **Step 3: OpenAPI** — `api/openapi.yaml`:
  - The five key-pair operations and `reloadOidcProviders`: description line "**Authorization Required:** platform operator — `ROLE_ADMIN` in the tenant `PLATFORM`." (replacing "ROLE_ADMIN role" where present); their `"403"` description: "Forbidden: the caller is not a platform operator (`ROLE_ADMIN` in the tenant `PLATFORM`)."
  - `:126`: "(requires ROLE_ADMIN)"; `:147`: the roles list without `SUPER_USER` (cyoda-go has none).
  - Run `go generate ./api`, then `go build ./...` and `go test ./internal/e2e/openapivalidator/ ./api/...`. Expected: PASS.

- [ ] **Step 4: README, ARCHITECTURE**
  - `README.md:128-134` endpoint table: reload row → "platform operator"; add or fix key-pair rows the same way; `:136` — reload re-reads every tenant's providers.
  - `docs/ARCHITECTURE.md`: `:1886`, `:1897`, `:1914`, `:2095`, `:2318` — state the platform-operator rule where they name who may call these endpoints; add one short paragraph in the auth section defining the platform operator and why the rule rests on tenant binding. Audit the surrounding auth section as a whole (present tense, no history, delete unverifiable claims).

- [ ] **Step 5: CHANGELOG** — `### Breaking`:

  "**Platform-wide admin endpoints need a platform operator.** The signing key-pair endpoints (`/oauth/keys/keypair*`), `POST /oauth/oidc/providers/reload`, `/admin/log-level` and `/admin/trace-sampler` accept only `ROLE_ADMIN` in the tenant `PLATFORM`; an admin of any other tenant gets `403 FORBIDDEN`. Get an operator token with `cyoda token --tenant PLATFORM`, or create an admin M2M client in `PLATFORM`. A tenant refreshes its own OIDC provider with an empty `PATCH`. Before this, any tenant's admin could revoke the signing key for the whole cluster. See `cyoda help cli token`."

- [ ] **Step 6: COMPATIBILITY** — in the v0.9.0 obligations (`:139-165`): "An out-of-tree plugin's parity fixture implements `PlatformOperator(t)` (a required `parity.BackendFixture` method); `fixtureutil.MintPlatformOperatorJWT` does it in one line."

- [ ] **Step 7: Cloud parity doc** — `docs/cloud-parity/platform-operator.md`, following `signing-key-pairs.md`'s layout ("What cyoda-go does" / "What Cloud must do" / "Evidence"), with spec §9's content: the six OpenAPI operations and the rule; `/admin/*` cyoda-go only; the two prerequisites (tenant binding for externally issued tokens; `PLATFORM` and `SYSTEM` unclaimable) with the Cloud citations; the found defects (keyId-only lookup reaching other tenants' keys, 401 vs 403, `SUPER_USER` added to every Auth0 user, OIDC providers not tenant-scoped). Add its row to `docs/cloud-parity/README.md` in the existing table format. Leave a "CaaS ticket:" line for Task 8.

- [ ] **Step 8: Exit check** — no doc still says a tenant admin may call an operator endpoint:

Run: `grep -rn 'ROLE_ADMIN' cmd/cyoda/help/content README.md docs/ARCHITECTURE.md api/openapi.yaml | grep -i 'keypair\|key pair\|key-pair\|reload\|log-level\|trace-sampler\|admin/'`
Expected: every hit names the platform operator / `PLATFORM`. And `grep -rn 'SUPER_USER' api/openapi.yaml cmd/cyoda/help/content README.md` → empty.

- [ ] **Step 9: Commit**

```bash
git add cmd/cyoda/help api README.md docs/ARCHITECTURE.md CHANGELOG.md COMPATIBILITY.md docs/cloud-parity
git commit -m "docs: platform operator for the platform-wide admin endpoints"
```

---

### Task 8: Cross-repo follow-through

**Files:** `docs/cloud-parity/platform-operator.md` (ticket number).

- [ ] **Step 1: Cassandra** — comment on `cyoda/cyoda-go-cassandra#108` (the v0.9.0 pin bump), title-only courtesy style per the sibling-repo convention: at the bump, `e2e/fixture.go`'s `cassandraFixture` needs

```go
func (f *cassandraFixture) PlatformOperator(t *testing.T) parity.Tenant {
	t.Helper()
	return fixtureutil.MintPlatformOperatorJWT(t, f.keySet)
}
```

  Do not open the PR now: cassandra pins cyoda-go v0.8.3, where `MintPlatformOperatorJWT` does not exist.

- [ ] **Step 2: CaaS ticket** — file in Jira project CP (component CaaS, title prefix `[CaaS]`) via the Rovo connector: "[CaaS] Platform operator for the platform-wide admin endpoints", body = the parity doc's "What Cloud must do" section. If the connector is unavailable, stop and give Paul the title and body to file.

- [ ] **Step 3:** put the ticket key in the parity doc's "CaaS ticket:" line and its README row; commit `docs(cloud-parity): cite the platform-operator CaaS ticket`.

---

## Final verification (after Task 8)

- [ ] `make test-full` — green (root + all plugin submodules + E2E). Read the `scripts/testreport` summary, not just the exit code.
- [ ] `go vet ./...` and `cd plugins/<each> && go vet ./...`.
- [ ] `make race` once.
- [ ] `make todos` — no new TODO without a plan reference.
- [ ] Whole-branch code review (`superpowers:requesting-code-review`, fresh-context reviewer) on the final head.
- [ ] Security audit (`antigravity-bundle-security-engineer:security-auditor`, fresh-context, most capable model) against Gate 3, told not to accept the tenant-binding claim on trust.
- [ ] PR against `release/v0.9.0` (check the base first); milestone #624 and close it when the PR lands.
