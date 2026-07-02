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
  - errors
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

`CYODA_CLUSTER_ENABLED=false` is the default for easy onboarding, not an indication that cluster/HA features are secondary. Multi-node correctness is a primary design target.

## TOPOLOGY

Any node can serve any HTTP or gRPC request. The load balancer does not need session affinity for stateless requests. For requests carrying a transaction-routing token (see `TRANSACTION ROUTING`), the in-process proxy forwards to the node that owns the transaction.

PostgreSQL is the only stateful component. Cluster size is bounded below by quorum-free PostgreSQL replication (typically 3 cyoda nodes minimum for HA) and above by PostgreSQL connection-pool capacity (typically 10 nodes maximum; see `config.database` for connection-pool tuning).

## DISCOVERY

Peer discovery uses SWIM gossip via HashiCorp `memberlist`. Cluster membership is eventually consistent across nodes. New nodes join via a seed-list — at least one peer's `host:gossip_port`. Nodes leave gracefully on SIGTERM and are evicted by gossip after a configurable suspect-then-confirm timeout if they crash.

The gossip protocol is operationally invisible — there are no per-message logs at INFO level. `memberlist`'s own log output is routed to `slog` at DEBUG.

## TRANSACTION ROUTING

PostgreSQL transactions are bound to the connection that begins them (`pgx.Tx` is single-owner). When a node begins a transaction, it issues a routing token containing the owning node's identity, an opaque transaction reference, and an expiry, signed with HMAC-SHA256 keyed on `CYODA_HMAC_SECRET`. The token is returned to the client (HTTP header `X-Tx-Token`, gRPC metadata key `tx-token`) and replayed on subsequent requests against that transaction.

The HTTP and gRPC frontends inspect the token, verify the HMAC, and either handle the request locally (token's owner is this node) or reverse-proxy to the owning node. Failure modes:

- Token signature mismatch, malformed token, or expired token — `400 Bad Request` (codes `BAD_REQUEST` / `TRANSACTION_EXPIRED`); the client must restart the transaction.
- Owner node not in the registry, marked dead by gossip, or unreachable from the proxy — `503 Service Unavailable` (code `TRANSACTION_NODE_UNAVAILABLE`); PostgreSQL has already aborted the connection's transaction on the dead node, so the client retries from scratch. Fail-closed semantics, no orphaned transactions.

`CYODA_HMAC_SECRET` is a deployment secret. All nodes in a cluster must share the same value; it is also the root key for peer-to-peer dispatch authentication (HKDF-derived AEAD), so rotating it requires a cluster-wide restart.

## OPERATIONS

- **Growing the cluster.** Start a new node with the same `CYODA_HMAC_SECRET` and a seed-list pointing at any existing node. Gossip propagates membership within seconds. The load balancer's health checks (against `/readyz`) decide when to send traffic.
- **Shrinking the cluster.** Send SIGTERM. The node finishes in-flight requests, declares itself dead via gossip, and exits. Outstanding transactions owned by the departing node abort cleanly via PostgreSQL connection close.
- **Rolling restart.** Restart one node at a time, waiting for `/readyz` to report ready before moving on. Transactions in flight on the restarting node abort; clients retry.
- **Network partitions.** A node partitioned from peers but still reachable from PostgreSQL continues to serve requests; gossip-level membership is best-effort and does not gate request handling. A node partitioned from PostgreSQL continues to pass `/readyz` (the current readiness check is a static initialization flag, not a live store probe — see `app/app.go ReadinessCheck`); individual requests fail at query time and the client retries. The full partition analysis (5 phases, dispatch and CRUD-callback paths) is in `docs/ARCHITECTURE.md` §4.5.

## COMPUTE CALLBACK TRANSACTION ROUTING

When the workflow engine dispatches to a compute node it mints a signed tx-token
and includes it as the `cyodatxtoken` CloudEvent extension attribute. The compute
node MUST echo this token on every callback:

- HTTP CRUD callbacks: `X-Tx-Token` request header
- gRPC EntityManage callbacks: `tx-token` metadata key

The receiving node verifies the token's HMAC and routes the callback to the
transaction-owning node (same proxy mechanism as `TRANSACTION ROUTING` above).
Without the echo the callback runs in a standalone transaction and cannot see
the cascade's uncommitted writes. Callback acks are provisional until the
owning transaction commits.

See `workflows` and `docs/PROCESSOR_EXECUTION_MODES.md` for mode-specific
semantics (`SYNC`, `ASYNC_NEW_TX`, `COMMIT_BEFORE_DISPATCH`).

## COMPOSITE UNIQUE KEY STALENESS

Composite unique keys are part of the model descriptor. They inherit the descriptor's existing cross-cluster coherence: when a model is locked or unlocked, the node that performs the operation invalidates the local model cache and gossip-broadcasts `topicModelInvalidate` so every other node evicts and reloads the descriptor (keys included).

**Changing a key on a live multi-node postgres deployment** requires a destructive teardown: unlock the model (requires zero live entities, so all entities must be deleted first), change the key definitions, then relock. This is an inherently disruptive operation. There is a bounded window during which a node that missed both the unlock and relock gossip messages may still enforce the **old** key set. Data written through that node in the window will carry claim rows under the old key IDs.

**Operator guidance:** after changing a composite unique key on a multi-node deployment, pause writes and allow the cluster to settle for at least one cache-lease TTL before resuming. The cache TTL is the backstop that guarantees every node has reloaded the new descriptor.

The proper fix — acknowledged model-cache invalidation, where the relock waits for every online node to confirm eviction before completing — is a planned cluster enhancement and is out of scope for v0.8.2.

**Scope:** this limitation applies to multi-node postgres only. The memory and sqlite backends are single-process; their cache invalidation is synchronous.

## SEE ALSO

- `config.database` — PostgreSQL is the only multi-node-capable backend
- `config.auth` — `CYODA_HMAC_SECRET` configuration
- `run` — server lifecycle
- `quickstart` — first-run defaults
- `helm` — Kubernetes deployment of multi-node clusters
- `errors` — per-code error reference
