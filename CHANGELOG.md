# Changelog

All notable changes to Cyoda-Go are documented here. The project follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) conventions and [Semantic Versioning](https://semver.org/) — pre-1.0, where the minor component signals a breaking change and new features ship in patches (see [README — Versioning](./README.md#versioning)).

## [Unreleased]

### Added

- **Entity partial-update (PATCH / RFC 7386 merge patch)** — `PATCH /api/entity/{format}/{entityId}`
  and `PATCH /api/entity/{format}/{entityId}/{transition}` apply a sparse JSON patch to the stored
  payload with RFC 7386 merge semantics (non-null key overwrites, explicit `null` deletes, omitted
  key untouched), closing the data-loss footgun where `PUT`'s wholesale-replace silently destroyed
  omitted fields. JSON-only (`XML` ⇒ `415`); `application/merge-patch+json` is implemented,
  `application/json-patch+json` (RFC 6902) returns `501`. `If-Match` is **required** (the merge is
  relative to the base the caller read): absent ⇒ `428 PRECONDITION_REQUIRED`, stale ⇒ `412`. The
  merged result is validated strictly against the model schema — PATCH never extends the model,
  even in an extend-permitting mode. Under a named transition the merge is applied first, then the
  transition's processors run on the merged state. New error codes: `PRECONDITION_REQUIRED` (428),
  `UNSUPPORTED_MEDIA_TYPE` (415).
  ([#341](https://github.com/Cyoda-platform/cyoda-go/issues/341))

- **Renderer annotations on processors & criteria** — the engine-ignored `annotations` bag now
  extends to the two workflow elements that lacked it: processors carry an embedded `annotations`
  object, and criteria carry a sibling `criterionAnnotations` object on the workflow and on each
  transition (the criterion tree round-trips verbatim and is never parsed to attach metadata). Two
  well-known optional keys — `displayName`, `description` — are documented uniformly across all
  five element types (workflow, state, transition, processor, criterion) for renderer and
  condition-builder use. Object-only, capped at 64 KB per field, stored and re-emitted compacted,
  never interpreted by the engine; processor annotations are stripped before dispatch and never
  reach compute members. Additive schema change: the workflow schema moves to **1.2** and every
  existing 1.1 payload remains valid (dual-shape). No new error codes.
  ([#384](https://github.com/Cyoda-platform/cyoda-go/issues/384))

- **Composite unique keys** — entity models can declare one or more composite unique keys via
  `PUT /model/{entityName}/{modelVersion}/unique-keys` (UNLOCKED models only). Each key is an
  ordered set of scalar field paths; uniqueness is scoped to `(tenant, model, version)` live
  entities. All-or-nothing null rule: all fields null or absent ⇒ exempt; partial ⇒
  `422 INVALID_UNIQUE_KEY`; all present ⇒ enforced on create and update. String comparison is
  byte-exact (case-sensitive, no normalization). Soft-delete frees the value-set.
  Supported by memory, sqlite, and postgres; the commercial backend returns
  `422 COMPOSITE_KEY_UNSUPPORTED` until its own support lands.
  New error codes: `UNIQUE_VIOLATION` (409), `INVALID_UNIQUE_KEY` (422),
  `COMPOSITE_KEY_UNSUPPORTED` (422), `INVALID_UNIQUE_KEY_DEFINITION` (422).

- **Search result sorting** — both search endpoints (`POST /api/search/direct/{entityName}/{modelVersion}`
  and the async variant `POST /api/search/async/{entityName}/{modelVersion}`) now accept one or more
  `sort` query parameters (HTTP) or a structured
  `orderBy` array (gRPC) to control result order. HTTP grammar: `[@]path[:asc|desc]` — bare
  dotted path for scalar data fields; `@`-prefixed name for meta fields (`state`, `creationDate`,
  `lastUpdateTime`, `transitionForLatestSave`, `transactionId`, `id`). Ordering is canonical
  across all backends: Text (byte order), Numeric (IEEE-754 double), Bool (`false < true`),
  Temporal (ms-floored chronological for meta date fields). Absent/null values sort last;
  `entity_id` is always the final tiebreaker. Unsortable, array, or unknown paths return
  `400 INVALID_FIELD_PATH`. Sort key count is capped at `CYODA_SEARCH_MAX_SORT_KEYS`
  (default `16`). New SPI field: `OrderSpec.Kind OrderKind` (enum: `OrderText`, `OrderNumeric`,
  `OrderBool`, `OrderTemporal`); ships with `cyoda-go-spi v0.8.2`.

- **Compute-node callback transaction-join** — processor and criteria-evaluation
  callbacks from a compute node now join the originating workflow transaction
  (`T`) rather than running in a standalone transaction. The engine mints a
  signed HMAC tx-token `{NodeID, TxRef}` before each dispatch and attaches it
  to the outbound CloudEvent as the `cyodatxtoken` extension attribute. Compute
  nodes echo the token on callbacks (`X-Tx-Token` HTTP header / `tx-token` gRPC
  metadata); the receiving node verifies the HMAC and routes the callback to the
  owner — local `Join` when owner is self, HTTP reverse proxy or gRPC EntityManage
  B→A forward otherwise. Callbacks see the cascade's uncommitted writes; callback
  acks are provisional until `T` commits. `ASYNC_NEW_TX` callbacks join `T` via a
  savepoint so writes are discarded on processor failure without aborting the
  cascade. An absent token causes the callback to run standalone (`Begin`), which
  is the normal behaviour for `COMMIT_BEFORE_DISPATCH` with
  `startNewTxOnDispatch=false`. New env vars: `CYODA_TX_TOKEN_TTL` (token
  validity, default `90s`), `CYODA_GRPC_NODE_ADDR` (gRPC address advertised in
  tokens for B→A forwarding), `CYODA_COMPUTE_HTTP_BASE` (base URL for
  compute-test-client HTTP callbacks).

- **Conditional `deleteEntities`** — `DELETE /api/entity/{entityName}/{modelVersion}` now honours
  an `AbstractConditionDto` request body, deleting only matching entities (empty body ⇒ all).
  `verbose=true` returns the deleted ids; `numberOfEntitites` (matched) and
  `numberOfEntititesRemoved` (removed) are reported separately. Closes a data-loss defect where
  the condition was ignored and the whole model was wiped. New error code `INVALID_CONDITION` (400).

- **`getAllEntities` point-in-time** — the model-scoped list read honours `pointInTime`, returning
  entities as-at the supplied time and stamping `meta.pointInTime`.

- **OpenAPI error-code conformance** — the E2E conformance validator now enforces documented
  error codes (`errorCode` string granularity) for the entity endpoints, in addition to response
  shapes.

- **Config-conditional `501` documented** — 21 IAM-gated operations (OIDC providers, JWT
  keypairs, trusted keys, M2M clients) now declare `501 NOT_IMPLEMENTED` in the spec when
  `CYODA_IAM_MODE ≠ jwt`. The 5 trusted-key ops additionally declare `404 FEATURE_DISABLED`
  when `CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED=false` (default off); the `501` is only
  reached when that feature is enabled and IAM ≠ jwt.

- **`getTechnicalUserToken` spec completions** — `client_credentials` grant type, `405
  method_not_allowed` on non-POST requests, and `server_error` / `method_not_allowed` error
  enum values are now declared in the spec.

- Workflow **processors** now accept an engine-ignored `annotations` object, and **criteria** an
  engine-ignored `criterionAnnotations` sibling (workflow-selection and transition guards).
  Well-known renderer keys `displayName`/`description` are documented across all five workflow
  element types (object-only, ≤64 KB, types advisory). Workflow schema version bumps **1.1 → 1.2**
  (additive; 1.1 still accepted).

### Changed

- **`DELETE /model/{entityName}/{modelVersion}` now enforces the documented
  UNLOCKED precondition** — deleting a `LOCKED` model returns `409
  MODEL_ALREADY_LOCKED` (previously the lock state was ignored). Unlock the model
  first. The `409 MODEL_HAS_ENTITIES` guard is unchanged.

- **Entity `meta` is typed-but-open** — `Envelope.meta` mirrors the canonical `EntityMetadata`
  (typed properties, never sealed); the obsolete `previousTransition` field is removed.

- **`listOidcProviders.activeOnly` retyped to boolean** — standard truthy values
  (`1`, `true`, `TRUE`, `t`) now correctly filter active-only results; unparseable
  values such as `?activeOnly=yes` return `400` instead of silently meaning false.

- **`changeType` spelling** — entity change records now use the canonical `CREATE/UPDATE/DELETE`
  across HTTP, gRPC, and the OpenAPI schema (HTTP previously emitted `CREATED/UPDATED/DELETED`).

- **gRPC entity `meta`** — now includes `modelKey` (and `pointInTime` when as-at), matching HTTP.

- Tightened the `create`/`createCollection` request-body schemas to their real shapes; documented
  unique-key `409`/`422` codes and reverse-chronological change ordering on the entity endpoints.

- PostgreSQL search now pushes supported predicates into SQL (JSONB `->>`
  extraction, numeric/range/string comparisons) instead of loading every entity
  of a model and filtering them in memory. Non-pushable operators (regex,
  case-insensitive) are post-filtered while rows stream, and `LIMIT`/`OFFSET`
  are pushed into SQL when no residual remains. This is a constant-factor win —
  no full-result wire transfer, no decode of every document, filtering and
  pagination done in the database — not a JSON-path-index speedup; adding
  indexes on queried paths remains a separate operational step. SQLite already
  did this; the memory backend keeps filtering in memory by design.
  ([#37](https://github.com/Cyoda-platform/cyoda-go/issues/37))

- **Unknown model → `404 MODEL_NOT_FOUND` (uniform)** — all model-scoped read
  operations (`getAllEntities`, `getEntityStatisticsForModel`,
  `getEntityStatisticsByStateForModel`, `searchEntities`, `submitAsyncSearchJob`,
  `queryGroupedEntityStatisticsForModel`) now return `404 MODEL_NOT_FOUND` when
  the requested model is not registered for the calling tenant. Previously these
  paths returned empty results silently; the ad-hoc `UNKNOWN_MODEL` code used by
  grouped-stats is retired.

- **`searchEntities` limit enforcement** — `limit > 10000` is now rejected with
  `400 BAD_REQUEST` across synchronous search (HTTP), gRPC direct search, and
  async search submission. Previously the spec documented this as a silent clamp.

- **`searchEntities` content type** — the synchronous search endpoint responds
  with `application/x-ndjson` only; the previously-listed `application/json`
  variant is removed from the contract.

- **`listOidcProviders` fictional `403` removed** — the `403` response was never
  emitted by the server (the endpoint is auth-only, not admin-only); the spec entry
  is removed.

- **Edge-message request metadata field renamed `meta-data` → `metaData`** — the
  `POST /api/message/new/{subject}` request envelope now carries its optional metadata map
  under `metaData` (camelCase), symmetric with the `getMessage` response and consistent with
  the rest of the API's JSON naming. The former kebab-case `meta-data` key is no longer honored;
  its contents are ignored. Breaking input change, shipped in a patch because edge messages have
  no known consumers. A new `cyoda help messages` topic documents the full edge-message API.
  ([#386](https://github.com/Cyoda-platform/cyoda-go/issues/386))

### Removed

- **`pointInTime` param on `getAsyncSearchResults`** — the point-in-time is
  fixed at job submission; the param was a no-op and is removed from the contract.

- **`timeoutMillis` param and `408` on `searchEntities`** — these were fictional
  contract surface not backed by an implementation; both are removed. Actual
  per-request timeout support is tracked separately.

- **Fictional time-based-UUID `400` on `getStateMachineFinishedEvent`** — any
  valid UUID is accepted; the fictional constraint is removed from the spec.

### Fixed

- **OIDC / admin op error envelope** — the 7 OIDC provider ops and
  `searchEntityAuditEvents` now declare `application/problem+json` `ProblemDetail`
  errors in the spec, matching the server. `getTechnicalUserToken` retains the
  RFC-6749 flat OAuth shape (`{error, error_description}`).

- **`registerOidcProvider` duplicate returns `409`** — duplicate provider
  registration now returns `409 OIDC_PROVIDER_DUPLICATE` (was `400`). The `400`
  path remains for validation failures (`OIDC_SSRF_BLOCKED`, `OIDC_INVALID_TENANT`,
  malformed body).

- **`ProblemDetailDto` schema consolidated** — the structural duplicate is removed;
  the 9 async-search error responses now reference the canonical `ProblemDetail`
  schema.

- **`getStateMachineFinishedEvent` error envelope** — error responses now use
  `application/problem+json` (`ProblemDetail`), matching the rest of the API.

- **`getAsyncSearchResults` documented default page size** — corrected from 10
  to 1000 in the spec; the implementation was already using 1000.

- Point-in-time ("as at T") reads now apply one canonical rule across all
  storage engines and read paths: inclusive of the requested instant (`<=`),
  compared at native precision, with no millisecond round-up. Previously the
  memory engine and the SQL `GetAsAt`/`GetAllAsAt` paths rounded the requested
  time up to the next millisecond (over-including later same-millisecond
  versions), and sqlite used a strict `<` bound — so different backends, and
  different read paths within one backend, could disagree at sub-millisecond
  boundaries. ([#349](https://github.com/Cyoda-platform/cyoda-go/issues/349))

### Security

- Bumped the Go toolchain `go 1.26.4` → `go 1.26.5` (root + all three plugin
  modules and `go.work`) to clear govulncheck advisory GO-2026-5856, a reachable
  `crypto/tls` vulnerability in the Go standard library fixed in go1.26.5.

- Bumped the indirect `github.com/yuin/goldmark` dependency `v1.7.13` → `v1.7.17`
  to clear govulncheck advisory GO-2026-5320 (XSS in goldmark HTML rendering,
  reached via `glamour` in the `cyoda help` renderer). The renderer only formats
  first-party help content embedded in the binary, so the advisory was not
  reachable with attacker-controlled input; the bump keeps the security scan clean.

## [0.8.1] — 2026-06-23

> No `v0.8.0` release. The `cyoda-go-spi v0.8.0` tag was poisoned by a premature
> tag on the Go module proxy and abandoned in favour of `v0.8.1` (see
> [COMPATIBILITY.md](./COMPATIBILITY.md)). To keep the binary aligned with the
> SPI it pins, `cyoda-go` skips `v0.8.0` too — `v0.8.1` is the first v0.8.x
> binary release. No functionality differs from what `v0.8.0` would have shipped.

### Added

- Optional `annotations` JSON-object field on workflows, states, and transitions — arbitrary client-owned metadata, stored and round-tripped (compacted) but never interpreted by the engine. Object-only, capped at 64 KB per field.
- New error code `WORKFLOW_SCHEMA_VERSION_UNSUPPORTED` (`400`).
- New help topic `workflows.schema-version` documenting the wire-format contract.
- New help action `cyoda help workflows schema-version versions` emitting the supported-version manifest as JSON.
- HTTP help-action mirror: `GET /help/<topic>/<action>` now reachable for every registered action (`grpc proto/json`, `openapi json/yaml/tags`, `cloudevents json`, `workflows.schema-version versions`) with declared `Content-Type`.
- OIDC provider per-tenant registry with 7 REST endpoints under `/oauth/oidc/providers` (register, list, get, update, invalidate, reactivate, delete, reload). Closes [#284](https://github.com/Cyoda-platform/cyoda-go/issues/284).
- Chained multi-issuer JWT validator: `JWKSValidator` (local issuer) first, `OIDCValidator` (registered OIDC providers) second — per ADR 0002 decision D3.
- Per-provider configurable fields: `issuers` (accepted `iss` values), `expectedAudiences`, `rolesClaim` (overrides the global default per-provider).
- Cluster broadcast for OIDC cache eviction: when a provider record is mutated or reloaded, a fire-and-forget message on `oidc-providers.invalidate` evicts the JWKS cache on every peer node — consistent with the model-cache invalidation pattern (single topic, no ACK required per ADR 0002 D7).
- 6 new env vars: `CYODA_OIDC_REQUIRE_HTTPS`, `CYODA_OIDC_CONNECT_TIMEOUT_MS`, `CYODA_OIDC_SOCKET_TIMEOUT_MS`, `CYODA_OIDC_CONNECTION_REQUEST_TIMEOUT_MS`, `CYODA_OIDC_ALLOW_PRIVATE_NETWORKS`, `CYODA_OIDC_ROLES_CLAIM`.
- 4 new error codes: `OIDC_PROVIDER_DUPLICATE`, `OIDC_PROVIDER_NOT_FOUND`, `OIDC_PROVIDER_INACTIVE`, `OIDC_SSRF_BLOCKED`. (5 additional error codes are wire-stubbed for future bearer-auth translation.)
- ADR 0002 — Federated Identity Provider Architecture (`docs/adr/0002-federated-identity-provider-architecture.md`).
- `/oauth/keys/keypair/*` and `/oauth/keys/trusted/*` — 10 admin endpoints now conform to the OpenAPI surface via chi-routed adapters in `internal/domain/account/`. ([#281](https://github.com/Cyoda-platform/cyoda-go/issues/281), sub-issue of [#194](https://github.com/Cyoda-platform/cyoda-go/issues/194))
- `/clients` OpenAPI surface — `GET /clients`, `POST /clients`, `DELETE /clients/{clientId}`, `PUT /clients/{clientId}/secret`. M2M client management is now reachable at the spec-conformant paths with the spec-conformant DTOs.
- 6 new error codes: `FEATURE_DISABLED`, `KEY_OWNED_BY_DIFFERENT_TENANT`, `KEYPAIR_NOT_FOUND`, `TRUSTED_KEY_CAP_REACHED`, `UNSUPPORTED_ALGORITHM`, `UNSUPPORTED_KEY_TYPE`.
- Error code `M2M_CLIENT_NOT_FOUND` (HTTP 404) emitted by the `/clients` admin operations on unknown or cross-tenant `clientId`.
- 5 new env vars: `CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED`, `CYODA_IAM_TRUSTED_KEY_MAX_PER_TENANT`, `CYODA_IAM_TRUSTED_KEY_MAX_VALIDITY_DAYS`, `CYODA_IAM_TRUSTED_KEY_MAX_JWK_PROPERTIES`, `CYODA_IAM_KEYPAIR_DEFAULT_VALIDITY_DAYS`. Plus `CYODA_JWT_BOOTSTRAP_AUDIENCE`.
- `CYODA_IAM_M2M_ADMIN_ROLE_ENABLED` env (default `false`) gates the `withAdminRole=true` query parameter on `POST /clients`. When off the request returns `404` with error code `FEATURE_DISABLED`.

### Changed

- **Cross-tenant OIDC routing is now deterministic and safe.** When two tenants register the same `wellKnownConfigUri` (same physical IdP), tokens are no longer routed non-deterministically via Go's randomized map iteration. Resolution now uses a two-layer disambiguation: (1) audience-based routing — if the JWT's `aud` claim uniquely matches one tenant's `ExpectedAudiences`, that provider is selected; (2) if no unique audience match exists (both providers have empty or overlapping `ExpectedAudiences`), the token is rejected with `401 UNAUTHORIZED` (`ErrAmbiguousProvider`). Operators sharing an IdP across tenants MUST set distinct `expectedAudiences` on each registration. A `WARN` log (`oidc.cross_tenant_audience_overlap`) is emitted at Register-time when audience overlap is detected. ([#284](https://github.com/Cyoda-platform/cyoda-go/issues/284))
- **OIDC pinned `issuers` are now enforced at discovery-fetch time.** When a provider's `issuers` list is non-empty, the discovery document's `issuer` field must match one of the pinned values; a mismatch refuses to install the provider source (logged at `WARN` with SHA-256 hashes of the issuer values, never raw strings) and the provider remains in the Phase-2-pending state until the admin reconciles. This is defence-in-depth: the runtime `issMatches` already enforces the pin at token-resolution time, but enforcing it at fetch time prevents the registry from caching an attacker-controlled `discoveryDoc.Issuer` value that could be silently trusted by future diagnostics or metrics code. ([#284](https://github.com/Cyoda-platform/cyoda-go/issues/284))
- OIDC provider registration now requires the calling tenant to be UUID-shaped.
  Bootstrap deployments using the literal `default-tenant` string for
  `CYODA_BOOTSTRAP_TENANT_ID` must migrate to a real tenant UUID before
  registering OIDC providers. Returns `400 BAD_REQUEST` with code
  `OIDC_INVALID_TENANT` otherwise.
- Seven `/oauth/oidc/providers/*` endpoints previously returned `501 NOT_IMPLEMENTED`; they now return real responses. Clients that special-cased the 501 status on these paths should update their error handling.
- Legacy `/oauth/keys/` prefix mux entry removed from `app/app.go`; chi router now owns all `/oauth/keys/*` paths.
- JWKS endpoint (`/.well-known/jwks.json`) now publishes grace-period-invalidated keys until their `validTo` passes.
- `KVTrustedKeyStore` KV-key encoding within the `trusted-keys` namespace changed from `<kid>` to `<tenantID>:<kid>`. Tenant isolation is now enforced at the storage layer.
- Trusted-key per-tenant cap counts only currently-valid keys (matches Cyoda Cloud).
- `withAdminRole` query parameter on `POST /clients` tightened from `string` to `boolean` in `api/openapi.yaml`. This is a deliberate divergence from the upstream Cyoda Cloud OpenAPI declaration.
- `auth.M2MClient.TenantID` promoted from `string` to `spi.TenantID`. `M2MClient` now carries `CreatedAt` and `UpdatedAt` timestamps.
- Workflow import (`POST /model/{entityName}/{modelVersion}/workflow/import`) now rejects structurally broken workflows with `400 VALIDATION_FAILED` instead of accepting them and degrading silently at runtime. New rules: empty workflow name, duplicate workflow names within a single request, empty `initialState`, `initialState` not declared in `states`, empty state-map key, transition `next` not declared in `states`, empty or duplicate transition names within a state, empty processor name, workflow / state / transition / processor name length > 256 chars, and unknown `executionMode` values. OpenAPI schema updated with matching `minLength: 1` + `maxLength: 256` on identifier fields and `propertyNames` on the `states` map. See ⚠️ Breaking changes below. ([#255](https://github.com/Cyoda-platform/cyoda-go/issues/255))
- Workflow import (`POST /model/{entityName}/{modelVersion}/workflow/import`) now honours an explicit `"active": false` on each incoming `WorkflowDefinition`. The handler previously force-overrode every incoming `active` to `true`; it now defaults to `true` only when the field is absent (or explicitly `null`) and passes explicit `true` / `false` through unchanged. This restores export → REPLACE re-import idempotency and lets operators stage inactive workflows. See ⚠️ Breaking changes below. ([#256](https://github.com/Cyoda-platform/cyoda-go/issues/256))
- Workflow engine: substitution of the embedded default workflow now emits a `slog.Warn` line in addition to the existing response-body warning. Log fields: `pkg=workflow`, `tenant`, `entityName`, `modelVersion`, `entityId`, `reason` ∈ {`no_workflows_imported`, `no_criterion_matched`}. The body warning text is corrected per cause — `"no workflows imported for model — using default workflow"` for the cold paths in `Execute` / `ManualTransition` / `Loopback`, `"no imported workflow matched entity — using default workflow"` for `selectWorkflow`. Operators driving large fleets can now detect models silently running on the embedded default. ([#256](https://github.com/Cyoda-platform/cyoda-go/issues/256))
- Workflow import (`POST /model/{entityName}/{modelVersion}/workflow/import`) now decodes the request body with `DisallowUnknownFields`. Unknown fields — at the top level *or* nested in the workflow / state / transition / processor sub-shapes — are rejected with `400 BAD_REQUEST` and the offending field name surfaced in the response detail (Go's decoder emits `json: unknown field "X"`). Typos like `"transitionn"` for `"transitions"` no longer silently import as a no-op. See ⚠️ Breaking changes below. The broader `DisallowUnknownFields` sweep across entity / cluster / auth boundaries remains in [#145](https://github.com/Cyoda-platform/cyoda-go/issues/145). ([#264](https://github.com/Cyoda-platform/cyoda-go/issues/264))

### Removed

- Private `/account/m2m*` HTTP surface and its `internal/auth/m2m.go` handler. M2M client management is exclusively at `/clients` going forward. `/account/m2m*` was never OpenAPI-declared.
- `501 NOT_IMPLEMENTED` response declarations on `listTechnicalUsers`, `createTechnicalUser`, `deleteTechnicalUser`, `resetTechnicalUserSecret`, `getTechnicalUserToken` in `api/openapi.yaml`.

### ⚠️ Breaking changes

- **Reactivate semantics changed.** `POST /oauth/keys/keypair/{keyId}/reactivate` and `POST /oauth/keys/trusted/{keyId}/reactivate` now require a `ReactivateKeyRequestDto` body with `validTo > now` (and `> validFrom` if supplied). Previously these endpoints had no request body. Cyoda Cloud's behaviour of clearing `validTo` to nil (zombie key) is intentionally not adopted; see [#281](https://github.com/Cyoda-platform/cyoda-go/issues/281) spec for rationale.
- **Trusted-key registration is disabled by default.** Set `CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED=true` to enable. Customers using `/oauth/keys/trusted/*` through the legacy mux must opt in.
- **Bootstrap signing key now has finite validity.** Defaults to 365 days (configurable via `CYODA_IAM_KEYPAIR_DEFAULT_VALIDITY_DAYS`). Long-running deployments must rotate before expiry; the startup banner emits a `WARN` if the active key expires within 30 days.
- **Algorithm scope.** cyoda-go v0.8.0 signs and verifies `RS256` only. The OpenAPI declares the full enum (`RS*`, `PS*`, `ES*`, `EdDSA`); non-`RS256` values are rejected with `400 UNSUPPORTED_ALGORITHM`. Trusted-key registration accepts only `kty=RSA` JWKs (`kty=EC`/`OKP` rejected with `400 UNSUPPORTED_KEY_TYPE`). v0.8.1 follow-up tracks multi-algorithm + non-RSA `kty` support.
- **Workflow-import structural validation tightened.** Imports that previously succeeded with structurally broken shapes (typo'd `executionMode`, empty/dangling `initialState`, transitions pointing at undeclared states, duplicate workflow names within a request, duplicate transition names within a state, empty workflow / state / transition / processor names, identifiers longer than 256 characters) now fail with `400 VALIDATION_FAILED`. These new H4/H6 structural rules apply to the **incoming request only** — existing stored workflows are not retroactively re-validated against them, so an in-place upgrade does not invalidate previously-imported configurations. The pre-existing cycle-detection and `startNewTxOnDispatch` flag-coherence checks continue to run against the merged result and so still catch regressions in stored workflows, preserving pre-v0.8.0 semantics for those specific invariants. ([#255](https://github.com/Cyoda-platform/cyoda-go/issues/255))
- **Workflow-import `active` field is no longer force-overridden.** Previously, every incoming workflow's `active` was unconditionally set to `true`, so a client sending `"active": false` was silently re-activated. The handler now passes explicit `true` / `false` through unchanged and only defaults to `true` when the field is absent. Clients that were relying on the force-override (knowingly or not) to coerce inactive workflows to active on import must update their payloads to send `"active": true` explicitly (or omit the field). ([#256](https://github.com/Cyoda-platform/cyoda-go/issues/256))
- **Workflow-import `workflows: []` is rejected in `REPLACE` / `ACTIVATE` modes.** Previously these modes accepted an empty workflows array, silently wiping or deactivating all stored workflows for the model and falling back to the embedded default at runtime — HTTP 200 hid the destruction. Both modes now return `400 VALIDATION_FAILED` with detail `"empty workflows array not allowed in REPLACE/ACTIVATE mode — use MERGE if you intended a no-op"`. The `workflows` key being absent entirely is equivalent to `[]` under JSON unmarshal semantics and is rejected the same way. `MERGE` with an empty array remains a legitimate no-op. ([#256](https://github.com/Cyoda-platform/cyoda-go/issues/256))
- **Workflow-import body is strict-decoded.** The `POST /model/{entityName}/{modelVersion}/workflow/import` handler now rejects any unknown JSON field anywhere in the import-request body — at the top level, on a workflow, on a state, on a transition, on a processor, or on a processor's `config` — with `400 BAD_REQUEST`. The handler also rejects trailing JSON content after the request object (a body like `{...valid request...}{junk}`), which `json.Decoder.Decode` would otherwise silently drop. Clients that previously relied on the silent-drop behaviour (e.g. sending forward-compat extras intended for a future cyoda-cloud version, or with typo'd field names) must clean their payloads before upgrading. The trade-off is intentional: typos like `"transitionn"` for `"transitions"` used to import as a no-op workflow with zero transitions, hiding the configuration error from operators. The response detail names the offending field verbatim so the fix is trivial. ([#264](https://github.com/Cyoda-platform/cyoda-go/issues/264))
- **Workflow-import `version` field is now strictly validated** as semver `MAJOR.MINOR`, and v0.8.0 bumps the supported set from `1.0` to `1.1`. Previously accepted values like `"1"` are rejected with `400 WORKFLOW_SCHEMA_VERSION_UNSUPPORTED`. `"1.0"` (the schema shipped on `release/v0.7.x`) is also rejected — v0.8.0's import surface adds enough strictness (structural validation #255, active semantics #256, asyncResult/crossover rejection #261, retryPolicy enum #262, strict-decoder #264, scheduled-transition shape) that staying on `1.0` would conflate two distinct contracts. v0.7.x clients must regenerate workflow payloads against `"1.1"`. Authoritative supported set: `GET /help/workflows/schema-version/versions` or `cyoda help workflows schema-version versions`. Bump rules and per-version notes: `docs/workflow-schema-versioning.md`.

### Fixed

- **Workflow audit-log `desc` preview is rune-aware.** The `truncateForLog` helper used by the workflow-import audit logger previously measured byte length and sliced byte offsets, splitting multi-byte UTF-8 characters (CJK, emoji, accented Latin) mid-codepoint and emitting invalid UTF-8 into the audit log. The helper now counts runes and cuts on rune boundaries, matching its documented contract. The cap is renamed `descLogPreviewRunes` to remove the bytes-vs-runes ambiguity. Surfaced by the [#264](https://github.com/Cyoda-platform/cyoda-go/issues/264) security review.
- **Workflow cycle-detection error reporting is deterministic.** `validateWorkflowLoops` previously iterated `wf.States` in Go map order, so a workflow with two or more disjoint unguarded automated cycles reported a different cycle path per run. The detector now sorts state names before iteration so the lexicographically-first cycle is reported. Surfaced by the [#264](https://github.com/Cyoda-platform/cyoda-go/issues/264) security review.

### Known limitations

- **Runtime-issued signing keypairs are lost on process restart.** The bootstrap key survives (its KID is deterministic per PEM). Persistent signing-key storage is tracked in a v0.8.x follow-up.
- **Pre-v0.8.0 KV trusted-key entries are orphaned.** Within the `trusted-keys` namespace, entries are now keyed `<tenantID>:<kid>` (was bare `<kid>`). v0.8.0 does not query the old shape; affected entries are left in place but not loaded. Operators must re-register affected keys. To audit, look for entries in the `trusted-keys` namespace whose key contains no `:` separator (the exact query depends on the KV backend; for the SQLite plugin: `SELECT key FROM kv_store WHERE namespace='trusted-keys' AND key NOT LIKE '%:%'`).
- **v0.8.0 → pre-v0.8.0 rollback hazard.** Trusted keys created under v0.8.0 are visible to pre-v0.8.0 binaries as mangled-kid entries (`<tenantID>:<kid>` treated as the kid). Purge out-of-band before rollback if visibility matters.
- **M2M clients created via `POST /clients` are held in-memory by the default `InMemoryM2MClientStore` and do not survive a server restart.** Customers running with the in-memory IAM mode must re-create their clients on every restart. A persistence follow-up tracking storage-SPI backing is on the roadmap; see the v0.8.0 milestone discussion.

### Dependencies

- Routine minor/patch dependency maintenance across the root and plugin modules: OpenTelemetry 1.43 → 1.44 (SDK, metric, trace, exporters, contrib), `jackc/pgx/v5` 5.9 → 5.10, `golang.org/x/crypto` 0.52 → 0.53, `getkin/kin-openapi` 0.139 → 0.140, `oapi-codegen/runtime` 1.4.1 → 1.4.2, `testcontainers-go` postgres 0.42 → 0.43, `ncruces/go-sqlite3` 0.34 → 0.35, and assorted `golang.org/x` updates.

---

## [0.7.0] — 2026-05-05

This release reconciles the OpenAPI spec with the actual server (#21,
breaking — see below), adds API-wide CORS, hardens the supply chain
with cosign keyless signatures on archives + checksums, closes a
suite of locking-discipline and tenant-isolation gaps across the
plugins, and ships a new chart docs surface for Gateway API operators.
18 issues closed in this milestone.

### ⚠️ Breaking changes (wire format)

The OpenAPI spec at `api/openapi.yaml` has been reconciled with the actual server wire format across all 81 declared operations. Clients generated from the pre-0.7.0 spec will be incorrect for the endpoints listed below — regenerate clients against `v0.7.0`'s `api/openapi.yaml` (or fetch via `cyoda help openapi yaml`).

**Server response shape changes:**

- **`GET /message/{messageId}` (`getMessage`)** — `content` field is now embedded JSON, not a JSON-encoded string. Wire was `"content": "{\"x\":1}"`; now `"content": {"x":1}`. Clients that did `JSON.parse(content)` must consume `content` directly.
- **Stub error code (account/IAM/OIDC/OAuth-keys ops)** — `errorCode` value in 501 responses changed from `"BAD_REQUEST"` to `"NOT_IMPLEMENTED"`. Pairs correctly with the HTTP status now.
- **`getStateMachineFinishedEvent`** — response now includes `microsTime` field (additive; non-breaking unless client strict-rejects unknown fields).

**Spec declaration changes (server unchanged but client codegen will differ):**

- All 4xx/5xx responses on entity ops, workflow export/import, and shared `components.responses.*` fragments now declare `Content-Type: application/problem+json` (RFC 9457). Server has always emitted this; spec was wrong.
- `getEntityChangesMetadata.changeType` enum corrected from `[CREATE, UPDATE, DELETE]` to `[CREATED, UPDATED, DELETED]`.
- `EntityTransactionResponse.entityIds` declared as `array<string>` (UUIDs), not `array<object>`.
- `getOneEntity` response declares the `Envelope` named schema `{type, data, meta}` instead of loose `type:object`.
- 7 malformed `type:array + sibling $ref` sites in the spec corrected to well-formed `type:array, items:{ $ref:... }` (`create`, `createCollection`, `updateCollection`, `getEntityChangesMetadata`, 3 statistics variants, `getAvailableEntityModels`).
- `messaging.deleteMessage` declares `MessageDeleteResponse` (`{entityIds: array<string>}`) instead of `EntityTransactionResponse` (no `transactionId` was ever emitted by the server).
- `messaging.deleteMessages` and `newMessage` declare `array<EntityTransactionResponse>` (was `type:string`, which never matched the server).
- 22 IAM/OAuth/OIDC/account stub endpoints declare `501 Not Implemented` per the design's deferred-implementation policy. Real implementation is tracked in #194. Clients generated from the pre-0.7.0 spec for these endpoints will be wrong.
- `basicAuth` security scheme declared (was referenced but never declared).

### Added

#### API + observability
- **`COMMIT_BEFORE_DISPATCH` processor execution mode** ([#27](https://github.com/Cyoda-platform/cyoda-go/issues/27)) — per-processor saga semantics for long-running cascades. Marking a processor with `executionMode: COMMIT_BEFORE_DISPATCH` (CBD) tells the engine to commit `TX_pre` before the processor runs and start `TX_post` for follow-on work, breaking the all-or-nothing dependency between the cascade-entry transaction and external dispatch. Engine surface gains `EngineResult.FinalCtx` / `FinalTxID` / `Segmented` so callers can commit the engine's final segment instead of the original (already-committed for CBD) entry transaction. New engine entry-points `ManualTransitionWithIfMatch` / `LoopbackWithIfMatch` plumb the caller's `If-Match` expected-txID via a single-shot context slot and apply `CompareAndSave` instead of `Save` at the FIRST CBD segment-flush, so a stale precondition aborts BEFORE any external dispatch fires (spec §4.1).
- **Per-item `ifMatch` on `PUT /api/entity/{format}` (bulk update)** ([#228](https://github.com/Cyoda-platform/cyoda-go/issues/228)) — same cross-request optimistic-concurrency precondition the single-item PUT endpoints support via `If-Match`, scoped per item on the bulk endpoint. Routing mirrors single `UpdateEntity`'s post-#27 flow: for CBD cascades the engine consumes the precondition at the first segment-flush; for non-segmenting cascades the handler applies `CompareAndSave` post-engine. Per-item `ENTITY_MODIFIED` conflicts are isolated to a new optional per-chunk `failed[]` array — the chunk still commits its remaining successful items rather than rolling everything back. Other per-item failures (missing entity, validation, non-conflict engine errors) continue to roll the chunk back, matching the pre-#228 [#92](https://github.com/Cyoda-platform/cyoda-go/issues/92) contract. When every item in a chunk fails its precondition, the chunk still commits as a zero-write transaction so the surfaced `transactionId` remains meaningful for audit correlation. Wire-format additions on `EntityTransactionResponse`: optional `failed[]` with `{entityId, error: {code, message, itemIndex}}`. `failed` uses JSON `omitempty` — fully-successful chunks keep the pre-#228 shape unchanged.
- **`TRANSITION_ABORTED` audit event** ([#228](https://github.com/Cyoda-platform/cyoda-go/issues/228) reviewer S1) — whenever a stale `ifMatch` precondition rejects an in-flight transition (engine CBD first-segment flush or handler-side post-engine `CompareAndSave`), the engine emits a paired `TRANSITION_ABORTED` event into the state-machine audit log alongside the entry-side `STATE_MACHINE_START` so consumers see a self-consistent entry+abort sequence. Payload carries `{reason: "ENTITY_MODIFIED", transitionName, expectedTxId, actualTxId}`. Applies to single `UpdateEntity` stale-`If-Match` and to the new `UpdateEntityCollection` per-item-isolated path. New constant added to `StateMachineAuditEventDto.eventType` enum.
- **API-wide CORS support** ([#196](https://github.com/Cyoda-platform/cyoda-go/issues/196)). New CORS middleware at `internal/api/middleware/cors.go` wraps the entire handler chain. Loopback-by-default mode (`http(s)://localhost`, `127.0.0.1`, `[::1]` on any port) — zero-config dev ergonomics, secure-by-default in production. Wildcard requires explicit `CYODA_CORS_ALLOWED_ORIGINS=*` (with startup WARN). Allowlist mode for production. Master switch `CYODA_CORS_ENABLED`. `/_internal/*` excluded from CORS; cluster proxy strips `Origin` and `Access-Control-Request-*` headers on outbound peer-to-peer requests (defence-in-depth).
- **OpenAPI runtime conformance validator** (`internal/e2e/openapivalidator/`) — every E2E response is matched against the spec via `kin-openapi`. Drift fails the build. Documented in [ADR 0001](./docs/adr/0001-openapi-server-spec-conformance.md).
- **2 previously-undocumented customer endpoints declared in the spec:**
  - `getEntityTransitions` (GET `/entity/{entityId}/transitions`)
  - `fetchEntityTransitions` (GET `/platform-api/entity/fetch/transitions`)
- **7 new named schemas** in `components/schemas/`: `Envelope`, `EdgeMessagePayload`, `MessageDeleteResponse`, `MessageDeleteBatchResponse`, `TransitionNameList`, `WorkflowImportSuccessDto`, `AuditEvent` (oneOf+discriminator union for state-machine + entity-change + system audit events).
- **4 shared response fragments** in `components/responses/`: `Unauthorized`, `Forbidden`, `InternalServerError`, `NotImplemented`.

#### Supply chain
- **Cosign keyless signatures** on release archives and `SHA256SUMS` ([#47](https://github.com/Cyoda-platform/cyoda-go/issues/47)). Sigstore + GitHub Actions OIDC; signing identity bound to `release.yml@refs/tags/v…` (push-trigger only). `scripts/install.sh` auto-verifies when `cosign` is on PATH; opt-out via `CYODA_COSIGN_VERIFY=false`; force-fail-without-cosign via `CYODA_COSIGN_VERIFY=required`.
- **`install.sh` published as a release asset at a stable URL** ([#49](https://github.com/Cyoda-platform/cyoda-go/issues/49)). The canonical install URL is now `https://github.com/cyoda-platform/cyoda-go/releases/latest/download/install.sh` — pinned per release, not a moving target on `main`.

#### SPI + cluster
- **`modelcache.CachingModelStore.SubscribeLocal`** ([#174](https://github.com/Cyoda-platform/cyoda-go/issues/174)) — in-process invalidation hook that fires for every model invalidation regardless of cluster topology. The path-validation cache wires through this so single-node and multi-node deployments alike react to schema changes immediately.
- **`fixtureutil.LaunchCyodaClusterAndComputeWithBinaries`** ([#157](https://github.com/Cyoda-platform/cyoda-go/issues/157)) — caller-supplied-binary variant of the cluster launcher, mirroring the single-node `…WithBinaries` symmetry. Out-of-tree backend plugins (e.g. `cyoda-go-cassandra`) can now drive the shared parity scenario suite against their own `cmd/cyoda` binary.
- **`AsRetryable()` fluent helper on `*AppError`** ([#140](https://github.com/Cyoda-platform/cyoda-go/issues/140)) — separates the (status, code, retryable) axes that were previously bundled into specialised `Conflict` / `RetryableConflict` constructors. Permits retryable 4xx with a specific dictionary code (previously unreachable). The deprecated constructors are removed; all callers migrated.

#### Documentation
- **Chart docs**: `deploy/helm/cyoda/docs/migrating-from-ingress.md` ([#57](https://github.com/Cyoda-platform/cyoda-go/issues/57)) — six-step Ingress2Gateway 1.0 walkthrough; `deploy/helm/cyoda/docs/gateway-api-policies.md` ([#58](https://github.com/Cyoda-platform/cyoda-go/issues/58)) — concrete `BackendTrafficPolicy` (rate limiting) and `SecurityPolicy` (JWT, CORS) YAML for Envoy Gateway, plus Cilium and Contour reference patterns.
- **Concurrency model** at `docs/CONCURRENCY.md` ([#199](https://github.com/Cyoda-platform/cyoda-go/issues/199) PR-C3) — per-node lock and state inventory, the SPI tx-state locking contract, cluster-routing failure modes; complements `docs/CONSISTENCY.md` (isolation contract) and `docs/ARCHITECTURE.md` (cluster routing).

### Fixed

#### API correctness
- **Collection endpoints now match the documented `transactionWindow` chunking + engine-routing contract** ([#227](https://github.com/Cyoda-platform/cyoda-go/issues/227)) — four pieces:
  - `CreateEntityCollection` now routes every item through the workflow engine. Pre-fix the handler hard-coded `State="CREATED"` and called `entityStore.SaveAll` directly, so the workflow's `initialState` was ignored, automated cascade transitions never fired during create, and no `STATE_MACHINE_*` audit events were emitted for collection-created entities. Now mirrors single `CreateEntity`'s engine flow per item.
  - `CreateCollection` and `UpdateCollection` handlers honor `transactionWindow` and chunk per the documented contract (default 100, max 1000, validated to (0, 1000]). Pre-fix `CreateCollection` ignored the param entirely and `UpdateCollection` rejected oversize batches with 400. Both now split the batch into chunks of size `window`, commit each chunk in its own transaction in commit order, and emit one `EntityTransactionResponse` element per committed chunk. Each chunk remains all-or-nothing internally, and chunks committed before any later failure stay durable.
  - Single-create endpoint `POST /api/entity/{format}/{entityName}/{modelVersion}` array-body path now chunks too. The handler auto-detects a JSON-array body and previously delegated the whole batch to `CreateEntityCollection` in one transaction, silently ignoring the advertised `transactionWindow` query param. It now applies the same chunking + per-chunk-array response shape as `CreateCollection`. Single-object (non-array) body behaviour is unchanged. Shared chunking primitive `Handler.runChunkedCreate` extracted so the wire contract lives in one place.
  - Wire format on partial-success: HTTP 200 with the per-chunk array, where the failed chunk appears as an `error` element carrying `{code, message, chunkIndex}` instead of `{transactionId, entityIds}`. The first-chunk-fail case (no durable progress) keeps the existing 4xx `application/problem+json` envelope. The `EntityTransactionResponse` schema is relaxed accordingly: `entityIds` is no longer required, and the optional `error` sub-object is declared.
- **`transactionTimeoutMillis` and `waitForConsistencyAfter` documented as Cloud-parity gaps** ([#227](https://github.com/Cyoda-platform/cyoda-go/issues/227)) — these query params on all five entity-mutation endpoints (single create, single update, single update-loopback, collection create, collection update) are advertised by the spec but not honored by the cyoda-go open-source storage plugins. Param descriptions now carry the same vendor-neutral "storage-engine-plugin dependent" caveat established in [#223](https://github.com/Cyoda-platform/cyoda-go/issues/223) for `asyncResult`/`crossoverToAsyncMs`. No Go code change; the params are still parsed-and-ignored.
- `GetOneEntity` now propagates the `transactionId` query parameter ([#150](https://github.com/Cyoda-platform/cyoda-go/issues/150)) — previously silently dropped, returning the latest entity instead of the at-tx snapshot. Bogus `transactionId` returns `ENTITY_NOT_FOUND@404` matching the dictionary contract (parity scenario `12_05` unblocked).
- `GetEntityChangesMetadata` now propagates `pointInTime` ([#152](https://github.com/Cyoda-platform/cyoda-go/issues/152)) — previously dropped; full history truncated to `timeOfChange ≤ pointInTime` as the dictionary requires.
- `messaging.GetMessage` content field — JSON-in-string defect (the original [#21](https://github.com/Cyoda-platform/cyoda-go/issues/21) confirmed defect for messaging).
- `messaging.NewMessage` — dead-code branch in `json.Compact` fallback removed; replaced with explicit invariant-broken 500.
- `audit.GetStateMachineFinishedEvent` — missing `microsTime` field added.
- `search.cancelAsyncSearch` 400 path — uses `WriteError` (proper Content-Type) instead of raw `WriteJSON`.
- `account` stub handlers — error code corrected to `NOT_IMPLEMENTED`.

#### Concurrency + tenant isolation
- **`tx.OpMu` locking discipline rolled out across `plugins/memory`** ([#176](https://github.com/Cyoda-platform/cyoda-go/issues/176)) — `Get`, `GetAll`, `Delete`, `DeleteAll`, `Exists`, `Count` now follow the `tx.OpMu.RLock()` + `defer tx.OpMu.RUnlock()` pattern that PR #153 established for `Save`/`CompareAndSave`. Six new race-conditional regression tests (one per method) green under `-race`.
- **`tx.OpMu` coverage gap on Savepoint/RollbackToSavepoint/Join** closed across the memory and sqlite plugins ([#199](https://github.com/Cyoda-platform/cyoda-go/issues/199) PR-A, PR-C1) and SPI v0.6.1 formalises the contract godoc + `.claude/rules/tx-state-locking.md` (PR-B).
- **Tenant-isolation hardening across plugin transaction-manager surfaces** ([#199](https://github.com/Cyoda-platform/cyoda-go/issues/199) PR-A, PR-C1, PR-C2) — every TM lifecycle method (`Commit`, `Rollback`, `Join`, `Savepoint`, `RollbackToSavepoint`, `ReleaseSavepoint`) on memory, sqlite, and postgres now verifies `uc.Tenant.ID == txState.tenantID` before any state mutation. Postgres was uniquely missing these checks despite RLS — RLS is row-level by design and does not extend to transaction-lifecycle commands.
- **Path-validation cache: single-node invalidation lost on schema-change** ([#174](https://github.com/Cyoda-platform/cyoda-go/issues/174)) — pre-fix the cache subscribed to the cluster broadcaster directly, so single-node deployments (where the broadcaster is nil) never received invalidations. Now wired through `modelcache.SubscribeLocal` so local mutations and gossip events alike reach the cache.
- **Path-validation cache: cross-tenant noisy-neighbor eviction** ([#175](https://github.com/Cyoda-platform/cyoda-go/issues/175)) — pre-fix a single global otter cache (10000 entries) allowed a flooding tenant to S3-FIFO-evict another tenant's entries. Restructured into per-`(tenant, ref)` bucket map of bounded otter caches. Cross-tenant flooding is contained to the attacker's own bucket.
- **Path-validation cache: bucket-map size cap with LRU eviction** ([#218](https://github.com/Cyoda-platform/cyoda-go/issues/218)) — bucket map now caps at 10000 buckets total with LRU eviction. Bounds memory under adversarial workloads (a tenant with model-creation privilege at scale).

#### Internal hygiene
- Pre-existing tag-list test stale entry (`CQL Execution Statistics` removed).
- Root `go.mod` / `go.sum` tidied after dependabot PR #180 bumped sqlite plugin deps without propagating to root (was breaking `Release smoke` and `per-module-hygiene` jobs on `main`).

### Refactored

- **`AppError` 4xx constructors** ([#140](https://github.com/Cyoda-platform/cyoda-go/issues/140)) — `Conflict()` and `RetryableConflict()` removed; replaced by `Operational(status, code, msg).AsRetryable()` chain. Wire shape unchanged at every existing call site; the change is purely API ergonomics.

### Process / Documentation

- ADR 0001 added: chose runtime validation via `kin-openapi` over compile-time strict typing (oapi-codegen strict-server, ogen, goa all evaluated).
- Conformance audit table at `docs/superpowers/audits/2026-04-29-openapi-conformance-audit.md` — one row per operationId, dispositioned with commit SHA. Carried forward as the starting point for future external-spec reconciliation work.
- Per-plugin tx-locking audit docs landed at `docs/audits/2026-05-{memory,sqlite,postgres}-plugin-tx-locking.md`.
- Issue [#194](https://github.com/Cyoda-platform/cyoda-go/issues/194) filed for the 22 stub-implemented IAM/OAuth/OIDC/account endpoints (deferred per the A+C policy of the OpenAPI conformance ADR).
- Issue [#200](https://github.com/Cyoda-platform/cyoda-go/issues/200) filed for SPI sentinel error types (rolled-back / closed / commit-in-progress) — deferred to v0.8.0.

### Versioning policy

`v0.6.x` is no longer maintained. No back-port branch exists. Consumers needing 0.6.x stability should pin to `v0.6.3`. If a concrete need emerges, open an issue and we'll consider branching `release/v0.6.x` from the `v0.6.3` tag.

---

## [0.6.3] — 2026-04-28 and earlier

For releases prior to 0.7.0, see the [Releases page](https://github.com/Cyoda-platform/cyoda-go/releases) and the git history. This is the first release with a maintained CHANGELOG.
