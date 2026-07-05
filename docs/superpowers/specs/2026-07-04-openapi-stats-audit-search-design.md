# OpenAPI stats / audit / search reconciliation — design

**Date:** 2026-07-04
**Umbrella:** #369 (OpenAPI contract reconciliation)
**Base:** `release/v0.8.2` @ `05e87d1` (entity slice merged)
**Governing policy:** typed-but-open schemas (never `additionalProperties:false`); cyoda-go
**defines** the contract (Cloud follows); cyoda-go is **primarily multi-node**; Gate 6 (resolve
divergence, don't defer); no zombies (every removal cleans both sides).

This is one slice/PR reconciling the `Stats / Audit / Search` area of `api/openapi.yaml`
(audit source: `docs/analysis/openapi/README.md` §6B) with the running server. It reuses the
entity-slice machinery: the error-code matrix, typed-but-open schemas, and the exercised-or-marked
coverage gate (Spec 1).

## 1. Scope

**In scope** — three sub-areas, one PR:

- **Unknown-model unification** (search + stats + list): one canonical `404 MODEL_NOT_FOUND` rule.
- **Fictional / misplaced surface removal** (search + audit).
- **Mechanical spec-stale fixes** (search).
- **Enum retention with documented rationale** (search + audit).
- **Deferred documented gap** (audit `changes` diff → cloud-parity).

**Out of scope — explicitly named, not missed:**

- **`timeoutMillis` implementation** on `searchEntities` — split to bug **#372**. Only item adding
  runtime behaviour + a new error path; the fictional param/`408` are *removed* here and re-added
  with the implementation in #372.
- **Message-op fictional v1-UUID `400` prose** (`deleteMessages`/`getMessage`/`deleteMessage`) —
  handled in the message slice (follow-on group 4). Named here so fixing only the finished-event
  copies reads as intentional.

## 2. Design area 1 — Unknown-model unification

### 2.1 Canonical rule

**Any model-scoped data operation on an unregistered model returns `404 MODEL_NOT_FOUND`.**
No silent-empty, no ad-hoc `400 UNKNOWN_MODEL`. One predictable behaviour across the surface.

This is coherent and leaks nothing: `create`/`update`/`patch`/`delete` already 404 on an
unregistered model (`internal/domain/entity/service.go:212,780,889,1062`), so no unregistered model
can hold entities via the API — `404` cannot hide present data. `modelStore.Get` is tenant-scoped, so
another tenant's model returns `ErrNotFound` → `404`, identical to truly-absent (no cross-tenant
existence leak).

### 2.2 Shared helper

```go
// ensureModelRegistered returns a 404 MODEL_NOT_FOUND AppError when ref names a
// model that is not registered for the caller's tenant. It performs at most one
// bounded RefreshAndGet (singleflight-collapsed, negative-cached) so a model just
// registered on a peer node is not falsely rejected — mirroring the write path's
// ValidateWithRefresh and the search path-validator. Degrades gracefully when the
// store has no RefreshAndGet (single-node / memory).
func ensureModelRegistered(ctx context.Context, ms spi.ModelStore, ref spi.ModelRef) *common.AppError
```

- **Home:** a package importable by `app`, `internal/domain/entity`, and `internal/domain/search`
  (the grouped-stats resolver lives in `app/app.go`). Candidate: `internal/domain/model` or a small
  `internal/common` helper. Decided at implementation; must not create an import cycle.
- **Multi-node:** confirmed the `RefreshAndGet` machinery exists and is safe
  (`internal/cluster/modelcache/cache.go:129` singleflight; used by `search/service.go:558`,
  `search/path_validate.go:151`, `entity/handler.go:233-253`). No SPI/storage change — `ModelStore`
  is a separate interface from `EntityStore` and its `Get` already returns `spi.ErrNotFound` on every
  backend (`persistence.go:67`; memory/sqlite/postgres `model_store.go`).

### 2.3 Application points (6)

| Endpoint | Op | Where the guard goes | Today → After |
|---|---|---|---|
| `GET /entity/{entityName}/{modelVersion}` | getAllEntities | `entity.Handler.ListEntities` (`service.go:957`) | 200 `[]` → 404 |
| `GET /entity/stats/{entityName}/{modelVersion}` | getEntityStatisticsForModel | `entity.Handler.GetStatisticsForModel` (`service.go:594`) | 200 `{count:0}` → 404 |
| `GET /entity/stats/states/{entityName}/{modelVersion}` | getEntityStatisticsByStateForModel | `entity.Handler.GetStatisticsByStateForModel` (`service.go:556`) | 200 `[]` → 404 |
| `POST /search/direct/{entityName}/{modelVersion}` | searchEntities | `search.SearchService.Search` (`service.go:122`) | 200 empty stream → 404 |
| `POST /search/async/{entityName}/{modelVersion}` | submitAsyncSearchJob | `search.SearchService.SubmitAsync` (`service.go:214`) | jobId over empty → 404 |
| `POST /entity/stats/{entityName}/{modelVersion}/query` | queryGroupedEntityStatisticsForModel | `app.go` resolver closure (`app.go:614-637`) | **400 UNKNOWN_MODEL** → 404 MODEL_NOT_FOUND |

HTTP **and** gRPC inherit the fix for the first five (both entry points call the same service method:
`internal/grpc/search.go:161,346,397,471`). **grouped-stats is HTTP-only** and its model check is a
plain `modelStore.Get` in the `app.go` resolver closure with **no** bounded refresh — so it *already*
carries the multi-node racy-false-negative bug. The fix upgrades that closure to
`ensureModelRegistered`, closing the latent bug (Gate-6 bonus).

The all-models stats ops (`getEntityStatistics`, `getEntityStatisticsByState`) enumerate the registry
and cannot encounter an unknown model — **unchanged**. Entity-ID-keyed ops (`getOneEntity`,
update/patch/delete-single, transitions, audit-by-id) are **unchanged** (unknown *entity* → 404).

### 2.4 grouped-stats: retire `UNKNOWN_MODEL`

`UNKNOWN_MODEL` is a bare unregistered string (not in `error_codes.go`, no `errors/UNKNOWN_MODEL.md`).
Retire it entirely in favour of the registered `MODEL_NOT_FOUND`:

- Emission: `grouped_stats_handler.go:137` → `ensureModelRegistered` / `Operational(404, ModelNotFound)`.
- Comments: `app.go:614`, `grouped_stats_handler.go:24`.
- Help doc: `cmd/cyoda/help/content/crud.md:536` and `:621`.
- Spec: `api/openapi.yaml:1130` (drop from the `400` code-list) + add a `404` response block to the
  grouped-stats operation (it has none today — **additive**, oasdiff-safe).
- Test: flip `grouped_stats_handler_test.go:108` (`UNKNOWN_MODEL` → `MODEL_NOT_FOUND`, `400` → `404`).

`MODEL_NOT_FOUND` code + `errors/MODEL_NOT_FOUND.md` already exist — `TestErrCode_Parity` unaffected.

### 2.5 Gate-6 zombie cleanup created by the 404 change

Once `Search`/`SubmitAsync` call `ensureModelRegistered` up front, the search path-validator's
"admit unregistered model" branch is unreachable for the unregistered case:

- Remove/reconcile the `ErrNotFound → return nil` admit-branch (`search/service.go:529-532`).
- Remove/reconcile the stale comment (`search/service.go:496-500`) claiming search must preserve
  "memory admits entity writes without a prior model" — no longer true at the search boundary.

## 3. Design area 2 — Fictional / misplaced surface removal

- **`getAsyncSearchResults` (`GET /search/async/{jobId}`): remove the `pointInTime` param.** PIT is
  captured and persisted at submit (`SubmitAsync` defaults nil→now and stores `job.PointInTime`,
  `service.go:235-273`; `GetAsyncResults` reads `job.PointInTime`, `service.go:399`). The paging
  handler never reads `params.PointInTime` (`handler.go:274-352`). Nothing reads it — safe removal.
- **`getStateMachineFinishedEvent` (`GET /audit/entity/{entityId}/workflow/{transactionId}/finished`):
  remove the fictional "must be a valid time-based UUID (version 1)" prose** (openapi.yaml:502-503,
  508-509, 516-517) **and the `400 "UUID is not time-based"` response** (openapi.yaml:529-534). The
  handler does no version check (`audit/handler.go:231-270`); it accepts any valid UUID and `404`s on
  no-match. Implementing v1-validation instead would make this op an incoherent outlier vs every other
  ID-keyed op (none validate UUID version). **Also** swap this op's error refs `ErrorResponseDto` →
  `ProblemDetail` (the platform emits `problem+json`; the sibling `/clients` op already uses it).
- **`searchEntities`: remove the fictional `timeoutMillis` param + `408` response + "configurable
  timeout" / "Default timeout: 60000 ms" prose** (openapi.yaml:6656, 6686, 6718-6724, 6783-6790).
  Advertised but unimplemented; tracked for reimplementation in **#372**. The `404` (unknown model)
  **stays** — it becomes real via §2.

## 4. Design area 3 — Mechanical spec-stale fixes (server already correct)

- **`searchEntities` `200`:** drop `application/json`, keep only `application/x-ndjson` (handler always
  streams ndjson, `handler.go:178`).
- **`searchEntities` `limit`:** drop "silently limited to 10000" prose; **document the `400`** on
  `limit>10000` — the server deliberately rejects rather than clamps (`handler.go:150-153`,
  rationale comment in code). Drop the ndjson variant from error bodies (errors are `problem+json`).
- **`getAsyncSearchResults`:** page-size default prose `10` → `1000` (real default, `handler.go:277`).

## 5. Design area 4 — Enum retention with documented rationale

**Do NOT remove `NOT_FOUND` (searchJobStatus / `CancelAsyncSearchDto.currentSearchJobStatus`) or
`System` (audit `eventType`).** OSS never *sets* them, but:

- `NOT_FOUND` is emitted by the **commercial self-executing search store** on snapshot-expiry races —
  the spec's own prose says so (openapi.yaml:6595-6612). Removing it would break the Cloud contract.
- `System` is not provably dead contract-wide (only OSS-dead).
- Typed-but-open policy keeps evolving value-sets' documented values, including commercial-only ones.

Instead, **document the rationale in code** so this is never re-litigated (Paul's explicit ask):

- `search/service.go:57` (`SearchJobStatus` type doc): note `NOT_FOUND` is emitted by the commercial
  self-executing store on snapshot-expiry races and is intentionally retained — **do not remove**.
- `audit/handler.go:30` (the `System` filter comment): note `System` is a reserved/commercial audit
  source retained in the contract — **do not remove**.

## 6. Design area 5 — Deferred documented gap

**`searchEntityAuditEvents.changes` before/after diff never emitted** (`audit/handler.go:70-97` builds
only metadata). Emitting it is real feature work (per-version diff materialization). Deferred: a
`docs/cloud-parity/openapi-conformance.md` note (mirrors the E6 `fieldsChangedCount` deferral). Not
implemented this slice. The `changes` field stays in the schema (typed-but-open; optional).

## 7. Per-endpoint error / status-code table

Only rows this slice adds or changes. "✚" = added this slice; "→" = changed.

| Endpoint | Status | Code | Notes |
|---|---|---|---|
| `GET /entity/{entityName}/{modelVersion}` | ✚ 404 | MODEL_NOT_FOUND | unknown model |
| `GET /entity/stats/{entityName}/{modelVersion}` | ✚ 404 | MODEL_NOT_FOUND | unknown model |
| `GET /entity/stats/states/{entityName}/{modelVersion}` | ✚ 404 | MODEL_NOT_FOUND | unknown model |
| `POST /search/direct/{entityName}/{modelVersion}` | 404 | MODEL_NOT_FOUND | now real (was fictional-empty); already in spec |
| `POST /search/direct/{entityName}/{modelVersion}` | 400 | BAD_REQUEST | `limit>10000` (newly documented; already emitted) |
| `POST /search/direct/{entityName}/{modelVersion}` | ✖ 408 | — | removed (fictional → #372) |
| `POST /search/async/{entityName}/{modelVersion}` | ✚ 404 | MODEL_NOT_FOUND | unknown model |
| `POST /entity/stats/{entityName}/{modelVersion}/query` | → 404 | MODEL_NOT_FOUND | was `400 UNKNOWN_MODEL` |
| `GET /audit/entity/{entityId}/workflow/{transactionId}/finished` | ✖ 400 | — | removed fictional v1-UUID 400; envelope → ProblemDetail |
| `GET /search/async/{jobId}` (results) | — | — | `pointInTime` param removed (no status change) |

Unchanged-but-relevant: `getAsyncSearchStatus`/`cancelAsyncSearch` (job-ID keyed, `404 SEARCH_JOB_NOT_FOUND`);
audit-by-id ops (`404 ENTITY_NOT_FOUND`).

## 8. Coverage matrix (scenario × layer)

Layers: **U** unit · **E** running-backend e2e (real Postgres) · **P** cross-backend parity
(memory/sqlite/postgres/commercial) · **G** gRPC envelope (`Error.Code`).

| Scenario | U | E | P | G |
|---|---|---|---|---|
| getAllEntities unknown model → 404 | ✓ | ✓ | (shared P below) | ✓ (grpc ListEntities) |
| getEntityStatisticsForModel unknown model → 404 | ✓ | ✓ | (shared P) | ✓ |
| getEntityStatisticsByStateForModel unknown model → 404 | ✓ | ✓ | (shared P) | ✓ |
| searchEntities unknown model → 404 | ✓ | ✓ | (shared P) | ✓ (grpc direct search) |
| submitAsyncSearchJob unknown model → 404 | ✓ | ✓ | (shared P) | ✓ (grpc async submit) |
| queryGroupedEntityStatisticsForModel unknown model → 404 | ✓ | ✓ | (shared P) | N/A — HTTP-only (**waived**) |
| **Unknown-model rule → 404** (one representative op per layer) | — | — | ✓ **one parity scenario** | — |
| **Multi-node**: model registered on peer, stale node → refresh → op succeeds (no false 404) | — | — | — | isolated **cluster** test (not parity) |
| searchEntities `limit>10000` → 400 | ✓ | ✓ | — | ⚠ verify |
| searchEntities 200 is `x-ndjson` (content-type) | — | ✓ | — | — |
| getStateMachineFinishedEvent accepts non-v1 UUID → 200/404 (no 400) | ✓ | ✓ | — | — |
| grouped-stats error-flip regression (`MODEL_NOT_FOUND`, not `UNKNOWN_MODEL`) | ✓ | ✓ | — | — |

Notes:
- One **shared parity scenario** covers the backend-agnostic unknown-model→404 rule; per-op parity
  rows are not duplicated (the rule is uniform).
- The **multi-node bounded-refresh** cell is mandatory (primarily-multi-node rule) and lives as an
  isolated cluster test — never folded into the shared parity suite.
- No concurrency/race scenario in this slice; if one arises it goes isolated, not in parity.
- ⚠ The `limit>10000` reject lives in the HTTP handler (`handler.go:150-153`), not the service —
  planning must verify whether gRPC direct search enforces the same cap. If it does not, that is an
  HTTP/gRPC divergence to reconcile (make the cap a service-layer concern) per the coherence mandate;
  if out of proportion, record explicitly rather than silently leave asymmetric.

## 9. oasdiff dispositions

Enum removals are dropped (§5), eliminating the request-enum-narrowing break. Remaining edits:

| Edit | oasdiff verdict | Action |
|---|---|---|
| Add `404` to 3 stats/list ops + grouped-stats | additive | none |
| searchEntities `404` already present; now exercised | none | none |
| Remove searchEntities `200` `application/json` variant | response content narrowing (non-client-breaking) | verify; ignore-entry if flagged |
| Remove searchEntities `408` + `timeoutMillis` param | response/param removal | verify; ignore-entry if flagged (documented; #372 re-adds) |
| Remove `pointInTime` param from getAsyncSearchResults | optional param removal | verify; ignore-entry if flagged |
| Remove finished-event `400` | response removal (non-client-breaking) | none/verify |

Any required ignore lives in `.github/oasdiff-err-ignore.txt` with a one-line rationale (the E4
pattern) — fail-closed, cannot mask other breaks.

## 10. Cloud-parity notes (`docs/cloud-parity/openapi-conformance.md`)

- Unknown-model → `404 MODEL_NOT_FOUND` is the canonical contract; Cloud mirrors it (was divergent:
  silent-empty on some ops, `400 UNKNOWN_MODEL` on grouped-stats).
- `searchEntityAuditEvents.changes` before/after diff = documented gap (deferred feature).
- `NOT_FOUND` search-job status is retained specifically because the commercial store emits it.

## 11. Dead-code / zombie sweep checklist

Every removal cleans both sides:

- [ ] `UNKNOWN_MODEL` string fully gone (emission, comments, crud.md ×2, openapi.yaml enum).
- [ ] searchEntities "configurable timeout" / "Default timeout" prose (openapi.yaml:6656,6686) gone
      with the `timeoutMillis`/`408` removal.
- [ ] finished-event v1-UUID prose (×3) + `400` gone; envelope refs → `ProblemDetail`.
- [ ] search "admit unregistered model" branch (`service.go:529-532`) + comment (`:496-500`) removed.
- [ ] `pointInTime` param gone from getAsyncSearchResults; no dangling `ResultOptions` field.
- [ ] `NOT_FOUND`/`System` **retained** with explicit "do-not-remove" code comments (§5).
- [ ] `go vet ./...` clean (catches call-site breaks a plain `go build` misses).

## 12. Verification gates

`go test ./... -v` (incl. e2e) green; `go vet ./...`; per-plugin submodule tests; `make race` once
before PR. Full `internal/e2e` suite run to validate the error-code matrix per-op completeness
(CONFLICT and other non-deterministic codes exempted from the `declared` check, per entity-slice
learnings).
