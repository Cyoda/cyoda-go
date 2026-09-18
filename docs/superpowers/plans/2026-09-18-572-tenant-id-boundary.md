# Tenant-ID Boundary Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Admit a tenant id only in a known grammar, at the two places one enters cyoda-go, and make the memory backend's blob path incapable of expressing a wrong one.

**Architecture:** A single `common.ValidateTenantID` enforced at exactly two doors — the `caas_org_id` JWT claim (which covers every HTTP and gRPC request) and `CYODA_BOOTSTRAP_TENANT_ID` at startup. Everywhere else carries values already admitted at one of those doors, so no further checks are added. Separately, the memory message store addresses blobs by `hex(tenant)/hex(id)` under an `os.Root`, which makes traversal, separators, empty and dot segments and case collision unrepresentable rather than rejected, and lets the existing hand-rolled check be deleted.

**Tech Stack:** Go 1.26.7, `os.Root` (Go 1.24+), `encoding/hex`, `crypto/rand`, `log/slog`, `github.com/google/uuid`, testcontainers-go for E2E.

**Spec:** [`docs/superpowers/specs/2026-09-18-572-tenant-id-boundary-design.md`](../specs/2026-09-18-572-tenant-id-boundary-design.md)

## Global Constraints

- Grammar, exactly: `^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`. 1–100 bytes, first byte a letter or digit, remaining bytes letters, digits, `.`, `_`, `-`. Case is preserved and significant.
- **No new error code.** Every rejection reuses an existing status and code. Do not add `internal/common/error_codes.go` entries and do not add `cmd/cyoda/help/content/errors/*.md`.
- **Never echo a rejected tenant id.** Not into an error string, a response body, or an `slog` attribute. Report the reason and a length or byte offset only. Log injection is one of the things this change exists to prevent.
- Use `log/slog` exclusively. Never `log.Printf` or `fmt.Printf`.
- Wrap errors with context: `fmt.Errorf("failed to X: %w", err)`.
- Tests live beside the code. Concurrency tests go in `plugins/memory`, never in `e2e/parity`.
- Iterate with `make test`. Never add `-count=1`. Never add `-v`.
- Commit after every task with the trailer `Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>`.

---

## File Structure

**Created**

| File | Responsibility |
|---|---|
| `internal/common/tenant_id.go` | `ValidateTenantID`, `ErrInvalidTenantID`, `MaxTenantIDLen`. Nothing else. |
| `internal/common/tenant_id_test.go` | The grammar's accept/reject table, including every tenant constant the binary ships. |
| `plugins/memory/blob_path.go` | `blobName`, `tenantBlobDir`, and the `os.Root` temp-file creator. Separated from `message_store.go` so the naming rule is readable on its own. |
| `plugins/memory/blob_path_test.go` | Encoding, distinctness including case, and temp-file loop behaviour. |
| `docs/cloud-parity/tenant-id-grammar.md` | The Gate-7 contract record. |

**Modified**

| File | Change |
|---|---|
| `internal/auth/validator.go:113-116` | `buildUserContext` validates the claim. |
| `internal/auth/validator_test.go` | Rejection and acceptance cases. |
| `internal/grpc/interceptor_test.go` | A real authenticator rejecting a bad-tenant token → `codes.Unauthenticated`. |
| `app/app.go:1051` | `validateBootstrapConfig` validates the tenant in the `idSet && secretSet` branch. |
| `app/app_test.go` | Bootstrap accept/reject, and the shipped-constants assertion. |
| `plugins/memory/message_store.go` | Root-relative I/O; `blobPath`/`tenantBlobDir` deleted; `Save` made atomic. |
| `plugins/memory/store_factory.go:131,231` | Open and close the `*os.Root`. |
| `plugins/memory/message_store_test.go` | Rewritten adversarial cases; case-distinctness; concurrency. |
| `internal/cluster/dispatch/handler.go:50-56` | Reject an `EntityMeta.TenantID` that disagrees with `TenantID`. |
| `internal/cluster/dispatch/handler_test.go` | The mismatch case. |
| `internal/domain/account/oidc_adapter.go` | Six tenant reads canonicalised (#587). |
| `internal/domain/account/oidc_adapter_test.go` | Uppercase-UUID round trip; non-UUID tenant → 400. |
| `internal/auth/token.go:78,97,211,230,272` | Ticket UUID on `server_error` (#588). |
| `internal/auth/token_test.go` | Ticket present and logged. |
| `internal/e2e/e2e_test.go` | Export the signing key so tests can mint arbitrary claims. |
| `internal/e2e/auth_failures_test.go` | Hostile-claim 401 and accepted-set 200 through the full stack. |
| `internal/e2e/oauth_keys_test.go` | Token-endpoint 500 ticket, OIDC canonicalisation. |
| `CHANGELOG.md` | `### Breaking`, `### Fixed`, `### Security` under `[Unreleased]`. |
| `cmd/cyoda/help/content/config/auth.md`, `cmd/cyoda/help/config_registry.go:101`, `README.md`, `docs/ARCHITECTURE.md:1588` | The accepted set for `CYODA_BOOTSTRAP_TENANT_ID`. |

**Deliberately untouched:** `internal/auth/kv_trusted_store.go` (kid is globally unique; a comment records that), `internal/auth/oidc/broadcast.go`, `internal/cluster/scheduler_rpc.go`, `internal/scheduler/executor.go`, `internal/domain/search/reaper.go`, the SPI.

---

### Task 1: The grammar

**Files:**
- Create: `internal/common/tenant_id.go`
- Test: `internal/common/tenant_id_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `common.ErrInvalidTenantID error`; `common.MaxTenantIDLen = 100`; `func common.ValidateTenantID(id spi.TenantID) error` — returns nil when `id` matches the grammar, otherwise an error wrapping `ErrInvalidTenantID` whose message never contains `id`.

- [ ] **Step 1: Write the failing test**

Create `internal/common/tenant_id_test.go`:

```go
package common

import (
	"errors"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func TestValidateTenantID_Accepts(t *testing.T) {
	// Every shape that exists in cyoda-go or in Cyoda Cloud today. A failure
	// here is a production or test-fixture lockout, not a style question.
	accepted := []string{
		"SYSTEM",                               // spi.SystemTenantID
		"CYODA",                                // Cloud's local-issuer fallback
		"default-tenant",                       // CYODA_BOOTSTRAP_TENANT_ID default
		"mock-tenant",                          // IAM mock mode
		"system-tenant",                        // parity fixtures
		"riskblocs",                            // scripts/multi-node-docker
		"my-tenant",                            // scripts README
		"tenant-abc-123",                       // published OpenAPI example
		"tenant-A",                             // case-varied package fixtures
		"tenant-a",
		"123",                                  // Cloud uses bare numerics
		"caas_mock-oidc-org-test-subject",      // Cloud's caas_<org_id> form
		"TEST_LEGAL_ENTITY",                    // Cloud fixtures
		"9f8c7b6a5d4e3f2a1b0c9d8e7f6a5b4c",     // Cloud's generated 32-hex id
		"1a2b3c4d-5e6f-4a8b-9c0d-1e2f3a4b5c6d", // canonical UUID
		"conformance-1a2b3c4d-5e6f-4a8b-9c0d-1e2f3a4b5c6d", // spitest, 48 chars
		"a",                                    // shortest legal
		strings.Repeat("x", 100),               // longest legal
		"a.b",                                  // dot is admitted
	}
	for _, id := range accepted {
		if err := ValidateTenantID(spi.TenantID(id)); err != nil {
			t.Errorf("ValidateTenantID(%q) = %v, want nil", id, err)
		}
	}
}

func TestValidateTenantID_Rejects(t *testing.T) {
	rejected := map[string]string{
		"empty":          "",
		"dot":            ".",
		"dotdot":         "..",
		"traversal":      "../victim",
		"hidden":         ".hidden",
		"leading-dash":   "-flag",
		"leading-under":  "_x",
		"slash":          "a/b",
		"backslash":      `a\b`,
		"colon":          "a:b",
		"space":          "a b",
		"newline":        "a\nb",
		"cr":             "a\rb",
		"nul":            "a\x00b",
		"tab":            "a\tb",
		"at":             "a@b",
		"percent":        "a%b",
		"quote":          `a"b`,
		"brace":          "{a}",
		"non-ascii":      "tenÅnt",
		"too-long":       strings.Repeat("x", 101),
	}
	for name, id := range rejected {
		t.Run(name, func(t *testing.T) {
			err := ValidateTenantID(spi.TenantID(id))
			if err == nil {
				t.Fatalf("ValidateTenantID(%q) = nil, want an error", id)
			}
			if !errors.Is(err, ErrInvalidTenantID) {
				t.Errorf("error does not wrap ErrInvalidTenantID: %v", err)
			}
		})
	}
}

// TestValidateTenantID_ErrorNeverEchoesValue is the log-injection guard: the
// rejected id reaches slog through the auth failure path's detail field, so an
// attacker-chosen claim must not be able to ride into a log record.
func TestValidateTenantID_ErrorNeverEchoesValue(t *testing.T) {
	needle := "NEEDLE-a/b\nforged-log-line"
	err := ValidateTenantID(spi.TenantID(needle))
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "NEEDLE") {
		t.Fatalf("error echoes the rejected value: %q", err.Error())
	}
	if strings.ContainsAny(err.Error(), "\n\r") {
		t.Fatalf("error carries a control character: %q", err.Error())
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/common/ -run TestValidateTenantID`
Expected: FAIL — `undefined: ValidateTenantID`, `undefined: ErrInvalidTenantID`.

- [ ] **Step 3: Write the implementation**

Create `internal/common/tenant_id.go`:

```go
package common

import (
	"errors"
	"fmt"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// MaxTenantIDLen bounds a tenant id at 100 bytes. The figure is not arbitrary:
// it matches the only length Cyoda Cloud declares on anything tenant-shaped,
// @ColumnValidation(maxLength = 100) on CSUser.legalEntityId, which Cloud's
// auto-enrollment already writes the caas_org_id claim into. A longer tenant
// works here and fails to persist a user there; capping at the same figure
// keeps the two tiers telling the caller the same thing.
const MaxTenantIDLen = 100

// ErrInvalidTenantID reports a tenant id outside the accepted grammar.
var ErrInvalidTenantID = errors.New("invalid tenant id")

// ValidateTenantID reports whether id is a tenant identifier cyoda-go admits:
//
//	^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$
//
// Case is preserved and significant — spi.SystemTenantID is "SYSTEM" and
// Cyoda Cloud's local-issuer fallback is "CYODA", so folding case would both
// reject real tenants and merge ones that must stay distinct.
//
// Requiring the first byte to be alphanumeric is what makes "", ".", "..",
// ".hidden" and "-leading" unrepresentable rather than enumerated.
//
// The returned error NEVER contains id. A rejected tenant id is
// attacker-chosen and reaches slog through the auth failure path's detail
// field; echoing it there is the log-injection this validation exists to
// prevent. The error carries a reason and a byte offset instead.
func ValidateTenantID(id spi.TenantID) error {
	s := string(id)
	switch {
	case len(s) == 0:
		return fmt.Errorf("%w: empty", ErrInvalidTenantID)
	case len(s) > MaxTenantIDLen:
		return fmt.Errorf("%w: %d bytes exceeds the %d-byte limit",
			ErrInvalidTenantID, len(s), MaxTenantIDLen)
	case !isTenantAlphanumeric(s[0]):
		return fmt.Errorf("%w: must begin with a letter or digit", ErrInvalidTenantID)
	}
	for i := 1; i < len(s); i++ {
		if c := s[i]; !isTenantAlphanumeric(c) && c != '.' && c != '_' && c != '-' {
			return fmt.Errorf("%w: disallowed byte at offset %d", ErrInvalidTenantID, i)
		}
	}
	return nil
}

func isTenantAlphanumeric(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/common/ -run TestValidateTenantID`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/common/tenant_id.go internal/common/tenant_id_test.go
git commit -m "feat(common): a tenant id has a grammar

^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$, admitting every value that exists in
cyoda-go or in Cyoda Cloud. The 100-byte cap matches Cloud's only declared
one, on CSUser.legalEntityId, which auto-enrollment already writes the claim
into.

The error never echoes the rejected value: it reaches slog through the auth
failure detail field, and a claim is attacker-chosen.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: Door 1 — the JWT claim

**Files:**
- Modify: `internal/auth/validator.go:113-116`
- Test: `internal/auth/validator_test.go`, `internal/grpc/interceptor_test.go`

**Interfaces:**
- Consumes: `common.ValidateTenantID`, `common.ErrInvalidTenantID` (Task 1).
- Produces: nothing new. `JWKSValidator.Validate` now returns a non-nil error for a token whose `caas_org_id` fails the grammar; `DelegatingAuthenticator.Authenticate` already collapses every such failure to `ErrAuthenticationFailed`.

Existing helper, already in the package, used by the tests below:
`signTokenWithKey(t *testing.T, kid string, priv *rsa.PrivateKey, iss, sub, orgID string, expOffsetSec int) string`
(`internal/auth/test_helpers_test.go:73`).

- [ ] **Step 1: Write the failing test**

Append to `internal/auth/validator_test.go`:

```go
// TestValidator_RejectsTenantOutsideGrammar pins door 1: the caas_org_id claim
// is the one place a tenant id enters cyoda-go on a request, covering HTTP and
// gRPC alike, so a claim outside the grammar must not produce a UserContext.
func TestValidator_RejectsTenantOutsideGrammar(t *testing.T) {
	v, kid, priv := newTestValidator(t)

	for name, org := range map[string]string{
		"traversal": "../victim",
		"dotdot":    "..",
		"slash":     "a/b",
		"colon":     "a:b",
		"newline":   "tenant\ninjected",
		"nul":       "tenant\x00",
		"too-long":  strings.Repeat("x", 101),
	} {
		t.Run(name, func(t *testing.T) {
			tok := signTokenWithKey(t, kid, priv, testIssuer, "user-1", org, 3600)
			uc, err := v.Validate(tok)
			if err == nil {
				t.Fatalf("Validate accepted tenant %q, got UserContext %+v", org, uc)
			}
			if uc != nil {
				t.Errorf("Validate returned a UserContext alongside an error: %+v", uc)
			}
			if strings.Contains(err.Error(), org) {
				t.Errorf("validator error echoes the rejected tenant: %q", err.Error())
			}
		})
	}
}

// TestValidator_AcceptsShippedTenantShapes is the regression half: the grammar
// must not lock out anything that authenticates today.
func TestValidator_AcceptsShippedTenantShapes(t *testing.T) {
	v, kid, priv := newTestValidator(t)

	for _, org := range []string{
		"SYSTEM",
		"default-tenant",
		"mock-tenant",
		"tenant-abc-123",
		"9f8c7b6a5d4e3f2a1b0c9d8e7f6a5b4c",
		"1a2b3c4d-5e6f-4a8b-9c0d-1e2f3a4b5c6d",
	} {
		t.Run(org, func(t *testing.T) {
			tok := signTokenWithKey(t, kid, priv, testIssuer, "user-1", org, 3600)
			uc, err := v.Validate(tok)
			if err != nil {
				t.Fatalf("Validate(%q) = %v, want nil", org, err)
			}
			if string(uc.Tenant.ID) != org {
				t.Errorf("tenant = %q, want %q", uc.Tenant.ID, org)
			}
		})
	}
}
```

Read `internal/auth/validator_test.go` first and reuse whatever constructor and
issuer constant the file already has. If there is no `newTestValidator` helper
returning `(*JWKSValidator, kid string, priv *rsa.PrivateKey)`, add one
modelled on the existing setup in that file, and reuse the file's existing
issuer constant rather than introducing `testIssuer`.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/auth/ -run TestValidator_RejectsTenantOutsideGrammar`
Expected: FAIL — `Validate accepted tenant "../victim"`.

- [ ] **Step 3: Write the implementation**

In `internal/auth/validator.go`, replace the `orgID` block in `buildUserContext`:

```go
	orgID, _ := claims["caas_org_id"].(string)
	if orgID == "" {
		return nil, fmt.Errorf("missing caas_org_id claim")
	}
	// Door 1. The caas_org_id claim is one of only two places a tenant id
	// enters cyoda-go from outside it (the other is CYODA_BOOTSTRAP_TENANT_ID),
	// and it covers every HTTP and gRPC request — the gRPC interceptor
	// delegates to this same authenticator. Validating here is what lets every
	// downstream consumer treat the tenant as well-formed without rechecking.
	//
	// The error deliberately carries no part of the claim: it reaches slog via
	// logAuthFailure's detail field, and the claim is attacker-chosen.
	if err := common.ValidateTenantID(spi.TenantID(orgID)); err != nil {
		return nil, fmt.Errorf("caas_org_id claim rejected: %w", err)
	}
```

Add `"github.com/cyoda-platform/cyoda-go/internal/common"` to the import block.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/auth/`
Expected: PASS. If a pre-existing test in this package mints a token with a
tenant outside the grammar, fix the fixture rather than widening the grammar —
and say so in the commit message.

- [ ] **Step 5: Add the gRPC cell**

Append to `internal/grpc/interceptor_test.go`. This deliberately wires the
*real* authenticator rather than `mockAuthService`, because the point is that
gRPC inherits door 1 rather than having a door of its own:

```go
// TestInterceptor_UnaryRejectsTenantOutsideGrammar proves gRPC inherits door 1:
// the interceptor delegates to the same AuthenticationService the HTTP
// middleware uses, so a claim outside the tenant grammar is Unauthenticated
// here too, with the same generic message.
func TestInterceptor_UnaryRejectsTenantOutsideGrammar(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	const kid = "grpc-tenant-test"
	const issuer = "cyoda-grpc-test"

	ks := auth.NewInMemoryKeyStore()
	if err := ks.Save(&auth.KeyPair{
		KID:        kid,
		Audience:   "client",
		Algorithm:  "RS256",
		PublicKey:  &priv.PublicKey,
		PrivateKey: priv,
		Active:     true,
		ValidFrom:  time.Now().Add(-time.Minute),
	}, auth.RotateOptions{}); err != nil {
		t.Fatalf("save key: %v", err)
	}

	validator := auth.NewValidatorFromSource(auth.NewLocalKeySource(ks), issuer)
	authSvc := auth.NewDelegatingAuthenticator(validator)
	interceptor := UnaryAuthInterceptor(authSvc)

	now := time.Now()
	tok, err := auth.Sign(map[string]any{
		"iss":          issuer,
		"sub":          "user-1",
		"caas_user_id": "user-1",
		"caas_org_id":  "../victim",
		"iat":          float64(now.Unix()),
		"exp":          float64(now.Add(time.Hour).Unix()),
	}, priv, kid)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.MD{"authorization": []string{"Bearer " + tok}})

	handler := func(_ context.Context, _ any) (any, error) {
		t.Fatal("handler must not be called when the tenant claim is rejected")
		return nil, nil
	}

	_, err = interceptor(ctx, "request",
		&googlegrpc.UnaryServerInfo{FullMethod: "/test.Service/Method"}, handler)
	if err == nil {
		t.Fatal("expected an error")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected a gRPC status error, got %v", err)
	}
	if st.Code() != codes.Unauthenticated {
		t.Errorf("code = %v, want Unauthenticated", st.Code())
	}
	if st.Message() != "authentication failed" {
		t.Errorf("message = %q, want the generic %q", st.Message(), "authentication failed")
	}
	if strings.Contains(st.Message(), "victim") {
		t.Error("gRPC status echoes the rejected tenant")
	}
}
```

Add `crypto/rand`, `crypto/rsa`, `time` and
`"github.com/cyoda-platform/cyoda-go/internal/auth"` to that file's imports.

- [ ] **Step 6: Run the gRPC test**

Run: `go test ./internal/grpc/ -run TestInterceptor_`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/auth/validator.go internal/auth/validator_test.go internal/grpc/interceptor_test.go
git commit -m "feat(auth): the caas_org_id claim must match the tenant grammar

Door 1 of two. The claim is where a tenant id enters cyoda-go on a request,
and it covers gRPC as well as HTTP because the interceptor delegates to the
same authenticator. A rejected claim collapses into the existing generic
ErrAuthenticationFailed, so the caller sees the same 401 as for any other bad
token and learns nothing from the difference.

The validator error carries no part of the claim: it reaches slog through
logAuthFailure's detail field.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: Door 2 — bootstrap config

**Files:**
- Modify: `app/app.go:1051` (`validateBootstrapConfig`)
- Test: `app/app_test.go`

**Interfaces:**
- Consumes: `common.ValidateTenantID` (Task 1).
- Produces: nothing new. `validateBootstrapConfig` returns a non-nil error for a bad tenant when a bootstrap client is configured; `app.New` already propagates it (`app/app.go:104`), and `cmd/cyoda` already exits non-zero on an `app.New` error.

**Note on the coverage matrix.** The spec listed a subprocess exit-code E2E for
this row. `app.New` returning the error *is* the behaviour; `main` turning a
non-nil error into a non-zero exit is Go. Testing `validateBootstrapConfig` and
`app.New` directly is the meaningful test and needs no subprocess, so that is
what this task builds.

- [ ] **Step 1: Write the failing test**

Append to `app/app_test.go`:

```go
// TestValidateBootstrapConfig_TenantGrammar pins door 2. A bootstrap tenant is
// operator-supplied configuration, so it is validated at startup and the
// process refuses to come up rather than running with a tenant that cannot be
// addressed consistently.
func TestValidateBootstrapConfig_TenantGrammar(t *testing.T) {
	base := func(tenant string) *Config {
		cfg := DefaultConfig()
		cfg.IAM.Mode = "jwt"
		cfg.Bootstrap.ClientID = "bootstrap-client"
		cfg.Bootstrap.ClientSecret = "bootstrap-secret"
		cfg.Bootstrap.TenantID = tenant
		return &cfg
	}

	for name, tenant := range map[string]string{
		"traversal": "../victim",
		"slash":     "a/b",
		"empty":     "",
		"too-long":  strings.Repeat("x", 101),
	} {
		t.Run("reject/"+name, func(t *testing.T) {
			if _, err := validateBootstrapConfig(base(tenant)); err == nil {
				t.Fatalf("validateBootstrapConfig accepted tenant %q", tenant)
			}
		})
	}

	for _, tenant := range []string{"default-tenant", "SYSTEM", "1a2b3c4d-5e6f-4a8b-9c0d-1e2f3a4b5c6d"} {
		t.Run("accept/"+tenant, func(t *testing.T) {
			if _, err := validateBootstrapConfig(base(tenant)); err != nil {
				t.Fatalf("validateBootstrapConfig(%q) = %v, want nil", tenant, err)
			}
		})
	}
}

// TestValidateBootstrapConfig_EmptyTenantWithoutBootstrapClient guards the one
// deployment shape the check could otherwise break. envString uses LookupEnv,
// so an explicitly-empty CYODA_BOOTSTRAP_TENANT_ID overrides the default — and
// a deployment that configures no bootstrap client never consumes the tenant
// at all, so it must still start.
func TestValidateBootstrapConfig_EmptyTenantWithoutBootstrapClient(t *testing.T) {
	cfg := DefaultConfig()
	cfg.IAM.Mode = "jwt"
	cfg.Bootstrap.ClientID = ""
	cfg.Bootstrap.ClientSecret = ""
	cfg.Bootstrap.TenantID = ""

	if _, err := validateBootstrapConfig(&cfg); err != nil {
		t.Fatalf("validateBootstrapConfig = %v, want nil when no bootstrap client is configured", err)
	}
}

// TestShippedTenantConstantsSatisfyGrammar stops a later change to a default
// from producing a binary that cannot start, or a mock mode that cannot
// authenticate.
func TestShippedTenantConstantsSatisfyGrammar(t *testing.T) {
	cfg := DefaultConfig()
	for name, id := range map[string]spi.TenantID{
		"spi.SystemTenantID":            spi.SystemTenantID,
		"IAM.MockTenantID":              spi.TenantID(cfg.IAM.MockTenantID),
		"CYODA_BOOTSTRAP_TENANT_ID dflt": spi.TenantID(cfg.Bootstrap.TenantID),
	} {
		if err := common.ValidateTenantID(id); err != nil {
			t.Errorf("%s (%q) fails the tenant grammar: %v", name, id, err)
		}
	}
}
```

Add `strings`, `spi "github.com/cyoda-platform/cyoda-go-spi"` and
`"github.com/cyoda-platform/cyoda-go/internal/common"` to that file's imports if
absent.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./app/ -run 'TestValidateBootstrapConfig_TenantGrammar|TestShippedTenantConstants'`
Expected: FAIL — `validateBootstrapConfig accepted tenant "../victim"`.

- [ ] **Step 3: Write the implementation**

In `app/app.go`, in `validateBootstrapConfig`, replace the `idSet && secretSet`
case:

```go
	case idSet && secretSet:
		// Bootstrap M2M client configured. Creation happens in New().
		//
		// Door 2 of two. The tenant is validated here rather than at the top of
		// the function because envString uses LookupEnv: an explicitly-empty
		// CYODA_BOOTSTRAP_TENANT_ID overrides the default, and a deployment
		// that configures no bootstrap client never consumes the value. Only
		// the branch that actually creates a client may refuse to start over it.
		if err := common.ValidateTenantID(spi.TenantID(out.Bootstrap.TenantID)); err != nil {
			return nil, fmt.Errorf("CYODA_BOOTSTRAP_TENANT_ID is not a valid tenant id: %w", err)
		}
		return &out, nil
```

Add `"github.com/cyoda-platform/cyoda-go/internal/common"` to the imports if
absent; `spi` is already imported.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./app/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add app/app.go app/app_test.go
git commit -m "feat(app): CYODA_BOOTSTRAP_TENANT_ID must match the tenant grammar

Door 2 of two. Validated inside the branch that actually creates a bootstrap
client: envString uses LookupEnv, so an explicitly-empty value overrides the
default, and a deployment configuring no bootstrap client never consumes the
tenant and must still start.

A unit test asserts every tenant constant the binary ships satisfies the
grammar, so a later change to a default cannot produce an unbootable binary.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 4: Door 1 and 2 through the full HTTP stack

**Files:**
- Modify: `internal/e2e/e2e_test.go` (export the signing key)
- Modify: `internal/e2e/auth_failures_test.go`

**Interfaces:**
- Consumes: Tasks 2 and 3.
- Produces: `e2eSignKey *rsa.PrivateKey` and `mintTokenWithTenant(t *testing.T, tenant string) string` in package `e2e_test`, for later tasks.

- [ ] **Step 1: Export the signing key from TestMain**

In `internal/e2e/e2e_test.go`, add to the `var (...)` block at line 35:

```go
	e2eSignKey      *rsa.PrivateKey                   // the stack's JWT signing key, for tests that mint bespoke claims
	e2eIssuer       string                            // the stack's JWT issuer, for the same
```

and in `TestMain`, immediately after the existing `rsaKey` is generated
(line 94) and after the config's issuer is known, assign both:

```go
	e2eSignKey = rsaKey
	e2eIssuer = cfg.IAM.JWTIssuer
```

Use whatever the config variable is actually called in that function; read the
surrounding lines before editing.

- [ ] **Step 2: Write the failing test**

Append to `internal/e2e/auth_failures_test.go`:

```go
// mintTokenWithTenant signs a first-party token carrying an arbitrary
// caas_org_id, so a test can present a claim no legitimate client could
// obtain. The kid is the one app.NewAuthService derives from the signing key's
// public part (sha256(SPKI)[:16] hex).
func mintTokenWithTenant(t *testing.T, tenant string) string {
	t.Helper()
	pubDER, err := x509.MarshalPKIXPublicKey(&e2eSignKey.PublicKey)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	sum := sha256.Sum256(pubDER)
	kid := hex.EncodeToString(sum[:16])

	now := time.Now()
	tok, err := auth.Sign(map[string]any{
		"sub":          "e2e-tenant-probe",
		"iss":          e2eIssuer,
		"caas_user_id": "e2e-tenant-probe",
		"caas_org_id":  tenant,
		"user_roles":   []string{"ROLE_ADMIN"},
		"exp":          now.Add(time.Hour).Unix(),
		"iat":          now.Unix(),
		"jti":          uuid.NewString(),
	}, e2eSignKey, kid)
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}
	return tok
}

// TestAuth_TenantClaimOutsideGrammar_401 proves door 1 holds through the full
// HTTP stack, on a token this server itself would accept but for the claim.
// The rejection must be indistinguishable from any other bad token.
func TestAuth_TenantClaimOutsideGrammar_401(t *testing.T) {
	for name, tenant := range map[string]string{
		"traversal": "../victim",
		"dotdot":    "..",
		"slash":     "a/b",
		"colon":     "a:b",
		"newline":   "tenant\ninjected",
		"too-long":  strings.Repeat("x", 101),
	} {
		t.Run(name, func(t *testing.T) {
			resp := unauthRequest(t, http.MethodGet, "/api/entity/e2e-auth-probe/1",
				"Bearer "+mintTokenWithTenant(t, tenant))
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				raw, _ := io.ReadAll(resp.Body)
				t.Fatalf("status=%d, want 401; body: %s", resp.StatusCode, raw)
			}
			assertUnauthorizedProblem(t, resp)

			raw, _ := io.ReadAll(resp.Body)
			if strings.Contains(string(raw), "victim") || strings.Contains(string(raw), "injected") {
				t.Errorf("response echoes the rejected tenant: %s", raw)
			}
		})
	}
}

// TestAuth_AcceptedTenantShapesStillAuthenticate is the regression half: the
// grammar must not lock out a shape that works today. 401 here would be a
// production lockout; anything else means the token was accepted.
func TestAuth_AcceptedTenantShapesStillAuthenticate(t *testing.T) {
	for _, tenant := range []string{
		"SYSTEM",
		"default-tenant",
		"tenant-abc-123",
		"9f8c7b6a5d4e3f2a1b0c9d8e7f6a5b4c",
		"1a2b3c4d-5e6f-4a8b-9c0d-1e2f3a4b5c6d",
		"conformance-1a2b3c4d-5e6f-4a8b-9c0d-1e2f3a4b5c6d",
	} {
		t.Run(tenant, func(t *testing.T) {
			resp := unauthRequest(t, http.MethodGet, "/api/model/",
				"Bearer "+mintTokenWithTenant(t, tenant))
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusUnauthorized {
				raw, _ := io.ReadAll(resp.Body)
				t.Fatalf("tenant %q was rejected — this is a lockout; body: %s", tenant, raw)
			}
		})
	}
}
```

Add `crypto/sha256`, `crypto/x509`, `encoding/hex`, `time`,
`"github.com/google/uuid"` and
`"github.com/cyoda-platform/cyoda-go/internal/auth"` to that file's imports.

- [ ] **Step 3: Run the test to verify it fails**

If Task 2 is already committed this passes immediately. To see it fail, stash
the validator change first. Otherwise verify the new tests run and pass:

Run: `go test ./internal/e2e/ -run 'TestAuth_TenantClaimOutsideGrammar_401|TestAuth_AcceptedTenantShapesStillAuthenticate'`
Expected: PASS (Docker must be running).

- [ ] **Step 4: Run the whole E2E package**

Run: `go test ./internal/e2e/`
Expected: PASS. A pre-existing test that mints a tenant outside the grammar
must have its fixture fixed, not the grammar widened.

- [ ] **Step 5: Commit**

```bash
git add internal/e2e/e2e_test.go internal/e2e/auth_failures_test.go
git commit -m "test(e2e): a tenant claim outside the grammar is an ordinary 401

Exercises door 1 on a token the stack itself signed, so the only thing wrong
with it is the claim — the case a client-minted token cannot construct. Asserts
the response is the uniform problem detail and echoes no part of the rejected
value, and pins the accepted shapes so the grammar cannot become a lockout.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 5: A blob path that cannot spell a tenant wrong

**Files:**
- Create: `plugins/memory/blob_path.go`, `plugins/memory/blob_path_test.go`
- Modify: `plugins/memory/message_store.go`, `plugins/memory/store_factory.go:113,131-136,231`
- Test: `plugins/memory/message_store_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks. This task is independent of Tasks 1–4.
- Produces (package-private, used only inside `plugins/memory`):
  - `func blobName(tenant spi.TenantID, id string) string` — `hex(tenant)/hex(id)`, always root-relative.
  - `func tenantBlobDir(tenant spi.TenantID) string` — `hex(tenant)`.
  - `func createTempBlob(root *os.Root, dir string) (*os.File, string, error)` — an exclusively-created temp file under `dir`, returning the handle and its root-relative name.
  - `StoreFactory.blobRoot *os.Root`.

**Working in this module:** `plugins/memory` has its own `go.mod`. Run its
tests from that directory: `cd plugins/memory && go test ./...`.

- [ ] **Step 1: Write the failing test**

Create `plugins/memory/blob_path_test.go`:

```go
package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// TestBlobName_IsUnspellable asserts the property the encoding buys: no tenant
// id and no message id, however hostile, can produce a name containing a path
// separator, a dot segment, or an upper-case byte. Traversal and case
// collision stop being rejections and become unrepresentable.
func TestBlobName_IsUnspellable(t *testing.T) {
	hostile := []string{
		"../victim", "..", ".", "", "a/b", `a\b`, "a:b", "NUL", "COM1",
		"tenant\x00", "tenant\n", strings.Repeat("x", 300),
	}
	for _, tenant := range hostile {
		for _, id := range hostile {
			name := blobName(spi.TenantID(tenant), id)
			if strings.Count(name, "/") != 1 {
				t.Fatalf("blobName(%q, %q) = %q: want exactly one separator", tenant, id, name)
			}
			for _, seg := range strings.Split(name, "/") {
				if seg == "" || seg == "." || seg == ".." {
					t.Fatalf("blobName(%q, %q) = %q: degenerate segment %q", tenant, id, name, seg)
				}
				for i := 0; i < len(seg); i++ {
					if c := seg[i]; !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
						t.Fatalf("blobName(%q, %q) = %q: non-hex byte %q", tenant, id, name, c)
					}
				}
			}
		}
	}
}

// TestBlobName_DistinctTenantsNeverShareADirectory is the case-collision
// regression. On APFS and on Windows, "tenant-a" and "tenant-A" are one
// directory; os.Root does not see that, because it guarantees confinement, not
// distinctness. Hex encoding is what separates them.
func TestBlobName_DistinctTenantsNeverShareADirectory(t *testing.T) {
	pairs := [][2]string{
		{"tenant-a", "tenant-A"},
		{"SYSTEM", "system"},
		{"Acme", "aCME"},
	}
	for _, p := range pairs {
		a := filepath.Dir(blobName(spi.TenantID(p[0]), "id"))
		b := filepath.Dir(blobName(spi.TenantID(p[1]), "id"))
		if a == b {
			t.Fatalf("tenants %q and %q share directory %q", p[0], p[1], a)
		}
	}
}

// TestCreateTempBlob_ExclusiveAndCleanable pins the replacement for
// os.CreateTemp, which *os.Root does not provide: each call must return a
// distinct, newly created file that can be removed through the same root.
func TestCreateTempBlob_ExclusiveAndCleanable(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer root.Close()

	const tenantDir = "abcdef"
	if err := root.MkdirAll(tenantDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		f, name, err := createTempBlob(root, tenantDir)
		if err != nil {
			t.Fatalf("createTempBlob: %v", err)
		}
		if seen[name] {
			t.Fatalf("createTempBlob returned a duplicate name %q", name)
		}
		seen[name] = true
		if !strings.HasPrefix(name, tenantDir+"/") {
			t.Fatalf("temp name %q is not under %q", name, tenantDir)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		if err := root.Remove(name); err != nil {
			t.Fatalf("remove %q: %v", name, err)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd plugins/memory && go test ./... -run 'TestBlobName|TestCreateTempBlob'`
Expected: FAIL — `undefined: blobName`, `undefined: createTempBlob`.

- [ ] **Step 3: Write the implementation**

Create `plugins/memory/blob_path.go`:

```go
package memory

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// Blob names are hex-encoded rather than validated.
//
// The tenant id and the message id both become path segments, and a check that
// rejects bad spellings cannot cover every way two names collide. The one that
// bit us was case: on APFS and on Windows "tenant-a" and "tenant-A" are the
// same directory, so two tenants shared a blob namespace. os.Root does not see
// that either — it guarantees confinement, not distinctness.
//
// Hex output is drawn from [0-9a-f], so a separator, a dot segment, an empty
// segment, a NUL, a Windows reserved device name and a case-only difference
// are all unrepresentable rather than rejected. The mapping is injective, so
// distinct inputs always name distinct files.
//
// The cost is that each segment doubles in length. A tenant is capped at 100
// bytes by common.ValidateTenantID, giving a 200-byte directory name; a
// message id is usable to 127 bytes before the 255-byte filename limit. Ids
// are server-generated time UUIDs (36 bytes), so that is slack rather than a
// constraint, and an over-long id surfaces as an ordinary write error.

// tenantBlobDir returns the root-relative directory holding one tenant's blobs.
func tenantBlobDir(tenant spi.TenantID) string {
	return hex.EncodeToString([]byte(tenant))
}

// blobName returns the root-relative path of one message blob.
func blobName(tenant spi.TenantID, id string) string {
	return tenantBlobDir(tenant) + "/" + hex.EncodeToString([]byte(id))
}

// tempBlobAttempts bounds the exclusive-create retry. A collision needs two
// callers to draw the same 128 random bits, so anything beyond a couple of
// attempts means the filesystem is refusing writes for another reason and
// spinning would hide it.
const tempBlobAttempts = 10

// createTempBlob is the *os.Root replacement for os.CreateTemp, which the
// rooted API does not provide. It returns an open handle and the file's
// root-relative name; the caller closes the handle and, on any failure after
// this point, removes the name through the same root.
func createTempBlob(root *os.Root, dir string) (*os.File, string, error) {
	var suffix [16]byte
	for attempt := 0; attempt < tempBlobAttempts; attempt++ {
		if _, err := rand.Read(suffix[:]); err != nil {
			return nil, "", fmt.Errorf("failed to draw temp blob suffix: %w", err)
		}
		name := dir + "/tmp-" + hex.EncodeToString(suffix[:])
		f, err := root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			return f, name, nil
		}
		if !os.IsExist(err) {
			return nil, "", fmt.Errorf("failed to create temp blob file: %w", err)
		}
	}
	return nil, "", fmt.Errorf("failed to create temp blob file: %d name collisions", tempBlobAttempts)
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `cd plugins/memory && go test ./... -run 'TestBlobName|TestCreateTempBlob'`
Expected: PASS.

- [ ] **Step 5: Commit the naming layer**

```bash
git add plugins/memory/blob_path.go plugins/memory/blob_path_test.go
git commit -m "feat(memory): name blobs by hex, not by the tenant's spelling

A check that rejects bad spellings cannot cover every way two names
collide, and the one that bit us was case: on APFS and on Windows
tenant-a and tenant-A are the same directory. os.Root does not see that
either — it guarantees confinement, not distinctness.

Hex output is [0-9a-f], so separators, dot segments, empty segments, NUL,
Windows device names and case-only differences all become unrepresentable
rather than rejected, and the mapping is injective.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

- [ ] **Step 6: Write the failing store test**

Replace the adversarial-id test at `plugins/memory/message_store_test.go:268-305`
with the following, and keep every other test in the file:

```go
// TestMessageStore_HostileTenantAndIDRoundTrip replaces the traversal-rejection
// test. Under hex naming these ids and tenants are no longer rejected — they
// are ordinary byte strings that round-trip and stay in their own directory.
func TestMessageStore_HostileTenantAndIDRoundTrip(t *testing.T) {
	f := NewStoreFactory()
	defer f.Close()

	hostile := []string{"../escape", "..", ".", "a/../../escape", `a\b`, "a:b"}
	for _, tenant := range hostile {
		for _, id := range hostile {
			ctx := ctxWithTenant(spi.TenantID(tenant))
			store, err := f.MessageStore(ctx)
			if err != nil {
				t.Fatalf("MessageStore(%q): %v", tenant, err)
			}
			payload := tenant + "|" + id
			if err := store.Save(ctx, id, spi.MessageHeader{}, spi.MessageMetaData{},
				strings.NewReader(payload)); err != nil {
				t.Fatalf("Save(%q,%q): %v", tenant, id, err)
			}
			_, _, rc, err := store.Get(ctx, id)
			if err != nil {
				t.Fatalf("Get(%q,%q): %v", tenant, id, err)
			}
			got, _ := io.ReadAll(rc)
			rc.Close()
			if string(got) != payload {
				t.Errorf("Get(%q,%q) = %q, want %q", tenant, id, got, payload)
			}
		}
	}

	// Nothing escaped: every regular file lives exactly two levels down.
	root := f.blobDir
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if depth := len(strings.Split(rel, string(filepath.Separator))); depth != 2 {
			t.Errorf("blob at depth %d: %q", depth, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}

// TestMessageStore_CaseOnlyTenantsStayDistinct is the regression this change
// exists for. Before hex naming, these two tenants shared a directory on any
// case-insensitive filesystem and the second Save overwrote the first.
func TestMessageStore_CaseOnlyTenantsStayDistinct(t *testing.T) {
	f := NewStoreFactory()
	defer f.Close()

	const id = "shared-id"
	for _, tc := range []struct{ tenant, payload string }{
		{"tenant-a", "payload-lower"},
		{"tenant-A", "payload-upper"},
	} {
		ctx := ctxWithTenant(spi.TenantID(tc.tenant))
		store, err := f.MessageStore(ctx)
		if err != nil {
			t.Fatalf("MessageStore(%q): %v", tc.tenant, err)
		}
		if err := store.Save(ctx, id, spi.MessageHeader{}, spi.MessageMetaData{},
			strings.NewReader(tc.payload)); err != nil {
			t.Fatalf("Save(%q): %v", tc.tenant, err)
		}
	}

	for _, tc := range []struct{ tenant, payload string }{
		{"tenant-a", "payload-lower"},
		{"tenant-A", "payload-upper"},
	} {
		ctx := ctxWithTenant(spi.TenantID(tc.tenant))
		store, err := f.MessageStore(ctx)
		if err != nil {
			t.Fatalf("MessageStore(%q): %v", tc.tenant, err)
		}
		_, _, rc, err := store.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get(%q): %v", tc.tenant, err)
		}
		got, _ := io.ReadAll(rc)
		rc.Close()
		if string(got) != tc.payload {
			t.Fatalf("tenant %q read %q, want %q — the two tenants share a blob",
				tc.tenant, got, tc.payload)
		}
	}
}

// TestMessageStore_EmptyTenantRefusedByFactory pins the invariant the blob
// layer relies on instead of re-checking it: hex("") is "", which would
// collapse a tenant directory into the blob root, so the factory must never
// hand out a store with an empty tenant. MessageStore is constructed in
// exactly one place (store_factory.go MessageStore), behind resolveTenant.
func TestMessageStore_EmptyTenantRefusedByFactory(t *testing.T) {
	f := NewStoreFactory()
	defer f.Close()

	if _, err := f.MessageStore(ctxWithTenant("")); err == nil {
		t.Fatal("MessageStore accepted an empty tenant")
	}
}

// TestMessageStore_ConcurrentSaveSameID asserts Save is atomic across its blob
// rename and its metadata insert: whichever writer wins, the payload the
// reader gets must be the one that writer wrote, never a mix.
func TestMessageStore_ConcurrentSaveSameID(t *testing.T) {
	f := NewStoreFactory()
	defer f.Close()

	ctx := ctxWithTenant("concurrent-tenant")
	store, err := f.MessageStore(ctx)
	if err != nil {
		t.Fatalf("MessageStore: %v", err)
	}

	const id = "contended"
	payloads := []string{strings.Repeat("A", 4096), strings.Repeat("B", 4096)}

	var wg sync.WaitGroup
	for _, p := range payloads {
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			if err := store.Save(ctx, id, spi.MessageHeader{Subject: p[:1]},
				spi.MessageMetaData{Values: map[string]any{"who": p[:1]}},
				strings.NewReader(p)); err != nil {
				t.Errorf("Save: %v", err)
			}
		}(p)
	}
	wg.Wait()

	header, meta, rc, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()

	winner := string(got[:1])
	if string(got) != strings.Repeat(winner, 4096) {
		t.Fatalf("payload is torn: starts %q but is not uniform", winner)
	}
	if header.Subject != winner {
		t.Errorf("header came from %q but the blob from %q", header.Subject, winner)
	}
	if meta.Values["who"] != winner {
		t.Errorf("metadata came from %v but the blob from %q", meta.Values["who"], winner)
	}
}
```

Add `io`, `io/fs`, `path/filepath`, `strings` and `sync` to that file's imports
as needed.

- [ ] **Step 7: Run the store test to verify it fails**

Run: `cd plugins/memory && go test ./... -run TestMessageStore_`
Expected: FAIL — `TestMessageStore_CaseOnlyTenantsStayDistinct` reports the two
tenants sharing a blob (on macOS), and the hostile round-trip fails because
`blobPath` still rejects those ids.

- [ ] **Step 8: Open the root in the factory**

In `plugins/memory/store_factory.go`, add the field beside `blobDir` (line 113):

```go
	blobDir        string
	// blobRoot confines every blob operation to blobDir at the OS level.
	// Encoding governs the name a blob can have; the root governs what that
	// name is allowed to resolve to, which is what a symlink planted under
	// blobDir would otherwise subvert.
	blobRoot       *os.Root
```

In `NewStoreFactory`, immediately after the existing `os.MkdirTemp` block:

```go
	blobRoot, err := os.OpenRoot(blobDir)
	if err != nil {
		panic(fmt.Sprintf("failed to open blob root: %v", err))
	}
```

and add `blobRoot: blobRoot,` to the struct literal beside `blobDir: blobDir,`.
The panic matches the existing failure mode two lines up — a factory that
cannot create its blob directory already panics.

Replace `Close`:

```go
func (f *StoreFactory) Close() error {
	// Close the root before removing the tree: on Windows an open handle
	// blocks the removal.
	if f.blobRoot != nil {
		if err := f.blobRoot.Close(); err != nil {
			return fmt.Errorf("failed to close blob root: %w", err)
		}
	}
	return os.RemoveAll(f.blobDir)
}
```

- [ ] **Step 9: Rewrite the store's I/O**

In `plugins/memory/message_store.go`, delete `blobPath` and `tenantBlobDir`
(lines 60-92 — the `tenantBlobDir` method on `*MessageStore`; the package-level
function of the same name in `blob_path.go` replaces it), and replace `Save`,
`Get` and `Delete`:

```go
func (s *MessageStore) Save(_ context.Context, id string, header spi.MessageHeader, metaData spi.MessageMetaData, payload io.Reader) error {
	f := s.factory
	root := f.blobRoot

	// Step 1: write the blob to a temp file OUTSIDE the lock.
	dir := tenantBlobDir(s.tenant)
	if err := root.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("failed to create tenant blob dir: %w", err)
	}

	tmpFile, tmpName, err := createTempBlob(root, dir)
	if err != nil {
		return err
	}

	if _, err := io.Copy(tmpFile, payload); err != nil {
		tmpFile.Close()
		root.Remove(tmpName)
		return fmt.Errorf("failed to write blob payload: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		root.Remove(tmpName)
		return fmt.Errorf("failed to close temp blob file: %w", err)
	}

	// Steps 2 and 3 share one critical section. The rename and the metadata
	// insert have to land together: two concurrent saves of the same id would
	// otherwise be free to interleave and leave one writer's blob paired with
	// the other's header. The payload copy above stays outside the lock, which
	// is what the split was for.
	f.msgMu.Lock()
	if err := root.Rename(tmpName, blobName(s.tenant, id)); err != nil {
		f.msgMu.Unlock()
		root.Remove(tmpName)
		return fmt.Errorf("failed to rename blob file: %w", err)
	}
	tenantMap := f.msgData[s.tenant]
	if tenantMap == nil {
		tenantMap = make(map[string]*messageEntry)
		f.msgData[s.tenant] = tenantMap
	}
	tenantMap[id] = &messageEntry{
		header:   header,
		metaData: copyMessageMetaData(metaData),
	}
	f.msgMu.Unlock()

	return nil
}

func (s *MessageStore) Get(_ context.Context, id string) (spi.MessageHeader, spi.MessageMetaData, io.ReadCloser, error) {
	f := s.factory

	f.msgMu.RLock()
	tenantMap := f.msgData[s.tenant]
	entry, ok := tenantMap[id]
	var header spi.MessageHeader
	var metaData spi.MessageMetaData
	if ok {
		header = entry.header
		metaData = copyMessageMetaData(entry.metaData)
	}
	f.msgMu.RUnlock()

	if !ok {
		return spi.MessageHeader{}, spi.MessageMetaData{}, nil, spi.ErrNotFound
	}

	file, err := f.blobRoot.Open(blobName(s.tenant, id))
	if err != nil {
		return spi.MessageHeader{}, spi.MessageMetaData{}, nil, fmt.Errorf("failed to open blob file: %w", err)
	}

	return header, metaData, &idempotentCloser{rc: file}, nil
}

func (s *MessageStore) Delete(_ context.Context, id string) error {
	f := s.factory

	f.msgMu.Lock()
	tenantMap := f.msgData[s.tenant]
	if tenantMap != nil {
		delete(tenantMap, id)
	}
	f.msgMu.Unlock()

	// Best-effort: the metadata removal above is what makes the message gone.
	f.blobRoot.Remove(blobName(s.tenant, id))

	return nil
}
```

Drop `path/filepath` and `strings` from the file's imports if nothing else
in it uses them; `os` is no longer needed either.

- [ ] **Step 10: Run the memory plugin's tests**

Run: `cd plugins/memory && go test ./...`
Expected: PASS, including the SPI conformance suite that runs in this package.

- [ ] **Step 11: Commit**

```bash
git add plugins/memory/
git commit -m "feat(memory): confine blobs at the OS level and drop the hand-rolled guard

os.Root governs what a blob name resolves to; hex naming governs what a
name can be. Together they make traversal and cross-tenant collision
unrepresentable, so blobPath's empty/dot/separator check has nothing left
to guard and goes.

Save's rename and metadata insert now share one critical section. They
were separate, so two concurrent saves of one id could leave one writer's
blob paired with the other's header. The payload copy stays outside the
lock, which is what the split was for.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 6: One dispatch request, one tenant

**Files:**
- Modify: `internal/cluster/dispatch/handler.go:50-56`
- Test: `internal/cluster/dispatch/handler_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: nothing. `handleCallout` gains a 400 branch.

- [ ] **Step 1: Write the failing test**

Append to `internal/cluster/dispatch/handler_test.go`, following whatever
request-construction and peer-auth helper the file already uses — read it
first:

```go
// TestHandleCallout_RejectsEntityMetaTenantMismatch closes a cross-tenant gap
// that has nothing to do with spelling. A dispatch request carries two
// tenants: TenantID, which becomes the UserContext the callout runs as, and
// EntityMeta.TenantID, which is handed to the local dispatcher as the entity's
// own. Nothing compared them, so a peer could run a callout as tenant B over
// tenant A's entity.
func TestHandleCallout_RejectsEntityMetaTenantMismatch(t *testing.T) {
	h, srv := newTestDispatchHandler(t) // existing helper in this file
	defer srv.Close()

	body := DispatchCalloutRequest{
		Kind:     "processor",
		TenantID: "tenant-a",
		UserID:   "user-1",
		EntityMeta: spi.EntityMeta{
			ID:       "entity-1",
			TenantID: "tenant-b", // disagrees with TenantID
		},
	}
	resp := h.postCallout(t, body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(raw), "tenant-a") || strings.Contains(string(raw), "tenant-b") {
		t.Errorf("response echoes a tenant id: %s", raw)
	}
}

// TestHandleCallout_AcceptsAbsentEntityMetaTenant keeps the check from
// breaking the requests that carry no entity at all — criteria and function
// callouts leave EntityMeta zero-valued.
func TestHandleCallout_AcceptsAbsentEntityMetaTenant(t *testing.T) {
	h, srv := newTestDispatchHandler(t)
	defer srv.Close()

	body := DispatchCalloutRequest{
		Kind:       "processor",
		TenantID:   "tenant-a",
		UserID:     "user-1",
		EntityMeta: spi.EntityMeta{ID: "entity-1"}, // TenantID empty
	}
	resp := h.postCallout(t, body)
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusBadRequest {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("an absent EntityMeta tenant was rejected: %s", raw)
	}
}
```

If `newTestDispatchHandler` / `postCallout` do not exist, write the two tests
against whatever the file does provide; do not invent a helper the package
lacks.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/cluster/dispatch/ -run TestHandleCallout_Rejects`
Expected: FAIL — status is not 400.

- [ ] **Step 3: Write the implementation**

In `internal/cluster/dispatch/handler.go`, after the `json.Unmarshal` block
(line 50-53) and before `buildContext`:

```go
	// A dispatch request carries two tenants: TenantID, which becomes the
	// UserContext this callout runs as, and EntityMeta.TenantID, which is
	// handed to the local dispatcher as the entity's own. They must agree, or
	// the callout runs as one tenant over another's entity. An empty
	// EntityMeta.TenantID is the criteria and function shape, which carries no
	// entity — those are unconstrained here.
	//
	// The response names neither value: both are peer-supplied.
	if req.EntityMeta.TenantID != "" && string(req.EntityMeta.TenantID) != req.TenantID {
		http.Error(w, "entity tenant does not match request tenant", http.StatusBadRequest)
		return
	}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/cluster/dispatch/`
Expected: PASS. If an existing test builds a request whose `EntityMeta` tenant
disagrees, fix the fixture — that fixture was encoding the bug.

- [ ] **Step 5: Commit**

```bash
git add internal/cluster/dispatch/
git commit -m "fix(dispatch): a callout may not run as one tenant over another's entity

DispatchCalloutRequest carries two tenants — its own, which becomes the
UserContext, and EntityMeta.TenantID, handed verbatim to the local
dispatcher — and nothing compared them. A criteria or function callout
carries no entity and leaves the field empty, which stays unconstrained.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 7: The OIDC store keys by UUID, not by spelling (#587)

**Files:**
- Modify: `internal/domain/account/oidc_adapter.go:148,207,238,311,345,369`
- Test: `internal/domain/account/oidc_adapter_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces (package-private): `func oidcTenantFromCtx(w http.ResponseWriter, r *http.Request) (spi.TenantID, uuid.UUID, bool)` — returns the caller's tenant in canonical UUID spelling plus the parsed UUID; on failure it has already written `400 OIDC_INVALID_TENANT` and returns `ok == false`.

- [ ] **Step 1: Write the failing test**

Append to `internal/domain/account/oidc_adapter_test.go`, using the harness the
file already has for driving the adapter:

```go
// TestOidcAdapter_NonCanonicalUUIDTenantRoundTrips is the #587 regression.
// Register keyed the provider blob by uuid.UUID.String() — always canonical
// lowercase — while Get, GetByURI, Delete and ListByTenant keyed by the
// caller's raw tenant. uuid.Parse accepts uppercase, braced and urn:uuid:
// forms, so such a tenant registered a provider it could never reach again.
func TestOidcAdapter_NonCanonicalUUIDTenantRoundTrips(t *testing.T) {
	const canonical = "1a2b3c4d-5e6f-4a8b-9c0d-1e2f3a4b5c6d"
	for name, tenant := range map[string]string{
		"uppercase": strings.ToUpper(canonical),
		"braced":    "{" + canonical + "}",
		"urn":       "urn:uuid:" + canonical,
		"canonical": canonical,
	} {
		t.Run(name, func(t *testing.T) {
			h := newOidcAdapterHarness(t) // existing helper in this file

			created := h.register(t, tenant, "https://idp.example/.well-known/openid-configuration")

			listed := h.list(t, tenant)
			if len(listed) != 1 {
				t.Fatalf("list returned %d providers, want 1 — the blob is stranded", len(listed))
			}
			if got := h.get(t, tenant, created.Id.String()); got == nil {
				t.Fatal("get returned nothing — the blob is stranded")
			}
			h.delete(t, tenant, created.Id.String())
			if remaining := h.list(t, tenant); len(remaining) != 0 {
				t.Fatalf("list returned %d providers after delete, want 0", len(remaining))
			}
		})
	}
}

// TestOidcAdapter_NonUUIDTenantIsRejectedEverywhere records the behaviour
// change. A non-UUID tenant such as default-tenant used to receive an empty
// 200 from the list endpoint, because its prefix scan matched nothing — a
// success implying a registration that could never have happened. It now
// receives the same 400 registration already gave it.
func TestOidcAdapter_NonUUIDTenantIsRejectedEverywhere(t *testing.T) {
	h := newOidcAdapterHarness(t)

	for name, call := range map[string]func() int{
		"list":       func() int { return h.listStatus(t, "default-tenant") },
		"get":        func() int { return h.getStatus(t, "default-tenant", uuid.NewString()) },
		"delete":     func() int { return h.deleteStatus(t, "default-tenant", uuid.NewString()) },
		"invalidate": func() int { return h.invalidateStatus(t, "default-tenant", uuid.NewString()) },
		"reactivate": func() int { return h.reactivateStatus(t, "default-tenant", uuid.NewString()) },
	} {
		t.Run(name, func(t *testing.T) {
			if got := call(); got != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 OIDC_INVALID_TENANT", got)
			}
		})
	}
}
```

Read the file first and build these against its existing helpers. If it has no
harness, drive the adapter the way its existing tests do and add only the
helpers those tests are missing.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/domain/account/ -run TestOidcAdapter_`
Expected: FAIL — the uppercase, braced and urn subtests report a stranded blob;
the non-UUID subtests report 200 where 400 is wanted.

- [ ] **Step 3: Write the implementation**

In `internal/domain/account/oidc_adapter.go`, add the helper near the top:

```go
// oidcTenantFromCtx returns the caller's tenant in the canonical UUID spelling
// the provider store keys by, plus the parsed UUID for the owner field.
//
// This exists because uuid.Parse accepts spellings that String() normalises
// away — uppercase, braced, and urn:uuid: prefixed. Register keys the provider
// blob and the URI index by OwnerLegalEntityID.String(), so a caller whose
// caas_org_id is spelled any other way would address a different key on every
// subsequent read, update or delete, and strand the provider it just created.
//
// On failure the response has already been written and ok is false.
func oidcTenantFromCtx(w http.ResponseWriter, r *http.Request) (spi.TenantID, uuid.UUID, bool) {
	raw := tenantFromCtx(r)
	owner, err := uuid.Parse(string(raw))
	if err != nil {
		common.WriteError(w, r, common.Operational(
			http.StatusBadRequest,
			common.ErrCodeOidcInvalidTenant,
			"oidc provider operations require a uuid-shaped tenant identifier; bootstrap deployments using the literal 'default-tenant' string must migrate to real tenant uuids",
		))
		return "", uuid.Nil, false
	}
	return spi.TenantID(owner.String()), owner, true
}
```

Then replace each tenant read:

- `RegisterOidcProvider` (`:148-164`) — delete the inline `tenantID := tenantFromCtx(r)` and the `uuid.Parse` block, and use:
  ```go
  	tenantID, ownerID, ok := oidcTenantFromCtx(w, r)
  	if !ok {
  		return
  	}
  ```
  `in.TenantID` then receives the canonical `tenantID` rather than the raw claim, and `in.OwnerLegalEntityID` receives `ownerID`.
- `ListOidcProviders` (`:201-207`) — keep the `uc == nil` 401 check, then replace `tenantID := spi.TenantID(uc.Tenant.ID)` with:
  ```go
  	tenantID, _, ok := oidcTenantFromCtx(w, r)
  	if !ok {
  		return
  	}
  ```
- `UpdateOidcProvider` (`:238`), `InvalidateOidcProvider` (`:311`), `ReactivateOidcProvider` (`:345`), `DeleteOidcProvider` (`:369`) — each replaces its `tenantFromCtx(r)` with the same three-line form, hoisted above the point of use.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/domain/account/`
Expected: PASS.

- [ ] **Step 5: Add the E2E cell**

Append to `internal/e2e/oauth_keys_test.go` a test that registers a provider as
an uppercase-UUID tenant using `mintTokenWithTenant` from Task 4 and asserts
`GET /oauth/oidc/providers` returns it. Follow that file's existing OIDC
request helpers.

Run: `go test ./internal/e2e/ -run TestOidc`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/domain/account/ internal/e2e/oauth_keys_test.go
git commit -m "fix(oidc): the provider store keyed by spelling, not by identity

Register keyed the blob and the URI index by OwnerLegalEntityID.String() —
always canonical lowercase — while Get, GetByURI, Delete, ListByTenant and
RaceValidateIndex keyed by the caller's raw tenant. uuid.Parse accepts
uppercase, braced and urn:uuid: forms that String() normalises away, so such
a tenant registered a provider it could then never list, read, update or
delete. Authentication was unaffected: the registry and kid index are built
from the stored provider's own owner id.

Canonicalising at the adapter, the only entry point to the OIDC service, is
what makes every operation address the same key.

A non-UUID tenant now gets 400 OIDC_INVALID_TENANT from every OIDC endpoint
rather than an empty 200 from the list one, which implied a registration that
could never have succeeded.

Closes #587

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 8: A token-endpoint 500 carries a ticket (#588)

**Files:**
- Modify: `internal/auth/token.go:78,97,211,230,272-284`
- Test: `internal/auth/token_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces (package-private): `func writeTokenServerError(w http.ResponseWriter, op string, cause error)` — mints a ticket, logs it with the cause, and writes `500` with `error: "server_error"` and `error_description: "server_error [ticket: <uuid>]"`.

- [ ] **Step 1: Write the failing test**

Append to `internal/auth/token_test.go`:

```go
// TestTokenEndpoint_ServerErrorCarriesTicket pins Gate 3 for this endpoint:
// every 5xx carries a generic message plus a ticket UUID and no internals.
// The OAuth2 body shape has no dedicated field, so the ticket rides in
// error_description, which the schema already declares as a string.
func TestTokenEndpoint_ServerErrorCarriesTicket(t *testing.T) {
	// A key store whose GetActive always fails drives the server_error path.
	h := NewTokenHandler(
		failingKeyStore{err: errors.New("hsm unreachable at 10.0.0.5:8443")},
		nil,
		newTestM2MStore(t), // existing helper in this file
		"cyoda-test",
		3600,
	)

	req := httptest.NewRequest(http.MethodPost, "/oauth/token",
		strings.NewReader("grant_type=client_credentials"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("testclient", "testsecret")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["error"] != "server_error" {
		t.Errorf("error = %q, want server_error", body["error"])
	}
	desc := body["error_description"]
	if !strings.HasPrefix(desc, "server_error [ticket: ") {
		t.Fatalf("error_description = %q, want a ticket", desc)
	}
	ticket := strings.TrimSuffix(strings.TrimPrefix(desc, "server_error [ticket: "), "]")
	if _, err := uuid.Parse(ticket); err != nil {
		t.Errorf("ticket %q is not a uuid: %v", ticket, err)
	}
	if strings.Contains(rec.Body.String(), "hsm") || strings.Contains(rec.Body.String(), "10.0.0.5") {
		t.Errorf("response leaks the underlying cause: %s", rec.Body.String())
	}
}

// failingKeyStore is a KeyStore whose GetActive always fails.
type failingKeyStore struct{ err error }

func (f failingKeyStore) GetActive(string) (*KeyPair, error) { return nil, f.err }
```

Complete `failingKeyStore` with whatever other methods `KeyStore` declares
(`internal/auth/store.go:66`), each returning `f.err` or a zero value. Reuse
the file's existing M2M store helper rather than adding one.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/auth/ -run TestTokenEndpoint_ServerErrorCarriesTicket`
Expected: FAIL — `error_description = "server_error", want a ticket`.

- [ ] **Step 3: Write the implementation**

In `internal/auth/token.go`, add:

```go
// writeTokenServerError answers a 500 on the OAuth2-shaped token endpoint.
//
// Gate 3 requires every 5xx to carry a generic message plus a ticket UUID and
// no internals. RFC 6749 §5.2 fixes this endpoint's body shape, which has no
// dedicated field, so the ticket rides in error_description — already a
// declared string in the schema. The cause goes to the log under the same
// ticket and never into the response, matching the LevelInternal rendering in
// internal/common/errors.go.
func writeTokenServerError(w http.ResponseWriter, op string, cause error) {
	ticket := uuid.NewString()
	slog.Error("internal error",
		"pkg", "auth",
		"ticket", ticket,
		"op", op,
		"cause", cause,
	)
	writeTokenError(w, http.StatusInternalServerError, "server_error",
		fmt.Sprintf("server_error [ticket: %s]", ticket))
}
```

Replace each of the four `server_error` sites:

- `token.go:78` → `writeTokenServerError(w, "keyStore.GetActive", err)`
- `token.go:97` → `writeTokenServerError(w, "Sign", err)`
- `token.go:211` → `writeTokenServerError(w, "keyStore.GetActive", err)`
- `token.go:230` → `writeTokenServerError(w, "Sign", err)`

Add `fmt` and `log/slog` to the file's imports; `uuid` is already imported.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/auth/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/auth/token.go internal/auth/token_test.go
git commit -m "fix(auth): a token-endpoint 500 carries a ticket

Gate 3 asks every 5xx for a generic message plus a ticket UUID and no
internals. This endpoint's body shape is fixed by RFC 6749 and has no
dedicated field, so the ticket rides in error_description, already a
declared string. The cause goes to the log under the same ticket, matching
the LevelInternal rendering in internal/common/errors.go.

Closes #588

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 9: Documentation (Gate 4 and Gate 7)

**Files:**
- Create: `docs/cloud-parity/tenant-id-grammar.md`
- Modify: `CHANGELOG.md`, `cmd/cyoda/help/content/config/auth.md`, `cmd/cyoda/help/config_registry.go:101`, `README.md`, `docs/ARCHITECTURE.md:1588`

**Interfaces:**
- Consumes: Tasks 1–8. Nothing consumes this.

**No `errors/<CODE>.md` task:** this change introduces no error code. Every
rejection reuses `UNAUTHORIZED`, `OIDC_INVALID_TENANT`, a plain 400, or an
OAuth2 `server_error`. `TestErrCode_Parity` needs nothing.

- [ ] **Step 1: Write the cloud-parity record**

Create `docs/cloud-parity/tenant-id-grammar.md` covering: the grammar verbatim;
that cyoda-go defines it and Cloud has no constraint at all today, so a
Cloud-issued token outside it becomes a silent 401 rather than a diagnosable
rejection; the evidence for the charset (Cloud's `EdgeMessageController`
`@Pattern`) and for the 100-byte cap (`CSUser.legalEntityId`
`@ColumnValidation(maxLength = 100)`); the Cloud-side action item tracked as
**CP-3968**, including its ask to check live legal entities against the
grammar; and the carve-out that Cloud's audit sentinel
`AuditActorInfoDto(legalId = "-")` fails the first-character rule and does not
reach `caas_org_id`. Follow the shape of a neighbouring file in that directory.

- [ ] **Step 2: Update the CHANGELOG**

Under `## [Unreleased]`, add to `### Breaking` (line 126) an entry for the
tenant grammar — the accepted set, the two doors, that a token outside it is an
ordinary 401 and a bad `CYODA_BOOTSTRAP_TENANT_ID` refuses to start, and that
non-UUID tenants now get 400 from every OIDC endpoint rather than an empty 200
from the list one. Add to `### Fixed` the memory backend's case-collision blob
sharing, `Save`'s non-atomic rename, the dispatch `EntityMeta` tenant mismatch,
#587 and #588. Cross-reference `docs/cloud-parity/tenant-id-grammar.md`.

- [ ] **Step 3: Update the config documentation**

`cmd/cyoda/help/content/config/auth.md`, `cmd/cyoda/help/config_registry.go:101`,
`README.md` and `docs/ARCHITECTURE.md:1588` each mention
`CYODA_BOOTSTRAP_TENANT_ID` or `default-tenant`. State the accepted set at each
and that an invalid value prevents startup when a bootstrap client is
configured. Keep `docs/ARCHITECTURE.md` in the present tense and audit the
surrounding passage while you are in it.

- [ ] **Step 4: Verify the help tree still builds**

Run: `go test ./cmd/cyoda/help/`
Expected: PASS — including `TestErrCode_Parity`, which should be unaffected.

- [ ] **Step 5: Commit**

```bash
git add docs/ CHANGELOG.md README.md cmd/cyoda/help/
git commit -m "docs: record the tenant-id grammar as a contract Cloud must follow

cyoda-go defines the grammar; Cloud constrains caas_org_id nowhere, so a
Cloud-issued token outside it is a silent 401 rather than a diagnosable
rejection. CP-3968 carries the Cloud-side half, including the one question
the source cannot answer: whether any live legal entity is already outside
the grammar.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 10: Verification (Gate 5)

**Files:** none.

- [ ] **Step 1: Static analysis**

Run: `go vet ./...`
Then, per module: `cd plugins/memory && go vet ./...`, and the same in
`plugins/sqlite` and `plugins/postgres`.
Expected: clean.

- [ ] **Step 2: Full suite**

Run: `make test-full`
Expected: green across the root module and all three plugin submodules,
including `internal/e2e`. Docker must be running; `make preflight` first if it
is not obviously healthy. Do not narrow the run if it fails — fix the failure.

- [ ] **Step 3: Race detector**

Run: `make race`
Expected: clean. Then the E2E carve-out separately:
`go test -race -timeout=20m ./internal/e2e/...`
This matters more than usual here — Task 5 moved `Save`'s rename inside
`msgMu`.

- [ ] **Step 4: Confirm the tenant is gone from the blob path**

Run: `grep -rn "filepath.Join" plugins/memory/`
Expected: no hit that joins a tenant or an id. The only path construction left
should be hex-based, in `blob_path.go`.

- [ ] **Step 5: Re-check CodeQL alert 92**

The alert (`go/path-injection`, high) points at `plugins/memory/message_store.go`.
After this change no path is built from unencoded data there. Re-run the scan
or wait for CI. If it still reports, dismiss it as a **false positive** — never
as "unused". The memory backend ships in the binary, is a documented
`CYODA_STORAGE_BACKEND` value, and is what a binary run with no configuration
uses; recording it as unused would be a false audit record.

- [ ] **Step 6: Commit any fixes and report**

Report the actual command output for `make test-full`, `make race` and
`go vet ./...`. Do not claim completion without it.

---

## Self-Review

**Spec coverage.** Grammar → Task 1. Door 1 (HTTP + gRPC) → Task 2, e2e in
Task 4. Door 2 → Task 3. "No check at the token endpoint / OIDC usercontext" →
no task, by design. Encoding + `os.Root` + `Save` atomicity → Task 5.
`EntityMeta` mismatch → Task 6. #587 → Task 7. #588 → Task 8. Docs and
cloud-parity → Task 9. Gate 5 → Task 10.

**Two deviations from the spec, both deliberate.** The bootstrap row's
"subprocess E2E" is a direct `validateBootstrapConfig` test instead (Task 3,
with the reasoning recorded there) — `app.New` returning the error is the
behaviour; `main` exiting non-zero on a non-nil error is Go. And the spec's
"rejected tenant never reaches a log field or a response body" row is covered
by `TestValidateTenantID_ErrorNeverEchoesValue` (Task 1) plus the body
assertions in Tasks 2, 4 and 6 rather than by a test of its own.

**Parity waiver, carried forward from the spec:** no cross-backend scenario for
the hostile-claim 401, because the rejection happens at the authenticator and
never reaches a storage backend. No `wantParityScenarioCount` bump is needed,
since this plan adds no parity scenario. The existing message-store parity
scenarios and the SPI conformance suite both exercise the rewritten store
unchanged.

**Type consistency.** `ValidateTenantID(spi.TenantID) error` and
`ErrInvalidTenantID` are used with those exact names in Tasks 2 and 3.
`blobName`/`tenantBlobDir`/`createTempBlob` are defined in Task 5 Step 3 and
used in Step 9 of the same task. `mintTokenWithTenant` is defined in Task 4 and
reused in Task 7. `oidcTenantFromCtx` and `writeTokenServerError` are each
defined and used within one task.

**Task independence.** Tasks 5, 6, 7 and 8 touch disjoint files and depend on
nothing from Tasks 1–4, so they can run concurrently with each other and with
the door work. Task 4 depends on Tasks 2 and 3; Task 9 depends on all; Task 10
is last.
