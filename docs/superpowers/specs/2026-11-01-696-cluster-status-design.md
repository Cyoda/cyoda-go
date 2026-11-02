# Cluster status, node lifecycle and gRPC health (#696)

## What this is

A read that tells a platform operator, in one call, which cyoda-go nodes are
in the cluster and whether each one is ready to work: `GET /platform/cluster`.
With it come the parts it needs or exposes:

- a lifecycle state per node (`STARTING`, `NORMAL`, `DRAINING`, `UNHEALTHY`),
  published in the node's gossip metadata;
- a live database check, the new storage SPI method `Ping`;
- the standard gRPC health service, `grpc.health.v1.Health`, `Check` only;
- `/readyz` and gRPC health failing from the moment shutdown begins;
- a metric counting live cluster members per lifecycle state.

It reuses the authenticated peer channel and the forwarder that the platform
clock's confirmation uses (#611 spec, "The peer channel":
`docs/superpowers/specs/2026-10-26-611-platform-clock-design.md`). This work
is built on that change.

## Why

A test that drives a cluster, a platform clock test above all, has to know
that every node it expects is present and serving before it starts. Today
there is no such call. Per node there is `/livez` and `/readyz` on the admin
listener and `GET /health` on the API listener
(`internal/api/health.go:10-30`), and each answers only for itself. A node is
in the cluster's gossip directory from inside `app.New` (`app/app.go:455`),
before its servers listen (`cmd/cyoda/main.go:173`), and after a shutdown
signal `/readyz` keeps answering "ready" until the process exits
(`app/app.go:906-915` has no drain condition). The storage SPI has no way to
ask whether the database answers (`StoreFactory`, cyoda-go-spi
`persistence.go:12-30`).

## Goals

1. One operator call lists every node in the gossip directory with its
   reachability, lifecycle state, database reachability, version and clock
   setting, and says whether the cluster as a whole is operational.
2. A test can wait, in that call, until a given number of nodes is serving.
3. Every node publishes its lifecycle state, so the state of a node that
   cannot answer is still known.
4. A node that is shutting down stops being ready, on `/readyz` and on gRPC
   health, from the moment shutdown begins.
5. Load balancers and orchestrators that probe gRPC can use the standard
   health service without a token.
6. Operators can watch the cluster's members per state as a metric.

## Non-goals

- **Readiness on the database.** `Ping` is used by the status read only. A
  database outage does not take every node out of the load balancer.
- **Partitions and HA.** A node cut off from gossip is not in the directory
  and is not listed. No partition handling.
- **Gossip's suspect and dead states.** memberlist's public `Node.State` field
  exists (memberlist v0.6.0 `state.go:44-56`) but memberlist never writes it:
  its internal `nodeState.State` shadows it (`state.go:79-84`, and `grep -n
  "Node.State =" state.go` is empty). Events carry no suspect transition, and
  `Members()` drops only dead and left nodes (`memberlist.go:608-620`). So
  "live" means alive or suspect, and the metric and the read cannot tell the
  two apart.
- **gRPC `Watch` and `List`.**
- **A gRPC door to the status read.** Like the other operator controls it is
  HTTP only.
- **Comparing clock readings.** `now` is never compared between nodes.

## API

### `GET /platform/cluster`

- **Auth:** platform operator only (`auth.OperatorGuard`,
  `internal/auth/operator_guard.go:33-52`; it refuses on-behalf-of tokens).
  Mock IAM lets any tenant's admin act as operator (`operator_guard.go:28-31`),
  as on every operator route.
- **Door:** HTTP only. Declared in `api/openapi.yaml` beside
  `/platform/clock` (`api/openapi.yaml:1014`), tag `Platform Cluster`,
  operationId `getPlatformCluster` (builder's call on both names). Served
  through the generated router, so the query rules refuse an undeclared
  parameter (`QueryParameterRefused`, `api/openapi.yaml:12617-12625`).
- **Query parameters**, both declared in OpenAPI:
  - `waitForNodes` (integer, 1 to 1000, optional): answer once at least this
    many nodes are `UP`/`NORMAL`. Absent: answer after one round.
  - `timeoutMs` (integer, 1 to 600000, optional, default 30000): the longest
    wait for `waitForNodes`. Given without `waitForNodes` it is refused
    with 400.

  Spec's call: the ranges, the default and the refusal of a lone `timeoutMs`.

#### How it works

Node A, the node that serves the request:

1. Lists every member of its gossip directory
   (`internal/cluster/registry/directory.go:11-73`), itself included. `List`
   is not enough: it skips a member whose metadata does not parse
   (`gossip.go:438-450`). The read uses the registry method that the clock
   confirmation uses to list every directory member.
2. Asks every member, itself included, for its status over the peer channel,
   all in parallel (see "Per-node status exchange").
3. Builds one entry per member:
   - **UP**: the member answered. Its state, database, version and clock come
     from the answer.
   - **DOWN**: the member is in the directory and did not answer: no answer
     within the bound, a connection that did not open, an answer that does
     not open or does not validate, or no usable address. Its state comes
     from its gossip metadata. A member whose metadata does not parse has no
     state.
4. A member that has left the directory is not in the answer at all.
   `waitForNodes` is how a test notices a node it expects is missing.

**With `waitForNodes=N`:** A runs rounds until at least N entries are
`UP`/`NORMAL`, then answers with that round. Between rounds it waits for the
registry's `Changed()` (`internal/contract/registry.go:18-23`) or a re-ask
interval of one second (builder's call), whichever comes first; the interval
is needed because a metadata update, such as a state change, does not fire
`Changed()` (`internal/cluster/registry/tags_worker.go:89-111`). When
`timeoutMs` has passed, A answers with the last completed round. The first
round always completes, so the request answers at the later of `timeoutMs`
and the end of the first round. Spec's call: the last completed round is the
answer, because a round cut short at the deadline would show answering nodes
as `DOWN`.

**Single-node mode** has no gossip and no peer channel. A builds its one entry
in process: id `local` (`app/app.go:157`), reachability `UP`, and its state,
database, version and clock read directly. `Local.Changed()` never fires
(`internal/cluster/registry/local.go:48-52`), so a wait re-asks on the
interval; `waitForNodes` above 1 can never be met and answers 503 at the
timeout.

#### Response

The same body for 200 and 503, written with `common.WriteJSON`
(`internal/common/errors.go:472-478`), never through `common.WriteError`, as
`GET /health` does (`internal/api/health.go:24-28`).

`ClusterStatusDto` (schema names are the builder's call):

| Member | Type | Meaning |
|---|---|---|
| `operational` | boolean, required | See below |
| `counts` | array of `ClusterStateCountDto`, required | One item per (reachability, state) combination present, with its count; none for an absent combination |
| `nodes` | array of `ClusterNodeStatusDto`, required | One per directory member, in ascending `id` order |

`ClusterStateCountDto`: `reachability` (`UP`/`DOWN`, required), `state`
(the four states; absent for members without a state), `count` (integer ≥ 1,
required).

`ClusterNodeStatusDto`:

| Member | Type | Present |
|---|---|---|
| `id` | string | always: the member's node id |
| `reachability` | `UP` \| `DOWN` | always |
| `state` | `STARTING` \| `NORMAL` \| `DRAINING` \| `UNHEALTHY` | always, except for a `DOWN` member whose metadata does not parse |
| `database` | boolean: the node's `Ping` succeeded | `UP` only |
| `version` | string: the node's `cfg.Version` (`app/config.go:30`) | `UP` only |
| `clock` | `ClusterNodeClockDto`: `controllable` (boolean), `version` (integer, int64), `writer` (string) | `UP` only |

`clock` is the node's clock setting without its times: `controllable` is
clock control on that node, and `version` and `writer` are the setting's
(`internal/platformclock/setting.go:24-30`). A node with clock control off
answers `controllable: false`, `version: 0`, `writer: ""`. Every clock
change installs a new setting with a new version and the handling node as
writer, and answers 200 only when every node holds it (#611 spec, "Every
change ends by installing and confirming a fresh setting"). So after a
change, nodes agree on (`version`, `writer`) only when every one holds that
change's setting, and the comparison below detects a node that does not.

**`operational`** is true only when all of these hold:

- every entry is `UP` and `NORMAL`;
- every entry has `database: true`;
- all entries have the same `version`;
- all entries have the same `clock` (`controllable`, `version`, `writer`);
- with `waitForNodes=N`, at least N entries are `UP`/`NORMAL`.

So with `waitForNodes`, an extra `DOWN` node still gives 503: the wait ends
when N nodes serve, and `operational` is judged over all of them.

**The body never contains** node addresses, peer URLs, the forwarder's error
text, database error text, or stack traces. They go to the log of the node
that saw them.

#### Status codes

| Status | Code | When |
|---|---|---|
| 200 | — | `operational` is true |
| 503 | — (body is `ClusterStatusDto`) | `operational` is false |
| 400 | `BAD_REQUEST` (query-parameter refusal) | an undeclared parameter; a value that is not an integer; `waitForNodes` outside 1–1000; `timeoutMs` outside 1–600000; `timeoutMs` without `waitForNodes`; `properties.parameter` names it |
| 401 | `UNAUTHORIZED` | missing or invalid credentials |
| 403 | `FORBIDDEN` | not a platform operator, or on behalf of a user |
| 500 | generic + ticket | an internal fault |

A client that leaves during the wait gets no answer; the error is marked
`ClientGone` (`internal/common/errors.go:54-56`) and logged at DEBUG.

OpenAPI declares the 503 with the `ClusterStatusDto` body, as its own
response, not the shared `ServiceUnavailable` problem response.

### Per-node status exchange

- **Route.** `POST /internal/dispatch/status` (builder's call on the path),
  registered beside the hand-over route and the clock route
  (`internal/cluster/dispatch/handler.go:50-53`; `app/app.go:731-734`,
  `:743-746`), at the root, on every node in cluster mode.
- **Authentication.** As every peer route: the request is sealed for its
  recipient with the method, path, timestamp and recipient bound into the
  associated data (`internal/cluster/dispatch/aead_peer_auth.go:92-105`,
  `:300-301`); the answer is sealed for the request it answers
  (`aead_peer_auth.go:303-311`). A request that fails authentication gets a
  bare 403 (`handler.go:75-78`), which the asker reads as no answer.
- **Request.** Carries nothing but the envelope.
- **Answer.** `state`, `database`, `version`, and the clock's `controllable`,
  `version` and `writer`, from the answering node's own lifecycle, `Ping`,
  `cfg.Version` and clock.
- **The database check.** The answering node calls `StoreFactory.Ping` with a
  bound of half its `CYODA_PROXY_TIMEOUT`, from a context derived from the
  peer request. A failure is logged at WARN on that node, with the error, and
  answered as `database: false`. Spec's call: the bound is half the peer
  timeout, so the ask always outlasts the check when the nodes share their
  configuration, as cluster nodes do.
- **Asking.** Node A sends through `HTTPForwarder`
  (`internal/cluster/dispatch/forwarder.go:62-106`), which reuses the
  peer-address check (`forwarder.go:122-130`) and the sealed round trip
  (`forwarder.go:146-225`). Opening each connection is bounded by
  `CYODA_DISPATCH_CONNECT_TIMEOUT`; each ask is bounded by
  `CYODA_PROXY_TIMEOUT`, which config validation keeps above zero (#611
  spec). The context derives from the request.
- **Validation of an answer.** A state outside the four, or a version or
  writer longer than a bound the builder sets, makes the answer invalid; the
  member is then `DOWN`. The answer reaches a client response, so it is
  checked although it is sealed.
- **Self.** A asks itself over the same channel, so its own entry proves its
  own peer route serves.

## Lifecycle states

### States and precedence

| State | Meaning |
|---|---|
| `STARTING` | inside `App.New`: the node is in gossip and not yet built |
| `NORMAL` | `App.New` has finished |
| `DRAINING` | shutdown has begun |
| `UNHEALTHY` | the health latch has tripped: a panic was recovered in engine or store work |

The state is computed from three facts, with precedence
`UNHEALTHY` > `DRAINING` > `STARTING` > `NORMAL`: the health latch tripped;
draining begun; `App.New` finished. None is ever undone. cyoda-go sets
every transition itself; an embedder that calls `app.New` and runs its own
servers, as the commercial cassandra binary does
(`cyoda-go-cassandra/cmd/cyoda-go/main.go:60-104`), calls nothing extra.

### Where each transition is set

- **`STARTING`** is the state from construction. The gossip metadata built in
  `NewGossip` (`internal/cluster/registry/gossip.go:113-123`) already carries
  it, so a node is `STARTING` from its first appearance in gossip
  (`app/app.go:455`).
- **`STARTING` → `NORMAL`:** set by `App.New` itself as its last step, before
  it returns (`app/app.go:775`). There is no method for an embedder to call.
- **Between `App.New` returning and the listeners opening** the node is
  `NORMAL` in gossip but cannot answer a status ask: its peer route is on the
  HTTP handler, which is not yet served (`cmd/cyoda/main.go:173`, `:184`). The
  status read lists it `DOWN`/`NORMAL`, so `waitForNodes`, which counts only
  `UP`/`NORMAL`, keeps waiting until the node answers. A `STARTING` node is
  likewise always `DOWN`: nothing serves its peer route while `App.New` runs.
- **`DRAINING`:** set inside the app at the start of every shutdown method an
  embedder can call:
  - `DrainScheduler` (`app/app.go:921`). The binary calls it first on a
    signal (`cmd/cyoda/run.go:128-131`).
  - `Shutdown` (`app/app.go:1051`). It does not call `DrainScheduler`; it
    stops the scheduler itself (`app/app.go:1055-1057`), so it sets the state
    on its own.
  - `Close` (`app/app.go:999`), through the `DrainScheduler` call it makes
    first (`app/app.go:1004`). The cassandra binary stops its HTTP server and
    then calls only `Close` (`cyoda-go-cassandra/cmd/cyoda-go/main.go:96-102`).
- **`UNHEALTHY`:** the health flag becomes a latch type that notifies when it
  trips. It replaces the `*atomic.Bool` (`app/app.go:94-104`, created at
  `:128`) everywhere it is passed. Six sites trip it today:
  - `app/app.go:792` (search reaper sweep);
  - `internal/grpc/recovery.go:95`;
  - `internal/scheduler/service.go:1040`;
  - `internal/api/middleware/recovery.go:65`;
  - `internal/domain/search/service.go:1182`;
  - `internal/domain/search/reaper.go:188`.

  Its readers are `GET /health` (`internal/api/health.go:24`) and
  `ReadinessCheck` (`app/app.go:911`). The notification does not block the
  tripping goroutine; how it reaches the gossip metadata is the builder's
  call. The latch's own comment (`app/app.go:94-103`) names four sites; it is
  corrected to the six.

### Published in gossip metadata

- `nodeMeta` (`internal/cluster/registry/gossip.go:44-53`) gains the state.
- **One builder.** Today `marshalMeta` takes the tag-list version
  (`gossip.go:55-57`), and `UpdateTags` calls it under the tag store's lock
  (`gossip.go:531-550`, `tags.go:111-129`). One metadata builder holds the
  current tag version and the current state under one lock and marshals both;
  `UpdateTags` and the state change both go through it, so no metadata ever
  pairs a stale version with a new state or the reverse. The builder makes no
  call into the tag store while it holds its lock.
- **Size.** `checkIdentityFits` (`gossip.go:72-85`) measures the metadata with
  the longest list version; it measures with the longest state name too
  (`UNHEALTHY`), so a node whose identity no longer fits the 512-byte limit
  refuses to start.
- **Re-advertise on change.** A state change stores the new metadata
  (`gossipDelegate.updateMeta`, `gossip.go:592-596`) and pokes the
  re-advertiser (`tags_worker.go:255-275`), as a tag change does
  (`tags_worker.go:278-283`). `DRAINING` is advertised before the node leaves:
  `Deregister` stops the re-advertiser after the drain has begun
  (`gossip.go:483-490`, called from `app/app.go:1073`).
- **Single-node mode** has no metadata; the state lives in the process.

### Readiness

`ReadinessCheck` (`app/app.go:906-915`) also fails while the node is
`DRAINING`, with its own reason ("node draining: shutdown has begun"; wording
is the builder's call). `/readyz` (`internal/admin/admin.go:47-48`) therefore
fails from the start of `DrainScheduler`, while the servers still serve, so a
load balancer stops sending new requests before the servers drain.
`GET /health` stays on the health latch alone. `/livez` is unchanged.

## gRPC health

- **Service.** `grpc.health.v1.Health` (`google.golang.org/grpc/health/grpc_health_v1`,
  grpc v1.83.2) registered on the gRPC server beside the CloudEvents service
  (`internal/grpc/server.go:155`). The gRPC listener only.
- **`Check`.** For the service name `""` (the server as a whole): `SERVING`
  while `ReadinessCheck` returns nil, that is while the node is ready,
  healthy and not draining; `NOT_SERVING` otherwise. Any other service name
  answers `NotFound`, as the standard health server does. Spec's call: only
  `""` is served.
- **`Watch` and `List`** are not implemented (the generated
  `UnimplementedHealthServer`, `health_grpc.pb.go:190-198`).
- **No token for `Check`.** The unary auth interceptor
  (`internal/grpc/interceptor.go:22-37`) and the unary `ROLE_M2M` interceptor
  (`internal/grpc/role_interceptor.go:30-37`) pass a call whose full method
  name is exactly `/grpc.health.v1.Health/Check`
  (`grpc_health_v1.Health_Check_FullMethodName`) without checking it. Nothing
  else is exempt: `Watch` and `List` go through both interceptors as every
  other call does. The `role_interceptor.go:15-16` comment, "gRPC has no
  allow-listed operation", is corrected.
- **Order.** Recovery stays first in both chains (`server.go:106-125`).
  `Check` carries no pass, so the tx-route interceptor hands it straight on
  (`internal/grpc/txroute_interceptor.go:79-87`, `:120-125`).

### Status table

| Call | Credentials | Node | Answer |
|---|---|---|---|
| `Check`, service `""` | none or any | ready, healthy, not draining | `SERVING` |
| `Check`, service `""` | none or any | latch tripped, draining, or storage not initialised | `NOT_SERVING` |
| `Check`, another service name | none or any | any | `NotFound` |
| `Watch` or `List` | none or invalid | any | `Unauthenticated` |
| `Watch` or `List` | valid, without `ROLE_M2M` | any | `PermissionDenied` |
| `Watch` or `List` | valid, with `ROLE_M2M` | any | `Unimplemented` |
| `Check` | any | the handler panics | `Internal`, through recovery, which trips the latch |

## Storage SPI: `Ping`

```go
// Ping reports whether the store answers. It returns within ctx and returns
// an error when ctx is done. After Close it returns an error.
Ping(ctx context.Context) error
```

- **Where.** A required method on `spi.StoreFactory` (cyoda-go-spi
  `persistence.go:12-30`), beside `Close`. Breaking for out-of-tree plugins.
- **postgres:** pings the main pool (`StoreFactory.pool`,
  `plugins/postgres/store_factory.go:14`), bounded by ctx. The scheduler's own
  pools are not checked.
- **sqlite:** pings the read handle (`StoreFactory.readDB`,
  `plugins/sqlite/store_factory.go:70`), bounded by ctx. After `Close`
  (`:433-450`) the handle is closed and the ping fails.
- **memory:** returns nil, and `ctx.Err()` when ctx is done. `Close`
  (`plugins/memory/store_factory.go:273-287`) records that it ran, and `Ping`
  after it returns an error.
- **`CachingStoreFactory`** forwards it, as it forwards `Close`
  (`internal/cluster/modelcache/factory.go:82`).
- **Root test fakes** that implement `spi.StoreFactory` without embedding it
  implement it, for example `app/app_close_idempotent_test.go:44`,
  `internal/cluster/modelcache/factory_test.go:40`, `:190` and
  `internal/domain/model/service_test.go:136`; the compiler lists the rest.
- **Used** only by the status exchange, never by `/readyz` or gRPC health.
- **Conformance (spitest)**:
  - `Ping` returns nil on a healthy store;
  - `Ping` returns an error for a context already cancelled;
  - `Ping` returns an error after `Close`. The case builds its own factory
    through the harness's `ClockFactory` hook (cyoda-go-spi
    `spitest/spitest.go:50-58`) and closes it; without the hook it skips with
    a reason. Builder's call: a dedicated hook instead.

  Spec's call: after `Close`, `Ping` returns an error on every backend.

## Metric

- **Name:** `cyoda.cluster.member.count`, an `Int64ObservableGauge`, unit
  `{member}`. It follows OpenTelemetry naming: a count of the entities a
  namespace names is `<namespace>.count`, not a plural.
- **Attribute:** `cyoda.cluster.member.state`, with the values `starting`,
  `normal`, `draining`, `unhealthy`.
- **Value:** the live members of this node's gossip directory, itself
  included, counted per state read from their metadata. A member whose
  metadata does not parse is not counted (spec's call; the registry already
  logs it, `gossip.go:441-447`).
- **Where:** registered with the membership instruments
  (`internal/cluster/registry/metrics.go:21-43`) and unregistered with them on
  `Deregister`. Cluster mode only; single-node mode has no gossip and no such
  metric.

## Coverage matrix

**Layers:**
- **U:** unit.
- **E:** HTTP e2e in `internal/e2e` on a running backend (Postgres). Cluster
  cases use cluster nodes on one database, as the clock tests do
  (`internal/e2e/platform_clock_test.go:700-717`).
- **P:** cross-backend parity in `e2e/parity`, registered in `registry.go`,
  using the fixture's `PlatformOperator` (`e2e/parity/fixture.go:52-57`).
- **G:** gRPC tests in `internal/grpc`.
- **C:** spitest conformance.

| Scenario | U | E | P | G | C |
|---|---|---|---|---|---|
| Single node: 200, one entry `local`, `UP`/`NORMAL`, `database` true, version and clock; `counts`; `operational` true | ✓ | ✓ | ✓ | | |
| Two cluster nodes: 200, both `UP`/`NORMAL`, A's own entry answered over the peer channel | ✓ | ✓ | | | |
| A member whose address serves nothing: 503, `DOWN` with the state from its metadata; the body holds no address | ✓ | ✓ | | | |
| A node that has left is absent | ✓ | ✓ | | | |
| A node inside `App.New`: `DOWN`/`STARTING`; after `App.New` and before its listeners open: `DOWN`/`NORMAL`, and `waitForNodes` keeps waiting | ✓ | waived: both windows lie before the in-process harness serves the node; status-service tests drive metadata in each state | | | |
| `App.New` returns with the state `NORMAL`, with no call from the embedder | ✓ | | | | |
| `DRAINING` is set by `DrainScheduler`, by `Shutdown`, and by `Close` alone | ✓ | | | | |
| A node draining (`DrainScheduler` called): 503, `UP`/`DRAINING` | ✓ | ✓ | | | |
| A node with the latch tripped: 503, `UP`/`UNHEALTHY` | ✓ | waived: no e2e can force a recovered panic; the status service is tested with a tripped latch | | | |
| A node whose `Ping` fails: 503, `database` false; the body holds no database error text | ✓ | waived: no e2e stops the database mid-test; the exchange is tested with a failing `Ping` | | | |
| Nodes on different versions: 503 | ✓ | ✓ | | | |
| Nodes with different clock settings (clock control on one, off on another): 503 | ✓ | ✓ | | | |
| A member whose metadata does not parse: 503, `DOWN`, no state | ✓ | waived: no cyoda-go node advertises such metadata; registry and service tests | | | |
| An answer that does not validate counts as `DOWN` | ✓ | | | | |
| `waitForNodes=2`: answers 200 once a second node joins | ✓ | ✓ | | | |
| `waitForNodes=2` met with a further `DOWN` member: 503 | ✓ | ✓ | | | |
| `waitForNodes` not met by `timeoutMs`: 503 with the last completed round | ✓ | ✓ | | | |
| Single node, `waitForNodes=2`: 503 at the timeout | ✓ | ✓ | | | |
| A state change without a membership change is seen at the next re-ask | ✓ | | | | |
| Each 400 cell, including the undeclared parameter | ✓ | ✓ | | | |
| 401; 403 for a non-operator and for on-behalf-of | | ✓ | | | |
| 500 | ✓ | waived: no e2e can force an internal fault; handler unit test | | | |
| A client that leaves during the wait: `ClientGone` | ✓ | waived: an e2e cannot read an answer its client has abandoned | | | |
| Status route: an envelope sealed for another path or node is refused; the `Ping` bound is half the ask bound | ✓ | | | | |
| Lifecycle: precedence; each transition at its call site; nothing is undone | ✓ | | | | |
| Metadata carries the state; one builder pairs tag version and state; size check with `UNHEALTHY`; re-advertised on change | ✓ | | | | |
| `/readyz` fails from the start of `DrainScheduler`; `GET /health` stays `UP` while draining | ✓ | waived: the e2e harness runs no admin listener; the binary's SIGTERM test (`cmd/cyoda/main_sigterm_test.go:19`) asserts `/readyz` fails during the drain | | | |
| gRPC `Check` without a token: `SERVING` | | | | ✓ | |
| gRPC `Check`: `NOT_SERVING` with the latch tripped, and while draining | | | | ✓ | |
| gRPC `Check` for another service name: `NotFound` | | | | ✓ | |
| gRPC `Watch`/`List`: `Unauthenticated` without a token, `PermissionDenied` without `ROLE_M2M`, `Unimplemented` with it | | | | ✓ | |
| Metric: members counted per state; an unparseable member not counted | ✓ | | | | |
| `Ping`: nil on a healthy store | | | | | ✓ |
| `Ping`: an error for a cancelled context | | | | | ✓ |
| `Ping`: an error after `Close` | | | | | ✓ |
| `CachingStoreFactory.Ping` forwards | ✓ | | | | |

The gRPC column covers gRPC health only; the status read has no gRPC door.
The parity row covers what differs per backend, the `Ping` behind
`database: true`; cluster behaviour runs on Postgres in `internal/e2e`.

## Production impact

**Clustering off:**
1. `GET /platform/cluster` exists for operators and answers from the node
   itself.
2. gRPC health is registered; `Check` answers without a token.
3. `/readyz` and gRPC health fail from the start of shutdown; today `/readyz`
   stays ready until the process exits. `GET /health` is unchanged.
4. The health flag is a latch type; tripping it behaves as today.
5. Every plugin implements `Ping`; only the status read calls it.
6. `App.New` sets the node `NORMAL` before it returns. No background work,
   store reads or locks are added, and an embedder changes nothing.

**Clustering on**, in addition:
1. Gossip metadata carries the state, up to 17 more bytes
   (`,"st":"UNHEALTHY"`, with the key name the builder's call). A node whose
   identity was within that margin of the 512-byte limit now refuses to start
   (`gossip.go:72-85`).
2. Each state change re-advertises the metadata: at most three extra
   metadata broadcasts in a node's life.
3. Every node registers `POST /internal/dispatch/status`. A status read sends
   one sealed request per member, only when an operator calls it.
4. The metric `cyoda.cluster.member.count` is reported.
5. A draining node stops being ready while its scheduler drains and its
   servers still serve; peers still reach it through gossip until it leaves.

## Security

- **Operator only.** `OperatorGuard`; on-behalf-of tokens are refused. The
  read holds no tenant data: ids, states, booleans, versions and clock
  versions and writers (node ids). No tenant can reach it.
- **What the body never contains:** node addresses, peer URLs, forwarder error
  text, database error text, stack traces. They go to the log of the node
  that saw them. The 503 is a status body, not a problem, so it carries no
  ticket and no detail.
- **Status route.** Accepts only an envelope sealed with the cluster key for
  this node and this path, the same trust as a callout hand-over. It is
  outside the context path and outside API authentication, like the
  hand-over route. A sealed answer is still validated before it reaches a
  client.
- **SSRF.** The read dials only directory addresses, through the forwarder's
  peer-address guard (`forwarder.go:122-130`).
- **Input validation.** Both query parameters are declared and range-checked;
  an undeclared one is refused.
- **Unauthenticated gRPC health** answers one status value about the node.
  It exposes nothing beyond what `GET /health` and the node's own behaviour
  show: `GET /health` gives the latch, and a draining node stops serving
  anyway. No other gRPC method is exempt, by exact full-method-name match.
- **Credentials.** No credential, token or key appears in the read, the
  exchange or the logs it adds.

## Documentation

- **Help, `cmd/cyoda/help/content/cluster.md`:** the status read (path, auth,
  parameters, body, `operational`, 200/503), the four states and their
  transitions, gRPC health, and that `/readyz` fails from the start of
  shutdown. The OPERATIONS bullets (`cluster.md:67-72`) are updated where they
  describe `/readyz`.
- **`run.md` HEALTH PROBES** (`cmd/cyoda/help/content/run.md:288-299`): `/readyz`
  and `GET /health` differ while draining; gRPC health.
- **`config/cluster.md`:** the identity budget figures (`:20`, "89 bytes are
  framing and the longest list version") include the state;
  `CYODA_PROXY_TIMEOUT` (`:27`) bounds each status ask, and half of it bounds
  the database check.
- **`telemetry.md`:** the metric beside the membership metrics (`:123-124`),
  and its attribute in ATTRIBUTE VOCABULARY (`:147`).
- **`grpc.md`:** gRPC health under SERVICES (`:53`).
- **`docs/ARCHITECTURE.md:2660`:** the metadata holds identity, a list version
  and the state.
- **OpenAPI:** the path, both parameters, the schemas, the 200 and the 503
  with the same body; then `go generate ./api`.
- **Cloud parity (Gate 7):** a new `docs/cloud-parity/cluster-status.md` and a
  row in `docs/cloud-parity/README.md`: the read's contract, the states, gRPC
  health, readiness on drain. A CaaS ticket.
- **CHANGELOG `[Unreleased]`:**
  - `### Breaking`: `StoreFactory.Ping` for plugin authors; `/readyz` fails
    from the start of shutdown;
  - `### Added`: `GET /platform/cluster`, lifecycle states in gossip, gRPC
    health, the metric.
- **SPI change, per `MAINTAINING.md` ("Bumping cyoda-go-spi", `:434-490`):**
  1. an SPI PR into `main` with `Ping` and the spitest cases; mid-milestone,
     no tag;
  2. a pseudo-pin of the merged SPI `main` in all four `go.mod` files, then
     `make check-spi-pin-sync` (`Makefile:182`);
  3. the plugins repinned (`make repin-plugins`, `Makefile:185`);
  4. `COMPATIBILITY.md` updated for the new pin in the same change;
  5. the cassandra issue (below) filed with the SPI PR.

## Follow-ups

- **cyoda-go-cassandra issue:** implement `StoreFactory.Ping` (a bounded
  check on the session, an error after `Close`) before its next SPI bump; the
  new spitest cases fail on it until then.
- **CaaS ticket** for the cloud-parity page.

## Left to the builder

- Package, type and method names; the schema names; the peer route's path and
  the wire encoding of the status answer.
- The lifecycle holder's shape and how the latch's notification reaches the
  gossip metadata.
- The metadata key for the state.
- The re-ask interval of a wait (one second unless there is reason to differ).
- The length bounds that validate a status answer.

## Calls made in this document (not discussed with Paul)

- **Parameter ranges:** `waitForNodes` 1–1000; `timeoutMs` 1–600000, default
  30000; `timeoutMs` alone is refused.
- **A wait answers with the last completed round**, and the first round
  always completes.
- **`Ping` bound:** half the answering node's `CYODA_PROXY_TIMEOUT`.
- **`Ping` after `Close`** returns an error on every backend.
- **A member with unparseable metadata** is listed `DOWN` without a state and
  is not counted by the metric.
- **gRPC health** serves only the service name `""`; `Watch` and `List` are
  not exempt from authentication.
- **The 200 and 503 body** lists `counts` as an array of combinations.
