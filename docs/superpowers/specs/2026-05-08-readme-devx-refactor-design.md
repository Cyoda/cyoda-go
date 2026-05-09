# README + OVERVIEW + Help-Topic DevX Refactor

**Date:** 2026-05-08
**Status:** Design (awaiting user review)
**Type:** Documentation refactor (no code changes)

## Problem

The current `README.md` is 545 lines and conflates four audiences and four content types:

1. **Evaluator-new-dev** wanting to understand the project and try it in 5 minutes
2. **Operator** deciding whether to deploy
3. **Integrator / app developer** wiring cyoda into a stack
4. **Contributor** to cyoda-go itself

Concretely, it suffers from:

- **Outdated content.** The Docker quickstart (lines 176–197) is explicitly marked "Temporarily unavailable."
- **Duplicated reference material.** ~265 lines of env-var tables and admin-endpoint reference duplicate `cyoda help config.*` and would belong in the canonical help system.
- **Buried value proposition.** The headline pitch is a long compound sentence. The "growth path" framing that docs.cyoda.net leads with is missing entirely.
- **No clear hand-off.** Sections of operational depth (multi-node cluster topology, security floors, JWKS bearer setup, admin endpoints) interrupt the evaluator's flow without giving operators a clean entry point either.
- **OVERVIEW.md is stale.** The user has flagged it as needing recon against current code before it can absorb depth-content evicted from README.

## Goals

1. **README ≤ ~140 lines**, optimized for an evaluator's first 60 seconds, with a clean hand-off matrix.
2. **Every removed concept lands in a definite home** — `cyoda help` topic, `OVERVIEW.md`, `docs/plugins.md`, or `MAINTAINING.md`. No information loss.
3. **OVERVIEW.md** is reconned against current code and extended to absorb evicted depth-content (use cases, cluster topology, scale).
4. **Two new help topics** (`admin`, `cluster`) eliminate the only gaps where evicted content has nowhere canonical to land.
5. **No transient broken cross-references** at any point in the implementation sequence.

## Non-goals

- Changing docs.cyoda.net (it auto-mirrors `cyoda help`).
- Refactoring existing `cyoda help config.*` topics. A gap audit is in scope; speculative restructuring is not.
- Touching install scripts, Helm chart, compose examples, or any non-doc Go source.
- Refactoring `CONTRIBUTING.md`, `SECURITY.md`, or `MAINTAINING.md` beyond the targeted additions called out below.

## Audience decision

**Primary:** Evaluator / new dev landing on the GitHub project page. The first screen must answer "what is this, why should I care, and how do I try it" in under 60 seconds.

**Secondary:** Operators, integrators, contributors, and SPI plugin authors. They are routed via a "Where to go next" matrix to deeper docs — they are not first-class consumers of the README body.

## Design

### 1. README skeleton — target ~132 lines

```
(badge row)                                                          (5 lines)

# cyoda-go                                                           (8 lines)
  **One transactional runtime for the entity lifecycle.**

  cyoda-go is an EDBMS — state machine, processors, and full revision
  history live inside the record, committed atomically. Minimizes the
  need for sagas, CDC pipelines, and external orchestration.

## Four storage engines, one application contract                   (14 lines)
  Same application code, four operational shapes:

  | Engine     | Where it fits                                 | Availability       |
  |------------|-----------------------------------------------|--------------------|
  | memory     | Local dev, unit tests, digital-twin scenarios | open source        |
  | sqlite     | Edge, single-node self-host, persistent dev   | open source        |
  | postgres   | Production transactional workloads, HA        | open source        |
  | cassandra  | Distributed scale, high write throughput      | commercial (Cyoda) |

  Switch by setting `CYODA_STORAGE_BACKEND` — no code changes.
  The cassandra engine is offered as a commercial backend by Cyoda for
  workloads that outgrow a single PostgreSQL primary; contact info in
  the cyoda.com website footer.

## Try it in 30 seconds                                              (12 lines)
  brew install cyoda-platform/cyoda-go/cyoda
  cyoda init && cyoda &
  curl http://localhost:8080/api/health
  # {"status":"UP"}
  → Mock auth, sqlite persistence at ~/.local/share/cyoda/cyoda.db.
    See "Install" for other OSes; "First real call" for jwt + entity creation.

## Install                                                           (35 lines)
  ### Homebrew (macOS / Linux)
  ### curl (any Unix)
  ### Debian / Ubuntu / Fedora / RHEL
  ### From source

## First real call                                                   (25 lines)
  sqlite + jwt mode, end-to-end:
    1. cyoda init
    2. export CYODA_IAM_MODE=jwt
       export CYODA_JWT_SIGNING_KEY_FILE=...
       export CYODA_BOOTSTRAP_CLIENT_ID=...
       export CYODA_BOOTSTRAP_CLIENT_SECRET=...
    3. cyoda &
    4. TOKEN=$(curl ... oauth/token ...)
    5. curl -H "Authorization: Bearer $TOKEN" ... POST /api/entity/...

## Where to go next                                                  (22 lines)
  Online docs mirror `cyoda help` — the same content is available offline
  via `cyoda help <topic>`.

  | Goal                          | Link                                              |
  |-------------------------------|---------------------------------------------------|
  | Build an app                  | https://docs.cyoda.net/help/quickstart            |
  | Configure                     | https://docs.cyoda.net/help/config                |
  | Error reference               | https://docs.cyoda.net/help/errors                |
  | Deploy with Helm              | https://docs.cyoda.net/help/helm                  |
  | Deploy with Docker Compose    | examples/compose-with-observability/              |
  | Architecture                  | OVERVIEW.md                                       |
  | Multi-node cluster            | https://docs.cyoda.net/help/cluster               |
  | Admin endpoints (log/trace)   | https://docs.cyoda.net/help/admin                 |
  | Write a storage plugin        | docs/plugins.md                                   |
  | Contribute                    | CONTRIBUTING.md                                   |
  | Security disclosures          | SECURITY.md                                       |

## Versioning                                                         (8 lines)
  Pre-1.0. Minor bumps may break wire/config; patches don't.
  Breaking changes: CHANGELOG.md.
  Maintenance policy for older release lines: MAINTAINING.md.

## License                                                            (3 lines)
  Apache-2.0. See LICENSE.
```

### 2. Destination map — every evicted concept

In the table below, **"verify"** means: confirm during Phase 1.8 that the destination already contains equivalent content. If a gap is found, fill it in the same PR (bounded — see risk section).

| Currently in README (line range) | Destination |
|---|---|
| Three-mode pitch (5–19) | Compressed into pitch + "Four engines" table; depth in `cyoda help config.database` |
| Target Applications (20–27) | New `OVERVIEW.md#use-cases` section |
| EDBMS Features 12-bullet list (29–44) | Already in OVERVIEW.md (verify during recon) |
| Documentation pointer (46–55) | Folded into "Where to go next" matrix |
| Requirements (57–61) | "From source" install subsection |
| Versioning (63–69) | README "Versioning" (kept, shortened) + maintenance-policy paragraph moves to `MAINTAINING.md` |
| Install (71–140) | README "Install" (kept, condensed by ~30%) |
| Local dev `run-local.sh` block (144–161) | "First real call" uses sqlite + jwt instead; the script remains documented in `cyoda help cli` |
| In-memory `go run` block (163–174) | "Try it in 30 seconds" covers the simplest path; deeper detail in `cyoda help quickstart` |
| Docker Compose block — broken (176–197) | **Deleted.** Replaced by routing-matrix link to `examples/compose-with-observability/` |
| Multi-node cluster diagram + routing-token paragraph (199–216) | New `OVERVIEW.md#cluster-topology` + new `cyoda help cluster` topic |
| Storage backend matrix (218–228) | Replaced by README "Four engines" table; full detail in `cyoda help config.database` |
| SQLite quick config (230–238) | `cyoda help config.database` (already there — verify) |
| PostgreSQL quick config (240–246) | `cyoda help config.database` (already there — verify) |
| Writing a third-party plugin (248–268) | New `docs/plugins.md` |
| Scale Profile table (270–279) | New `OVERVIEW.md#scale` section |
| Configuration top + sources + subcommands + profiles (281–329) | `cyoda help config` + `cyoda help cli` (verify; bridge gaps) |
| Server env vars table (332–344) | `cyoda help config.server` (verify) |
| Authentication env vars table (346–356) | `cyoda help config.auth` (verify) |
| `_FILE` suffix doc (358–375) | `cyoda help config` (verify; add if missing) |
| Bootstrap env vars table (377–384) | `cyoda help config.auth` (verify) |
| Schema extension log table (386–391) | `cyoda help config.database` (verify) |
| SQLite env vars table (393–401) | `cyoda help config.database` (verify) |
| PostgreSQL env vars table (403–410) | `cyoda help config.database` (verify) |
| gRPC env vars table (412–417) | `cyoda help config.grpc` (verify; add if missing) |
| Observability env vars table (419–426) | `cyoda help config.observability` or `cyoda help telemetry` (verify) |
| Admin / Observability Listener (428–449) | `cyoda help config.observability` (verify; bridge gaps) |
| Security floor + mock-auth banner (451–459) | `cyoda help config.auth` (verify; add if missing) |
| Admin Endpoints — log-level (461–480) | New `cyoda help admin` topic |
| Admin Endpoints — trace-sampler (482–545) | New `cyoda help admin` topic |

### 3. OVERVIEW.md recon plan

OVERVIEW.md is 231 lines and explicitly flagged as "quite outdated." Two-phase pass:

**Audit pass — verify each claim against current code:**

| OVERVIEW claim | Verify against |
|---|---|
| Modular monolith, DDD boundaries | Top-level package layout under `internal/` |
| Auth + Recovery middleware (RFC 9457) | `internal/middleware/`, `internal/api/errors/` |
| Domain modules table (Entity / Model / Workflow / Search / Audit / Messaging / Auth / gRPC / Cluster) | One package per row exists; responsibilities match |
| SPI Layer interfaces | Current `cyoda-go-spi` interface set |
| In-Memory Store + PostgreSQL Store (SI+FCW: RR + FCW) | `internal/store/postgres/`, `plugins/memory/`, `plugins/postgres/`, `plugins/sqlite/` (sqlite missing from current OVERVIEW diagram) |
| 13 audit event types | `internal/audit/` event-type enum |
| OBO token exchange (RFC 8693) | `internal/auth/` |
| Bidirectional gRPC streaming for processors/criteria via CloudEvents | `internal/grpc/`, proto definitions |

**Extension pass — absorb evicted depth-content:**

- New `## Use Cases` section (~25 lines) — financial ledgers, OMS, regulatory compliance, digital-twin orchestration. Source: README lines 20–27, expanded with one-paragraph elaborations.
- New `## Cluster Topology` section (~50 lines) — diagram from README lines 201–211, gossip/SWIM, txID routing-token, owner-node failure semantics. Sourced from current `internal/cluster/` code (recon) plus README lines 213–216.
- New `## Scale` section (~20 lines) — sweet-spot vs upper-bound table from README lines 270–279, plus a short paragraph on when cassandra is the right escape hatch.
- The existing "Persistence" section drops backend-specific config tables and keeps the SPI contract diagram only.

### 4. New help topic: `cyoda help admin`

Path: `cmd/cyoda/help/content/admin.md` (~80 lines).

Structure (per the conventions in `CONTRIBUTING.md#help-topic-tree`):

```
---
topic: admin
title: "cyoda admin — runtime-switchable admin endpoints"
stability: stable
see_also:
  - run
  - telemetry
  - config.auth
---

# admin

## NAME
admin — runtime-switchable controls exposed on the main API listener.

## SYNOPSIS
GET/POST /api/admin/log-level
GET/POST /api/admin/trace-sampler

## DESCRIPTION
Both endpoints require ROLE_ADMIN on the JWT and update process-local state
atomically. Multi-node deployments must hit each node's endpoint separately.

## ENDPOINTS

### /api/admin/log-level
  GET  → {"level": "<level>"}
  POST → {"level": "debug"|"info"|"warn"|"error"}

### /api/admin/trace-sampler
  GET  → {"sampler": "<sampler>", "ratio": <float>, "parent_based": <bool>}
  POST → same shape; sampler ∈ {always, never, ratio}; ratio ∈ [0, 1]

## EXAMPLES
(full curl invocations from current README lines 475–516)

## NOTES
- parent_based interaction with upstream sampling decisions
  (verbatim from current README lines 521–534)
- Initial sampler seeded from OTEL_TRACES_SAMPLER / OTEL_TRACES_SAMPLER_ARG
  (verbatim from current README lines 536–541)
- Process-local: each node has its own sampler/level
  (verbatim from current README lines 543–545)

## SEE ALSO
- run — server lifecycle
- telemetry — OpenTelemetry exporters and metrics
- config.auth — JWT and ROLE_ADMIN
```

Registration: add `admin` to `topLevelTopicsV061` in `cmd/cyoda/help/help_test.go` per `CONTRIBUTING.md#additions`.

### 5. New help topic: `cyoda help cluster`

Path: `cmd/cyoda/help/content/cluster.md` (~120 lines).

Structure:

```
---
topic: cluster
title: "cyoda cluster — multi-node topology and operations"
stability: stable
see_also:
  - config.database
  - run
---

# cluster

## NAME
cluster — multi-node cyoda-go topology, discovery, and transaction routing.

## SYNOPSIS
3–10 stateless nodes behind a load balancer, sharing one PostgreSQL.

## DESCRIPTION
All nodes are stateless and identical: no leader election, no shard
ownership, no external service-discovery infrastructure. Postgres is
the single coordination layer. SI+FCW (REPEATABLE READ + first-
committer-wins) provides correctness; gossip provides peer awareness;
signed routing tokens bind in-flight pgx.Tx handles to their owning
node.

## TOPOLOGY
(diagram from current README lines 201–211)

## DISCOVERY
SWIM gossip protocol — verify exact env vars and defaults against
internal/cluster/ during recon.

## TRANSACTION ROUTING
When a node begins a PostgreSQL transaction, it generates a signed
routing token encoding which node owns the pgx.Tx handle. Subsequent
requests carrying that token are routed to the owning node. If the
owner dies, PostgreSQL auto-rolls back the connection and the client
retries from scratch — fail-closed semantics, no orphaned transactions.

## ENV VARS
(table — to be enumerated from current code during implementation;
gossip port, peer hints, routing-token signing key, etc.)

## OPERATIONS
- Growing the cluster: how new nodes join the gossip mesh
- Shrinking: graceful drain semantics
- /readyz signals during a rolling restart
- What clients see when an owner-node dies mid-transaction

## SEE ALSO
- config.database — postgres backend (only multi-node-capable engine)
- run — server lifecycle
```

Registration: add `cluster` to `topLevelTopicsV061`.

### 6. New `docs/plugins.md`

Absorbs README lines 248–268 plus expansion. Target ~80 lines.

Sections:
- **Why a plugin?** When external storage justifies a plugin. The commercial cassandra engine is referenced here only as motivation for the SPI's existence — its source is not public, so this doc describes the SPI contract that any storage backend must satisfy, not a recipe for cloning a specific implementation.
- **Dependency rule.** Only `cyoda-go-spi`. SPI is stdlib-only at the surface.
- **Required interfaces.** `spi.Plugin`, `spi.DescribablePlugin`, `spi.StoreFactory`, `spi.TransactionManager`, optional `spi.Startable`. One-line contract per method.
- **Reference implementations.** `plugins/memory/` (simplest), `plugins/postgres/` (txID-bridge pattern). These are the canonical, public references — readers wanting to see a working out-of-tree plugin should fork from `plugins/sqlite/` or `plugins/postgres/`.
- **Custom-binary blank-import example.** Verbatim from current README lines 258–266.
- **Pin discipline.** Link to `MAINTAINING.md#bumping-cyoda-go-spi`. The CI gate `check-spi-pin-sync` is mentioned here too.

### 7. MAINTAINING.md addition

A short subsection under existing release-management content: **"Maintenance of older release lines."** Verbatim adoption of README lines 67–69 (no back-port branches; concrete need → open an issue → consider creating a maintenance branch).

## Implementation sequence

```
Phase 1 — Build destinations (no README touch)
  1.1  Add cmd/cyoda/help/content/admin.md
  1.2  Add cmd/cyoda/help/content/cluster.md
  1.3  Update cmd/cyoda/help/help_test.go (topLevelTopicsV061)
  1.4  Add docs/plugins.md
  1.5  Add maintenance-policy subsection to MAINTAINING.md
  1.6  OVERVIEW.md audit pass (verify each claim against code)
  1.7  OVERVIEW.md extension pass (Use Cases, Cluster Topology, Scale)
  1.8  Help-topic gap audit:
       - For every "verify" row in the destination map, confirm the
         content already lives in the named cyoda help topic. If a
         gap is found, file or fix in the same PR. Note: do not
         restructure topics speculatively.

Phase 2 — README cut
  2.1  Rewrite README to skeleton above
  2.2  Manual link check: every link resolves (markdown link checker
       + click-through to docs.cyoda.net for help-topic links)
  2.3  Destination-map audit: grep the old README for every distinctive
       phrase moved (e.g. "txID-to-physical-handle bridge", "SI+FCW",
       "Bootstrap M2M client") and confirm it lives in the destination

Phase 3 — Verify hygiene
  3.1  go test ./cmd/cyoda/... (help-tree tests)
  3.2  Manual: cyoda help admin / cyoda help cluster render correctly
  3.3  Manual: cyoda help and the topic index include the new entries
  3.4  Verify CLAUDE.md "documentation hygiene" rule (Gate 4) still
       reflects reality; tweak only if README changes invalidate the
       wording (probably no change needed)
```

## Risks and mitigations

- **Risk:** External links (blog posts, docs sites) reference README anchors that will disappear.
  **Mitigation:** Aggressive cuts are sanctioned by user. Anchor breakage is acceptable; link breakage to docs.cyoda.net is not (it's the canonical replacement). The link-check in Phase 2.2 is binding.

- **Risk:** OVERVIEW recon surfaces architectural drift larger than a doc fix can absorb.
  **Mitigation:** OVERVIEW recon is bounded — verify-and-extend, not rewrite. If the recon surfaces a code-vs-docs drift that requires *code* changes to resolve, surface it to the user (Gate 6: "stop and surface the choice"); do not silently rewrite OVERVIEW to match aspirational architecture.

- **Risk:** Help-topic gap audit (1.8) balloons.
  **Mitigation:** Audit is bounded to the destination-map rows. Each row is verify-or-add — no speculative restructuring. If a row's gap exceeds ~30 lines of new content, surface it as a follow-up rather than bundling.

- **Risk:** New `cluster` help topic requires recon depth that turns into its own work-stream.
  **Mitigation:** Topic is registered with `stability: evolving` if recon reveals the routing-token / gossip surface is itself in flux. The README routing-matrix link still resolves; depth fills in over subsequent commits.

## Verification — definition of done

- README is ≤ 150 lines and follows the skeleton above
- Every line in the destination map resolves to a real, populated destination
- `go test ./cmd/cyoda/...` is green (help-tree tests pass with new topics)
- `cyoda help admin` and `cyoda help cluster` render
- Manual: every link in the README resolves
- OVERVIEW.md no longer contains claims contradicted by current code
- No external Go source files changed (this is a docs-only PR)
