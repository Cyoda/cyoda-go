# OIDC Object-Shaped `rolesClaim` (Zitadel) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:subagent-driven-development` (recommended) or `superpowers:executing-plans` to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Extend federated-OIDC role extraction so the value at the configured top-level `rolesClaim` may be a JSON **object** (Zitadel's `urn:zitadel:iam:org:project:roles` shape), in addition to the currently supported string-array and string forms. When the value is an object, the role list is its top-level keys.

**Architecture:** Single-function extension in `internal/auth/oidc/usercontext.go` — `extractRoles(claim any) []string` grows a `map[string]any` case that returns the keys (empty-string keys dropped, inner values ignored, native iteration order — no sort). Top-level claim-name lookup stays as-is; operators continue to point `rolesClaim` at whatever literal JWT key carries roles (`urn:zitadel:iam:org:project:roles`, `cognito:groups`, `https://app.example/roles`, etc.). No new config, no new flag, no path-walking syntax. Tests at the unit layer (extraction matrix + Zitadel-shaped full-`UserContext` build) and parity layer (E2E through HTTP using the existing `MintJWTWithRolesClaim` fixture). Help-topic notes the three supported shapes plus a small per-IdP value reference.

**Tech Stack:** Go 1.26+; `log/slog`; `testify` not used in this package — keep the existing minimal `t.Errorf`/`len()` style; existing parity registry (`e2e/parity/registry.go`) wires the new E2E case into every backend (memory/sqlite/postgres + out-of-tree Cassandra on next dep bump).

**Issue:** #317
**Spec:** `docs/proposals/cyoda-go-oidc-zitadel-roles-claim.md`
**Target branch:** `release/v0.8.0` (v0.8.0 milestone)

---

## File Structure

Files touched, with one-line responsibility each:

- **Modify** `internal/auth/oidc/usercontext.go` — extend `extractRoles()` with `map[string]any` case; update doc comment to enumerate the three accepted shapes.
- **Modify** `internal/auth/oidc/usercontext_test.go` — add object-shape cases to `TestExtractRoles_HandlesAllInputForms`; add `TestBuildOIDCUserContext_ZitadelObjectRolesClaim` covering single-role + multi-role Zitadel shapes through the full `buildOIDCUserContext` path; add a dedicated `TestExtractRoles_ObjectShape` to assert membership semantics (since the existing table-driven test only counts elements and that's insufficient for verifying which keys came out).
- **Modify** `e2e/parity/oidc.go` — add `RunOidcD23_RolesParsingObjectKeys_Zitadel(t, fix BackendFixture)` covering the Zitadel object claim end-to-end.
- **Modify** `e2e/parity/registry.go` — register the new function under the existing D23 UserContext block.
- **Modify** `cmd/cyoda/help/content/auth/oidc.md` — expand the TOKEN section's `rolesClaim` bullet (currently L154) into a list of the three supported shapes; add a short "common values" table for Cognito/Zitadel/Auth0/Keycloak/default.

---

## Task 1 — Extend the unit extraction table with object cases (RED)

**Files:**
- Modify: `internal/auth/oidc/usercontext_test.go` (`TestExtractRoles_HandlesAllInputForms`, around L151–L175)

- [ ] **Step 1: Add object-shape rows to the existing table**

Replace the `cases` slice in `TestExtractRoles_HandlesAllInputForms` (currently at L152–L166) with this extended version:

```go
	cases := []struct {
		name string
		in   any
		want int // length of returned slice
	}{
		{"nil", nil, 0},
		{"empty-string", "", 0},
		{"space-delimited-string", "admin user view", 3},
		{"string-with-extra-whitespace", "  admin   user  ", 2},
		{"[]string", []string{"admin", "user"}, 2},
		{"[]string-with-empties", []string{"admin", "", "user"}, 2},
		{"[]any-of-strings", []any{"admin", "user"}, 2},
		{"[]any-mixed-types", []any{"admin", 42, "user", nil}, 2},
		// Object-shaped claim (Zitadel projectRoleAssertion etc.): keys are the roles.
		{"map[string]any-zitadel-single", map[string]any{
			"admin": map[string]any{"312orgId": "sioms.localhost"},
		}, 1},
		{"map[string]any-zitadel-multi", map[string]any{
			"sales_rep": map[string]any{"312orgId": "sioms.localhost"},
			"warehouse": map[string]any{"312orgId": "sioms.localhost"},
		}, 2},
		{"map[string]any-empty", map[string]any{}, 0},
		{"map[string]any-with-empty-key", map[string]any{
			"":      map[string]any{"orgId": "x"},
			"admin": map[string]any{"orgId": "y"},
		}, 1},
		{"map[string]any-mixed-value-types", map[string]any{
			"admin":   42,
			"viewer":  true,
			"editor":  nil,
			"manager": map[string]any{"orgId": "x"},
		}, 4},
		// map[string]string is also a legitimate JSON-object decode shape;
		// keys are still the roles, inner string values are ignored.
		{"map[string]string", map[string]string{"admin": "x", "viewer": "y"}, 2},
		// Truly unsupported scalars stay unsupported.
		{"unsupported-type-int", 42, 0},
		{"unsupported-type-bool", true, 0},
	}
```

- [ ] **Step 2: Run the table test to verify the new rows fail**

Run:
```bash
go test ./internal/auth/oidc/ -run TestExtractRoles_HandlesAllInputForms -v
```

Expected: PASS for the existing rows. **FAIL** for `map[string]any-*` rows and the `map[string]string` row (they currently fall into `default → return nil`, so `len=0` instead of the expected count). The pre-existing `unsupported-type` row was renamed to `unsupported-type-int`/`unsupported-type-bool`; both must still pass since both fall through to `default`.

- [ ] **Step 3: Do not commit yet — proceed to Task 2 to add the membership-asserting test.**

---

## Task 2 — Add membership-asserting unit test for object extraction (RED)

The existing table only counts elements. We need a separate test that asserts **which** keys came out, since the bug we're guarding against (returning `nil` when given an object) and a hypothetical regression (returning the inner orgId values instead of keys) both produce the same element-count for the multi-role case. This test also exercises native map-iteration order: we compare as a set.

**Files:**
- Modify: `internal/auth/oidc/usercontext_test.go` (append after `TestExtractRoles_HandlesAllInputForms`)

- [ ] **Step 1: Append the new test**

```go
// TestExtractRoles_ObjectShape verifies that when a JWT claim is decoded as
// a JSON object (e.g. Zitadel's `urn:zitadel:iam:org:project:roles` with
// projectRoleAssertion=true), the top-level keys are the extracted roles
// regardless of the value type underneath. Element-count alone is not
// sufficient — a regression that returned the inner orgId values would
// also produce the right count for a 2-role input. We compare as a set
// because map iteration order is not stable.
func TestExtractRoles_ObjectShape(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want []string // membership, order-independent
	}{
		{
			name: "zitadel-single-role",
			in: map[string]any{
				"admin": map[string]any{"312orgId": "sioms.localhost"},
			},
			want: []string{"admin"},
		},
		{
			name: "zitadel-multi-role",
			in: map[string]any{
				"sales_rep": map[string]any{"312orgId": "sioms.localhost"},
				"warehouse": map[string]any{"312orgId": "sioms.localhost"},
			},
			want: []string{"sales_rep", "warehouse"},
		},
		{
			name: "empty-key-dropped",
			in: map[string]any{
				"":      map[string]any{"orgId": "x"},
				"admin": map[string]any{"orgId": "y"},
			},
			want: []string{"admin"},
		},
		{
			name: "values-are-ignored-regardless-of-type",
			in: map[string]any{
				"admin":   42,
				"viewer":  true,
				"editor":  nil,
				"manager": map[string]any{"orgId": "x"},
			},
			want: []string{"admin", "viewer", "editor", "manager"},
		},
		{
			name: "map-string-string-also-supported",
			in:   map[string]string{"admin": "x", "viewer": "y"},
			want: []string{"admin", "viewer"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := extractRoles(c.in)
			if !sameStringSet(got, c.want) {
				t.Errorf("extractRoles(%v) = %v, want set %v", c.in, got, c.want)
			}
		})
	}
}

// sameStringSet returns true iff a and b contain the same elements
// (multiset semantics, order-independent). Tiny — extraction inputs
// are O(10) elements at most.
func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	ca := map[string]int{}
	cb := map[string]int{}
	for _, s := range a {
		ca[s]++
	}
	for _, s := range b {
		cb[s]++
	}
	if len(ca) != len(cb) {
		return false
	}
	for k, v := range ca {
		if cb[k] != v {
			return false
		}
	}
	return true
}
```

- [ ] **Step 2: Run the new test, verify it fails**

Run:
```bash
go test ./internal/auth/oidc/ -run TestExtractRoles_ObjectShape -v
```

Expected: **FAIL** for every sub-case — `extractRoles()` currently returns `nil` for `map[string]any` and `map[string]string`, so `sameStringSet([], wanted)` is false.

- [ ] **Step 3: Do not commit yet — Task 3 adds the full-`UserContext` test.**

---

## Task 3 — Add full-`UserContext` Zitadel test (RED)

This drives the `buildOIDCUserContext` integration path: claim name is the literal Zitadel claim string; `rolesClaim` config is a per-provider override; the resulting `UserContext.Roles` must contain the role keys.

**Files:**
- Modify: `internal/auth/oidc/usercontext_test.go` (append after the new `sameStringSet` helper)

- [ ] **Step 1: Append the integration test**

```go
// TestBuildOIDCUserContext_ZitadelObjectRolesClaim verifies that with a
// Zitadel-shaped object claim and a per-provider rolesClaim pointing at
// it, buildOIDCUserContext extracts the role keys onto UserContext.Roles.
func TestBuildOIDCUserContext_ZitadelObjectRolesClaim(t *testing.T) {
	const zitadelClaim = "urn:zitadel:iam:org:project:roles"
	rc := zitadelClaim
	p := provider(&rc)

	t.Run("single-role", func(t *testing.T) {
		uc, err := buildOIDCUserContext(p, map[string]any{
			"sub": "alice",
			zitadelClaim: map[string]any{
				"admin": map[string]any{"312orgId": "sioms.localhost"},
			},
		}, "roles")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if !sameStringSet(uc.Roles, []string{"admin"}) {
			t.Errorf("Roles = %v, want [admin]", uc.Roles)
		}
	})

	t.Run("multi-role", func(t *testing.T) {
		uc, err := buildOIDCUserContext(p, map[string]any{
			"sub": "alice",
			zitadelClaim: map[string]any{
				"sales_rep": map[string]any{"312orgId": "sioms.localhost"},
				"warehouse": map[string]any{"312orgId": "sioms.localhost"},
			},
		}, "roles")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if !sameStringSet(uc.Roles, []string{"sales_rep", "warehouse"}) {
			t.Errorf("Roles = %v, want set [sales_rep warehouse]", uc.Roles)
		}
	})

	t.Run("empty-object-yields-no-roles", func(t *testing.T) {
		uc, err := buildOIDCUserContext(p, map[string]any{
			"sub":        "alice",
			zitadelClaim: map[string]any{},
		}, "roles")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if len(uc.Roles) != 0 {
			t.Errorf("Roles = %v, want empty slice", uc.Roles)
		}
	})

	t.Run("absent-claim-yields-no-roles", func(t *testing.T) {
		uc, err := buildOIDCUserContext(p, map[string]any{
			"sub": "alice",
			// zitadelClaim intentionally absent
		}, "roles")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if len(uc.Roles) != 0 {
			t.Errorf("Roles = %v, want empty slice", uc.Roles)
		}
	})
}
```

- [ ] **Step 2: Run the integration test, verify it fails**

Run:
```bash
go test ./internal/auth/oidc/ -run TestBuildOIDCUserContext_ZitadelObjectRolesClaim -v
```

Expected: **FAIL** on `single-role` and `multi-role` sub-cases (object claim returns `nil` → empty roles). `empty-object-yields-no-roles` and `absent-claim-yields-no-roles` should PASS even pre-fix (current default branch returns `nil` for both).

- [ ] **Step 3: Commit RED (unit tests only — parity test added in Task 5)**

```bash
git add internal/auth/oidc/usercontext_test.go
git commit -m "test(auth/oidc): #317 RED — object-shaped rolesClaim cases

Adds Zitadel-style object-claim coverage to extractRoles() at three levels:
- table-row count assertions extended (TestExtractRoles_HandlesAllInputForms)
- membership-asserting set-equality test (TestExtractRoles_ObjectShape)
- full UserContext build through buildOIDCUserContext, single + multi role
  (TestBuildOIDCUserContext_ZitadelObjectRolesClaim)

Element-count alone is not sufficient — a regression that returned inner
orgId map values would produce matching counts for the multi-role case.
The set helper accommodates Go's non-deterministic map iteration order.

These tests fail until extractRoles() learns to handle map[string]any.

Refs: #317

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Task 4 — GREEN: extend `extractRoles()` to handle `map[string]any` / `map[string]string`

**Files:**
- Modify: `internal/auth/oidc/usercontext.go` (replace `extractRoles` at L71–L102, including the doc comment)

- [ ] **Step 1: Replace the function body**

Replace the entire `extractRoles` function and its doc comment (current L71–L102) with:

```go
// extractRoles accepts the value found at the configured rolesClaim and
// returns the user's roles as a flat slice. Recognised shapes:
//
//   - nil / empty → empty slice (authenticated but unprivileged).
//   - string → space-delimited per RFC 6749 §3.3 / RFC 8693 §4.2; a lone
//     non-empty token like "admin" yields ["admin"].
//   - []string / []any (of strings) → use as-is, dropping empty entries
//     and non-string entries in the []any case.
//   - map[string]any / map[string]string → JSON-object claim; the
//     top-level **keys** are the role values, inner values are ignored.
//     This is the shape Zitadel emits for `urn:zitadel:iam:org:project:roles`
//     with projectRoleAssertion=true (inner values are { orgId: domain }
//     routing metadata, irrelevant for role extraction). Empty-string
//     keys are dropped for parity with array handling.
//
// Any other scalar (number, bool, etc.) → empty slice (no panic). Map
// iteration order is preserved as-is; downstream consumers
// (spi.HasRole) are order-independent and tests compare as sets, so the
// per-request sort cost is not paid here.
func extractRoles(claim any) []string {
	switch v := claim.(type) {
	case nil:
		return nil
	case string:
		if v == "" {
			return nil
		}
		return strings.Fields(v)
	case []string:
		out := make([]string, 0, len(v))
		for _, s := range v {
			if s != "" {
				out = append(out, s)
			}
		}
		return out
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	case map[string]any:
		out := make([]string, 0, len(v))
		for k := range v {
			if k != "" {
				out = append(out, k)
			}
		}
		return out
	case map[string]string:
		out := make([]string, 0, len(v))
		for k := range v {
			if k != "" {
				out = append(out, k)
			}
		}
		return out
	default:
		return nil
	}
}
```

- [ ] **Step 2: Run the new tests, verify they pass**

Run:
```bash
go test ./internal/auth/oidc/ -run 'TestExtractRoles_HandlesAllInputForms|TestExtractRoles_ObjectShape|TestBuildOIDCUserContext_ZitadelObjectRolesClaim' -v
```

Expected: **PASS** for all sub-cases.

- [ ] **Step 3: Run the full oidc package test, verify no regressions**

Run:
```bash
go test ./internal/auth/oidc/ -v
```

Expected: PASS across all `TestBuildOIDCUserContext_*` and `TestExtractRoles_*` tests.

- [ ] **Step 4: Commit GREEN**

```bash
git add internal/auth/oidc/usercontext.go
git commit -m "feat(auth/oidc): #317 accept object-shaped rolesClaim (Zitadel)

Extends extractRoles() with map[string]any and map[string]string cases —
JSON-object claims now contribute their top-level keys as roles, matching
Zitadel's projectRoleAssertion shape:

  \"urn:zitadel:iam:org:project:roles\": {
    \"admin\":     { \"312orgId\": \"sioms.localhost\" },
    \"warehouse\": { \"312orgId\": \"sioms.localhost\" }
  }

→ Roles = [admin, warehouse]. Inner values (Zitadel's orgId→domain routing
metadata) are read and discarded regardless of type. Empty-string keys
are dropped for parity with array handling.

No new config or per-provider flag — the lookup at the configured
top-level rolesClaim is unchanged; only the value shape recognised at
that key is extended. Backward-compatible: string-array, []any, and
space-delimited-string paths are byte-identical to v0.7.x behaviour.

Per-request: native map iteration order; downstream spi.HasRole is
order-independent so no sort cost is paid in the hot path.

Closes #317

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Task 5 — Add parity E2E test for the Zitadel shape

The parity test exercises the change through the full HTTP stack against every backend. It registers a provider with `rolesClaim` pointing at the literal Zitadel claim string, mints a JWT with the object-shaped claim, and verifies a ROLE_ADMIN-gated endpoint accepts it.

**Files:**
- Modify: `e2e/parity/oidc.go` (append after `RunOidcD23_RolesParsingMultiFormat` at L2437)
- Modify: `e2e/parity/registry.go` (insert registration in the D23 UserContext block at L217)

- [ ] **Step 1: Append the parity test in `e2e/parity/oidc.go`**

After the closing `}` of `RunOidcD23_RolesParsingMultiFormat` (around L2437) and before the `// --- D23 sub bounds (rows 57-59) ---` comment, insert:

```go
// RunOidcD23_RolesParsingObjectKeys_Zitadel verifies #317: when the
// rolesClaim value is a JSON object (Zitadel's projectRoleAssertion
// shape), the top-level object keys are the user's roles. The operator
// points rolesClaim at the literal Zitadel claim string
// `urn:zitadel:iam:org:project:roles`; cyoda must accept the object
// value and read the keys.
//
// Verification: mint a JWT whose Zitadel claim object includes
// ROLE_ADMIN as a top-level key (inner value is the Zitadel routing
// payload, ignored); call a ROLE_ADMIN-gated endpoint and expect 2xx.
// If extractRoles() failed to handle the object shape, roles would be
// empty and the ROLE_ADMIN gate would return 403.
//
// Pattern follows RunOidcD23_RolesParsingMultiFormat: own tenant, IdP,
// and provider per test for isolation; JWKS warm-up via ProbeAuthRaw
// before hitting the ROLE_ADMIN-gated endpoint.
func RunOidcD23_RolesParsingObjectKeys_Zitadel(t *testing.T, fix BackendFixture) {
	const zitadelClaim = "urn:zitadel:iam:org:project:roles"

	admin := fix.NewTenant(t)
	adminC := client.NewClient(fix.BaseURL(), admin.Token)

	idp := NewParityFixtureIdP(t)
	p, err := adminC.RegisterOidcProvider(t, map[string]any{
		"wellKnownConfigUri": idp.WellKnownURI(),
		"rolesClaim":         zitadelClaim,
	})
	if err != nil {
		t.Fatalf("RegisterOidcProvider: %v", err)
	}

	// Zitadel-shaped object claim. Inner { orgId: domain } map is
	// routing metadata — cyoda reads the keys and ignores it.
	zitadelRoles := map[string]any{
		"ROLE_ADMIN": map[string]any{"312orgId": "sioms.localhost"},
		"warehouse":  map[string]any{"312orgId": "sioms.localhost"},
	}

	// Warm the JWKS cache via a non-ROLE_ADMIN probe first.
	warmToken := idp.MintJWTWithRolesClaim(t, idp.DefaultKid, admin.ID, map[string]any{
		zitadelClaim: zitadelRoles,
	})
	warmC := client.NewClient(fix.BaseURL(), warmToken)
	if s, b, e := warmC.ProbeAuthRaw(t); e != nil {
		t.Fatalf("warm ProbeAuthRaw transport: %v", e)
	} else if s != http.StatusOK {
		t.Fatalf("warm probe: status %d, want 200 (JWT validation failed for Zitadel object claim) (body: %s)", s, b)
	}

	// Hit a ROLE_ADMIN-gated endpoint. UpdateOidcProvider (PATCH, no-field-change)
	// requires ROLE_ADMIN and does not perturb JWKS state — same probe pattern as
	// RunOidcD23_RolesParsingMultiFormat.
	token := idp.MintJWTWithRolesClaim(t, idp.DefaultKid, admin.ID, map[string]any{
		zitadelClaim: zitadelRoles,
	})
	probeC := client.NewClient(fix.BaseURL(), token)
	if _, err := probeC.UpdateOidcProvider(t, p.ID, map[string]any{}); err != nil {
		t.Errorf("UpdateOidcProvider (ROLE_ADMIN gate via object-keys extraction): %v", err)
	}
}
```

- [ ] **Step 2: Register the new function in `e2e/parity/registry.go`**

Replace the D23 UserContext block (currently L214–L217) with:

```go
	// D23 UserContext (rows 54-56, #317).
	{"OidcD23_CrossIdPSubCollisionDistinctUserIDs", RunOidcD23_CrossIdPSubCollisionDistinctUserIDs},
	{"OidcD23_PerProviderRolesClaim", RunOidcD23_PerProviderRolesClaim},
	{"OidcD23_RolesParsingMultiFormat", RunOidcD23_RolesParsingMultiFormat},
	{"OidcD23_RolesParsingObjectKeys_Zitadel", RunOidcD23_RolesParsingObjectKeys_Zitadel},
```

- [ ] **Step 3: Compile the parity package to catch any signature mismatches**

Run:
```bash
go build ./e2e/parity/...
```

Expected: clean build, no output.

- [ ] **Step 4: Run the parity test against the in-process E2E harness**

The parity tests are exercised by the per-backend wrappers under `internal/e2e/`. The repo's main entrypoint is `go test ./internal/e2e/... -v` which runs the full E2E suite (PostgreSQL testcontainers — needs Docker).

For a targeted run of just the new parity case:
```bash
go test ./internal/e2e/... -run OidcD23_RolesParsingObjectKeys_Zitadel -v
```

Expected: PASS across all backends wired in `internal/e2e/`.

If Docker is unavailable in the sandbox, fall through to the full `go test ./... -v` run at the verification gate (Task 7) and confirm there.

- [ ] **Step 5: Commit**

```bash
git add e2e/parity/oidc.go e2e/parity/registry.go
git commit -m "test(e2e/parity): #317 OIDC Zitadel object-claim parity scenario

Adds RunOidcD23_RolesParsingObjectKeys_Zitadel — mints a JWT with the
Zitadel projectRoleAssertion shape at urn:zitadel:iam:org:project:roles
(an object whose top-level keys are role names; inner values are the
Zitadel orgId→domain routing payload), registers a provider with
rolesClaim pointed at that literal claim string, and verifies that a
ROLE_ADMIN-gated endpoint (UpdateOidcProvider) accepts the token.

Follows the JWKS warm-then-probe pattern from
RunOidcD23_RolesParsingMultiFormat for cross-sub-test isolation.

Wired into the parity registry alongside the existing D23 UserContext
scenarios — all backends pick it up (memory/sqlite/postgres + the
out-of-tree Cassandra plugin on next dependency bump).

Refs: #317

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Task 6 — Update the `auth oidc` help topic

The TOKEN section's `rolesClaim` bullet currently states "must yield a string array of role values." Extend it to enumerate the three supported shapes and include a small table of common per-IdP values.

**Files:**
- Modify: `cmd/cyoda/help/content/auth/oidc.md` (TOKEN section, around L148–L156)

- [ ] **Step 1: Replace the TOKEN bullet list and add the reference table**

In `cmd/cyoda/help/content/auth/oidc.md`, replace the four lines starting at L150 ("JWTs issued by the federated IdP must conform...") through L156 ("Cyoda does not re-mint federated tokens — they are validated and trusted directly.") with:

```markdown
JWTs issued by the federated IdP must conform to the universal cyoda claim contract documented in `auth.tokens`. In particular:

- `iss` must match per the `issuers` / discovery-document rule above.
- `aud` must match `expectedAudiences` if set.
- The configured `rolesClaim` (per-provider override or `CYODA_OIDC_ROLES_CLAIM`) is looked up as a literal **top-level** JWT claim name. The value at that key may be any of:
  1. **JSON array of strings** — `["admin","warehouse"]` → used as-is.
  2. **JSON object** — `{ "admin": {…}, "warehouse": {…} }` → roles are the **top-level keys** (`["admin","warehouse"]`); inner values are ignored. This is the shape Zitadel emits for `urn:zitadel:iam:org:project:roles` with `projectRoleAssertion=true`.
  3. **String** — `"admin warehouse"` → split on whitespace per RFC 6749 §3.3 / RFC 8693 §4.2; a lone token `"admin"` yields one role.

  Empty / absent / non-collection scalar (number, bool) → no roles, no error (the user is authenticated but unprivileged).

  Common per-IdP values for `rolesClaim`:

  | IdP                                 | `rolesClaim` value                                |
  | ----------------------------------- | ------------------------------------------------- |
  | Default (no override)               | `roles`                                           |
  | Amazon Cognito                      | `cognito:groups`                                  |
  | Zitadel (project-wide)              | `urn:zitadel:iam:org:project:roles`               |
  | Zitadel (per-project)               | `urn:zitadel:iam:org:project:<projectId>:roles`   |
  | Auth0 (namespaced custom claim)     | `https://your-app.example.com/roles`              |

  Note: there is no path/dot-walking syntax — whatever string you configure is used as a single literal claim-key lookup against the JWT, so colons, slashes, and dots inside the value are treated as part of the key name (which is exactly what Zitadel, Cognito, and Auth0 need).

Cyoda does not re-mint federated tokens — they are validated and trusted directly.
```

- [ ] **Step 2: Verify the help topic renders by spot-checking via `cyoda help`**

If a `cyoda` binary is already built locally:
```bash
go build -o bin/cyoda ./cmd/cyoda
./bin/cyoda help auth oidc | grep -A2 'rolesClaim'
```

Expected: the new bullet list and table appear in the rendered output.

If the topic uses any pre-render validation (`TestHelpTopicsCompile` or similar), run:
```bash
go test ./cmd/cyoda/help/... -v
```

Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add cmd/cyoda/help/content/auth/oidc.md
git commit -m "docs(help): #317 enumerate rolesClaim shapes + per-IdP values

Replaces the single-line 'must yield a string array' bullet in the TOKEN
section with a numbered list of the three supported claim shapes
(string array · JSON object — keys are roles · string), plus a
reference table mapping common IdPs (Cognito, Zitadel project-wide and
per-project, Auth0 namespaced) to their literal rolesClaim values.

Makes explicit that the lookup is a literal top-level claim-name match
with no dot/path syntax — operators copy the exact JWT key into
rolesClaim, which is what Zitadel's urn:* and Auth0's https://*
namespaced claims require.

Refs: #317

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Task 7 — Verification gate

**Files:** none modified.

- [ ] **Step 1: Full root-module tests (E2E included)**

Run:
```bash
go test ./... -v 2>&1 | tail -40
```

Expected: all green. E2E suite spins up its own PostgreSQL container via testcontainers-go; needs Docker running. If Docker is unavailable, fall back to `go test -short ./... -v` and note the gap explicitly in the PR body.

- [ ] **Step 2: Per-plugin tests (root `./...` does not cross submodule boundaries)**

Run:
```bash
make test-short-all
```

Expected: all green across root + `plugins/memory|sqlite|postgres`.

- [ ] **Step 3: Static analysis**

Run:
```bash
go vet ./...
```

Expected: no output.

- [ ] **Step 4: Race detector — one-shot before PR**

Run:
```bash
go test -race ./... 2>&1 | tail -40
```

Expected: green. Per `.claude/rules/race-testing.md`, this is the end-of-deliverable sanity check.

- [ ] **Step 5: Confirm no `TODO(#317)` or `TODO(...)` left in the diff**

Run:
```bash
git diff release/v0.8.0...HEAD | grep -E '^\+.*TODO' || echo "clean"
```

Expected: `clean`. (Gate 6: resolve, don't defer.)

---

## Task 8 — Open the PR

**Files:** none modified.

- [ ] **Step 1: Push the feature branch**

```bash
git push -u origin feat/oidc-zitadel-object-roles
```

If push is rejected for credential reasons, use the `GH_TOKEN` inline-credential-helper pattern (per project memory `feedback_git_push_credential`).

- [ ] **Step 2: Create the PR against `release/v0.8.0`**

```bash
gh pr create \
  --base release/v0.8.0 \
  --head feat/oidc-zitadel-object-roles \
  --milestone v0.8.0 \
  --title "feat(auth/oidc): #317 support object-shaped rolesClaim (Zitadel)" \
  --body-file /tmp/pr-body-317.md
```

Where `/tmp/pr-body-317.md` is:

```markdown
## What

Extends `extractRoles()` in `internal/auth/oidc/usercontext.go` to accept a JSON-object value at the configured `rolesClaim`. The role list is the object's top-level keys; inner values are ignored. This is the shape Zitadel emits for `urn:zitadel:iam:org:project:roles` with `projectRoleAssertion=true`.

No new config, no new per-provider flag, no claim-path syntax. The existing top-level claim-name lookup is unchanged; only the recognised value shape at that key is extended. Backward-compatible — string-array / `[]any` / space-delimited-string paths are byte-identical to v0.7.x.

## Why

Without this, every Zitadel-federated user resolves to *no roles*, breaking the SIOMS OIDC federation migration. Filed as #317 with the SIOMS team's full Zitadel-shape requirement captured in `docs/proposals/cyoda-go-oidc-zitadel-roles-claim.md`.

## How

- `extractRoles()` grows `map[string]any` and `map[string]string` cases that return the keys (empty-string keys dropped for parity with array handling).
- Native map iteration order; downstream `spi.HasRole` is order-independent so no per-request sort cost.
- Tests at three layers:
  - Unit table extended in `TestExtractRoles_HandlesAllInputForms` (count assertions for the new shapes).
  - Membership-asserting `TestExtractRoles_ObjectShape` (set-equality, since count alone can't catch a regression that returns inner orgId values instead of keys).
  - Integration `TestBuildOIDCUserContext_ZitadelObjectRolesClaim` covering single + multi role Zitadel claims through the full `buildOIDCUserContext` path.
  - Parity `RunOidcD23_RolesParsingObjectKeys_Zitadel` end-to-end through HTTP via the existing `MintJWTWithRolesClaim` fixture, wired into all backends via `e2e/parity/registry.go`.
- Help topic `cmd/cyoda/help/content/auth/oidc.md` TOKEN section now enumerates the three supported claim shapes and includes a reference table mapping common IdPs (Cognito, Zitadel, Auth0) to their literal `rolesClaim` values.

## Spec

`docs/proposals/cyoda-go-oidc-zitadel-roles-claim.md` (added in this branch as the spec record).

## Authorization mapping — unchanged

Per the SIOMS auth design ([docs/design/auth.md](../blob/main/docs/design/auth.md)), a tenant-bound federated user is authorized for tenant-scoped CRUD by virtue of valid federation. Extracted roles are descriptive only — used for `sub`-attribution and made available to handlers, but not a cyoda-side authorization gate for tenant CRUD. This PR does not touch admin guards or authorization semantics.

## Verification

- `go test ./... -v` — all green (root module incl. E2E).
- `make test-short-all` — all green (root + plugin submodules).
- `go vet ./...` — clean.
- `go test -race ./...` — green.

Closes #317

🤖 Generated with [Claude Code](https://claude.com/claude-code)
```

- [ ] **Step 3: Confirm PR is created with the correct base and milestone**

Run:
```bash
gh pr view --json baseRefName,milestone,title
```

Expected: `baseRefName: "release/v0.8.0"`, milestone `v0.8.0`, title matches. Per project memory `feedback_release_milestone_invariant`, the milestone IS the changelog source — un-milestoned issues are invisible to release notes.

---

## Spec Coverage Self-Check

Spec section → task mapping:

| Spec requirement (`docs/proposals/cyoda-go-oidc-zitadel-roles-claim.md`) | Implemented in |
|---|---|
| String array shape unchanged | Task 1 (regression rows stay green) |
| Object shape → top-level keys | Task 1 + Task 2 + Task 4 |
| Single string → `["admin"]` (nice-to-have) | Pre-existing behaviour, Task 1 regression row `"space-delimited-string"` covers; lone-string covered by existing `space-delimited` path (`strings.Fields("admin") == ["admin"]`) |
| Empty array / empty object / claim absent → empty roles | Task 1 (`map[string]any-empty`) + Task 3 (`empty-object-yields-no-roles`, `absent-claim-yields-no-roles`) |
| Non-string/non-array/non-object scalar → empty, no crash | Task 1 (`unsupported-type-int`, `unsupported-type-bool`) |
| Default extraction (no per-provider flag) | Task 4 — extension is in the switch, no new config |
| Acceptance: Zitadel JWT with object claim yields role keys, single + multi | Task 3 + Task 5 |
| Acceptance: string-array extraction unchanged | Task 1 regression rows + existing parity `RunOidcD23_RolesParsingMultiFormat` |
| Acceptance: empty/absent/non-collection → empty roles, no error | Task 3 sub-cases + Task 1 unsupported-type rows |
| Acceptance: Zitadel-federated user can perform tenant-scoped CRUD with `sub` attribution | Pre-existing federation behaviour; the change here only widens role extraction. Verified by parity `RunOidcD23_RolesParsingObjectKeys_Zitadel` which exercises a real federated request through a ROLE_ADMIN-gated endpoint. |
| Acceptance: `cyoda help auth oidc` notes the supported claim shapes | Task 6 |
