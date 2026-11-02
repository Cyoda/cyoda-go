# Cluster status, node lifecycle and health probes — Cloud twin-alignment spec

This document is the contract Cyoda Cloud implements to stay aligned with
cyoda-go's cluster status read, its node lifecycle states, its gRPC health
service and its readiness during shutdown. cyoda-go is the authoritative
implementation.

## 1. The cluster status read

`GET /platform/cluster` (under the context path, `/api` by default) lists
every node of the cluster and says whether the cluster is operational. It is
served over HTTP only; there is no CloudEvent for it.

**Authorization.** A platform operator only: `ROLE_ADMIN` in the tenant
`PLATFORM`. `ROLE_M2M` is not required. An on-behalf-of token is refused
whatever its roles.

**Parameters** (query, both optional):

| Parameter | Type | Range | Meaning |
|---|---|---|---|
| `waitForNodes` | int32 | 1 to 1000 | Answer once at least this many nodes are `UP` and `NORMAL`. Absent, the answer follows one round. |
| `timeoutMs` | int32 | 1 to 600000, default 30000 | The longest wait for `waitForNodes`. Given without `waitForNodes`, it is refused. |

**A round.** The node that serves the request lists every member of its
membership directory, itself included, and asks each one for its status, all
in parallel. A member that answers is `UP`, with its state, database check,
version and clock setting from the answer. A member that does not answer — no
answer within the ask bound, a connection that does not open, an answer that
does not authenticate or validate, or no usable address — is `DOWN`, with the
state its membership metadata names, or no state when that metadata does not
parse or names no valid state. A node that has left the directory is not listed. Without clustering
the one entry has the id `local` and is read in process.

**The wait.** With `waitForNodes`, the node runs rounds until one has at least
that many `UP`/`NORMAL` nodes. It runs a new round whenever the directory
changes, and at least once a second. When `timeoutMs` has passed, the answer
is the last completed round; a round still running at that moment is called
off and does not count. The first round always completes, so the answer comes
at the later of `timeoutMs` and the end of the first round. A client that
leaves during the wait gets no answer.

**The body**, `ClusterStatusDto`, is the same for `200` and `503`. It is a
status, not a problem document: no ticket, no detail.

- `nodes` — one item per member, in ascending `id` order: `id`,
  `reachability` (`UP` or `DOWN`), `state` (absent when unknown), and, for an
  `UP` node only, `database` (the node's store answered its check), `version`
  (the node's build version) and `clock` (`controllable`, `version`, `writer`:
  the node's platform clock setting without its times; `false`, `0`, `""` on a
  node without clock control).
- `counts` — one item per combination of `reachability` and `state` present,
  with its `count` (at least 1): `UP` before `DOWN`, then `STARTING`, `NORMAL`,
  `DRAINING`, `UNHEALTHY`, the item without a state last. A combination no
  node has is absent.
- `operational` — true only when there is at least one node, every node is
  `UP` and `NORMAL` with `database` true, all nodes have the same `version`
  and the same `clock`, and, with `waitForNodes`, at least that many nodes are
  `UP` and `NORMAL`. An extra `DOWN` node therefore makes the cluster not
  operational even when `waitForNodes` is met.

The body never holds node addresses, peer URLs or error text.

| Status | Code | When |
|---|---|---|
| 200 | — | `operational` is true |
| 503 | — | `operational` is false; the same body as the 200 |
| 400 | `BAD_REQUEST` | a query string that does not parse; an undeclared parameter; a parameter given more than once; a value that is not an integer or is outside the 32-bit range; `waitForNodes` outside 1 to 1000; `timeoutMs` outside 1 to 600000; `timeoutMs` without `waitForNodes`. Except for a query string that does not parse, `properties.parameter` names the parameter |
| 401 | `UNAUTHORIZED` | no token, or a token that does not authenticate |
| 403 | `FORBIDDEN` | not a platform operator ("platform operator required"), or an on-behalf-of token ("on-behalf-of tokens cannot administer") |

**The ask between nodes** is cyoda-go's own: a sealed peer request, bounded by
`CYODA_PROXY_TIMEOUT`, and the answering node bounds its database check by
half of its own `CYODA_PROXY_TIMEOUT`. How Cloud nodes ask each other is
Cloud's own; the bounds and the answer above are the contract.

## 2. Node lifecycle states

Every node is in one of four states. The node sets them itself; no transition
is undone, and the precedence is `UNHEALTHY` > `DRAINING` > `STARTING` >
`NORMAL`.

| State | From |
|---|---|
| `STARTING` | the node joins the cluster, until it has finished building |
| `NORMAL` | the node has finished building |
| `DRAINING` | the start of shutdown |
| `UNHEALTHY` | the first panic recovered in engine or store work |

A node publishes its state in its membership metadata, so a peer can name the
state of a node that does not answer. A node publishes `DRAINING` before it
leaves the cluster. A `STARTING` node does not yet serve the status ask, so it
is always listed `DOWN`; so is a `NORMAL` node whose listeners are not yet
open.

## 3. Readiness during shutdown

- `/readyz` (the admin listener's readiness probe) fails after a recovered
  panic and from the start of shutdown, so a load balancer
  stops sending the node new requests while it drains. The body stays
  generic; the reason goes to the server log.
- `GET /health` reflects the recovered-panic condition only: it stays
  `200 {"status":"UP"}` while the node drains.
- `/livez` is unconditional.

## 4. gRPC health

The gRPC listener serves the standard `grpc.health.v1.Health` service, for
probes such as `grpc_health_probe` and Kubernetes `grpc` probes.

- Only `Check` is served, and only for the empty service name `""`; any other
  service name answers `NOT_FOUND`.
- `Check` answers `SERVING` while `/readyz` would answer `200`, and
  `NOT_SERVING` otherwise. The answer carries no reason.
- `Check` needs no token and no role: a probe cannot authenticate. It is the
  only gRPC method served without a token.
- `Watch` and `List` are not implemented. They require a token and `ROLE_M2M`
  like every other method, and then answer `UNIMPLEMENTED`.

## 5. What Cloud does

Cloud has none of this today. A search of Cloud's backend (Kotlin and Java
sources under `backend/src/main`) finds no cluster status read, no node
lifecycle states and no `grpc.health.v1` service; its readiness is Spring
Actuator's `/actuator/health/readiness`. Cloud implements:

- `GET /platform/cluster` with the authorization, parameters, body, statuses
  and `operational` rule above;
- the four node states with their precedence, reported by every node;
- readiness that fails from the start of shutdown, with the health summary
  unaffected by draining;
- `grpc.health.v1.Health/Check` without a token, for `""` only, with `Watch`
  and `List` authenticated and unimplemented.
