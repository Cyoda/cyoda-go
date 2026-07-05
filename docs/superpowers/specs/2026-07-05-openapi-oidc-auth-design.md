# OpenAPI auth / OIDC reconciliation (group 3, §6E) — design

Group 3 of the OpenAPI contract reconciliation effort (issue #369). Reconciles the
**auth / OIDC / clients / token / keys** surface of `api/openapi.yaml` with the server.
Continues the entity slice (#371), stats/audit/search slice (#373), and entity-model &
workflow slice (#376). Governed by ADR 0003 (contract conformance & evolution) and the
typed-but-OPEN schema policy (`docs/analysis/openapi/schema-strictness-research.md`).

Source findings: audit `docs/analysis/openapi/README.md` **§6E**, re-verified against the
tree at `release/v0.8.2` @ `2c512f5` by three fresh-context code audits (2026-07-05). The
audit was directionally right; the verified surface is smaller and a few facts shifted
(recorded per design area below).

**Centerpiece:** the `ErrorResponseDto → ProblemDetail` envelope sweep, which folds in the
deferred **D-1** (`searchEntityAuditEvents`, from the stats/audit/search slice). Only genuine
RFC-6749 OAuth endpoints keep `ErrorResponseDto`.

---

## 1. Scope

Verified facts that bound the slice:

- **37 `ErrorResponseDto` refs, 9 ops** (36 `$ref` + 1 schema def). Clients / keys / trusted
  CRUD reference `ErrorResponseDto` **not at all** — the sweep is smaller than the audit's
  "~38 across the spec, likely clients/keys/trusted" estimate implied.
- Sweep targets: **CONVERT** 7 OIDC ops (28 refs) + `searchEntityAuditEvents` (5 refs, D-1);
  **KEEP** `getTechnicalUserToken` (4 refs — server genuinely emits the flat OAuth shape).
- The wire `ProblemDetail` (`common/errors.go:157-165`) carries the machine code at
  `properties.errorCode` (not a top-level field), plus `retryable`/`ticket` when set. The
  spec `ProblemDetail` schema (line 8264) already models `properties` as an open bag — correct.
- 21 ops are **config-conditional 501**: live 2xx in `CYODA_IAM_MODE=jwt`, `501 NOT_IMPLEMENTED`
  in the default `mock` mode (backing stores wired only in jwt mode). Not "unimplemented" —
  implemented-but-IAM-gated.
- All 11 error codes touched (`OIDC_PROVIDER_DUPLICATE/INACTIVE/NOT_FOUND`,
  `UNSUPPORTED_ALGORITHM`, `UNSUPPORTED_KEY_TYPE`, `KEYPAIR_NOT_FOUND`, `FEATURE_DISABLED`,
  `BAD_REQUEST`, `NOT_IMPLEMENTED`, `FORBIDDEN`, `UNAUTHORIZED`) are **already emitted and
  already have help topics** — **no new error codes**, so no `errors/<CODE>.md` additions and
  no `TestErrCode_Parity` impact.

**One runtime change** in this slice: `registerOidcProvider` duplicate `400 → 409` (design
area C1). Everything else is spec reconciliation (documentation), one param retype, and one
schema consolidation.

---

## 2. Design area A — the envelope sweep (`ErrorResponseDto → ProblemDetail`)

### 2.1 The drift
The 7 OIDC ops and `searchEntityAuditEvents` declare `application/json` `ErrorResponseDto`
(RFC-6749 shape: required `error` + `error_description`, optional `error_uri`). The server
funnels every one of these error paths through `common.WriteError`, emitting
`application/problem+json` with an RFC-9457 `ProblemDetail` body (`common/errors.go:240`;
`oidc_adapter.go` throughout). The spec has never matched the server here.

### 2.2 The fix
Convert every error response (all statuses) on these 8 ops from
`application/json` `ErrorResponseDto` → `application/problem+json` `ProblemDetail`. This is
exactly the reconciliation #373 already performed for `getStateMachineFinishedEvent` — reuse
that precedent verbatim (schema, media type, `properties.errorCode` documentation).

Converted ops (33 refs):
`listOidcProviders`, `registerOidcProvider`, `reloadOidcProviders`, `deleteOidcProvider`,
`updateOidcProvider`, `invalidateOidcProvider`, `reactivateOidcProvider`,
`searchEntityAuditEvents` (D-1).

### 2.3 What stays on `ErrorResponseDto`
`getTechnicalUserToken` — `writeTokenError` (`auth/token.go:272-284`) emits the flat OAuth
shape (`{error, error_description}`, `application/json`) per RFC-6749. This is correct; keep
`ErrorResponseDto`. (Its enum/status gaps are fixed in area B.)

### 2.4 `properties.errorCode` documentation
Each converted status documents that the machine-readable code lives at
`properties.errorCode` inside the open `properties` bag — mirroring the #373 finished-event
treatment. No schema change to `ProblemDetail` for this (the open bag already permits it);
the op-level response descriptions name the emitted code.

---

## 3. Design area B — `getTechnicalUserToken` spec-stale fixes (server is right)

Keeps `ErrorResponseDto`. All additive (non-breaking):

- **`grant_type` request enum** (openapi.yaml:5852): add `client_credentials`. This is the
  **primary M2M path** (`token.go:62-69`) and is currently absent from the enum — the biggest
  single spec bug on this op.
- **`issued_token_type` enum** (`TokenResponseDto`, ~9204): add
  `urn:ietf:params:oauth:token-type:jwt` (emitted on token-exchange, `token.go:240`).
- **`error` enum** (`ErrorResponseDto`, ~8675): add `server_error` (`token.go:81,100,212,232`)
  and `method_not_allowed` (`token.go:42`).
- **Document `405`**: non-POST → `405` `method_not_allowed` (`token.go:41-44`). Add the `405`
  response (`ErrorResponseDto`, `application/json`).
- **`error_uri`**: declared optional, never emitted. Leave as-is — optional output, tolerant-
  reader, harmless. Noted, not changed.

---

## 4. Design area C — OIDC per-op reconciliation

### C1. `registerOidcProvider` duplicate `400 → 409` (RUNTIME CHANGE)
Duplicate provider currently returns `400 OIDC_PROVIDER_DUPLICATE` (`oidc_adapter.go:176-179`);
the spec already documents `409`. `409 Conflict` is textbook for a duplicate resource and
matches the sibling `409 OIDC_PROVIDER_INACTIVE`. **Fix the code** to emit `409` (spec already
right). Producing e2e flips the assertion `400 → 409`. Cloud-parity entry (§9, P1). Both the
validation `400` and the duplicate `409` remain documented (both `ProblemDetail`).

### C2. `updateOidcProvider` — document `409 OIDC_PROVIDER_INACTIVE`
Emitted at `oidc_adapter.go:297-299` when updating an invalidated provider; undocumented. Add
the `409` response (`ProblemDetail`). Spec-incomplete (closed).

### C3. `listOidcProviders` — remove fictional `403`, retype `activeOnly`
- **Remove `403`**: no admin guard exists — `ListOidcProviders` is auth-only by design (D21,
  `oidc_adapter.go:198-205`); the `403` is never emitted. Spec-stale removal.
- **Retype `activeOnly` `string → boolean`** (RUNTIME code touch): today typed `string` and
  compared literally to `"true"` (`oidc_adapter.go:209-212`), so `"1"`/`"TRUE"`/`"yes"`
  silently mean false. Model as `type: boolean`; adjust the adapter to read the parsed
  `*bool`. Wire form `?activeOnly=true` unchanged. Fixes the silent-false footgun.

---

## 5. Design area D — config-conditional `501` (21 ops)

Per the approved disposition: on each of the 21 IAM-gated ops add a `501` response
(`application/problem+json` `ProblemDetail`, `NOT_IMPLEMENTED`) **and** an
`x-cyoda-iam-mode: jwt` annotation, with a one-line description: *"Returned when the IAM
subsystem is not active (`CYODA_IAM_MODE` ≠ `jwt`)."* All additive/non-breaking.

The 21 ops:
- **7 OIDC**: list / register / reload / delete / update / invalidate / reactivate OidcProvider.
- **5 keys**: issue / getCurrent / delete / invalidate / reactivate JwtKeyPair.
- **5 trusted**: register / list / delete / invalidate / reactivate TrustedKey.
- **4 clients (M2M)**: create / delete TechnicalUser, resetTechnicalUserSecret, listTechnicalUsers.

This is distinct from `x-cyoda-status: planned` (not-yet-built). `accountSubscriptionsGet`
keeps its existing `x-cyoda-status: planned` + unconditional `501` (a genuine stub) — untouched.

---

## 6. Design area E — keys / trusted spec-stale (server is right)

These ops do **not** use `ErrorResponseDto`; the sweep does not touch their envelope. Plan
phase verifies each op's current error schema and normalises to `ProblemDetail` where an
inline/other shape is found. Documentation additions:

- **`issueJwtKeyPair`**: keep the full 10-algorithm enum as a roadmap placeholder (Paul's
  call); add field prose "only `RS256` is honoured in this version"; document
  **`400 UNSUPPORTED_ALGORITHM`** (`keys_adapter.go:44`) alongside the malformed-body `400`.
- **`registerTrustedKey`**: keep the RSA/EC/OKP description as a roadmap placeholder; add
  "only RSA is honoured in this version"; document **`400 UNSUPPORTED_KEY_TYPE`**
  (`trusted_adapter.go:132-133`) and the **`404 FEATURE_DISABLED`** gate (trusted-key
  registration is **off by default**, `gateTrustedKeyFeature`).
- **`invalidateJwtKeyPair` / `reactivateJwtKeyPair` / `getCurrentJwtKeyPair`**: document the
  emitted `400` (malformed body / out-of-range grace / bad audience) with the nuance that a
  bad/unknown **id** → **`404 KEYPAIR_NOT_FOUND`**, not `400`.
- **`deleteTrustedKey` / `invalidateTrustedKey` / `listTrustedKeys` / `reactivateTrustedKey`**:
  document `400` (bad id pattern / body / grace) and `404` (unknown id **and** `FEATURE_DISABLED`).

---

## 7. Design area F — Gate-6 `ProblemDetailDto` consolidation

`ProblemDetailDto` (line 8306, 9 refs, all on group-1 async-search ops: `submitAsyncSearchJob`,
`getAsyncSearchResults`, `cancelAsyncSearch`, `getAsyncSearchStatus`, `searchEntities`) is a
structural duplicate of `ProblemDetail` (line 8264, 161 refs) — both RFC-7807 shape;
`ProblemDetailDto` merely carries richer field descriptions/examples.

**Consolidate toward the bare name** `ProblemDetail` (RFC-7807's own schema name; already
canonical by usage 161:9; matches the bare-name trend of the recent slices — `Envelope`,
`EntityMetadata`, `EntityChangeMeta`, `GroupedStatsRequest`, …):
1. Fold `ProblemDetailDto`'s descriptions/examples into `ProblemDetail` (enrich the canonical).
2. Repoint the 9 search refs to `ProblemDetail`.
3. Delete `ProblemDetailDto`.

Bounded, mechanical, and squarely in the spirit of #369 (eliminate duplicate/drifted schemas).
No repo-wide `Dto`-suffix cleanup — that is 84 breaking renames, out of scope.

---

## 8. Per-endpoint error / status-code table (target contract)

Envelope column: **PD** = `application/problem+json` `ProblemDetail`; **ERD** =
`application/json` `ErrorResponseDto` (RFC-6749). ✎ = changed/added by this slice.

| Op | 200 | 400 | 401 | 403 | 404 | 405 | 409 | 500 | 501 (iam) | Envelope |
|---|---|---|---|---|---|---|---|---|---|---|
| `listOidcProviders` | ✓ | — | UNAUTHORIZED | ~~✎ removed~~ | — | — | — | ✓ | ✎ | PD ✎ |
| `registerOidcProvider` | ✓ | BAD_REQUEST | ✓ | FORBIDDEN | — | — | ✎ OIDC_PROVIDER_DUPLICATE | ✓ | ✎ | PD ✎ |
| `reloadOidcProviders` | ✓ | — | ✓ | FORBIDDEN | — | — | — | ✓ | ✎ | PD ✎ |
| `deleteOidcProvider` | ✓ | — | ✓ | FORBIDDEN | OIDC_PROVIDER_NOT_FOUND | — | — | ✓ | ✎ | PD ✎ |
| `updateOidcProvider` | ✓ | BAD_REQUEST | ✓ | FORBIDDEN | OIDC_PROVIDER_NOT_FOUND | — | ✎ OIDC_PROVIDER_INACTIVE | ✓ | ✎ | PD ✎ |
| `invalidateOidcProvider` | ✓ | — | ✓ | FORBIDDEN | OIDC_PROVIDER_NOT_FOUND | — | — | ✓ | ✎ | PD ✎ |
| `reactivateOidcProvider` | ✓ | — | ✓ | FORBIDDEN | OIDC_PROVIDER_NOT_FOUND | — | — | ✓ | ✎ | PD ✎ |
| `getTechnicalUserToken` | ✓ | invalid_request / unsupported_grant_type / invalid_grant | invalid_client | access_denied | — | ✎ method_not_allowed | — | ✎ server_error | — | **ERD** (kept) |
| `searchEntityAuditEvents` | ✓ | BAD_REQUEST | ✓ | ✓ | ✓ | — | — | ✓ | — | PD ✎ (D-1) |
| `issueJwtKeyPair` | ✓ | ✎ UNSUPPORTED_ALGORITHM + malformed | ✓ | ✓ | — | — | — | ✓ | ✎ | PD |
| `getCurrentJwtKeyPair` | ✓ | ✎ bad audience | ✓ | ✓ | ✎ KEYPAIR_NOT_FOUND | — | — | ✓ | ✎ | PD |
| `deleteJwtKeyPair` | ✓ | — | ✓ | ✓ | ✎ KEYPAIR_NOT_FOUND | — | — | ✓ | ✎ | PD |
| `invalidateJwtKeyPair` | ✓ | ✎ body/grace | ✓ | ✓ | ✎ KEYPAIR_NOT_FOUND | — | — | ✓ | ✎ | PD |
| `reactivateJwtKeyPair` | ✓ | ✎ body | ✓ | ✓ | ✎ KEYPAIR_NOT_FOUND | — | — | ✓ | ✎ | PD |
| `registerTrustedKey` | ✓ | ✎ UNSUPPORTED_KEY_TYPE + malformed | ✓ | ✓ | ✎ FEATURE_DISABLED | — | — | ✓ | ✎ | PD |
| `listTrustedKeys` | ✓ | — | ✓ | ✓ | ✎ FEATURE_DISABLED | — | — | ✓ | ✎ | PD |
| `deleteTrustedKey` | ✓ | ✎ bad id | ✓ | ✓ | ✎ unknown id / FEATURE_DISABLED | — | — | ✓ | ✎ | PD |
| `invalidateTrustedKey` | ✓ | ✎ id/body/grace | ✓ | ✓ | ✎ unknown id / FEATURE_DISABLED | — | — | ✓ | ✎ | PD |
| `reactivateTrustedKey` | ✓ | ✎ body | ✓ | ✓ | ✎ unknown id / FEATURE_DISABLED | — | — | ✓ | ✎ | PD |
| `createTechnicalUser` | ✓ | ✓ | ✓ | ✓ | — | — | — | ✓ | ✎ | PD |
| `deleteTechnicalUser` | ✓ | — | ✓ | ✓ | ✓ | — | — | ✓ | ✎ | PD |
| `resetTechnicalUserSecret` | ✓ | — | ✓ | ✓ | ✓ | — | — | ✓ | ✎ | PD |
| `listTechnicalUsers` | ✓ | — | ✓ | ✓ | — | — | — | ✓ | ✎ | PD |

Exact current-state statuses for the keys/trusted/M2M ops are verified per-op in the plan
(they already document a subset); this table is the **target**. `501 (iam)` is additive on
all 21 gated ops.

---

## 9. Coverage matrix (scenario × layer)

Layers: **U** = service/handler unit test; **E** = running-backend e2e (`internal/e2e`, real
Postgres); **P** = cross-backend parity (`e2e/parity`); **G** = gRPC. These are **HTTP-only
surfaces** — no gRPC entry point exists for auth/OIDC/keys/trusted (§10) → **G = n/a** for all.

| Scenario | U | E | P | Notes |
|---|---|---|---|---|
| OIDC error envelope = `problem+json` + `properties.errorCode` (per op) | | ✓ | | assert content-type + body shape on a 4xx |
| `registerOidcProvider` duplicate → **409** (runtime flip) | ✓ | ✓ | | producing test; drives `ErrProviderDuplicate` |
| `updateOidcProvider` inactive → 409 | | ✓ | | producing test; drives `ErrProviderInactive` |
| `listOidcProviders` no 403 for non-admin authed user | | ✓ | | assert 200, not 403 |
| `listOidcProviders` `activeOnly` boolean filter | | ✓ | | `?activeOnly=true` vs omitted; assert filtering |
| `getTechnicalUserToken` `client_credentials` accepted | | ✓ | | happy-path M2M grant |
| `getTechnicalUserToken` non-POST → 405 `method_not_allowed` | | ✓ | | producing test |
| `getTechnicalUserToken` error shape = flat OAuth (ERD), not PD | | ✓ | | assert `{error,error_description}` json |
| `issueJwtKeyPair` non-RS256 → 400 `UNSUPPORTED_ALGORITHM` | | ✓ | | producing test (jwt mode) |
| `registerTrustedKey` non-RSA → 400 `UNSUPPORTED_KEY_TYPE` | | ✓ | | producing test (jwt mode + feature on) |
| `registerTrustedKey` feature off → 404 `FEATURE_DISABLED` | | ✓ | | producing test (default config) |
| keys/trusted bad id → 404, bad body/grace → 400 | | ✓ | | producing tests per op |
| 21 gated ops → 501 in mock IAM mode | ✓ | ✓ | | e2e default mode already `mock`; assert 501 |
| `searchEntityAuditEvents` 4xx envelope = PD (D-1) | | ✓ | | assert content-type on 400/404 |
| Non-producible cells (if any surface) | ✓ | | | service unit test with fakes (mirrors #376) |

**Auth ops are not keys in `EntityErrorCodeMatrix`** — adding documented codes there does NOT
auto-fail the matrix; write producing tests deliberately per plan (mirrors #373/#376). Do not
extend the matrix (per-op-completeness footgun). Concurrency tests, if any, stay isolated
single-backend (never parity).

Parity (P): the envelope/behaviour here is IAM-subsystem-specific (built-in JWT IAM), not a
storage-backend contract, so **no cross-backend parity scenario applies** — the same reason the
auth subsystem is HTTP+IAM-scoped, not storage-scoped. Recorded explicitly so the empty P
column is intentional, not an omission.

---

## 10. gRPC coverage note

The auth / OIDC / clients / keys / trusted / token surface has **no gRPC entry point** — these
are HTTP admin/IAM endpoints only (the gRPC contract is entity/search/workflow data-plane). No
gRPC envelope tests apply. Recorded so the G column's n/a is a deliberate, verified carve-out.

---

## 11. oasdiff dispositions

`.github/oasdiff-err-ignore.txt` surgical fail-closed entries (ADR 0003 Decision 7), one per
op×status×property, following the #373 finished-event template exactly:

- **Envelope sweep** — 7 OIDC ops + `searchEntityAuditEvents`, each converted status loses
  required `error` and `error_description` → `response-required-property-removed` per
  op×status×property. (8 ops × their 4xx/5xx statuses × 2 props.) Media-type change
  (`application/json` → `application/problem+json`) may also register as
  `response-media-type-removed` per status — include those entries too if the pinned oasdiff
  emits them.
- **`listOidcProviders` `activeOnly`** `string → boolean` → request param type change
  (`request-parameter-type-changed` or equivalent) → one entry.
- **Additive (no ignore needed):** all `501` responses, the token-op `405`, `updateOidcProvider`
  `409` doc, the token enum additions (request enum widening is more-permissive; response enum
  additions are non-breaking warnings), and the newly-documented keys/trusted 400/404 responses.
- **`registerOidcProvider` 400→409:** the spec already documents `409`; the code catches up →
  **no spec diff, no ignore**.
- **Part F consolidation:** `ProblemDetailDto → ProblemDetail` on 5 search ops — structurally
  identical (no required-property loss); expected non-breaking or warning. If the enrichment
  adds a required field it would break — it will not (neither has `required`). Verify the diff
  is clean; add entries only if the pinned oasdiff flags a property description/example delta as
  ERR (it should not).

Each entry carries a comment block explaining the spec-to-reality rationale. Entries are
prunable once merged (the corrected shape becomes the baseline).

---

## 12. Cloud-parity notes (`docs/cloud-parity/openapi-conformance.md`)

New section "Auth / OIDC reconciliations (2026-07)":

- **A1 — `registerOidcProvider` duplicate → 409 (runtime change).** Duplicate provider now
  returns `409 OIDC_PROVIDER_DUPLICATE` (was `400`). Direction: server-gap (closed). Cloud MUST
  return `409` on duplicate provider registration.
- **A2 — error envelope = RFC-9457 `ProblemDetail` on OIDC/admin ops.** The 7 OIDC ops +
  `searchEntityAuditEvents` emit `application/problem+json` with `errorCode` under `properties`.
  Direction: spec-stale (closed). Cloud MUST emit `ProblemDetail`, not the OAuth `ErrorResponseDto`,
  on these ops. OAuth token endpoint keeps the RFC-6749 shape.
- **A3 — documented-but-IAM-gated ops.** 21 ops return `501` unless `CYODA_IAM_MODE=jwt`.
  Direction: spec-incomplete (closed). Cloud's IAM-mode contract must match.
- **A4 — roadmap-placeholder crypto enums.** `issueJwtKeyPair` (10-algo enum, RS256 honoured)
  and `registerTrustedKey` (RSA/EC/OKP prose, RSA honoured) intentionally retain the wider
  advertised set with "honoured in this version" prose. Direction: needs-decision → RESOLVED
  keep-placeholder. Cloud may honour a wider set; cyoda-go rejects non-RS256/RSA with the
  documented `400`.

No new Cloud-fact-blocked open questions from this slice.

---

## 13. Documentation hygiene (Gate 4)

- **Help topics:** all 11 error codes already have `cmd/cyoda/help/content/errors/<CODE>.md`
  topics (verified) — no additions, no `TestErrCode_Parity` change.
- **`cmd/cyoda/help/content/`:** check for an auth/OIDC topic that describes provider/keys/token
  behaviour; update if the 409 semantics, IAM-gating, or the RS256/RSA "this version" limits are
  documented there. Add/adjust only what drifts (compact — actionable core only).
- **No env-var changes** → no `config/*.md` / `README.md` / `DefaultConfig()` triad update.
- **`COMPATIBILITY.md`:** no SPI pin / chart / binary-release change → untouched.
- **CHANGELOG:** add the group-3 reconciliation entry at PR time.

---

## 14. Dead-code / zombie sweep checklist

- `ProblemDetailDto` schema **deleted** after repointing (area F) — verify zero remaining refs.
- `listOidcProviders` `403` response **removed** from spec — verify no doc/test asserts it.
- `activeOnly` string-compare `== "true"` **removed** from the adapter after the boolean retype
  — verify the `*bool` path is the only reader (no orphaned string parse).
- Regenerate `api/generated.go` (the `codegen-sync` CI gate requires it in-sync); the
  `activeOnly` retype and any schema edits change the generated types.

---

## 15. Verification gates (Gate 5)

- `go test ./internal/e2e/... -v` green (includes the conformance suite; the controller runs
  e2e at consolidation points — subagent Docker is inconsistent).
- `go test -short ./... -v` + plugin submodules (`make test-short-all`) green.
- `go vet ./...` clean (a signature change — the `activeOnly` adapter edit — needs vet to catch
  all call sites; `go build` does not compile test files).
- `make race` (CI-parity scope) green as the one-shot pre-PR check.
- `make check-codegen` (generated.go in sync), `make check-gofmt`, and the oasdiff gate all pass
  (oasdiff passes only via the documented err-ignore entries in §11).
- `docs/cloud-parity/openapi-conformance.md` updated (§12); `#369` umbrella stays OPEN.

---

## 16. Execution notes (carried from groups 1–2)

- Controller runs Docker-backed e2e at consolidation points; subagents go-vet-compile only.
- gRPC n/a here (§10) — no envelope-code assertions.
- Producing tests written deliberately (auth ops absent from `EntityErrorCodeMatrix`); waived/
  non-producible cells → service unit tests with fakes.
- Keep the PR scoped: a gofmt sweep can drag drive-by churn into non-slice files (incl. plugin
  submodules) — gofmt only what this slice touches.
