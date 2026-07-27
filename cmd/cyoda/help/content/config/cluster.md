---
topic: config.cluster
title: "cyoda cluster & dispatch configuration"
stability: stable
see_also:
  - config
  - run
---

# config.cluster

## NAME

config.cluster — multi-node clustering, gossip, and cross-node dispatch env vars.

## DESCRIPTION

- `CYODA_CLUSTER_ENABLED` (bool, default: `false`) — enable multi-node clustering.
- `CYODA_NODE_ID` (string, default: unset) — unique node identifier; required when `CYODA_CLUSTER_ENABLED=true`; any non-empty string is accepted.
- `CYODA_NODE_ADDR` (string, default: `http://localhost:8080`) — this node's HTTP base URL; must include scheme (`http://` or `https://`).
- `CYODA_GRPC_NODE_ADDR` (string, default: unset) — this node's gRPC endpoint advertised to peers (`host:port`, no scheme). When set, peers dial this address for cross-node gRPC callback forwarding. When unset, peers derive the gRPC address from this node's HTTP host plus their own `CYODA_GRPC_PORT` (uniform-deployment default).
- `CYODA_GOSSIP_ADDR` (string, default: `:7946`) — gossip protocol listen address; format `[host]:port` — parsed via `net.SplitHostPort`; invalid format causes startup failure.
- `CYODA_GOSSIP_STABILITY_WINDOW` (duration, default: `2s`) — gossip stability window.
- `CYODA_SEED_NODES` (string, default: empty) — comma-separated list of seed node addresses (e.g., `node1.example.com:7946,node2.example.com:7946`); empty means single-node or seed-discovery handled externally.
- `CYODA_HMAC_SECRET` (string, default: unset) — hex-encoded HMAC secret for inter-node dispatch authentication; required when `CYODA_CLUSTER_ENABLED=true`. Supports `_FILE` suffix.
- `CYODA_PROXY_TIMEOUT` (duration, default: `30s`) — request proxy timeout.
- `CYODA_DISPATCH_WAIT_TIMEOUT` (duration, default: `5s`) — how long the dispatcher polls gossip for a compute member with matching tags.
- `CYODA_DISPATCH_FORWARD_TIMEOUT` (duration, default: `30s`) — HTTP timeout for the cross-node forwarding call.
- `CYODA_TX_TOKEN_TTL` (duration, default: `90s`) — TTL of the signed transaction routing token minted on processor/criteria dispatch; must be ≥ `CYODA_DISPATCH_FORWARD_TIMEOUT` so the token remains valid through the full round-trip and callback verification, including the forwarded-chain case where two budgets stack.
- `CYODA_KEEPALIVE_INTERVAL` (int, default: `10`) — keep-alive send interval in seconds.
- `CYODA_KEEPALIVE_TIMEOUT` (int, default: `30`) — keep-alive timeout in seconds.

## SEE ALSO

- config
- run
