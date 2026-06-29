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

### 3.1 Claims computed in the service layer

The service layer (cyoda-go) owns a single **signature helper**. Given a model's unique-key
definitions and an entity doc it produces a list of **claims** `{keyId, signature}`:

- A claim is emitted **only** when a key is *fully present*. All-null keys produce no claim
  (⇒ exempt). Partially-filled keys are rejected *before* this point with a 422.
- The `signature` is a **type-tagged canonical** encoding of the ordered scalar values.
  Because it is computed in exactly one place, canonicalization is byte-identical across
  every engine — there is no cross-engine drift surface.

Claims ride on `spi.Entity` as an **additive, transient field** (not marshaled into the
stored doc / `Data` bytes). Engines read it on the write path and enforce; they never
compute signatures themselves. The field is provably non-persisted: memory stores
`copyEntity(entity)` which copies only `Meta`+`Data` (`plugins/memory/entity_store.go`
~`:37-41`), and postgres builds the doc from explicit `Meta` fields + `entity.Data` via
`marshalEntityDoc` (`plugins/postgres/entity_doc.go` ~`:34-74`) — neither does
`json.Marshal(entity)`, so a new struct field cannot leak into storage.

**Why claims-in-service (not an SPI helper):** keeps the canonicalization in one Go code
path, avoids plumbing model descriptors into every engine's `Save`, and keeps the SPI
change purely additive (a struct field, not a new interface method). See §3.6.

### 3.2 Signature canonicalization (pinned)

`spi.Entity.Data` is raw `[]byte`; the doc never round-trips through `float64` at storage,
so precision can be preserved. Rules:

- **Numbers:** parse with `json.Number` into an **arbitrary-precision decimal** (pin the
  exact library in the plan — `math/big.Rat` or a decimal package; **never** `float64`/
  `big.Float`, which round). Normalize to one canonical decimal form so `1`, `1.0`, `1e0`
  collide and **full int64+ precision is preserved**. Enumerate and test the edge cases:
  negative zero (`-0` ≡ `0`), large integers (> 2^53), high-exponent literals (`1e400`
  must not overflow or round), and trailing-fraction normalization. Do **not** reuse
  `cyoda_try_float8` (search-read helper; lossy above 2^53 and never present at write time).
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
- **`SaveAll`** — collection create (`spi.EntityStore.SaveAll`); see §3.7 for intra-batch
  uniqueness.

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
- Release-on-soft-delete happens under `entityMu` on **every** delete path (commit tombstone
  loop, the non-tx `Delete`, and `DeleteAll`), not only the commit loop.

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
- **sqlite / memory:** map their unique-conflict signal to the same sentinel.
- **Service mapping:** add a branch to `common.Internal` (`internal/common/errors.go`
  ~`:96`) mirroring the existing `ErrConflict` branch but returning
  `Operational(409, UNIQUE_VIOLATION)` **without** `.AsRetryable()`.

`spi.ErrUniqueViolation` is a **new additive sentinel** in `cyoda-go-spi`. The 409 body
names the violated `keyId` only — **never** the incumbent entity's id (need-to-know; avoids
leaking another entity's existence even within a tenant).

### 3.6 Capability gate

**Hard requirement:** it must be impossible to configure / persist a composite unique key
on a storage engine that does not support it. The commercial backend will not support it
for some time; any attempt to declare a key on an unsupported backend MUST be rejected with
**422 `COMPOSITE_KEY_UNSUPPORTED`** — *never* silently accepted-and-unenforced. The
rejection must fire on **every path that can introduce a key definition** — model
registration / import (`ImportModel`) and the lock transition — not just one of them.

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

### 3.7 Collection create / `SaveAll` — intra-batch uniqueness

`SaveAll` (collection create, `spi.EntityStore.SaveAll`) must enforce uniqueness **within
the batch** as well as against committed state: two entities in one `SaveAll` sharing a
value-set must collide. SQL gets this for free (the second claim INSERT raises `23505` in
the same tx); the memory signature-map check in the flush must likewise reject the second
in-batch claim. Coverage matrix includes an intra-batch-duplicate row.

## 4. Unique-key definition & declaration surface

```
UniqueKey {
  id:     string        // stable identifier, unique within the model
  fields: [ string ]    // ordered scalar leaf JSONPaths
}
```

**Declaration surface (corrected — supersedes any earlier "WorkflowConfigurationDto"
reference).** Models in cyoda-go are *not* defined via an explicit schema DTO — they are
imported from `SAMPLE_DATA` (schema inferred) and exported via `ExportMetadata`. The model
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
- **Persistence:** the key list lives **inside the model schema bytes**
  (`spi.ModelDescriptor.Schema`), via a codec evolution — the current `Marshal`/`Unmarshal`
  emit a bare `wireNode` tree; they gain a back-compat wrapper
  `{ "root": <wireNode>, "uniqueKeys": [...] }` (a missing wrapper / bare-node payload reads
  as "no unique keys"). Engines persist `Schema []byte` opaquely ⇒ **zero per-engine
  model-store change, no model-table migration**. `ExportMetadata` round-trips the keys.

Validation (on the `unique-keys` PUT — capability first, then definition; both 422):
- backend **capability** present (else 422 `COMPOSITE_KEY_UNSUPPORTED`);
- every field is a known **scalar leaf** in the (inferred) model schema — reject
  array / object / wildcard / unknown paths;
- `fields` non-empty, no duplicate field within a key; `id` unique within the model
  → else 422 `INVALID_UNIQUE_KEY_DEFINITION`.

Because the keys ride inside `Schema`, this touches the model import/export surface ⇒
follow `docs/workflow-schema-versioning.md` bump rules (Gate 4), and bump the schema codec's
back-compat handling.

## 5. Data flow

**Create:** service computes claims → partial key ⇒ 422 `INVALID_UNIQUE_KEY`; else write
entity + claim rows in one tx → unique primitive collision ⇒ `ErrUniqueViolation` ⇒ 409
`UNIQUE_VIOLATION`. *This closes the First-Committer-Wins gap*: two concurrent creates of
distinct entity ids sharing a value-set have disjoint write-sets, so the existing FCW check
cannot catch them — the native unique index (sql) / `entityMu`-guarded map (memory)
serializes them so exactly one commits.

**Update (moving a key):** recompute claims → delete-first old claim(s), insert new → 409 on
collision; 412 `ENTITY_MODIFIED` still applies via `CompareAndSave`.

**Soft-delete:** engine removes the entity's claim rows in the same tx → value freed.

## 6. Error / status-code table

| Operation (HTTP & gRPC) | Code | HTTP | Retryable | Trigger |
|---|---|---|---|---|
| Create entity | `INVALID_UNIQUE_KEY` | 422 | no | partially-filled composite key |
| Create entity | `UNIQUE_VIOLATION` | 409 | no | fully-present key collides with another live entity |
| Update / patch entity | `INVALID_UNIQUE_KEY` | 422 | no | partially-filled composite key |
| Update / patch entity | `UNIQUE_VIOLATION` | 409 | no | moved/created key collides with another live entity |
| Update / patch entity | `ENTITY_MODIFIED` | 412 | no | existing optimistic-lock failure (unchanged) |
| Configure / lock model | `COMPOSITE_KEY_UNSUPPORTED` | 422 | no | unique key declared on a backend without capability |
| Configure / lock model | `INVALID_UNIQUE_KEY_DEFINITION` | 422 | no | key references non-scalar / array / unknown path, dup id, empty fields (reuse an existing model-validation code instead if one fits cleanly) |
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
| `SaveAll` intra-batch duplicate ⇒ 409 (no torn write) | | ✓ | ✓ | |
| Multiple independent keys per model | | ✓ | ✓ | |
| Definition validation (non-scalar/array/unknown/dup) ⇒ 422 | ✓ | ✓ | | ✓ |
| `COMPOSITE_KEY_UNSUPPORTED` on unsupported backend | | | ✓ (commercial asserts this) | ✓ |
| Concurrency: two concurrent creates same key ⇒ exactly one wins, other 409, no torn write | | ✓ (isolated, single-backend) | **never in shared parity** | |

Parity scenarios registered in `e2e/parity/registry.go`; positive uniqueness scenarios are
**capability-gated** (skipped on backends that report unsupported), while the
`COMPOSITE_KEY_UNSUPPORTED` rejection *is* asserted there — so every backend either enforces
uniqueness or cleanly refuses to configure it. Concurrency tests are isolated single-backend
e2e asserting consistency, per `.claude/rules/test-coverage.md` and the known
memory-backend parity-destabilization.

## 8. Cross-cutting / dependencies

- **SPI coordinated release** (`MAINTAINING.md`): the additive `spi.Entity` claims field,
  `spi.ErrUniqueViolation`, the `CompositeUniqueKeyCapable` interface, and the SPI-level
  unique-key definition land in `cyoda-go-spi` on `main`; cyoda-go pseudo-version-pins
  during the milestone; SPI tag + pin-bump as the final step. **No `replace` directive.**
- **Gate 4 docs:** OpenAPI / help topics for the new model-config `uniqueKeys` surface;
  help topics for the new error codes; `COMPATIBILITY.md` on the SPI bump;
  `docs/workflow-schema-versioning.md` bump per the import-surface change.
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
