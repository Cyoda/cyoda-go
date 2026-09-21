---
topic: config.cluster
title: "cyoda cluster & dispatch configuration"
stability: stable
see_also:
  - config
  - config.grpc
  - run
---

# config.cluster

## NAME

config.cluster — multi-node clustering, gossip, and cross-node dispatch env vars.

## DESCRIPTION

- `CYODA_CLUSTER_ENABLED` (bool, default: `false`) — enable multi-node clustering.
- `CYODA_NODE_ID` (string, default: unset) — unique node identifier; required when `CYODA_CLUSTER_ENABLED=true`; any non-empty string is accepted. Together with `CYODA_NODE_ADDR` and `CYODA_GRPC_NODE_ADDR` it must fit the 512-byte gossip metadata (about 420 bytes for the three values together; 89 bytes are framing and the longest list version); a node whose identity does not fit refuses to start.
- `CYODA_NODE_ADDR` (string, default: `http://localhost:8080`) — this node's HTTP base URL; must include scheme (`http://` or `https://`).
- `CYODA_GRPC_NODE_ADDR` (string, default: unset) — this node's gRPC endpoint advertised to peers (`host:port`, no scheme). When set, peers dial this address for cross-node gRPC callback forwarding. When unset, peers derive the gRPC address from this node's HTTP host plus their own `CYODA_GRPC_PORT` (uniform-deployment default).
- `CYODA_GOSSIP_ADDR` (string, default: `:7946`) — gossip protocol listen address; format `[host]:port` — parsed via `net.SplitHostPort`; invalid format causes startup failure.
- `CYODA_GOSSIP_STABILITY_WINDOW` (duration, default: `2s`) — gossip stability window.
- `CYODA_SEED_NODES` (string, default: empty) — comma-separated list of seed node addresses (e.g., `node1.example.com:7946,node2.example.com:7946`); empty means single-node or seed-discovery handled externally.
- `CYODA_HMAC_SECRET` (string, default: unset) — hex-encoded HMAC secret for inter-node dispatch authentication; required when `CYODA_CLUSTER_ENABLED=true`. Supports `_FILE` suffix. Single root secret for gossip encryption, dispatch AEAD, and tx-token signing; no versioned-key rotation — changing it requires a full-cluster stop/start (see the `cluster` help topic, `SECRET ROTATION`).
- `CYODA_PROXY_TIMEOUT` (duration, default: `30s`) — request proxy timeout.
- `CYODA_DISPATCH_WAIT_TIMEOUT` (duration, default: `5s`) — the patience: how long one callout waits, in total, for a compute member with matching tags to exist. It applies on a single node as in a cluster and whatever the callout's `retryPolicy` — waiting for a member to exist is not a retry and costs no try. The wait ends the moment a member attaches or a peer announces one. `0` disables waiting: a callout with no member fails at once with `NO_COMPUTE_MEMBER_FOR_TAG`. Must not be negative; startup fails otherwise.
- `CYODA_DISPATCH_CONNECT_TIMEOUT` (duration, default: `2s`) — time allowed to open the connection when a callout is handed over to another node. A node that cannot be connected to costs no try; the next one is asked. Behind a sidecar or an ingress the connection always opens and a dead peer shows as a 502–504, which counts as one try. Must be `> 0`; startup fails otherwise.
- `CYODA_CALLOUT_HANDOVER_ALLOWANCE` (duration, default: `30s`) — what the owning node allows a hand-over on top of `tries left × answer limit`, and the last term of a callout's overall deadline (`tries × answer limit + CYODA_DISPATCH_WAIT_TIMEOUT + this`; 155 s at the defaults). Must be `> 0`; startup fails otherwise.
- `CYODA_CALLOUT_PASS_ALLOWANCE` (duration, default: `30s`) — how long the transaction token given to a compute member outlives its try's answer limit: the margin for routing a callback between nodes and for clocks that differ between them. Must be `> 0`; startup fails otherwise.
- `CYODA_DISPATCH_FORWARD_TIMEOUT` (duration, default: `30s`) — whole-request timeout of the node-to-node call that delegates a scheduled transition to another node. It does not govern callout hand-overs. Must be `> 0`; startup fails otherwise.

The tries and answer-limit settings for a callout are in `config grpc`.

## SEE ALSO

- config
- config.grpc
- run
