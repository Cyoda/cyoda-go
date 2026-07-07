# OpenAPI conformance — Cloud hand-off

Tracked in issue #369.

## Roles

cyoda-go **leads** `api/openapi.yaml`. Cyoda Cloud **conforms** to it — not the
reverse. Cloud aligns its contract to cyoda-go's; when the two disagree it is
Cloud that must reconcile, unless cyoda-go's spec is stale (tracked per-finding
in `../cyoda/cloud-divergences.md`).

## Live common ground

Operations **without** an `x-cyoda-status` extension are live — exercised end-to-end
in `internal/e2e/` and validated against the declared schema. This is the
shared contract surface Cloud MUST implement and conform to.

Operations carrying `x-cyoda-status: planned` or `x-cyoda-status: unimplemented`
are shared intent — both sides intend to implement them — but Cloud MAY or MAY
NOT have them yet. They are not part of the current conformance obligation.

## Contract access points

Cloud consumes the spec from either:

- `GET /openapi.json` (served at runtime by the cyoda-go binary)
- `cyoda help openapi json` / `cyoda help openapi yaml` (help subsystem artefact)

Both surfaces serve identical content from the embedded `api/openapi.yaml`.

## Tolerant-reader obligation

Response schemas in `api/openapi.yaml` are typed-but-open (ADR 0003, Decision 3):
properties are enumerated but objects are not sealed (`additionalProperties`
absent or `true`, never `false`). Cloud, as a consumer of cyoda-go responses,
MUST ignore unrecognised fields — new fields are additive, non-breaking changes.

The same obligation applies in the reverse direction: cyoda-go, as a consumer of
Cloud-emitted data in shared scenarios, also ignores unrecognised fields. Neither
side seals the envelope.

## Breaking-change discipline (cyoda-go side)

A CI gate (`openapi-breaking-change.yml`, using `oasdiff`) blocks merges that
introduce breaking spec changes — removing an operation or field, sealing an
open object, narrowing a type, or adding a required field to an existing
response. Cloud may rely on this gate as a stability guarantee: the live common
ground only grows; it does not shrink.

## Complement: field-level divergence catalog

`../cyoda/cloud-divergences.md` is the **inverse** catalog: fields declared in
`api/openapi.yaml` that cyoda-go does not yet implement server-side. This file
covers operation-level status and conformance obligations; the two are
complementary, not overlapping.

## Entity-slice reconciliations (2026-07)

Per-finding contract decisions from the entity reconciliation slice (design
`docs/superpowers/specs/2026-07-02-openapi-contract-reconciliation-design.md`,
ADR 0003). Each states the direction and what Cloud must mirror.

- **E1 — `Envelope.meta` typed-but-open.** `meta` now mirrors the canonical
  `EntityMetadata.json` (required `id/state/creationDate/lastUpdateTime`; optional
  `modelKey/pointInTime/transitionForLatestSave/transactionId`); the
  `previousTransition` fossil is removed. Direction: spec-incomplete + spec-stale.
  Cloud MUST emit the same typed-but-open meta.
- **E1b — `modelKey` on all reads (deviation A2 abandoned).** Previously by-model reads
  (`getAllEntities`, search — HTTP and gRPC) omitted `modelKey` while by-id `getOneEntity`
  included it (the "A2" optimization). That deviation is abandoned: `modelKey` is now emitted on
  every entity read (single-get, list, search) across HTTP and gRPC — one uniform meta shape.
  Direction: needs-decision → decided (byte-saving rationale negligible; uniform meta is simpler
  and fully canonical). Cloud MUST emit `modelKey` on all entity reads. (The `EntityMetadata`
  schema already marks `modelKey` optional, so this is additive/non-breaking for consumers.)
- **E2 — conditional `deleteEntities` (HTTP).** `DELETE /entity/{name}/{version}`
  with an `AbstractConditionDto` body deletes only matching entities (empty body ⇒
  all); `verbose=true` returns deleted ids; `numberOfEntitites` (matched) and
  `numberOfEntititesRemoved` (removed) are distinct. Direction: server-gap (closed a
  data-loss defect). Cloud MUST honour the condition — never ignore it.
- **E3 — `getAllEntities` as-at.** The model-scoped list read honours `pointInTime`
  (via the list-PIT primitive) and stamps `meta.pointInTime`. Direction: server-gap.
- **E8 — `changeType` spelling = `CREATE/UPDATE/DELETE`.** Every entity-change surface now
  agrees on the present-tense spelling (was `CREATED/UPDATED/DELETED` on HTTP+OpenAPI): the
  `EntityChangeMeta` enum + `getEntityChangesMetadata`, gRPC, the canonical schema, AND the
  audit endpoint `GET /audit/entity/{entityId}` (`EntityChangeAuditEventDto`). Direction:
  needs-decision → decided (canonical/gRPC already used it; changing the Cloud-consumed
  canonical field was the higher-risk alternative). Cloud MUST emit `CREATE/UPDATE/DELETE`
  on all these surfaces.

## Stats/audit/search reconciliations (2026-07)

Per-finding contract decisions from the stats/audit/search reconciliation slice.

- **S1 — unknown model → `404 MODEL_NOT_FOUND` (uniform).** All model-scoped read
  operations (`getAllEntities`, `getEntityStatisticsForModel`,
  `getEntityStatisticsByStateForModel`, `searchEntities`, `submitAsyncSearchJob`,
  `queryGroupedEntityStatisticsForModel`) now return `404 MODEL_NOT_FOUND` when the
  requested `(entityName, modelVersion)` is not registered for the calling tenant.
  Direction: spec-stale + server-gap (closed). Previous Cloud behaviour: list/stats/search
  silently returned empty; grouped-stats returned `400 UNKNOWN_MODEL`. The ad-hoc
  `UNKNOWN_MODEL` code is retired. Cloud MUST return `404 MODEL_NOT_FOUND` on all
  these paths for unregistered models.
- **S2 — `searchEntityAuditEvents.changes` diff (documented gap).** The
  `EntityAuditEventDto.changes` before/after diff field is declared in the schema but
  not emitted by cyoda-go (nor populated server-side). This is a deferred feature, not
  a spec contradiction. Direction: server-gap (open; tracked separately). Cloud behaviour
  is authoritative for now; cyoda-go will close the gap when the feature is implemented.
- **S3 — `NOT_FOUND` async search-job status (retained).** The `searchJobStatus` field
  in `GET /api/search/async/{jobId}/status` responses may carry the value `NOT_FOUND`.
  This is retained in the contract because the commercial self-executing search store
  emits it. Direction: needs-decision → retained (commercial store compatibility). Cloud
  MUST continue to emit `NOT_FOUND` where applicable; cyoda-go tolerates it on inbound
  status payloads.

## Model/workflow reconciliations (2026-07)

Per-finding contract decisions from the entity-model & workflow reconciliation slice.

- **M1 — `deleteEntityModel` enforces UNLOCKED (runtime change).** Deleting a
  LOCKED model now returns `409 MODEL_ALREADY_LOCKED` (reuses the locked-refusal
  code, delete-specific message). The `409 MODEL_HAS_ENTITIES` guard is retained
  (multi-node create/unlock TOCTOU backstop). Direction: server-gap (closed).
  Cloud MUST refuse deletion of a locked model.
- **M2 — documented error-code clarifications.** Previously-undocumented codes now
  in the contract are clarifications Cloud must match: unlock `MODEL_HAS_ENTITIES`
  / `MODEL_ALREADY_UNLOCKED`; lock & import `MODEL_ALREADY_LOCKED`; import
  `INVALID_UNIQUE_KEY_DEFINITION`; setUniqueKeys `MODEL_NOT_FOUND` / `BAD_REQUEST`
  / `COMPOSITE_KEY_UNSUPPORTED`; changeLevel `INVALID_CHANGE_LEVEL`; import/export
  unsupported-converter `BAD_REQUEST`; workflow-import `VALIDATION_FAILED` /
  `WORKFLOW_SCHEMA_VERSION_UNSUPPORTED`; workflow-export `MODEL_NOT_FOUND`-vs-
  `WORKFLOW_NOT_FOUND` disambiguation. Direction: spec-incomplete (closed).
- **M3 — `exportMetadata.uniqueKeys` typed.** The export 200 body now enumerates
  the top-level `uniqueKeys` array (typed-but-open) alongside `currentState`/`model`.
  Direction: spec-incomplete (closed). Cloud MUST emit `uniqueKeys` when keys exist.

## Auth / OIDC reconciliations (2026-07)

Per-finding contract decisions from the auth / OIDC reconciliation slice.

- **A1 — `registerOidcProvider` duplicate → `409` (runtime change).** Duplicate provider
  registration now returns `409 OIDC_PROVIDER_DUPLICATE` (was `400`). Direction: server-gap
  (closed). Cloud MUST return `409` on duplicate provider registration.
- **A2 — error envelope = RFC-9457 `ProblemDetail` on OIDC / admin ops.** The 7 OIDC provider
  ops and `searchEntityAuditEvents` emit `application/problem+json` `ProblemDetail` with
  `errorCode` under `properties`. Direction: spec-stale (closed). Cloud MUST emit `ProblemDetail`,
  not the OAuth `ErrorResponseDto`, on these ops. The OAuth token endpoint (`getTechnicalUserToken`)
  keeps the RFC-6749 flat shape.
- **A3 — documented-but-IAM-gated ops.** 21 ops (`501 NOT_IMPLEMENTED` when `CYODA_IAM_MODE ≠
  jwt`): 7 OIDC, 5 JWT-keypair, 4 M2M-client, and 5 trusted-key ops. Trusted-key nuance (B1):
  the 5 trusted-key ops check the feature flag first — they return `404 FEATURE_DISABLED` when
  `CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED=false` (default off); `501` is only reached when
  the feature is enabled AND `CYODA_IAM_MODE ≠ jwt`. Direction: spec-incomplete (closed). Cloud's
  IAM-mode and feature-flag contract must match.
- **A5 — `listOidcProviders.activeOnly` string → boolean (runtime change).** The query parameter
  is now a real boolean: standard truthy values (`1`, `true`, `TRUE`, `t`, …) filter; unparseable
  values return `400` (was silently false). Direction: spec-stale (closed). Cloud MUST treat
  `activeOnly` as a boolean.
- **A4 — roadmap-placeholder crypto enums.** `issueJwtKeyPair` retains the full 10-algorithm enum
  (only RS256 honoured). `registerTrustedKey` retains RSA / EC / OKP prose (only RSA honoured).
  Direction: needs-decision → RESOLVED keep-placeholder. Cloud may honour a wider set; cyoda-go
  rejects non-RS256/RSA with the documented `400`.

## Message-slice reconciliations (2026-07)

Per-finding contract decisions from the Edge-message reconciliation slice.

- **M1 — request body shapes corrected.** `newMessage` body is a JSON object envelope
  `{payload (any JSON value, required), meta-data (optional flat map)}` — not a bare `string`;
  a top-level array is rejected (single object only). `deleteMessages` body is a JSON `array` of
  uuid strings — not a single `string`. Direction: spec-incomplete (closed). Cloud's documented
  request contract must match these shapes.
- **M2 — fictional v1-UUID `400` removed; `messageId` typed `uuid`.** `getMessage` / `deleteMessage`
  performed no version-1 UUID validation and emit no `400`; the documented `400 "not a time-based
  UUID"` and `format: uuid-v1` were prototype-era fiction. Corrected to `format: uuid`, which (via
  codegen) now binds `messageId` as a typed UUID. A malformed uuid path (or query) param now returns
  a uniform RFC-9457 `ProblemDetail` `400` (`application/problem+json`, `properties.errorCode:
  BAD_REQUEST`, `properties.parameter: <name>`) via a custom binding-error handler that replaces
  oapi-codegen's `text/plain` default **API-wide** — the previous behaviour on every uuid path param.
  This `400` is now documented on `getMessage` / `deleteMessage`, and the entity id ops'
  (`getOneEntity` / `deleteSingleEntity`) `400` examples were corrected from their earlier fiction to
  match the real handler output. `deleteMessages` keeps its real invalid-JSON `400`. Direction:
  server-gap + spec-stale (closed, Gate-6 cross-cutting fix). **Cloud MUST emit the `ProblemDetail`
  shape** (not `text/plain`) for malformed path/query params.
- **M3 — `getMessage.metaData` simplified to a flat symmetric map (cyoda-go defines the contract).**
  The bucketed `metaData: {values, indexedValues}` split and the injected `typeReferences: {}` were
  cyoda-cloud indexing workarounds. In cyoda-go, `values` was always empty (all client `meta-data`
  routes to indexed storage) and `typeReferences` was an always-empty placeholder. cyoda-go now
  emits a **single flat map** — what a client PUTs in `meta-data` it GETs back in `metaData`. The
  `ValueMaps` / `LocalTime` schemas are deleted. This is a response-shape change only (no SPI /
  storage / indexing change). Direction: **cyoda-go leads — Cloud MUST conform**: emit the flat
  `metaData` map and drop `values` / `indexedValues` / `typeReferences` from the read response.
  Cloud's derived integration tests (e.g. the Kotlin edge-message test asserting
  `metaData.indexedValues.strings`) must be updated to the flat shape.
- **M4 — `deleteMessages` `413` on oversized body (runtime change).** A >10 MB batch-delete body now
  returns `413` (was `500`), matching `newMessage`. Direction: server-gap (closed, Gate-6 parity fix).
- **Deferred (out of this slice).** Honoring `transactionTimeoutMillis` / `transactionSize` uniformly
  → **#379** (supersedes #372). Native non-JSON / content-type payloads (binary without base64
  wrapping) → **#193**. The message contract documents current JSON-envelope behavior.

## Open questions (Cloud-fact-blocked)

Decided once the Cloud facts are gathered (Gate 7):

- **E6 — `EntityChangeMeta.fieldsChangedCount`.** Declared in canonical
  `EntityChangeMeta.json`, never emitted by cyoda-go. Question: **does Cloud emit
  `fieldsChangedCount`?** If yes → implement emission (server-gap); if no → remove
  from the canonical schema (spec-stale). Leaning: implement (removing a
  Cloud-consumed canonical field is higher-risk). Not implemented in this slice.
- **D2 — gRPC conditional delete.** The conditional delete is an HTTP-contract
  feature; the gRPC/proto contract exposes only unconditional delete-all + per-entity
  delete. Question: **does Cloud expect gRPC conditional-delete parity?** If yes →
  add it to the proto contract; if no → HTTP-only is correct. Not implemented here.
- **Error-code naming — `BAD_REQUEST` vs `MALFORMED_REQUEST`.** Entity write handlers
  emit `BAD_REQUEST` for malformed bodies; `grouped_stats` emits `MALFORMED_REQUEST`
  for the same class. Unifying spans create/collection/patch and the stats/search +
  model slices. Deferred to a cross-slice follow-on; the entity error tables document
  the codes actually emitted.

## Group 5 — dead-surface disposition (final reconciliation slice)

The excluded-tag / always-501 dead surface is disposed as follows:

- **SQL-Schema** (`/sql/schema/*`, 9 ops) — retained as `x-cyoda-status: planned`; the
  authored contract was corrected (well-formed array responses, array request body,
  `problem+json` 404s, `FieldConfigDto` reconciled to its examples). Implementation is
  tracked (Trino SQL management API, #382). Cloud mirrors the corrected contract.
- **Stream Data** (`/platform-api/stream-data/*`, 13 ops) — retained as
  `x-cyoda-status: unimplemented`; left minimally touched pending a disposition decision
  (implement / redesign / remove, #381).
- **CQL Execution Statistics** — vestigial exclude-tags entry removed (no ops).
- **accountSubscriptionsGet** — unchanged (`planned`, routed, returns 501; tracked in #283).

**Invariant:** every non-live `x-cyoda-status` marker is backed by a tracking issue, so a
marked surface can never become unowned relabeled fiction. The e2e conformance gate enforces
exactly-one-of {exercised, marked} and fails any marker that goes stale (op returns 2xx).
