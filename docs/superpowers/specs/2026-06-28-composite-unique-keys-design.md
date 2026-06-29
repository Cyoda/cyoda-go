# Composite Unique Keys — Design

**Status:** Approved design (post independent review). Pending implementation plan.
**Date:** 2026-06-28
**Target:** v0.8.2 — purely **additive / non-breaking** for apps and users (opt-in schema
field, new error codes only on use) and additive to the SPI; ships on `release/v0.8.2`.
**Scope:** cyoda-go (memory, sqlite, postgres) + `cyoda-go-spi`. Commercial (Cassandra)
backend deferred to a separate issue in its own repository.

## 1. Summary

Allow an entity model to declare one or more **composite unique keys** over a set of
**scalar** fields, such that no two *live* entities of that model (within a tenant) may
share the same value-set on a key. Modelled on SQL `UNIQUE` (declared in the schema,
enforced on **create and update**, key fields mutable as long as uniqueness holds) with two
**deliberate deviations** from the SQL standard: (1) the **all-or-nothing null rule** (§2)
rejects partially-filled keys with a 422 rather than admitting them as distinct via
NULL-distinctness; (2) scope is restricted to *live* entities, so soft-delete frees a value.

## 2. Agreed semantics

| Aspect | Decision |
|---|---|
| Constraint type | General SQL-style `UNIQUE`; enforced on create **and** update; key fields mutable. |
| Uniqueness scope | Per `(tenant, model name, model version)`, **live (non-soft-deleted) entities only**. |
| Soft-delete | **Frees** the value-set (the claim is released). |
| Null rule | **All-or-nothing**: all key fields null/absent ⇒ entity **exempt**; *some but not all* present ⇒ **422 validation error**; all present ⇒ enforced. |
| Field types | Scalar leaf JSONPaths only (string / number / integer / bool). Array / object / wildcard paths rejected at declaration. |
| Cardinality | Multiple independent unique keys per model. |
| Declaration window | Editable only while the model is **unlocked**. |
| Backfill | **None, ever** — see §3. |

### 2.1 The no-backfill invariant (verified)

A model can only be unlocked when it has **zero live entities**. This is enforced today at
a single service chokepoint shared by HTTP and gRPC (`internal/domain/model/service.go`
~`:285-300`: `Count(...) > 0 → 409 MODEL_HAS_ENTITIES`), and "live" is exact because
`Count` excludes soft-deleted rows in every engine. Therefore a newly declared/locked
unique key holds from the very first insert, and **no scan/validation of pre-existing data
is ever required**. `DeleteModel` carries the same guard.

The implementation plan must include a guard test that *fails* if this invariant ever
weakens (it is load-bearing for the entire feature).

### 2.2 No resurrect

There is no operation that resurrects a soft-deleted entity by id (create always mints a
fresh UUID; update/patch 404 on tombstoned entities). The design therefore makes **no**
"resurrect re-claims the value" promise. If such an operation is added later, the claim
re-insert will correctly collide via the unique primitive, but the
fail-resurrect-vs-restore decision is explicitly **out of scope** here.

## 3. Architecture (Approach A — engine-internal enforcement)

### 3.1 Claims computed in the store; keys resolved by the handler, threaded via context

> **Revision history (two supersessions, kept visible).** (a) An early draft computed claims
> in the *service* and attached them to the entity before `Save`. That fails because the
> workflow engine saves the **primary entity internally** during COMMIT_BEFORE_DISPATCH
> segmenting (`internal/domain/workflow/engine_processors.go` ~`:311,:345`) *after* a
> processor may have rewritten `entity.Data` (~`:294-296`) — and **processors can mutate a
> unique-key field** — so service-attached claims would be stale on those saves. Enforcement
> therefore lives in the **store**, computing from the **live** `entity.Data` on every save.
> (b) A second draft carried the key *definitions* on a transient `spi.Entity.UniqueKeys`
> field. That is fragile: any save path that forgets to set it computes **zero claims and
> silently enforces nothing** — many drop sites. (c) A third draft had the *entity store*
> look the keys up "via the existing model cache." That is impossible: the model cache
> (`internal/cluster/modelcache`) is a main-module decorator a plugin store cannot reach
> (circular module dependency). **This revision (b):** the **handler** (main module, where the
> coherent cache *is* reachable) resolves the keys and threads them to the store via the
> request **context**; the store reads them from context and computes claims from the live
> `entity.Data`.

Mechanics:

- The model's **unique-key definitions** live on a new additive field
  **`spi.ModelDescriptor.UniqueKeys []spi.UniqueKey`** — durable, **outside** the foldable
  schema-node tree (see §4 / C1: storing them inside `Schema` bytes is destroyed by
  `ExtendSchema`/`Merge`/`Apply`). They ride inside each model store's existing serialized
  descriptor blob, so **no model-table migration** is needed (S1).
- **Cross-cluster coherence is inherited, not invented.** Keys are part of the descriptor,
  which already has a proven coherence story (`internal/cluster/modelcache/cache.go`):
  UNLOCKED models bypass the cache (read fresh from disk while keys are editable, `:119-120`);
  Lock/Unlock/Save/Delete `invalidate` → evict locally **and gossip-broadcast**
  `topicModelInvalidate` (`:160-188,:238-245`) → every node evicts and reloads the descriptor
  (keys included) from disk. The lock that lets a model accept saves *is* the invalidation
  event. **No new cache, no new invalidation.**
- **Key resolution + threading (the handler already has the descriptor).** Every entity write
  path loads `desc` for validation via the cached `ModelStore`, so it already holds
  `desc.UniqueKeys` at **zero extra cost**. The handler puts the keys on the request context
  (`spi.WithUniqueKeys(ctx, desc.UniqueKeys)`) right after that load. The engine's internal
  CBD saves inherit them automatically — `engine.Execute`/CBD derive their tx context via
  `context.WithoutCancel(ctx)`, which **preserves values** — so post-processor internal saves
  read the same keys. No transient `spi.Entity` field; no per-save model read; the
  engine-internal saves that broke draft (a) are covered by context propagation, not a
  re-attached field.
- **The store enforces.** Inside `Save`, the store reads `spi.UniqueKeysFromContext(ctx)`,
  calls **`spi.ComputeClaims(keys, entity.Data) ([]UniqueClaim, error)`**, and enforces the
  result (side table / map). `ComputeClaims` lives in **`cyoda-go-spi`** so all three engine
  submodules call one canonicalization path. A claim is emitted **only** for a *fully-present*
  key; all-null keys produce none (⇒ exempt); a *partially-filled* key returns
  `ErrPartialUniqueKey`.
- **Field-path resolution (S3 — corrected).** A field is a dotted leaf path identical in form
  to `schema.FieldDescriptor.Path` (built by `collectFields` joining segments with `.`). The
  helper resolves a path by splitting on `.` exactly as the schema *constructs* it — so it
  inherits cyoda's existing project-wide assumption that field identifiers contain no `.`
  (the same (un)ambiguity `FieldDescriptor.Path` already has; **no new ambiguity**). It does
  **not** issue a raw gjson query (gjson's `* ? # @ [ ]` metacharacters and array semantics
  are the real reason to avoid it). Decode with `json.Number` preserved.
- **Partial-key pre-check stays in the service** on the *input* doc (it has `desc.UniqueKeys`):
  `ComputeClaims(desc.UniqueKeys, inputDoc)` before `engine.Execute`; `ErrPartialUniqueKey` →
  422 `INVALID_UNIQUE_KEY` — a fast, clean 422 for client partial input even on a segmenting
  workflow. A partial key *produced by a processor* surfaces from the store and is routed to a
  sanitized workflow 5xx (§3.5 C2).

**Why (b):** uniform enforcement at the store (sees the live post-processor doc on every
path), keys resolved where the coherent cache is reachable (the handler) and propagated by
context (so the engine-internal saves are covered for free), with **zero extra reads** and
**no new coherence machinery**. Residual silent-skip surface: only a **non-transactional**
`store.Save` (no context keys) — but the entity service always writes inside a transaction,
so non-tx saves are not user-write paths; a guard test asserts this. Cost (accepted): the
additive `spi.ModelDescriptor.UniqueKeys` field + `spi.WithUniqueKeys`/`UniqueKeysFromContext`
context helpers + each model store wiring the field through its existing descriptor blob
(no migration). The SPI also gains `UniqueKey`/`UniqueClaim`, `ComputeClaims`,
`ErrUniqueViolation`/`ErrPartialUniqueKey`, and the capability interface (§3.6). There is
**no** new `spi.Entity` field.

### 3.2 Signature canonicalization (pinned)

`spi.Entity.Data` is raw `[]byte`; the doc never round-trips through `float64` at storage,
so precision can be preserved. Rules:

- **Numbers (S4 — DoS-safe, no new dependency).** **Bound the input first, then use stdlib
  `math/big`.** The OOM risk is from *unbounded* input (`1e1000000000` materialized by
  `big.Rat` → multi-GB → Gate-3 DoS), not from `big` itself: at canonicalization, **reject**
  any numeric literal exceeding a fixed coefficient-digit / exponent-magnitude bound — checked
  on the raw `json.Number` string **before** any `big` materialization. The rejection is a
  **`ComputeClaims` value error in the `ErrPartialUniqueKey` family** → mapped to **422
  `INVALID_UNIQUE_KEY`** by *both* `common.Internal` *and* `classifyWorkflowError` (F3 — it must
  not reach the `WORKFLOW_FAILED` catch-all, else a processor emitting an over-large value
  re-opens the C2 raw-text leak). On the *bounded* remainder, stdlib `math/big` cannot blow up, so no
  external decimal lib is added to the deliberately-minimal SPI contract module (only
  `google/uuid` today). Normalize to one canonical form so `1`, `1.0`, `1e0`, `1E0`, `-0`
  collide and **full int64+ precision is preserved**; cover integers > 2^53, leading zeros,
  and `e`/`E` explicitly. Do **not** reuse `cyoda_try_float8` (lossy float8 search helper).
  *(This reconciles the earlier spec/plan split — the plan must use this bound-then-stdlib
  approach, not hand-rolled string normalization and not a new dep.)*
- **Strings:** **byte-exact** (decided). Case-sensitive, **no** Unicode folding (NFC ≠ NFD),
  **no** whitespace trimming — the bytes the app sent are what's compared (SQL `UNIQUE` under
  a binary collation). Apps wanting looser matching (e.g. case-insensitive emails) normalize
  before writing. This is a tested concern (§7 unit row).
- **Booleans:** `true` / `false`.
- Each value is type-tagged so `"1"` (string) and `1` (number) never collide.

### 3.3 Engine claim storage — invariant

> A claim row exists **iff** the entity is currently live and has a fully-present key.

**This invariant has multiple maintenance points, not one** — every write *and* every
removal path must keep it true. There is no hard-delete of entities, but soft-delete and
writes happen on several distinct code paths the plan must enumerate and individually test:

- transactional commit (single-entity create/update via the tx tombstone/flush loop),
- the **non-transactional** `Save` and `Delete` paths (memory `entity_store.go` ~`:225-245`,
  ~`:480-504`),
- **`DeleteAll`** — bulk soft-delete of every entity of a model (memory `entity_store.go`
  ~`:507-581`; postgres bulk soft-delete behind `DeleteAllEntities`,
  `internal/domain/entity/service.go` ~`:677`) — **must release every affected claim**,
- **`CreateEntityCollection`** — per-item create within one tx (`service.go:882-966`); see
  §3.7 for intra-batch uniqueness. (`spi.EntityStore.SaveAll` has no production callers.)

Missing any of these orphans claim rows, which later resurface as **spurious 409s** when a
freed value-set is re-used. Coverage (§7) asserts "zero claim rows after `DeleteAll`" and
"after model delete".

**postgres / sqlite (Option 1 — chosen):** a side table
`unique_claims(tenant, model, version, key_id, signature, entity_id)` with a **plain**
`UNIQUE(tenant, model, version, key_id, signature)` index, maintained **in the same
transaction** as the entity write. On soft-delete the claim row is `DELETE`d (no `deleted`
flag, no partial index).

- *Chosen over Option 2 (keep row + `deleted` flag + partial index)* because: (a) the heap
  stays bounded by the **live** set, whereas Option 2 accumulates a row for every
  soft-deleted entity forever; (b) the "row ⟺ live claim" invariant is trivially auditable;
  correctness and per-write cost are otherwise equal.
- *Chosen over a partial expression index on `entities.doc`* because a precision-preserving
  Go-computed signature must be **stored** to be compared; an expression index would force
  runtime `CREATE INDEX` DDL (dynamic per-model fields) and lossy in-SQL number extraction.
  The real trade is *maintenance-invariant-risk (side table)* vs
  *runtime-DDL + lossy canonicalization (index)*; storing the Go signature tips it to the
  side table.

Postgres: the side table carries `tenant_id` and an **RLS policy** mirroring the existing
tables. Both the entity upsert and the claim mutation run inside the same `pgx.Tx`.

**memory:** a `signature → entityId` map. It must be:
- guarded by **`factory.entityMu`** held continuously across the commit flush (not the
  short-lived `m.mu`), and
- evaluated against **current committed state, ignoring snapshot isolation** (no
  `submitTime.After(SnapshotTime)` filter) — otherwise a committer whose snapshot predates
  a rival's commit would skip the conflict.
- Release-on-soft-delete happens under `entityMu` on **every** delete path (#10): the commit
  flush has a **Buffer loop** (`txmanager.go:204`) *and* a separate **Deletes loop** (`:245`) —
  in-tx soft-delete and `DeleteAll` land in `tx.Deletes`, so claim release must hook the
  **Deletes loop**, not only the buffer/tombstone loop — plus the non-tx `Delete` and
  `DeleteAll` paths.

### 3.4 Update that moves a key value

Claims are computed from the **post-merge full doc**, never a patch fragment: `PATCH`
merges the fragment into the stored doc, so the helper must run on the merged result (a
patch that nulls/removes one key field then triggers the all-or-nothing rule → 422
`INVALID_UNIQUE_KEY`, a case the coverage matrix must include).

Within the same (version-checked) transaction as the write:
1. **Delete-first** the entity's prior claim rows for the affected key(s), then insert the
   new claim(s). Delete-first avoids self-collision when a value is unchanged or reused.
2. The no-op case (signature unchanged) must be a no-op, not a delete+reinsert that could
   transiently free the value.
3. A `CompareAndSave` / read-set validation failure rolls back the claim mutation with the
   entity write (same tx).

Engines **self-release by entityId** on soft-delete (they do not need the service to
recompute claims for release).

### 3.5 Error classification (critical)

A unique-index conflict must surface as a **non-retryable 409 `UNIQUE_VIOLATION`**,
distinct from the existing *retryable* `CONFLICT`.

Concrete mechanism (so an implementer can't accidentally make it retryable or a 500):

- **Postgres:** `classifyError` (`plugins/postgres/transaction_manager.go` ~`:446`) today
  maps only `40001`/`40P01` → `spi.ErrConflict`; `23505` is unhandled (→ 500) and
  `ConstraintName` is used nowhere. Extend it to wrap `23505` **iff
  `pgconn.PgError.ConstraintName == <the claim unique constraint>`** into
  `spi.ErrUniqueViolation`. A `23505` on **any other** constraint must fall through
  unchanged (→ 500). The claim INSERT already flows through the classifying querier
  (`plugins/postgres/classifying_querier.go` ~`:35`), so no new plumbing is needed.
  *(A duplicate INSERT under `RepeatableRead` surfaces as `23505`, not `40001`; the
  rare-race `40001` path is retried and deterministically converges to the `23505` →
  `UNIQUE_VIOLATION` outcome, so constraint-name detection is the real fix, not a
  `40001` reclassification.)*
- **sqlite / memory:** map their unique-conflict signal to the same sentinel. Note these
  buffer writes and **surface the violation at `Commit` (flush), not `Save`** (memory flush
  `txmanager.go:204` under `entityMu`; sqlite `flushToSQLite`). That is fine — the commit sites
  route through `common.Internal` (`service.go:297,966,…`) and the engine wraps segment-commit
  in `ErrCommitBeforeDispatchInfra` → `common.Internal`, both of which get the sentinel
  branches. Coverage must include a **memory test where the violation surfaces at commit**,
  including a segmenting cascade (not only the postgres INSERT-time path).
- **Service mapping:** add branches to `common.Internal` (`internal/common/errors.go`
  ~`:96`), each more specific than the existing `ErrConflict` branch and placed before it:
  `spi.ErrUniqueViolation` → `Operational(409, UNIQUE_VIOLATION)` **without** `.AsRetryable()`;
  `spi.ErrPartialUniqueKey` → `Operational(422, INVALID_UNIQUE_KEY)`. Both non-retryable.
  **`spi.ErrPartialUniqueKey` is the umbrella sentinel for *all* `ComputeClaims` value-invalid
  errors** — partial key, **numeric over-bound (F3/S4)**, and **non-scalar at a key path
  (#5)** — so they all `errors.Is(_, ErrPartialUniqueKey)` and a single detection (here and in
  `classifyWorkflowError`) routes every one to 422 with a fixed sanitized message.
- **Workflow-path routing (C2 — required, not just `common.Internal`).** A store violation
  during `engine.Execute` does **not** always reach `common.Internal`. Three engine-internal
  save sites differ (`internal/domain/workflow/engine_processors.go`):
  - the plain-`Save` CBD segment (~`:345`) is wrapped with `ErrCommitBeforeDispatchInfra`,
    routed by `classifyWorkflowError` → `common.Internal`, and `errors.Is` chain-walks to the
    new sentinel — **works** once the `common.Internal` branches exist;
  - the **universal CBD apply-result `CompareAndSave`** (~`:311`, returns unwrapped at `:313`)
    runs for *every* CBD cascade (the If-Match-specific unwrapped site is `:340-343`). Both
    bubble **unwrapped** to `classifyWorkflowError` (`internal/domain/entity/service.go`
    ~`:1534`) → catch-all → **400 `WORKFLOW_FAILED` with raw sentinel text in the body** (wrong
    status *and* a Gate-3 output-sanitization leak). **Fix:** `classifyWorkflowError` must
    detect `spi.ErrUniqueViolation` (→ 409) and `spi.ErrPartialUniqueKey` (→ 422) **before**
    its catch-all. Coverage must include a **plain CBD-segmenting** violation, not only the
    If-Match one (Co-1).
  - the **`ASYNC_NEW_TX` path** (S5 — corrected to match the code): `executeAsyncNewTx`
    (`engine_processors.go:174-208`) **never calls `EntityStore.Save`** — by design it
    **discards** the processor's returned entity mutations ("side-effects only"). So the
    cascade **cannot persist a duplicate** through this path *at all* — not because the store
    rejects it, but because the mutation is dropped before any save. Any entity a side-effect
    processor persists is a **separate inbound API call**, independently store-enforced like any
    other write. **v1: no special handling, no reject-config.** Coverage: assert that an
    `ASYNC_NEW_TX` processor "writing" a duplicate key value leaves **no duplicate persisted**
    (because the mutation is discarded), and that the originating request is unaffected.

`spi.ErrUniqueViolation` and `spi.ErrPartialUniqueKey` are **new additive sentinels** in
`cyoda-go-spi`. The 409 body names the violated `keyId` only — **never** the incumbent
entity's id (need-to-know; avoids leaking another entity's existence even within a tenant).

### 3.6 Capability gate

**Hard requirement:** it must be impossible to configure / persist a composite unique key
on a storage engine that does not support it. The commercial backend will not support it
for some time; any attempt to declare a key on an unsupported backend MUST be rejected with
**422 `COMPOSITE_KEY_UNSUPPORTED`** — *never* silently accepted-and-unenforced.

There is exactly **one** path that introduces a key definition: the new `PUT
…/unique-keys` sub-resource (§4). `ImportModel` cannot carry keys (it hard-rejects any
converter ≠ `SAMPLE_DATA`, `internal/domain/model/service.go` ~`:93-95`, and infers the
schema from sample data), and no other raw-schema import exists. So the gate lives on that
one endpoint — no need to also guard import/lock (corrects an earlier over-broad framing).

The advertisement *mechanism* is an implementation detail (the engineer's call): presence
of the optional interface is sufficient signal; the explicit bool below is retained only so
a backend could implement-but-temporarily-disable. What matters is the hard requirement
above, enforced at config time.

Support is advertised by an **additive optional interface**, e.g.:

```go
// Optional. Absence ⇒ composite unique keys unsupported.
type CompositeUniqueKeyCapable interface {
    SupportsCompositeUniqueKeys() bool
}
```

- Checked at **declaration time** (the `unique-keys` PUT, §4), never in the write hot path.
- memory / sqlite / postgres support it; the **commercial backend does not** ⇒ unsupported,
  until its own issue lands.
- Purely additive — it does **not** add a method to the existing `StoreFactory` interface
  (which would be a breaking change for out-of-tree consumers, per `MAINTAINING.md`).
- **Out of scope:** a process-wide backend swap (supported → unsupported) under a model that
  already declares keys would strand enforcement. The backend is process-wide, so this
  requires a deliberate data migration; explicitly not handled here.

### 3.7 Collection create — intra-batch uniqueness

> **Corrected (rev-4 #3):** `spi.EntityStore.SaveAll` has **no production callers** — the
> mechanism named in earlier drafts is dead code. Collection-create is
> `CreateEntityCollection` (`service.go:882-966`) looping **per-item `engine.Execute` + `Save`
> within one transaction**. The intra-batch concern below is real; it just lives in that loop.

Collection create must enforce uniqueness **within the batch** as well as against committed
state: two items in one `CreateEntityCollection` sharing a value-set must collide. SQL gets
this for free (the second claim INSERT raises `23505` in the same tx). For **memory** the
batch shares one `tx.Buffer`, and the flush loop iterates a
**map** (`plugins/memory/txmanager.go` ~`:204`) — *nondeterministic order*, so "the second
in-batch claim loses" has no defined winner (S3). The flush must therefore process buffered
entities in a **deterministic order** (e.g. sorted by entityId) so the winner is stable and
testable. **Sorted order is necessary but not sufficient (Co-3):** correctness *also* requires
the claim check to run under `entityMu` against **current committed state** (ignoring
`SnapshotTime`, §3.3) and the same maintenance wired into all three non-tx paths — order alone
only fixes the intra-batch winner, not the cross-tx race or the non-tx paths. Non-tx `SaveAll`
is N independent `entityMu` cycles with no batch-wide section —
postgres's unique index still catches it, but memory's per-cycle check is the enforcement
point; assert consistency (one winner), not a specific interleave. Coverage matrix includes
an intra-batch-duplicate row.

### 3.8 Key-staleness — documented bounded limitation (multi-node)

> **Scope:** this is a **postgres / commercial multi-node** concern only — memory and sqlite
> are single-node (in-process / single-process flock), so their cache invalidation is
> synchronous and coherent.

Keys inherit the descriptor's existing cross-cluster coherence (UNLOCKED bypasses the cache;
Lock/Unlock/Save/Delete `invalidate` → gossip `topicModelInvalidate` → evict; TTL backstop —
`cache.go:119,160-188,238-245`). They inherit its *eviction* but not full *self-healing*:
schema is additive-only, but a unique key can be **non-monotonically replaced** via a
destructive `unlock (requires zero live entities, §2.1) → change keys → relock` on the same
`(tenant, model, version)`.

**The residual risk (accepted + documented for v1).** If a postgres node misses **both** the
unlock and relock gossip messages and is still within its cache-lease TTL, it can briefly
enforce the **old** key-set against new writes; data it admits in that window can violate the
**new** key and persists (its claim rows are under the old key-ids). The window is narrow and
self-heals at the TTL, and it is gated behind a **destructive model teardown** (all entities
deleted to change a key). This is the same gossip+TTL staleness posture the system already
accepts for model state/schema (`cache.go:1-7` calls these "performance/hygiene layers").

We deliberately do **not** ring-fence it with per-save model reads (a permanent hot-path cost
to cover a transient teardown-only risk) **nor** a rev-token + per-write rev-check + store-side
retry (that contradicts §3.1's zero-extra-read property, and a *post-commit* retry is unsafe
across a CBD cascade that already durably committed a segment — rev-4 review F1/F2). Instead:

- **Document** this limitation in operator docs: after changing a unique key on a multi-node
  deployment, allow the cluster to settle (≤ cache TTL) before resuming writes.
- The proper fix is a **separate, general cluster enhancement — *acknowledged* model-cache
  invalidation** (the current `ClusterBroadcaster.Broadcast` is fire-and-forget, `cluster.go:16`;
  an ack'd clear over the node `registry` + `dispatch` peer channel would let a relock confirm
  every online node has evicted before completing, closing this window and tightening schema
  staleness too). Tracked as its own issue; **out of scope here** (building cluster
  coordination into this feature would balloon it beyond reviewability — Gate 6).

## 4. Unique-key definition & declaration surface

```
UniqueKey {
  id:     string        // stable identifier, unique within the model
  fields: [ string ]    // ordered scalar leaf JSONPaths
}
```

**Declaration surface (corrected — supersedes any earlier "WorkflowConfigurationDto"
reference).** Models in cyoda-go are *not* defined via an explicit schema DTO — they are
imported from `SAMPLE_DATA` (schema inferred) and exported via `ExportModel`. The model
write surface is a set of sub-resources mirroring `/lock`, `/unlock`,
`/changeLevel/{changeLevel}`. Unique keys are therefore declared through a **new dedicated
model sub-resource**, not the workflow surface:

- **HTTP:** `PUT /model/{entityName}/{modelVersion}/unique-keys` with body
  `{ "uniqueKeys": [ { "id": "...", "fields": ["$.a","$.b"] }, ... ]}` — idempotent
  *set-the-whole-list*. Allowed only while the model is **UNLOCKED** (else 409
  `MODEL_ALREADY_LOCKED`, matching the existing transition guards). Validated **immediately**
  on this call (not deferred to lock).
- **gRPC:** a new `EntityModelManage` CloudEvents event type alongside the existing
  import/export/transition/delete handlers (`internal/grpc/model.go`).
- **Persistence (C1 — corrected, supersedes an earlier "store inside `Schema` bytes"
  draft).** The key list is stored on a **new additive field
  `spi.ModelDescriptor.UniqueKeys []spi.UniqueKey`**, persisted by each engine's model store
  as its own column/blob — **NOT** inside the `Schema` node bytes. Storing it in `Schema`
  is destroyed by the schema-fold: the codec emits a bare `wireNode`, and the first
  schema-extending entity write after lock runs `ExtendSchema → Unmarshal → Apply → Marshal`
  (`internal/domain/entity/handler.go` ~`:135`, `app/app.go` ~`:852`) which round-trips
  through the node tree and **drops** any wrapper; `ImportModel`'s `Merge` drops it too.
  A dedicated descriptor field sits outside the foldable tree and survives. **No migration
  (S1):** both SQL model stores already persist the *entire* descriptor as one opaque JSON
  blob (postgres `model_store.go` `doc JSONB`; sqlite identical), so `UniqueKeys` rides inside
  it with **zero DDL** — only the private `modelDoc` struct + `Save`/`unmarshalModelDoc` need
  field wiring, plus a deep-copy arm in memory's `cloneDescriptor` so the slice isn't aliased.
  *(The `unique_claims` **side table** in §3.3 is a separate thing — it IS a new table/migration;
  don't conflate the enforcement index with the descriptor field.)*
- **Round-trip preservation (S2 — required).** `ImportModel` rebuilds a fresh descriptor
  copying only `ChangeLevel` forward (`internal/domain/model/service.go` ~`:148-156`), so a
  re-import would **silently drop** `UniqueKeys`. It must **preserve** `UniqueKeys` across
  re-import (like `ChangeLevel`) and **re-validate** them against the merged schema (reject the
  import if a re-imported schema removes a field a key references). **Export ≠ import path
  (#6):** export is `ExportModel` (`internal/domain/model/service.go` ~`:174-199`) over the
  schema node via an `Exporter`; import is `SAMPLE_DATA`-only and never re-ingests an exported
  doc. So preservation is **copy-from-existing-descriptor on re-import** (`UniqueKeys` carried
  forward), and `ExportModel` additionally **includes `UniqueKeys` for external visibility /
  migrate tooling** — these are two separate obligations, not a symmetric round-trip.

Validation (on the `unique-keys` PUT — capability first, then definition; both 422):
- backend **capability** present (else 422 `COMPOSITE_KEY_UNSUPPORTED`);
- every field is a known **scalar leaf** in the (inferred) model schema — reject
  array / object / wildcard / unknown paths;
- `fields` non-empty, no duplicate field within a key; `id` unique within the model
  → else 422 `INVALID_UNIQUE_KEY_DEFINITION`.

**Post-declaration type-widening guard (rev-4 #5).** `schema.Merge`/`mergeKind` only ever
*widen* (Leaf+Object→Object, keeping children; `schema/merge.go:43-57`), and `ExtendSchema`
runs **after** lock — so a field that was a valid scalar key at declaration can later be
promoted to object/array by a schema-extending write or re-import, *after* the scalar check
passed. Two defenses, both required: (1) the schema-extend / re-import path must **reject
widening a field referenced by a unique key** (preserve the scalar invariant for key fields);
(2) `spi.ComputeClaims` must **defensively reject a non-scalar value** at a key path
(→ a value-invalid 4xx), never silently mis-canonicalize an object/array.

Because `UniqueKeys` now appears in the model **export DTO** (S2), this touches the
model export surface ⇒ follow `docs/workflow-schema-versioning.md` bump rules (Gate 4). *(It
does **not** touch the schema-codec / `Schema`-bytes format — keys are no longer stored there,
N-1.)*

## 5. Data flow

**Create:** the handler loads `desc` (carrying `UniqueKeys`), pre-checks the input doc
(partial key ⇒ 422 `INVALID_UNIQUE_KEY`), and puts the keys on the context
(`spi.WithUniqueKeys`); the store, inside `Save`, reads `spi.UniqueKeysFromContext(ctx)` and
computes claims from the **live** `entity.Data` via `spi.ComputeClaims`, writing entity +
claim rows in one tx → unique primitive collision ⇒ `ErrUniqueViolation` ⇒ 409
`UNIQUE_VIOLATION`. *This closes the
First-Committer-Wins gap*: two concurrent creates of distinct entity ids sharing a value-set
have disjoint write-sets, so the existing FCW check cannot catch them — the native unique
index (sql) / `entityMu`-guarded map (memory) serializes them so exactly one commits.

**Update (moving a key):** recompute claims → delete-first old claim(s), insert new → 409 on
collision; 412 `ENTITY_MODIFIED` still applies via `CompareAndSave`.

**Soft-delete:** the store removes the entity's claim rows in the same tx → value freed.

## 6. Error / status-code table

| Operation (HTTP & gRPC) | Code | HTTP | Retryable | Trigger |
|---|---|---|---|---|
| Create entity | `INVALID_UNIQUE_KEY` | 422 | no | partially-filled composite key |
| Create entity | `UNIQUE_VIOLATION` | 409 | no | fully-present key collides with another live entity |
| Update / patch entity | `INVALID_UNIQUE_KEY` | 422 | no | partially-filled composite key |
| Update / patch entity | `UNIQUE_VIOLATION` | 409 | no | moved/created key collides with another live entity |
| Update / patch entity | `ENTITY_MODIFIED` | 412 | no | existing optimistic-lock failure (unchanged) |
| Set unique keys (`PUT …/unique-keys`) | `COMPOSITE_KEY_UNSUPPORTED` | 422 | no | unique key declared on a backend without capability |
| Set unique keys (`PUT …/unique-keys`) | `MODEL_ALREADY_LOCKED` | 409 | no | declared on a LOCKED model (existing code reused) |
| Set unique keys (`PUT …/unique-keys`) | `INVALID_UNIQUE_KEY_DEFINITION` | 422 | no | key references non-scalar / array / unknown path, dup id, empty fields (reuse an existing model-validation code instead if one fits cleanly) |
| Create entity / commit | `CONFLICT` | 409 | yes | existing transaction conflict (unchanged) |

Each **new** code (`UNIQUE_VIOLATION`, `INVALID_UNIQUE_KEY`, `COMPOSITE_KEY_UNSUPPORTED`,
and `INVALID_UNIQUE_KEY_DEFINITION` if not reusing an existing code) requires:
- a constant in `internal/common/error_codes.go`,
- a mapping in `internal/common/errors.go` producing the correct (non-retryable) status,
- a `cmd/cyoda/help/content/errors/<CODE>.md` help topic (`TestErrCode_Parity` enforces a
  strict bijection).

## 7. Coverage matrix (scenario × layer)

| Scenario | unit | running-backend e2e (postgres) | cross-backend parity | gRPC |
|---|---|---|---|---|
| Signature canonicalization (numeric `1`/`1.0`/`1e0`, large int, type-tag, string normalization) | ✓ | | | |
| All-or-nothing null rule (all-null exempt; partial ⇒ 422) | ✓ | ✓ | ✓ | ✓ |
| Create duplicate ⇒ 409 `UNIQUE_VIOLATION` | | ✓ | ✓ | ✓ |
| Update moves key ⇒ 409 on collision; success when free | | ✓ | ✓ | ✓ |
| PATCH that nulls/removes a key field ⇒ 422 (all-or-nothing on merged doc) | | ✓ | ✓ | ✓ |
| Soft-delete frees value (re-create with same key succeeds) | | ✓ | ✓ | |
| `DeleteAll` releases all claims (zero claim rows after; re-create succeeds) | | ✓ | ✓ | |
| `CreateEntityCollection` intra-batch duplicate ⇒ 409 (no torn write); per-item keys for a mixed-model batch | | ✓ | ✓ | |
| Multiple independent keys per model | | ✓ | ✓ | |
| Definition validation (non-scalar/array/unknown/dup) ⇒ 422 | ✓ | ✓ | | ✓ |
| `COMPOSITE_KEY_UNSUPPORTED` — declare on an unsupported backend ⇒ 422 | ✓ (fake unsupported factory) | | | ✓ |
| Set keys on LOCKED model ⇒ 409 `MODEL_ALREADY_LOCKED` | | ✓ | | ✓ |
| Schema-extend after lock does NOT drop keys (C1 regression guard) | | ✓ | ✓ | |
| Schema-extend/re-import widening a key field ⇒ rejected; non-scalar at key path ⇒ 4xx (#5) | ✓ | ✓ | | |
| Processor rewrites a key field ⇒ final value enforced (create dup of rewritten value ⇒ 409) | | ✓ | | |
| CBD-segmenting violation (plain **and** If-Match) ⇒ 409 (not 400/WORKFLOW_FAILED, no raw text) (C2/Co-1) | | ✓ | | ✓ |
| `ASYNC_NEW_TX` processor writes duplicate key ⇒ no duplicate persisted (constraint holds; 2xx + WARN) (S5) | | ✓ | | |
| Re-import preserves `UniqueKeys` (copy-from-existing-descriptor); re-import dropping a key field is rejected (S2/#6) | | ✓ | | ✓ |
| Model export includes `UniqueKeys` (external visibility) (S2/#6) | | ✓ | | ✓ |
| Concurrency: two concurrent creates same key ⇒ exactly one wins, other 409, no torn write | | ✓ (isolated, single-backend) | **never in shared parity** | |

Parity scenarios registered in `e2e/parity/registry.go`; positive uniqueness scenarios are
**capability-gated** (skipped on backends that report unsupported). The
`COMPOSITE_KEY_UNSUPPORTED` rejection **cannot be exercised in the in-repo parity suite** —
all three in-repo backends support the feature and the commercial one is out of repo — so it
is covered by a **unit test against a fake `StoreFactory` that does not implement
`CompositeUniqueKeyCapable`** (S4), plus the commercial backend asserting it on its next dep
update. Concurrency tests are isolated single-backend e2e asserting consistency, per
`.claude/rules/test-coverage.md` and the known memory-backend parity-destabilization.
`ASYNC_NEW_TX` (§3.5 S5): the store enforces on the async save too, so the test asserts **no
duplicate is persisted** (constraint holds) while the request returns 2xx + WARN — no
reject-config.

## 8. Cross-cutting / dependencies

- **SPI surface (all additive):** `spi.UniqueKey`/`spi.UniqueClaim` types, the new
  **`spi.ModelDescriptor.UniqueKeys []UniqueKey`** field, the **`spi.WithUniqueKeys` /
  `spi.UniqueKeysFromContext`** context helpers, `spi.ComputeClaims`, `spi.ErrUniqueViolation`
  + `spi.ErrPartialUniqueKey`, and `CompositeUniqueKeyCapable`. There is **no** `spi.Entity`
  field and **no rev token** (§3.8). `ComputeClaims` does JSON extraction by segment (S3) +
  bound-then-stdlib-`math/big` canonicalization (S4) — **no new third-party dependency** in
  the minimal SPI contract module.
- **Per-engine model-store persistence (NO migration — S1):** each model store reads/writes
  `ModelDescriptor.UniqueKeys` **inside its existing serialized descriptor blob** (postgres
  `doc JSONB`, sqlite identical). **Add the field to the private `modelDoc` struct, not just
  `Save`/`unmarshal` (#9):** sqlite's lifecycle RMW ops (`Lock`, `SetChangeLevel`,
  `updateStateField`) round-trip through `modelDoc` and would **strip** any field absent from
  the struct. Memory needs a deep-copy arm in `cloneDescriptor`. The descriptor is already one
  opaque blob, so **no DDL**. (Only the `unique_claims` enforcement side table is a new
  table/migration.)
- **Context plumbing (C1 of rev-4 review — per-item for batches).** The handler sets
  `spi.WithUniqueKeys(ctx, desc.UniqueKeys)` after its existing descriptor load; the engine's
  CBD saves inherit it via `context.WithoutCancel`. **Single writes** (`CreateEntity`,
  `UpdateEntity`, `PatchEntity`→`updateEntityCore`) set it once. **Batch writes** mix models
  with **different key sets** across one batch, so each item's keys must be set on the
  **per-item context immediately before that item's `engine.Execute`/save** (setting
  batch-wide silently enforces item-0's keys for all):
  - `UpdateEntityCollection` (`service.go:1360→1463`) loads `desc` *inside* the execution loop
    (`:1360`), so per-item `WithUniqueKeys(currentCtx, desc.UniqueKeys)` is direct.
  - `CreateEntityCollection` (`service.go:838→956`) loads `desc` in a **separate earlier
    validation loop and discards it** — `parsedItem` keeps only `{ref, payloadBytes}` (`:864`).
    So the plan must **carry `desc.UniqueKeys` in `parsedItem`** (the validation loop already
    loaded it — no extra read) and set it on `currentCtx` before each item's `Execute` (F4).
  - A guard test asserts a non-transactional `store.Save` (the only path without context keys)
    is never a user-write path.
- **SPI coordinated release** (`MAINTAINING.md`): the above land in `cyoda-go-spi` on `main`;
  cyoda-go pseudo-version-pins during the milestone; SPI tag + pin-bump as the final step.
  **No `replace` directive.**
- **Gate 4 docs:** OpenAPI / help topics for the new `unique-keys` endpoint and the
  `UniqueKeys` field on model export; help topics for the new error codes; `COMPATIBILITY.md`
  on the SPI bump; `docs/workflow-schema-versioning.md` bump per the export-surface change.
- **`ModelStore.Delete`** requires zero live entities (the same guard as unlock, §2.1), and
  soft-deleted entities hold no claims (released on soft-delete, §3.3) — so the claim set for
  a `(tenant, model, version)` is **already empty** at delete time. No orphan is possible
  unless the §3.3 release invariant is itself broken, which the §3.3 path tests already
  cover. An `ON DELETE CASCADE` FK / explicit claim purge on model delete is therefore at
  most a **cheap belt-and-suspenders sanity cleanup, not a correctness requirement** —
  include it only if it falls out for free (e.g. an FK that exists anyway); do **not** build
  dedicated machinery or tests for it. *(The `DeleteAll`-releases-claims requirement is
  separate and real — that frees values for live entities bulk-deleted under a surviving
  model.)*

## 9. Out of scope

- Commercial (Cassandra) backend enforcement — tracked separately; advertises
  unsupported until then.
- Backend-swap stranding (§3.6).
- Resurrect semantics (§2.2).
- Unique keys over non-scalar / array / computed fields.
- Adding a unique key to a model that holds live entities (impossible by §2.1).
- **Closing the multi-node key-staleness window (§3.8)** — documented as a bounded known
  limitation here; the fix is a general **acknowledged model-cache invalidation** cluster
  primitive, tracked as a separate enhancement issue (not built into this feature).
