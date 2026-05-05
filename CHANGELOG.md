# Changelog

All notable changes to Cyoda-Go are documented here. The project follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) conventions and [Semantic Versioning](https://semver.org/) — pre-1.0, so minor bumps may include breaking changes (see [README — Versioning](./README.md#versioning)).

## [Unreleased]

### Added

- **`PUT /api/entity/{format}` items now accept an optional `ifMatch` field** ([#228](https://github.com/Cyoda-platform/cyoda-go/issues/228)) — the same cross-request optimistic-concurrency precondition that the single-item PUT endpoints already supported via the `If-Match` header, scoped per item on the bulk-update endpoint. Routing mirrors single `UpdateEntity`'s post-#27 flow: for `COMMIT_BEFORE_DISPATCH` cascades the engine consumes the precondition at the first segment-flush (spec §4.1, before any external dispatch fires); for non-segmenting cascades the handler applies `CompareAndSave` post-engine. Per-item `ENTITY_MODIFIED` conflicts are isolated to a new optional per-chunk `failed[]` array — the chunk still commits its remaining successful items rather than rolling everything back. Other per-item failures (missing entity, validation, non-conflict engine errors) continue to roll the chunk back, matching the pre-#228 #92 contract. When every item in a chunk fails its precondition, the chunk still commits as a zero-write transaction so the surfaced `transactionId` remains meaningful for audit correlation. Wire-format additions on `EntityTransactionResponse`: optional `failed[]` with `{entityId, error: {code, message, itemIndex}}`. `failed` uses JSON `omitempty` — fully-successful chunks keep the pre-#228 shape unchanged. `itemIndex` is per-chunk relative.

## [0.7.0] — 2026-05-04

This release reconciles the OpenAPI spec with the actual server (#21,
breaking — see below), adds API-wide CORS, hardens the supply chain
with cosign keyless signatures on archives + checksums, closes a
suite of locking-discipline and tenant-isolation gaps across the
plugins, and ships a new chart docs surface for Gateway API operators.
15 issues closed in this milestone.

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
