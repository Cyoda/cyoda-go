# README + OVERVIEW→FEATURES + Help-Topic DevX Refactor — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Cut README.md from 545 → ~140 lines (evaluator-first landing page); collapse OVERVIEW.md into `docs/FEATURES.md` (features + API surface only, with architecture sections deleted because `docs/ARCHITECTURE.md` is the canonical source); add two new help topics (`admin`, `cluster`); add a thin `docs/plugins.md` entry point and a `MAINTAINING.md` maintenance-policy section.

**Architecture:** Build all destinations first, rewrite README last. Every concept removed from README has a definite home (cyoda help topic, ARCHITECTURE.md, PRD.md, FEATURES.md, plugins.md, MAINTAINING.md). No transient broken cross-refs at any commit.

**Tech Stack:** Go (for help-system tests), Markdown (for content), Bash + git for orchestration. The help system enforces a markdown subset — no tables, no nested lists, no HTML, no blockquotes (`cmd/cyoda/help/renderer/linter.go`). Help-content tests live in `cmd/cyoda/help/help_test.go`.

**Spec:** `docs/superpowers/specs/2026-05-08-readme-devx-refactor-design.md`.

---

## File Structure

**Created:**
- `cmd/cyoda/help/content/admin.md` — new top-level help topic (log-level + trace-sampler runtime endpoints)
- `cmd/cyoda/help/content/cluster.md` — new top-level help topic (multi-node topology, gossip, txID routing)
- `docs/plugins.md` — thin entry point for SPI plugin authors (~40 lines, defers to ARCHITECTURE §1)
- `docs/FEATURES.md` — renamed from `OVERVIEW.md`; shrunk to feature & API surface inventory only

**Modified:**
- `cmd/cyoda/help/help_test.go` — add `admin` and `cluster` to `topLevelTopicsV061`
- `MAINTAINING.md` — add `## Maintenance of older release lines` section
- `README.md` — full rewrite to evaluator-first ~140-line skeleton
- `docs/proposals/2026-04-16-Cyoda_Mem_PRD_Final.md:8` — update GitHub URL from OVERVIEW.md to docs/FEATURES.md

**Deleted (via rename):**
- `OVERVIEW.md` (becomes `docs/FEATURES.md` via `git mv`, preserves history)

---

## Task 1: Add `cyoda help admin` topic (TDD)

**Files:**
- Create: `cmd/cyoda/help/content/admin.md`
- Modify: `cmd/cyoda/help/help_test.go` (line ~422, `topLevelTopicsV061`)

- [ ] **Step 1: Add `admin` to the topLevelTopicsV061 list (RED)**

Edit `cmd/cyoda/help/help_test.go` lines 422–426:

```go
var topLevelTopicsV061 = []string{
	"cli", "config", "errors", "crud", "search", "analytics",
	"models", "workflows", "run", "helm", "telemetry",
	"openapi", "grpc", "quickstart", "admin",
}
```

- [ ] **Step 2: Run the test to confirm it fails**

Run: `go test ./cmd/cyoda/help/ -run TestAllTopLevelTopicsPresent -v`

Expected: FAIL with `top-level topic "admin" missing from embedded content`.

- [ ] **Step 3: Create the `admin.md` content file (GREEN)**

Create `cmd/cyoda/help/content/admin.md` with these contents (note: the markdown linter forbids tables, nested lists, HTML, and blockquotes — use flat bullets only):

````markdown
---
topic: admin
title: "admin — runtime-switchable admin endpoints"
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

```
GET  /api/admin/log-level
POST /api/admin/log-level
GET  /api/admin/trace-sampler
POST /api/admin/trace-sampler
```

## DESCRIPTION

Both endpoint families require `ROLE_ADMIN` on the JWT and update process-local state atomically. State is **not** propagated across nodes; multi-node deployments must hit each node's endpoint separately, the same as `/api/admin/log-level` has always behaved.

## ENDPOINTS

### log-level

`GET /api/admin/log-level` returns the current effective log level as JSON:

```
{"level": "info"}
```

`POST /api/admin/log-level` changes the level atomically. Body shape mirrors the GET response. Valid values: `debug`, `info`, `warn`, `error`.

### trace-sampler

`GET /api/admin/trace-sampler` returns the current OpenTelemetry sampler configuration:

```
{"sampler": "ratio", "ratio": 0.1, "parent_based": true}
```

`POST /api/admin/trace-sampler` changes the sampler atomically. Body shape mirrors the GET response. Valid `sampler` values: `always`, `never`, `ratio`. When `sampler` is `ratio`, `ratio` must be a float in `[0, 1]`.

## EXAMPLES

```
# Read current log level
curl -H "Authorization: Bearer $TOKEN" \
  http://localhost:8080/api/admin/log-level

# Switch to debug
curl -X POST -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"level":"debug"}' \
  http://localhost:8080/api/admin/log-level

# Sample 10% of traces
curl -X POST -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"sampler":"ratio","ratio":0.1}' \
  http://localhost:8080/api/admin/trace-sampler

# Force 100% sampling on this node regardless of upstream
curl -X POST -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"sampler":"always","parent_based":false}' \
  http://localhost:8080/api/admin/trace-sampler

# Disable tracing
curl -X POST -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"sampler":"never"}' \
  http://localhost:8080/api/admin/trace-sampler
```

## NOTES

- `parent_based` defaults to `true` and respects upstream sampling decisions in the `traceparent` header. With `parent_based: true`, an upstream "do not sample" overrides this node's `sampler: always`. This is standard OpenTelemetry `ParentBased` semantics and is usually correct for distributed-trace integrity. Set `parent_based: false` to override.
- The initial sampler at process start is seeded from the standard OpenTelemetry env vars `OTEL_TRACES_SAMPLER` and `OTEL_TRACES_SAMPLER_ARG`. Supported values are the six standard combinations from the OTel spec (`always_on`, `always_off`, `traceidratio`, and their `parentbased_` variants). The admin endpoint is a runtime override, not a replacement.
- Sampler and log level are process-local. Each node has its own state; multi-node deployments need to hit each node's admin endpoint separately.

## SEE ALSO

- `run` — server lifecycle
- `telemetry` — OpenTelemetry exporters and metrics
- `config.auth` — JWT and `ROLE_ADMIN`
````

- [ ] **Step 4: Run the test to confirm it passes**

Run: `go test ./cmd/cyoda/help/ -run TestAllTopLevelTopicsPresent -v`

Expected: PASS.

- [ ] **Step 5: Run the linter and see-also tests**

Run: `go test ./cmd/cyoda/help/ -run 'TestContentMarkdownSubsetLinter|TestSeeAlsoResolution' -v`

Expected: PASS. If `TestContentMarkdownSubsetLinter` reports an issue in `admin.md`, the most likely cause is an accidental table or nested list — recheck the content above is verbatim.

- [ ] **Step 6: Run all help tests as a regression check**

Run: `go test ./cmd/cyoda/help/... -v`

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add cmd/cyoda/help/content/admin.md cmd/cyoda/help/help_test.go
git commit -m "$(cat <<'EOF'
feat(help): add admin topic for runtime log-level and trace-sampler endpoints

Centralizes the two runtime-switchable admin endpoints (log-level and
trace-sampler) into a single help topic. Mirrors content from the
existing README admin-endpoints section, which will be removed in a
later commit. Registered in topLevelTopicsV061.
EOF
)"
```

---

## Task 2: Add `cyoda help cluster` topic (TDD)

**Files:**
- Create: `cmd/cyoda/help/content/cluster.md`
- Modify: `cmd/cyoda/help/help_test.go` (`topLevelTopicsV061`)

The cluster topic is purely narrative — all cluster env vars (`CYODA_CLUSTER_ENABLED`, `CYODA_HMAC_SECRET`, `CYODA_STARTUP_TIMEOUT`) are already documented in `cmd/cyoda/help/content/config.md` and `config/auth.md`, so this topic does not introduce new env vars and does not require updates to `TestConfig_EnvVarCoverage`.

- [ ] **Step 1: Add `cluster` to the topLevelTopicsV061 list (RED)**

Edit `cmd/cyoda/help/help_test.go`. The list now reads (note: include both `admin` from Task 1 and `cluster`):

```go
var topLevelTopicsV061 = []string{
	"cli", "config", "errors", "crud", "search", "analytics",
	"models", "workflows", "run", "helm", "telemetry",
	"openapi", "grpc", "quickstart", "admin", "cluster",
}
```

- [ ] **Step 2: Run the test to confirm it fails**

Run: `go test ./cmd/cyoda/help/ -run TestAllTopLevelTopicsPresent -v`

Expected: FAIL with `top-level topic "cluster" missing from embedded content`.

- [ ] **Step 3: Create the `cluster.md` content file (GREEN)**

Create `cmd/cyoda/help/content/cluster.md`:

````markdown
---
topic: cluster
title: "cluster — multi-node topology and operations"
stability: stable
see_also:
  - config.database
  - config.auth
  - run
  - quickstart
  - helm
---

# cluster

## NAME

cluster — multi-node cyoda topology, peer discovery, and transaction routing.

## SYNOPSIS

```
3–10 stateless cyoda nodes
       │
       ▼ load balancer (HTTP + gRPC)
       │
       ▼ shared PostgreSQL (single primary)
```

## DESCRIPTION

Multi-node cyoda is supported only on the `postgres` storage backend. All nodes are stateless and identical: no leader election, no shard ownership, no external service-discovery infrastructure. PostgreSQL is the single coordination layer. Snapshot Isolation with first-committer-wins (`REPEATABLE READ` + commit-time validation) provides correctness; gossip provides peer awareness; HMAC-signed routing tokens bind in-flight `pgx.Tx` handles to their owning node.

## TOPOLOGY

Any node can serve any HTTP or gRPC request. The load balancer does not need session affinity for stateless requests. For requests carrying a transaction-routing token (see `TRANSACTION ROUTING`), the in-process proxy forwards to the node that owns the transaction.

PostgreSQL is the only stateful component. Cluster size is bounded below by quorum-free PostgreSQL replication (typically 3 cyoda nodes minimum for HA) and above by PostgreSQL connection-pool capacity (typically 10 nodes maximum).

## DISCOVERY

Peer discovery uses SWIM gossip (HashiCorp `memberlist`). Cluster membership is eventually consistent across nodes. New nodes join via a seed-list — at least one peer's `host:gossip_port`. Nodes leave gracefully on SIGTERM and are evicted by gossip after a configurable suspect-then-confirm timeout if they crash.

The gossip protocol is operationally invisible — there are no per-message logs at INFO level. Membership-change events surface as `slog` debug-level entries.

## TRANSACTION ROUTING

PostgreSQL transactions are bound to the connection that begins them (`pgx.Tx` is single-owner). When a node begins a transaction, it generates a routing token containing the transaction ID and the node's identity, signed with `CYODA_HMAC_SECRET`. The token is returned to the client (typically as a header) and replayed on subsequent requests against that transaction.

The HTTP and gRPC frontends inspect the token, verify the HMAC, and either handle the request locally (token's owner is this node) or proxy to the owning node. If the owning node has died since the token was issued, PostgreSQL has already aborted the connection's transaction; the proxy returns a `409 Conflict` and the client retries from scratch — fail-closed semantics, no orphaned transactions.

`CYODA_HMAC_SECRET` is a deployment secret. All nodes in a cluster must share the same value.

## OPERATIONS

- **Growing the cluster.** Start a new node with the same `CYODA_HMAC_SECRET` and a seed-list pointing at any existing node. Gossip propagates membership within seconds. The load balancer's health checks (against `/readyz`) decide when to send traffic.
- **Shrinking the cluster.** Send SIGTERM. The node finishes in-flight requests, declares itself dead via gossip, and exits. Outstanding transactions owned by the departing node abort cleanly via PostgreSQL connection close.
- **Rolling restart.** Restart one node at a time, waiting for `/readyz` to report ready before moving on. Transactions in flight on the restarting node abort; clients retry.
- **Network partitions.** A node partitioned from PostgreSQL fails its own `/readyz` and is removed from the load balancer. A node partitioned from peers but still reachable from PostgreSQL continues to serve requests; gossip-level membership is best-effort and does not gate request handling. The full partition analysis (5 phases, dispatch and CRUD-callback paths) is in `docs/ARCHITECTURE.md` §4.5.

## SEE ALSO

- `config.database` — PostgreSQL is the only multi-node-capable backend
- `config.auth` — `CYODA_HMAC_SECRET` configuration
- `run` — server lifecycle
- `quickstart` — first-run defaults
- `helm` — Kubernetes deployment of multi-node clusters
````

- [ ] **Step 4: Run the test to confirm it passes**

Run: `go test ./cmd/cyoda/help/ -run TestAllTopLevelTopicsPresent -v`

Expected: PASS.

- [ ] **Step 5: Run the linter and see-also tests**

Run: `go test ./cmd/cyoda/help/ -run 'TestContentMarkdownSubsetLinter|TestSeeAlsoResolution' -v`

Expected: PASS. The five `see_also` references (`config.database`, `config.auth`, `run`, `quickstart`, `helm`) all exist as topic files in `cmd/cyoda/help/content/`.

- [ ] **Step 6: Run all help tests**

Run: `go test ./cmd/cyoda/help/... -v`

Expected: PASS, including `TestConfig_EnvVarCoverage` (no new env vars introduced).

- [ ] **Step 7: Commit**

```bash
git add cmd/cyoda/help/content/cluster.md cmd/cyoda/help/help_test.go
git commit -m "$(cat <<'EOF'
feat(help): add cluster topic for multi-node topology and operations

Operator-facing narrative for cyoda's multi-node mode: stateless
postgres-backed cluster, SWIM gossip discovery, HMAC-signed transaction
routing tokens, growth/shrink/rolling-restart procedure, partition
behavior. Defers the full network-partition phase analysis to
docs/ARCHITECTURE.md §4.5.

No new env vars introduced; CYODA_CLUSTER_ENABLED, CYODA_HMAC_SECRET,
and CYODA_STARTUP_TIMEOUT are already documented in config.md and
config/auth.md.
EOF
)"
```

---

## Task 3: Add `docs/plugins.md` thin entry point

**Files:**
- Create: `docs/plugins.md`

This is a pure-documentation task — no test surface. The file is a discoverable starting point that defers to `docs/ARCHITECTURE.md` §1 (Plugin Contract summary, SPI module surface) for the full contract reference.

- [ ] **Step 1: Create `docs/plugins.md`**

````markdown
# Writing a Storage Plugin

This is the entry point for authors of out-of-tree cyoda storage plugins. The complete contract reference lives in `docs/ARCHITECTURE.md` §1 ("Plugin Contract summary" and "The cyoda-go-spi Module") and on `pkg.go.dev` for the SPI module itself; this file points you at the right destinations.

## Audience

You are writing a Go module that plugs into a custom cyoda binary as a new storage backend (alongside or instead of the stock `memory`, `sqlite`, and `postgres` plugins).

## Where the contract lives

- `docs/ARCHITECTURE.md` §1 — the SPI surface, the `Plugin` / `DescribablePlugin` / `Startable` / `StoreFactory` / `TransactionManager` interfaces, and how the binary resolves plugins via `spi.GetPlugin`.
- `pkg.go.dev/github.com/cyoda-platform/cyoda-go-spi` — the API documentation for the SPI module itself. The SPI is stdlib-only by design; depending on it does not pull in transitive dependencies.

## Reference implementations to fork

The three in-tree plugins each ship with a `doc.go` explicitly maintained as a reference for plugin authors:

- `plugins/memory/doc.go` — simplest implementation; in-process SI+FCW with `sync.RWMutex`. Read this first.
- `plugins/sqlite/doc.go` — single-file persistent storage with embedded SQL migrations and a JSON predicate planner.
- `plugins/postgres/doc.go` — production-grade persistent storage with the `txID`-to-`pgx.Tx` bridge pattern for multi-node transaction routing.

The Cassandra storage backend offered as a commercial product by Cyoda implements the same SPI contract; its source is not public, but no hidden interfaces are involved — every plugin uses the same surface as the in-tree examples.

## Custom binary

Your binary's `main.go` blank-imports the plugins it ships:

```go
package main

import (
    _ "github.com/cyoda-platform/cyoda-go/plugins/memory"
    _ "github.com/cyoda-platform/cyoda-go/plugins/postgres"
    _ "github.com/cyoda-platform/cyoda-go/plugins/sqlite"
    _ "example.com/your-org/your-plugin"

    "github.com/cyoda-platform/cyoda-go/cmd/cyoda/cyoda"
)

func main() { cyoda.Main() }
```

Selecting your plugin at runtime is then `CYODA_STORAGE_BACKEND=your-plugin-name`, where the name is whatever your `Plugin.Name()` method returns.

## SPI version pin discipline

Your plugin's `go.mod` must pin the same `cyoda-go-spi` version as the cyoda-go binary you compile into. If they diverge, your plugin will not satisfy the interfaces the binary expects.

When you bump `cyoda-go-spi` in your plugin, bump it identically in the cyoda-go binary's `go.mod` in the same release. The cyoda-go repository's CI gate `check-spi-pin-sync` enforces this rule for in-tree plugins; out-of-tree plugins follow the same convention. See `MAINTAINING.md` (section "Bumping cyoda-go-spi") for the full procedure.
````

- [ ] **Step 2: Verify the file is well-formed Markdown**

Run: `cat docs/plugins.md | head -5`

Expected: shows the `# Writing a Storage Plugin` heading and intro paragraph. (No structural test — this is a docs-site Markdown file, not subject to the help-content linter.)

- [ ] **Step 3: Commit**

```bash
git add docs/plugins.md
git commit -m "$(cat <<'EOF'
docs: add docs/plugins.md as thin entry point for SPI plugin authors

Discoverable starting point that points readers at docs/ARCHITECTURE.md
§1 (the contract reference) and the in-tree plugins/*/doc.go reference
examples. Deliberately thin (~40 lines) — the contract itself is
already exhaustively documented in ARCHITECTURE and pkg.go.dev for
cyoda-go-spi; this file just makes the entry point findable.
EOF
)"
```

---

## Task 4: Add `## Maintenance of older release lines` section to `MAINTAINING.md`

**Files:**
- Modify: `MAINTAINING.md`

`MAINTAINING.md` currently has no policy statement on older release lines. Add a section after `## Pre-release testing` (line 403) and before `## Gotcha: snapshot-testing from a local clone` (line 430).

- [ ] **Step 1: Read the file to find exact insertion point**

Run: `grep -n '^## ' MAINTAINING.md | head -20`

Note the lines where `## Pre-release testing` and `## Gotcha: snapshot-testing from a local clone` appear (currently 403 and 430; verify with the live file in case other PRs landed since this plan was written).

- [ ] **Step 2: Insert the new section**

Use the Edit tool. Find the line `## Gotcha: snapshot-testing from a local clone` and replace it with:

```markdown
## Maintenance of older release lines

Cyoda-go is pre-1.0 and **older release lines are not maintained**. No back-port branches exist by default. Patch bumps within the active line are non-breaking; minor bumps may break wire format, configuration, or operational surface.

If a real consumer needs a fix on an older line:

1. Open an issue describing the constraint (which version, which fix, why an upgrade is not viable).
2. The maintainers will consider creating an official maintenance branch for that line.
3. If accepted, the branch is named `release/vX.Y.x` (e.g. `release/v0.6.x`) and is cut from the relevant tag.

Until a maintenance branch is created and announced, treat older lines as frozen.

## Gotcha: snapshot-testing from a local clone
```

(That is, the new `## Maintenance of older release lines` section is inserted immediately before the existing `## Gotcha: snapshot-testing from a local clone` heading; the old heading is preserved verbatim at the end so the old anchor still works.)

- [ ] **Step 3: Verify the file still renders correctly**

Run: `grep -n '^## ' MAINTAINING.md | head -20`

Expected: see `## Maintenance of older release lines` between `## Pre-release testing` and `## Gotcha: snapshot-testing from a local clone`.

- [ ] **Step 4: Commit**

```bash
git add MAINTAINING.md
git commit -m "$(cat <<'EOF'
docs(maintaining): add policy for maintenance of older release lines

Codifies the no-backport-by-default policy and the request-an-issue
escalation path. Verbatim adoption from the README's existing
'Versioning' paragraph, which will be cut to a single line in the
README rewrite (deeper detail belongs in MAINTAINING).
EOF
)"
```

---

## Task 5: Rename `OVERVIEW.md` → `docs/FEATURES.md` (rename only, no content change)

**Files:**
- Move: `OVERVIEW.md` → `docs/FEATURES.md`

Doing rename and content edit in the same commit makes `git log --follow` rename-detection fragile. Split rename and edit into two commits.

- [ ] **Step 1: Run the rename**

```bash
git mv OVERVIEW.md docs/FEATURES.md
```

- [ ] **Step 2: Verify rename detection works**

Run: `git status`

Expected: `renamed: OVERVIEW.md -> docs/FEATURES.md`. If git shows a delete + add instead, the file is too different from any other file in the index and the rename will not be detected — but this should not happen for a pure move. If it does, abort and investigate.

- [ ] **Step 3: Verify `git log --follow` resolves history**

Run: `git log --follow --oneline docs/FEATURES.md | head -3`

Note: this will not work until after the commit lands. Defer the actual check to Step 5.

- [ ] **Step 4: Commit the rename**

```bash
git commit -m "$(cat <<'EOF'
docs: rename OVERVIEW.md → docs/FEATURES.md (no content change)

Pure rename to enable git rename-detection. Content edit (shrink,
re-scope intro, add sqlite to feature list) lands in the next commit.
The new name advertises the file's actual scope (feature & API surface
inventory, not architecture overview); architecture content lives in
docs/ARCHITECTURE.md.
EOF
)"
```

- [ ] **Step 5: Verify history resolves through the rename**

Run: `git log --follow --oneline docs/FEATURES.md | head -5`

Expected: shows the new rename commit followed by the original OVERVIEW.md commits.

---

## Task 6: Shrink and re-scope `docs/FEATURES.md`

**Files:**
- Modify: `docs/FEATURES.md`

Strip the 9 architecture sections that duplicate `docs/ARCHITECTURE.md`; keep Feature List + REST API Surface + gRPC API Surface; replace the intro; add `sqlite` to the Pluggable Persistence feature list.

- [ ] **Step 1: Inspect the current file structure**

Run: `grep -n '^##' docs/FEATURES.md`

Expected sections (from the legacy OVERVIEW.md): `## System Architecture`, `### Domain Modules`, `### Persistence`, `### Multi-Tenancy`, `### Transactions`, `### Workflow Engine`, `### gRPC & Externalized Processing`, `### Authentication`, `### Error Handling`, `## Feature List`, `### Entity Management`, `### Entity Models`, `### Workflow Engine`, `### Search`, `### Audit Trail`, `### Edge Messaging`, `### gRPC Integration`, `### Authentication & Authorization`, `### Multi-Tenancy`, `### Temporal Integrity`, `### Pluggable Persistence`, `## REST API Surface`, `## gRPC API Surface`.

- [ ] **Step 2: Read the full file**

Use the Read tool on `docs/FEATURES.md` to load the current contents into context. You will need the exact text of two boundary regions for the Edit operations below.

- [ ] **Step 3: Replace the title + intro + entire architecture block in a single Edit**

The architecture content is one contiguous block: title (line 1) + 1-line intro + `## System Architecture` heading through the end of the `### Error Handling` section. The block ends just before the `---` separator that precedes `## Feature List`.

Use the Edit tool with:

- `old_string` — start at `# Cyoda-Go — Architecture & Feature Overview` and end at the closing line of the `### Error Handling` paragraph (the line that ends with `(sanitized or verbose).`). Include every line in between, exactly as in the file. The block ends just before the `---` that starts the Feature List section.
- `new_string`:

```markdown
# Cyoda-Go — Feature & API Surface Inventory

This document inventories every feature implemented in cyoda-go and lists the REST and gRPC API surfaces. It is the answer to "what can this thing do" and "where does endpoint X live."

For **architecture** — modular layout, storage plugin contract, transaction model, multi-node routing, partition analysis — see [`docs/ARCHITECTURE.md`](ARCHITECTURE.md).

For **product context** — value proposition, target use cases, scale envelope, cost model — see [`docs/PRD.md`](PRD.md).
```

After the Edit, the file should begin with the new title + 3 paragraphs, followed immediately by the existing `---` separator and `## Feature List` (untouched).

- [ ] **Step 4: Verify the architecture block is gone**

Run: `grep -n '^## System Architecture\|^### Domain Modules\|^### Persistence$\|^### Multi-Tenancy$\|^### Transactions$\|^### gRPC & Externalized Processing\|^### Authentication$\|^### Error Handling' docs/FEATURES.md`

Expected: **no output**. Each of these section headings was part of the deleted architecture block. (Note: `### Multi-Tenancy` and `### Workflow Engine` and `### Authentication & Authorization` appear in the kept Feature List, which is fine — the grep above pins the exact heading text used in the architecture half so it is unambiguous.)

- [ ] **Step 5: Add `sqlite` to the "Pluggable Persistence" feature list**

Use the Edit tool with:

- `old_string`:

```markdown
### Pluggable Persistence
- In-memory backend (zero dependencies, sub-millisecond)
- PostgreSQL backend (durable, SI+FCW via `REPEATABLE READ` + first-committer-wins, automatic migrations)
```

- `new_string`:

```markdown
### Pluggable Persistence
- In-memory backend (zero dependencies, sub-millisecond)
- SQLite backend (single-file persistent storage; no external server; embedded SQL migrations)
- PostgreSQL backend (durable, SI+FCW via `REPEATABLE READ` + first-committer-wins, automatic migrations, multi-node-capable)
```

- [ ] **Step 6: Verify the final file structure**

Run: `grep -n '^#' docs/FEATURES.md`

Expected: `# Cyoda-Go — Feature & API Surface Inventory` at line 1, then `## Feature List`, then the `### …` sub-sections of the feature list, then `## REST API Surface`, then `## gRPC API Surface`. No architecture sections.

- [ ] **Step 7: Verify file size**

Run: `wc -l docs/FEATURES.md`

Expected: roughly 120–135 lines (down from 231). If significantly larger, an architecture section was missed; if significantly smaller, kept content was lost.

- [ ] **Step 8: Commit**

```bash
git add docs/FEATURES.md
git commit -m "$(cat <<'EOF'
docs(features): shrink to feature & API surface inventory; defer to ARCHITECTURE

The legacy OVERVIEW.md duplicated docs/ARCHITECTURE.md at lower
fidelity and had drifted ('single-node Go digital twin' tagline; sqlite
missing from Persistence). With ARCHITECTURE.md as the version-tagged
source of truth for system architects, FEATURES.md collapses to its
non-overlapping role: feature inventory + REST/gRPC API surface tables.

- Removed 9 architecture sections (System Architecture, Domain Modules,
  Persistence, Multi-Tenancy, Transactions, Workflow Engine, gRPC &
  Externalized Processing, Authentication, Error Handling) — all
  duplicates of ARCHITECTURE.md sections
- New title + intro paragraph re-scopes the doc and points readers at
  ARCHITECTURE.md and PRD.md
- Added sqlite to Pluggable Persistence feature list (was missing)
EOF
)"
```

---

## Task 7: Update inbound link in `docs/proposals/2026-04-16-Cyoda_Mem_PRD_Final.md`

**Files:**
- Modify: `docs/proposals/2026-04-16-Cyoda_Mem_PRD_Final.md` (line 8)

- [ ] **Step 1: Verify the current link**

Run: `sed -n '5,12p' docs/proposals/2026-04-16-Cyoda_Mem_PRD_Final.md`

Expected: line 8 contains `https://github.com/Cyoda-platform/cyoda-go/blob/main/OVERVIEW.md`.

- [ ] **Step 2: Replace the link**

Use the Edit tool to replace the URL from `.../blob/main/OVERVIEW.md` to `.../blob/main/docs/FEATURES.md`. The exact replacement target is the URL substring; preserve the rest of the line (link text, surrounding markdown) verbatim.

- [ ] **Step 3: Verify the change**

Run: `grep -n 'OVERVIEW.md\|FEATURES.md' docs/proposals/2026-04-16-Cyoda_Mem_PRD_Final.md`

Expected: only the new `FEATURES.md` URL appears; no OVERVIEW.md mentions remain.

- [ ] **Step 4: Commit**

```bash
git add docs/proposals/2026-04-16-Cyoda_Mem_PRD_Final.md
git commit -m "docs(proposals): update Cyoda_Mem_PRD link from OVERVIEW.md to docs/FEATURES.md"
```

---

## Task 8: Help-topic gap audit (bounded)

**Files:**
- Read: `cmd/cyoda/help/content/config.md`, `cmd/cyoda/help/content/config/*.md`, `cmd/cyoda/help/content/telemetry.md`
- Read: the current `README.md` (for the env-var sections being removed)
- Modify (only if a gap is found and fits the bound): the relevant `cyoda help` topic file

The `TestConfig_EnvVarCoverage` test guarantees every `CYODA_*` env var in source is mentioned in at least one `config/**/*.md` or `config.md` file. So the env-var inventory itself is already complete. This task audits the **explanatory prose** that accompanied env vars in the README — text that may not have transferred to the help topics.

The bound: any single fix should be ≤ 30 lines of new help-topic content. If a gap exceeds 30 lines, stop and surface to the user (Gate 6).

- [ ] **Step 1: Enumerate the README sections being removed**

Run: `sed -n '281,545p' README.md > /tmp/readme-evicted.txt && wc -l /tmp/readme-evicted.txt`

This captures the entire Configuration / Admin / Security section block being removed.

- [ ] **Step 2: Diff the prose against the destination help topics**

For each subsection in the evicted block, identify its destination per the spec's destination map (spec §2). Read both the README subsection and the destination help topic. Note any **explanatory prose** present in the README but absent from the topic. Examples to look for specifically:

- The `_FILE` suffix paragraph (README lines 358–375). Verify `cmd/cyoda/help/content/config.md` and/or `config/auth.md` explain the `_FILE` precedence rule, the trailing-whitespace stripping, and the canonical Docker/Kubernetes-secret pattern. If only the env-var names are listed (without the precedence explanation), that is a gap.
- The mock-auth banner paragraph (README lines 458–459). Verify `config/auth.md` explains why `CYODA_SUPPRESS_BANNER=true` is a CI-only setting.
- The admin-listener bind-address discussion (README lines 444–449). Verify `cmd/cyoda/help/content/telemetry.md` covers desktop/Kubernetes/Docker bind-address modes.
- The `CYODA_REQUIRE_JWT` "production safety floor" paragraph (README lines 453–455). Verify `config/auth.md` explains the coupled-flag check and the Helm-default behaviour.

- [ ] **Step 3: For each found gap, decide bounded vs. surface**

For each gap, estimate the size of the fix:

- ≤ 30 lines of new content in an existing topic: apply the fix inline. Add a note in the commit message summarizing the gap and the fix.
- > 30 lines of new content, or the fix would require a new help topic file, or the gap reveals a code-vs-docs drift requiring code changes: stop. Report the gap to the user as a follow-up. Do not silently document aspirational behaviour.

- [ ] **Step 4: Apply each bounded fix as its own commit**

For each gap fixed, commit with a message of the form:

```
docs(help): backfill <subject> in <topic> from README

Closes a gap where the README explained <X> but the cyoda help topic
only listed the env vars without the explanation. Surfaced during the
README cut audit.
```

- [ ] **Step 5: Run the help test suite as a regression check**

Run: `go test ./cmd/cyoda/help/... -v`

Expected: PASS for all gap-fix changes.

- [ ] **Step 6: Report**

Print a summary of gaps found, gaps fixed inline, and gaps surfaced for follow-up. If there were no gaps, report `gap audit clean — no help-topic backfill needed`.

---

## Task 9: Rewrite `README.md`

**Files:**
- Modify: `README.md` (full rewrite)

This is the largest single task. Replace the entire 545-line README with the ~140-line evaluator-first skeleton from the spec. All linked destinations must already exist by this point.

- [ ] **Step 1: Verify all destinations exist**

Run:

```bash
test -f docs/ARCHITECTURE.md && echo "ARCHITECTURE.md ✓"
test -f docs/PRD.md && echo "PRD.md ✓"
test -f docs/FEATURES.md && echo "FEATURES.md ✓"
test -f docs/plugins.md && echo "plugins.md ✓"
test -f cmd/cyoda/help/content/admin.md && echo "admin help ✓"
test -f cmd/cyoda/help/content/cluster.md && echo "cluster help ✓"
test -d examples/compose-with-observability && echo "compose-with-observability ✓"
test -f CONTRIBUTING.md && echo "CONTRIBUTING.md ✓"
test -f SECURITY.md && echo "SECURITY.md ✓"
test -f LICENSE && echo "LICENSE ✓"
test -f CHANGELOG.md && echo "CHANGELOG.md ✓"
test -f MAINTAINING.md && echo "MAINTAINING.md ✓"
```

Expected: every line prints a `✓`. If any are missing, the corresponding earlier task did not land — fix that before continuing.

- [ ] **Step 2: Replace `README.md` with the new skeleton**

Use the Write tool to overwrite `README.md` with these exact contents (note: the badges row is left empty if no upstream badges existed in the legacy README; check `git log -1 --pretty=format:%H -- README.md` and `git show <SHA>:README.md | head -3` to confirm. If badges existed, preserve them as-is at the top):

````markdown
# cyoda-go

**One transactional runtime for the entity lifecycle.**

cyoda-go is an EDBMS — state machine, processors, and full revision history live inside the record, committed atomically. Minimizes the need for sagas, CDC pipelines, and external orchestration.

## Four storage engines, one application contract

Same application code, four operational shapes:

| Engine     | Where it fits                                 | Availability       |
|------------|-----------------------------------------------|--------------------|
| memory     | Local dev, unit tests, digital-twin scenarios | open source        |
| sqlite     | Edge, single-node self-host, persistent dev   | open source        |
| postgres   | Production transactional workloads, HA        | open source        |
| cassandra  | Distributed scale, high write throughput      | commercial (Cyoda) |

Switch by setting `CYODA_STORAGE_BACKEND` — no code changes. The cassandra engine is offered as a commercial backend by Cyoda for workloads that outgrow a single PostgreSQL primary; contact information is on the [cyoda.com](https://cyoda.com) website.

## Try it in 30 seconds

```bash
brew install cyoda-platform/cyoda-go/cyoda
cyoda init && cyoda &
curl http://localhost:8080/api/health
# {"status":"UP"}
```

This starts cyoda with mock auth and sqlite persistence at `~/.local/share/cyoda/cyoda.db`. See **Install** for non-Homebrew options and **First real call** for jwt + a real authenticated request.

## Install

### Homebrew (macOS / Linux)

```bash
brew install cyoda-platform/cyoda-go/cyoda
```

### curl (any Unix)

```bash
curl -fsSL https://github.com/cyoda-platform/cyoda-go/releases/latest/download/install.sh | sh
```

Installs to `~/.local/bin/cyoda` and runs `cyoda init`. Pin a version with `CYODA_VERSION=v0.7.1 curl ... | sh`. The installer SHA256-verifies the archive and, if [`cosign`](https://docs.sigstore.dev/cosign/installation/) is on `PATH`, also verifies a Sigstore keyless signature from the cyoda-go release workflow.

### Debian / Ubuntu / Fedora / RHEL

```bash
# Debian / Ubuntu
wget https://github.com/cyoda-platform/cyoda-go/releases/latest/download/cyoda_linux_amd64.deb
sudo dpkg -i cyoda_linux_amd64.deb

# Fedora / RHEL
wget https://github.com/cyoda-platform/cyoda-go/releases/latest/download/cyoda_linux_amd64.rpm
sudo rpm -i cyoda_linux_amd64.rpm
```

Replace `amd64` with `arm64` for ARM hosts. Both packages drop `/usr/bin/cyoda` and `/etc/cyoda/cyoda.env` (sqlite as the system-wide default, preserved across upgrades).

### From source

Requires **Go 1.26+**.

```bash
go install github.com/cyoda-platform/cyoda-go/cmd/cyoda@latest
```

This binary uses the in-memory backend by default. Run `cyoda init` for sqlite persistence, or set `CYODA_STORAGE_BACKEND` directly.

## First real call

The 30-second example uses mock auth. To exercise the real auth chain end-to-end with sqlite + jwt:

```bash
# Generate a JWT signing key
openssl genrsa -out /tmp/jwt.key 2048

# Initialize sqlite config and configure jwt mode
cyoda init
export CYODA_IAM_MODE=jwt
export CYODA_JWT_SIGNING_KEY_FILE=/tmp/jwt.key
export CYODA_BOOTSTRAP_CLIENT_ID=demo
export CYODA_BOOTSTRAP_CLIENT_SECRET=demo-secret

# Start the server
cyoda &

# Get an OAuth 2.0 token via client_credentials
TOKEN=$(curl -sX POST http://localhost:8080/api/oauth/token \
  -u "$CYODA_BOOTSTRAP_CLIENT_ID:$CYODA_BOOTSTRAP_CLIENT_SECRET" \
  -d "grant_type=client_credentials" | jq -r .access_token)

# Make an authenticated call
curl -H "Authorization: Bearer $TOKEN" http://localhost:8080/api/account
```

The `/api/account` response confirms the bootstrap client's tenant and roles. From here, follow the **Build an app** link below to register an entity model and start creating entities.

## Where to go next

Online docs at [docs.cyoda.net](https://docs.cyoda.net) mirror the `cyoda help` topic tree — the same content is available offline via `cyoda help <topic>`.

| Goal                          | Link                                              |
|-------------------------------|---------------------------------------------------|
| Build an app                  | https://docs.cyoda.net/help/quickstart            |
| Configure                     | https://docs.cyoda.net/help/config                |
| Error reference               | https://docs.cyoda.net/help/errors                |
| Deploy with Helm              | https://docs.cyoda.net/help/helm                  |
| Deploy with Docker Compose    | [examples/compose-with-observability/](examples/compose-with-observability/) |
| Architecture                  | [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)      |
| Product overview              | [docs/PRD.md](docs/PRD.md)                        |
| Feature & API inventory       | [docs/FEATURES.md](docs/FEATURES.md)              |
| Multi-node cluster            | https://docs.cyoda.net/help/cluster (deep dive: [docs/ARCHITECTURE.md §4](docs/ARCHITECTURE.md#4-multi-node-routing-architecture)) |
| Admin endpoints (log/trace)   | https://docs.cyoda.net/help/admin                 |
| Write a storage plugin        | [docs/plugins.md](docs/plugins.md)                |
| Contribute                    | [CONTRIBUTING.md](CONTRIBUTING.md)                |
| Security disclosures          | [SECURITY.md](SECURITY.md)                        |

## Versioning

cyoda-go is **pre-1.0**. Minor bumps may break wire format, configuration, or operational surface; patch bumps do not. See [`CHANGELOG.md`](CHANGELOG.md) for breaking changes and [`MAINTAINING.md`](MAINTAINING.md#maintenance-of-older-release-lines) for the policy on older release lines.

## License

Apache-2.0 — see [LICENSE](LICENSE).
````

- [ ] **Step 3: Verify line count**

Run: `wc -l README.md`

Expected: ≤ 150 lines. If significantly under (say < 100) something was lost; if over 150 something was kept that should not have been.

- [ ] **Step 4: Verify every link in the README is reachable**

Run:

```bash
# In-repo links
for f in docs/ARCHITECTURE.md docs/PRD.md docs/FEATURES.md docs/plugins.md \
         CONTRIBUTING.md SECURITY.md LICENSE CHANGELOG.md MAINTAINING.md \
         examples/compose-with-observability; do
  test -e "$f" && echo "$f ✓" || echo "$f MISSING"
done
```

Expected: every line prints `✓`. Any `MISSING` is a bug.

For external links (https://docs.cyoda.net/help/...), perform a manual smoke check by curling each:

```bash
for path in quickstart config errors helm cluster admin; do
  echo -n "https://docs.cyoda.net/help/$path → "
  curl -s -o /dev/null -w "%{http_code}\n" "https://docs.cyoda.net/help/$path"
done
```

Expected: every URL returns `200`. **If `https://docs.cyoda.net/help/cluster` or `https://docs.cyoda.net/help/admin` returns 404**, that is expected at this point — docs.cyoda.net mirrors `cyoda help` on its own publish cadence; the new topics added in Tasks 1 and 2 will surface there only after the docs site rebuilds. Note this in the PR body.

- [ ] **Step 5: Destination-map audit (grep distinctive phrases)**

For each removed README concept, confirm it lives in its destination. Run:

```bash
# Each phrase below was unique to the old README; confirm it survives somewhere.
echo '== txID-to-physical-handle bridge =='
grep -rn 'txID-to-physical-handle\|txID-to-pgx\|txID.*bridge' docs/ARCHITECTURE.md docs/plugins.md cmd/cyoda/help/content/

echo '== SI+FCW =='
grep -rn 'SI+FCW' docs/ARCHITECTURE.md docs/PRD.md cmd/cyoda/help/content/

echo '== Bootstrap M2M client =='
grep -rn 'CYODA_BOOTSTRAP_CLIENT' cmd/cyoda/help/content/

echo '== _FILE suffix =='
grep -rn '_FILE' cmd/cyoda/help/content/config*

echo '== mock-auth banner =='
grep -rn 'CYODA_SUPPRESS_BANNER\|mock-auth.*banner\|mock auth.*banner' cmd/cyoda/help/content/

echo '== gossip / SWIM =='
grep -rn 'SWIM\|gossip' cmd/cyoda/help/content/cluster.md docs/ARCHITECTURE.md
```

Expected: every section produces at least one match. A zero-match section indicates content was lost; investigate before continuing.

- [ ] **Step 6: Commit**

```bash
git add README.md
git commit -m "$(cat <<'EOF'
docs(readme): rewrite as evaluator-first ~140-line landing page

Cuts README from 545 → ~140 lines. The new structure leads with the
'one transactional runtime' value proposition and the four-storage-
engines growth path, then a 30-second-try block, install, a real
sqlite+jwt first-call sequence, and a routing matrix to deeper docs.

Removed sections (every concept has a definite home):
- Architecture / cluster topology / scale → docs/ARCHITECTURE.md (canonical)
- Product context / use cases → docs/PRD.md
- Feature inventory / REST/gRPC API surface → docs/FEATURES.md
- Configuration env-var tables → cyoda help config.* (canonical)
- Admin endpoints (log-level, trace-sampler) → cyoda help admin (new)
- Multi-node cluster operations → cyoda help cluster (new)
- SPI plugin authoring → docs/plugins.md (thin entry point) +
  docs/ARCHITECTURE.md §1 (full contract)
- Older-release-line maintenance policy → MAINTAINING.md
- Broken Docker compose quickstart (marked 'temporarily unavailable')
  → deleted; replaced with link to examples/compose-with-observability/

Spec: docs/superpowers/specs/2026-05-08-readme-devx-refactor-design.md
EOF
)"
```

---

## Task 10: Final verification — full test suite + manual smoke

**Files:** none (verification only)

- [ ] **Step 1: Run all help tests**

Run: `go test ./cmd/cyoda/help/... -v`

Expected: PASS. Specifically `TestAllTopLevelTopicsPresent`, `TestSeeAlsoResolution`, `TestContentMarkdownSubsetLinter`, and `TestConfig_EnvVarCoverage` all pass.

- [ ] **Step 2: Run all root-module tests as a regression check**

Run: `go test ./... -short -v`

Expected: PASS. The `-short` flag skips E2E tests (which require Docker); this is a docs-only PR so no functional regression is expected.

- [ ] **Step 3: Run `go vet`**

Run: `go vet ./...`

Expected: no output (clean).

- [ ] **Step 4: Build the binary and smoke-test the new help topics manually**

```bash
go build -o /tmp/cyoda ./cmd/cyoda
/tmp/cyoda help admin | head -20
/tmp/cyoda help cluster | head -20
/tmp/cyoda help | grep -E 'admin|cluster'
```

Expected:
- `cyoda help admin` and `cyoda help cluster` render their content (no "topic not found" error).
- The top-level `cyoda help` index lists `admin` and `cluster` as topics.

- [ ] **Step 5: Verify rename history**

Run: `git log --follow --oneline docs/FEATURES.md | tail -5`

Expected: history walks back through the rename to the original OVERVIEW.md commits.

- [ ] **Step 6: Verify the inbound proposal link**

Run: `grep -rn 'OVERVIEW\.md' --include='*.md' --include='*.go' . | grep -v '^./.worktrees/\|^./docs/superpowers/specs/\|^./docs/audits/'`

Expected: only matches in `docs/superpowers/specs/2026-04-16-provisioning-shared-design.md` (intentionally not updated per spec — historical record). Any other match is a missed inbound link.

- [ ] **Step 7: Run the documentation-hygiene self-check**

Per CLAUDE.md Gate 4: when changing user-facing behaviour or developer workflow, check `README.md` and `CONTRIBUTING.md` are still accurate. Read CONTRIBUTING.md and check whether anything it says is invalidated by the README rewrite.

Run: `grep -n 'README\|OVERVIEW' CONTRIBUTING.md`

Expected: any mention of README content (e.g., "see README for X") still resolves. The most likely thing to check is that CONTRIBUTING.md does not link to a now-deleted README anchor.

- [ ] **Step 8: Report definition of done**

Verify each item:

- [ ] README.md is ≤ 150 lines and follows the skeleton from the spec
- [ ] OVERVIEW.md no longer exists at the repo root
- [ ] docs/FEATURES.md exists with title `# Cyoda-Go — Feature & API Surface Inventory`, contains only Feature List + REST/gRPC API Surface + intro paragraph pointing to ARCHITECTURE.md / PRD.md
- [ ] `git log --follow docs/FEATURES.md` resolves history back through the rename
- [ ] docs/FEATURES.md "Feature List" includes the sqlite plugin under Pluggable Persistence
- [ ] cmd/cyoda/help/content/admin.md exists; `cyoda help admin` renders
- [ ] cmd/cyoda/help/content/cluster.md exists; `cyoda help cluster` renders
- [ ] Both new topics are in `topLevelTopicsV061` and the index
- [ ] docs/plugins.md exists (~40 lines); content matches Task 3
- [ ] MAINTAINING.md contains a `## Maintenance of older release lines` section
- [ ] README routing matrix's "Architecture" row points to `docs/ARCHITECTURE.md`; "Feature & API inventory" row points to `docs/FEATURES.md`
- [ ] docs/proposals/2026-04-16-Cyoda_Mem_PRD_Final.md GitHub URL points to docs/FEATURES.md
- [ ] `go test ./cmd/cyoda/help/...` is green
- [ ] `go test ./... -short` is green
- [ ] `go vet ./...` is clean
- [ ] No external Go source files modified (this is a docs-only PR — verify with `git diff --stat origin/main..HEAD --name-only | grep '\.go$' | grep -v '_test.go'` returning empty)
