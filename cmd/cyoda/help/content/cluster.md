---
topic: cluster
title: "cluster — multi-node topology and operations"
stability: stable
see_also:
  - config.database
  - config.auth
  - config.cluster
  - config.grpc
  - grpc
  - workflows
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
2–20 stateless cyoda nodes
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

Each node announces, in its gossip metadata, who it is — its id and its HTTP and gRPC addresses — and the version of the list of compute tags it hosts. The list itself, one entry per tenant with a compute node attached, is sent to every peer over the membership layer's reliable (TCP) channel whenever it changes, and a node that finds it holds a different version from the one a peer announces asks that peer for it. One mechanism covers a lost message, a node that joins late, a node restarted under the same id, and a healed partition. The number of tenants and tags a node can host is not limited by the membership layer.

The metadata is limited to 512 bytes by `memberlist`. Its size depends only on `CYODA_NODE_ID`, `CYODA_NODE_ADDR` and `CYODA_GRPC_NODE_ADDR`; a node whose identity would not fit refuses to start and names the three settings. All membership traffic, the tag lists included, is encrypted with `CYODA_HMAC_SECRET`.

The gossip protocol is operationally invisible — there are no per-message logs at INFO level. `memberlist`'s own log output is routed to `slog` at DEBUG.

## TRANSACTION ROUTING

PostgreSQL transactions are bound to the connection that begins them (`pgx.Tx` is single-owner). When a node gives a callout to a compute member it mints a token that names the owning node, an opaque transaction reference, the callout and the try it belongs to, and an expiry, signed with HMAC-SHA256 keyed on `CYODA_HMAC_SECRET`. The compute member echoes it on its callbacks (HTTP header `X-Tx-Token`, gRPC metadata key `tx-token`). A token lives for its try's answer limit plus `CYODA_CALLOUT_PASS_ALLOWANCE`.

The HTTP and gRPC frontends inspect the token, verify the HMAC, and either handle the request locally (token's owner is this node) or reverse-proxy to the owning node. Failure modes:

- Signature mismatch or malformed token — `401 UNAUTHORIZED`. Expired token — `410 TRANSACTION_EXPIRED`.
- Owner node not in the registry, marked dead by gossip, or unreachable from the proxy — `503 Service Unavailable` (code `TRANSACTION_NODE_UNAVAILABLE`); PostgreSQL has already aborted the connection's transaction on the dead node, so the client retries from scratch. Fail-closed semantics, no orphaned transactions.

`CYODA_HMAC_SECRET` is a deployment secret. All nodes in a cluster must share the same value; it is also the root key for peer-to-peer dispatch authentication (HKDF-derived AEAD), so rotating it requires a cluster-wide restart — see `SECRET ROTATION`.

## OPERATIONS

- **Growing the cluster.** Start a new node with the same `CYODA_HMAC_SECRET` and a seed-list pointing at any existing node. Gossip propagates membership within seconds. The load balancer's health checks (against `/readyz`) decide when to send traffic.
- **Shrinking the cluster.** Send SIGTERM. The node finishes in-flight requests, declares itself dead via gossip, and exits. Outstanding transactions owned by the departing node abort cleanly via PostgreSQL connection close.
- **Rolling restart.** Restart one node at a time, waiting for `/readyz` to report ready before moving on. Transactions in flight on the restarting node abort; clients retry.
- **Network partitions.** A node partitioned from peers but still reachable from PostgreSQL continues to serve requests; gossip-level membership is best-effort and does not gate request handling. A node partitioned from PostgreSQL continues to pass `/readyz` (readiness is a static initialization flag plus the panic-recovery health flag, not a live store probe — see `app/app.go ReadinessCheck`); individual requests fail at query time and the client retries. The full partition analysis (5 phases, dispatch and CRUD-callback paths) is in `docs/ARCHITECTURE.md` §4.5.

## CALLOUTS ACROSS NODES

A callout (processor, criterion or scheduled function) is run by the node that holds the operation's transaction — the owner. The owner first tries its own matching compute members, one after another. If that does not produce an answer and tries are left, it **hands the callout over** to one peer that advertises the tag, together with the number of tries left, the answer limit and the request id. The peer tries its own members only; it never hands on. The owner then asks the next such peer, and when nobody anywhere has a matching member it waits — up to `CYODA_DISPATCH_WAIT_TIMEOUT` in total — for one to attach or for a peer to announce one.

What counts as a try:

- A peer that **cannot be connected to** within `CYODA_DISPATCH_CONNECT_TIMEOUT`, or that answers that it handed the work to nobody, costs no try; the next peer is asked.
- A hand-over whose **answer is lost** — no reply, a broken connection, a non-2xx status, an answer that does not authenticate — counts as one try, because the peer may have given the work to a member. For a processor not declared `idempotent` nothing else is tried and the operation fails with `503 DISPATCH_FORWARD_FAILED`.
- Every hand-over opens a connection of its own, so "could not connect" is the only case in which a dead peer costs nothing. Behind a sidecar or an ingress the connection always opens and a dead peer shows as a `502`–`504`; with an `https://` node address a failed TLS handshake is not a connect failure either. Both count as a lost answer — the safe side.

The number of tries is therefore the normal number, not a hard limit. The time is: see `cyoda help config cluster` (`CYODA_CALLOUT_HANDOVER_ALLOWANCE`) and `cyoda help config grpc`.

Node clocks more than 30 seconds apart make a peer refuse a hand-over before reading it. That refusal cannot be authenticated, so it counts as a lost answer: clocks that far apart fail operations whose processors are not `idempotent` rather than being routed around. Keep node clocks synchronised.

## SECRET ROTATION

`CYODA_HMAC_SECRET` is the single root secret for three primitives: gossip encryption, inter-node dispatch authentication (HKDF-derived AES-256-GCM), and transaction-routing token signing. There is no versioned-key support, so nodes holding different secrets cannot interoperate: a node started with a mismatched secret fails its gossip join and exits at startup, and dispatch requests between mismatched nodes are rejected with `403`. Failure is loud — never a silent split-brain.

Rotating the secret therefore requires full-cluster downtime: stop all nodes, update the secret everywhere, start the cluster again. A rolling restart across a secret change is not possible. In-flight transactions and routing tokens do not survive the restart; clients retry from scratch.

## DISPATCH REPLAY PROTECTION

A callout handed over to another node travels as an AES-256-GCM envelope, and so does the answer. Both are keyed from `CYODA_HMAC_SECRET`. A request binds its direction, method, path, timestamp and the id of the node it is sealed for; an answer binds its direction, the path, the timestamp, that same node and the nonce of the one request it answers — so an envelope cannot be replayed onto another endpoint, reflected back in the other direction, moved onto another request, or delivered to a different node of the cluster, which holds the same key and would otherwise answer it. The node id is not sent: the sender names the node whose address it looked up and the receiver names itself, so an envelope opens only on the node it was meant for. This is why `CYODA_NODE_ID` must be distinct on every node of a cluster — two nodes sharing one id can open each other's hand-overs. A duplicate is not left to be discovered: a node whose id a live node of the cluster already holds refuses the join and exits, naming `CYODA_NODE_ID`, the address the id was found at and the seed it was learned from, while the node already holding the id keeps serving. A node that crashed rather than leaving gracefully, and comes back at another address, is refused the same way: its peers still hold the record of its previous life, and nothing distinguishes that record from a second node's — it starts once its peers have reaped the stale record, and the message names this cause alongside the other. The check happens during the join exchange with a seed, so it is a strong detector, not a proof — it cannot fire when the seed does not yet know the incumbent, as when two nodes start at the same instant or a partition heals, nor when no seeds are configured. A node that sees one id claimed from two addresses logs that at ERROR, once per address; it does not stop, because seeing the conflict does not say which of the two is the misconfigured one, and stopping a healthy node costs availability for no correctness gain. Every hand-over opens a connection of its own.

The entity's payload travels base64-encoded, byte for byte, in both directions: it is what the store holds and what the compute member is handed, and a re-encoding on the way would drop whitespace and rewrite characters in a tenant's stored data. The envelope ceiling follows from that — the 10 MiB an entity write may carry, base64-encoded, plus room for the entity's meta, the processor definition or criterion, the roles and the tags — so an entity the API accepts can always be handed over. A body above the ceiling is refused before anything is sent: the callout fails without using a try rather than being retried identically on every node.

Each node keeps an in-memory replay cache of request nonces: entries live for 60 seconds (twice the 30-second timestamp-skew window) and the cache holds at most 100 000 nonces, per node. The cache is fail-closed: when it is full, or a nonce repeats, the request is refused; the duplicate check runs first, so a replay is never turned into something answerable just because the cache also happens to be full. A repeated nonce authenticates like any other request but gets a bare `403` anyway — a replay must not be confirmed under any seal — which the owner cannot trust and reads as a lost answer (`DISPATCH_FORWARD_FAILED`). A request refused only for the cache being full is answered — under seal — that nothing was handed to a compute member; the node that sent the hand-over asks the next peer and no try is used. Such a refusal records no nonce, so the node also remembers the refused request's timestamp and refuses every later request stamped at or before it whose nonce it does not hold — otherwise the refused hand-over would simply be accepted once the cache had room, which is all an attacker holding a captured one has to wait for. A genuine hand-over caught by that rule is refused the same way, and costs no try either. Answers need no cache: each opens only under the nonce of a request the owner itself chose, and is protected the same way requests are — encrypted, with a nonce of its own, bound to the request it answers — but never enters this cache. The ceiling admits roughly 1 600 sustained inbound hand-overs per second per node — far above realistic callout rates; reaching it indicates a flood, not normal load.

The cache lives in memory only, so a node restarted inside the 30-second skew window accepts a replay of a request its previous life ran. Reaching that needs an attacker on the network between nodes holding the cluster secret's traffic and a restart inside the window; binding a node's lifetime into the seal instead would refuse every hand-over to a node whose membership has not yet gossiped after a restart, which is a far more likely failure.

## COMPUTE CALLBACK TRANSACTION ROUTING

The node that gives a callout to a compute member mints a signed token for that
try and includes it as the `cyodatxtoken` CloudEvent extension attribute — also
when the callout was handed over, in which case the token still names the
owner. The compute member MUST echo this token on every callback:

- HTTP CRUD callbacks: `X-Tx-Token` request header
- gRPC EntityManage callbacks: `tx-token` metadata key

The receiving node verifies the token's HMAC and routes the callback to the
transaction-owning node (same proxy mechanism as `TRANSACTION ROUTING` above).
Without the echo the callback runs in a standalone transaction and cannot see
the cascade's uncommitted writes. Callback acks are provisional until the
owning transaction commits.

The owner admits a callback only while the token's callout is in progress and the token belongs to the member that currently has the work. A callback from a member that was replaced, or whose callout has ended, is refused with `410 CALLOUT_SUPERSEDED` while the transaction is open, and with `404 TRANSACTION_NOT_FOUND` afterwards. Before the owner gives the work to the next member, and before the workflow carries on after a callout, it waits for any callback still in progress on the transaction to finish. Callbacks of one transaction are served one at a time.

A callback's request and its answer are both held in memory on the owner while the transaction is held, each under a 10 MiB ceiling: an over-size request body is refused with `413` (HTTP), and an answer that would pass the ceiling fails the callback with a ticketed `500` rather than being cut short. Page a large read instead. A callback still waiting its turn when its member goes away is dropped — it has touched nothing; one that already has the transaction runs to completion. This is logged at `DEBUG` and carries no ticket: nothing was wrong on the server, and there is nobody left to quote a ticket to.

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
- `config.cluster` — cluster settings, including the callout hand-over ones
- `config.grpc` — tries, answer limit, the PostgreSQL ceiling
- `grpc` — the callout envelope, retries and callback routing
- `workflows` — `retryPolicy`, `idempotent`, and what a failed callout leaves behind
- `run` — server lifecycle
- `quickstart` — first-run defaults
- `helm` — Kubernetes deployment of multi-node clusters
- `errors` — per-code error reference
