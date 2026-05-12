# README + OVERVIEW→FEATURES + Help-Topic DevX Refactor

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
- **OVERVIEW.md duplicates `docs/ARCHITECTURE.md` at lower fidelity.** ARCHITECTURE.md (1694 lines, v2.1, dated 2026-04-18, status "reconciled against commit at branch tip") is the maintained, version-tagged source of truth for system architects. OVERVIEW.md is undated, unversioned, and already drifted (it calls cyoda-go a "single-node Go digital twin" and omits the sqlite plugin from its Persistence section). Two architecture docs means only the one architects actually use stays correct.

## Goals

1. **README ≤ ~140 lines**, optimized for an evaluator's first 60 seconds, with a clean hand-off matrix.
2. **Every removed concept lands in a definite home** — `cyoda help` topic, `docs/ARCHITECTURE.md`, `docs/PRD.md`, `docs/FEATURES.md` (the renamed OVERVIEW), `docs/plugins.md`, or `MAINTAINING.md`. No information loss.
3. **`docs/ARCHITECTURE.md` becomes the canonical "Architecture" destination** referenced from the README. The legacy `OVERVIEW.md` is renamed to `docs/FEATURES.md` and shrunk to its non-overlapping role (feature inventory + REST/gRPC API surface tables); it stops claiming to be an architecture doc and the new filename advertises its actual scope.
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
  | Architecture                  | docs/ARCHITECTURE.md                              |
  | Product overview              | docs/PRD.md                                       |
  | Feature & API inventory       | docs/FEATURES.md                                  |
  | Multi-node cluster            | https://docs.cyoda.net/help/cluster (deep dive: docs/ARCHITECTURE.md §4) |
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
| Target Applications (20–27) | Already in `docs/PRD.md` §1 ("Target Applications") — no action |
| EDBMS Features 12-bullet list (29–44) | `docs/FEATURES.md` "Feature List" (already there in expanded form in the legacy OVERVIEW.md content; verify during shrink pass) |
| Documentation pointer (46–55) | Folded into "Where to go next" matrix |
| Requirements (57–61) | "From source" install subsection |
| Versioning (63–69) | README "Versioning" (kept, shortened) + maintenance-policy paragraph moves to `MAINTAINING.md` |
| Install (71–140) | README "Install" (kept, condensed by ~30%) |
| Local dev `run-local.sh` block (144–161) | "First real call" uses sqlite + jwt instead; the script remains documented in `cyoda help cli` |
| In-memory `go run` block (163–174) | "Try it in 30 seconds" covers the simplest path; deeper detail in `cyoda help quickstart` |
| Docker Compose block — broken (176–197) | **Deleted.** Replaced by routing-matrix link to `examples/compose-with-observability/` |
| Multi-node cluster diagram + routing-token paragraph (199–216) | Already in `docs/ARCHITECTURE.md` §4 ("Multi-Node Routing Architecture", 6 subsections including swimlane and partition analysis) + new `cyoda help cluster` topic for operator quick-reference |
| Storage backend matrix (218–228) | Replaced by README "Four engines" table; full detail in `cyoda help config.database` |
| SQLite quick config (230–238) | `cyoda help config.database` (already there — verify) |
| PostgreSQL quick config (240–246) | `cyoda help config.database` (already there — verify) |
| Writing a third-party plugin (248–268) | New `docs/plugins.md` (thin entry point — defers to `docs/ARCHITECTURE.md` §1 for the contract reference and `pkg.go.dev/.../cyoda-go-spi` for the API surface) |
| Scale Profile table (270–279) | Already in `docs/PRD.md` §1 ("Scale Profile", per-engine envelope) and `docs/ARCHITECTURE.md` §14 ("Non-Functional Limits and Design Boundaries") — no action |
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

### 3. OVERVIEW.md → docs/FEATURES.md — rename, relocate, drop the architecture half

The legacy `OVERVIEW.md` duplicates `docs/ARCHITECTURE.md` at lower fidelity. Rather than recon-and-extend (which preserves the duplication), the file is **renamed to `docs/FEATURES.md`** (the new name advertises its actual scope) and shrunk to its **non-overlapping** role: a flat feature-and-API-surface inventory that ARCHITECTURE.md does not provide.

**Move:** `git mv OVERVIEW.md docs/FEATURES.md` (preserves git history; relocates to `docs/` alongside ARCHITECTURE.md and PRD.md). Title becomes `# Cyoda-Go — Feature & API Surface Inventory`.

**New scope (kept):**
- One-paragraph re-scoped intro — explicitly states this is the feature & API inventory; points readers at `docs/ARCHITECTURE.md` for architecture and `docs/PRD.md` for product context.
- **Feature List** — ~80-bullet inventory across Entity / Models / Workflow / Search / Audit / Messaging / gRPC / Auth / Multi-Tenancy / Temporal / Pluggable Persistence. **Refresh:** add the sqlite plugin to "Pluggable Persistence" (the legacy file lists only memory + postgres).
- **REST API Surface** — one-row-per-area table.
- **gRPC API Surface** — `CloudEventsService` proto block.

**Removed (now lives in ARCHITECTURE.md):**
- "System Architecture" section + diagram (duplicates ARCHITECTURE §1)
- "Domain Modules" table (duplicates ARCHITECTURE §1 package layout)
- "Persistence" prose (duplicates ARCHITECTURE §2)
- "Multi-Tenancy" prose (duplicates ARCHITECTURE §1, §5)
- "Transactions" prose + table (duplicates ARCHITECTURE §3)
- "Workflow Engine" prose (duplicates ARCHITECTURE §5)
- "gRPC & Externalized Processing" prose (duplicates ARCHITECTURE §6)
- "Authentication" prose (duplicates ARCHITECTURE §7)
- "Error Handling" prose (duplicates ARCHITECTURE §8)
- "single-node Go digital twin" tagline (factually wrong since cluster work shipped — ARCHITECTURE §4)

**Verification of the kept content** (bounded — these are the only checks needed):
- The Feature List bullets match what is implemented today. Spot-check via `internal/domain/` package surface and the OpenAPI spec; remove any feature that was deprecated or never shipped, add any that landed since the last edit.
- The REST API Surface table matches the OpenAPI spec generated from `api/`. Update any drifted endpoint group.
- The gRPC API Surface proto block matches `proto/`.

**Inbound link updates** (do these in the same PR):
- `README.md:44` — currently `See [OVERVIEW.md](OVERVIEW.md)`. Removed by README rewrite (Phase 2.1) — no separate fix needed.
- `docs/proposals/2026-04-16-Cyoda_Mem_PRD_Final.md:8` — uses an absolute GitHub URL `.../blob/main/OVERVIEW.md`. Update to `.../blob/main/docs/FEATURES.md`. (This is a proposal doc, not a historical plan, so updating links is appropriate.)
- `docs/superpowers/specs/2026-04-16-provisioning-shared-design.md:210` — historical spec record, not living documentation. Per `.claude/rules/documentation-hygiene.md` ("`docs/plans/` — historical records, not living documents") and the same logic for completed specs, **do not edit**. The mention there refers to the OSS-metadata layout decision at the time of that spec.

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

### 6. New `docs/plugins.md` — thin entry point

Absorbs README lines 248–268. Target **~40 lines** (not 80) — `docs/ARCHITECTURE.md` §1 already contains the full Plugin Contract reference (interfaces, code blocks, blank-import pattern, package layout including each plugin's `doc.go` "reference example for plugin authors"). This file is the discoverable starting point that points readers at the right destinations; it does not re-state the contract.

Sections:
- **Audience.** Authors of out-of-tree storage plugins.
- **Where to read the contract.** `docs/ARCHITECTURE.md` §1 (Plugin Contract summary + SPI module surface) and the upstream `pkg.go.dev/github.com/cyoda-platform/cyoda-go-spi`.
- **Reference examples to fork.** `plugins/memory/doc.go`, `plugins/sqlite/doc.go`, `plugins/postgres/doc.go` (each is explicitly maintained as a "reference example for plugin authors", per ARCHITECTURE §1 package layout).
- **Custom-binary blank-import example.** Verbatim from current README lines 258–266.
- **Pin discipline.** SPI version in your plugin's `go.mod` must match the cyoda-go binary you compile into. Link to `MAINTAINING.md#bumping-cyoda-go-spi` and note the in-repo CI gate `check-spi-pin-sync` enforces this for in-tree plugins.
- **About the cassandra engine.** Cassandra is offered as a commercial backend by Cyoda; its source is not public. The SPI contract is the same — third-party plugins implement it the same way the open-source plugins do.

### 7. MAINTAINING.md addition

A short subsection under existing release-management content: **"Maintenance of older release lines."** Verbatim adoption of README lines 67–69 (no back-port branches; concrete need → open an issue → consider creating a maintenance branch).

## Implementation sequence

```
Phase 1 — Build destinations (no README touch)
  1.1  Add cmd/cyoda/help/content/admin.md
  1.2  Add cmd/cyoda/help/content/cluster.md
  1.3  Update cmd/cyoda/help/help_test.go (topLevelTopicsV061)
  1.4  Add docs/plugins.md (thin entry point)
  1.5  Add maintenance-policy subsection to MAINTAINING.md
  1.6  OVERVIEW.md → docs/FEATURES.md:
       a. git mv OVERVIEW.md docs/FEATURES.md (preserves history)
       b. Strip architecture sections; refresh Feature List
          (add sqlite to Pluggable Persistence) + REST/gRPC API Surface
       c. New title (# Cyoda-Go — Feature & API Surface Inventory)
          and intro paragraph re-scoping the doc and pointing readers
          at ARCHITECTURE.md / PRD.md
       d. Update inbound link in
          docs/proposals/2026-04-16-Cyoda_Mem_PRD_Final.md (GitHub URL)
  1.7  Help-topic gap audit:
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
       (cyoda help topic, ARCHITECTURE.md, PRD.md, FEATURES.md, plugins.md,
       or MAINTAINING.md)

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

- **Risk:** FEATURES.md reframe surfaces feature-list staleness (a feature shipped that's not listed, or vice versa).
  **Mitigation:** Bounded by the spot-check rules in §3 ("verification of the kept content"). If a gap is found, fix it inline. If a gap turns out to be a code-vs-docs drift requiring *code* changes (i.e. an undocumented behavior that should arguably be removed), surface it to the user (Gate 6) rather than silently documenting aspirational behavior.

- **Risk:** External inbound links to `github.com/.../blob/main/OVERVIEW.md` (blog posts, tweets, slides) 404 after rename.
  **Mitigation:** Acceptable per the user's "aggressive cuts" mandate. The two in-repo references are handled in §3 ("Inbound link updates"). External rot is a one-time hit, comparable in severity to the README anchor breakage already accepted.

- **Risk:** ARCHITECTURE.md itself drifts before this PR lands, making the README link stale.
  **Mitigation:** ARCHITECTURE.md is dated and version-tagged; if its date is older than ~3 months at PR-prep time, flag for the user — the README link is still correct in *direction*, but a separate ARCHITECTURE refresh may be warranted (out of scope here).

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
- `OVERVIEW.md` no longer exists at the repo root; `docs/FEATURES.md` exists and contains only Feature List + REST/gRPC API Surface, with an intro paragraph pointing to ARCHITECTURE.md / PRD.md
- `git log --follow docs/FEATURES.md` resolves history back through the rename to the original OVERVIEW.md commits
- `docs/FEATURES.md` "Feature List" includes the sqlite plugin under Pluggable Persistence
- README routing matrix's "Architecture" row points to `docs/ARCHITECTURE.md`; "Feature & API inventory" row points to `docs/FEATURES.md`
- `docs/proposals/2026-04-16-Cyoda_Mem_PRD_Final.md` GitHub URL updated to `docs/FEATURES.md`
- No external Go source files changed (this is a docs-only PR)
