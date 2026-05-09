# Cyoda-Mem: Enterprise AI Memory System

## Dual PRD — Platform Vector Search + Stateful Memory Application

**Version:** 3.0 — April 2026
**Status:** Proposal for Stakeholder Review
**Authors:** Cyoda Platform Team
**Prerequisite Reading:** [Cyoda-Go FEATURES.md](https://github.com/Cyoda-platform/cyoda-go/blob/main/docs/FEATURES.md)

---

## Executive Summary

AI agents need memory that persists, compounds, and stays correct under concurrent access. The 2026 market has converged on vector search and graph relationships as baseline capabilities — every serious framework now offers them. What no framework offers today is **database-enforced transactional safety for multi-agent memory writes**, **bi-temporal audit trails that distinguish when a fact was true from when it was recorded**, **identical local-to-production execution semantics with zero infrastructure**, and **a clear scale path from a single SQLite file to a horizontally-scalable distributed cluster**.

This proposal consists of two interdependent workstreams:

1. **PRD 1 — Cyoda-Go Vector Search:** A platform enhancement adding native vector and semantic search to the core EDBMS at the SPI level, benefiting all applications built on Cyoda-Go.
2. **PRD 2 — Cyoda-Mem:** An application built on Cyoda-Go that serves as a stateful, auditable AI memory backend for enterprise agent systems.

Cyoda-Go's pluggable storage architecture means a single application binary runs unchanged across four backends: **In-Memory** (CI/testing), **SQLite** (local desktop agents), **PostgreSQL** (production single-instance), and a **licensed Cassandra 5 backend** (horizontal write scalability, no single points of failure). The Cassandra plugin — a proprietary module built on proven Cyoda Cloud architecture — provides shard-based distributed transaction coordination, append-only point-in-time storage, and I/O fencing against zombie writers. Enterprise deployments requiring scale beyond PostgreSQL's single-writer ceiling can license this backend without any application-layer changes.

**Recommendation:** Green-light PRD 1 immediately — it is a low-risk platform enhancement with value beyond memory. Approve PRD 2 for a scoped v1 build targeting the regulated enterprise niche, with benchmark validation as a phase gate before broader investment.

---

# PRD 1: Cyoda-Go Platform Enhancements (Vector Search)

## 1. Objective

Introduce a generic vector and semantic search facility into the core Cyoda-Go EDBMS at the Service Provider Interface (SPI) level, keeping the platform domain-agnostic while enabling high-performance similarity search for any application — including but not limited to AI memory.

## 2. Target Audience & Use Cases

**Audience:** Cyoda platform engineers, database administrators, and application developers building on Cyoda-Go.

**Use Cases:**

- **Semantic Filtering:** Finding records by conceptual similarity rather than exact keyword matches (e.g., "find active transactions similar to this vector pattern").
- **Hybrid Querying:** Combining strict access controls (tenant isolation, entity state filtering) with fuzzy similarity search in a single query, with the platform enforcing that vector queries never bypass tenant boundaries.
- **Local Persistent Prototyping:** Developers build and test vector-reliant applications on local SQLite with zero infrastructure, then deploy to PostgreSQL without code changes. The in-memory backend remains available for transient testing and CI pipelines.

## 3. Core Features & Requirements

### 3.1. SPI Vector Query Extension

Extend the `cyoda-go-spi` Search interface to accept an optional `VectorQuery` struct alongside traditional metadata filters.

**VectorQuery Payload:**

```go
type VectorQuery struct {
    Embedding      []float32  // The query vector
    Limit          int        // k-nearest neighbors to return
    DistanceMetric string     // "cosine" | "l2" | "inner_product"
}
```

**Supported Distance Metrics:**

| Metric | Best For | pgvector Operator |
|---|---|---|
| Cosine | Normalized LLM embeddings (OpenAI, Cohere) | `<=>` |
| L2 / Euclidean | Spatial and magnitude-sensitive data | `<->` |
| Inner Product | Recommendation engines (MaxIP search) | `<#>` |

**Hybrid Routing:** The platform's query router must securely bind the `VectorQuery` to existing `TenantID` and JSONB field constraints. A vector similarity search must never return results from a different tenant, regardless of semantic proximity. This is enforced at the SPI layer, not the application layer.

### 3.2. Storage Plugin Implementations

All open-source backends must produce identical result ordering for the same dataset and query vector, differing only in performance characteristics. The licensed Cassandra backend must meet the same parity contract.

**PostgreSQL Plugin (pgvector):**

- Add an `embedding` column to the core entities table migration. Support both `vector(N)` (float32, indexable up to 2,000 dimensions) and `halfvec(N)` (float16, supports up to 4,096 dimensions at 50% memory reduction — required for high-dimensional models like Cohere embed-v4).
- Translate SPI distance metrics to pgvector SQL operators.
- Support dynamic provisioning of two ANN index types:
  - **HNSW** (default): High-recall, continuous-insert workloads. Expose `m` (default 16) and `ef_construction` (default 64) at migration time, and session-level `hnsw.ef_search` at query time.
  - **IVFFlat** (fallback): Memory-constrained environments with massive datasets. Expose `lists` at creation and session-level `ivfflat.probes` at query time.

**SQLite Plugin (sqlite-vec):**

- Implement vector search using the `sqlite-vec` extension, providing persistent local vector storage without Docker or PostgreSQL.
- This is the critical enabler for the "local agent workspace" use case in PRD 2. Without it, local persistence requires infrastructure that defeats the scale-to-zero promise.
- Accept the tradeoff: sqlite-vec provides exact KNN (no ANN indexing), which is acceptable for the local-development volume profile (thousands to low tens-of-thousands of vectors).

**In-Memory Plugin:**

- Implement exact KNN in pure Go using brute-force distance computation over stored `[]float32` arrays.
- Purpose: transient testing and CI pipelines where persistence is unnecessary. Not intended for production workloads.

**Cassandra Plugin (Licensed):**

- The proprietary Cassandra 5 backend (separate binary, licensed for enterprise deployments) must also implement the vector SPI. The implementation strategy — whether via Cassandra's native vector search (SAI-based ANN), a sidecar vector index, or brute-force over the existing append-only entity storage — is a design decision for the Cassandra plugin team and is outside the scope of this open-source PRD.
- The SPI contract guarantees that the application layer (including Cyoda-Mem) requires zero changes to run on Cassandra. Vector queries route through the same `VectorQuery` struct regardless of backend.
- Parity testing must include the Cassandra backend to maintain the "develop locally, deploy anywhere" contract.

### 3.3. Parity Testing

Add an end-to-end parity test suite (extending the existing `e2e/parity` framework) that runs identical vector queries against all backends and asserts identical result ordering within floating-point tolerance. The open-source suite covers In-Memory, SQLite, and PostgreSQL. The Cassandra parity suite runs in the licensed build. This is essential for the "develop locally, deploy anywhere" contract.

## 4. Architecture Impact

| Component | Layer | Change |
|---|---|---|
| `cyoda-go-spi` | Interface | New `VectorQuery` struct on the Search interface |
| PostgreSQL plugin | Infrastructure | New column, operators, index migrations |
| SQLite plugin | Infrastructure | sqlite-vec extension integration |
| In-Memory plugin | Infrastructure | Brute-force KNN implementation |
| Cassandra plugin (licensed) | Infrastructure | Vector SPI implementation (design TBD by Cassandra team) |
| Core EDBMS | Platform | Query router extended to bind vector queries to tenant/state filters |

## 5. Risks

**Key risks:**

- pgvector's 2,000-dimension indexing limit may become a constraint as embedding models grow. Mitigation: `halfvec` support for up to 4,096 dimensions at reduced precision.
- sqlite-vec is younger than pgvector. Mitigation: exact KNN means fewer edge cases than ANN, and the parity test suite catches behavioral drift.

---

# PRD 2: Cyoda-Mem (Stateful AI Memory Application)

## 1. Objective

Build a memory application (`cyoda-mem`) on top of Cyoda-Go that serves as a stateful, auditable AI memory backend for enterprise agent systems. Cyoda-Mem leverages the platform's workflows, bi-temporal versioning, SERIALIZABLE transactions, and entity lifecycle engine to provide capabilities that no existing memory framework delivers.

## 2. Why Now — Market Context

The AI memory market has matured rapidly. In 2024, "we have vector search" was a differentiator. By April 2026, vector search, graph relationships, and MCP interfaces are table stakes. Mem0 has 48K+ GitHub stars and $24M in funding. Hindsight scores 91.4% on LongMemEval. Memori ships SQL-native auditable memory with SOC 2 compliance. Supermemory claims #1 on three major benchmarks.

Simultaneously, Andrej Karpathy's LLM Wiki pattern has shifted developer expectations toward compiled, compounding knowledge systems rather than stateless retrieve-and-re-derive pipelines. MemPalace hit 23K stars in 72 hours by shipping 19 read-write MCP tools including knowledge graph mutations and agent diary writes.

**Entering this market on vector search and graph features alone would be commercially irrelevant.** Cyoda-Mem must lead with what no one else has.

## 3. True Differentiation — Three Pillars

### Pillar 1: SERIALIZABLE Transaction Safety

When dozens of autonomous agents concurrently read, update, and resolve memory states, race conditions corrupt the knowledge base. Every other memory framework either uses eventual consistency or relies on last-write-wins semantics.

Cyoda-Go provides PostgreSQL-native SERIALIZABLE Snapshot Isolation with conflict detection at commit. A memory write that fails validation rolls back atomically — the agent retries, not the human. In-memory and SQLite backends provide equivalent SSI semantics.

**This matters in practice:** A procurement agent and a compliance agent both discover contradictory facts about the same vendor. With last-write-wins, one fact silently overwrites the other. With Cyoda-Mem, both writes enter a transaction, the conflict is detected, and the workflow engine routes them through the SUPERSEDES resolution path deterministically.

### Pillar 2: Deterministic Auditability & Bi-Temporal Versioning

Cyoda-Go natively tracks three timestamps on every entity version:

- **valid_time:** When the fact was true in the real world.
- **transaction_time:** When an AI agent recorded or mutated the fact.
- **wall_clock_time:** When the database physically committed the change.

This means Cyoda-Mem can answer questions no competitor can: *"What did the agent believe about this customer at 3pm on Tuesday, and when did it learn that belief was wrong?"* Point-in-time retrieval across both temporal axes is a built-in platform capability, not an application-layer approximation.

Cyoda's audit ledger records 13 event types covering the full state machine narrative — workflow selection, transitions attempted and made, processor results, and cancellation. Every memory mutation is traceable.

### Pillar 3: Scale-to-Zero Through Enterprise Scale

Identical execution semantics across four storage backends, selected at startup with zero application changes:

| Backend | Use Case | Transaction Model | Persistence |
|---|---|---|---|
| **In-Memory** | CI pipelines, unit tests | SSI (in-process) | None |
| **SQLite** | Local desktop agents, single-user | SSI (in-process) | Local file |
| **PostgreSQL** | Production single-instance, small-to-medium teams | SERIALIZABLE (native PG SSI) | Durable, multi-node stateless app tier |
| **Cassandra 5** (licensed) | Enterprise horizontal scale | Snapshot Isolation with first-committer-wins, shard-based distributed coordination via Redpanda | Append-only, no single point of failure |

A developer builds and tests a memory-backed agent on their laptop with a single binary and a SQLite file. The same application deploys to a PostgreSQL cluster for production. When write throughput exceeds PostgreSQL's single-writer ceiling, the enterprise licenses the Cassandra backend — which provides shard-based distributed transaction coordination, I/O fencing against zombie writers, a multi-node consistency clock, and append-only point-in-time storage — without any application-layer changes. The Cassandra plugin is a proprietary module adapted from Cyoda's proven Cloud platform architecture; it ships as a separate binary and is not open-sourced.

No other memory framework offers this range. Hindsight requires Docker with embedded PostgreSQL. Mem0 requires configuring a vector backend. Letta runs as a full server. Cognee uses three separate local stores (SQLite + LanceDB + Kuzu). None of them have a horizontal write-scalability story.

## 4. Competitive Positioning (2026)

| Competitor | Strength | Cyoda-Mem Advantage | Cyoda-Mem Concession |
|---|---|---|---|
| **Mem0** | Market leader. 48K stars, AWS Agent SDK integration, graph memory via Neo4j/Neptune. | ACID transactions, bi-temporal audit trail, no temporal model gap. Horizontal scale path via Cassandra. | Mem0's ecosystem breadth and community size are unmatched. Cyoda-Mem is not competing for the personalization market. |
| **Memori** | Closest architectural peer. SQL-native, auditable, 81.95% LoCoMo, SOC 2/PCI compliant. | SERIALIZABLE isolation (vs. Memori's standard SQL), built-in workflow engine, horizontal scale path via licensed Cassandra backend. | Memori has a 13K-star head start and shipping enterprise customers. |
| **Hindsight** | Retrieval depth leader. 91.4% LongMemEval, four parallel retrieval strategies, embedded Postgres. | Deterministic state machine enforcement, stricter temporal modeling, zero-infrastructure local path, scale beyond single-Postgres via Cassandra. | Hindsight's multi-strategy retrieval is more sophisticated than Cyoda-Mem v1's single-strategy approach. |
| **Zep / Graphiti** | Temporal reasoning leader. Bi-temporal knowledge graphs, 63.8% → Mem0's 49% on temporal tasks (GPT-4o). | Standard PostgreSQL/SQLite instead of Neo4j. Operational simplicity. Cassandra backend for write-heavy workloads where Neo4j's single-writer model limits throughput. | Zep's specialized graph traversal will outperform relational graph emulation for deep multi-hop queries. |
| **Letta** | Agent autonomy. Agents manage their own tiered memory via OS-like abstractions. | Cyoda-Mem is a memory *service* agents call into, not a runtime that replaces your agent stack. Four-tier storage spectrum from local to distributed. | Letta's self-editing memory gives agents more autonomy over what they remember and forget. |
| **Supermemory** | Benchmark leader (self-reported). Full stack: RAG + connectors + memory + profiles in one API. | Open architecture, no vendor lock-in, self-hostable on standard Postgres or horizontally-scalable Cassandra. | Supermemory's all-in-one convenience and coding-agent UX are hard to match. |

**Explicit non-goal:** Cyoda-Mem is not competing for the consumer personalization market (Mem0's strength) or the coding-agent workflow market (Supermemory's strength). The target is **regulated enterprise environments where correctness, auditability, multi-agent concurrency safety, and a clear scale path from laptop to data center are requirements, not nice-to-haves.**

## 5. Target Audience & Use Cases

**Primary audience:** Developers building multi-agent enterprise systems, autonomous orchestrators, and local persistent AI assistants in regulated industries.

**Use Case 1 — Regulated Enterprise AI:**
Financial services, healthcare, and government agents where every memory retrieval and state change must be tenant-isolated, resolvable under high concurrency, and logged for SOC 2, PCI, and HIPAA compliance. Example: A fleet of customer-facing agents in a bank, where one agent's memory about a client's risk profile must never leak to another tenant, and every change to that profile must be auditable with valid-time semantics for regulatory reporting.

**Use Case 2 — Multi-Agent Research Orchestration:**
Autonomous research agents conducting multi-day investigations that build, traverse, and verify a network of facts. Multiple agents writing concurrently must not corrupt each other's findings. The SUPERSEDES workflow ensures contradictions are resolved deterministically rather than silently overwritten.

**Use Case 3 — Local Persistent Agent Workspaces:**
A developer or knowledge worker running an autonomous assistant entirely locally on SQLite, persisting memory between reboots without Docker, cloud accounts, or containerized infrastructure. The assistant tracks projects, codebases, and preferences over months with the same semantic and temporal capabilities as the production system.

## 6. Core Features & Requirements

### 6.1. Entity Definitions — Memory Taxonomy & Knowledge Graph

**Requirement:** Define a graph-relational schema that implements the modern AI memory taxonomy and leverages bi-temporal capabilities for contradiction resolution.

**Memory Taxonomy:**

| Memory Type | Description | Typical Lifetime | Example |
|---|---|---|---|
| **Working** | Recent, transient observations from the current task | Minutes to hours | "The user just asked about Q3 revenue" |
| **Episodic** | Session summaries and event logs | Days to weeks | "In yesterday's session, we reviewed the compliance report" |
| **Semantic** | Cross-session consolidated facts | Months to permanent | "The user prefers dark mode and metric units" |
| **Procedural** | Extracted patterns, instructions, and workflows | Permanent until superseded | "When deploying to staging, always run the integration tests first" |

**MemoryNode Entity:**

```
MemoryNode {
    content:                string       // The fact or observation
    memory_type:            enum         // WORKING | EPISODIC | SEMANTIC | PROCEDURAL
    confidence:             float        // 0.0–1.0, set by the extracting agent
    source_uri:             string       // Provenance pointer (URL, doc ID, session ID)
    valid_time:             timestamp    // When this fact was/is true in the real world
    embedding_model_version: string      // e.g., "openai/text-embedding-3-small/v1"
    session_id:             string       // Scoping for episodic retrieval

    // Managed by platform:
    // transaction_time, wall_clock_time, version, tenant_id, state
}
```

**MemoryEdge Entity:**

```
MemoryEdge {
    source_id:      string   // Origin node
    target_id:      string   // Target node
    relation_type:  enum     // SUPPORTS | CONTRADICTS | SUPERSEDES | DERIVED_FROM | RELATED_TO
    reasoning:      string   // Agent's explanation for this relationship
    confidence:     float    // Strength of the relationship
}
```

**State Machine (MemoryNode lifecycle):**

```
PENDING_EMBEDDING → ACTIVE → SUPERSEDED
                  → ACTIVE → STALE → PENDING_COMPILATION → ACTIVE (as Semantic)
                  → ACTIVE → FORGOTTEN (hard delete path for GDPR)
```

### 6.2. Conflict Resolution Strategy

When an agent detects a contradiction (e.g., a user's preference changes, a fact is corrected):

1. The agent creates a **new MemoryNode** with the updated fact.
2. The agent creates a **SUPERSEDES edge** from the new node to the old node, with a `reasoning` field explaining the change.
3. A **Cyoda Workflow Rule** detects the SUPERSEDES edge and transitions the old node from `ACTIVE` to `SUPERSEDED` within the same transaction.
4. `SUPERSEDED` nodes are excluded from default retrieval scopes but remain queryable for audit and point-in-time reconstruction.

This is a deterministic, database-enforced resolution path — not an LLM-driven heuristic. The audit ledger records the full transition narrative.

### 6.3. Memory Lifecycle — Decay & Compilation

Unbounded memory growth degrades retrieval quality and increases cost. Cyoda-Mem implements lifecycle management via workflow rules:

- **TTL-Based Decay:** Working and Episodic nodes that have not been accessed within a configurable window (default: 7 days for Working, 30 days for Episodic) transition to `STALE`.
- **Background Compilation:** A workflow rule matches `STALE` nodes and dispatches them to a compilation processor. The processor summarizes clusters of stale episodic memories into consolidated Semantic nodes, creates `DERIVED_FROM` edges for provenance, and transitions the originals to `COMPILED` (retained for audit) or deletes them per retention policy.
- **Reinforcement:** Each access (retrieval hit) resets the decay timer, implementing an Ebbinghaus-inspired retention curve where frequently-accessed memories persist naturally.

### 6.4. Embedding Strategy & Model Evolution

**Problem:** Embedding models change. Switching from `text-embedding-3-small` (1536d) to a future 4096d model invalidates similarity comparisons with existing vectors.

**Solution:**

- Every MemoryNode stores `embedding_model_version` in metadata.
- Queries filter to the current active model version by default, ensuring consistent similarity rankings.
- When a model is swapped, a **Re-index Workflow** queues all `ACTIVE` nodes with the legacy version, dispatches them to the embedding processor for re-vectorization, and updates both the vector and model version in an atomic transaction.
- During the migration window, a mixed-model query strategy falls back to metadata-only filtering for nodes not yet re-indexed, ensuring no retrieval gaps.

### 6.5. Workflows & Processors (State Management)

All heavy computation is decoupled from the write path:

| Trigger | Processor Action | State Transition |
|---|---|---|
| New node created | Fetch embedding from external API | `PENDING_EMBEDDING` → `ACTIVE` |
| SUPERSEDES edge created | Transition old node | `ACTIVE` → `SUPERSEDED` |
| Access TTL expired | Mark for review | `ACTIVE` → `STALE` |
| Stale nodes accumulated | Summarize into Semantic node | `STALE` → `PENDING_COMPILATION` → `COMPILED` |
| Model version mismatch | Re-embed with current model | `ACTIVE` → `ACTIVE` (vector updated) |

Processors are external gRPC workers (Python or Go) that receive entity payloads via Cyoda's CloudEventsService. They call external APIs (OpenAI, Cohere, local models), attach results to the entity, and commit state transitions. The processor execution modes (`SYNC`, `ASYNC_SAME_TX`, `ASYNC_NEW_TX`) let the application choose between strict transactional coupling and fire-and-forget.

### 6.6. AI Agent Interface — MCP Server

**Requirement:** A production-grade Model Context Protocol server exposing Cyoda-Mem's capabilities to any MCP-compatible LLM or agent framework.

**Scoping Model:**

- `TenantID` → Organization or user scope (enforced at platform level, not bypassable)
- `session_id` → Conversation or task scope (tracked in node metadata)
- `memory_type` → Taxonomy filter for retrieval

**Exposed Tools:**

| Tool | Purpose | Notes |
|---|---|---|
| `store_memory(content, memory_type, valid_time?, source_uri?)` | Create a new MemoryNode | Returns node ID. Embedding generated asynchronously. |
| `query_memory(query, memory_type_filter?, max_results?)` | Semantic hybrid search | Combines vector similarity with metadata filters. Returns ranked results with confidence scores. |
| `update_supersede(old_id, new_content, reasoning)` | Correct or update a fact | Creates new node + SUPERSEDES edge. Old node transitions automatically. |
| `forget_memory(node_id)` | Hard-delete a node and edges | GDPR Article 17 compliance. Irreversible. Audit record retained. |
| `link_memories(source_id, target_id, relation_type, reasoning)` | Create a MemoryEdge | For agent-driven graph construction. |
| `assemble_context(query, max_tokens, memory_type_filter?)` | Budget-aware context assembly | Fetches relevant nodes, traverses active edges, deduplicates, ranks by recency × relevance × confidence, and packs a context window bounded by the agent's token budget. Returns structured context, not raw dumps. |
| `get_timeline(node_id)` | Temporal audit trail | Returns the full version history of a node across both valid_time and transaction_time axes. |

**Design principle:** Every tool operates within the caller's tenant scope. There is no tool or parameter combination that can access another tenant's memory. This is enforced by the platform, not by the MCP server code.

## 7. Architecture

```
┌─────────────────────────────────────────────────────────┐
│  AI Agents (Claude, GPT, Local LLMs, Custom Agents)     │
│  ↕ Model Context Protocol (MCP)                         │
├─────────────────────────────────────────────────────────┤
│  Cyoda-Mem MCP Server                                   │
│  Tool routing · Tenant enforcement · Token budgeting     │
├─────────────────────────────────────────────────────────┤
│  Cyoda-Go EDBMS                                         │
│  Entity CRUD · Search (metadata + vector hybrid)         │
│  Workflow engine · State machine enforcement             │
│  Bi-temporal versioning · Audit ledger                   │
│  Multi-tenancy (structural, not conventional)            │
│  Transactions (SSI across all backends)                  │
├─────────────────────────────────────────────────────────┤
│  External Processors (gRPC)                              │
│  Embedding generation · Episodic compilation             │
│  Re-indexing · Custom enrichment                         │
├───────────┬──────────┬──────────────┬───────────────────┤
│ In-Memory │  SQLite  │  PostgreSQL  │  Cassandra 5      │
│ (KNN)     │(sqlite-  │  (pgvector)  │  (licensed,       │
│           │  vec)    │              │   + Redpanda)     │
│ Testing   │  Local   │  Production  │  Enterprise Scale │
└───────────┴──────────┴──────────────┴───────────────────┘
```

| Component | Layer | Technology | Responsibility |
|---|---|---|---|
| MCP Server | API | Go or Node.js | Agent-facing interface. Tool dispatch, tenant scoping, token budget enforcement. |
| App Config | Application | cyoda-mem YAML/JSON | MemoryNode/Edge schemas, workflow rules, TTL policies, taxonomy mappings. |
| Processors | Integration | gRPC Workers (Python/Go) | Async execution: embedding APIs, episodic-to-semantic compilation, re-indexing. |
| Cyoda-Go | Platform | Go EDBMS | All persistence, search, transactions, audit, multi-tenancy, workflows. |
| Storage (open) | Infrastructure | In-Memory / SQLite / PostgreSQL | Open-source backends. Selected at startup via `CYODA_STORAGE_BACKEND`. |
| Storage (licensed) | Infrastructure | Cassandra 5 + Redpanda | Proprietary backend for horizontal write scalability. Shard-based distributed TX coordination, I/O fencing, append-only point-in-time storage. Separate binary. |

## 8. Evaluation & Benchmarks

### 8.1. Benchmark Targets

Empirical validation is a **phase gate** — Cyoda-Mem must demonstrate competitive retrieval quality before broader investment.

| Benchmark | What It Tests | Current Leader | Cyoda-Mem Target | Rationale |
|---|---|---|---|---|
| **LongMemEval** | Temporal queries, knowledge updates, multi-session recall | Hindsight 91.4% | ≥85% | Bi-temporal `valid_time` architecture should excel at temporal reasoning and knowledge-update categories. |
| **LoCoMo** | Long-term context, contradiction resolution | Memori 81.95% | ≥85% | Deterministic SUPERSEDES workflow should outperform heuristic conflict resolution. |

### 8.2. Beyond Retrieval Benchmarks

Current benchmarks (LoCoMo, LongMemEval) only measure retrieval from conversational histories. They do not measure what matters most for Cyoda-Mem's target market:

- **Concurrency correctness:** Do concurrent agent writes produce consistent memory state? (Custom test suite required.)
- **Audit completeness:** Can every memory state be reconstructed at any point in time? (Custom test suite required.)
- **Token efficiency:** What is the context assembly overhead vs. full-context prompting? (Measure tokens-per-query against Memori's 4.97% baseline.)

These custom evaluation suites must be developed alongside v1 and published as part of the project's credibility strategy.

## 9. Phasing & Milestones

### Phase 1: Platform Foundation
- PRD 1 implementation: Vector search in SPI, pgvector plugin, sqlite-vec plugin, in-memory KNN.
- Parity test suite across all three backends.
- **Gate:** All parity tests green. Vector search available for any Cyoda-Go application.

### Phase 2: Core Memory Application
- MemoryNode/Edge entity definitions and state machine.
- PENDING_EMBEDDING workflow with external processor.
- SUPERSEDES conflict resolution workflow.
- Basic MCP server: `store_memory`, `query_memory`, `update_supersede`, `forget_memory`.
- **Gate:** End-to-end demo — an agent stores, queries, supersedes, and forgets memories via MCP on both SQLite and PostgreSQL.

### Phase 3: Retrieval Quality & Compilation
- `assemble_context` tool with token budgeting and edge traversal.
- Memory lifecycle workflows (TTL decay, episodic compilation).
- Embedding re-index workflow.
- **Gate:** LongMemEval and LoCoMo benchmark runs. Results must meet or exceed targets in §8.1.

### Phase 4: Hardening & Go-to-Market
- Concurrency stress testing (multi-agent write storms).
- Audit trail completeness validation.
- Documentation, examples, and developer guides.
- Public benchmark publication and competitive positioning.
- **Gate:** Stakeholder go/no-go on broader investment based on benchmark results and developer feedback.

## 10. Risks & Mitigations

| Risk | Severity | Mitigation |
|---|---|---|
| Benchmark scores don't meet targets | High | Phase 3 gate kills broader investment early. Core vector search (PRD 1) retains value regardless. |
| Memori or Hindsight ships SERIALIZABLE isolation | Medium | Neither has shown interest in this direction; their architectures don't support it natively. Move fast. |
| PostgreSQL single-writer ceiling reached under heavy multi-agent workloads | Low | Licensed Cassandra 5 backend provides horizontal write scalability via shard-based distributed TX coordination. This is a proven architecture adapted from the Cyoda Cloud platform — not speculative. Upgrading requires only changing `CYODA_STORAGE_BACKEND` and providing Cassandra/Redpanda infrastructure. |
| sqlite-vec maturity issues | Medium | Parity test suite catches behavioral drift. Exact KNN has fewer failure modes than ANN. Fallback: in-memory with periodic disk snapshots. |
| MCP protocol evolves in breaking ways | Low | MCP server is a thin wrapper over Cyoda-Go REST/gRPC APIs. Re-implementing the MCP layer is low-cost. |
| "Knowledge compilation" pattern (Karpathy/MemPalace) becomes the dominant paradigm, making entity-level memory less relevant | Medium | Cyoda-Mem's episodic-to-semantic compilation workflow is the database-backed equivalent of the LLM Wiki pattern. Position this explicitly in marketing. The compilation processor can implement any summarization strategy, including wiki-style page generation. |
| Adoption friction — enterprise buyers want managed service, not self-hosted | High | v1 targets self-hosted and developer-led adoption. Managed service is a post-validation consideration. |
| Cassandra vector search implementation complexity | Medium | The Cassandra plugin team owns this decision independently. The SPI contract isolates it from the application layer. If Cassandra-native vector search (SAI) proves insufficient, a sidecar approach or brute-force over the entity store are fallbacks. Cyoda-Mem on PostgreSQL is the primary v1 production target; Cassandra vector support can trail. |

## 11. Out of Scope (v1)

- **Raw Document Storage:** Cyoda-Mem stores extracted, structured facts. Source documents (PDFs, videos, large text files) remain in external object storage (S3, local disk). MemoryNodes store URI pointers via `source_uri`.
- **Multi-Modal Memory:** Image, audio, and video embeddings are deferred. The schema supports them (the embedding column is type-agnostic), but v1 processors only handle text.
- **Managed Cloud Service:** v1 is self-hosted only. A managed offering requires operational maturity and is a post-validation investment.
- **Advanced Multi-Strategy Retrieval:** Hindsight's four-parallel-strategy approach (semantic + BM25 + graph traversal + temporal) is out of scope for v1. Cyoda-Mem v1 uses vector similarity + metadata filtering. Multi-strategy retrieval is a Phase 5 enhancement if benchmarks show it's needed.
- **Built-in LLM Inference:** Cyoda-Mem is a memory backend, not an agent runtime. It does not host or invoke LLMs directly. Embedding generation and knowledge compilation are delegated to external processors.
- **Karpathy-Style Wiki Export:** While the episodic-to-semantic compilation pipeline is architecturally compatible with generating wiki-style markdown artifacts, a dedicated wiki rendering layer is deferred to v2.
- **Cassandra Vector Search Design:** The proprietary Cassandra plugin's vector search implementation (SAI-based ANN, sidecar index, or brute-force) is a separate design decision owned by the Cassandra plugin team. The SPI contract guarantees application-layer compatibility regardless of approach.

## 12. Success Criteria

| Criteria | Threshold | Measured By |
|---|---|---|
| LongMemEval score | ≥85% | Phase 3 benchmark run |
| LoCoMo score | ≥85% | Phase 3 benchmark run |
| Concurrent agent correctness | Zero data corruption under 50 concurrent writing agents | Custom stress test |
| Audit reconstruction | 100% of memory states reconstructable at any historical point | Custom temporal query suite |
| Backend parity | Identical result ordering on all open-source backends (PostgreSQL, SQLite, In-Memory) for same dataset. Cassandra parity validated separately in licensed build. | Automated parity test suite |
| Local startup time | < 2 seconds from binary launch to first MCP tool response on SQLite | Performance benchmark |
| Token efficiency | ≤10% of full-context token footprint per query | assemble_context measurement vs. baseline |

## 13. Decision Required

Stakeholders are asked to approve:

1. **PRD 1 — Immediate start.** Low-risk platform enhancement with value beyond memory.
2. **PRD 2 — Phased approval with gates.** Approve Phases 1–2. Phase 3 benchmark results serve as the go/no-go gate for Phase 4 and broader investment. If benchmarks fall short, the investment is capped and the vector search platform enhancement still delivers standalone value.

---

*End of document.*
