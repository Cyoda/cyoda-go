# Auth help topic + v0.8.0 OIDC polish — implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship the OIDC response-mapper bugfix, stale-doc corrections, and a new top-level `auth` help topic tree (with `clients`, `tokens`, `oidc`, `trusted-keys` subtopics) in one PR against `release/v0.8.0`.

**Architecture:** Five-commit PR per spec §5. Commit 1 is TDD on the parity-test framework that covers all backends. Commits 2–4 are surgical doc edits. Commit 5 adds 5 new help-content markdown files; the help subsystem is filesystem-driven (front-matter validated at boot), so no new Go code is needed for the topic tree itself — only a few load-resolves test cases.

**Tech Stack:** Go 1.26+, parity-test framework in `e2e/parity/`, help subsystem at `cmd/cyoda/help/` (YAML front-matter + markdown bodies, embedded via `//go:embed`).

**Spec:** `docs/superpowers/specs/2026-06-17-auth-help-topic-design.md` (commits `ddcf8b9`, `a08a611`, `572abb3`).

**Branch:** `feat/auth-help-topic` off `release/v0.8.0`. **Worktree:** `.worktrees/feat-auth-help-topic` (already created — work from there).

---

## File structure

| Path | Action | Responsibility |
|---|---|---|
| `internal/domain/account/oidc_adapter.go` | Modify | Populate `expectedAudiences` + `rolesClaim` in `toOidcProviderResponseDto`. Remove stale "Task 8.1" comment. |
| `e2e/parity/oidc.go` | Modify | Add 4 new `Run*` scenarios asserting the two fields round-trip through register/list/update/reactivate. |
| `e2e/parity/registry.go` | Modify | Register the 4 new scenarios in `allTests`. |
| `cmd/cyoda/help/content/openapi.md` | Modify | Rewrite the line stating OIDC is 501-until-v0.9.0. |
| `docs/cyoda/README.md` | Create | Document the two coexisting spec-layer artefacts in `docs/cyoda/`. |
| `docs/cyoda/cloud-divergences.md` | Modify | Replace stale "22 IAM stubs" row with current snapshot + derivation snippet. |
| `cmd/cyoda/help/content/auth.md` | Create | Top-level landing / decision tree. |
| `cmd/cyoda/help/content/auth/clients.md` | Create | M2M client lifecycle subtopic. |
| `cmd/cyoda/help/content/auth/tokens.md` | Create | `/oauth/token` endpoint + grants (incl. OBO as a section). |
| `cmd/cyoda/help/content/auth/oidc.md` | Create | Federated OIDC providers subtopic. |
| `cmd/cyoda/help/content/auth/trusted-keys.md` | Create | Offline-signing trusted-keys subtopic. |
| `cmd/cyoda/help/help_test.go` | Modify | Add load-resolve cases for the 5 new topics. |

---

## Task 1: OIDC response-mapper bugfix (commit 1)

Bugfix via TDD on the parity-test framework. The 4 new test scenarios run against every backend (memory, sqlite, postgres) via the parity registry.

**Files:**
- Modify: `e2e/parity/oidc.go` (add 4 new `Run*` functions at end of file)
- Modify: `e2e/parity/registry.go` (register 4 new `NamedTest` entries)
- Modify: `internal/domain/account/oidc_adapter.go` (`toOidcProviderResponseDto`)

### 1.1 — Write failing parity tests (RED)

- [ ] **Step 1: Add the 4 new test functions to `e2e/parity/oidc.go`**

Append at end of file (after the existing `Run*` functions):

```go
// --- Audiences + rolesClaim round-trip (D7) ---
//
// Each of these scenarios provides non-empty expectedAudiences and a
// non-default rolesClaim at the relevant write path, then asserts the
// response carries those fields. The mapper toOidcProviderResponseDto
// previously dropped both fields silently — these scenarios are the
// regression guard.

// RunOidcRegisterAudiencesRoundTrip verifies POST /oauth/oidc/providers
// returns expectedAudiences and rolesClaim in the response when supplied
// at registration.
func RunOidcRegisterAudiencesRoundTrip(t *testing.T, fix BackendFixture) {
	tenant := fix.NewTenant(t)
	c := client.NewClient(fix.BaseURL(), tenant.Token)

	uri := oidcWellKnownURI(tenant.ID, "register-aud-roles")
	wantAud := []string{"aud-a", "aud-b"}
	wantRoles := "cognito:groups"

	p, err := c.RegisterOidcProvider(t, map[string]any{
		"wellKnownConfigUri": uri,
		"expectedAudiences":  wantAud,
		"rolesClaim":         wantRoles,
	})
	if err != nil {
		t.Fatalf("RegisterOidcProvider: %v", err)
	}

	if p.ExpectedAudiences == nil {
		t.Fatal("expectedAudiences must be present in response (was nil)")
	}
	if !reflect.DeepEqual(*p.ExpectedAudiences, wantAud) {
		t.Errorf("expectedAudiences: got %v, want %v", *p.ExpectedAudiences, wantAud)
	}
	if p.RolesClaim == nil {
		t.Fatal("rolesClaim must be present in response (was nil)")
	}
	if *p.RolesClaim != wantRoles {
		t.Errorf("rolesClaim: got %q, want %q", *p.RolesClaim, wantRoles)
	}
}

// RunOidcListAudiencesRoundTrip verifies GET /oauth/oidc/providers
// surfaces expectedAudiences and rolesClaim on every listed provider.
func RunOidcListAudiencesRoundTrip(t *testing.T, fix BackendFixture) {
	tenant := fix.NewTenant(t)
	c := client.NewClient(fix.BaseURL(), tenant.Token)

	uri := oidcWellKnownURI(tenant.ID, "list-aud-roles")
	wantAud := []string{"aud-list"}
	wantRoles := "realm_access.roles"

	registered, err := c.RegisterOidcProvider(t, map[string]any{
		"wellKnownConfigUri": uri,
		"expectedAudiences":  wantAud,
		"rolesClaim":         wantRoles,
	})
	if err != nil {
		t.Fatalf("RegisterOidcProvider: %v", err)
	}

	providers, err := c.ListOidcProviders(t, false)
	if err != nil {
		t.Fatalf("ListOidcProviders: %v", err)
	}

	var found *client.OidcProviderResponse
	for i := range providers {
		if providers[i].ID == registered.ID {
			found = &providers[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("registered provider %s missing from list", registered.ID)
	}
	if found.ExpectedAudiences == nil || !reflect.DeepEqual(*found.ExpectedAudiences, wantAud) {
		t.Errorf("expectedAudiences in list: got %v, want %v", found.ExpectedAudiences, wantAud)
	}
	if found.RolesClaim == nil || *found.RolesClaim != wantRoles {
		t.Errorf("rolesClaim in list: got %v, want %q", found.RolesClaim, wantRoles)
	}
}

// RunOidcUpdateAudiencesRoundTrip verifies PATCH /oauth/oidc/providers/{id}
// surfaces updated expectedAudiences and rolesClaim in the response.
func RunOidcUpdateAudiencesRoundTrip(t *testing.T, fix BackendFixture) {
	tenant := fix.NewTenant(t)
	c := client.NewClient(fix.BaseURL(), tenant.Token)

	uri := oidcWellKnownURI(tenant.ID, "update-aud-roles")
	initial, err := c.RegisterOidcProvider(t, map[string]any{
		"wellKnownConfigUri": uri,
		"expectedAudiences":  []string{"aud-initial"},
		"rolesClaim":         "roles-initial",
	})
	if err != nil {
		t.Fatalf("RegisterOidcProvider: %v", err)
	}

	wantAud := []string{"aud-updated-a", "aud-updated-b"}
	wantRoles := "groups"
	updated, err := c.UpdateOidcProvider(t, initial.ID, map[string]any{
		"expectedAudiences": wantAud,
		"rolesClaim":        wantRoles,
	})
	if err != nil {
		t.Fatalf("UpdateOidcProvider: %v", err)
	}

	if updated.ExpectedAudiences == nil || !reflect.DeepEqual(*updated.ExpectedAudiences, wantAud) {
		t.Errorf("expectedAudiences after update: got %v, want %v", updated.ExpectedAudiences, wantAud)
	}
	if updated.RolesClaim == nil || *updated.RolesClaim != wantRoles {
		t.Errorf("rolesClaim after update: got %v, want %q", updated.RolesClaim, wantRoles)
	}
}

// RunOidcReactivateAudiencesRoundTrip verifies POST
// /oauth/oidc/providers/{id}/reactivate surfaces expectedAudiences and
// rolesClaim in the response. The provider is registered with both
// fields, invalidated, then reactivated.
func RunOidcReactivateAudiencesRoundTrip(t *testing.T, fix BackendFixture) {
	tenant := fix.NewTenant(t)
	c := client.NewClient(fix.BaseURL(), tenant.Token)

	uri := oidcWellKnownURI(tenant.ID, "reactivate-aud-roles")
	wantAud := []string{"aud-react"}
	wantRoles := "roles-react"

	registered, err := c.RegisterOidcProvider(t, map[string]any{
		"wellKnownConfigUri": uri,
		"expectedAudiences":  wantAud,
		"rolesClaim":         wantRoles,
	})
	if err != nil {
		t.Fatalf("RegisterOidcProvider: %v", err)
	}

	if err := c.InvalidateOidcProvider(t, registered.ID); err != nil {
		t.Fatalf("InvalidateOidcProvider: %v", err)
	}

	reactivated, err := c.ReactivateOidcProvider(t, registered.ID)
	if err != nil {
		t.Fatalf("ReactivateOidcProvider: %v", err)
	}

	if reactivated.ExpectedAudiences == nil || !reflect.DeepEqual(*reactivated.ExpectedAudiences, wantAud) {
		t.Errorf("expectedAudiences after reactivate: got %v, want %v", reactivated.ExpectedAudiences, wantAud)
	}
	if reactivated.RolesClaim == nil || *reactivated.RolesClaim != wantRoles {
		t.Errorf("rolesClaim after reactivate: got %v, want %q", reactivated.RolesClaim, wantRoles)
	}
}
```

Add `"reflect"` to the imports at the top of the file if not already present.

- [ ] **Step 2: Register the 4 new scenarios in `e2e/parity/registry.go`**

Find the comment `// Phase 9.2 — OIDC CRUD + authz (#284)` and the existing OIDC entries (around line 121–140). Add a new comment block + 4 entries directly after the existing `OidcRegister` / `OidcListAll` etc. group, before the negative-path entries:

```go
	// D7 — expectedAudiences + rolesClaim round-trip (response-mapper bug)
	{"OidcRegisterAudiencesRoundTrip", RunOidcRegisterAudiencesRoundTrip},
	{"OidcListAudiencesRoundTrip", RunOidcListAudiencesRoundTrip},
	{"OidcUpdateAudiencesRoundTrip", RunOidcUpdateAudiencesRoundTrip},
	{"OidcReactivateAudiencesRoundTrip", RunOidcReactivateAudiencesRoundTrip},
```

Also update the scenario-count comment at the top of the file: change `Total parity scenarios: 135` → `Total parity scenarios: 139`.

- [ ] **Step 3: Run the new tests and confirm RED**

Run from the worktree root:

```bash
go test -short -run "Audiences" -v ./internal/e2e/...
```

Expected output: all four `*AudiencesRoundTrip` scenarios FAIL with `expectedAudiences must be present in response (was nil)` or `rolesClaim must be present in response (was nil)`.

If they pass: the bug isn't reproducing — re-read `internal/domain/account/oidc_adapter.go:402-415` to confirm the mapper still drops the fields. Don't proceed until red is verified.

### 1.2 — Fix the mapper (GREEN)

- [ ] **Step 4: Modify `toOidcProviderResponseDto` in `internal/domain/account/oidc_adapter.go`**

Replace the existing function (and its preceding comment block):

```go
// toOidcProviderResponseDto maps a domain OidcProvider to the wire DTO.
// Fields not yet in the generated DTO (expectedAudiences, rolesClaim) are
// skipped until Task 8.1 adds them to the OpenAPI spec.
func toOidcProviderResponseDto(p *oidc.OidcProvider) genapi.OidcProviderResponseDto {
	dto := genapi.OidcProviderResponseDto{
		Id:                 p.ID,
		WellKnownConfigUri: p.WellKnownConfigURI,
		Active:             p.Active(),
		CreatedAt:          p.CreatedAt,
	}
	if len(p.Issuers) > 0 {
		issuers := make([]string, len(p.Issuers))
		copy(issuers, p.Issuers)
		dto.Issuers = &issuers
	}
	return dto
}
```

With:

```go
// toOidcProviderResponseDto maps a domain OidcProvider to the wire DTO.
// Every field declared in the OpenAPI OidcProviderResponseDto schema is
// populated. Slice fields are defensively copied so the response is
// independent of the caller's view of the domain object.
func toOidcProviderResponseDto(p *oidc.OidcProvider) genapi.OidcProviderResponseDto {
	dto := genapi.OidcProviderResponseDto{
		Id:                 p.ID,
		WellKnownConfigUri: p.WellKnownConfigURI,
		Active:             p.Active(),
		CreatedAt:          p.CreatedAt,
	}
	if len(p.Issuers) > 0 {
		issuers := make([]string, len(p.Issuers))
		copy(issuers, p.Issuers)
		dto.Issuers = &issuers
	}
	if len(p.ExpectedAudiences) > 0 {
		audiences := make([]string, len(p.ExpectedAudiences))
		copy(audiences, p.ExpectedAudiences)
		dto.ExpectedAudiences = &audiences
	}
	if p.RolesClaim != nil && *p.RolesClaim != "" {
		s := *p.RolesClaim
		dto.RolesClaim = &s
	}
	return dto
}
```

> **Note on the `RolesClaim` shape check:** Read `internal/auth/oidc/types.go` first to confirm whether `OidcProvider.RolesClaim` is `*string`, `string`, or `[]string`. The snippet above assumes `*string`. If it is a non-pointer `string`, replace the `if p.RolesClaim != nil ...` block with `if p.RolesClaim != "" { s := p.RolesClaim; dto.RolesClaim = &s }`.

- [ ] **Step 5: Run the parity tests again and confirm GREEN**

```bash
go test -short -run "Audiences" -v ./internal/e2e/...
```

Expected: all four `*AudiencesRoundTrip` scenarios PASS.

- [ ] **Step 6: Run the full OIDC scenario set and the broader root-module unit tests to confirm no regressions**

```bash
go test -short -run "Oidc" -v ./internal/e2e/...
go test -short ./... -v 2>&1 | tail -30
```

Expected: all tests pass. No new failures introduced.

### 1.3 — Cleanup (Gate 6)

- [ ] **Step 7: Verify the stale "Task 8.1" comment is gone**

```bash
grep -n "Task 8.1" internal/domain/account/oidc_adapter.go
```

Expected: no output. (The replacement in Step 4 dropped it; this confirms.)

### 1.4 — Commit

- [ ] **Step 8: Commit**

```bash
git add internal/domain/account/oidc_adapter.go e2e/parity/oidc.go e2e/parity/registry.go
git -c commit.gpgsign=false commit -m "fix(iam): OIDC response mapper emits expectedAudiences + rolesClaim

The toOidcProviderResponseDto mapper silently dropped both fields
despite the generated DTO declaring them in OidcProviderResponseDto
and the OpenAPI spec requiring them. The stale comment framed this
as deferred 'Task 8.1' work; the DTO and spec were both already in
place — this was silent data loss in a public response.

Adds 4 parity scenarios (round-trip through register / list / update
/ reactivate, the 4 endpoints that return OidcProviderResponseDto)
and fixes the mapper. All scenarios run against every storage
backend via the parity registry.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Task 2: openapi.md OIDC-status rewrite (commit 2)

**Files:**
- Modify: `cmd/cyoda/help/content/openapi.md` (the line with "remain 501 until v0.9.0")

### 2.1 — Edit the stale line

- [ ] **Step 1: Replace line 96 of `cmd/cyoda/help/content/openapi.md`**

The current text (around line 96):

```
- **IAM** — OAuth token and key management under `/oauth/`. As of v0.8.0, the 10 `/oauth/keys/*` admin operations (keypair + trusted-key lifecycle) are conformant. OIDC providers under `/oauth/oidc/providers/*` remain 501 until v0.9.0.
```

Replace with:

```
- **IAM** — OAuth token and key management under `/oauth/`. As of v0.8.0, the 10 `/oauth/keys/*` admin operations (keypair + trusted-key lifecycle), the 7 `/oauth/oidc/providers/*` operations, and the `/clients` operations are conformant. Remaining stubbed surface across `/account/*` and adjacent paths is tracked in `docs/cyoda/cloud-divergences.md`.
```

- [ ] **Step 2: Verify help_test.go still passes (front-matter unchanged, body still well-formed)**

```bash
go test -short ./cmd/cyoda/help/... -v 2>&1 | tail -15
```

Expected: PASS.

- [ ] **Step 3: Smoke-render the openapi topic to confirm the new text reads right**

```bash
go build -o bin/cyoda ./cmd/cyoda && bin/cyoda help openapi 2>&1 | sed -n '/IAM/,/SQL Schema/p'
```

Expected: the IAM bullet shows the new text; "501 until v0.9.0" is gone.

### 2.2 — Commit

- [ ] **Step 4: Commit**

```bash
git add cmd/cyoda/help/content/openapi.md
git -c commit.gpgsign=false commit -m "docs(help): rewrite openapi.md OIDC status as v0.8.0 conformant

The IAM tag bullet still claimed OIDC providers under
/oauth/oidc/providers/* remain 501 until v0.9.0. That stopped being
true when #314 landed on release/v0.8.0; all 7 endpoints are wired
through internal/domain/account/oidc_adapter.go. Updated to reflect
v0.8.0 conformance (keys + OIDC + clients) and point readers at
cloud-divergences.md for the remaining stubs.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Task 3: docs/cyoda/README.md documenting both spec layers (commit 3)

**Files:**
- Create: `docs/cyoda/README.md`

### 3.1 — Write the README

- [ ] **Step 1: Create `docs/cyoda/README.md`**

```markdown
# docs/cyoda — Cyoda Cloud OpenAPI reference

This directory is a **read-only reference** mirror of the Cyoda Cloud
OpenAPI spec, used by cyoda-go for parity work. Nothing in this
directory is served at runtime — cyoda-go's authoritative, embedded
spec lives at `api/openapi.yaml` at the repo root.

## Layout

Two coexisting layers, each with a distinct purpose:

### `api/` — upstream-split mirror (parity reference)

```
api/openapi.yml             # aggregator
api/openapi-audit.yml       # audit operations
api/openapi-common.yml      # common schemas
api/openapi-entity-search.yml
api/openapi-iam.yml         # OAuth, OIDC, technical users
api/openapi-workflow.yml
```

Byte-identical mirror of `Cyoda-platform/cyoda` `develop` branch at
`client/src/main/resources/api/`. This is the form maintainers read
when checking parity ("does cyoda-go's `OidcProviderResponseDto`
match upstream?"). The split files are easier to diff against
upstream than the bundled form.

### `openapi.yml` — resolved bundle (citation anchor)

A single, all-refs-resolved bundle of the same content. Kept because
the parity test client and one handler test cite specific line
numbers as the canonical source-of-truth anchor:

```go
// Canonical: docs/cyoda/openapi.yml:1055 (getOneEntity).
```

These citations appear ~20 times across `e2e/parity/client/http.go`,
`e2e/parity/client/types.go`, and
`internal/domain/entity/handler_create_collection_chunking_test.go`.
The split files have different line numbers, so removing the bundle
would invalidate every citation.

## What lives where

| Topic | File |
|---|---|
| Cross-cloud parity notes (known divergences) | `cloud-divergences.md` |
| Range-index / dindex storage schema | `dindex-range-index-tables.md` |
| gRPC protobuf reference | `proto/` |
| JSON Schemas referenced by the spec | `schema/` |

## Sync procedure

When upstream Cloud spec changes, refresh both layers:

```bash
# 1. Refresh the split files from upstream
UPSTREAM=/path/to/Cyoda-platform/cyoda/client/src/main/resources/api
cp -r "$UPSTREAM"/* docs/cyoda/api/

# 2. Re-bundle. The bundled form is produced by the same tool the
#    upstream uses to publish its consolidated spec — currently
#    redocly-cli (`redocly bundle docs/cyoda/api/openapi.yml -o
#    docs/cyoda/openapi.yml`). If the tool changes upstream, follow
#    suit.

# 3. Verify citations still resolve. Line numbers will shift; the
#    citation comments in e2e/parity/client/*.go and
#    internal/domain/entity/handler_create_collection_chunking_test.go
#    must be updated to match the new line numbers.

# 4. Commit with a message naming the upstream commit SHA you synced from.
```

## What this directory is not

- **Not** the spec cyoda-go serves. That is `api/openapi.yaml` at repo
  root, embedded via `//go:embed` and exposed at `/api/v3/api-docs`.
- **Not** a binding contract. cyoda-go deliberately diverges from
  Cloud in specific places — see `cloud-divergences.md` for the
  canonical list.
- **Not** edited in place. All changes flow from upstream Cloud
  `develop` via the sync procedure above.
```

- [ ] **Step 2: Verify the file is valid markdown and gitignore-safe**

```bash
ls -la docs/cyoda/README.md
head -5 docs/cyoda/README.md
git check-ignore docs/cyoda/README.md && echo "OOPS: ignored" || echo "OK: tracked"
```

Expected: file exists, starts with `# docs/cyoda`, is tracked.

### 3.2 — Commit

- [ ] **Step 3: Commit**

```bash
git add docs/cyoda/README.md
git -c commit.gpgsign=false commit -m "docs(cyoda): add README documenting both spec layers

The docs/cyoda directory holds two coexisting OpenAPI artefacts —
the split-file mirror (parity reference) and the resolved bundle
(line-number anchor for ~20 'Canonical: docs/cyoda/openapi.yml:NNN'
citations in parity tests and a handler test). Without a README a
future maintainer has to re-derive why both forms exist. Documents
both, the sync procedure from upstream Cyoda-platform/cyoda develop,
and explicitly states this directory is read-only reference (not the
spec cyoda-go serves).

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Task 4: cloud-divergences.md row rewrite with snapshot + derivation (commit 4)

The current row claims "22 IAM/OAuth/OIDC/account stub endpoints". That count is stale across PRs #312 (clients), #314 (OIDC), and the keys-conformance work. Replace with a current snapshot AND a re-derivation snippet.

**Files:**
- Modify: `docs/cyoda/cloud-divergences.md`

### 4.1 — Derive the current snapshot

- [ ] **Step 1: Build a current cyoda binary**

```bash
go build -o bin/cyoda ./cmd/cyoda
```

- [ ] **Step 2: Start the binary in JWT mode against an ephemeral sqlite DB**

```bash
# Use a fresh temp DB; emit the bootstrap token so we can probe
export CYODA_STORAGE_BACKEND=sqlite
export CYODA_SQLITE_PATH=$(mktemp -t cyoda-stubs-XXXXXX.db)
export CYODA_IAM_MODE=jwt
export CYODA_JWT_SIGNING_KEY="$(openssl genrsa 2048 2>/dev/null)"
export CYODA_BOOTSTRAP_CLIENT_ID=stub-probe
export CYODA_BOOTSTRAP_CLIENT_SECRET=stub-probe-secret
export CYODA_HTTP_PORT=18080
export CYODA_ADMIN_PORT=18081
export CYODA_SUPPRESS_BANNER=true
bin/cyoda serve &
CYODA_PID=$!
sleep 2
```

- [ ] **Step 3: Acquire a bootstrap token**

```bash
TOKEN=$(curl -s -u "$CYODA_BOOTSTRAP_CLIENT_ID:$CYODA_BOOTSTRAP_CLIENT_SECRET" \
  -d "grant_type=client_credentials" \
  http://127.0.0.1:18080/api/oauth/token | python3 -c 'import json,sys;print(json.load(sys.stdin)["access_token"])')
echo "TOKEN=${TOKEN:0:24}..."
```

Expected: prints a JWT prefix.

- [ ] **Step 4: Run the derivation snippet — probe every spec operation, capture 501s**

```bash
# Extract (method, path) tuples from the spec, then probe each with a
# minimal request. A 501 response means the binary still has the
# unimplemented.go stub mounted for that operation; any other status
# (including 4xx) means the operation is implemented.

python3 - <<'EOF'
import re, urllib.request, urllib.error, json, sys
spec = open("api/openapi.yaml").read()
# Pull paths and the verbs declared under each. Simple line-anchored
# extraction is enough for this spec's shape.
ops = []
path = None
for line in spec.splitlines():
    m = re.match(r"^  (/\S+):", line)
    if m:
        path = m.group(1)
        continue
    m = re.match(r"^    (get|post|put|patch|delete):", line)
    if m and path:
        ops.append((m.group(1).upper(), path))

token = "TOKEN_PLACEHOLDER"
stubs = []
for method, path in ops:
    # Concretise path params with throwaway placeholders
    concrete = re.sub(r"\{[^}]+\}", "00000000-0000-0000-0000-000000000000", path)
    url = f"http://127.0.0.1:18080/api{concrete}"
    req = urllib.request.Request(url, method=method)
    req.add_header("Authorization", f"Bearer {token}")
    if method in ("POST", "PUT", "PATCH"):
        req.add_header("Content-Type", "application/json")
        req.data = b"{}"
    try:
        urllib.request.urlopen(req, timeout=2)
        code = 200
    except urllib.error.HTTPError as e:
        code = e.code
    except Exception:
        code = -1
    if code == 501:
        stubs.append(f"{method} {path}")

print(f"# {len(stubs)} stubbed operations in v0.8.0:")
for s in sorted(stubs):
    print(f"- `{s}`")
EOF
```

> Replace `TOKEN_PLACEHOLDER` with the actual `$TOKEN` value from Step 3 before running, or wrap the python in a shell heredoc that interpolates it.

Capture the output verbatim — it becomes the "current snapshot" portion of the new row.

- [ ] **Step 5: Shut down the binary**

```bash
kill $CYODA_PID
unset CYODA_STORAGE_BACKEND CYODA_SQLITE_PATH CYODA_IAM_MODE CYODA_JWT_SIGNING_KEY
unset CYODA_BOOTSTRAP_CLIENT_ID CYODA_BOOTSTRAP_CLIENT_SECRET CYODA_HTTP_PORT
unset CYODA_ADMIN_PORT CYODA_SUPPRESS_BANNER
```

### 4.2 — Edit the divergences row

- [ ] **Step 6: Replace the "22 stubs" row in `docs/cyoda/cloud-divergences.md`**

Find the existing row:

```
| 22 IAM/OAuth/OIDC/account stub endpoints | Declared in OpenAPI as `501 Not Implemented`; handlers return 501 by design (per ADR 0001's A+C policy on the conformance reconciliation). | Deferred. | [#194](https://github.com/Cyoda-platform/cyoda-go/issues/194) |
```

Replace with (filling `<N>` with the count from Step 4 and the list-of-endpoints with the actual output):

```
| Remaining 501 Not Implemented endpoints | Declared in OpenAPI but unhandled at runtime. As of v0.8.0 the keypair + trusted-key (`/oauth/keys/*`), OIDC provider (`/oauth/oidc/providers/*`), and `/clients` surfaces are conformant; the `<N>` endpoints below still return 501 via `internal/api/unimplemented.go`. Re-derive with the snippet beneath this table whenever IAM/account surfaces move. | Deferred. | [#194](https://github.com/Cyoda-platform/cyoda-go/issues/194) |

**Current 501 snapshot** (rerun the derivation below to refresh):

<paste the bullet list from Step 4 output here>

**Derivation.** Spin up a fresh cyoda binary in JWT mode and probe every
spec operation; record those that still respond `501`:

```bash
go build -o bin/cyoda ./cmd/cyoda
# (start cyoda + acquire a bootstrap token — see
# docs/superpowers/plans/2026-06-17-auth-help-topic.md task 4 for the
# full recipe)
python3 - <<'EOF'
# (the derivation snippet from the plan, with TOKEN populated)
EOF
```
```

> Keep the prose tight — the snippet is canonical, the snapshot is its current output. A future maintainer should be able to refresh by running the snippet, not by hand-editing the list.

- [ ] **Step 7: Smoke-verify the divergences page is well-formed**

```bash
head -50 docs/cyoda/cloud-divergences.md
```

Expected: table renders; the new row replaces the old one cleanly.

### 4.3 — Commit

- [ ] **Step 8: Commit**

```bash
git add docs/cyoda/cloud-divergences.md
git -c commit.gpgsign=false commit -m "docs(cyoda): rewrite cloud-divergences IAM-stubs row with snapshot + derivation

The 'unimplemented' count was stale across #312 (clients), #314 (OIDC),
and the keys-conformance work. Replaced the fixed count with the
current 501 endpoint snapshot AND a shell snippet that re-derives it
by probing a running cyoda binary against every spec operation. The
snippet is the canonical source; the snapshot is its current output.

This row no longer goes stale on every IAM-surface PR — it goes stale
when somebody forgets to re-run the snippet. The friction asymmetry
favours accuracy.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Task 5: new auth/* help topics (commit 5)

Five new markdown files. The help subsystem is filesystem-driven (per `cmd/cyoda/help/help.go` `loadInto`), so no Go code change beyond test additions.

**Files:**
- Create: `cmd/cyoda/help/content/auth.md`
- Create: `cmd/cyoda/help/content/auth/clients.md`
- Create: `cmd/cyoda/help/content/auth/tokens.md`
- Create: `cmd/cyoda/help/content/auth/oidc.md`
- Create: `cmd/cyoda/help/content/auth/trusted-keys.md`
- Modify: `cmd/cyoda/help/help_test.go` (add 5 load-resolve cases)

### 5.1 — Write the landing page

- [ ] **Step 1: Create `cmd/cyoda/help/content/auth.md`**

```markdown
---
topic: auth
title: "auth — authenticate client applications against cyoda"
stability: evolving
version_added: 0.8.0
see_also:
  - config.auth
  - openapi
---

# auth

## NAME

auth — authenticate client applications against cyoda.

## GOAL

Every cyoda API call needs an `Authorization: Bearer <jwt>` header. This page helps you decide how to get that JWT.

## WHICH PATH DO I NEED?

| You have | Use | Subtopic |
|---|---|---|
| `client_id`/`secret` you'll manage in cyoda | M2M client + token endpoint | `auth.clients` + `auth.tokens` |
| an existing IdP (Cognito, Keycloak, Auth0, …) | federated OIDC | `auth.oidc` |
| a key you sign tokens with yourself, no IdP | trusted-keys | `auth.trusted-keys` |
| an M2M client acting on behalf of a user | token-exchange grant | `auth.tokens` (OBO section) |

> **Looking for OBO?** The token-exchange (on-behalf-of) grant is documented as a section of `auth.tokens` — there is no separate `auth.obo` page. Run `cyoda help auth tokens` and read the token-exchange section.

> **Looking for env vars?** All `CYODA_OIDC_*`, `CYODA_IAM_*`, `CYODA_JWT_*`, `CYODA_HMAC_*`, and `CYODA_BOOTSTRAP_*` knobs live in `config.auth`. Run `cyoda help config auth`.

## TOKEN PRESENTATION

All cyoda APIs accept the JWT via `Authorization: Bearer <token>`. The token claim shape — `sub`, `iss`, `caas_org_id`, `caas_user_id`, `user_roles`, `caas_tier`, `exp`, `iat`, `jti`, optionally `act` — is documented in `auth.tokens`.

## SEE ALSO

- `config.auth` — env-var reference for every auth knob
- `openapi` — run `cyoda help openapi tags` for spec by tag, including OAuth and OIDC
- `errors.UNAUTHORIZED`, `errors.FORBIDDEN` — universal auth-failure codes
```

### 5.2 — Write `auth/clients.md`

- [ ] **Step 2: Create `cmd/cyoda/help/content/auth/clients.md`**

```markdown
---
topic: auth.clients
title: "auth.clients — M2M client lifecycle"
stability: evolving
version_added: 0.8.0
see_also:
  - auth
  - auth.tokens
  - config.auth
  - errors.M2M_CLIENT_NOT_FOUND
  - errors.FEATURE_DISABLED
---

# auth.clients

## NAME

auth.clients — provision and manage machine-to-machine (M2M) clients that authenticate against cyoda via the `client_credentials` grant.

## GOAL

You want a backend service or CI job to call cyoda APIs. Register an M2M client to obtain a `client_id` + `client_secret`. Your service then mints JWTs via `POST /api/oauth/token` (documented in `auth.tokens`) and presents them as `Authorization: Bearer …` on every request.

Use this path when you control both the service and its cyoda registration. For user-facing flows, federate via `auth.oidc` instead.

## PREREQUISITES

**Admin (cyoda operator) sets up:**

- `CYODA_IAM_MODE=jwt` (mock mode bypasses auth entirely — fine for dev, never for prod)
- `CYODA_JWT_SIGNING_KEY` (PEM RSA key; `_FILE` suffix supported)
- Optionally: `CYODA_BOOTSTRAP_CLIENT_ID` + `CYODA_BOOTSTRAP_CLIENT_SECRET` provisions a single admin M2M at startup, useful for CI. See `config.auth`.
- For admin-scoped M2M creation (`withAdminRole=true`): `CYODA_IAM_M2M_ADMIN_ROLE_ENABLED=true`. Off by default; when off, the `withAdminRole=true` request shape returns `404 FEATURE_DISABLED`.

**Client (you) needs:**

- An `Authorization: Bearer …` token with `ROLE_ADMIN` to create or delete clients. (`GET /clients` requires only `ROLE_M2M`.)
- A target tenant ID — the client is scoped to the caller's tenant.

## REQUEST FLOW

### Provision a client

```bash
curl -X POST https://cyoda.example.com/api/clients \
  -H "Authorization: Bearer ${ADMIN_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{
        "name": "billing-svc",
        "description": "billing service backend"
      }'
```

Response (`200 OK`):

```json
{
  "clientId": "c1a3b9e0-…",
  "clientSecret": "1f4e…",
  "name": "billing-svc",
  "tenantId": "00000000-…",
  "roles": ["ROLE_M2M"]
}
```

> **`clientSecret` is shown only at creation time.** Capture it now; the server cannot return it again.

### List clients in the caller's tenant

```bash
curl -X GET https://cyoda.example.com/api/clients \
  -H "Authorization: Bearer ${TOKEN}"
```

Returns `[]` of clients (no secrets — clients carry `clientId`, `name`, `tenantId`, `roles`).

### Delete a client

```bash
curl -X DELETE https://cyoda.example.com/api/clients/${CLIENT_ID} \
  -H "Authorization: Bearer ${ADMIN_TOKEN}"
```

Response: `204 No Content`. The deleted client's tokens remain valid until their natural `exp`; deletion stops new token issuance.

## TOKEN

Clients are not tokens. After provisioning, the client uses `auth.tokens` (the `/oauth/token` endpoint) to mint JWTs. The JWT carries the client's `tenantId` in `caas_org_id` and its roles in `user_roles`. Full claim shape is in `auth.tokens`.

## ERRORS

- `errors.FORBIDDEN` (`403`) — caller lacks `ROLE_ADMIN` for create/delete; `ROLE_M2M` is required for list.
- `errors.M2M_CLIENT_NOT_FOUND` (`404`) — referenced `clientId` does not exist or belongs to a different tenant.
- `errors.FEATURE_DISABLED` (`404`) — `withAdminRole=true` requested with `CYODA_IAM_M2M_ADMIN_ROLE_ENABLED=false`.
- `errors.BAD_REQUEST` (`400`) — request body shape invalid.
- `errors.UNAUTHORIZED` (`401`) — bearer token missing, expired, or untrusted.

## SEE ALSO

- `auth.tokens` — the `/oauth/token` endpoint and JWT claim contract
- `config.auth` — `CYODA_BOOTSTRAP_*`, `CYODA_IAM_M2M_ADMIN_ROLE_ENABLED`
- `openapi` — `cyoda help openapi tags` and look for the `User, Machine` tag
```

### 5.3 — Write `auth/tokens.md`

- [ ] **Step 3: Create `cmd/cyoda/help/content/auth/tokens.md`**

```markdown
---
topic: auth.tokens
title: "auth.tokens — /oauth/token grants and JWT claim contract"
stability: evolving
version_added: 0.8.0
see_also:
  - auth
  - auth.clients
  - auth.oidc
  - auth.trusted-keys
  - config.auth
  - errors.UNAUTHORIZED
  - errors.FORBIDDEN
---

# auth.tokens

## NAME

auth.tokens — exchange credentials for a JWT at `POST /api/oauth/token`. Covers every supported grant and the canonical JWT claim contract that all cyoda tokens (M2M, OBO, federated OIDC, trusted-key) conform to.

## GOAL

You have a way to prove identity (an M2M `client_id`/`secret`, or a JWT minted by a federated IdP, or a JWT you signed offline) and you want a cyoda-issued (or cyoda-validated) JWT to present on subsequent API calls.

This is the single home for the JWT claim contract. `auth.oidc` and `auth.trusted-keys` link here for claim shape.

## PREREQUISITES

**Admin (cyoda operator) sets up:**

- `CYODA_IAM_MODE=jwt`
- `CYODA_JWT_SIGNING_KEY` (PEM RSA private key; tokens cyoda issues are signed with this)
- `CYODA_JWT_ISSUER` (default `cyoda`; populates the `iss` claim)
- `CYODA_JWT_AUDIENCE` (default empty = no `aud` check on inbound tokens)
- `CYODA_JWT_EXPIRY_SECONDS` (default `3600`)
- `CYODA_JWT_BOOTSTRAP_AUDIENCE` (default `client`; controls which key signs M2M tokens)

See `config.auth` for the full env-var reference.

**Client (you) needs:**

- For `client_credentials`: a registered M2M `client_id`/`secret` (see `auth.clients`).
- For token-exchange (OBO): an already-valid subject JWT plus an M2M `client_id`/`secret` to act as the actor.

## REQUEST FLOW

### client_credentials — most common

Mint an M2M JWT with your client credentials:

```bash
curl -X POST https://cyoda.example.com/api/oauth/token \
  -u "${CLIENT_ID}:${CLIENT_SECRET}" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "grant_type=client_credentials"
```

Response (`200 OK`):

```json
{
  "access_token": "eyJhbGciOiJSUzI1NiIs…",
  "token_type":   "Bearer",
  "expires_in":   3600
}
```

Use the `access_token` as `Authorization: Bearer …` on every subsequent API call. Mint again when it nears `exp`; cyoda does not issue refresh tokens.

### token-exchange (OBO)

You are an M2M actor (e.g. a backend service) and you want to call cyoda **on behalf of a user** whose token you already hold. The OBO grant re-signs the subject token so cyoda sees the user as the principal and your service as the actor (RFC 8693).

```bash
curl -X POST https://cyoda.example.com/api/oauth/token \
  -u "${CLIENT_ID}:${CLIENT_SECRET}" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "grant_type=urn:ietf:params:oauth:grant-type:token-exchange" \
  -d "subject_token=${USER_TOKEN}" \
  -d "subject_token_type=urn:ietf:params:oauth:token-type:jwt"
```

Response shape matches `client_credentials` plus a `issued_token_type` field:

```json
{
  "access_token":      "eyJhbGciOiJSUzI1NiIs…",
  "token_type":        "Bearer",
  "expires_in":        3600,
  "issued_token_type": "urn:ietf:params:oauth:token-type:jwt"
}
```

Key constraints:

- The subject token's `caas_org_id` must match the M2M client's tenant. Tenant mismatch → `403 access_denied`.
- The issued OBO token carries `sub` = the subject's `sub`, `user_roles` from the subject token, and an `act` claim `{"sub": "<m2m client_id>"}` identifying the actor.
- Subject token must already be valid (signature, not expired).

## TOKEN

Every JWT cyoda accepts — whether minted via `client_credentials`, OBO, federated OIDC (`auth.oidc`), or trusted-key offline signing (`auth.trusted-keys`) — conforms to this claim shape:

| Claim | Type | Meaning |
|---|---|---|
| `sub` | string | Principal — `client_id` for M2M, user ID for OBO and federated tokens. |
| `iss` | string | Issuer. Cyoda-minted tokens use `CYODA_JWT_ISSUER`. Federated tokens use the upstream IdP's issuer. |
| `aud` | string or string[] | Audience. Checked against `CYODA_JWT_AUDIENCE` if set; against `expectedAudiences` for federated providers. |
| `exp` | int (unix) | Expiry. |
| `iat` | int (unix) | Issued-at. |
| `jti` | string (UUID) | Unique token ID. |
| `caas_org_id` | string (UUID) | Tenant scope. Every API call is constrained to this tenant. |
| `caas_user_id` | string | User identifier. For M2M tokens this duplicates `sub` (= `client_id`). |
| `user_roles` | string[] | Roles granted (e.g. `ROLE_ADMIN`, `ROLE_M2M`). Federated OIDC tokens carry roles from the provider's configured `rolesClaim` (default `roles`; per-provider override available — see `auth.oidc`). |
| `caas_tier` | string | Tier label (cyoda-go: always `"unlimited"`; Cloud distinguishes paid tiers). |
| `act` | object | **OBO only.** `{"sub": "<m2m client_id>"}` — identifies the M2M actor that exchanged the user token. Absent on `client_credentials` tokens. |

Cyoda issues tokens signed with `CYODA_JWT_SIGNING_KEY` (RS256). The `kid` header points at the active keypair in the keystore (`/oauth/keys/*`). Federated OIDC tokens are validated against the registered provider's JWKS — never signed by cyoda. Trusted-key tokens are signed by you (offline) with the matching private key for a registered public key.

## ERRORS

- `errors.UNAUTHORIZED` (`401`) — `Authorization` header missing, token expired, signature invalid, issuer untrusted, or `kid` not in any registered keystore / OIDC JWKS / trusted-key registry.
- `errors.FORBIDDEN` (`403`) — token valid but caller lacks the required role for the operation.
- `errors.BAD_REQUEST` (`400`) — malformed `grant_type`, missing form fields, invalid `subject_token` shape.
- The `/oauth/token` endpoint returns OAuth-shaped errors (`{"error": "...", "error_description": "..."}`) per RFC 6749 rather than the generic cyoda error envelope — `invalid_client`, `invalid_grant`, `access_denied`, `server_error`.

## SEE ALSO

- `auth.clients` — provision the M2M client used by `client_credentials` and OBO
- `auth.oidc` — federate an external IdP whose JWTs cyoda will accept directly
- `auth.trusted-keys` — register a public key so JWTs you sign offline are accepted
- `config.auth` — `CYODA_JWT_*`, `CYODA_BOOTSTRAP_*`
- `openapi` — `cyoda help openapi tags` and look for the `IAM` tag
```

### 5.4 — Write `auth/oidc.md`

- [ ] **Step 4: Create `cmd/cyoda/help/content/auth/oidc.md`**

```markdown
---
topic: auth.oidc
title: "auth.oidc — federated OIDC providers"
stability: evolving
version_added: 0.8.0
see_also:
  - auth
  - auth.tokens
  - config.auth
  - errors.OIDC_DISCOVERY_FAILED
  - errors.OIDC_JWKS_UNAVAILABLE
  - errors.OIDC_PROVIDER_DUPLICATE
  - errors.OIDC_PROVIDER_NOT_FOUND
  - errors.OIDC_PROVIDER_INACTIVE
  - errors.OIDC_SSRF_BLOCKED
  - errors.OIDC_INVALID_TENANT
  - errors.OIDC_AUDIENCE_MISMATCH
  - errors.OIDC_CLAIMS_INVALID
  - errors.OIDC_TOKEN_PRE_TRANSITION
---

# auth.oidc

## NAME

auth.oidc — register an external OIDC identity provider so JWTs it issues are accepted by cyoda directly, without re-minting at `/oauth/token`.

## GOAL

You already have an OIDC IdP — Cognito, Keycloak, Auth0, Okta, your own — and you want clients to present that IdP's JWTs to cyoda. Register the provider once; cyoda fetches its JWKS, validates inbound tokens against the keys, maps roles from the configured claim, and binds the resulting identity to a tenant.

Use this path when the IdP is the source of truth for user accounts. For pure M2M, `auth.clients` + `auth.tokens` is simpler.

## PREREQUISITES

**Admin (cyoda operator) sets up:**

- `CYODA_IAM_MODE=jwt` (federated OIDC is unavailable in mock mode).
- `CYODA_OIDC_REQUIRE_HTTPS=true` for production. Set to `false` only for dev IdPs over plain HTTP.
- `CYODA_OIDC_ALLOW_PRIVATE_NETWORKS=false` for production. Setting to `true` disables the SSRF blocklist that prevents `wellKnownConfigUri` resolving to RFC 1918 / loopback / link-local addresses.
- `CYODA_OIDC_ROLES_CLAIM` (default `roles`) — global default JWT claim from which roles are read. Per-provider override available at registration.
- `CYODA_OIDC_CONNECT_TIMEOUT_MS`, `CYODA_OIDC_SOCKET_TIMEOUT_MS`, `CYODA_OIDC_CONNECTION_REQUEST_TIMEOUT_MS` (each default `5000`) — HTTP timeouts for discovery + JWKS fetches.

**Client (you) needs:**

- A working `.well-known/openid-configuration` URL on the IdP.
- A `ROLE_ADMIN` cyoda token to register the provider.
- A **UUID-shaped tenant ID** — the bootstrap convenience literal `default-tenant` is rejected by registration (returns `OIDC_INVALID_TENANT`) because non-UUID tenant identifiers collide in storage.

## REQUEST FLOW

### Register a provider

```bash
curl -X POST https://cyoda.example.com/api/oauth/oidc/providers \
  -H "Authorization: Bearer ${ADMIN_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{
        "wellKnownConfigUri": "https://idp.example.com/.well-known/openid-configuration",
        "issuers":            ["https://idp.example.com/"],
        "expectedAudiences":  ["cyoda-prod"],
        "rolesClaim":         "cognito:groups"
      }'
```

Response (`200 OK`) carries the full provider DTO:

```json
{
  "id":                 "f47ac10b-58cc-…",
  "wellKnownConfigUri": "https://idp.example.com/.well-known/openid-configuration",
  "active":             true,
  "createdAt":          "2026-06-17T12:00:00Z",
  "issuers":            ["https://idp.example.com/"],
  "expectedAudiences":  ["cyoda-prod"],
  "rolesClaim":         "cognito:groups"
}
```

Behaviour on `expectedAudiences`:

- Omitted, `null`, or empty array → no `aud` enforcement (issuer-binding is the trust anchor).
- Non-empty → inbound JWT `aud` claim must match at least one entry byte-wise.

Behaviour on `issuers`:

- Omitted, `null`, or empty → cyoda enforces the `iss` claim matches the discovery document's `issuer` field byte-wise (OIDC Core 1.0 §2).
- Non-empty → inbound JWT `iss` must match one of the listed values byte-wise.

### List providers

```bash
curl -X GET "https://cyoda.example.com/api/oauth/oidc/providers?activeOnly=true" \
  -H "Authorization: Bearer ${TOKEN}"
```

`activeOnly=true` filters out invalidated providers. Available to any authenticated tenant member, not just admin.

### Update a provider (tri-state PATCH)

```bash
curl -X PATCH https://cyoda.example.com/api/oauth/oidc/providers/${PROVIDER_ID} \
  -H "Authorization: Bearer ${ADMIN_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{
        "expectedAudiences": ["cyoda-prod", "cyoda-staging"]
      }'
```

Per-field tri-state semantics: absent = unchanged, `null` or `[]` = clear, value = set. Same for `issuers` and `rolesClaim`.

### Invalidate / reactivate / delete

```bash
# Invalidate — token validation stops accepting this provider's JWTs immediately.
curl -X POST https://cyoda.example.com/api/oauth/oidc/providers/${PROVIDER_ID}/invalidate \
  -H "Authorization: Bearer ${ADMIN_TOKEN}"

# Reactivate — by default refreshes JWKS from upstream.
curl -X POST https://cyoda.example.com/api/oauth/oidc/providers/${PROVIDER_ID}/reactivate \
  -H "Authorization: Bearer ${ADMIN_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{"reactivateKeys": true}'

# Delete permanently.
curl -X DELETE https://cyoda.example.com/api/oauth/oidc/providers/${PROVIDER_ID} \
  -H "Authorization: Bearer ${ADMIN_TOKEN}"
```

### Reload JWKS for all providers

```bash
curl -X POST https://cyoda.example.com/api/oauth/oidc/providers/reload \
  -H "Authorization: Bearer ${ADMIN_TOKEN}"
```

Forces an immediate JWKS refresh for every active provider on the receiving node. In a multi-node cluster the reload is broadcast.

### Present an IdP-issued JWT

Once registered, clients present the IdP's JWT directly:

```bash
curl -X GET https://cyoda.example.com/api/clients \
  -H "Authorization: Bearer ${IDP_ISSUED_JWT}"
```

The token is validated against the provider's JWKS; roles are read from the configured `rolesClaim`; identity is bound to the tenant that registered the provider.

## TOKEN

JWTs issued by the federated IdP must conform to the universal cyoda claim contract documented in `auth.tokens`. In particular:

- `iss` must match per the `issuers` / discovery-document rule above.
- `aud` must match `expectedAudiences` if set.
- The configured `rolesClaim` (per-provider override or `CYODA_OIDC_ROLES_CLAIM`) must yield a string array of role values.

Cyoda does not re-mint federated tokens — they are validated and trusted directly.

## DIAGNOSTICS

### "I get 401 but my token looks valid"

When two tenants register the **same IdP** with overlapping or empty `expectedAudiences`, cyoda cannot deterministically route an inbound token to one tenant rather than the other. Internally this is `ErrAmbiguousProvider` (in `internal/auth/oidc/registry.go`), but it is wrapped in `ErrUnknownKID` and surfaces as a generic `401 UNAUTHORIZED` — *not* a dedicated error code.

This is deliberate: a precise "two tenants claim your IdP" error would leak provider-routing topology across the tenant boundary. The diagnostic path is server-side logs (look for `oidc.registry` resolve events identifying the conflict), not the HTTP response.

To resolve: pick **disjoint** `expectedAudiences` for the same IdP across tenants, or accept that one tenant must register and the other federate through it.

### "JWKS warmup window"

After registering a provider, cyoda warms JWKS asynchronously. During the cold-start window (typically <1s), tokens whose `kid` is not yet cached fall through to `ErrUnknownKID` → 401. Retry; or force-warm with the `/reload` endpoint above.

## ERRORS

- `errors.OIDC_DISCOVERY_FAILED` (`502`) — `wellKnownConfigUri` unreachable or returned malformed JSON.
- `errors.OIDC_JWKS_UNAVAILABLE` (`502`) — discovery succeeded but the JWKS endpoint did not.
- `errors.OIDC_SSRF_BLOCKED` (`400`) — `wellKnownConfigUri` resolves to a blocked address range (set `CYODA_OIDC_ALLOW_PRIVATE_NETWORKS=true` for dev).
- `errors.OIDC_PROVIDER_DUPLICATE` (`400`) — same `wellKnownConfigUri` already registered for this tenant.
- `errors.OIDC_PROVIDER_NOT_FOUND` (`404`) — referenced provider ID absent in this tenant.
- `errors.OIDC_PROVIDER_INACTIVE` (`409`) — update or operation attempted on an invalidated provider; reactivate first.
- `errors.OIDC_INVALID_TENANT` (`400`) — caller's tenant ID is not UUID-shaped (commonly: bootstrap `default-tenant` literal).
- `errors.OIDC_AUDIENCE_MISMATCH` (`401`) — token `aud` does not match any `expectedAudiences`.
- `errors.OIDC_CLAIMS_INVALID` (`401`) — required claim missing or malformed.
- `errors.OIDC_TOKEN_PRE_TRANSITION` (`401`) — token `iat` precedes the most recent provider reactivation; mint a fresh one.
- `errors.UNAUTHORIZED` (`401`) — generic fallback (includes the ambiguous-routing case described in DIAGNOSTICS).

## SEE ALSO

- `auth.tokens` — universal claim contract
- `config.auth` — `CYODA_OIDC_*` env vars
- `openapi` — `cyoda help openapi tags` and look for `OAuth, OIDC Providers`
```

### 5.5 — Write `auth/trusted-keys.md`

- [ ] **Step 5: Create `cmd/cyoda/help/content/auth/trusted-keys.md`**

```markdown
---
topic: auth.trusted-keys
title: "auth.trusted-keys — register public keys for offline JWT signing"
stability: evolving
version_added: 0.8.0
see_also:
  - auth
  - auth.tokens
  - config.auth
  - errors.TRUSTED_KEY_NOT_FOUND
  - errors.TRUSTED_KEY_CAP_REACHED
  - errors.KEY_OWNED_BY_DIFFERENT_TENANT
  - errors.UNSUPPORTED_KEY_TYPE
  - errors.FEATURE_DISABLED
---

# auth.trusted-keys

## NAME

auth.trusted-keys — register a public key with cyoda so JWTs you sign offline with the matching private key are accepted, without an IdP or `/oauth/token` round-trip.

## GOAL

You have a workload that can sign JWTs (a CI job, a controller, an air-gapped service) and you don't want to depend on cyoda being reachable for token minting or an external IdP for discovery. Register the public key once; thereafter your workload signs JWTs locally and cyoda validates them by `kid` lookup against the registry.

Use this path when token minting must work even when cyoda's `/oauth/token` is unreachable, or when you want zero IdP infrastructure.

> **Feature flag.** The 5 trusted-key endpoints under `/oauth/keys/trusted/*` are **off by default**. The operator must set `CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED=true` to enable them; otherwise every endpoint returns `404 FEATURE_DISABLED`. This is intentional — trusted keys move the trust boundary, and that posture should be explicit.

## PREREQUISITES

**Admin (cyoda operator) sets up:**

- `CYODA_IAM_MODE=jwt`.
- `CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED=true` (gate; see callout above).
- `CYODA_IAM_TRUSTED_KEY_MAX_PER_TENANT` (default `10`) — per-tenant cap on currently-valid trusted keys. Counts active + within-validity entries only.
- `CYODA_IAM_TRUSTED_KEY_MAX_VALIDITY_DAYS` (default `90`) — default validity for trusted keys when not specified at registration.

**Client (you) needs:**

- A keypair you generated yourself. cyoda-go v0.8.0 supports `kty: "RSA"` only. Cloud also supports `kty: "EC"` and `kty: "OKP"`; cyoda-go parity is tracked for a future release.
- A `ROLE_ADMIN` cyoda token to register / delete / lifecycle the entry.

## REQUEST FLOW

### Register a public key

```bash
# Generate a keypair locally
openssl genrsa -out signing.pem 2048
openssl rsa -in signing.pem -pubout -out signing.pub
# Convert the public key to JWK shape — your tooling of choice;
# the API expects the JWK members at the top level.

curl -X POST https://cyoda.example.com/api/oauth/keys/trusted \
  -H "Authorization: Bearer ${ADMIN_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{
        "keyId": "my-signing-key-2026-06",
        "kty":   "RSA",
        "n":     "<base64url-modulus>",
        "e":     "AQAB"
      }'
```

Response (`200 OK`) echoes the registered key shape plus lifecycle metadata.

Pick a stable, descriptive `keyId`. It becomes the JWT `kid` header you must set when signing.

### List trusted keys

```bash
curl -X GET https://cyoda.example.com/api/oauth/keys/trusted \
  -H "Authorization: Bearer ${TOKEN}"
```

Returns the tenant's keys with status (active / invalidated) and validity window.

### Invalidate / reactivate

```bash
# Stop accepting tokens signed with this key, without removing the entry.
curl -X POST https://cyoda.example.com/api/oauth/keys/trusted/${KEY_ID}/invalidate \
  -H "Authorization: Bearer ${ADMIN_TOKEN}"

# Re-enable.
curl -X POST https://cyoda.example.com/api/oauth/keys/trusted/${KEY_ID}/reactivate \
  -H "Authorization: Bearer ${ADMIN_TOKEN}"
```

### Delete

```bash
curl -X DELETE https://cyoda.example.com/api/oauth/keys/trusted/${KEY_ID} \
  -H "Authorization: Bearer ${ADMIN_TOKEN}"
```

### Sign and present a token

Your workload signs a JWT with the matching private key, setting `kid` to the registered `keyId`:

```text
Header:  { "alg": "RS256", "typ": "JWT", "kid": "my-signing-key-2026-06" }
Payload: { universal cyoda claim contract — see auth.tokens }
```

Present it like any other cyoda token:

```bash
curl -X GET https://cyoda.example.com/api/clients \
  -H "Authorization: Bearer ${SIGNED_JWT}"
```

cyoda looks up `kid` in the trusted-key registry, validates the RS256 signature against the registered public key, then enforces the universal claim checks (`iss`, `aud`, `exp`, …).

## TOKEN

JWTs you sign with a trusted-key private key must conform to the universal cyoda claim contract documented in `auth.tokens`. In particular:

- `iss` must match `CYODA_JWT_ISSUER` (typically the value you set on the cyoda deployment) — trusted-key tokens are *not* issuer-bound to your IdP because there is no IdP.
- `aud` is checked against `CYODA_JWT_AUDIENCE` if set.
- `caas_org_id` must match the tenant that registered the key.

Cyoda does not mint trusted-key JWTs — you sign them. This page covers the registration + lifecycle of the public key; the claim contract is in `auth.tokens`.

## ERRORS

- `errors.FEATURE_DISABLED` (`404`) — trusted-key endpoints called with `CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED=false`.
- `errors.TRUSTED_KEY_NOT_FOUND` (`404`) — referenced `keyId` not in the registry (also returned for cross-tenant access — the existence of another tenant's key is never confirmed).
- `errors.TRUSTED_KEY_CAP_REACHED` (`400`) — per-tenant cap reached; delete or invalidate an old key first.
- `errors.KEY_OWNED_BY_DIFFERENT_TENANT` (`409`) — registration request specifies a `keyId` that already belongs to another tenant. Pick a fresh `keyId`.
- `errors.UNSUPPORTED_KEY_TYPE` (`400`) — `kty` is not `"RSA"` (the only type cyoda-go v0.8.0 accepts).
- `errors.UNAUTHORIZED` (`401`) — caller lacks a valid bearer for the management call; or, at validation time, the signing key is unknown / invalidated / expired.

## SEE ALSO

- `auth.tokens` — universal JWT claim contract
- `config.auth` — `CYODA_IAM_TRUSTED_KEY_*` env vars
- `openapi` — `cyoda help openapi tags` and look for the `IAM` tag's `/oauth/keys/trusted/*` operations
```

### 5.6 — Add help-test load-resolve cases

- [ ] **Step 6: Append load-resolve cases to `cmd/cyoda/help/help_test.go`**

At the end of the file, add:

```go
// TestDefaultTree_AuthTopics ensures the new auth topic tree loads from
// embedded content and that every topic's body contains the rigid
// 7-section anchors the LLM-targeted template promises (per spec D5).
func TestDefaultTree_AuthTopics(t *testing.T) {
	tree := DefaultTree
	if tree == nil || tree.Root == nil {
		t.Fatal("DefaultTree.Root nil")
	}

	cases := []struct {
		path           []string
		title          string
		requireAnchors []string
	}{
		{[]string{"auth"}, "auth — authenticate client applications against cyoda",
			[]string{"## NAME", "## GOAL", "## WHICH PATH DO I NEED?", "## TOKEN PRESENTATION", "## SEE ALSO"}},
		{[]string{"auth", "clients"}, "auth.clients — M2M client lifecycle",
			[]string{"## NAME", "## GOAL", "## PREREQUISITES", "## REQUEST FLOW", "## TOKEN", "## ERRORS", "## SEE ALSO"}},
		{[]string{"auth", "tokens"}, "auth.tokens — /oauth/token grants and JWT claim contract",
			[]string{"## NAME", "## GOAL", "## PREREQUISITES", "## REQUEST FLOW", "## TOKEN", "## ERRORS", "## SEE ALSO"}},
		{[]string{"auth", "oidc"}, "auth.oidc — federated OIDC providers",
			[]string{"## NAME", "## GOAL", "## PREREQUISITES", "## REQUEST FLOW", "## TOKEN", "## DIAGNOSTICS", "## ERRORS", "## SEE ALSO"}},
		{[]string{"auth", "trusted-keys"}, "auth.trusted-keys — register public keys for offline JWT signing",
			[]string{"## NAME", "## GOAL", "## PREREQUISITES", "## REQUEST FLOW", "## TOKEN", "## ERRORS", "## SEE ALSO"}},
	}

	for _, tc := range cases {
		t.Run(strings.Join(tc.path, "."), func(t *testing.T) {
			topic := tree.Find(tc.path)
			if topic == nil {
				t.Fatalf("Find(%v): topic missing", tc.path)
			}
			if topic.Title != tc.title {
				t.Errorf("title: got %q, want %q", topic.Title, tc.title)
			}
			if topic.Stability != "evolving" {
				t.Errorf("stability: got %q, want %q", topic.Stability, "evolving")
			}
			body := string(topic.Body)
			for _, anchor := range tc.requireAnchors {
				if !strings.Contains(body, anchor) {
					t.Errorf("body missing anchor %q", anchor)
				}
			}
		})
	}
}

// TestDefaultTree_AuthLandingListsAllChildren verifies the auth landing
// page renders descriptors for every subtopic — the renderer auto-populates
// the parent topic's Children slice from the embedded filesystem.
func TestDefaultTree_AuthLandingListsAllChildren(t *testing.T) {
	tree := DefaultTree
	auth := tree.Find([]string{"auth"})
	if auth == nil {
		t.Fatal("Find([auth]): topic missing")
	}
	want := map[string]bool{"clients": true, "tokens": true, "oidc": true, "trusted-keys": true}
	got := map[string]bool{}
	for _, c := range auth.Children {
		if len(c.Path) == 0 {
			t.Fatalf("child with empty path: %#v", c)
		}
		got[c.Path[len(c.Path)-1]] = true
	}
	for k := range want {
		if !got[k] {
			t.Errorf("auth missing child %q (have %v)", k, got)
		}
	}
}
```

- [ ] **Step 7: Run the help-tree tests to confirm they pass**

```bash
go test -short ./cmd/cyoda/help/... -v 2>&1 | tail -30
```

Expected: `TestDefaultTree_AuthTopics` and `TestDefaultTree_AuthLandingListsAllChildren` PASS, every prior test still PASSES.

- [ ] **Step 8: Smoke-render every new page**

```bash
go build -o bin/cyoda ./cmd/cyoda
for t in auth "auth clients" "auth tokens" "auth oidc" "auth trusted-keys"; do
  echo "=== cyoda help $t ===" ; bin/cyoda help $t 2>&1 | head -25 ; echo
done
```

Expected: each topic renders with its title, NAME / GOAL sections, and at least the first few lines of body. No "topic not found" errors. No malformed-frontmatter panics at startup.

### 5.7 — Commit

- [ ] **Step 9: Commit**

```bash
git add cmd/cyoda/help/content/auth.md cmd/cyoda/help/content/auth/ cmd/cyoda/help/help_test.go
git -c commit.gpgsign=false commit -m "feat(help): top-level auth topic with clients/tokens/oidc/trusted-keys subtopics

Per spec §3: a new top-level auth topic tree optimised for LLM
consumers helping developers write client code against cyoda. Each
subtopic uses the rigid 7-section template (NAME / GOAL /
PREREQUISITES / REQUEST FLOW / TOKEN / ERRORS / SEE ALSO) so an
LLM can pattern-match section anchors without parsing prose.

Landing page (auth.md) is a decision tree pointing developers at
clients+tokens (M2M, the 80% case), oidc (federated IdP),
trusted-keys (offline signing), or tokens (OBO via token-exchange).

auth.tokens is the single home for the universal JWT claim
contract; auth.oidc and auth.trusted-keys cross-reference (per D12).

All topics marked stability:evolving, version_added:0.8.0.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Task 6: Pre-PR verification

**Files:** none modified.

### 6.1 — Run all gated test suites

- [ ] **Step 1: Root module unit + E2E tests**

Requires Docker for the PostgreSQL testcontainer:

```bash
go test ./... -v 2>&1 | tail -60
```

Expected: all PASS. If the postgres testcontainer fails to start, restart Docker and retry. Do not proceed if any test fails.

- [ ] **Step 2: Plugin submodules**

```bash
make test-short-all 2>&1 | tail -20
```

Expected: all PASS for `plugins/memory`, `plugins/sqlite`, `plugins/postgres`. If postgres is flaky, `make test-all` (longer, fuller coverage) is the fallback.

- [ ] **Step 3: Static analysis**

```bash
go vet ./...
cd plugins/memory && go vet ./... && cd ../..
cd plugins/sqlite && go vet ./... && cd ../..
cd plugins/postgres && go vet ./... && cd ../..
```

Expected: silent. Any output is a vet warning that must be addressed before PR.

- [ ] **Step 4: Confirm all 5 new help topics resolve at runtime**

```bash
bin/cyoda help auth 2>&1 | grep -E "^TOPICS|clients|tokens|oidc|trusted-keys"
bin/cyoda help auth oidc 2>&1 | head -10
bin/cyoda help auth tokens 2>&1 | grep -E "client_credentials|token-exchange"
```

Expected: the landing TOPICS list shows all four subtopics; oidc renders; tokens covers both grants.

- [ ] **Step 5: Confirm stale openapi.md text is gone**

```bash
bin/cyoda help openapi 2>&1 | grep -c "501 until v0.9.0"
```

Expected: `0`.

- [ ] **Step 6: Confirm docs/cyoda layering doc exists**

```bash
ls docs/cyoda/README.md
head -3 docs/cyoda/README.md
```

Expected: file exists, starts with `# docs/cyoda`.

- [ ] **Step 7: Confirm the cloud-divergences row is updated**

```bash
grep -A2 "Remaining 501" docs/cyoda/cloud-divergences.md | head -10
```

Expected: the new row appears; the old "22 IAM/OAuth/OIDC/account stub endpoints" wording is gone.

### 6.2 — Skip race detector locally

Race detector runs in CI per `.claude/rules/race-testing.md`. No local run required (per spec D13 — no concurrency surface changes in this PR).

---

## Task 7: File umbrella issue + open PR

### 7.1 — File the umbrella issue

- [ ] **Step 1: Check for existing OIDC polish issue on v0.8.0 milestone**

```bash
gh issue list --milestone v0.8.0 --search "OIDC polish OR auth help OR OIDC bugfix"
```

If a matching open issue exists, reference it instead of filing a new one.

- [ ] **Step 2: File the umbrella issue if none exists**

```bash
gh issue create \
  --title "auth help topic + v0.8.0 OIDC pre-release polish" \
  --milestone "v0.8.0" \
  --label "documentation,bug" \
  --body "$(cat <<'EOF'
Aggregates the pre-release polish for the OIDC providers subsystem (#284, #314) and adds the missing top-level auth topic tree in cyoda help.

## Scope

1. **fix(iam):** OIDC response mapper drops `expectedAudiences` and `rolesClaim` despite the OpenAPI spec declaring them. Silent data loss in a public response. Fixed via TDD on the parity-test framework — 4 new scenarios cover every endpoint that returns `OidcProviderResponseDto`.
2. **docs(help):** `cmd/cyoda/help/content/openapi.md` still claimed OIDC remains 501 until v0.9.0. Rewritten as v0.8.0-conformant.
3. **docs(cyoda):** Add `docs/cyoda/README.md` documenting the two coexisting OpenAPI artefacts (split-file mirror + resolved bundle). Document the sync procedure.
4. **docs(cyoda):** Replace the stale "22 IAM stub endpoints" row in `cloud-divergences.md` with a current snapshot and a derivation snippet that re-runs against a live cyoda binary.
5. **feat(help):** New top-level `auth` topic tree — `auth`, `auth/clients`, `auth/tokens` (incl. OBO section), `auth/oidc`, `auth/trusted-keys` — optimised for LLM-implementing-a-client audience per a rigid 7-section template.

## Out of scope (deferred)

- `cyoda` CLI subcommands for OIDC management (REST is sufficient).
- E2E multi-node cluster broadcast tests for OIDC reload.
- Promoting auth/* topics evolving → stable (post-v0.8.0 decision).
- Renaming/restructuring `config/auth.md`.

## Design + plan

- Spec: `docs/superpowers/specs/2026-06-17-auth-help-topic-design.md`
- Plan: `docs/superpowers/plans/2026-06-17-auth-help-topic.md`
EOF
)"
```

Capture the printed issue number as `$ISSUE_NUM`.

### 7.2 — Open the PR

- [ ] **Step 3: Push the branch**

```bash
git push -u origin feat/auth-help-topic
```

- [ ] **Step 4: Open the PR against `release/v0.8.0`**

```bash
gh pr create \
  --base release/v0.8.0 \
  --head feat/auth-help-topic \
  --title "auth help topic + v0.8.0 OIDC polish" \
  --body "$(cat <<EOF
Closes #${ISSUE_NUM}

## What this changes

Five commits, one PR:

1. \`fix(iam):\` OIDC response mapper emits expectedAudiences + rolesClaim (TDD; 4 new parity scenarios)
2. \`docs(help):\` rewrite openapi.md OIDC status as v0.8.0 conformant
3. \`docs(cyoda):\` add README documenting both spec layers
4. \`docs(cyoda):\` rewrite cloud-divergences IAM-stubs row with snapshot + derivation
5. \`feat(help):\` top-level auth topic with clients/tokens/oidc/trusted-keys subtopics

## Why

Pre-release polish for the OIDC providers subsystem that landed in #314 — an audit during the auth-help-topic brainstorm surfaced (a) silent data loss in the response mapper, (b) stale doc text claiming OIDC is unimplemented, (c) a discoverability gap where there was no narrative entry point in \`cyoda help\` for client-application authentication. All addressed here.

## Verification

\`\`\`bash
make test-all           # root module + all plugin submodules
go vet ./...            # root module
bin/cyoda help auth     # smoke-render the new topic tree
\`\`\`

Race detector runs in CI per .claude/rules/race-testing.md — no local run for this PR (no concurrency-surface changes).

🤖 Generated with [Claude Code](https://claude.com/claude-code)
EOF
)"
```

- [ ] **Step 5: Apply the v0.8.0 milestone to the PR (and re-verify on the issue)**

```bash
gh pr edit --milestone "v0.8.0"
gh issue view ${ISSUE_NUM} --json milestone -q '.milestone.title'
```

Expected: PR carries v0.8.0; issue still shows v0.8.0. Per memory `feedback_release_milestone_invariant`, this milestone IS the release-notes source — both PR and issue must be tagged.

---

## Self-review checklist (done before handoff)

- **Spec coverage:** §1 OIDC bugfix → Task 1. §1 openapi.md → Task 2. §1 docs/cyoda README → Task 3. §1 cloud-divergences row → Task 4. §1 auth help topics → Task 5. Pre-PR verification → Task 6. Umbrella issue + PR → Task 7. **All §5 commits accounted for.**
- **Placeholder scan:** No "TBD" / "fill in details" / "similar to Task N" / "appropriate error handling". The cloud-divergences derivation snippet has a `TOKEN_PLACEHOLDER` marker, but it is explicitly described in the surrounding prose as something to substitute at runtime, not a deferred decision.
- **Type consistency:** Test function names match between `e2e/parity/oidc.go` definitions and `e2e/parity/registry.go` registrations (`RunOidcRegisterAudiencesRoundTrip` etc.). `OidcProviderResponse` fields (`ExpectedAudiences`, `RolesClaim`) match `e2e/parity/client/oidc.go`. The `*string` assumption for `OidcProvider.RolesClaim` is flagged for verification in Step 4 of Task 1.
