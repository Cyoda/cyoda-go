# Compatibility

Cross-repo version compatibility for the cyoda-go ecosystem.

The ecosystem has five independent SemVer axes, each tracking a different stability promise to a different audience. This file declares the supported combinations.

## The five axes

| Axis | What it tracks | Bumps when | Consumed by |
|---|---|---|---|
| **`cyoda-go` binary** `v<X.Y.Z>` | The shipped EDBMS application | Each user-facing release | End users (Homebrew, downloads, Docker) |
| **`cyoda-go-spi`** `v<X.Y.Z>` | The stable plugin contract surface | The SPI Go interface changes | Storage-plugin authors (in-tree + out-of-tree) |
| **`cyoda-go/plugins/<x>`** `v<X.Y.Z>` | Each in-tree plugin's exported API | The plugin module's exported Go API changes | Out-of-tree plugin authors (test fixtures) |
| **Chart `version:`** | Helm chart's manifest output | Templates / values / schema change | Helm operators |
| **Chart `appVersion:`** | Default binary the chart ships | Each binary release worth advertising via the chart | Helm operators |

Coupled by the dependency direction:

```
cyoda-go-spi   ←   cyoda-go   ←   cyoda-go-cassandra (out-of-tree)
                       ↓
                   plugins/{memory,sqlite,postgres}
                       ↓
                   deploy/helm/cyoda  (chart)
                       ↓
                   homebrew-cyoda-go  (formula, auto-synced by GoReleaser)
```

Coordinated-release procedure documented in [`MAINTAINING.md`](./MAINTAINING.md).

## Compatibility matrix — `cyoda-go` × `cyoda-go-spi`

| `cyoda-go` | Root `go.mod` pins | In-tree plugin go.mods pin | SPI surface added in this release |
|---|---|---|---|
| **`v0.8.3`** _(planned)_ | `cyoda-go-spi v0.8.3` | `cyoda-go-spi v0.8.3` | Scheduled-transition runtime: `ScheduledTask` type, `ScheduledTaskStore` interface (durable per-backend task store — `Upsert`/`ScanDue`/`MarkRedispatch`/`Get`/`Delete`/`ReconcileForEntity`), and `SCHEDULED_TRANSITION_ARM`/`FIRE`/`EXPIRE`/`CANCEL` `StateMachineEventType` constants. Plus a further additive surface for the arm-time Function callout, implemented against a local `cyoda-go-spi` checkout ahead of the pinned pseudo-version (see the "v0.8.3 milestone — SPI pseudo-version pin" section below): `TransitionSchedule.Function *ScheduleFunction`, and `ReconcileRequest.Cancel []string` (born-expired task IDs deleted atomically alongside the existing source-state-mismatch cancels). Finalized into the root/plugin `go.mod` pins, and this row, at the real `cyoda-go-spi v0.8.3` tag. |
| **`v0.8.2`** | `cyoda-go-spi v0.8.2` | `cyoda-go-spi v0.8.2` | Composite-unique-key support: `CompositeUniqueKeyCapable` optional interface, `UniqueKey`, `UniqueClaim` types, `WithUniqueKeys`/`UniqueKeysFromContext` context helpers; search-sort ordering: `OrderKind` enum (`OrderText`, `OrderNumeric`, `OrderBool`, `OrderTemporal`) + `OrderSpec.Kind OrderKind` field; client-owned annotations extended to processors and criteria: `ProcessorDefinition.Annotations`, `WorkflowDefinition.CriterionAnnotations`, `TransitionDefinition.CriterionAnnotations` (workflow schema 1.1 → 1.2, dual-shape). Binary-only additions: compute-node callback tx-join (token minted per dispatch, `cyodatxtoken` CloudEvent attribute, `Join`-not-`Begin` semantics on callback, cross-node routing via HTTP proxy / gRPC B→A forward); new env vars `CYODA_TX_TOKEN_TTL`, `CYODA_GRPC_NODE_ADDR`, `CYODA_COMPUTE_HTTP_BASE`. |
| **`v0.8.1`** | `cyoda-go-spi v0.8.1` | `cyoda-go-spi v0.8.1` | Transaction-state sentinel hierarchy: `ErrTxNotFound`, `ErrSavepointNotFound`, `ErrTxTerminated`, `ErrTxRolledBack`, `ErrTxAlreadyCommitted`, `ErrTxCommitInProgress`, `ErrTxTenantMismatch`; grouped-stats optional capabilities: `Iterable` (`Iterate`, `Iterator`, `IterateOptions`, `Filter`) and `GroupedAggregator` (`GroupedAggregate`, `GroupedAggregationOptions`, `ErrAggregationNotPushdownable`); scheduled-transition shape: `TransitionDefinition.Schedule *TransitionSchedule` (`DelayMs`, `TimeoutMs *int64`); async-result shape: `ProcessorConfig.AsyncResult *bool`, `ProcessorConfig.CrossoverToAsyncMs *int64`; client-owned annotations: `Annotations json.RawMessage` on the workflow, state, and transition definitions |
| **`v0.7.1`** | `cyoda-go-spi v0.7.1` | `cyoda-go-spi v0.7.1` | — (pin-sync correction; no new SPI surface) |
| **`v0.7.0`** | `cyoda-go-spi v0.7.0` | `cyoda-go-spi v0.6.1`† | `ProcessorConfig.StartNewTxOnDispatch *bool` |
| `v0.6.3` | `cyoda-go-spi v0.6.0` | `cyoda-go-spi v0.6.0` | — (binary-only changes) |
| `v0.6.2` | `cyoda-go-spi v0.6.0` | `cyoda-go-spi v0.6.0` | — |
| `v0.6.1` | `cyoda-go-spi v0.6.0` | `cyoda-go-spi v0.6.0` | — |
| `v0.6.0` | `cyoda-go-spi v0.6.0` | `cyoda-go-spi v0.6.0` | `ExtendSchema` retry + ctx-cancellation contract |

† The in-tree plugin submodules pin `spi v0.6.1` rather than `v0.7.0` because they don't use `StartNewTxOnDispatch`. SPI is strictly additive — `v0.7.0` is fully backward-compatible with `v0.6.1` consumers.

### Independent version axes

`cyoda-go` and `cyoda-go-spi` version **independently**. The binary pins the SPI version it needs and never skips or burns a version number to mirror the SPI's — this matrix is the source of truth for which binary pins which SPI, not a shared digit. `cyoda-go-spi` follows the same convention as everything else ([README — Versioning](./README.md#versioning)): a **patch** for additive interface surface, a **minor** only for a breaking interface change. Because the SPI has been strictly additive throughout `v0.5.0…v0.8.1`, its minor moves rarely while the binary iterates freely.

Two repo-internal rules still hold and must not be confused with a binary↔SPI coupling: **pin-sync** (root + every `plugins/*/go.mod` agree on one SPI version, CI-gated by `make check-spi-pin-sync`) and **plugin-tag-equals-umbrella** (plugin submodule tags match the `cyoda-go` tag). Both are release hygiene *within* this repo; neither ties the binary's version number to the SPI's.

### Out-of-tree plugin authors

A plugin pinned to **`cyoda-go-spi v<X.Y.Z>`** is compatible with any **`cyoda-go v<A.B.C>`** whose root `go.mod` pins the same `v<X.Y>.*` series or any *later* `v<X+1.0.0>` series that hasn't broken the interfaces the plugin uses.

In practice, today: **all SPI versions `v0.5.0` … `v0.8.1` are mutually source-compatible** (additive changes only). Plugins on any of these versions build and run correctly against `cyoda-go v0.7.x` and `v0.8.1`. This will change only when SPI introduces a breaking interface (none planned for v0.x).

### Migration window

cyoda-go's root `go.mod` may pin a **newer** SPI version than out-of-tree plugins are using. Consumers compose at runtime — the active plugin's pinned SPI version determines which SPI surface is actually exercised, and unused additions are inert. There is no requirement that the binary's SPI pin and the plugin's SPI pin match exactly.

### v0.8.3 milestone — SPI pseudo-version pin

During the v0.8.3 development milestone the root `go.mod` and every in-tree plugin submodule pin a pseudo-version of `cyoda-go-spi` (currently `v0.8.3-0.20260716224445-52bca554ecf2`) while the `ScheduledTaskStore` surface — and, as work continues, the arm-time Function additions (`TransitionSchedule.Function`, `ReconcileRequest.Cancel`) — land. The pseudo-version pin advances as milestone work merges; it is not re-pinned per PR, only when a dependent change needs the newer SPI surface to build. The final pin bump to the real `cyoda-go-spi v0.8.3` tag happens at end of milestone, per the coordinated-release procedure in [`MAINTAINING.md`](./MAINTAINING.md).

### Why there is no `v0.8.0` (SPI or binary)

A `cyoda-go-spi v0.8.0` tag was cut prematurely on 2026-06-13 at an incomplete
commit and was fetched through `proxy.golang.org`, which **permanently** binds a
version to the first commit it serves. A Go module version cannot be re-cut once
the proxy/checksum-database has seen it, so `v0.8.0` is abandoned and
**`cyoda-go-spi v0.8.1`** is the canonical, complete v0.8.x SPI release — it
resolves cleanly through the public proxy with no `GOPRIVATE` workaround. At the
time, `cyoda-go` also skipped `v0.8.0` to keep its number aligned with the SPI's
and shipped **`v0.8.1`**. That number-matching is **no longer policy** — the
binary and SPI now version independently (see [Independent version axes](#independent-version-axes)),
so a future poisoned SPI tag would not force the binary to skip a number. See
[`MAINTAINING.md`](./MAINTAINING.md): a module version is tagged exactly once, at
the final commit, and never re-cut.

### Optional capability interfaces (grouped-stats)

The `Iterable` and `GroupedAggregator` SPI interfaces are **type-assertion-only**. The `EntityStore` base interface is unchanged. Existing plugins keep compiling without implementing either. A plugin that implements neither causes `POST /api/entity/stats/{entityName}/{modelVersion}/query` to return 501 `NOT_IMPLEMENTED_BY_BACKEND`; every other endpoint continues to work normally. Implementing only `Iterable` is sufficient — the service layer falls back to streaming-tally when `GroupedAggregator` is absent. Implementing `GroupedAggregator` without `Iterable` is supported but loses the in-transaction code path (in-tx calls route through `Iterable` to preserve read-your-writes).

## Plugin tag history

| Plugin module | Latest tag | Tracks |
|---|---|---|
| `cyoda-go/plugins/memory` | `v0.7.1` | Memory backend (test + reference) |
| `cyoda-go/plugins/sqlite` | `v0.7.1` | SQLite backend (single-node, embedded) |
| `cyoda-go/plugins/postgres` | `v0.7.1` | PostgreSQL backend (production multi-node) |

These rarely move because each plugin's *exported* Go API (factory constructors, package-level helpers) is stable. Internal changes ride along with `cyoda-go` binary releases without a submodule tag bump. Out-of-tree consumers (e.g. cyoda-go-cassandra's parity test fixtures) pin pseudo-versions resolving to specific cyoda-go commits.

## Out-of-tree plugins

| Plugin | Latest tag | Pins `cyoda-go` | Pins `cyoda-go-spi` | Status |
|---|---|---|---|---|
| `cyoda-go-cassandra` | `v0.1.1` | `v0.6.3-0.20260427233530-f7bc7ee68c60` (pre-#27 pseudo-version) | `v0.6.0` | Implementation-current at v0.6.x; v0.7.0 parity-scenario adoption pending next dependency bump |

When cyoda-go-cassandra bumps to `cyoda-go v0.7.0`, the four new parity scenarios from #229 + #230 (`transactionWindow` chunking, per-item ifMatch isolation, chunk-rollback, paired `STATE_MACHINE_START` + `TRANSITION_ABORTED` audit) automatically run against the Cassandra backend via `e2e/parity/registry.go`. The plugin must implement the optional `parity.TxBoundAuditFixture` interface (returning `true`) to pass the audit-pairing scenario.

## Helm chart × binary

| Chart `version:` | Chart `appVersion:` | Default binary | Notes |
|---|---|---|---|
| `0.7.0` | `0.7.1` | `cyoda-go v0.7.1` | **Current.** Adds optional `migrate.postgres` DSN — a separate migration-Job (owner/DDL) role for the two-role DB model; backward-compatible (falls back to `postgres.existingSecret`). First chart-manifest change since `0.6.3`. |
| `0.6.3` | `0.7.0` | `cyoda-go v0.7.0` | Chart manifests unchanged since `cyoda-0.6.3`; `appVersion` advances independently per [PR #232](https://github.com/cyoda/cyoda-go/pull/232). |
| `0.6.3` | `0.6.3` | `cyoda-go v0.6.3` | Tagged as `cyoda-0.6.3` chart release (April 2026). |

The chart's `version:` bumps only when **rendered manifests** change (templates, values, schema). The chart's `appVersion:` advances each binary release worth advertising via the chart. The two are decoupled by Helm convention.

### Operator action required

| Upgrading binary from → to | Chart action | Operator action |
|---|---|---|
| Any `v0.6.x` → `v0.7.0` | None (chart manifests unchanged) | If fronting a browser SPA: set `extraEnv` `CYODA_CORS_ALLOWED_ORIGINS=https://your-spa.example.com`. New CORS middleware defaults to loopback-only. See [`cmd/cyoda/help/content/config/cors.md`](./cmd/cyoda/help/content/config/cors.md). |
| Any `v0.6.x` → `v0.7.0` | None | Wire-format breaking changes per [`CHANGELOG.md`](./CHANGELOG.md#070--2026-05-05): `messaging.GetMessage` content shape; stub `errorCode` rename; OpenAPI spec reconciliation. Affects API/SDK clients, not the deployment manifests. |

## Homebrew formula

[`homebrew-cyoda-go`](https://github.com/cyoda/homebrew-cyoda-go) ships a single binary per release. The `cyoda.rb` formula is auto-updated by GoReleaser on every `v*` tag push and pins:

- `version "<X.Y.Z>"` (matches the binary tag)
- `url "…/cyoda_<X.Y.Z>_<os>_<arch>.tar.gz"`
- `sha256 "…"` per platform

End users always get a coherent install — the formula's `version` IS the binary's version. There is no separate compatibility concern at this layer.

## Reading this matrix

- **End user installing or upgrading the binary**: care about the binary version only. Use Homebrew, the GitHub Release archives, or the Helm chart with `appVersion`.
- **Helm operator**: care about chart `version:` for upgrades and `appVersion:` for which binary you're deploying. Read the "Operator action required" table when bumping `appVersion:`.
- **Out-of-tree plugin author**: pin `cyoda-go-spi` at the version whose surface you use. The matrix tells you which `cyoda-go` binary versions ship a compatible engine.
- **Downstream Go-module consumer**: the binary's pinned SPI version is whichever the root `go.mod` declares for that release. The plugin submodule pins are independent — they apply only if you're consuming a submodule directly.

## Maintaining this file

Update on every cyoda-go binary release **and** on every cyoda-go-spi tag. The release-manager workflow in [`MAINTAINING.md`](./MAINTAINING.md) includes this file in the release checklist.

When the SPI introduces a breaking interface change (not planned in v0.x), add a "Breaking" row to the matrix and document the migration path in [`CHANGELOG.md`](./CHANGELOG.md).
