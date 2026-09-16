# Point-in-time read cost, and dating writes at commit — design

**Issues:** #582 (read cost), #583 (stamping), #498 (durable submit time, folded in)
**Milestone:** v0.9.0
**Date:** 2026-09-16
**Status:** design agreed, pending spec review

## Problem

Two defects in the postgres plugin, independent in cause and joined by the
query they meet in.

**One — a point-in-time read costs what the history costs.** The
point-in-time branch of `searchBaseQuery` resolves the latest revision per
entity with `DISTINCT ON (entity_id)` over `entity_versions`. The planner
walks every revision of every entity up to the instant and applies the
caller's condition afterwards, so a point-in-time search, an async snapshot
scan and grouped statistics at an instant all cost what the *history* costs,
not what the *model* costs. On 10,000 entities of 250 revisions, a
point-in-time search whose condition matches one entity reads 2,430,000
revisions and takes 4.9 s, against 0.09 ms for the equivalent current-state
search. Measurements below.

**Two — a write is dated when its transaction started, not when it
committed.** `Save` stamps `valid_time` from `CURRENT_TIMESTAMP`, which
PostgreSQL fixes at transaction start; `transaction_time` defaults from the
same expression; `Commit` captures the submit time the same way. A
transaction that starts at T0 and commits at T0+30s therefore writes
revisions dated T0. A read at an instant between the two misses them; the
same read after the commit finds them. **Two reads at the same instant
disagree, and the past changes after it has been served.**

The second defect is postgres-only: memory, sqlite and the commercial
Cassandra backend all date at commit, as does the production Cyoda platform.
Two existing workarounds are symptoms of it — point-in-time reads must be
routed off the ambient transaction because `T_start <= T_start` is trivially
true, and `CompareAndSave` needs `statement_timestamp()` because its
transaction fixes its start before waiting on the row lock.

## Decisions

Recorded rulings, with the reasoning that produced them.

1. **An entity's model reference is immutable** — its model id, name and
   version are fixed at creation. No storage backend enforces this today;
   all three overwrite it from the caller's values. This design enforces it.
2. **`transaction_time` is the instant the transaction is submitted** — the
   commit instant on a relational backend. For a normal change
   `valid_time == transaction_time`; a backdated change (not implemented)
   carries the `transaction_time` of the version it replaces.
3. **`creationDate` / `lastUpdateTime` take the commit instant**, matching
   the platform, memory, sqlite and Cassandra, which treat them as derived
   payload rather than an anchor.
4. **`wall_clock_time` stays the physical insertion moment** —
   `clock_timestamp()` at the inserting statement. It is the one value that
   answers "when was this row actually written", which a commit-phase stamp
   cannot.
5. **Temporal values live in columns; the document does not copy them.** The
   original storage design already names the columns as the source of truth.
   Rewriting JSONB documents at commit would roughly double every
   transaction's write volume and end `entity_versions`' append-only
   property.
6. **Audit events share their transaction's commit instant**, so the audit
   trail and the version history cannot drift apart or invert.

## Part A — the point-in-time base query follows entities

### Shape

```sql
SELECT doc, creation_date, last_modified FROM (
  SELECT v.doc, v.entity_id, v.version, v.model_name, v.model_version,
         v.creation_date, v.last_modified
  FROM entities e
  CROSS JOIN LATERAL (
    SELECT ev.doc, ev.entity_id, ev.version, ev.model_name, ev.model_version,
           ev.creation_date, ev.transaction_time AS last_modified
    FROM entity_versions ev
    WHERE ev.tenant_id = e.tenant_id AND ev.entity_id = e.entity_id
      AND ev.model_name = $2 AND ev.model_version = $3
      AND ev.valid_time <= $4 AND ev.transaction_time <= CURRENT_TIMESTAMP
    ORDER BY ev.valid_time DESC, ev.transaction_time DESC, ev.version DESC
    LIMIT 1
  ) v
  WHERE e.tenant_id = $1 AND e.model_name = $2 AND e.model_version = $3
) latest
WHERE (doc->'_meta'->>'deleted')::boolean IS NOT TRUE
```

Three details are load-bearing, each from a review finding:

- **The inner subquery projects the same columns today's `latest` derived
  table projects.** The query planner emits *bare* `doc`
  (`query_planner.go:446`) and maps `version` / `deleted` / `model_name` to
  *bare column names* (`:398-405`). A caller condition appended to a query
  exposing both `entities` and the lateral would be `42702 ambiguous column`
  — or, for `version`, would silently resolve against the entity's current
  row instead of its row at the instant. Projecting through a subquery keeps
  every generated expression resolving exactly as it does now.
- **The model predicate is repeated inside the lateral.** Today the model is
  filtered on the *version* row. Filtering only on `entities` would move that
  axis onto the entity's current row.
- **`version DESC` is the final tiebreak.** Every row a transaction writes
  already shares `valid_time` and `transaction_time`, so a
  delete-then-recreate in one transaction ties on both sort keys and the
  winner is arbitrary. Part B makes every row of a transaction tie, so this
  is required, not defensive. `GetAsAt` gets the same tiebreak.

### Why the result set is unchanged

- `entities` holds a row for every entity that has ever existed: `Delete` and
  `DeleteAll` set `deleted = true` and no path removes rows. An entity
  deleted since the instant is still found, and its lateral probe returns its
  tombstone or its earlier revision.
- An entity's model reference is immutable (decision 1, now enforced), so
  selecting by model on `entities` and on the version row select the same set.
- An entity created after the instant yields no lateral row and drops out.

A foreign key `(tenant_id, entity_id) REFERENCES entities` makes the first
point an enforced invariant rather than a property that happens to hold, so a
future retention or erasure feature cannot silently truncate history.

### Index

`idx_entities_model_entity_id` (migration 000008) is
`(tenant_id, model_name, model_version, entity_id COLLATE "C")
WHERE NOT deleted`. A point-in-time read must see entities deleted since the
instant, so the migration drops the partial predicate rather than adding a
second near-identical index to the hot write table.

`TestMigrations_IndexesOnExistingTablesAreConcurrent` requires
`CREATE INDEX CONCURRENTLY` for an index on a table an earlier migration
created. `CONCURRENTLY` deterministically deadlocks the concurrent
multi-node boot path, which is why 000008 is grandfathered; this migration
needs its own entry with the same justification.

The index is not always chosen, and the design does not depend on it being
chosen: on a model whose entity count is small enough that every row
qualifies, the planner prefers a sequential scan of `entities` — measured at
10,000 entity rows once 2,000 carried `deleted = true`. It earns its place on
the ordered page path, where it was used, and as the model grows. The
plan test therefore asserts the *lateral probe* into `idx_ev_bitemporal`,
which is the property that bounds the cost, not the outer access method.

Tenant isolation is unchanged. `committedQuerier` is pool-routed and sets no
`app.current_tenant`, so under the owner role both tables' policies are
bypassed identically and under a non-owner role the point-in-time path
already returns nothing. Isolation continues to rest on the explicit
`e.tenant_id = $1` predicate, and the lateral's `ev.tenant_id = e.tenant_id`
correlation is safe only because the outer query pins the tenant — that
correlation must not be relaxed into an unqualified join.

### Reach

`searchBaseQuery` serves `Search`, `Iterate` / grouped statistics and
`GetPage(asAt)`. `GetAsAt` has its own hand-written query and gets the same
ordering and tiebreak, so the family does not fork.

`GetPage(asAt)` gains early termination: ordering by `entity_id COLLATE "C"`
can walk the index and stop at `LIMIT` instead of deduplicating the model
first. That benefit does not apply to a JSON-field sort.

### Measurements

`postgres:17-alpine`, `jit=off`, parallelism disabled, 10,000 entities of 250
revisions (2.5M rows), an instant admitting 2,430,000 of them, `ANALYZE` run.
Three shapes: **V0** as shipped, **V1** the alternative below, **V2** this
design. Execution times from `EXPLAIN (ANALYZE, BUFFERS)`.

| Query | V0 | V1 | V2 |
|---|---|---|---|
| Common condition, `LIMIT 51` | 89.7 ms | 56.8 ms | **1.96 ms** |
| One-match condition, `LIMIT 51` | 4866 ms | 4157 ms | **177.9 ms** |
| Common condition, no `LIMIT` | 2009 ms | 3326 ms | **149.5 ms** |
| Page, `LIMIT 20 OFFSET 5000` | 1924 ms | 2519 ms | **154.4 ms** |
| Rows read, worst case | 2,430,000 | 2,430,000 | 10,000 probes |

**Generic plans hold.** Under `plan_cache_mode = force_generic_plan` the
lateral keeps its nested-loop-over-index-only-scan shape at 134 ms, against
3741 ms for `DISTINCT ON` under the same forcing. The first execution of the
generic plan costs 626 ms and settles to 134 ms.

**Equivalence is measured, not argued.** With 2,000 of the 10,000 entities
deleted *after* the instant, a read at the instant returns 10,000 under both
V0 and V2, and a read at "now" returns 8,000 under V2.

### The alternative that was measured and rejected

Keep `DISTINCT ON`, add the `version DESC` tiebreak, and add a covering index
`entity_versions (tenant_id, model_name, model_version, entity_id,
valid_time DESC, transaction_time DESC, version DESC)` — one migration, no
query rewrite, semantics provably unchanged. That is V1 above.

It does not change the asymptote: still 2,430,000 rows read, because
PostgreSQL 17 has no skip scan. Being wider than the index it replaces in the
plan, it makes two of the four cases **slower than changing nothing** —
3326 ms against 2009 ms unbounded, 2519 ms against 1924 ms paged — and buys
1.6x on one case. It costs 184 MB against a 1776 MB table, 7.4 s to build,
and write amplification on the largest table for every insert.

Rejected on those measurements.

## Part B — one commit instant per transaction

### What moves

| Value | Today | After |
|---|---|---|
| `valid_time` | transaction start | commit instant |
| `transaction_time` | transaction start (column default) | commit instant |
| `entity_versions.creation_date` (new column) | inside `_meta`, transaction start | commit instant of the entity's first transaction |
| `entities.creation_date` / `last_modified` (new columns) | inside `_meta`, transaction start | commit instant |
| `sm_audit_events.timestamp` | engine process clock | commit instant |
| recorded submit time | transaction start | commit instant |
| `wall_clock_time` | inserting statement | unchanged |

### Mechanism

One `clock_timestamp()` read in `Commit`, after read-set validation and
immediately before `COMMIT`, is the transaction's instant. It is applied by
narrow-column `UPDATE`s over the transaction's own rows in `entity_versions`,
`entities` and `sm_audit_events` — no JSONB rewrite, because the documents no
longer carry these values.

- **`transaction_id` column on `entity_versions`**, indexed, identifies those
  rows. `GetVersionByTransaction` moves onto it, replacing its unindexed
  `doc->'_meta'->>'transaction_id'` probe, so the table has one source of
  truth rather than two. The column is load-bearing for correctness, not an
  optimisation: after a savepoint rollback the in-memory write set disagrees
  with the table, and the table is right.
- **A non-transactional write keeps the empty-string transaction id.** It
  does not mint a synthetic one — `CompareAndSave` rests on the empty string
  being what a non-transactional write stores — and its own transaction
  updates its single row by primary key.
- **Lock analysis.** `validateInChunks` takes `FOR SHARE` on the *read* set;
  the commit-phase `UPDATE` touches the transaction's own *written* rows,
  already held exclusively. A row in both sets is already self-locked, so no
  lock upgrade occurs and the update cannot deadlock against the validation
  that precedes it. This ceases to hold if the update is ever widened beyond
  the transaction's own rows.
- **`creation_date` is stamped on every row of an entity whose first version
  belongs to this transaction** — not only on that first version. A
  transaction that creates an entity and then updates it carries the
  creation date forward by reading the entity *inside* the transaction, so
  the second version holds the provisional value; stamping only version 1
  would leave the two disagreeing.
- **Rows carry a provisional stamp between insert and commit**, so the
  columns stay `NOT NULL`. Nothing outside the transaction can observe them;
  an in-transaction read sees provisional values, which is what memory and
  sqlite already do, and is documented rather than designed away.

### Reads project from columns

`unmarshalEntityDoc` parses both dates out of `_meta` and errors when they
are absent, so it takes them as arguments instead. Every read path projects
them: `Get`, `GetPage`, `GetAsAt`, `GetVersionByTransaction`,
`GetVersionMetadata`, `scanEntities`, `scanEntitiesFilterDeleted` and
`postgresIter.Next`. The S-1 single-column invariant is replaced by a named,
documented column list shared between the base query and its scanners.

`scanEventRows` likewise takes the `timestamp` column and overrides the
copy inside the event document.

A migration backfills the new columns from `_meta` before writes stop
populating it.

### Consequences claimed as fixes

- `CompareAndSave` stamps at its own commit, deleting the
  `stampAtTxStart` / `stampAtStatement` split.
  `TestNonTxCompareAndSave_StampsAfterTheLockWait` is kept and re-justified:
  commit stamping satisfies its intent.
- The transitions endpoint resolves a transaction id through `GetSubmitTime`
  into `GetAsAt`. Today `CompareAndSave` dates its version *after* that
  submit time, so the lookup can miss its own write. Commit stamping fixes
  that latent bug; it gets a test.
- **Non-transactional `Save` gets its own transaction**, which it needs for a
  commit phase to exist. That also closes a torn-write window: today it runs
  four separate autocommit statements and inserts the `entities` row with a
  `'null'::jsonb` placeholder before updating it, so a concurrent reader can
  observe `doc = 'null'` and a process death can leave `entities` updated
  without its version row. This lands as its own commit with its own failing
  test, first in the series.
- **The conformance harness's clock floor widens.** It reads `Now` from the
  database clock with a 5 ms `AdvanceClock` floor sized for clock resolution.
  With the stamp taken in the commit phase, that floor must also absorb
  commit latency, or a test that advances the clock by one tick and expects
  ordering will flake under a slow commit.

### Durable submit time (#498)

The submit time is written to a `submit_times` table inside the commit
transaction — `(tenant_id, tx_id)` keyed, RLS policy like every other table,
pruned on the existing 1-hour TTL — mirroring sqlite's. The in-process map
stays as the fast path. This settles that issue's open choice: the table
covers a transaction that committed no entity write, which deriving from
`entity_versions` cannot, and it costs nothing beside the update already
being made. Without Part B it would only make the wrong value durable.

### Model-reference immutability

`Save` rejects a write whose model reference differs from the stored
entity's, instead of overwriting it. All three in-tree backends overwrite
today (postgres `ON CONFLICT DO UPDATE SET model_name = EXCLUDED.model_name`,
sqlite `INSERT OR REPLACE`, memory copying `entity.Meta.ModelRef`), so all
three change, and the rule belongs in the SPI conformance suite, which
obliges the commercial backend to follow.

New error code `ENTITY_MODEL_MISMATCH`, HTTP 400, with its
`errors/ENTITY_MODEL_MISMATCH.md` topic and index entry (`TestErrCode_Parity`
enforces the topic).

## What this does not fix

A read taken *while* another transaction commits can still change: reading
the clock immediately before `COMMIT` is not the commit's linearization
point. A transaction that stamps at `T_a` and then blocks in `COMMIT` can be
overtaken by one stamping `T_b > T_a`, so a read at an instant between them
is served without the first and gains it afterwards.

The window shrinks from the transaction's lifetime — seconds to minutes,
spanning processor callouts and client-held transactions — to the commit
itself. memory and sqlite avoid even that, structurally, by holding a global
gate from stamp to publish; postgres cannot without serialising every commit.
Cassandra and the platform's plain read path carry the same residue.

**The spec claims exactly that and no more.** Closing it needs a consistency
horizon — a read at an instant later than the earliest in-flight
transaction's start either waits or fails — which is #581, deliberately not
scheduled.

## Contract and error table

Part A changes no results. Part B changes observable timestamps but brings
postgres into line with the contract the other three backends already
implement, so it is a bug fix, not a Gate 7 contract change: no
`docs/cloud-parity/` file.

Reconciled against Cloud's own source rather than inferred.
`TransactionEntityPair.calculateEntityChange`
(`cyoda-platform/core-libs/core/src/main/java/com/cyoda/core/consistency/TransactionEntityPair.java:56-61`)
stamps `lastUpdateTime` from the transaction's commit-phase submit time on
every write, and `creationDate` from the same value when there was no live
prior entity:

```java
Date trSbmtDate = getDateFromUUID(transactionSbmtTime);
newEntity.setLastUpdateTime(trSbmtDate);
if (oldEntity == null || oldEntity.isDeleted()) {
    newEntity.setCreationDate(trSbmtDate);
}
```

So this change moves postgres toward Cloud, and Cloud keys `creationDate` off
"was there a live prior entity" — which is the rule this design adopts for a
create-then-update in one transaction, independently arrived at. The genuine gap is that the SPI documents no
instant for any of these values; this design writes it into the SPI godoc
(`CreationDate`, `LastModifiedDate`, `EntityVersion.Timestamp`,
`EntityVersionMeta.Timestamp`, `GetSubmitTime`) and into `CONSISTENCY.md`.

| Endpoint | Code | Status | When |
|---|---|---|---|
| `POST /entity/{name}/{version}` (and collection/update/patch forms) | `ENTITY_MODEL_MISMATCH` | 400 | the save targets an entity whose stored model reference differs |
| `GET /entity/{id}/transitions?transactionId=` | unchanged | 200 / 400 | now resolves its own write correctly (was a latent miss) |
| every point-in-time read | unchanged | unchanged | results unchanged; only the instant a revision is dated at moves |

## Coverage matrix

| Scenario | Unit | e2e (postgres) | Parity | gRPC |
|---|---|---|---|---|
| PIT search / iterate / page at an instant returns the same set as before | ✓ | ✓ | existing scenarios | ✓ |
| Entity deleted since the instant is still returned | ✓ | ✓ | ✓ | — |
| Entity created after the instant is absent | ✓ | ✓ | ✓ | — |
| Same-transaction delete-then-recreate resolves deterministically | ✓ | ✓ | ✓ | — |
| PIT base query uses the entities index (EXPLAIN, generic plan) | ✓ | — | — | — |
| A revision is dated at its transaction's commit, not its start | ✓ | ✓ | ✓ | — |
| A read at T does not change across a slow commit that started before T | — | ✓ (isolated, single-backend) | — | — |
| All entities of one transaction share one instant | ✓ | ✓ | ✓ | — |
| `creationDate` / `lastUpdateTime` report the commit instant | ✓ | ✓ | ✓ | ✓ |
| `creationDate` survives create-then-update in one transaction | ✓ | ✓ | ✓ | — |
| Audit event times agree with version times | ✓ | ✓ | — | — |
| `GetSubmitTime` survives a restart and answers on another node | ✓ | ✓ | ✓ | — |
| `transitions?transactionId=` resolves a CompareAndSave write | — | ✓ | — | — |
| Model-reference change rejected | ✓ | ✓ | ✓ (conformance) | ✓ |
| Non-transactional save is atomic (no `'null'` doc observable) | ✓ | ✓ | — | — |

Concurrency scenarios stay in isolated single-backend e2e, never the shared
parity suite.

Each new parity scenario is registered in `e2e/parity/registry.go` and
`wantParityScenarioCount` moves with it, or the suite passes while running
nothing.

The plan test follows the existing pattern: the point-in-time base SQL
becomes a **named constant shared between production and the test**, as
`getPageCurrentQuery` already is, so the EXPLAIN assertion plans the query
that actually runs rather than a copy that can drift from it.

## Documentation

`POSTGRES.md` (its "application-supplied `valid_time`" line is false today,
and the schema changes), the two contradictory `PRD.md` definitions,
`CONSISTENCY.md` (silent on stamping), `cyoda help` content for `crud` and
any topic quoting these fields, `errors/ENTITY_MODEL_MISMATCH.md` plus the
error index, CHANGELOG, and the SPI godoc with a CHANGELOG entry in that repo.
The point-in-time canonicalization spec's per-path predicate table goes stale
under both parts and is corrected.

## Order

1. Non-transactional save atomicity (independent, strictly good).
2. Part A, with its migration and plan tests.
3. Part B, including #498 and model-reference immutability.

One PR against `release/v0.9.0`, staged as reviewable commits.
