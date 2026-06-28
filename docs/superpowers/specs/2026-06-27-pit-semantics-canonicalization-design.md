# Point-in-time read semantics canonicalization (#349)

**Status:** design agreed
**Milestone:** v0.8.2
**Relationship:** prerequisite for #37 (PostgreSQL predicate pushdown). #37 consumes
the canonical bound this work establishes and introduces no PIT-bound logic of its own.

## Problem

Point-in-time ("as at T") reads do not apply a single consistent rule for bounding
the snapshot timestamp. The behaviour diverges on two independent axes, so different
storage engines — and different read paths within the same engine — can return
different historical version sets for the same query.

### Axis 1 — uneven millisecond round-up

`GetAsAt`/`GetAllAsAt` and every memory-engine PIT path round the requested timestamp
up to the next millisecond before comparing:

```go
asAt = asAt.Truncate(time.Millisecond).Add(time.Millisecond)
```

The Search / Iterate / grouped-stats PIT paths on the SQL engines bind the **raw**
timestamp instead. The round-up originated in the memory engine with the rationale
"clients work at millisecond precision but submitTime has micro/nanosecond precision."
That premise is unverified and contradicted by the system itself: PIT input is parsed
with `time.Parse(time.RFC3339, …)` (arbitrary fractional precision) and entity
timestamps are emitted at nanosecond precision (`RFC3339Nano`). There is no
millisecond-canonicalization layer anywhere.

Effect: "as at 302.000ms" becomes `<= 303.000ms`, over-including a version written at
302.9ms (later than asked for) by up to ~1ms. Raw paths correctly exclude it.

### Axis 2 — strictness differs even among the rounded paths

| Path | rounds asAt? | predicate | strictness |
|---|---|---|---|
| memory — all PIT paths | yes | `!submitTime.After(asAt)` | inclusive `<=` |
| postgres `GetAsAt` / `GetAllAsAt` | yes | `valid_time <= $N` | inclusive `<=` |
| postgres Search / Iterate / grouped-stats | no (raw) | `valid_time <= $N` | inclusive `<=` |
| sqlite `GetAsAt` / `GetAllAsAt` | yes | `submit_time < ?` | **strict `<`** |
| sqlite Search / Iterate / grouped-stats | no (raw) | `submit_time <= ?` | inclusive `<=` |
| commercial (Cassandra) — all PIT paths | no | `<= asAtHLC` | inclusive `<=` |

### Impact

- **Cross-engine divergence (fidelity violation).** For the same "as at T" query the
  memory backend can return a version set that sqlite/postgres do not, when a version
  falls in the ≤1ms window around T. The digital-twin goal is engine-independent
  behavioural fidelity; this breaks it.
- **Self-inconsistency within an engine (user-visible).** Async search selects result
  IDs via `Search` (raw bound on SQL engines) but `GetAsyncResults` re-fetches each
  entity via `GetAsAt` (rounded). At a boundary a selected entity can re-fetch to a
  different version or `ENTITY_NOT_FOUND`.
- **No documented contract, no boundary test.** Every existing PIT test separates
  versions by 20–100ms and samples mid-gap (`e2e/parity/temporal.go`,
  `e2e/parity/externalapi/point_in_time.go`), so the boundary behaviour is entirely
  untested — which is why this went unnoticed.

Blast radius is a timing boundary (≤1ms window): no data loss, surfaces only under
fine-grained / bursty-write timing. But it is a genuine consistency-contract violation.

## Canonical rule

**Point-in-time reads compare the requested instant against stored timestamps with
inclusive `<=`, at the engine's native stored precision, with no artificial rounding.**

Applied uniformly to `GetAsAt`, `GetAllAsAt`, PIT `Search`, PIT `Iterate`, and
grouped-stats PIT, across memory, sqlite, postgres, and the commercial backend.

**Precision floor.** Stored-timestamp precision differs per engine: memory nanosecond
(`time.Time`), sqlite/postgres microsecond (`timeToMicro` / `timestamptz`), commercial
millisecond (HLC `bigint`, high 48 bits = `UnixMilli`; sub-ms truncated on encode).
Therefore **cross-engine behavioural fidelity is guaranteed to millisecond
granularity** — the coarsest backend. Finer-grained distinctions are engine-defined.
This is the honest digital-twin contract and is what we document. (Rejected
alternative: normalize all engines to a common millisecond precision at storage —
discards the `RFC3339Nano` wire format the API commits to and is a large, risky storage
change for no real-world benefit, since the round-up artifact, not storage precision,
is the defect.)

"Full precision" in the issue means *removal of the artificial 1ms round-up*, not a
sub-millisecond cross-engine guarantee that Cassandra physically cannot honour.

## Production changes (cyoda-go only)

Nine edits. No new types, no SPI change, no migration.

| Engine | File:line | Change |
|---|---|---|
| memory | `plugins/memory/entity_store.go:324` (`GetAsAt`) | remove round-up |
| memory | `plugins/memory/entity_store.go:420` (`GetAllAsAt`) | remove round-up |
| memory | `plugins/memory/grouped_stats.go:90` (`buildSnapshot`, feeds `Iterate` + grouped-stats PIT) | remove round-up |
| sqlite | `plugins/sqlite/entity_store.go:414` (`GetAsAt`) | remove round-up **and** flip `submit_time < ?` → `<= ?` |
| sqlite | `plugins/sqlite/entity_store.go:549` (`GetAllAsAt`) | remove round-up **and** flip `submit_time < ?` → `<= ?` |
| postgres | `plugins/postgres/entity_store.go:183` (`GetAsAt`) | remove round-up |
| postgres | `plugins/postgres/entity_store.go:242` (`GetAllAsAt`) | remove round-up |

All other PIT paths are already raw inclusive `<=` and become consistent automatically
once the rounded paths stop rounding:

- sqlite `searcher.go` PIT base query: already `submit_time <= ?` (raw).
- postgres `searcher.go` / `grouped_stats.go` PIT: already `valid_time <= $N` (raw).
- memory snapshot helpers (`getSnapshotVersion`, `getAllSnapshotUnlocked`,
  `getAllSnapshotPointersUnlocked`): already `!submitTime.After(...)` = `<=`.

**No collateral.** Transaction-snapshot reads use `tx.SnapshotTime` and never applied
the round-up; they are untouched. The async-search self-inconsistency resolves for free
once `GetAsAt` stops rounding (both Search-select and re-fetch then bind raw `<=`).

**Commercial backend.** Cassandra already implements the canonical rule (inclusive
`<=`, no round-up) at millisecond precision; it requires no production change. The new
boundary parity scenarios run against it on its next cyoda-go dependency update and
confirm convergence. Note Cassandra does **not** implement sync `Search`/`Iterate`/
grouped-stats PIT (only `GetAsAt`/`GetAllAsAt` + index-reader/async-search PIT), so
PIT-search parity scenarios that target those paths are gated to the engines that
implement them, as today.

### Comment hygiene

Remove or correct the round-up rationale comments at each deleted site. The sqlite
`searcher.go` comment that references "matching the memory plugin's convention" /
`!v.submitTime.After(...)` remains accurate (it documents inclusive `<=`) and stays.

## Documentation

- Add a concise **"Point-in-time semantics"** block to `cmd/cyoda/help/content/crud.md`
  as the canonical home: reads are inclusive of the requested instant (`<=`); no
  rounding is applied; the bound is compared at the engine's native precision;
  cross-engine behavioural fidelity is guaranteed to millisecond granularity.
- Cross-reference that block from `cmd/cyoda/help/content/search.md` (PIT search) and
  `cmd/cyoda/help/content/analytics.md` (grouped-stats PIT).
- No `errors/*.md` changes — no new error codes (TestErrCode_Parity unaffected).
- Gate-4: no env vars, no public Go interface, no chart/COMPATIBILITY/SPI-pin change.
  Add a `CHANGELOG` entry under v0.8.2.

## Error / status table (per PIT endpoint)

The change alters *which version* is returned, not the status taxonomy. No new codes.
The behaviour-changing row is the exact-boundary case.

| Endpoint (HTTP / gRPC) | SPI path | Codes (unchanged) | Boundary behaviour after fix |
|---|---|---|---|
| `GET /api/entity/{id}?pointInTime` | `GetAsAt` | 200; 404 `ENTITY_NOT_FOUND`; 400 `INVALID_POINT_IN_TIME`; 400 `BAD_REQUEST` (pointInTime+transactionId) | as-at exactly a version's T → 200 that version (**sqlite previously 404 under strict `<` — fixed**); a later sub-ms version is not over-included |
| `GET /api/entity/{name}/{ver}?pointInTime` | `GetAllAsAt` | 200; 400 `INVALID_POINT_IN_TIME` | set includes versions with T == bound; excludes T > bound (sqlite strictness fixed) |
| `POST /api/entity/search/{…}` + gRPC `Search` (`pointInTime`) | PIT `Search` | 200/202; 400 | already raw `<=`; now identical to `GetAsAt` (async select == re-fetch) |
| `POST /api/entity/stats/{…}/query` + gRPC (`pointInTime`) | grouped-stats / `Iterate` | 200; 400 `INVALID_POINT_IN_TIME`/`INVALID_LIMIT`; 422 `GROUP_CARDINALITY_EXCEEDED`; 501 `NOT_IMPLEMENTED_BY_BACKEND` | memory stops rounding → matches SQL engines |
| `GET /api/entity/{id}/changes?pointInTime`, `…/transitions?pointInTime` + gRPC `GetChangesMetadata` | history / transitions as-at | 200; 404; 400 `INVALID_POINT_IN_TIME` | verify raw inclusive `<=` (no round-up site here today — add boundary assertion to prove it) |

## Coverage matrix (scenario × layer)

| Scenario | Unit (per-engine, white-box) | Running-backend e2e | Cross-backend parity | gRPC |
|---|---|---|---|---|
| Exact-T inclusive (`as-at T_v` returns v) | memory / sqlite / postgres | — | ✅ registry: `GetAsAt` + `GetAllAsAt` (all backends incl. Cassandra @ ms) | ✅ search / changes PIT envelope |
| No over-inclusion (later sub-ms version excluded at `T_v`) | memory (ns), sqlite (µs), postgres (µs) — hand-placed timestamps | — | — (sub-ms not representable on Cassandra) | — |
| sqlite `<` → `<=` (version at exactly creation-T visible) | sqlite | — | covered by exact-T parity | — |
| PIT Search / Iterate / grouped-stats boundary | memory / sqlite / postgres | — | ✅ memory / sqlite / postgres (Cassandra path not implemented → gated) | ✅ |
| Async self-consistency (Search select == `GetAsAt` re-fetch at boundary) | — | ✅ isolated single-backend (postgres) | — | — |

### Test approach rationale

- **Over-inclusion and strictness** are sub-millisecond / timing-sensitive defects, so
  they are driven by **per-engine white-box unit tests with hand-placed version
  timestamps** (deterministic; these are the RED tests). For each engine that rounds,
  cover every affected path: `GetAsAt`, `GetAllAsAt`, and the engine's PIT
  Search/Iterate/grouped-stats. Assertions: (a) a version written at exactly T is
  included by `as-at T` (inclusive boundary); (b) a version written at T+ε within the
  same millisecond is excluded by `as-at T` (no round-up over-inclusion); (c) sqlite:
  the version at exactly its creation T is visible (`<=` not `<`).
- **Exact-T inclusivity** is deterministic at millisecond granularity, so it is the
  **cross-backend parity** guard — it runs on Cassandra too and asserts every backend
  returns the same version when queried at a version's own reported timestamp. New
  scenarios are registered in `e2e/parity/registry.go` (and the external-API
  `parity.Register` set) so all backends, including the commercial one, run them.
- The **async self-consistency** check is consistency-flavoured (Search-select vs
  `GetAsAt` re-fetch agreeing at a boundary); it stays an **isolated single-backend
  e2e** rather than entering the shared parity suite, per `.claude/rules/test-coverage.md`.

### Existing tests to review under GREEN

Tests that encode the old rounded expectation must be re-derived (not blindly relaxed):
`plugins/memory/search_store_test.go`, `plugins/postgres/model_store_test.go`,
`plugins/postgres/model_extensions_*_test.go`, `plugins/sqlite/model_extensions_internal_test.go`.
Any that constructed expected timestamps via `Truncate(time.Millisecond).Add(...)` to
match the buggy bound are corrected to the canonical inclusive raw `<=`.

## Out of scope

- Storage-precision normalization across engines (rejected above).
- Any change to transaction-snapshot semantics (`tx.SnapshotTime`).
- #37's predicate pushdown — separate work that builds on this canonical bound.
- Commercial-repo changes — Cassandra is already canonical; convergence is verified by
  the parity suite, not by a change in this milestone.

## Verification

- `make test-all` (root + memory/sqlite/postgres plugins; Docker for postgres
  testcontainers) green, including the new unit, e2e, and parity scenarios.
- `go vet ./...` and per-plugin vet clean.
- `make race` once before PR.
