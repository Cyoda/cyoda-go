# cyoda-go — architecture brief

The one-page map a reviewer or a new engineer reads before the code. It is a
reference, in the present tense, measured against `release/v0.8.4` at
`134bcaa`; `docs/ARCHITECTURE.md` is the long-form reference behind it. When
this brief and the code disagree, fix one of them in the same change.

Each section ends with the check that keeps it honest.

## 1. Concept index — domain noun → directory

| Noun | Lives in | Not in |
|---|---|---|
| Entity CRUD, transitions, conditional delete, grouped stats | `internal/domain/entity/` | — |
| Model: descriptors, schema tree, extend/lock, import/export, payload ingest checks | `internal/domain/model/` (`schema/`, `ingest/`, `importer/`, `exporter/`) | the schema **node** type lives in the SPI (`ModelNode`), not here |
| Search: condition validation, path grammar, sort keys, direct + async execution, worker pool | `internal/domain/search/` | predicate *evaluation* (below) |
| Predicate evaluation kernel: leaf comparison, filter match, path resolution, LIKE glob | `cyoda-go-spi` (`filter_match.go`, `eval_leaf.go`, `filter_path*.go`, `like_pattern.go`) | `internal/match` |
| In-process predicate tree adapter over the kernel (criteria, delete/stats residual) | `internal/match/` | plugins (they use the SPI kernel directly) |
| Workflow FSM, criteria, processors, cascade, audit of transitions | `internal/domain/workflow/` | — |
| Scheduled transitions (coordinator scan loop) | `internal/scheduler/` | `workflow/` owns the arm/re-arm semantics |
| Transactions: contract | `cyoda-go-spi` (`transaction.go`, `txcontext.go`) | — |
| Transactions: implementation (SI + first-committer-wins) | `plugins/<backend>/txmanager.go` / `transaction_manager.go` | the engine never implements isolation |
| Transaction join across nodes: token, resolution, HTTP middleware, per-tx gate | `internal/cluster/token/`, `internal/domain/txjoin/`, `internal/httpmw/`, `internal/txgate/` | — |
| Cluster: discovery/gossip, transparent proxy, compute dispatch, model cache, peer SSRF guard | `internal/cluster/{registry,proxy,dispatch,modelcache,peeraddr}/` | — |
| Compute-node integration (CloudEvents over gRPC), member lifecycle | `internal/grpc/`; stubs `api/grpc/`; definitions `proto/` | — |
| HTTP server, router, health, help endpoint, binding errors | `internal/api/` | domain handlers live beside their service (`internal/domain/*/handler.go`) |
| Generated OpenAPI types | `api/` (from `api/openapi.yaml`) | — |
| Errors: `AppError`, error codes, tickets, diagnostics | `internal/common/` | per-code documentation: `cmd/cyoda/help/content/errors/<CODE>.md` |
| Auth: JWT/JWKS/M2M/OBO, key sources | `internal/auth/`; per-tenant OIDC registry `internal/auth/oidc/`; dev mock `internal/iam/mock/` | — |
| Accounts, API keys, trusted issuers | `internal/domain/account/` | — |
| Audit trail; edge messages | `internal/domain/audit/`, `internal/domain/messaging/` | — |
| Config, env/`_FILE` loading, wiring | `app/` | — |
| Observability (OTel), logging wrappers, admin listener | `internal/observability/`, `internal/logging/`, `internal/admin/` | — |
| Consumer-side interfaces between engine layers | `internal/contract/` | plugin authors never implement these |
| Storage backends (one active per binary) | `plugins/memory`, `plugins/sqlite`, `plugins/postgres` (own `go.mod` each); commercial backend in a private sibling repo | — |
| CLI + help topics; help is also the spec-artefact source (`cyoda help openapi/grpc/cloudevents`) | `cmd/cyoda/help/` | — |
| Tests: cross-backend parity scenarios | `e2e/parity/` (registry in `registry.go`; consumed by every backend incl. commercial) | `internal/e2e` |
| Tests: full-HTTP-stack single-backend e2e (own Postgres container) | `internal/e2e/` | — |
| Tests: external API scenario dictionary | `docs/test-scenarios/external-api-scenarios.md` → `e2e/externalapi/`, `e2e/parity/externalapi/` | — |
| Tests: SPI conformance suite every backend runs | `cyoda-go-spi/spitest/` | — |

Check: a grep for a noun's directory finds its owner, not a consumer.

## 2. Module index — one responsibility, and what it must not do

| Module | Responsibility | Must not |
|---|---|---|
| `cyoda-go-spi` | The storage-plugin contract and the evaluation kernel; stdlib only | import cyoda-go; carry domain wire syntax (one recorded exception, §4 inv. 4) |
| `app/` | Config and dependency wiring; resolves the plugin via `spi.GetPlugin` | contain domain logic |
| `cmd/cyoda` | Entrypoint; blank-imports stock plugins; `help` subcommand | be imported by anything but `internal/api` (help content) |
| `internal/api` | HTTP router and server-level endpoints; binds domain handlers | implement domain rules |
| `internal/grpc` | CloudEvents service, streaming, member registry, dispatch envelope | be imported by domain packages |
| `internal/domain/*` | One domain each (entity, model, search, workflow, account, audit, messaging, txjoin, pagination) | import `internal/api`, `internal/grpc`, or a plugin |
| `internal/match` | Tree adapter over the SPI kernel for in-process evaluation | import `domain/search`, `domain/entity`, `domain/workflow` (it serves them) |
| `internal/cluster/*` | Node discovery, routing, cross-node dispatch, model-cache invalidation | own transaction semantics (that is the plugin's) |
| `internal/txgate`, `internal/httpmw`, `internal/domain/txjoin` | Per-transaction exclusion; token → joined context | — |
| `internal/auth`, `internal/auth/oidc`, `internal/iam/mock` | Authentication; implement `contract.AuthenticationService` | be bypassed by any handler |
| `internal/common` | `AppError`, error codes, tenant/uuid/diagnostics primitives | import any domain package |
| `internal/contract` | Interfaces between engine layers | contain implementations |
| `internal/scheduler` | Coordinator-only loop firing due scheduled tasks | decide workflow semantics |
| `internal/observability`, `internal/logging`, `internal/admin` | OTel init and decorators; slog wrappers; `/livez /readyz /metrics` | — |
| `internal/skeleton` | No-op stand-ins for two engine contracts. `AuditService` is wired by `app` and consumed by nothing; `ExternalProcessingService` (every criterion true) has no caller | be wired where a real implementation exists — it would silently satisfy invariant-2 violations (see §5) |
| `plugins/{memory,sqlite,postgres}` | One storage backend each: stores, transaction manager, predicate pushdown | import cyoda-go internals (module boundary enforces it); diverge from each other on the same contract |
| `e2e/parity`, `internal/e2e`, `internal/testpg`, `internal/testing/localproc` | Test scenarios and fixtures | — |
| `internal/oasdiffcheck`, `cmd/release-preflight` | Build gates (OpenAPI breaking-change gate; releasability) | — |

Check: every row's responsibility is one sentence with one verb.

## 3. Dependency rules

Measured with `go list` over the root module (see the check below); direction
is "importer → imported".

**Layers, top to bottom.**

```
cmd/cyoda, app
  → internal/api, internal/grpc, internal/httpmw, internal/cluster/*, internal/scheduler, internal/auth*, internal/iam
    → internal/domain/*
      → internal/match, internal/txgate, internal/domain/model (schema), internal/domain/pagination
        → internal/common, internal/contract, internal/logging, internal/observability
          → cyoda-go-spi  (stdlib only; imports nothing above it)
plugins/<backend>  → cyoda-go-spi only
```

**Rules.**

1. Anything may import the SPI; the SPI imports nothing of ours.
2. A plugin imports only the SPI. Nothing but `cmd/cyoda` imports a plugin
   (blank import for registration).
3. Domain packages never import `internal/api` or `internal/grpc`; both of
   those import domain. Measured: no `internal/domain/* → internal/api` or
   `→ internal/grpc` edge exists.
4. `internal/match` imports only `internal/domain/model/schema` (canonical
   field-path form) and the SPI. Its consumers are `domain/search`,
   `domain/entity`, `domain/workflow`.
5. `internal/common` and `internal/contract` import only the SPI.
6. Within `internal/cluster`, `token` and `peeraddr` are leaves; `proxy` and
   `dispatch` sit above them; the root package sits above `dispatch`.

**Deliberate exceptions, and why.**

- `internal/domain/* → api` (the generated OpenAPI types, top-level `api/`):
  domain handlers bind the generated request/response types directly. The
  generated package is a wire-shape leaf, not the HTTP server.
- `internal/api → cmd/cyoda/help`: the HTTP help endpoint serves the same
  content tree the CLI renders; help content is the single spec-artefact
  source and is not duplicated.
- `internal/cluster/dispatch → internal/grpc`: cross-node compute dispatch
  forwards through the gRPC member registry. `internal/grpc` does not import
  `dispatch` (it imports `cluster/proxy` and `cluster/token` only), so there
  is no cycle.
- `internal/cluster` (root) `→ internal/domain/workflow, internal/scheduler`:
  `scheduler_rpc.go` exposes the coordinator's scheduler over the cluster
  RPC. Cluster is above domain, so this is layer-consistent.
- `internal/domain/txjoin → internal/cluster/token`: a domain package
  reaching up to a cluster leaf. Accepted because `token` is a pure codec
  with no dependency of its own beyond `common`.

**Enforcement today.** No `golangci` depguard exists. Architecture is guarded
by tests that parse source: `internal/txgate/suspend_call_sites_test.go`,
`plugins/memory/mutex_discipline_test.go`,
`internal/e2e/goroutinesafety/`. This section is the source of truth for an
arch-lint config when one is added.

Check (re-derive the graph):

```sh
go list -f '{{.ImportPath}}|{{join .Imports ","}}' ./internal/... ./app/... ./cmd/... \
  | grep cyoda-go/ | awk -F'|' '{print $1" -> "$2}'
```

## 4. Invariants — "we never X, because Y"

1. **We never commit partially or substitute a fallback value.** An
   operation that cannot complete correctly is rejected and rolled back,
   because a wrong-but-available answer is indistinguishable from a right
   one to the caller (`.claude/rules/correctness-over-availability.md`).
2. **We never degrade an unavailable required dependency** (compute node,
   peer, store) into a lesser answer, because the dependency being up is a
   precondition of the request, not a nice-to-have.
3. **No search path materialises a model.** Every read of more than one
   entity is one bounded cursor (`Searcher.Search`, `Iterate`) or one page
   (`GetPage`), because peak memory must be independent of model size
   (`docs/cloud-parity/direct-search-bounded-or-fail.md`). As of `134bcaa`
   the engine's translation-failure fallback and four plugin-internal
   in-transaction helpers still violate this; #477 (#516 row 13) closes them.
4. **A storage plugin never sees domain predicate syntax**; it receives
   `spi.Filter`, because the filter is the anti-corruption layer that lets
   the condition language change without touching backends. Recorded
   exception: `SearchJob.Condition` carries the raw condition for
   self-executing async stores (spi#31 closed not-planned; spi#45 documents).
5. **No data path crosses a tenant.** Every request context carries a
   resolved `TenantID` and every store partitions on it, because tenant
   isolation is not a feature but the ground every other guarantee stands on
   (`.claude/rules/security.md`).
6. **A point-in-time read is committed-only and ignores the ambient
   transaction**; a live read inside a transaction is read-your-own-writes.
   "As at T" and "plus writes with no commit time yet" have no consistent
   joint answer (`docs/cloud-parity/tx-aware-search.md`).
7. **Direct search never truncates.** A matched set over the limit is an
   error, because a truncated prefix cannot be told from a complete result.
8. **One path grammar, one resolver, and the kernel decides.** What a
   `jsonPath` addresses is model-driven; pushdown narrows a candidate set but
   never decides a match (`docs/cloud-parity/path-grammar.md` §12,
   `operator-semantics.md` §7). Backends therefore cannot disagree.
9. **Backends never diverge on the same contract.** A memory/sqlite/postgres
   difference is a bug; parity tests guard consistency, they do not define
   behaviour (`CLAUDE.md` "External Storage Plugins").
10. **We never log or return secrets or internals.** Credentials, tokens and
    keys are never logged at any level; 5xx responses carry a ticket and a
    generic message, 4xx carry full domain detail
    (`.claude/rules/security.md`, `error-handling.md`).
11. **cyoda-go defines the contract; Cloud follows.** Every change to the
    API, wire or entity/workflow semantics is recorded in
    `docs/cloud-parity/` (Gate 7). Shipped artefacts never carry issue IDs.
12. **A published Go module tag is immutable.** A wrong tag is superseded
    by a fresh version, never moved or re-cut (SPI `MAINTAINING.md`).

Check: a design review names each invariant it touches, by number.

## 5. Legacy map

| Area | State | Target |
|---|---|---|
| Whole-model reads: engine translate-failure fallback (`internal/domain/search/service.go`), sqlite `getAllTx`/`getPageTx`, memory `Search` deep copy, in-tx `Count`/`DeleteAll` on memory + sqlite | migrating (#477 in flight, spec `docs/superpowers/specs/2026-09-01-477-…`) | one cursor or one page everywhere; `GetAll`/`GetAllAsAt` removed from the SPI; `Iterate` required |
| `internal/match` | reduced role: criterion evaluation and the delete/grouped-stats residual | #464 "one evaluator": share traversal with the SPI kernel |
| `api/openapi.yaml` | hand-maintained; structurally drifts from the server (#369, dead operations; groups 1–4 reconciled) | generated or contract-tested spec (`docs/adr/0003-…`); `internal/oasdiffcheck` gates breaking changes meanwhile |
| Cluster/peer wire formats | pre-1.0: no backward-compat scaffolding is kept; collapse to the elegant form | — |
| `docs/ARCHITECTURE.md` §1 says `internal/match` is "consumed by memory plugin"; its contracts section says `AuditService` is "implemented by internal/domain/audit" | stale (plugins cannot import `internal/`; the wired `AuditService` is `internal/skeleton`'s no-op and nothing consumes it — the real trail is `internal/domain/audit` over the SPI audit store) | fix on next touch |
| `internal/skeleton` and `contract.AuditService` / `contract.ExternalProcessingService`'s no-op stand-ins | vestigial: wired or defined, consumed by nothing | delete (Gate 6), own change |
| `.claude/rules/ownership-mutability.md` `paths:` name `internal/spi/**` and `internal/persistence/**` | stale globs (directories do not exist) | point at `internal/domain/**` and `plugins/**` |
| `docs/plans/` referenced by `CLAUDE.md` and `documentation-hygiene.md` | empty; plans live in `docs/superpowers/plans/` | update the references |
| Frozen | `docs/superpowers/*`, `docs/audits/`, `docs/analysis/`, `docs/proposals/`, `docs/release-notes/`, `docs/adr/` (append-only) | never edited to reflect later truth |

Check: every "migrating" row names an issue or spec; every "frozen" tree is
excluded from "the doc says" arguments.

## 6. Doc routing — which doc answers which question

| Question | Read | Not |
|---|---|---|
| How do I run or configure it? | `README.md`; `cmd/cyoda/help/content/config/*.md` (env-var reference, one topic per area); `docs/ARCHITECTURE.md` §9 | `docs/PRD.md` |
| How do I build, test, release? | `CONTRIBUTING.md`; `Makefile` (`make test`, `make test-full`, `make race`, `make repin-plugins`); `.claude/rules/*.md`; `COMPATIBILITY.md` (cross-repo pins); SPI `MAINTAINING.md` (tag procedure) | — |
| How does subsystem X work today? | `docs/ARCHITECTURE.md` (reference, present tense); `docs/CONSISTENCY.md` (isolation); `docs/CONCURRENCY.md`; `docs/PROCESSOR_EXECUTION_MODES.md`; `docs/plugins/{IN_MEMORY,SQLITE,POSTGRES}.md`; `docs/workflow-schema-versioning.md` | any dated spec |
| What is the API contract? | `api/openapi.yaml` (source); `proto/`; `cmd/cyoda/help/content/*.md` (user-facing, and the spec artefacts `cyoda help openapi/grpc/cloudevents` emit); `cmd/cyoda/help/content/errors/<CODE>.md` (one per code, parity-enforced) | `docs/FEATURES.md` (inventory, not contract) |
| What must Cyoda Cloud mirror? | `docs/cloud-parity/*.md` (24 contracts plus the `README.md` index) | — |
| What does cyoda-go declare but not implement? | `docs/cyoda/cloud-divergences.md` (the inverse vector) | — |
| Why was X decided? | `docs/adr/` (3 records); `docs/ARCHITECTURE.md` §13; the dated spec in `docs/superpowers/specs/` for the reasoning **at that time** | — |
| What is the plugin contract? | `cyoda-go-spi/doc.go`; `docs/plugins.md` (authoring guide); `spitest/` (executable contract) | `internal/contract` (engine-internal) |
| What is being remediated, in what order? | the tracking issue (for search: Cyoda/cyoda-go#516) | assistant memory files |
| Archives | `docs/superpowers/` — 164 dated files (70 specs, 73 plans, 7 research, 13 reviews, 1 audit); `docs/audits/`, `docs/analysis/`, `docs/proposals/`, `docs/release-notes/` | treated as truth about the present |

Check: a question routed here lands on a living document; a dated file is
cited only for "why then", never for "how now".
