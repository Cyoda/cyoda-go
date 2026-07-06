# OpenAPI auth / OIDC reconciliation (group 3, §6E) — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Reconcile the auth/OIDC/keys/trusted/token surface of `api/openapi.yaml` with the actual Go server — convert the OIDC error envelope to `ProblemDetail`, fix one duplicate-status runtime bug, retype `activeOnly`, document the config-conditional 501 surface and the emitted-but-undocumented error codes, and consolidate a duplicate schema.

**Architecture:** Spec-first reconciliation (ADR 0003). Most tasks edit `api/openapi.yaml` and prove the server already matches via producing e2e tests + the `openapivalidator` conformance suite (ModeEnforce). Two tasks change runtime code (409 duplicate, `activeOnly` boolean). Breaking-but-correct spec edits are allowed through surgical `.github/oasdiff-err-ignore.txt` entries. The `501`/`404 FEATURE_DISABLED` paths are unproducible on the shared jwt e2e harness, so two dedicated `httptest` fixtures are built.

**Tech Stack:** Go 1.26, `oapi-codegen` v2.7.0 (`go generate ./api/...`), kin-openapi validator, testcontainers Postgres e2e, oasdiff CI gate.

## Global Constraints

- **Full coverage** (`.claude/rules/test-coverage.md`): every documented status/error code in the spec's §8 table → a running-backend test; HTTP-only surface (gRPC n/a — no entry point); no cross-backend parity scenario (IAM-subsystem behaviour, not storage-backend). Concurrency tests, if any, isolated single-backend.
- **Typed-but-OPEN** ([policy](../../analysis/openapi/schema-strictness-research.md)): never `additionalProperties: false`; enumerate properties, keep open.
- **No new error codes**: every code touched (the ~16-code set in the spec §1) already exists and already has a `cmd/cyoda/help/content/errors/<CODE>.md` topic — **no `errors/<CODE>.md` additions**; `TestErrCode_Parity` unaffected. Re-confirm each topic exists before adding its producing test.
- **No issue IDs in shipped artefacts** (spec bodies, YAML descriptions, help topics, code comments). Issue refs only in commits/PR/spec docs.
- **Never log/echo secrets**; private keys never marshalled into responses (server already compliant — preserve).
- **Regen discipline**: after any `api/openapi.yaml` edit that changes generated types, `go generate ./api/...` must be re-run; `make check-codegen` is the gate. Task 12 is the final sync check.
- **Canonical error envelope**: converted ops use bare `ProblemDetail` (`api/openapi.yaml` schema at ~line 8264), `application/problem+json`, machine code under `properties.errorCode`. Mirror the `getStateMachineFinishedEvent` precedent from #373.
- **Spec reference**: `docs/superpowers/specs/2026-07-05-openapi-oidc-auth-design.md` — §8 error table is the checklist; §9 the coverage matrix; §11 the oasdiff dispositions.

**e2e harness vocabulary** (reuse — do not reinvent):
- `internal/e2e/helpers_test.go`: `e2eNewRequest(t, method, urlStr, body)`, `getToken(t, clientID, clientSecret)`, `authRequest(t, method, path, body)`.
- `internal/e2e/oauth_keys_test.go`: `adminRequest(t, method, path, body []byte) *http.Response` (bootstrap admin `testclient`/`testsecret`), `createM2MClient(t, tenantID, userID, roles) (clientID, clientSecret)`.
- `internal/e2e/clients_test.go`: `statusForToken(t, clientID, clientSecret) int`.
- `internal/e2e/oidc_providers_test.go`: `const oidcTenantUUID`, plus an `init()` that relaxes OIDC hardening **process-wide** (`CYODA_OIDC_REQUIRE_HTTPS=false`, `CYODA_OIDC_ALLOW_PRIVATE_NETWORKS=true`) — so `OIDC_SSRF_BLOCKED` is NOT producible on the shared harness (see Task 8).
- Fixture precedent for a bespoke-config server: `internal/e2e/cors_e2e_test.go:22-23` (`app.New(cfg)` + `httptest.NewServer(a.Handler())`), `internal/e2e/callback_harness_test.go:187`.
- Shared harness config (`internal/e2e/e2e_test.go` `TestMain`): `IAM.Mode="jwt"` (L123), `TrustedKeyRegistrationEnabled=true` (L136), `M2MAdminRoleEnabled=true` (L137), bootstrap tenant `"test-tenant"` (L130).

**Canonical endpoint paths (CRITICAL — all mounted under `/api`).** The spec's paths are relative; the server mounts the whole surface under `/api` (confirmed: `getToken` posts to `serverURL+"/api/oauth/token"`; `adminRequest` builds `serverURL+"/api"+path`). URL-construction rules:
- `adminRequest(t, method, path, body)` **prepends `/api` itself** — pass the spec path WITHOUT `/api` (e.g. `adminRequest(t,"POST","/oauth/keys/keypair",body)`).
- `authRequest(t, method, path, body)` and hand-built `serverURL+…` do **NOT** prepend — include the full `/api/…` prefix.
- Bespoke fixtures (F-a/F-b/hardened) serve `a.Handler()` with the same `/api` mount.

| operationId | full URL (hand-built / authRequest) | adminRequest path |
|---|---|---|
| listOidcProviders / registerOidcProvider | `/api/oauth/oidc/providers` | `/oauth/oidc/providers` |
| delete/update/invalidate/reactivateOidcProvider | `/api/oauth/oidc/providers/{id}` | `/oauth/oidc/providers/{id}` |
| reloadOidcProviders | `/api/oauth/oidc/providers/reload` (confirm) | `/oauth/oidc/providers/reload` |
| getTechnicalUserToken | `/api/oauth/token` | n/a (form POST, basic-auth) |
| issueJwtKeyPair | `/api/oauth/keys/keypair` | `/oauth/keys/keypair` |
| getCurrentJwtKeyPair | `/api/oauth/keys/keypair/current` | `/oauth/keys/keypair/current` |
| invalidateJwtKeyPair | `/api/oauth/keys/keypair/{keyId}/invalidate` | `/oauth/keys/keypair/{keyId}/invalidate` |
| registerTrustedKey / listTrustedKeys | `/api/oauth/keys/trusted` | `/oauth/keys/trusted` |
| delete/invalidateTrustedKey | `/api/oauth/keys/trusted/{keyId}[/invalidate]` | `/oauth/keys/trusted/{keyId}[/invalidate]` |
| create/listTechnicalUsers | `/api/clients` | `/clients` |
| delete/resetTechnicalUserSecret | `/api/clients/{clientId}[/secret]` | `/clients/{clientId}[/secret]` |
| searchEntityAuditEvents | `/api/audit/entity/{entityId}` | `/audit/entity/{entityId}` |

Confirm the exact `{id}`/reactivate/reload sub-path spellings against `api/openapi.yaml` when finalizing each task (the table covers the base routes; some lifecycle verbs use a suffix segment).

**oasdiff path note:** oasdiff reads the **spec** paths (no `/api` mount — the spec `servers` block has no base path). So `.github/oasdiff-err-ignore.txt` entries use the bare spec path (`/oauth/oidc/providers`, `/audit/entity/{entityId}`), NOT the `/api/…` test URL. The example entries below already reflect this.

**Commit convention:** end messages with `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>`. No issue IDs in code/YAML; PR body carries `#369`.

---

## Task ordering & dependency notes

Runtime/code first (1–2), then the spec doc sweeps that prove-server-already-right (3–8), then the fixtures + gated coverage (9), schema consolidation (10), then regen/oasdiff/docs/verify (11–14). Only Task 2 needs an in-task regen (the adapter reads the regenerated `*bool`); all other yaml edits defer to the Task 12 final regen. Producing tests parse HTTP JSON directly (not generated client types), so transient `generated.go` staleness never breaks them.

---

### Task 1: Runtime — `registerOidcProvider` duplicate `400 → 409` (design area C1)

**Files:**
- Modify: `internal/domain/account/oidc_adapter.go:176-179`
- Test: `internal/e2e/oidc_reconciliation_test.go` (create)

**Interfaces:**
- Consumes: `createM2MClient`, `adminRequest`/`authRequest`, `oidcTenantUUID`, `e2eNewRequest`.
- Produces: `internal/e2e/oidc_reconciliation_test.go` (new file reused by Tasks 3, 4).

- [ ] **Step 1: Write the failing test.** In a new file `internal/e2e/oidc_reconciliation_test.go`:

```go
package e2e_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
)

// registerOIDCProvider POSTs a minimal provider under the UUID tenant used by
// the OIDC lifecycle tests, driven by an admin token for that tenant.
func registerOIDCProvider(t *testing.T, token, name string) *http.Response {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"name":         name,
		"issuerUri":    "http://oidc.example.test/" + name,
		"clientId":     "cid-" + name,
		"clientSecret": "secret",
	})
	req, err := e2eNewRequest(t, "POST", serverURL+"/api/oauth/oidc/providers", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return resp
}

func TestOIDC_RegisterDuplicate_Returns409(t *testing.T) {
	cid, secret := createM2MClient(t, oidcTenantUUID, "dup-user", []string{"ROLE_ADMIN", "ROLE_M2M"})
	token := getToken(t, cid, secret)

	first := registerOIDCProvider(t, token, "dupprov")
	io.Copy(io.Discard, first.Body)
	first.Body.Close()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first register: got %d, want 200", first.StatusCode)
	}

	dup := registerOIDCProvider(t, token, "dupprov")
	defer dup.Body.Close()
	if dup.StatusCode != http.StatusConflict {
		raw, _ := io.ReadAll(dup.Body)
		t.Fatalf("duplicate register: got %d, want 409; body=%s", dup.StatusCode, raw)
	}
	// The machine code lives under properties.errorCode (ProblemDetail).
	var pd struct {
		Properties map[string]any `json:"properties"`
	}
	raw, _ := io.ReadAll(dup.Body)
	_ = json.Unmarshal(raw, &pd)
	if got := fmt.Sprintf("%v", pd.Properties["errorCode"]); got != "OIDC_PROVIDER_DUPLICATE" {
		t.Fatalf("errorCode: got %q, want OIDC_PROVIDER_DUPLICATE; body=%s", got, raw)
	}
}
```

- [ ] **Step 2: Run test to verify it fails.**

Run: `go test ./internal/e2e/ -run TestOIDC_RegisterDuplicate_Returns409 -v`
Expected: FAIL — `got 400, want 409` (server currently returns `400`).

- [ ] **Step 3: Change the status.** In `internal/domain/account/oidc_adapter.go`, in the `RegisterOidcProvider` duplicate branch (~line 176-179), change `http.StatusBadRequest` to `http.StatusConflict`:

```go
	if errors.Is(err, oidc.ErrProviderDuplicate) {
		common.WriteError(w, r, common.Operational(http.StatusConflict, common.ErrCodeOIDCProviderDuplicate,
			"OIDC provider with this name already exists"))
		return
	}
```

(Keep the exact existing message string; only the status constant changes.)

- [ ] **Step 4: Run test to verify it passes.**

Run: `go test ./internal/e2e/ -run TestOIDC_RegisterDuplicate_Returns409 -v`
Expected: PASS.

- [ ] **Step 5: Confirm the spec already documents 409.** `grep -n "'409'" api/openapi.yaml` near the `registerOidcProvider` block (~line 5505) — the `409` response already exists (it becomes `ProblemDetail` in Task 4). No yaml edit here.

- [ ] **Step 6: Commit.**

```bash
git add internal/domain/account/oidc_adapter.go internal/e2e/oidc_reconciliation_test.go
git commit -m "fix(oidc): registerOidcProvider duplicate returns 409 not 400

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 2: Runtime — `listOidcProviders.activeOnly` string → boolean (design area C3)

**Files:**
- Modify: `api/openapi.yaml` (the `activeOnly` query param on `listOidcProviders`, ~line 5420-5440)
- Modify: `api/generated.go` (regen)
- Modify: `internal/domain/account/oidc_adapter.go:209-214`
- Test: `internal/e2e/oidc_reconciliation_test.go`

**Interfaces:**
- Consumes: regenerated `ListOidcProvidersParams.ActiveOnly *bool`.
- Produces: adapter reads `*bool` directly.

- [ ] **Step 1: Write the failing tests.** Append to `internal/e2e/oidc_reconciliation_test.go`:

```go
func TestOIDC_ActiveOnly_BooleanFilter(t *testing.T) {
	cid, secret := createM2MClient(t, oidcTenantUUID, "ao-user", []string{"ROLE_ADMIN", "ROLE_M2M"})
	token := getToken(t, cid, secret)

	// Truthy "1" must now filter (previously silently false under string=="true").
	req, _ := e2eNewRequest(t, "GET", serverURL+"/api/oauth/oidc/providers?activeOnly=1", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("activeOnly=1: got %d, want 200", resp.StatusCode)
	}
}

func TestOIDC_ActiveOnly_GarbageReturns400(t *testing.T) {
	cid, secret := createM2MClient(t, oidcTenantUUID, "ao-bad-user", []string{"ROLE_ADMIN", "ROLE_M2M"})
	token := getToken(t, cid, secret)

	req, _ := e2eNewRequest(t, "GET", serverURL+"/api/oauth/oidc/providers?activeOnly=yes", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("activeOnly=yes: got %d, want 400 (ParseBool rejects)", resp.StatusCode)
	}
}
```

- [ ] **Step 2: Run to verify failure.**

Run: `go test ./internal/e2e/ -run 'TestOIDC_ActiveOnly' -v`
Expected: `GarbageReturns400` FAILs (currently `?activeOnly=yes` → 200, silent-false).

- [ ] **Step 3: Retype the param in the spec.** In `api/openapi.yaml`, the `activeOnly` parameter on `listOidcProviders`, change `schema: { type: string }` to `type: boolean`:

```yaml
        - name: activeOnly
          in: query
          required: false
          description: When true, return only active (non-invalidated) providers.
          schema:
            type: boolean
```

- [ ] **Step 4: Regenerate the client types.**

Run: `go generate ./api/...`
Expected: `api/generated.go` now declares `ActiveOnly *bool` on `ListOidcProvidersParams`.

- [ ] **Step 5: Update the adapter to read `*bool`.** In `internal/domain/account/oidc_adapter.go` (~line 209-214), replace the string compare:

```go
	activeOnly := params.ActiveOnly != nil && *params.ActiveOnly
	list, err := a.service.ListByTenant(ctx, tenantID, activeOnly)
```

- [ ] **Step 6: Vet + run tests.**

Run: `go vet ./internal/domain/account/... && go test ./internal/e2e/ -run 'TestOIDC_ActiveOnly' -v`
Expected: both PASS.

- [ ] **Step 7: Capture the oasdiff entry.** Run the oasdiff gate locally (see Task 11 for the exact invocation) to capture the precise breaking-change line for the param type change, then append it to `.github/oasdiff-err-ignore.txt` under a new commented block:

```
# auth/OIDC slice — listOidcProviders.activeOnly retyped string→boolean: the
# param was always a boolean-in-intent (server compared string=="true"); typing
# it correctly is the reconciliation. oasdiff flags the request param type change.
<PASTE EXACT oasdiff singleline here, e.g.:>
error at api/openapi.yaml, in API GET /oauth/oidc/providers the parameter 'activeOnly' type/format changed from 'string'/'' to 'boolean'/'' [request-parameter-type-changed].
```

- [ ] **Step 8: Commit.**

```bash
git add api/openapi.yaml api/generated.go internal/domain/account/oidc_adapter.go internal/e2e/oidc_reconciliation_test.go .github/oasdiff-err-ignore.txt
git commit -m "feat(oidc): type listOidcProviders.activeOnly as boolean

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 3: Spec — remove fictional `403` on `listOidcProviders` (design area C3)

**Files:**
- Modify: `api/openapi.yaml` (the `403` response on `listOidcProviders`, ~line 5447)
- Modify: `.github/oasdiff-err-ignore.txt`
- Test: `internal/e2e/oidc_reconciliation_test.go`

- [ ] **Step 1: Write the failing/guard test.** Append:

```go
// listOidcProviders is auth-only (any authenticated tenant member, D21) — it has
// no admin guard, so a non-admin authed user gets 200, never 403.
func TestOIDC_List_NonAdmin_Returns200Not403(t *testing.T) {
	cid, secret := createM2MClient(t, oidcTenantUUID, "nonadmin-user", []string{"ROLE_M2M"}) // no ROLE_ADMIN
	token := getToken(t, cid, secret)

	req, _ := e2eNewRequest(t, "GET", serverURL+"/api/oauth/oidc/providers", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("non-admin list: got %d, want 200 (no admin guard on list)", resp.StatusCode)
	}
}
```

- [ ] **Step 2: Run to verify it passes already** (this asserts current server behaviour; it should PASS immediately — the point is to lock the "no 403" contract before removing it from the spec).

Run: `go test ./internal/e2e/ -run TestOIDC_List_NonAdmin_Returns200Not403 -v`
Expected: PASS.

- [ ] **Step 3: Remove the `403` from the spec.** In `api/openapi.yaml`, delete the entire `'403':` response block under `listOidcProviders` (~line 5447, the `application/json ErrorResponseDto` one). Leave `401` and `500`.

- [ ] **Step 4: Run conformance to confirm nothing asserts 403.**

Run: `go test ./internal/e2e/ -run TestOpenAPIConformance -v` (the `zzz_openapi_conformance_test.go` suite)
Expected: PASS (no op exercises a 403 on this path).

- [ ] **Step 5: Add the oasdiff `response-removed` entry.** Run oasdiff locally, then append:

```
# auth/OIDC slice — listOidcProviders 403 removed: the op has no admin guard
# (auth-only by design, D21); the 403 was never emitted. Removing a documented
# response status is oasdiff-breaking but breaks no working client.
error at api/openapi.yaml, in API GET /oauth/oidc/providers removed the response with the status '403' [response-removed].
```

- [ ] **Step 6: Commit.**

```bash
git add api/openapi.yaml internal/e2e/oidc_reconciliation_test.go .github/oasdiff-err-ignore.txt
git commit -m "docs(oidc): remove fictional 403 from listOidcProviders (auth-only)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 4: Envelope sweep — 7 OIDC ops `ErrorResponseDto → ProblemDetail` + 400 sub-codes (design area A + C1/C2 docs)

**Files:**
- Modify: `api/openapi.yaml` (all error responses on the 7 OIDC ops)
- Modify: `.github/oasdiff-err-ignore.txt`
- Test: `internal/e2e/oidc_reconciliation_test.go`

**Interfaces:**
- Consumes: bare `ProblemDetail` schema (`api/openapi.yaml` ~line 8264).

- [ ] **Step 1: Write the envelope producing test.** Append — asserts the real server emits `application/problem+json` + `properties.errorCode` on a representative 4xx per op family (register 409 already covered in Task 1; here cover a 404 and the 400 sub-codes):

```go
func assertProblemJSON(t *testing.T, resp *http.Response, wantStatus int, wantCode string) {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status: got %d, want %d; body=%s", resp.StatusCode, wantStatus, raw)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("content-type: got %q, want application/problem+json", ct)
	}
	var pd struct {
		Status     int            `json:"status"`
		Properties map[string]any `json:"properties"`
	}
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, &pd); err != nil {
		t.Fatalf("unmarshal ProblemDetail: %v; body=%s", err, raw)
	}
	if got := fmt.Sprintf("%v", pd.Properties["errorCode"]); got != wantCode {
		t.Fatalf("errorCode: got %q, want %q; body=%s", got, wantCode, raw)
	}
}

func TestOIDC_Delete_NotFound_ProblemDetail(t *testing.T) {
	cid, secret := createM2MClient(t, oidcTenantUUID, "del-nf-user", []string{"ROLE_ADMIN", "ROLE_M2M"})
	token := getToken(t, cid, secret)
	req, _ := e2eNewRequest(t, "DELETE", serverURL+"/api/oauth/oidc/providers/does-not-exist", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	assertProblemJSON(t, resp, http.StatusNotFound, "OIDC_PROVIDER_NOT_FOUND")
}

// OIDC_INVALID_TENANT: register under a non-UUID tenant. The bootstrap
// "test-tenant" is not UUID-shaped, so its admin token triggers the guard.
func TestOIDC_Register_InvalidTenant_ProblemDetail(t *testing.T) {
	token := getToken(t, "testclient", "testsecret") // bootstrap tenant = "test-tenant" (non-UUID)
	resp := registerOIDCProvider(t, token, "badtenant")
	assertProblemJSON(t, resp, http.StatusBadRequest, "OIDC_INVALID_TENANT")
}
```

- [ ] **Step 2: Run to verify current envelope.** These assert the server's ACTUAL behaviour (it already emits `problem+json`), so they PASS immediately — but the **spec** still says `application/json ErrorResponseDto`, so the conformance suite will FAIL until Step 4.

Run: `go test ./internal/e2e/ -run 'TestOIDC_Delete_NotFound_ProblemDetail|TestOIDC_Register_InvalidTenant_ProblemDetail' -v`
Expected: PASS (asserting real server behaviour).

- [ ] **Step 3: Confirm the conformance gap.**

Run: `go test ./internal/e2e/ -run TestOpenAPIConformance -v`
Expected: FAIL — responses are `application/problem+json` but the spec declares `application/json ErrorResponseDto` on the OIDC ops.

- [ ] **Step 4: Convert all error responses on the 7 OIDC ops.** In `api/openapi.yaml`, for each of `listOidcProviders`, `registerOidcProvider`, `reloadOidcProviders`, `deleteOidcProvider`, `updateOidcProvider`, `invalidateOidcProvider`, `reactivateOidcProvider`, replace every error-response block of the form:

```yaml
        '404':
          description: ...
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/ErrorResponseDto'
```

with the ProblemDetail form (mirror `getStateMachineFinishedEvent`):

```yaml
        '404':
          description: >-
            Provider not found. Body is an RFC-9457 problem document;
            the machine-readable code is in `properties.errorCode`
            (e.g. `OIDC_PROVIDER_NOT_FOUND`).
          content:
            application/problem+json:
              schema:
                $ref: '#/components/schemas/ProblemDetail'
```

Apply to every status (400/401/403/404/409/500 as present per op). While here, enrich the **400** descriptions on `registerOidcProvider`/`updateOidcProvider` to name the emitted sub-codes: `BAD_REQUEST`, `OIDC_SSRF_BLOCKED`, `OIDC_INVALID_TENANT`; and confirm `updateOidcProvider` documents `409 OIDC_PROVIDER_INACTIVE` (C2 — add the `409` block if absent). Do NOT touch `getTechnicalUserToken` (Task 6). Do NOT touch the removed `listOidcProviders 403` (Task 3).

- [ ] **Step 5: Run conformance + producing tests.**

Run: `go test ./internal/e2e/ -run 'TestOpenAPIConformance|TestOIDC_' -v`
Expected: all PASS.

- [ ] **Step 6: Capture oasdiff entries.** Run oasdiff locally; for each converted op×status it will emit `response-required-property-removed` for `error` and `error_description` (and possibly `response-media-type-removed` for `application/json`). Paste the exact lines into `.github/oasdiff-err-ignore.txt` under one commented block. Explicitly EXCLUDE `listOidcProviders 403` (handled in Task 3). Example header:

```
# auth/OIDC slice — 7 OIDC ops converted ErrorResponseDto→ProblemDetail: the
# server has always emitted application/problem+json (RFC-9457); the OAuth-shaped
# ErrorResponseDto was prototype-era drift. Correcting removes error/error_description.
<PASTE all exact singlelines here>
```

- [ ] **Step 7: Commit.**

```bash
git add api/openapi.yaml internal/e2e/oidc_reconciliation_test.go .github/oasdiff-err-ignore.txt
git commit -m "docs(oidc): convert 7 OIDC ops to ProblemDetail envelope + document 400 sub-codes

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 5: Envelope sweep — `searchEntityAuditEvents` (D-1) `ErrorResponseDto → ProblemDetail`

**Files:**
- Modify: `api/openapi.yaml` (`searchEntityAuditEvents` error responses, ~line 458-482)
- Modify: `.github/oasdiff-err-ignore.txt`
- Test: `internal/e2e/audit_envelope_test.go` (create; or extend an existing audit test)

- [ ] **Step 1: Write the producing test.** Create `internal/e2e/audit_envelope_test.go`:

```go
package e2e_test

import (
	"net/http"
	"testing"
)

// searchEntityAuditEvents on a malformed/unknown request emits ProblemDetail
// (application/problem+json), not the OAuth-shaped ErrorResponseDto the spec used.
func TestAudit_Search_BadRequest_ProblemDetail(t *testing.T) {
	token := getToken(t, "testclient", "testsecret")
	// Malformed entityId → 400 BAD_REQUEST via ProblemDetail.
	req, _ := e2eNewRequest(t, "GET", serverURL+"/api/audit/entity/not-a-uuid?pageSize=-5", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	// assertProblemJSON is defined in oidc_reconciliation_test.go (same package).
	assertProblemJSON(t, resp, http.StatusBadRequest, "BAD_REQUEST")
}
```

(Confirm the exact path/param that yields a 400 against `internal/domain/.../audit` handler before finalizing; adjust the code assertion to the real emitted code if it differs — the point is `application/problem+json`.)

- [ ] **Step 2: Run to verify current envelope (passes) + conformance gap (fails).**

Run: `go test ./internal/e2e/ -run 'TestAudit_Search_BadRequest_ProblemDetail|TestOpenAPIConformance' -v`
Expected: producing test PASS; conformance FAIL (spec still `ErrorResponseDto`).

- [ ] **Step 3: Convert the 5 error responses.** In `api/openapi.yaml`, `searchEntityAuditEvents` (~458-482), convert `400/401/403/404/500` from `application/json ErrorResponseDto` to `application/problem+json ProblemDetail` (same form as Task 4).

- [ ] **Step 4: Run conformance.**

Run: `go test ./internal/e2e/ -run 'TestOpenAPIConformance|TestAudit_' -v`
Expected: PASS.

- [ ] **Step 5: oasdiff entries.** Capture + append the `response-required-property-removed` (and any media-type) lines for `searchEntityAuditEvents` × 5 statuses.

- [ ] **Step 6: Commit.**

```bash
git add api/openapi.yaml internal/e2e/audit_envelope_test.go .github/oasdiff-err-ignore.txt
git commit -m "docs(audit): convert searchEntityAuditEvents to ProblemDetail envelope (D-1)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 6: Token — `getTechnicalUserToken` enum/status fixes (design area B)

**Files:**
- Modify: `api/openapi.yaml` (`getTechnicalUserToken` request/response enums + `405`; `ErrorResponseDto.error` enum ~8675; `TokenResponseDto.issued_token_type` enum ~9204)
- Test: `internal/e2e/token_reconciliation_test.go` (create)

**Interfaces:**
- Consumes: `statusForToken`, `getToken`, `e2eNewRequest`.

- [ ] **Step 1: Write the producing tests.** Create `internal/e2e/token_reconciliation_test.go`:

```go
package e2e_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func postToken(t *testing.T, form url.Values, basicUser, basicPass string) *http.Response {
	t.Helper()
	req, _ := e2eNewRequest(t, "POST", serverURL+"/api/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if basicUser != "" {
		req.SetBasicAuth(basicUser, basicPass)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return resp
}

func TestToken_ClientCredentials_Accepted(t *testing.T) {
	resp := postToken(t, url.Values{"grant_type": {"client_credentials"}}, "testclient", "testsecret")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("client_credentials: got %d, want 200; body=%s", resp.StatusCode, raw)
	}
}

func TestToken_BadGrantType_400UnsupportedGrantType(t *testing.T) {
	resp := postToken(t, url.Values{"grant_type": {"password"}}, "testclient", "testsecret")
	assertOAuthError(t, resp, http.StatusBadRequest, "unsupported_grant_type")
}

func TestToken_BadClient_401InvalidClient(t *testing.T) {
	resp := postToken(t, url.Values{"grant_type": {"client_credentials"}}, "testclient", "wrongsecret")
	assertOAuthError(t, resp, http.StatusUnauthorized, "invalid_client")
}

func TestToken_NonPost_405MethodNotAllowed(t *testing.T) {
	req, _ := e2eNewRequest(t, "GET", serverURL+"/api/oauth/token", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	assertOAuthError(t, resp, http.StatusMethodNotAllowed, "method_not_allowed")
}

// assertOAuthError asserts the flat RFC-6749 shape (application/json, {error,error_description}),
// NOT ProblemDetail — the token endpoint deliberately keeps ErrorResponseDto.
func assertOAuthError(t *testing.T, resp *http.Response, wantStatus int, wantErr string) {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status: got %d, want %d; body=%s", resp.StatusCode, wantStatus, raw)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type: got %q, want application/json (flat OAuth shape)", ct)
	}
	var e struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &e)
	if e.Error != wantErr {
		t.Fatalf("error: got %q, want %q; body=%s", e.Error, wantErr, raw)
	}
	if e.ErrorDescription == "" {
		t.Fatalf("error_description empty (spec marks it required); body=%s", raw)
	}
}
```

Add `invalid_grant` (bad token-exchange subject token) and `access_denied` (tenant mismatch) cases following the same `postToken`/`assertOAuthError` pattern — reuse `createM2MClient` to mint a token for the token-exchange grant. (Confirm the exact token-exchange request shape from `internal/auth/token.go` before finalizing those two.)

- [ ] **Step 2: Run to verify producing tests pass against the server** (server already emits these; the spec is what's stale).

Run: `go test ./internal/e2e/ -run TestToken_ -v`
Expected: PASS.

- [ ] **Step 3: Run conformance to expose the enum/status gaps.**

Run: `go test ./internal/e2e/ -run TestOpenAPIConformance -v`
Expected: FAIL — `client_credentials` grant_type, `405`, and the `method_not_allowed`/`server_error` error values are undocumented.

- [ ] **Step 4: Fix the spec enums + add 405.** In `api/openapi.yaml`:
  - `getTechnicalUserToken` request body `grant_type` enum (~5852): add `client_credentials`.
  - `TokenResponseDto.issued_token_type` enum (~9204): add `urn:ietf:params:oauth:token-type:jwt`.
  - `ErrorResponseDto.error` enum (~8675): add `server_error` and `method_not_allowed`.
  - Add a `'405'` response block to `getTechnicalUserToken` (`application/json ErrorResponseDto`, description names `method_not_allowed`).
  - Leave `error_uri` and the three unemitted enum values (`invalid_request`, `unauthorized_client`, `invalid_scope`) as-is (documented-but-unused output; harmless).

- [ ] **Step 5: Run conformance + producing tests.**

Run: `go test ./internal/e2e/ -run 'TestOpenAPIConformance|TestToken_' -v`
Expected: PASS.

- [ ] **Step 6: oasdiff.** Request-enum widening (`grant_type +client_credentials`) and response-enum additions are non-breaking (additive) — expect NO new err-ignore entries. Confirm the oasdiff run stays green without additions.

- [ ] **Step 7: Commit.**

```bash
git add api/openapi.yaml internal/e2e/token_reconciliation_test.go
git commit -m "docs(token): document client_credentials grant, 405, server_error enum

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 7: Keys/trusted spec-stale docs (design area E) — shared-harness producible codes

**Files:**
- Modify: `api/openapi.yaml` (`issueJwtKeyPair`, `registerTrustedKey`, and the invalidate/delete/reactivate keys/trusted ops)
- Test: `internal/e2e/keys_trusted_reconciliation_test.go` (create)

**Interfaces:**
- Consumes: `adminRequest` (bootstrap admin, jwt harness, trusted feature ON).

- [ ] **Step 1: Write producing tests for the jwt-harness-producible codes.** Create `internal/e2e/keys_trusted_reconciliation_test.go`:

```go
package e2e_test

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

func codeFromProblem(t *testing.T, resp *http.Response) (int, string) {
	t.Helper()
	defer resp.Body.Close()
	var pd struct {
		Properties map[string]any `json:"properties"`
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &pd)
	code := ""
	if pd.Properties != nil {
		if c, ok := pd.Properties["errorCode"]; ok {
			code = c.(string)
		}
	}
	return resp.StatusCode, code
}

func TestKeys_IssueNonRS256_400UnsupportedAlgorithm(t *testing.T) {
	resp := adminRequest(t, "POST", "/oauth/keys/keypair", []byte(`{"algorithm":"ES256","audience":"a"}`))
	if st, code := codeFromProblem(t, resp); st != http.StatusBadRequest || code != "UNSUPPORTED_ALGORITHM" {
		t.Fatalf("got %d/%s, want 400/UNSUPPORTED_ALGORITHM", st, code)
	}
}

func TestTrusted_RegisterNonRSA_400UnsupportedKeyType(t *testing.T) {
	// A well-formed EC JWK — server accepts only RSA (v0.8.0).
	body := []byte(`{"jwk":{"kty":"EC","crv":"P-256","x":"<b64url-x>","y":"<b64url-y>"}}`)
	resp := adminRequest(t, "POST", "/oauth/keys/trusted", body)
	if st, code := codeFromProblem(t, resp); st != http.StatusBadRequest || code != "UNSUPPORTED_KEY_TYPE" {
		t.Fatalf("got %d/%s, want 400/UNSUPPORTED_KEY_TYPE", st, code)
	}
}
```

Add cases (same `adminRequest`/`codeFromProblem` shape) for: `registerTrustedKey` cross-tenant → `409 KEY_OWNED_BY_DIFFERENT_TENANT`; `registerTrustedKey` cap reached → `400 TRUSTED_KEY_CAP_REACHED`; `invalidateJwtKeyPair` bad body → `400`, unknown id → `404 KEYPAIR_NOT_FOUND`; `deleteTrustedKey`/`invalidateTrustedKey` bad id → `400`, unknown id → `404 TRUSTED_KEY_NOT_FOUND`. **Confirm the exact request paths and JSON shapes** from `internal/domain/account/keys_adapter.go` / `trusted_adapter.go` before finalizing (the placeholders above must become real paths/bodies).

- [ ] **Step 2: Run producing tests (server already emits these; feature ON on shared harness).**

Run: `go test ./internal/e2e/ -run 'TestKeys_|TestTrusted_' -v`
Expected: PASS.

- [ ] **Step 3: Run conformance to expose undocumented statuses.**

Run: `go test ./internal/e2e/ -run TestOpenAPIConformance -v`
Expected: FAIL on the newly-exercised undocumented statuses/codes.

- [ ] **Step 4: Document the statuses in the spec.** In `api/openapi.yaml`:
  - `issueJwtKeyPair`: keep the full 10-value `algorithm` enum; add field description "Only `RS256` is honoured in this version." Add `400` (names `UNSUPPORTED_ALGORITHM` + malformed body).
  - `registerTrustedKey`: keep the RSA/EC/OKP description; append "Only RSA is honoured in this version." Add/confirm `400` (`UNSUPPORTED_KEY_TYPE`, `TRUSTED_KEY_CAP_REACHED`, malformed), `404` (`FEATURE_DISABLED`), `409` (`KEY_OWNED_BY_DIFFERENT_TENANT`).
  - `invalidateJwtKeyPair`/`getCurrentJwtKeyPair`/`reactivateJwtKeyPair`: document `400` (body/grace/audience), `404 KEYPAIR_NOT_FOUND`.
  - `deleteTrustedKey`/`invalidateTrustedKey`/`listTrustedKeys`/`reactivateTrustedKey`: document `400` (id/body/grace), `404` (`TRUSTED_KEY_NOT_FOUND` and `FEATURE_DISABLED`).
  - All error bodies use `application/problem+json ProblemDetail` (the keys/trusted ops already emit it — verify each op's current schema; normalise any stray inline shape).

- [ ] **Step 5: Run conformance + producing tests.**

Run: `go test ./internal/e2e/ -run 'TestOpenAPIConformance|TestKeys_|TestTrusted_' -v`
Expected: PASS.

- [ ] **Step 6: oasdiff.** These are additive (documenting already-emitted statuses/codes) → expect no err-ignore entries. Confirm green.

- [ ] **Step 7: Commit.**

```bash
git add api/openapi.yaml internal/e2e/keys_trusted_reconciliation_test.go
git commit -m "docs(keys): document keys/trusted error surface + RS256/RSA this-version limits

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 8: `OIDC_SSRF_BLOCKED` producing test on a hardened fixture

**Files:**
- Test: `internal/e2e/oidc_hardened_fixture_test.go` (create)

**Rationale:** the shared harness `init()` disables OIDC SSRF/HTTPS enforcement process-wide, so `OIDC_SSRF_BLOCKED` cannot be produced there. Build a dedicated `app.New(cfg)` + `httptest` server with hardening ON (per `cors_e2e_test.go`).

- [ ] **Step 1: Write the fixture + test.** Create `internal/e2e/oidc_hardened_fixture_test.go`:

```go
package e2e_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cyoda/cyoda-go/app"
)

// newHardenedOIDCServer builds a server with OIDC SSRF/HTTPS enforcement ON
// (the shared harness relaxes them process-wide), so registering a provider
// whose issuer resolves to a private/link-local address is SSRF-blocked.
func newHardenedOIDCServer(t *testing.T) (string, func()) {
	t.Helper()
	cfg := app.DefaultConfig()
	cfg.IAM.Mode = "jwt"
	cfg.OIDC.RequireHTTPS = true
	cfg.OIDC.AllowPrivateNetworks = false
	// ... seed the same jwt bootstrap client as TestMain (see e2e_test.go:120-140).
	a := app.New(cfg)
	srv := httptest.NewServer(a.Handler())
	return srv.URL, srv.Close
}

func TestOIDC_Register_SSRFBlocked_400(t *testing.T) {
	base, closeFn := newHardenedOIDCServer(t)
	defer closeFn()
	token := getToken(t, "testclient", "testsecret") // adjust to the fixture's bootstrap client
	body, _ := json.Marshal(map[string]any{
		"name":      "ssrf",
		"issuerUri": "http://169.254.169.254/latest", // link-local metadata endpoint
		"clientId":  "cid", "clientSecret": "s",
	})
	req, _ := http.NewRequest("POST", base+"/api/oauth/oidc/providers", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if st, code := codeFromProblem(t, resp); st != http.StatusBadRequest || code != "OIDC_SSRF_BLOCKED" {
		t.Fatalf("got %d/%s, want 400/OIDC_SSRF_BLOCKED", st, code)
	}
}
```

**Confirm** the real `app.Config` field names for OIDC hardening and the bootstrap-client seeding from `e2e_test.go` `TestMain` before finalizing — the snippet's field paths are illustrative. If a standalone fixture proves impractical, fall back to asserting `OIDC_SSRF_BLOCKED` via the existing `internal/auth/oidc` unit tests and record the waiver in the spec §9 (running-backend-non-producible → unit).

- [ ] **Step 2: Run.**

Run: `go test ./internal/e2e/ -run TestOIDC_Register_SSRFBlocked_400 -v`
Expected: PASS (or convert to the unit-test waiver above).

- [ ] **Step 3: Commit.**

```bash
git add internal/e2e/oidc_hardened_fixture_test.go
git commit -m "test(oidc): OIDC_SSRF_BLOCKED producing test on hardened fixture

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 9: Config-conditional 501 + fixtures + gated coverage (design area D)

**Files:**
- Create: `internal/e2e/iam_gated_fixtures_test.go` (F-a mock-IAM, F-b feature-off servers)
- Create: `internal/e2e/iam_gated_501_test.go` (table-driven coverage)
- Modify: `api/openapi.yaml` (add `501` + `x-cyoda-iam-mode` to 21 ops; `x-cyoda-feature` on 5 trusted)

**Interfaces:**
- Produces: `newMockIAMServer(t) (base string, close func())`, `newFeatureOffServer(t) (base string, close func())`.

- [ ] **Step 1: Build the two fixtures.** Create `internal/e2e/iam_gated_fixtures_test.go`:

```go
package e2e_test

import (
	"net/http/httptest"
	"testing"

	"github.com/cyoda/cyoda-go/app"
)

// F-a: mock-IAM server. IAM.Mode != "jwt" leaves the OIDC adapter and the
// keys/M2M/trusted stores nil, so every IAM-gated op returns 501. RequireAdmin
// is satisfied by the default mock roles {ROLE_ADMIN, ROLE_M2M}. Feature flag
// ON so the trusted feature gate passes and the nil store gate is what 501s.
func newMockIAMServer(t *testing.T) (string, func()) {
	t.Helper()
	cfg := app.DefaultConfig()
	cfg.IAM.Mode = "mock"
	cfg.IAM.TrustedKeyRegistrationEnabled = true // feature on → 501 comes from nil store, not 404
	a := app.New(cfg)
	srv := httptest.NewServer(a.Handler())
	return srv.URL, srv.Close
}

// F-b: jwt server with the trusted-key feature OFF, so trusted ops hit the
// feature gate (404 FEATURE_DISABLED) before the store gate.
func newFeatureOffServer(t *testing.T) (string, func()) {
	t.Helper()
	cfg := app.DefaultConfig()
	cfg.IAM.Mode = "jwt"
	cfg.IAM.TrustedKeyRegistrationEnabled = false
	// ... seed the jwt bootstrap client (see e2e_test.go TestMain).
	a := app.New(cfg)
	srv := httptest.NewServer(a.Handler())
	return srv.URL, srv.Close
}
```

**Confirm** `app.DefaultConfig()` mock-mode wiring (adapter/stores nil) and the jwt bootstrap seeding before finalizing.

- [ ] **Step 2: Write the table-driven 501/404 coverage.** Create `internal/e2e/iam_gated_501_test.go` — one row per gated op, hitting F-a for the 16 (and the 5 trusted feature-on variant) and F-b for the 5 trusted 404:

```go
package e2e_test

import (
	"net/http"
	"testing"
)

type gatedOp struct{ method, path string }

// 16 ops that return 501 on F-a (mock IAM): 7 OIDC + 5 keys + 4 M2M.
// All paths are the /api-mounted spec routes (see the Canonical endpoint table).
var mockIAM501Ops = []gatedOp{
	{"GET", "/api/oauth/oidc/providers"},
	{"POST", "/api/oauth/oidc/providers"},
	// ... reload, delete, update, invalidate, reactivate OIDC (providers/{id}[/verb]) ...
	{"POST", "/api/oauth/keys/keypair"},
	{"GET", "/api/oauth/keys/keypair/current"},
	// ... delete, invalidate, reactivate keypair ...
	{"POST", "/api/clients"},
	{"GET", "/api/clients"},
	// ... delete, resetSecret M2M (/api/clients/{clientId}[/secret]) ...
}

func TestGated_MockIAM_Returns501(t *testing.T) {
	base, closeFn := newMockIAMServer(t)
	defer closeFn()
	for _, op := range mockIAM501Ops {
		t.Run(op.method+" "+op.path, func(t *testing.T) {
			req, _ := http.NewRequest(op.method, base+op.path, nil)
			// mock mode injects an admin user context without a bearer.
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("do: %v", err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusNotImplemented {
				t.Fatalf("%s %s: got %d, want 501", op.method, op.path, resp.StatusCode)
			}
		})
	}
}

// 5 trusted ops return 501 on F-a (feature on + nil store).
var trusted501Ops = []gatedOp{
	{"POST", "/api/oauth/keys/trusted"},
	{"GET", "/api/oauth/keys/trusted"},
	// ... delete, invalidate, reactivate (/api/oauth/keys/trusted/{keyId}[/invalidate]) ...
}

func TestGated_Trusted_MockIAM_Returns501(t *testing.T) {
	base, closeFn := newMockIAMServer(t)
	defer closeFn()
	for _, op := range trusted501Ops {
		t.Run(op.method+" "+op.path, func(t *testing.T) {
			req, _ := http.NewRequest(op.method, base+op.path, nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("do: %v", err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusNotImplemented {
				t.Fatalf("%s %s: got %d, want 501", op.method, op.path, resp.StatusCode)
			}
		})
	}
}

// 5 trusted ops return 404 FEATURE_DISABLED on F-b (jwt + feature off).
func TestGated_Trusted_FeatureOff_Returns404(t *testing.T) {
	base, closeFn := newFeatureOffServer(t)
	defer closeFn()
	token := getToken(t, "testclient", "testsecret") // adjust to fixture bootstrap
	for _, op := range trusted501Ops {
		t.Run(op.method+" "+op.path, func(t *testing.T) {
			req, _ := http.NewRequest(op.method, base+op.path, nil)
			req.Header.Set("Authorization", "Bearer "+token)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("do: %v", err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("%s %s: got %d, want 404 FEATURE_DISABLED", op.method, op.path, resp.StatusCode)
			}
		})
	}
}
```

Fill in ALL 16 + 5 op paths (confirm exact routes from the router / `api/openapi.yaml` paths).

- [ ] **Step 3: Run the gated coverage.**

Run: `go test ./internal/e2e/ -run 'TestGated_' -v`
Expected: PASS (fixtures produce 501 / 404 as designed).

- [ ] **Step 4: Document 501 + annotations in the spec.** In `api/openapi.yaml`, add to each of the 21 ops a `'501'` response (`application/problem+json ProblemDetail`, description "Returned when the IAM subsystem is not active (`CYODA_IAM_MODE` ≠ `jwt`).") and the vendor extension `x-cyoda-iam-mode: jwt`. On the 5 trusted ops additionally add `x-cyoda-feature: trusted-key-registration` and note the `404 FEATURE_DISABLED` precedence (already documented via Task 7).

- [ ] **Step 5: Run conformance.**

Run: `go test ./internal/e2e/ -run TestOpenAPIConformance -v`
Expected: PASS (the shared harness never emits 501, but the conformance validator accepts the additional documented response).

- [ ] **Step 6: oasdiff.** Adding a `501` response is additive → no err-ignore. Confirm green.

- [ ] **Step 7: Commit.**

```bash
git add api/openapi.yaml internal/e2e/iam_gated_fixtures_test.go internal/e2e/iam_gated_501_test.go
git commit -m "docs(iam): document config-conditional 501 on 21 IAM-gated ops + coverage fixtures

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 10: Part F — consolidate `ProblemDetailDto` into `ProblemDetail`

**Files:**
- Modify: `api/openapi.yaml` (enrich `ProblemDetail` ~8264; repoint 9 refs; delete `ProblemDetailDto` ~8306)

- [ ] **Step 1: Enrich `ProblemDetail`.** Copy the field `description`/`example` values from `ProblemDetailDto` onto the matching `ProblemDetail` properties (`type`/`title`/`status`/`detail`/`instance`/`properties`). Keep `ProblemDetail`'s `format: uri` on `type`/`instance`. Do not add `required` (stay open).

- [ ] **Step 2: Repoint the 9 refs.** Replace every `$ref: "#/components/schemas/ProblemDetailDto"` (on `submitAsyncSearchJob`, `getAsyncSearchResults`, `cancelAsyncSearch`, `getAsyncSearchStatus`, `searchEntities` — ~lines 6579-6884) with `$ref: '#/components/schemas/ProblemDetail'`.

- [ ] **Step 3: Delete `ProblemDetailDto`.** Remove the schema definition (~8306). Verify zero remaining refs: `grep -c "ProblemDetailDto" api/openapi.yaml` → `0`.

- [ ] **Step 4: Validate + conformance.**

Run: `go test ./internal/e2e/ -run 'TestOpenAPIConformance|TestSearch' -v`
Expected: PASS (search ops now validate against the enriched `ProblemDetail`; both schemas were structurally equivalent).

- [ ] **Step 5: oasdiff.** Expect non-breaking (no `required` loss; responses gain `format: uri` which is non-breaking). If oasdiff unexpectedly ERRs, capture + add a documented entry; otherwise none.

- [ ] **Step 6: Commit.**

```bash
git add api/openapi.yaml
git commit -m "refactor(openapi): consolidate ProblemDetailDto into bare ProblemDetail

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 11: oasdiff gate — consolidate & verify all err-ignore entries

**Files:**
- Modify: `.github/oasdiff-err-ignore.txt` (verify/tidy)

- [ ] **Step 1: Run the oasdiff gate exactly as CI does.** Find the invocation (`grep -rn oasdiff .github/ Makefile scripts/`), then run it against `origin/release/v0.8.2` as base. Expected: all breaking changes from Tasks 2/3/4/5 are matched by err-ignore entries; **zero un-ignored breaking changes**.

- [ ] **Step 2: Verify fail-closed.** Confirm each entry is the exact singleline oasdiff emits (a reworded pinned-oasdiff output would stop matching and re-flag). Confirm no entry is broad enough to mask an unrelated break (each names a specific op+status+property).

- [ ] **Step 3: Commit any tidy.**

```bash
git add .github/oasdiff-err-ignore.txt
git commit -m "chore(oasdiff): consolidate auth/OIDC slice err-ignore entries

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 12: Regenerate `generated.go` + codegen-sync

**Files:**
- Modify: `api/generated.go`

- [ ] **Step 1: Final regen.**

Run: `go generate ./api/... && make check-codegen`
Expected: `make check-codegen` PASS (generated.go in sync with all yaml edits).

- [ ] **Step 2: Build + vet.**

Run: `go build ./... && go vet ./...`
Expected: clean.

- [ ] **Step 3: Commit (if generated.go changed beyond Task 2).**

```bash
git add api/generated.go
git commit -m "chore(api): regenerate generated.go for auth/OIDC spec edits

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 13: Gate-4 documentation — cloud-parity, help topic, CHANGELOG

**Files:**
- Modify: `docs/cloud-parity/openapi-conformance.md`
- Modify: `cmd/cyoda/help/content/` (auth/OIDC topic, if present and drifted)
- Modify: `CHANGELOG.md`

- [ ] **Step 1: Cloud-parity entries.** Append the "Auth / OIDC reconciliations (2026-07)" section from spec §12 (A1 duplicate→409; A2 ProblemDetail envelope; A3 IAM-gated 501 + trusted 404 nuance; A4 roadmap-placeholder crypto enums; A5 activeOnly boolean).

- [ ] **Step 2: Help topic.** `grep -rl -i "oidc\|technical.user.token\|jwt.*key" cmd/cyoda/help/content/` — if an auth/OIDC topic exists and documents the 409 semantics, IAM-gating, or algorithm limits, update it (compact — actionable core only). No error-code topic additions (all codes already have topics).

- [ ] **Step 3: CHANGELOG.** Add a group-3 entry under the release/v0.8.2 section (envelope sweep, 409 fix, activeOnly boolean, 501 documentation, ProblemDetailDto consolidation). No issue IDs in the CHANGELOG body text beyond the established convention.

- [ ] **Step 4: Commit.**

```bash
git add docs/cloud-parity/openapi-conformance.md cmd/cyoda/help/content/ CHANGELOG.md
git commit -m "docs: cloud-parity + CHANGELOG for auth/OIDC reconciliation

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 14: Full verification (Gate 5)

- [ ] **Step 1: Full e2e (Docker required; controller runs this, not subagents).**

Run: `go test ./internal/e2e/... -v`
Expected: green (includes conformance + all new producing/gated tests).

- [ ] **Step 2: Short unit + plugin submodules.**

Run: `make test-short-all`
Expected: green (root + `plugins/memory|sqlite|postgres`).

- [ ] **Step 3: Vet + gofmt + codegen + oasdiff gates.**

Run: `go vet ./... && make check-gofmt && make check-codegen`
Expected: all clean. Run the oasdiff gate (Task 11 invocation): no un-ignored breaks.

- [ ] **Step 4: Race (one-shot, pre-PR).**

Run: `make race`
Expected: green (CI-parity scope; excludes internal/e2e).

- [ ] **Step 5: Confirm no secrets/issue-IDs leaked.** `grep -rn "#369\|#37[0-9]" api/openapi.yaml cmd/cyoda/help/content/ internal/domain/account/` → only expected (none in shipped artefacts). Confirm no private-key material in any keys/trusted response body test fixture.

- [ ] **Step 6: Final commit / branch ready for PR.** Ensure working tree clean; branch `worktree-feat-openapi-oidc-auth` ready to open a PR into `release/v0.8.2`.

---

## Self-Review checklist (run after execution, before PR)

- **Spec coverage:** every §8 table row → a producing test or documented waiver (Tasks 1,4,5,6,7,8,9); every §9 matrix row → a task. 501/404 gated rows → Task 9 fixtures. Envelope → Tasks 4,5. Consolidation → Task 10.
- **oasdiff:** every breaking edit (activeOnly type, 403 removal, envelope property/media removals) → a captured, fail-closed err-ignore entry (Tasks 2,3,4,5,11); additive edits (501, 405, enums, keys/trusted docs) → none.
- **Gate-4:** cloud-parity A1-A5, help topic, CHANGELOG (Task 13); no new error codes → no `errors/<CODE>.md` (Global Constraints).
- **No placeholders in shipped artefacts;** no issue IDs in YAML/help/code; private keys never in responses.
