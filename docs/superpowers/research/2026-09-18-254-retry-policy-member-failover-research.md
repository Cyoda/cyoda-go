# #254 RetryPolicy with member failover — research

Verified map of the contract surrounding the change, written before any design.
Base: `release/v0.9.0` at `c0a4b7d9`. Cloud source: `~/dev/cyoda` as checked out
on 2026-09-18. Claims carry `file:line`; anything not read is marked
**could not verify**. Four claims the design rests on were re-checked by hand
after the survey and are marked **[re-verified]**.

## 1. What the issue asks for, and what is already stale in it

The issue body predates three things that have since landed:

| Issue says | State at `c0a4b7d9` |
|---|---|
| Scope 1: surface inbound `retryable` onto `ProcessingResponse` | Done by #262. `internal/grpc/members.go:55`, populated at `internal/grpc/streaming.go:238, 272, 308` for processor, criteria and function responses. Nothing reads it **[re-verified]**. |
| Scope 4: reject unknown `retryPolicy` at import | Done by #262 for **processors only** — `internal/domain/workflow/validate.go:72-83, 579-581` → 400 `VALIDATION_FAILED` (`handler.go:320-322`). |
| Scope 6 / test (c): `asyncResult=true` forces one attempt; "gate on #223 or pre-wire" | `asyncResult=true` and any `crossoverToAsyncMs` are **rejected at import** — `validate.go:548-574` **[re-verified]**. #223 is open and unmilestoned. No stored workflow can carry the flag, so a suppression branch is unreachable and test (c) cannot import its fixture. |
| Dispatcher line refs `dispatch.go:46–135, 181–260` | Stale. Shared core is `dispatchCalloutToMember` at `dispatch.go:109`; entry points `DispatchProcessor` `:206`, `DispatchCriteria` `:273`, and a third the issue omits, `DispatchFunction` `:346`. |

The second issue comment (the three-way split) is current and is treated as the
governing statement of intent. Its related observation on `EPOCH_MISMATCH` is
confirmed in §6 and is out of scope here.

## 2. Dispatch today (`internal/grpc`)

### 2.1 Shape

`contract.ExternalProcessingService` (`internal/contract/processing.go:59-72`)
has three methods — `DispatchProcessor`, `DispatchCriteria`,
`DispatchFunction`. Production implementations: `*grpc.ProcessorDispatcher`
(local), `*dispatch.ClusterDispatcher` (wraps local),
`*observability.TracingExternalProcessingService` (decorator),
`*skeleton.ExternalProcessingService` (no-op),
`*localproc.LocalProcessingService` (in-process test double under
`internal/testing/`).

Each local method: `MustGetUserContext` (panics if absent) → one
`registry.FindByTags(tenantID, tags)` → build the request CloudEvent → the
shared `dispatchCalloutToMember` → map the response. Strictly single-shot: no
loop, counter, delay or exclusion anywhere in `internal/grpc`.

`ProcessorDispatcher` holds `registry, uuids, signer, selfNodeID, tokenTTL`
(`dispatch.go:37-54`). It receives nothing from `app.Config` except
`cfg.Cluster.TxTokenTTL` (`app/app.go:441`) — there is no config conduit for
retry parameters today.

### 2.2 Failure taxonomy of one attempt

| Condition | Site | Result |
|---|---|---|
| No member for tags | `dispatch.go:212-214, 302-305, 353` | wrapped sentinel `contract.ErrNoMatchingMember`; becomes 503 `NO_COMPUTE_MEMBER_FOR_TAG` retryable only later, at `entity/service.go:2867-2869` |
| Member evicted before/at send, or mid-wait | `:137-142, 150-153, 172-175` | `disconnectedErr` → 503 `COMPUTE_MEMBER_DISCONNECTED`, retryable |
| Enqueue timeout ("member not draining") | `:156-160` | 503 `DISPATCH_TIMEOUT`, retryable |
| Response timeout | `:186-192` | 503 `DISPATCH_TIMEOUT`, retryable |
| Parent ctx dead | `:154-155, 186-189` | bare `ctx.Err()` |
| **Member-reported failure (`success=false`)** | `:176-183` | `fmt.Errorf("%s dispatch failed: %s", label, errMsg)` — **no code, no retryable flag**; also `common.AddError(ctx, …)` **[re-verified]** |
| CloudEvent build / auth-context failure | `:112, 115` | plain wrapped errors; the latter joins `contract.ErrAuthContextUnavailable` → ticketed 500 |

Member warnings are drained before the failure checks (`:167-171`), so a failed
attempt still surfaces its warnings onto the request.

Timeout: per-callout `responseTimeoutMs`, `<= 0` → compiled-in 30 000 ms
(`dispatch.go:33, 122-124`). One deadline covers enqueue and wait (`:129`). No
server config key, no cap, no import validation of the value.

### 2.3 Member registry

- `FindByTags` (`members.go:467-479`) ranges a `map[string]*Member` and returns
  the first tenant-and-tag match. Go map order is randomised per iteration, so
  selection among equal candidates is arbitrary — not round-robin, not stable.
  No exclusion parameter. Three callers, all in `dispatch.go`.
- Tenant scoping: exact `m.TenantID == tenantID`; member tenant is fixed at join
  from the authenticated stream, and a join naming another legal entity is
  refused (`streaming.go:63-66`).
- Tag match is **any-overlap** (`internal/common/tags.go:7-20`); empty required
  CSV matches every member. Cloud requires **all** tags
  (`ExternalizerBase.kt:263-272`). `cmd/cyoda/help/content/workflows.md:190`
  says "all" — the help text matches Cloud and contradicts the code.
  Pre-existing divergence, not introduced here.
- `Member.ID` is a per-connection `uuid.NewString()` assigned at join
  (`streaming.go:74`). A reconnecting compute node is a new member.
- Correlation is member-scoped (`pendingReqs` per member, `members.go:113-118`):
  a reply on another member's stream can never satisfy a pending request.
  `AbandonRequest` is deferred on every path (`dispatch.go:147`).
- Eviction (`members.go:179-185, 308-325`) fails every pending request with a
  synthetic `{Success:false, Disconnected:true}`.

### 2.4 Request identity

One `requestID` per dispatch call, used as both CloudEvent `ID` and `RequestID`
(`dispatch.go:218`). Cloud reuses the **same** `requestId` and transaction id on
a failover attempt (`ExternalizedGrpcProcessorIT.kt:709-711`) and tells members
to deduplicate on it (`docs/client-calculation-member-guide.md:752`).

## 3. Cluster mode (`internal/cluster/dispatch`)

Wiring, `app/app.go:524-561`: test-injected `cfg.ExternalProcessing` →
else `ClusterDispatcher` when `cfg.Cluster.Enabled` → else the local dispatcher;
then optionally wrapped for tracing.

`ClusterDispatcher`, identical across all three methods
(`cluster_dispatcher.go:71-204`):

1. mint the tx token onto ctx **before** the local/remote split, so a callback
   from a peer-hosted member routes back to the owner node;
2. call the **local** dispatcher;
3. success → return; any error other than `ErrNoMatchingMember` → return it
   unchanged (`:82, 130, 175`) **[re-verified]**;
4. only "no local member" forwards, via `forwardWithFailover`.

So a local timeout, disconnect, or member-reported failure never goes
cross-node today.

`forwardWithFailover` (`:242-283`) is the only failover loop in the dispatch
path. It excludes by **peer NodeID**, tries each peer at most once, has no delay
and no attempt counter, and fails over on exactly two classes: forwarder
transport error, or a peer answering `NO_COMPUTE_MEMBER_FOR_TAG`. It
deliberately does not re-execute a callout that was actually dispatched
(`:226-230`). `findPeerWithPolling` (`:288-310`) waits up to
`CYODA_DISPATCH_WAIT_TIMEOUT` (5 s), polling gossip every 200 ms, for a peer
advertising the tags — an existing, cluster-only form of "wait for a member to
appear".

Peer side: `handler.go:88, 99, 115` run the local dispatcher and answer HTTP 200
with `Success:false` plus `ErrorCode/ErrorStatus/ErrorRetryable`
(`handler.go:192-208`). The origin re-mints via `remintPeerError` (`:427-437`):
same code, status and retryable flag, generic message. A `Success:false` with an
empty `ErrorCode` — which is what a member-reported failure is, being a plain
error — becomes `fmt.Errorf("peer dispatch failed")` (`:104, 152, 197`). **The
member's message and verdict are both lost across the node boundary today.**

There is no cluster-wide member registry or member identity. Gossip carries
`nodeMeta{ID, Addr, GRPCAddr, Tags map[tenant][]string}`
(`registry/gossip.go:35-40`): a member is visible cluster-wide only as "tenant T
on node N advertises tag X". A node's advertised tags do not say how many
members stand behind them.

Consequence for the design: an exclusion set of member IDs is meaningful only on
the node holding those members. Reaching "a different member" across nodes means
either the origin drives the loop and peers report which member they used, or
each node loops locally and the origin loops over nodes.

## 4. Engine and the path out

### 4.1 Execution modes (`validate.go:22-25`, `engine_processors.go:112-187`)

| Mode | Where the callout runs | On processor error |
|---|---|---|
| `SYNC`, `ASYNC_SAME_TX` (identical today) | inline in the caller's open transaction | fatal — `fmt.Errorf("processor %s failed: %w", …)` (`:181-184`), state not advanced, tx rolled back |
| `ASYNC_NEW_TX` | inside a savepoint | **non-fatal** — warn, rollback to savepoint, pipeline continues; returned entity mutations discarded |
| `COMMIT_BEFORE_DISPATCH` | **after TX_pre has committed**; with `startNewTxOnDispatch=false` the dispatch ctx carries no transaction (`:365`) | fatal; the dispatch error is returned bare (`:334-336, 367-369`) |

Every engine call site wraps the dispatch in `txgate.Suspend(ctx)` /
`resume()` (e.g. `engine_processors.go:204-207`). A loop inside the dispatcher
sits inside that suspension, sleeps included.

The issue comment's argument for not failing over a `retryable: true` verdict —
same transaction, same snapshot — holds for `SYNC`/`ASYNC_SAME_TX`. For
`COMMIT_BEFORE_DISPATCH` without a new transaction there is no enclosing
snapshot for the callout, so the argument does not apply as stated.

### 4.2 Criteria (`engine.go:1000-1017`)

Three call sites: workflow selection (`:596-601`), named transition
(`:799-804`), automated cascade (`:918-922`). An **error** aborts the whole save
and rolls back in all three; a clean `matches=false` is not an error.

### 4.3 Classification

`classifyWorkflowError` (`entity/service.go:2818-2909`) returns any
`*common.AppError` in the chain unchanged — first branch, via `errors.As`.
Everything it does not recognise falls to the catch-all: non-retryable 400
`WORKFLOW_FAILED` with `err.Error()` as detail and no cause. That is where a
member-reported failure lands today. It is the **only** mint site of
`WORKFLOW_FAILED`, and there is no separate gRPC classifier: gRPC handlers reuse
the same `*AppError` through `internal/grpc/errors.go:buildErrorFields`.

`retryable` on the way out:
- HTTP: written only at `common/errors.go:339-341`, as
  `properties.retryable: true`; **omitted** when false. OpenAPI's
  `ProblemDetail.properties` is free-form (`openapi.yaml:8825-8849`); `retryable`
  appears only in examples.
- gRPC envelope: `buildErrorFields` sets `retryable` only for an operational
  error with `Retryable` true; the envelope `code` is the coarse
  `CLIENT_ERROR`/`SERVER_ERROR`, with the precise code inside `message` as a
  `CODE: detail` prefix.

Retryable codes today: `STORAGE_UNAVAILABLE`, `CONFLICT`,
`NO_COMPUTE_MEMBER_FOR_TAG`, `DELETE_NOT_CONVERGED`, `DISPATCH_TIMEOUT`,
`COMPUTE_MEMBER_DISCONNECTED`, `DISPATCH_FORWARD_FAILED`, `SEARCH_QUEUE_FULL`,
and the 408 timeouts. `WORKFLOW_FAILED` is not among them, and its help topic
says `Retryable: no` (`errors/WORKFLOW_FAILED.md:19`, index `errors.md:121`).

## 5. What Cloud actually does

Two files: `net/cyoda/saas/retry/` and `ExternalizerBase.sendAndHandleResponse`.

| Aspect | Cloud | Source |
|---|---|---|
| Policies / default | `NONE`, `FIXED`; unset → `FIXED` | `RetryStrategyConfig.kt:39-51`, `ExternalizerBase.kt:51, 227-233` |
| Parameters | server config only; `numRetries=3`, `delayMs=500`; no per-processor override; no overrides anywhere in the checkout | `RetryStrategyConfig.kt:16-37` |
| Count | 3 retries **after** the first → **4 attempts** | `SimpleRetryStrategy.kt:25-49` |
| Exclusion | `usedMembers` outside the loop, never cleared; member added **before** send | `ExternalizerBase.kt:296-307` |
| Pool exhausted | **keeps looping**: "no member" is itself retryable, so the budget is spent re-querying with delays; a member connecting meanwhile is picked up | `ExternalizerBase.kt:272-282`; `ExternalizationExtendedIT.kt:417-496` |
| Delay | before every retry, failover included; no backoff, no jitter | `SimpleRetryStrategy.kt:44-47` |
| Selection | prefers a member on the local node, else arbitrary | `CalculationMemberService.kt:206-215` |
| What is retried | everything except JVM `Error` and a member-reported failure whose `retryable` is not exactly `true` | `ExternalizerBase.kt:518-530, 561-562` **[re-verified]** |
| `retryable: true` | **fails over to another member** | `ExternalizedGrpcProcessorIT.kt:661-749` |
| `retryable: false` | stop; second member never called | `…IT.kt:566-659` |
| `retryable` absent / no `error` object | treated as `false` → stop | `ExternalizerBase.kt:562`; `…IT.kt:583-586` |
| Criteria | same path, same default; no async mode | `ExternalizedCriteriaChecker.kt:123-136`, `ExternalizerBase.kt:258-260` |
| Async | `noRetryMode = asyncResponseProcess` → one attempt | `ExternalizerBase.kt:285-292, 524-528` |
| Identity on retry | same `requestId`, same transaction id | `…IT.kt:709-711, 800-802` |
| Transaction | whole loop, sleeps included, inside the open tx for SYNC | `ExternalizerBase.kt:295-332` |
| Exhaustion | `All retries exhausted, got N failures: [member<id>: cause], [member<->: cause (2 times)]`; N is the pre-dedup count; dedup key is the rendered string; a **single** failure is not wrapped | `ExternalizerBase.kt:532-554` |
| What the caller sees | cancelled transaction — HTTP 422, gRPC `CANCELLED` (criteria: `FAILED_PRECONDITION`); error code literally `-`; **no retryable marker on anything outbound** | `ExternalizerBase.kt:403-427`; `CloudEventsApiGrpcService.kt:505, 509` |

Two points where the issue and Cloud disagree:

1. **`retryable: true`.** Cloud fails over. The issue comment's case 3 says stop
   and propagate the verdict. The comment's reasoning is sound for a
   transaction-level cause; the cost is a member-local transient ("my
   downstream is down") that another member could have served, which is
   indistinguishable on a one-bit flag.
2. **Outbound verdict.** Cloud propagates none. The issue comment adds one. This
   is new contract, to be logged under `docs/cloud-parity/` per Gate 7.

Cloud's own `docs/grpc-integration.md:315` says `NoApplicableMembersException`
aborts immediately; the code and ITs retry it. Mirror the code.

The closed `com.cyoda:core`/`service` jars are not in the checkout, so how a
failed `StateProcessResult` becomes a transaction cancellation, and whether the
platform re-runs a transition after a version-check failure, **could not be
verified** from source; the former is verified behaviourally by the ITs cited.

## 6. Adjacent defects found on the way

| Defect | Where |
|---|---|
| Criteria `retryPolicy`: declared in OpenAPI with `enum: [NONE, FIXED]` (`openapi.yaml:9419-9427`), validated nowhere, dropped by the dispatcher's parse struct (`dispatch.go:278-290`). A criterion can carry `retryPolicy: "BANANA"`. | import + dispatch |
| `ScheduleFunctionDto` has no `retryPolicy` (`openapi.yaml` ends `:10409`) though `DispatchFunction` shares the transport core. | contract gap |
| `errors.md:47` claims gRPC responses carry `errorCode` and `retryable` in trailer metadata; no production code sets trailers. | help |
| `docs/ARCHITECTURE.md:662` says no failover to a second peer; false since #500 (`:859` is right). | docs |
| `common/errors.go:18-20`: a doc comment for `ErrEpochMismatch` with no declaration under it. | code |
| `EPOCH_MISMATCH`: constant + help topic + two trivial tests; no plugin returns `spi.ErrEpochMismatch` and nothing raises the API error. The issue comment's observation is confirmed. | separate issue |
| `workflows.md:192-200` and `validate.go:66-68` and `members.go:48-55` all say "captured but not consumed". They become false with this change. | must update |
| `docs/WORKFLOW_IMPORT_EXPORT_AUDIT.md` §M1 row and "second special case" describe the gap as open. | must update |

## 7. Conventions the change must satisfy

**Config.** `DefaultConfig()` at `app/config.go:287`. Parsers silently fall back
on a bad value; range validation is a separate explicit layer, and current
policy is a hard startup error, not a clamp (`config.go:756-760`). `envMillis`
(`:510`) is the integer-milliseconds idiom. A new key must pass four guards:
`rootConfigVars` (`cmd/cyoda/help/config_registry.go:31`),
`TestConfig_EnvVarCoverage` (`help_test.go:489`), `TestConfigAll_Complete`
(`config_registry_test.go:73`), and `TestRootConfigVars_MatchDefaults`
(`app/config_registry_binding_test.go:177`, needs a `defaultFor` entry). Each
validator is called both from `Config.Validate()` and individually from
`cmd/cyoda/main.go:87-114`.

**Error codes.** `TestErrCode_Parity` (`help_test.go:546-583`) enforces a
bijection between `ErrCode*` constants and `errors/<CODE>.md` — existence only,
not content. `errors.md:61-122` is a hand-maintained index with no parity test.

**Parity harness.** One `compute-test-client` subprocess per fixture
(`fixtureutil.go:660, 1080`), tags hardcoded (`cmd/compute-test-client/dispatch.go:88-93`),
a process-lifetime catalog whose `processorFunc` signature cannot express
`retryable` (`catalog.go:40`), no disconnect hook, no control surface beyond
`/healthz`. Server env is fixed per package in `TestMain`; a small retry delay
is settable only as a fixture-wide constant, in lockstep across
`e2e/parity/{memory,sqlite,postgres}/fixture.go` and the multinode fixture —
precedent: `CYODA_SCHEDULER_SCAN_INTERVAL=50ms` (`memory/fixture.go:61-69`).
Three scenarios are already registered and skipped for want of exactly this
capability: `RunExternalAPI_09_09/10/11` (`workflow_externalization.go:300-312`),
each mapped to a Cloud IT. Un-skipping them needs no count bump. The commercial
backend consumes the same registry and the same harness.

**Other harnesses.** `internal/e2e`'s `callbackHarness` is a real gRPC member
with per-test closures and `newCallbackHarnessConfigured(t, func(*app.Config))`
(`callback_harness_test.go:174`); it joins one member today. In `internal/grpc`
unit tests, `MemberRegistry.Register` takes a plain `SendFunc`, so two same-tag
members is a two-line setup.

**Schema version.** Shape and import acceptance of processor `retryPolicy` are
unchanged, and the OpenAPI description already promises the behaviour. The three
v0.8.4 precedents of this shape were each recorded as "no bump" under
`## When NOT to bump` (`docs/workflow-schema-versioning.md:49-140`), each "decided
on its own terms". Newly validating **criteria** `retryPolicy` is a tightening
and must be decided under the same doc's rubric.

**Existing tests that pin single-shot behaviour** and will need a deliberate
decision each: `internal/grpc/dispatch_test.go:180, 204, 650, 707, 1045, 1071,
1092, 1167, 1187, 1207, 1224`; `frozen_member_test.go:24, 206`;
`internal/cluster/dispatch/cluster_dispatcher_failover_test.go:88, 155, 179,
211, 284, 315`.

**Release.** `CHANGELOG.md` has `[Unreleased]` only; entries lead with the new
contract in bold and close by citing the `docs/cloud-parity/*.md` file, whose
`README.md:20-53` table needs a row per file.

## 8. Adding `retryPolicy` to the scheduled-transition Function

Verified for the ruling in §9 that Functions get the selector on the DTO.

- **SPI.** `ScheduleFunction` (`cyoda-go-spi/types.go:324-334`) is six plain
  value fields; `TransitionSchedule.Function` is its only referrer. Nothing in
  the SPI enumerates its fields: no marshal customisation, clone, equality or
  validation helper on any workflow type, and `spitest/workflow.go` asserts
  only slice lengths. `ProcessorConfig.RetryPolicy` (`types.go:243`) is the only
  occurrence of the name in the SPI. A plain `string` keeps the struct
  comparable, which `schedule_roundtrip_test.go:139` (`got != fn`) relies on.
  The local SPI checkout is clean at `1ab57a6`, exactly the pinned
  pseudo-version.
- **Storage.** All four backends persist the whole `[]spi.WorkflowDefinition`
  as one JSON blob — memory (`plugins/memory/workflow_store.go:57-67`, deep copy
  is a JSON round-trip, not field-by-field), sqlite and postgres
  (`workflow_store.go`, `json.Marshal` → kv `Put`), and the commercial backend
  (`internal/store/data_store.go:294-317`), which has zero references to
  `ScheduleFunction`. A new field round-trips with no plugin change.
- **No DTO↔SPI mapping layer.** Import decodes straight into SPI types with
  `DisallowUnknownFields` (`workflow/handler.go:190-192`), so
  `schedule.function.retryPolicy` is a 400 today and is accepted the moment the
  SPI field exists and the pin moves. Export emits the SPI structs verbatim.
  The `attachEntityProbe` second decode needs no entry: absent and `""` both
  mean "default".
- **Cross-node.** `DispatchCalloutRequest.Function` is `*spi.ScheduleFunction`
  (`cluster/dispatch/types.go:46`); the whole struct crosses the hop.
- **Guards that fail until extended.** `api/schedule_function_test.go:35, 88`
  hard-code the six-field list.
- **Schema version.** `CurrentSchemaVersion = "1.4"`, range
  `{Major:1, MinMinor:1, MaxMinor:4}` (`schemaversion.go:43, 62-64`). An
  optional field is an additive MINOR with dual-shape acceptance, as 1.2, 1.3
  and 1.4 were: next is `1.5`, `MaxMinor: 5`.
- **Criteria.** A criterion's function config is `json.RawMessage` at the SPI
  level, so validating its `retryPolicy` means parsing the `FunctionCondition`
  envelope at import.

## 9. Rulings (Paul, 2026-09-18)

1. **Functions** get the failover loop and `retryPolicy` on
   `ScheduleFunctionDto` — not an implicit `FIXED`. SPI field + schema `1.5`.
2. **No member available / pool exhausted**: keep retrying with the delay, as
   Cloud does, so a member that reconnects inside the window picks the work up.
   It must not multiply with `findPeerWithPolling`'s 5 s wait.
3. **The three-way split** in the second issue comment governs: infrastructure
   failure → fail over; member-reported `retryable: false` or absent → stop,
   non-retryable; member-reported `retryable: true` → stop, and the outer
   `WORKFLOW_FAILED` is retryable. This diverges from Cloud, which fails over on
   `true` and propagates no verdict. The case given up: a member-local transient
   is indistinguishable from a transaction-level one on a one-bit flag.
4. **Async suppression** (scope bullet 6, parity test c) is dropped:
   `asyncResult=true` cannot be imported, so the branch is unreachable.

**Superseded in part on 2026-09-19.** Ruling 3's "infrastructure failure → fail
over" was narrowed: what decides is whether the work reached a cnode, and after
delivery only criteria, functions and processors marked `idempotent` move on. The
agreed design is in `2026-09-19-254-design-brief.md`, which governs wherever the
two documents differ. Sections 10 to 12 below are the evidence gathered for it.

## 10. How Cloud reaches a cnode on another pnode

- **Record.** `CalculationMember{id, tags, connectedNodeId, legalEntityId}`
  (`backend/.../externalize/model/CalculationMemberNotification.kt:15-23`). The id
  is a random UUID minted per gRPC stream (`CloudEventsApiGrpcService.kt:152`), so
  a reconnect is a new member.
- **Registry.** No shared store. Each node holds an in-memory map of *all*
  members (`CalculationMemberService.kt:57`); the hosting node broadcasts
  online / offline / alive-changed once to the nodes ZooKeeper lists as up
  (`:99-126`, `DefaultIntercomNodeNotificationExchanger.kt:137-155`). No
  reconciliation: a late joiner is never told; nothing reacts to a node leaving
  (no `OnlineNodesChangedListener` anywhere in `backend/src/main`).
- **Choice.** The calling node picks a specific member id cluster-wide, local
  first, else first match; no balancing (`CalculationMemberService.kt:206-215`,
  with a `// todo we can store them sorted`). A member must have ALL required
  tags.
- **Send / reply.** Over the internal Netty "intercom" bus to the hosting node,
  which pushes it onto the member's stream (`:221-242, 323-385`); the reply
  returns the same way, matched on `requestId`.
- **Far-side failures that never reach the caller.** Hosting node does not know
  the member → exception swallowed into an "…with N errors" payload
  (`DefaultIntercomNodeNotificationExchanger.kt:234-256`); member disconnects
  mid-request on the hosting node → the forwarding coroutine has no failure
  handler (`CalculationMemberService.kt:370-382`). In both the caller waits out
  `withTimeout` (`ExternalizerBase.kt:318`). The hosting node's pending entry for
  forwarded work never expires (`Long.MAX_VALUE` overflow, `:286-288, 520-527`).
- **Callbacks.** `transactionId` is sent to the member as data only; inbound
  platform-API requests carry none and start their own transaction
  (`EntityCreateRequestHandler.kt:31-41`). No routing by transaction, no token.
- **Tests.** `ExternalizationExtendedIT.kt` runs two containerised nodes plus
  the test JVM and proves cross-node selection, timeout failover and
  disconnect failover (`:423-679`).

## 11. The delivery boundary in cyoda-go

- **Inside a pnode.** A member's outbox is an **unbuffered** channel
  (`internal/grpc/members.go:99, 129`); `Member.Send` returns nil only once the
  single writer goroutine has taken the item, and an error means the channel
  operation did not happen (`:137-157`; `members_outbox_test.go:65-96`).
  Certainly never left the process: `FindByTags` nil (`dispatch.go:210-214`),
  CloudEvent/auth build failure (`:111-117`), `ErrMemberEvicted` from
  `TrackRequest` (`:137-142`) or from `Send` (`:150-153`), parent ctx cancelled
  during enqueue (`:154-155`), enqueue timeout "member not draining"
  (`:156-160`). May have reached the member: `resp.Disconnected` while waiting
  (`:172-175`), response timeout (`:186-192`), ctx cancelled while waiting
  (`:187-188`). `AbandonRequest` only deletes the pending-response entry
  (`members.go:289-293`); once `Send` returned nil nothing stops the write.
- **The two sides share error codes.** `disconnectedErr` serves both `:153`
  (never sent) and `:174` (may have run); `DISPATCH_TIMEOUT` serves both the
  enqueue phase (`:158`) and the response phase (`:191`), told apart only by a
  log field and the message suffix. No machine-readable discriminator exists.
- **Across pnodes.** Every non-2xx the peer handler can emit is produced before
  it calls its local dispatcher — 403 auth/replay (`handler.go:135-146`), 400 bad
  body / tenant mismatch / unknown kind (`:50-53, 69-72, 126-128`); every
  dispatch outcome is a 200 (`:91, 102, 118`). Certainly not delivered: address
  validation or signing failure (`forwarder.go:58-60, 99-102`), non-2xx, 200 with
  `NO_COMPUTE_MEMBER_FOR_TAG`. Ambiguous: any `client.Do` error (a refused dial
  and a mid-flight failure are the same `fwdErr`, `:106-109`), the client timeout
  (`CYODA_DISPATCH_FORWARD_TIMEOUT`, 30 s), a decode error, and 200 with
  `DISPATCH_TIMEOUT` / `COMPUTE_MEMBER_DISCONNECTED` (the peer's enqueue-versus-
  response distinction is lost on the wire; message sanitised, `handler.go:91`).
  `forwardWithFailover` fails over on transport errors today and its own comment
  concedes the callout may run twice (`cluster_dispatcher.go:232-237`).
- **Request id.** `DispatchCalloutRequest` (`cluster/dispatch/types.go:17-47`)
  has no request-id field; the peer mints its own per callout
  (`internal/grpc/dispatch.go:218, 309, 358`).
- **Peer authentication.** `POST /internal/dispatch/callout` is outside the auth
  middleware; AES-256-GCM over the whole body with method, path and timestamp as
  associated data, 30 s skew, fail-closed nonce cache (`aead_peer_auth.go`,
  `nonce_cache.go`). Responses are not wrapped (`forwarder.go:78-80`).
- **Tx token.** Claims `{NodeID, TxRef, ExpiresAt}` only
  (`internal/cluster/token/token.go:18-22`): bearer, tenant-scoped at `Join`
  (`plugins/memory/txmanager.go:434-438`), nothing per callout or per member.
  TTL `CYODA_TX_TOKEN_TTL` 90 s; expiry is checked before the transaction is
  looked up, so an expired token is 410 `TRANSACTION_EXPIRED` even while the
  transaction is open (`token.go:83-87`, `txjoin.go:42-53`). Rolled back or
  committed → 404 `TRANSACTION_NOT_FOUND` (`txjoin.go:54-65`). Minted per
  dispatch call; on a forwarded dispatch the owner's token from ctx wins
  (`dispatch.go:60-73`).
- **Transaction lifetime.** No TTL, no reaper (`CHANGELOG.md:639-644`). The only
  server-side ceiling is PostgreSQL's per-idle-gap
  `idle_in_transaction_session_timeout`, default 5 m
  (`plugins/postgres/config.go:80-83, 195-196`) → 503 `STORAGE_UNAVAILABLE`;
  memory and SQLite have none. `transactionTimeoutMillis` is client-supplied,
  opt-in, and rejected on a request that joins an open transaction. No
  per-request deadline, by decision (`app/config.go:93-101`).
- **Unique keys inside one open transaction.** The second create of the same
  key value is refused on all three backends — PostgreSQL at the `Save`
  (`plugins/postgres/entity_store.go:360-363`, index `unique_claims_uq`), memory
  and SQLite at `Commit` (`plugins/memory/txmanager.go:585-596`;
  `plugins/sqlite/entity_store.go:469`). A documented, unreconciled divergence
  (`plugins/postgres/unique_claims_test.go:305-334`). The two-creates-in-one-tx
  case is tested on memory only (`memory/unique_claims_test.go:232-257`).
- **Selection today.** `members map[string]*Member`, no counter or last-used
  state; `Member.ConnectedAt` exists and is unused for selection
  (`members.go:95, 341-353, 467-479`). No test depends on which of several
  matching members is returned. `grpc.md:408` and `ARCHITECTURE.md:1232`
  document the random choice. The peer side already has a `PeerSelector`
  interface (`cluster/dispatch/selector.go:10-23`).

## 12. Cluster membership metadata

- **What is published.** `nodeMeta{id, addr, grpcAddr, tags map[tenant][]string}`
  as JSON in memberlist node metadata (`internal/cluster/registry/gossip.go:34-40`).
  `memberlist.MetaMaxSize = 512` is an untyped const with no config knob
  (`memberlist@v0.6.0/net.go:83`), enforced by `panic` at `memberlist.go:458-461`
  and `:517-520`. It exists because `Meta` rides in the `alive` message, gossiped
  over UDP within `UDPBufferSize` 1400 (`config.go:336`, `state.go:611-616`).
- **Oversize path.** `gossipDelegate.NodeMeta` logs a WARN and returns nil
  (`gossip.go:288-300`); memberlist then stores and gossips nil meta for the node
  (`state.go:1131`); `UpdateTags` returns nil, so `MemberRegistry.notifyChange`
  records the version as published and never retries
  (`internal/grpc/members.go:483-516`). `List` skips a node whose meta fails to
  parse (`gossip.go:196-219`); `Lookup` returns an error for it (`:183-194`).
  `Members()` includes self, so the node vanishes from its own view too.
- **Consumers and what breaks.** Callout routing `findPeer`
  (`cluster_dispatcher.go:315-335`) — the only reader of `Tags`; gRPC tx routing
  `ResolveNodeInfo` (`proxy/grpc.go:88-100`) → 503 `TRANSACTION_NODE_UNAVAILABLE`;
  HTTP tx routing (`proxy/http.go:57-78`) → **500**, because `Lookup` errors
  rather than reporting not-alive; scheduler `tick` (`scheduler/service.go:151`)
  — with an empty member list `LowestLiveNodeID.IsCoordinator` returns true on
  every node (`scheduler/coordinator.go:18-29`); `ClusterExecutor.Execute`
  (`cluster/scheduler_rpc.go:121-126`) drops the task.
- **Size.** `total = 81 + N·(L + 18)` bytes for N tenants with ids of length L,
  one 10-character tag each, and a 72-byte identity. L = 36 → over at N = 8
  (513 bytes); L = 10 → N = 16; L = 100 → N = 4.
- **No test** covers the oversize path. `ARCHITECTURE.md:1922` lists the limit
  with "Monitor and alert"; no metric or alert exists.
- **Other defects in the same function.** `UpdateTags` has two bare
  `g.mu.Unlock()` (`gossip.go:256, 260`) against `go-mutex-discipline.md`;
  `UpdateNode(0)` blocks until the alive broadcast is finished or invalidated
  (`memberlist.go:540-551`) while `publishMu` is held; `computeTagsLocked` ranges
  a map, so tag order is nondeterministic (`members.go:520-542`).
- **What memberlist offers.** `LocalState`/`MergeRemoteState`: TCP, on join and
  every `PushPullInterval` (30 s LAN, one random node per round, flat up to 32
  nodes), user state up to 20 MiB (`net.go:88, 1251-1252`); cyoda-go's are no-ops
  (`gossip.go:323-324`). `SendReliable`: TCP, "no limit on the size of the
  message", delivery guaranteed if no error, one node per call
  (`memberlist.go:597-603`). Gossip broadcast via `GetBroadcasts`: UDP, a message
  must fit the free space of a 1400-byte packet or is silently skipped
  (`queue.go:317-329`) — the existing `Broadcast`/`Subscribe` facility
  (`gossip_broadcast.go`) inherits that. `EventDelegate.NotifyJoin/Leave/Update`
  exist; cyoda-go registers none (`gossip.go:68-75`). Config is
  `DefaultLANConfig()` with name, bind address/port, secret key, delegate and log
  output overridden; encryption is on.
- **Harness.** `gossip_test.go` starts two `NewGossip` instances in one process on
  fixed loopback ports and polls; `TestGossipRegistry_TagPropagation` (`:85`) is
  the natural base for a "many tenants stay visible" test.
