# Callout failover, honest failure reporting, and cluster membership that scales

Specification for issue #254 (milestone v0.9.0), which also takes in #565.

The agreed design, in plain language and with its scenarios, is
`docs/superpowers/research/2026-09-19-254-design-brief.md` (the **brief**). The
evidence for every statement about today's code and about Cyoda Cloud is
`docs/superpowers/research/2026-09-18-254-retry-policy-member-failover-research.md`
(the **research**, cited as R§n). This document turns the brief into decisions an
implementation plan can be written from. Where it departs from the text of #254
it does so on rulings recorded in the brief; the brief governs over the issue.

Items marked **V-n** are verification points: facts the design relies on that
have not yet been read in the code. The plan resolves each before the task that
depends on it.

## 0. Terms

- **pnode** — one cyoda-go process. **Owner** — the pnode where the client's
  operation arrived and its transaction lives.
- **cnode** — a customer's compute program, attached to exactly one pnode over
  the `StartStreaming` gRPC stream. In code: `internal/grpc.Member`. A
  reconnect is a new `Member` with a new id (R§2.3).
- **Callout** — one processor, criterion or function request. Three entry points
  on `contract.ExternalProcessingService`: `DispatchProcessor`,
  `DispatchCriteria`, `DispatchFunction` (R§2.1).
- **Try** — one attempt to hand a callout's work to one chosen cnode, whether or
  not the hand-off succeeds. Looking for a cnode and finding none is *not* a
  try, and neither is failing to reach another pnode.
- **Hand-off** — `Member.Send` returning nil: the single writer goroutine has
  taken the event from the member's unbuffered outbox (R§11). Before it, the
  work provably never left the pnode. After it, the work may have reached the
  cnode.
- **Hand-over** — the owner passing a callout to another pnode
  (`POST /internal/dispatch/callout`).
- **Repeat-safe** — a callout that may be tried on another cnode after a
  hand-off: every criterion, every function, and a processor whose config has
  `idempotent: true`.
- **Pass** — the transaction token given to a cnode with the work
  (`internal/cluster/token`).
- **Patience** — how long a callout waits for a cnode to exist.
- **Answer limit** — how long a pnode waits for one cnode's answer
  (`responseTimeoutMs`).

## 1. What is already done (not redone here)

- The inbound `retryable` flag is on `ProcessingResponse` for all three callout
  kinds (R§1, #262).
- `retryPolicy` on a **processor** is validated at import (R§1, #262).
- Importing `asyncResult: true` or any `crossoverToAsyncMs` is refused, so #254's
  "async suppression" has nothing to suppress. It is dropped, with parity test
  (c); #223 owns it.
- Cross-pnode failover over *unreachable peers* exists (`forwardWithFailover`,
  R§3). It is replaced, not extended (§5).

## 2. Decisions

| # | Decision |
|---|---|
| D1 | One line decides whether another cnode may be tried: **was there a hand-off?** No hand-off → always. Hand-off, then no answer or a dropped connection → only if the callout is repeat-safe. A cnode that answered "failed" → never. |
| D2 | `idempotent` (bool, default false) is added to `spi.ProcessorConfig`. It is a declaration by the workflow author, covering cyoda and every system the processor touches. Criteria and functions are repeat-safe by rule. |
| D3 | `retryPolicy` selects the number of tries: `NONE` → 1; `FIXED` or unset → 1 + `CYODA_RETRY_FIXED_NUM_RETRIES`. It is added to `spi.ScheduleFunction` and validated at import on criteria and functions as it already is on processors. |
| D4 | Every pnode runs the same **local procedure** over its own cnodes. There is no pause between cnodes. The next cnode is chosen by a replaceable selector; the first is round robin. |
| D5 | The owner runs the local procedure first, then hands the callout to one alive pnode advertising the tag **with the tries left**. A pnode that receives a hand-over runs the local procedure only and never hands on. |
| D6 | The number of tries is the normal number, not a hard limit. A lost hand-over answer counts as one try. |
| D7 | **Patience** is separate from tries. `CYODA_DISPATCH_WAIT_TIMEOUT` is kept and becomes the one waiting mechanism: single pnode and cluster, regardless of `retryPolicy`, event-driven, one allowance per callout. `CYODA_RETRY_FIXED_DELAY_MS` from #254 is not introduced. |
| D8 | A pass names its callout. When a callout ends the owner closes it and then drains the transaction's gate; a callback bearing a closed callout's pass is refused. |
| D9 | The hand-over carries the request id, tries left, answer limit and owner id; its answer states explicitly whether there was a hand-off, and is authenticated and encrypted like the request. |
| D10 | The default answer limit and an upper bound on it become configuration (#565). |
| D11 | Tenants and tags leave the 512-byte memberlist node metadata. The metadata keeps identity and a list version; lists travel by reliable message and are fetched by any pnode that is behind. |
| D12 | The cnode's own failure message and verdict reach the client, across pnodes too. |

## 3. Classifying what happened to a try

A new type in `internal/contract` carries the outcome of a failed callout
through every layer that today flattens it to a string (R§4.3):

```go
// CalloutFailure reports why a callout produced no result.
type CalloutFailure struct {
    Kind      CalloutFailureKind
    Code      string   // error code for the client; empty for MemberFailed
    Message   string   // client-safe text; for MemberFailed, the cnode's own
    Retryable *bool    // MemberFailed only: the cnode's verdict, nil if absent
    Attempts  []CalloutAttempt // every try made, for the exhaustion message
}

type CalloutFailureKind int
const (
    NoHandOff     CalloutFailureKind = iota // another cnode may be tried
    NoAnswer                                // hand-off, then silence or a drop
    MemberFailed                            // the cnode answered success=false
    Terminal                                // would fail identically anywhere
)
```

Every error site in `dispatchCalloutToMember` and its three callers is assigned a
kind; none is left to a default (R§11):

| Site (`internal/grpc/dispatch.go`) | Kind |
|---|---|
| no matching member | `NoHandOff` |
| `TrackRequest` → `ErrMemberEvicted` | `NoHandOff` |
| `Send` → `ErrMemberEvicted` | `NoHandOff` |
| `Send` → deadline ("member not draining") | `NoHandOff` |
| `Send` → parent ctx cancelled | returns `ctx.Err()` unchanged; the loop ends |
| response wait → `resp.Disconnected` | `NoAnswer` |
| response wait → answer limit | `NoAnswer` |
| response wait → parent ctx cancelled | returns `ctx.Err()` unchanged; the loop ends |
| `resp.Success == false` | `MemberFailed`, with `resp.Error` and `resp.Retryable` |
| CloudEvent build, auth-context attach | `Terminal` |
| response payload unmarshal | `Terminal` |

`disconnectedErr` and the two `DISPATCH_TIMEOUT` constructions keep their codes,
statuses and messages; the kind travels beside them. The enqueue wait and the
response wait keep their shared deadline (the answer limit): a cnode that is
attached but not taking data costs up to one answer limit before it is
classified `NoHandOff`.

**Whether another cnode may be tried:**

| Kind | Repeat-safe callout | Processor, `idempotent` false |
|---|---|---|
| `NoHandOff` | yes | yes |
| `NoAnswer` | yes | **no** — stop |
| `MemberFailed` | no — stop | no — stop |
| `Terminal` | no — stop | no — stop |

## 4. The local procedure (`internal/grpc`)

`ProcessorDispatcher` gains one method used by all three callout kinds:

```go
// RunLocal tries the callout on this pnode's own matching cnodes, one after
// another, until one answers, a failure that forbids another try occurs, the
// matching cnodes are used up, or maxTries is reached.
func (d *ProcessorDispatcher) RunLocal(ctx context.Context, call Callout, maxTries int) LocalResult
```

`Callout` holds what the three entry points build today — kind, tenant, tags,
the request builder, the response mapper — plus `RequestID`, `AnswerLimit`,
`RepeatSafe`, `OwnerNodeID` and `TxID`. `LocalResult` holds the mapped result or
a `*CalloutFailure`, `TriesUsed`, and `Attempts`.

- A cnode is never tried twice within one `RunLocal`. The tried set lives in the
  call and is not shared between calls or pnodes (brief §4: a second visit may
  try a cnode again; accepted).
- Every try sends the same `RequestID` as CloudEvent `id` and as `requestId`.
  Correlation is per member (R§2.3), so two tries in flight cannot be confused.
- Every try mints its own pass (§7).
- Between tries `RunLocal` checks `ctx.Err()`, so a cancelled owner, a client
  that went away, or a pnode shutting down ends the procedure promptly.
- When no untried matching cnode remains it returns `NoHandOff` with the tries
  it used. It does not wait; waiting is the owner's (§6).

**Selection.** `FindByTags` is replaced by `Candidates(tenantID, tagsCSV)
[]*Member`, ordered by `(ConnectedAt, ID)` so the order is stable, and a
selector:

```go
type MemberSelector interface {
    // Select picks one of candidates (never empty). key identifies the
    // (tenant, tags) pair the choice is being made for.
    Select(key string, candidates []*Member) *Member
}
```

`RoundRobinSelector` keeps, per key, the id it returned last and returns the
candidate after it in the stable order (the first if that id is gone). Its map
is bounded by dropping a key when `Candidates` for it is empty. No test today
depends on which of several matching cnodes is chosen (R§11). The peer selector
(`PeerSelector`, random) is unchanged.

## 5. The owner's loop (`internal/callout`, new)

A new package holds the one loop. `Coordinator` implements
`contract.ExternalProcessingService` and replaces both the direct use of
`ProcessorDispatcher` (single pnode) and `ClusterDispatcher`'s three
near-identical methods (cluster). `app.go` builds it in both modes; in single
pnode mode its peer router is nil. `TracingExternalProcessingService` still wraps
the outside.

```
resolve tries from retryPolicy (D3); create RequestID; start patience clock at 0
loop:
    r := local.RunLocal(ctx, call, triesLeft)            # §4
    triesLeft -= r.TriesUsed; record r.Attempts
    if r ok                      -> return result
    if r stops (table in §3)     -> return failure        # §8 decides the error
    if triesLeft == 0            -> return exhaustion
    for each alive peer advertising the tag, in selector order, not yet asked
    in this pass:
        a := peers.HandOver(ctx, peer, call, triesLeft)   # §6
        triesLeft -= a.TriesUsed; record a.Attempts
        if a ok / a stops / triesLeft == 0 -> as above
    # nothing anywhere had a cnode to offer
    if patience used up          -> return NO_COMPUTE_MEMBER_FOR_TAG
    wait for a membership change, or the rest of the patience, or ctx
    start a new pass (the asked set is cleared)
```

- A peer that cannot be connected to, or that answers "no cnode", uses no try.
- **Patience (D7).** The wait is on a change signal, not a timer loop.
  `MemberRegistry` and the node registry each expose `Changed() <-chan struct{}`,
  a channel closed and replaced on every change (a cnode attaching or detaching
  locally; a peer's list arriving, a peer joining or leaving). The allowance is
  cumulative across the callout: `CYODA_DISPATCH_WAIT_TIMEOUT` in total, not per
  wait. `0` disables waiting.
- `findPeerWithPolling` and its 200 ms poll are deleted. `forwardWithFailover`
  is deleted; its behaviour on an unreachable peer is the loop's "uses no try,
  ask the next".
- **Worst case** for one callout, normal case: `tries × answer limit +
  patience`, plus one connect timeout per unreachable peer per pass.

## 6. The hand-over (`internal/cluster/dispatch`)

`DispatchCalloutRequest` gains:

| Field | Meaning |
|---|---|
| `requestID` | created by the owner; used for every try (today the peer mints its own, R§11) |
| `triesLeft` | the most tries the peer may make; ≥ 1 |
| `answerLimitMs` | resolved by the owner (§9), so two pnodes cannot disagree |
| `ownerNodeID` | the node id the peer puts in the passes it mints (§7) |
| `repeatSafe` | decided by the owner from D1/D2 |

`DispatchCalloutResponse` is restated:

| Field | Meaning |
|---|---|
| `outcome` | `ok` \| `no_handoff` \| `no_answer` \| `member_failed` \| `terminal` |
| `triesUsed` | tries the peer made |
| `attempts` | one entry per try: member id, kind, client-safe text |
| `memberError`, `memberRetryable` | the cnode's own message and verdict (`member_failed` only) |
| `errorCode`, `errorStatus`, `errorRetryable` | as today, for the peer's own classified errors |
| result fields | as today |

The peer handler calls `RunLocal` with `triesLeft` and never the `Coordinator`.

**How the owner reads an answer.** Only a decoded, authenticated response whose
`outcome` is `no_handoff` means "nothing was handed to a cnode". Everything else
that is not `ok`, `member_failed` or `terminal` is `no_answer`: a transport error
after the connection was opened, the wait running out, a truncated body, any
non-2xx status (`middleware.Recovery` answers 500 for a panic *after* a hand-off,
and an intermediary can answer 502–504), or a response missing `outcome`. An
answer that never arrives, or arrives without `triesUsed`, counts as **one** try
(D6) — never zero, so the loop always makes progress. The peer may in fact have
made more; that is the case in which the total exceeds the setting. **A connection that could not be
opened** — `*net.OpError` with `Op == "dial"`, including the connect timeout —
is `no_handoff` and uses no try.

**Transport.**
- The forwarder sets `Request.GetBody`, so `net/http` retries the one case it can
  prove wrote nothing (a stale pooled connection) instead of reporting it as an
  ambiguous failure.
- A connect timeout separate from the wait: `CYODA_DISPATCH_CONNECT_TIMEOUT`,
  default `2s`, on the transport's dialer.
- The owner's wait for the answer is `triesLeft × answerLimit +
  CYODA_DISPATCH_FORWARD_TIMEOUT`. That setting's meaning narrows from "the whole
  wait" to "the allowance on top of what the cnodes may take"; its default (30 s)
  and its use by the scheduler RPC client are unchanged. The forwarder therefore
  takes a per-request deadline from `ctx` rather than a client-wide `Timeout`.
- **The response is wrapped** with the request's AEAD scheme. The peer encrypts
  the response body under the same derived key with the request's nonce and
  timestamp bound in as associated data, so an answer cannot be forged or
  replayed onto another request. `Content-Type: application/cyoda-dispatch-v1`
  on both legs. An answer that fails to open is `no_answer`.

## 7. The pass ends with its callout

**Claims.** `token.Claims` gains `CalloutID` (`"c"`), set to the callout's
`RequestID`. A pass is minted per try by the pnode that makes the hand-off —
every pnode holds the cluster secret — with `NodeID` = the owner's id and
`ExpiresAt = now + answer limit + CYODA_TX_TOKEN_TTL`. `CYODA_TX_TOKEN_TTL`'s
meaning narrows from "the pass's whole life" to "the allowance beyond the answer
limit" (routing, clock difference between pnodes); default unchanged. The
`resolveTxToken` rule "a token already on ctx wins" is removed: no pass is minted
before the try it belongs to.

**Closing.** `txgate.Registry` gains:

```go
// CloseCallout marks calloutID closed for txID, then acquires and releases
// txID's gate, so that on return no callback of that callout holds the gate
// and none can take it.
func (r *Registry) CloseCallout(txID, calloutID string)

// CalloutClosed reports whether calloutID was closed for txID. It is
// meaningful only while the caller holds txID's gate.
func (r *Registry) CalloutClosed(txID, calloutID string) bool

// Forget drops everything held for txID. Called when the owner's
// transaction scope is released.
func (r *Registry) Forget(txID string)
```

The `Coordinator` calls `CloseCallout` on the owner when a callout ends — with a
result, a failure, or a cancelled context — before it returns to the engine.
While tries of one callout are in progress its passes stay open: two cnodes
working at once is what `idempotent` promises to tolerate.

A callback is checked **every time it takes the gate** — on entry, and again
whenever it takes the gate back. The second matters for nested callouts: a
callback can itself run a workflow that makes a callout, and it releases the
gate for the length of that inner callout (`txgate.Suspend`). If the outer
callout is given up in that window the barrier finds the gate free and passes,
so the check on re-acquiring is what stops the callback from carrying on. `resume`
therefore reports a closed callout, and the joined operation is abandoned with
`CALLOUT_ENDED` without touching the transaction again (**V-1**: the call sites
that acquire the gate for a joined request, HTTP and gRPC, and the unwinding
path after a failed `resume`; `suspend_call_sites_test.go` lists the engine
side).

`CloseCallout` takes the gate, so it must run where the calling chain does not
hold it: inside the window in which the engine has suspended the gate around the
dispatch, which is where the `Coordinator` runs. The owner's own chain never
holds the gate.

A closed callout's pass is refused with `CALLOUT_ENDED` (§8). A pass without
`CalloutID` is refused as invalid: passes are minted only for callouts
(`grpc/dispatch.go:67` and `cluster_dispatcher.go:213` are the only callers of
`Signer.Issue`), so after this change none exists.

This closes, for every backend and without a plugin change, both the case this
change introduces (a given-up cnode of an idempotent processor writing after its
replacement answered and later processors ran) and the one that exists today
(`ASYNC_NEW_TX`, where a failed callout does not fail the operation and the
transaction goes on to commit). The commercial backend's `Join` checks tenant
and open transaction exactly as the in-tree ones do and is covered equally.

Callouts with no transaction (`COMMIT_BEFORE_DISPATCH` with
`startNewTxOnDispatch: false`) carry no pass, as today.

## 8. What the client sees

### 8.1 By processor mode

The brief's "the operation fails, the transaction is rolled back, cyoda's own
state is clean for a re-run" is true only where this table says so.

| Mode | Another cnode after `NoAnswer` | Callout fails → | cyoda state after the failure |
|---|---|---|---|
| `SYNC`, `ASYNC_SAME_TX` | if `idempotent` | operation fails; error per §8.2 | rolled back; clean for a re-run, **unless** an earlier `COMMIT_BEFORE_DISPATCH` processor in the same cascade already committed |
| `ASYNC_NEW_TX` | if `idempotent` | **operation continues**; savepoint undone; a WARN is logged; nothing reaches the client, not even `MemberFailed`'s verdict | the rest of the transaction commits; the failed cnode's pass is closed (§7) |
| `COMMIT_BEFORE_DISPATCH`, new tx on dispatch | if `idempotent` | operation fails; error per §8.2 | TX_pre **stays committed**; TX_post rolled back. Not clean for a re-run. Already documented as at-least-once |
| `COMMIT_BEFORE_DISPATCH`, no tx on dispatch | if `idempotent` | operation fails; error per §8.2 | TX_pre stays committed; the cnode's callbacks were transactions of their own and stand |

Criteria (all three call sites, R§4.2) and the arming function are always
repeat-safe; an error aborts the save as today. What the scheduler does with a
retryable failure of an arming function is unchanged by this design (**V-2**:
read `arm.go`'s failure path and state it in the plan; there is no client to
re-run a scheduled transition).

### 8.2 Error / status table

HTTP entity endpoints that run workflows — create, create-collection, update,
transition, loopback — and the gRPC `EntityManage` / `EntityManageCollection`
envelopes (`CLIENT_ERROR` / `SERVER_ERROR` with the code as the message prefix,
`retryable` set when true; R§4.3).

| Situation | Status | Code | Retryable | Message |
|---|---|---|---|---|
| No cnode appeared within the patience | 503 | `NO_COMPUTE_MEMBER_FOR_TAG` | yes | as today |
| One try, `NoHandOff`, tries = 1 | 503 | the try's own code (`COMPUTE_MEMBER_DISCONNECTED`, `DISPATCH_TIMEOUT`) | yes | as today |
| `NoAnswer`, processor not idempotent | 503 | the try's own code | yes | as today |
| Every try used, more than one attempt | 503 | **`CALLOUT_TRIES_EXHAUSTED`** (new) | yes | `all tries exhausted, got N failures: [member<id>: cause], [member<id>: cause (2 times)]` — Cloud's shape (R§5): N counts before collapsing; identical entries collapse |
| Every try used, exactly one attempt recorded | 503 | that attempt's own code | yes | not wrapped |
| `MemberFailed`, verdict true | 400 | `WORKFLOW_FAILED` | **yes** | `processor <name> failed: <cnode message>` |
| `MemberFailed`, verdict false or absent | 400 | `WORKFLOW_FAILED` | no | same |
| `Terminal` | as today (500 ticketed for auth-context; 400 `WORKFLOW_FAILED` otherwise) | | no | as today |
| Hand-over could not be made to any peer and no local cnode | 503 | `NO_COMPUTE_MEMBER_FOR_TAG` | yes | as today; `DISPATCH_FORWARD_FAILED` is **retired** with `forwardWithFailover` |
| Callback bearing a closed callout's pass | 410 | **`CALLOUT_ENDED`** (new) | no | `the callout this transaction pass was issued for has ended` |
| Import: criterion or function `retryPolicy` not `NONE`/`FIXED`/unset | 400 | `VALIDATION_FAILED` | no | names the workflow, state, transition |
| Import: `responseTimeoutMs` above the upper bound, or negative | 400 | `VALIDATION_FAILED` | no | names the bound |

`classifyWorkflowError` gains one branch, before the catch-all: a
`*contract.CalloutFailure` of kind `MemberFailed` becomes `Operational(400,
WORKFLOW_FAILED, err.Error())`, `.AsRetryable()` when the verdict is true. The
engine's `processor %s failed: %w` wrap is kept, so the message still names the
processor. Every other kind already carries an `*AppError` and passes through the
first branch.

Member ids appear in client-visible text. They identify the tenant's own
connections, are random, and are already given to the cnode in its greet. Node
ids and peer addresses do not appear (as `forwardFailedClientMessage` ensures
today).

`retryable: true` on a `NoAnswer` failure speaks for cyoda's state only, and only
in the rows of §8.1 that say "clean". The help topics for `DISPATCH_TIMEOUT`,
`COMPUTE_MEMBER_DISCONNECTED` and `WORKFLOW_FAILED` say so, and say that what the
processor did outside cyoda is the application's to reconcile.

## 9. Configuration

| Setting | Default | Rule | Notes |
|---|---|---|---|
| `CYODA_RETRY_FIXED_NUM_RETRIES` | `3` | int ≥ 0 | new. Retries after the first try |
| `CYODA_CALLOUT_RESPONSE_TIMEOUT_MS` | `30000` | int ≥ 1, ≤ the max | new. Replaces `defaultResponseTimeoutMs` |
| `CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS` | `60000` | int ≥ 1 | new (#565). Import refuses a larger `responseTimeoutMs` |
| `CYODA_DISPATCH_WAIT_TIMEOUT` | `5s` | duration ≥ 0 | kept; meaning widened to the patience (D7) |
| `CYODA_DISPATCH_CONNECT_TIMEOUT` | `2s` | duration > 0 | new |
| `CYODA_DISPATCH_FORWARD_TIMEOUT` | `30s` | unchanged | meaning narrowed (§6) |
| `CYODA_TX_TOKEN_TTL` | `90s` | unchanged | meaning narrowed (§7) |

Invalid values are startup errors, not clamps (`app/config.go:756-760`). Each new
key passes the four guards in R§7 and is validated both in `Config.Validate()`
and by name in `cmd/cyoda/main.go`.

The answer limit for a callout is its `responseTimeoutMs` if positive, else
`CYODA_CALLOUT_RESPONSE_TIMEOUT_MS`. A stored workflow whose value exceeds a
bound that was lowered later is clamped to the bound at fire time and a WARN is
logged; import is where it is refused.

The relation to PostgreSQL's idle-in-transaction ceiling
(`CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT`, 5 m) is documented, not enforced: the root
module does not read plugin configuration. With every default, `4 × 60 s + 5 s`
stays under it.

## 10. Workflow configuration, SPI, schema version

- `spi.ProcessorConfig.Idempotent bool` (`json:"idempotent,omitempty"`).
- `spi.ScheduleFunction.RetryPolicy string` (`json:"retryPolicy,omitempty"`). A
  plain string keeps the struct comparable (`schedule_roundtrip_test.go:139`).
- Both ride one SPI change, pseudo-pinned per the coordinated-release procedure;
  Paul cuts the tag.
- OpenAPI: `idempotent` on `ExternalizedProcessorConfigDto`; `retryPolicy` on
  `ScheduleFunctionDto`; `maximum` documented on the three `responseTimeoutMs`.
  The `retryPolicy` descriptions drop "N and delay" for "the number of tries is
  server-configured, and is the normal number, not a hard limit". `go generate
  ./api`.
- Import validation (`validate.go`): criterion function config is parsed for
  `retryPolicy` and `responseTimeoutMs`; function likewise; processors gain the
  `responseTimeoutMs` bound. New rule-ledger entries beside M1.
- **Schema 1.4 → 1.5**, additive MINOR, dual-shape (`MaxMinor: 5`), with the
  full ceremony in `docs/workflow-schema-versioning.md`. Validating criterion
  `retryPolicy` and bounding `responseTimeoutMs` tighten what import accepts;
  with no deployments on either tier they are recorded in that document as
  tightenings taken in the same MINOR, with the reason.
- No storage backend changes: all four persist the workflow as one JSON document
  (R§8).

## 11. Cluster membership (`internal/cluster/registry`)

**Defect.** R§12. Tenants and tags ride in memberlist node metadata, capped at
512 bytes by an untyped constant; past it the pnode publishes nil metadata,
every peer and the pnode itself drop it from `List`, `Lookup` errors, and nothing
retries.

**Metadata** becomes identity plus a list version:

```go
type nodeMeta struct {
    ID       string  `json:"id"`
    Addr     string  `json:"addr"`
    GRPCAddr string  `json:"grpcAddr,omitempty"`
    Tags     listVersion `json:"tv"`   // {epoch: process start, unix nanos; seq: counter}
}
```

Its size depends only on operator settings. `NewGossip` marshals it with the
largest possible version and **refuses to start** if it exceeds
`memberlist.MetaMaxSize`, naming `CYODA_NODE_ID`, `CYODA_NODE_ADDR` and
`CYODA_GRPC_NODE_ADDR`. The oversize branch in `gossipDelegate.NodeMeta` becomes
unreachable and is deleted.

**Lists.** Each pnode holds `map[nodeID]{version, tags}`. Versions order by
`(epoch, seq)`, so a pnode restarted under the same id — the Helm chart's normal
case — supersedes its former self. A pnode never accepts a foreign copy of its
own list.

- *Publish.* On a change the pnode bumps `seq`, updates its metadata
  (`UpdateNode`, off the publish lock and without the blocking wait: a bounded
  timeout rather than `0`), and sends `{nodeID, version, tags}` to every alive
  member with `memberlist.SendReliable`, concurrently, each send bounded by
  memberlist's TCP timeout. Tags are sorted, so an unchanged set marshals
  identically.
- *Receive.* Reliable user messages arrive at `NotifyMsg` beside gossip
  broadcasts, so they use the existing topic framing (`cluster.tags`,
  `cluster.tags.request`) and are handed to a worker: handlers on memberlist's
  receive goroutine must not block. A list older than the one held is ignored.
- *Catch up.* An `EventDelegate` is registered. On `NotifyJoin` and
  `NotifyUpdate`, a pnode that holds a version older than the one in the node's
  metadata — or none — sends `cluster.tags.request`; the node answers with its
  list. One path covers a lost message, a late joiner and a healed partition.
  A request that gets no list is repeated on the next metadata event for that
  node and, as a floor, on a slow timer while the mismatch lasts.
- *Leave.* `NotifyLeave` drops the node's list.
- `List` returns each member's identity with its held tags (empty if not yet
  known). `List` and `Lookup` treat unparseable metadata the same way — not
  alive, with a WARN — so HTTP transaction routing answers 503, not 500.
- Both reliable paths are encrypted when a secret key is set, and it always is in
  cluster mode (`memberlist net.go:904-911, 956, 1196-1198`; `app.go:1113-1116`).
- The registry's change signal (§5) fires on list arrival, join and leave.

`UpdateTags`' two bare `Unlock()` calls are replaced per
`go-mutex-discipline.md`.

## 12. Observability and logging

- Counters (**V-3**: match the existing metric naming and registration):
  tries by callout kind and outcome; hand-overs by outcome; callouts that waited
  and for how long; `CALLOUT_ENDED` refusals; failed list sends and outstanding
  list requests — the alert `ARCHITECTURE.md:1922` promises.
- One INFO line per callout that needed more than one try or waited, with
  tenant, tags, tries, elapsed. Per-try detail at DEBUG. A cnode's failure
  message is tenant content: it goes to the client and to `common.AddError` as
  today, and is not repeated at INFO. No pass, secret or token is logged at any
  level.
- The tracing decorator records tries and whether a hand-over happened on the
  callout's span.

## 13. Coverage matrix

Layers: **U** unit · **E** running-backend e2e (`internal/e2e`, PostgreSQL) ·
**G** gRPC envelope (`internal/grpc`) · **P** cross-backend parity
(`e2e/parity`) · **M** multi-pnode parity (`e2e/parity/multinode`).

Harness work this needs, as tasks of their own:
- `internal/e2e`'s callback harness attaches **several** scripted cnodes (it
  attaches one today).
- `cmd/compute-test-client` takes its tags and a behaviour from the environment
  (`stall`, `fail`, `fail-retryable`, `late-callback`), and the parity fixtures
  gain an optional capability to start and stop extra compute clients. A backend
  fixture without it skips those scenarios, so the commercial backend's suite is
  not broken by their arrival.
- Round robin over a stable order makes "the first cnode attached is tried
  first" deterministic; scenarios rely on that, never on timing.

| Scenario | U | E | G | P | M |
|---|---|---|---|---|---|
| Every dispatcher error site → its kind (§3) | ✓ | | | | |
| `NoHandOff` → next local cnode answers | ✓ | ✓ | | ✓ | |
| `NoAnswer`, processor not idempotent → stop, 503 own code | ✓ | ✓ | ✓ | ✓ | |
| `NoAnswer`, `idempotent` → next cnode answers | ✓ | ✓ | | ✓ (09_10) | |
| `NoAnswer`, criterion / function → next cnode answers | ✓ | ✓ | | ✓ | |
| cnode drops after hand-off (09_09, 09_11) — both settings | ✓ | ✓ | | ✓ | |
| `MemberFailed` verdict true → 400 retryable, one try | ✓ | ✓ | ✓ | ✓ | |
| `MemberFailed` verdict false / absent → 400, one try | ✓ | ✓ | ✓ | ✓ | |
| `retryPolicy: NONE` → one try | ✓ | ✓ | | ✓ | |
| Every try used → 503 `CALLOUT_TRIES_EXHAUSTED`, message shape | ✓ | ✓ | ✓ | | |
| No cnode → waits, one attaches → succeeds, no try used | ✓ | ✓ | | ✓ | ✓ |
| No cnode within patience → 503 `NO_COMPUTE_MEMBER_FOR_TAG`; patience 0 → at once | ✓ | ✓ | ✓ | | |
| `ASYNC_NEW_TX`: callout fails → operation succeeds | ✓ | ✓ | | | |
| Late callback after the callout ended → 410 `CALLOUT_ENDED` (HTTP and gRPC doors) | ✓ | ✓ | ✓ | | |
| Late callback in `ASYNC_NEW_TX` after failure → 410 | | ✓ | | | |
| Callback during the callout, second cnode working → accepted | ✓ | ✓ | | | |
| Outer callout given up while its callback is inside a nested callout → the callback is abandoned on taking the gate back, 410 | ✓ | ✓ | | | |
| Round robin across two cnodes | ✓ | ✓ | | | |
| Import: criterion / function `retryPolicy` invalid → 400 | ✓ | ✓ | | ✓ | |
| Import: `responseTimeoutMs` over the bound → 400 | ✓ | ✓ | | ✓ | |
| Import / export round-trip of `idempotent`, function `retryPolicy`; schema 1.5 | ✓ | ✓ | | ✓ | |
| Owner has a cnode that fails `NoHandOff` → hand-over succeeds | ✓ | | | | ✓ |
| Hand-over: peer makes two tries in one exchange | ✓ | | | | ✓ |
| Hand-over answer lost → one try counted; not idempotent → stop | ✓ | | | | |
| Peer unreachable (dial) → no try used, next peer | ✓ | | | | ✓ |
| Non-2xx / truncated / unauthenticated answer → `no_answer` | ✓ | | | | |
| cnode message and verdict survive the hand-over | ✓ | | | | ✓ |
| Response AEAD: forged or replayed answer refused | ✓ | | | | |
| Pass minted by a peer joins the owner's transaction; closed after | ✓ | | | | ✓ |
| Many tenants on one pnode stay visible (R§12 sizes) | ✓ | | | | ✓ |
| Restart under the same id supersedes the old list | ✓ | | | | |
| Late joiner / lost list message → fetched | ✓ | | | | |
| Leaving pnode's list dropped | ✓ | | | | |
| Identity over 512 bytes → refuses to start | ✓ | | | | |
| Each new setting: default, valid, invalid → startup error | ✓ | | | | |

Concurrency (two callouts racing on one cnode; attach and detach during a
callout; a callback racing `CloseCallout`) is tested in isolated single-backend
e2e and unit tests under `-race`, never in the shared parity suite, asserting
consistency — one outcome, no torn write — not an interleaving.

The existing tests that pin single-shot behaviour (R§7) are each revisited: most
stay true for a callout with one matching cnode and `NoAnswer` on a
non-idempotent processor; those asserting `forwardWithFailover` or
`findPeerWithPolling` go with them.

## 14. Gate 4 documentation touch-set

- Help: `config/cluster.md`, `config/grpc.md` (new and changed settings);
  `workflows.md` (`idempotent`, `retryPolicy` on all three, the "captured but not
  consumed" note removed, the tag-matching sentence corrected to any-overlap,
  side effects outside cyoda and the saga responsibility); `grpc.md` (selection is
  round robin; same request id on every try and what an SDK that de-duplicates on
  it will do; criteria and functions must have no effects); `cluster.md`;
  `errors/CALLOUT_TRIES_EXHAUSTED.md`, `errors/CALLOUT_ENDED.md` (new);
  `errors/WORKFLOW_FAILED.md`, `DISPATCH_TIMEOUT.md`,
  `COMPUTE_MEMBER_DISCONNECTED.md`, `NO_COMPUTE_MEMBER_FOR_TAG.md` (revised);
  `errors/DISPATCH_FORWARD_FAILED.md` removed with its code; `errors.md` index,
  and its false sentence about gRPC trailer metadata corrected.
- `README.md` configuration reference; `DefaultConfig()`; `config_registry.go`.
- `docs/ARCHITECTURE.md`: cluster discovery, DD-7, the operational-limits row,
  the dispatch section (incl. the stale "no failover to a second peer" row), the
  selection sentence — audited as a whole, present tense.
- `docs/workflow-schema-versioning.md`; `docs/WORKFLOW_IMPORT_EXPORT_AUDIT.md`
  §M1.
- `docs/cloud-parity/callout-failover.md` (new) and a row in its README: the
  departures from Cloud listed in the brief, as a contract Cloud can implement.
- `CHANGELOG.md` `[Unreleased]`: Added, Changed, Fixed (the membership defect,
  the late-callback hole), and the retired setting semantics.
- `COMPATIBILITY.md`: the SPI pin.
- Stale comments: `members.go:48-55`, `validate.go:66-68`,
  `internal/common/errors.go:18-20` (orphaned), `arm.go:243`.

## 15. Compatibility

No deployments exist on either tier, and pnode-to-pnode messages carry no
compatibility promise before 1.0. A mixed-version cluster is not supported: a
pnode of this version reads an answer without `outcome` as `no_answer` and one
without `triesUsed` as one try, so it degrades safely and cannot loop. Workflows
at schema 1.1–1.4 import unchanged, except that a criterion carrying an invalid
`retryPolicy`, or any callout with a `responseTimeoutMs` above the new bound, is
now refused. `### Breaking` in the CHANGELOG records: `DISPATCH_FORWARD_FAILED`
retired; a single pnode with no cnode waits out the patience before failing; the
narrowed meanings of `CYODA_DISPATCH_FORWARD_TIMEOUT` and `CYODA_TX_TOKEN_TTL`;
passes minted by earlier versions are refused.

## 16. Out of scope

A cluster-wide list of individual cnodes. Enforcing that criteria and functions
have no effects. Choosing cnodes by load. `EPOCH_MISMATCH` being defined,
documented and never raised (R§6) — to be filed. The scheduler treating an empty
cluster view as "I am the coordinator": unreachable once a pnode is always in its
own view, noted for whoever next touches `scheduler/coordinator.go`.
