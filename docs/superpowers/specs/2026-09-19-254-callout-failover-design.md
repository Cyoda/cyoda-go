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
  try, and neither is failing to *connect to* another pnode. A hand-over whose
  answer is lost counts as one try: the other pnode may have chosen a cnode.
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
| D6 | The number of tries is the normal number, not a hard limit: a lost hand-over answer counts as one try. The **time** a callout may take is a hard limit, fixed when it starts. |
| D7 | **Patience** is separate from tries. `CYODA_DISPATCH_WAIT_TIMEOUT` is kept and becomes the one waiting mechanism: single pnode and cluster, regardless of `retryPolicy`, event-driven, one allowance per callout. `CYODA_RETRY_FIXED_DELAY_MS` from #254 is not introduced. |
| D8 | A pass names its callout and is valid only while that callout is open on the owner. When the callout ends the owner drains the transaction's gate; a joined operation bearing an ended callout's pass is refused on entry, and abandoned if it comes back from a nested callout. |
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
- Between tries `RunLocal` checks `ctx.Err()`, so a cancelled owner or a client
  that went away ends the procedure promptly. A pnode shutting down does not:
  `http.Server.Shutdown` waits for requests in progress without cancelling their
  contexts (`cmd/cyoda/run.go`), so a callout in progress runs on until the
  drain budget is spent, as today.
- When no untried matching cnode remains it returns `NoHandOff` with the tries
  it used. It does not wait; waiting is the owner's (§6).

**Selection.** `FindByTags` is replaced by `Candidates(tenantID, tagsCSV)
[]*Member`, ordered by `(ConnectedAt, ID)` so the order is stable, and a
selector:

```go
type MemberSelector interface {
    // Select picks one of candidates (never empty; already-tried cnodes are
    // not among them).
    Select(candidates []*Member) *Member
}
```

`RoundRobinSelector` returns the candidate that was picked longest ago, ties
broken by the stable order, and stamps it. The stamp is a field on `Member`, set
from one counter on the registry, so there is no per-tag state to grow — tag
strings are tenant-supplied — and cnodes that come and go are handled without
bookkeeping: a new one has never been picked and goes first. `RunLocal` passes
the selector only the candidates it has not tried, so "never twice" and the
selector compose. No test today depends on which of several matching cnodes is
chosen (R§11). The peer selector (`PeerSelector`, random) is unchanged.

## 5. The owner's loop (`internal/callout`, new)

A new package holds the one loop. `Coordinator` implements
`contract.ExternalProcessingService` and replaces both the direct use of
`ProcessorDispatcher` (single pnode) and `ClusterDispatcher`'s three
near-identical methods (cluster). `app.go` builds it in both modes; in single
pnode mode its peer router is nil. `TracingExternalProcessingService` still wraps
the outside.

```
resolve tries from retryPolicy (D3); create RequestID; open the callout (§7)
deadline := now + tries × answer limit + patience + hand-over allowance
loop:
    take both Changed() channels            # before looking, so no wake-up is lost
    r := local.RunLocal(ctx, call, triesLeft)            # §4
    triesLeft -= r.TriesUsed; record r.Attempts
    if r ok                      -> return result
    if r stops (table in §3)     -> return failure        # §8 decides the error
    if triesLeft <= 0            -> return exhaustion
    for each alive peer advertising the tag, in selector order, not yet asked
    in this pass:
        a := peers.HandOver(ctx, peer, call, triesLeft)   # §6
        triesLeft -= a.TriesUsed; record a.Attempts
        if a ok / a stops / triesLeft <= 0 -> as above
    # no cnode anywhere took the work in this pass
    if patience used up or deadline passed -> return per the precedence below
    wait for either channel, or the rest of the patience, or ctx
    start a new pass (the asked set is cleared)
```

- A peer that cannot be connected to, or that answers "no cnode", uses no try.
- **Patience (D7).** The wait is on a change signal, not a timer loop.
  `MemberRegistry` and the node registry each expose `Changed() <-chan struct{}`,
  a channel closed and replaced on every change (a cnode attaching or detaching
  locally; a peer's list arriving, a peer joining or leaving). The allowance is
  cumulative across the callout and counts elapsed waiting time:
  `CYODA_DISPATCH_WAIT_TIMEOUT` in total, not per wait. `0` disables waiting. A
  pass that made tries may still wait: a cnode that dropped and is coming back
  is the case the patience exists for.
- **Precedence when nothing more can be done.** If the callout recorded any
  attempt, the error reports the attempts (the single attempt's own code, or
  `CALLOUT_TRIES_EXHAUSTED` with the list). `NO_COMPUTE_MEMBER_FOR_TAG` is
  returned only when no try was ever made.
- **A hard limit on time, though not on tries (D6).** A lost hand-over answer
  counts one try while the peer may have made more, so the *number* of tries can
  exceed the setting. The *time* cannot: the deadline above is fixed when the
  callout starts, no try or hand-over starts after it, and one in progress is
  cut off at it (as `NoAnswer`). Without it a peer that hangs on every hand-over
  would cost `(4+3+2+1) × 30 s + 4 × 30 s` = 7 minutes at the defaults, past
  PostgreSQL's five-minute ceiling.
- `findPeerWithPolling` and its 200 ms poll are deleted. `forwardWithFailover`
  is deleted; its behaviour on an unreachable peer is the loop's "uses no try,
  ask the next".
- **Worst case** for one callout: `tries × answer limit + patience + hand-over
  allowance` — at the defaults 4 × 30 s + 5 s + 30 s = 155 s; at the upper bound
  of the answer limit, 4 × 60 s + 35 s = 275 s.

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

`triesUsed` is accepted only when `0 ≤ triesUsed ≤ triesLeft`; anything else
makes the answer `no_answer`. A refusal by the peer's handler before it does
anything — 403 for a failed or replayed authentication, which includes pnode
clocks more than 30 s apart — is a non-2xx and therefore `no_answer` too: it
cannot be authenticated, so it is not taken as proof. Clocks that far apart fail
non-repeat-safe operations rather than being quietly routed around.

**Transport.**
- **Every hand-over opens its own connection** (`DisableKeepAlives` on the
  hand-over transport). With a kept-alive connection a peer that died is
  discovered only when the read fails — after the whole wait, and
  indistinguishable from a peer that took the work and then died. With a fresh
  connection the rule "could not connect → no hand-off" is the whole rule. The
  cost is one TCP handshake per hand-over.
- A connect timeout separate from the wait: `CYODA_DISPATCH_CONNECT_TIMEOUT`,
  default `2s`, on the transport's dialer.
- The owner's wait for the answer is `triesLeft × answerLimit +
  CYODA_CALLOUT_HANDOVER_ALLOWANCE` (§9), and never past the callout's deadline
  (§5). It is a per-request deadline on `ctx`; the hand-over client has no
  client-wide `Timeout`. `CYODA_DISPATCH_FORWARD_TIMEOUT` keeps its name and its
  meaning — the whole wait — for the scheduler RPC client, which has its own
  `http.Client`, and no longer governs hand-overs.
- **The response is authenticated and encrypted** with the key the request uses.
  It gets **a fresh random nonce of its own** — reusing the request's would break
  the cipher. Its associated data is a direction label (`"response"`; requests
  gain `"request"`), the path, and the request's nonce and timestamp, so an
  answer cannot be forged, reflected back as a request, or replayed onto another
  request. Responses do not enter the replay cache: the binding to a request
  nonce the owner chose is what makes them single-use. An owner asking the same
  peer again on a later pass uses a new request nonce, so the fail-closed cache
  does not get in the way. `Content-Type: application/cyoda-dispatch-v1` on both
  legs. An answer that fails to open is `no_answer`.

## 7. The pass ends with its callout

**Claims.** `token.Claims` gains `CalloutID` (`"c"`), set to the callout's
`RequestID`. A pass is minted per try by the pnode that makes the hand-off —
every pnode holds the cluster secret — with `NodeID` = the owner's id and
`ExpiresAt = now + answer limit + CYODA_CALLOUT_PASS_ALLOWANCE` (§9). The
`resolveTxToken` rule "a token already on ctx wins" is removed, and with it
`DispatchCalloutRequest.TxToken`, `WithTxToken` and `TxTokenFromContext`: no pass
is minted before the try it belongs to. `Signer.Issue` gains the callout id; its
two production callers and the ten test files that call it change with it.

**A pass is valid only while its callout is open on the owner.** The rule is
stated this way round — open, not "not yet closed" — so that it fails closed and
needs no clean-up keyed by transaction:

```go
// package callout
// open registers calloutID and returns the func that ends it. The Coordinator
// calls it at the start of a callout and defers the returned func, so a callout
// is ended on every exit path, a panic included.
func (r *OpenCallouts) open(calloutID string) (end func())

// IsOpen reports whether calloutID is currently open on this pnode.
func (r *OpenCallouts) IsOpen(calloutID string) bool
```

There is nothing to forget when a transaction ends, and nothing depends on the
transaction id, which changes mid-operation under `COMMIT_BEFORE_DISPATCH` and is
owned by the scheduler, not the entity service, on a scheduled fire.

All tries of one callout share its id. A cnode that was given up on therefore
keeps a valid pass until the callout ends: two cnodes working at once is what
`idempotent` promises to tolerate. What no promise covers is a write arriving
after the callout, and the processors that follow it, are done.

**Three places enforce it**, because a callback has three ways to be running:

1. *On entry.* `txjoin.JoinFromToken` — the one function both doors and every
   proxied request pass through on the owner (`httpmw/txjoin_mw.go:33`,
   `grpc/txroute_interceptor.go:147, 188`) — refuses a verified pass whose
   callout is not open, with `CALLOUT_ENDED` (§8), and puts the callout id on the
   context. This covers every joined operation: reads, searches and non-entity
   endpoints as well as entity writes, none of which take the gate
   (`entity/service.go:427`, `search/handler.go:175-185`).
2. *In progress when the callout ends.* The callout's `end` func removes it from
   the open set and then takes and releases the transaction's gate once. Entity
   writes hold the gate for their whole length (`acquireJoinedGate`,
   `entity/handler.go:121-125`, nine callers), so on return none is still
   running. `end` runs deferred, inside the window in which the engine has
   released the gate around the dispatch; the owner's own chain never holds it.
   It can wait as long as a callback's store call takes; it cannot deadlock —
   one mutex per transaction, nested callbacks release it, and the registry lock
   is never held across a gate wait.
3. *Coming back from a nested callout.* A callback can itself run a workflow
   that makes a callout, releasing the gate for its length (`txgate.Suspend`).
   If its own callout ends in that window, step 2 finds the gate free. `resume`
   **always** re-acquires — the caller's deferred release would otherwise unlock
   an unheld mutex, which is a fatal no `recover` catches. Each of the five sites
   that resume then asks, through one helper, whether the chain's callout is
   still open, **before touching the transaction**, and abandons the operation
   with `CALLOUT_ENDED` if not:

   | Site | After `resume()`, if the callout has ended |
   |---|---|
   | `engine_processors.go:204` (SYNC) | return the error; no result is applied |
   | `engine_processors.go:233` (ASYNC_NEW_TX, no tx manager) | return the error |
   | `engine_processors.go:249` (ASYNC_NEW_TX, savepoint) | return the error **without** `RollbackToSavepoint`; the abandoned chain makes no further use of the transaction |
   | `engine.go:1014` (criterion) | `resume` is no longer only deferred: its result is checked before the verdict is used |
   | `arm.go:239` (function) | return the error |

   The per-item loop at `entity/service.go:2640-2655` stops at this error rather
   than continuing to the next item. What an abandoned chain had already written
   stays in the owner's transaction: in `ASYNC_NEW_TX` the owner's savepoint
   rollback removes it; otherwise it is "partly run", which is what `idempotent`
   declares tolerable.

The two `COMMIT_BEFORE_DISPATCH` dispatch sites (`engine_processors.go:333,
365`) do not release the gate around their dispatch. They are unchanged; a joined
chain that reaches them holds the gate through the inner callout, and step 2
waits for it.

**What is guaranteed.** Once a callout has ended, no joined operation bearing its
pass starts, and no entity write bearing it is in progress or resumes. A joined
*read* already in progress may complete. A pass without `CalloutID` is refused as
invalid: passes are minted only for callouts (`grpc/dispatch.go:67` and
`cluster_dispatcher.go:213` are the only production callers of `Signer.Issue`),
so after this change none exists.

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
| `MemberFailed`, verdict true | 400 | `WORKFLOW_FAILED` | **yes** | `processor <name> failed: <cnode message>`; for a criterion `failed to evaluate transition criterion: <cnode message>` (or `…workflow criterion for "<wf>"…`); for a function the arming wrap. Today's inner `processor dispatch failed:` segment goes |
| `MemberFailed`, verdict false or absent | 400 | `WORKFLOW_FAILED` | no | same |
| `Terminal` | as today (500 ticketed for auth-context; 400 `WORKFLOW_FAILED` otherwise) | | no | as today |
| A hand-over's answer was lost — no reply, a broken connection, an answer that does not authenticate — and the callout is not repeat-safe, or it was the only attempt | 503 | `DISPATCH_FORWARD_FAILED` (kept) | yes | the sanitised message it has today; recorded as an attempt with member `-` |
| No peer could be connected to, and no local cnode | 503 | `NO_COMPUTE_MEMBER_FOR_TAG` | yes | as today |
| Joined request bearing the pass of a callout that has ended (on entry, or on coming back from a nested callout) | 410 | **`CALLOUT_ENDED`** (new) | no | `the callout this transaction pass was issued for has ended` |
| Joined request bearing a pass with no callout id | 401 | `UNAUTHORIZED` | no | `invalid transaction token`, as any malformed pass today |
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
| `CYODA_DISPATCH_WAIT_TIMEOUT` | `5s` | duration ≥ 0 | kept; meaning widened to the patience (D7); validated for the first time |
| `CYODA_DISPATCH_CONNECT_TIMEOUT` | `2s` | duration > 0 | new |
| `CYODA_CALLOUT_HANDOVER_ALLOWANCE` | `30s` | duration > 0 | new. What the owner allows a hand-over on top of `tries × answer limit` |
| `CYODA_CALLOUT_PASS_ALLOWANCE` | `30s` | duration > 0 | new. How long a pass outlives its try's answer limit (routing; clocks that differ between pnodes) |
| `CYODA_DISPATCH_FORWARD_TIMEOUT` | `30s` | duration > 0 | name and meaning unchanged; now used by the scheduler RPC client only; validated for the first time |
| `CYODA_TX_TOKEN_TTL` | — | — | **removed**: a pass no longer has a fixed life |

New names rather than old names with new meanings: an operator who lowered
`CYODA_DISPATCH_FORWARD_TIMEOUT` to tighten hand-overs would otherwise cut off
delegated scheduled fires.

Out-of-range values are startup errors, not clamps (`app/config.go:756-760`). A
value that does not parse falls back to the default silently — that is how every
setting behaves today (`envInt`, `envDuration`), and changing it is not part of
this work. Each new
key passes the four guards in R§7 and is validated both in `Config.Validate()`
and by name in `cmd/cyoda/main.go`.

The answer limit for a callout is its `responseTimeoutMs` if positive, else
`CYODA_CALLOUT_RESPONSE_TIMEOUT_MS`. A stored workflow whose value exceeds a
bound that was lowered after it was imported is **not** quietly clamped — a
substituted value is what `correctness-over-availability.md` rules out. The
callout fails as `Terminal`, naming the setting. Because the bound is a server
setting, a workflow exported from one deployment can be refused by another with
a lower bound; the help text says so.

The relation to PostgreSQL's idle-in-transaction ceiling
(`CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT`, 5 m) is documented, not enforced: the root
module does not read plugin configuration. §5's deadline is what makes the
arithmetic hold: 275 s at the upper bound with every other default.

**The scheduler.** `CYODA_SCHEDULER_REDISPATCH_BACKOFF` (30 s) is a throttle, not
a lease (`scheduler/service.go:42-47`): a scheduled fire still running after it
is dispatched again, and its processors run a second time, concurrently. Today
that takes a fire longer than 30 s; with this change one timed-out try on an
idempotent processor is enough. **Open — §17.**

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

**Lists.** Each pnode holds `map[nodeID]{version, tags}`. **The version a pnode
announces in its own metadata is the authority** for which of its lists is
current. A peer holds the right list when the version it holds *equals* the
announced one; on any difference it fetches. Versions are never ordered across
epochs — only compared for equality — so a pnode that restarts under the same id
(the Helm chart's normal case), even with a clock that stepped backwards or on a
host whose clock is behind, cannot be mistaken for an older self and ignored.
The epoch only has to differ between two lives of one pnode; epochs of different
pnodes are never compared. A pnode never accepts a foreign copy of its own list.

- *Publish.* On a change the pnode bumps `seq`, updates its metadata
  (`UpdateNode` with a bounded timeout, off the publish lock; a timeout leaves the
  broadcast queued, so its error is logged at DEBUG and otherwise ignored), and
  sends `{nodeID, version, tags}` to every other alive member with
  `memberlist.SendReliable`, concurrently, each send bounded by memberlist's TCP
  timeout. Tags are sorted, so an unchanged set marshals identically.
- *Receive.* A list is stored when its version equals the one announced in the
  sender's metadata, or is a later `seq` of that epoch (the list can arrive
  before the metadata does). Anything else is dropped; the mismatch that remains
  triggers a fetch.
- *Catch up.* An `EventDelegate` is registered. On `NotifyJoin` and
  `NotifyUpdate`, a pnode that holds no list for the node, or one whose version
  differs from the announced one, sends `cluster.tags.request`; the node answers
  with its list. One path covers a lost message, a late joiner, a restarted
  pnode and a healed partition. A request that gets no list is repeated on the
  next metadata event for that node and, as a floor, on a slow timer while the
  mismatch lasts. Twenty pnodes starting together exchange about 380 small
  messages; no throttling is needed.
- *Leave.* `NotifyLeave` drops the node's list.
- **Nothing calls into memberlist from inside one of its callbacks.**
  `NotifyJoin`, `NotifyUpdate` and `NotifyLeave` run under memberlist's node lock
  (`state.go:941-943`): a `SendReliable` there stalls all membership processing
  for up to the TCP timeout, and `Members()` or `UpdateNode` deadlocks.
  `NotifyMsg` runs on the receive goroutine. All four only copy what they were
  given — `NotifyMsg`'s buffer is reused by the library — and enqueue it for one
  worker goroutine, which does the sending, fetching and storing. `NotifyJoin`
  fires for the pnode itself inside `memberlist.Create`, before the registry
  holds its `*Memberlist`; the worker starts after `Create` returns and ignores
  events about self. Reliable user messages arrive at `NotifyMsg` beside gossip
  broadcasts (`net.go:1344`), so they use the existing topic framing
  (`cluster.tags`, `cluster.tags.request`).
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
(`e2e/parity`) · **M** multi-pnode parity (`e2e/parity/multinode`). A **w** is a
waiver, with its reason below the table.

**What each harness can do, and what it needs.**
- `internal/e2e` builds a server per test and its callback harness already has
  `newComputeMember` and `stop()` (`callback_harness_test.go:533, 653`); it is
  extended to attach **several** scripted cnodes. Per-test server configuration
  (`newCallbackHarnessConfigured`) makes patience and tries settable per test.
  This is the layer for anything that depends on order or timing.
- The parity suites share one server, one tenant and one tag across all
  scenarios (`fixtureutil.go:206`; `compute-test-client/dispatch.go:88-93`), so
  selector state carries over between scenarios. `cmd/compute-test-client`
  therefore takes its **tags** and a **behaviour** from the environment —
  `stall`, `fail`, `fail-retryable`, `late-callback`, `drop` (close the stream
  on receiving work) — and the fixtures gain an optional capability to start and
  stop extra compute clients. Each failover scenario uses a tag of its own and
  attaches its cnodes one after the other, which is what makes "the cnode
  attached first is tried first" hold. A backend fixture without the capability
  skips those scenarios, so the commercial backend's suite is not broken by
  their arrival.
- Parity fixtures set `CYODA_DISPATCH_WAIT_TIMEOUT` low for the whole package
  (the precedent is `CYODA_SCHEDULER_SCAN_INTERVAL=50ms`), so the existing "no
  compute member" scenario does not gain five seconds on every backend.

| Scenario | U | E | G | P | M |
|---|---|---|---|---|---|
| Every dispatcher error site → its kind (§3) | ✓ | | | | |
| `NoHandOff` → next local cnode answers | ✓ | w¹ | | w¹ | |
| `NoHandOff`, tries = 1 → the try's own code | ✓ | w¹ | ✓ | | |
| `NoAnswer`, processor not idempotent → stop, 503 own code | ✓ | ✓ | ✓ | ✓ | |
| `NoAnswer`, `idempotent` → next cnode answers (09_10) | ✓ | ✓ | | ✓ | |
| `NoAnswer`, criterion; function → next cnode answers | ✓ | ✓ | | ✓ | |
| cnode drops after hand-off (09_09, 09_11) — both settings | ✓ | ✓ | | ✓ | |
| `MemberFailed` verdict true → 400 retryable, one try | ✓ | ✓ | ✓ | ✓ | |
| `MemberFailed` verdict false / absent → 400, one try | ✓ | ✓ | ✓ | ✓ | |
| `Terminal` → stop, as today | ✓ | ✓ | ✓ | | |
| `retryPolicy: NONE` on a processor, a criterion, a function → one try | ✓ | ✓ | | ✓ | |
| Every try used → 503 `CALLOUT_TRIES_EXHAUSTED`, message shape | ✓ | ✓ | ✓ | ✓ | |
| Exactly one attempt recorded → not wrapped | ✓ | ✓ | | | |
| Attempts on record beat "no cnode" (§5 precedence) | ✓ | ✓ | | | |
| Same request id on every try | ✓ | ✓ | | | |
| Callout deadline cuts off a try in progress | ✓ | | | | |
| Client timeout (408) / cancellation during a wait and during a try | ✓ | ✓ | ✓ | | |
| No cnode → waits, one attaches → succeeds, no try used | ✓ | ✓ | | w² | w² |
| Patience is one allowance across several waits | ✓ | | | | |
| Patience applies with `retryPolicy: NONE` | ✓ | ✓ | | | |
| No cnode within patience → 503 `NO_COMPUTE_MEMBER_FOR_TAG`; patience 0 → at once | ✓ | ✓ | ✓ | ✓ | |
| `ASYNC_NEW_TX`: callout fails → operation succeeds, nothing reported | ✓ | ✓ | | | |
| `COMMIT_BEFORE_DISPATCH`, both variants: failure leaves TX_pre committed (§8.1) | ✓ | ✓ | | | |
| Callouts made from a scheduled fire follow the same rules | ✓ | ✓ | | | |
| Late callback after the callout ended → 410 `CALLOUT_ENDED`, HTTP and gRPC doors, write and read | ✓ | ✓ | ✓ | | |
| Late callback in `ASYNC_NEW_TX` after failure → 410 | | ✓ | | | |
| Callback during the callout, second cnode working → accepted | ✓ | ✓ | | | |
| Outer callout ends while its callback is in a nested callout → abandoned on coming back, at each of the five sites | ✓ | ✓ | | | |
| Callout ended by a panic → its pass is refused | ✓ | | | | |
| Pass without a callout id → 401; expired pass → 410 `TRANSACTION_EXPIRED` | ✓ | ✓ | | | |
| Pass lifetime follows the answer limit | ✓ | | | | |
| Round robin across two cnodes; a new cnode goes first | ✓ | ✓ | | | |
| Two tenants share a tag on one pnode: each callout goes only to its tenant's cnode; attempts name only that tenant's cnodes | ✓ | ✓ | | | ✓ |
| Import: criterion / function `retryPolicy` invalid → 400 | ✓ | ✓ | | ✓ | |
| Import: `responseTimeoutMs` over the bound; negative → 400 | ✓ | ✓ | | ✓ | |
| Stored `responseTimeoutMs` over a lowered bound → `Terminal` | ✓ | | | | |
| Import / export round-trip of `idempotent`, function `retryPolicy`; schema 1.5 | ✓ | ✓ | | ✓ | |
| Owner's cnode fails → hand-over succeeds | ✓ | | | | ✓ |
| Hand-over: peer makes two tries in one exchange; honours the owner's answer limit | ✓ | | | | ✓ |
| A pnode that receives a hand-over never hands on | ✓ | | | | |
| Hand-over answer lost → one try counted; not repeat-safe → 503 `DISPATCH_FORWARD_FAILED` | ✓ | | | | |
| `triesUsed` out of range → `no_answer` | ✓ | | | | |
| Peer cannot be connected to → no try used, next peer | ✓ | | | | w³ |
| No peer can be connected to, no local cnode → 503 `NO_COMPUTE_MEMBER_FOR_TAG` | ✓ | | | | |
| Non-2xx / truncated / unauthenticated answer → `no_answer` | ✓ | | | | |
| cnode message and verdict survive the hand-over | ✓ | | | | ✓ |
| Response protection: forged, reflected or replayed answer refused; nonce never reused | ✓ | | | | |
| Pass minted by a peer joins the owner's transaction; refused once the callout ended | ✓ | | | | ✓ |
| Many tenants on one pnode stay visible (R§12 sizes) | ✓ | | | | ✓ |
| Restart under the same id, with an earlier clock → new list accepted | ✓ | | | | |
| Late joiner / lost list message → fetched | ✓ | | | | |
| Leaving pnode's list dropped | ✓ | | | | |
| No call into memberlist from inside a callback (review + a test that a slow peer does not stall membership events) | ✓ | | | | |
| Identity over 512 bytes → refuses to start | ✓ | | | | |
| Unparseable metadata → not alive; HTTP transaction routing 503, not 500 | ✓ | | | | |
| Each new or newly validated setting: default, valid, out of range → startup error | ✓ | | | | |

¹ A hand-off cannot be made to fail from outside the process. A cnode that has
gone is evicted and is no longer a candidate; a frozen cnode's first event is
taken by the writer, which *is* a hand-off. The unit tests drive `Member.Send`
directly.
² Starting a subprocess inside a few seconds of patience is a race on a slow CI
runner, and the fixture's patience is fixed per package. The e2e layer attaches
an in-process cnode, which is deterministic.
³ The peer selector is random, so the assertion would hold by luck half the
time, and killing a node damages the shared fixture for later scenarios.

Concurrency (two callouts racing on one cnode; attach and detach during a
callout; a callback racing the end of its callout) is tested in isolated
single-backend e2e and unit tests under `-race`, never in the shared parity
suite, asserting consistency — one outcome, no torn write — not an interleaving.

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
  `COMPUTE_MEMBER_DISCONNECTED.md`, `NO_COMPUTE_MEMBER_FOR_TAG.md`,
  `DISPATCH_FORWARD_FAILED.md` (revised); `errors.md` index, and its false
  sentence about gRPC trailer metadata corrected; `telemetry.md` (the
  `cyoda.dispatch.duration` buckets stop at 10 s, below one answer limit; new
  counters); `config/scheduler.md`; `workflows/schema-version.md`; `search.md:201`
  and any other mention of the 30000 default; `cloudevents.md` and the see-also
  lists that name changed topics.
- `docs/cloud-parity/scheduled-transitions.md:273`.
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
now refused. `### Breaking` in the CHANGELOG records: a single pnode with no
cnode waits out the patience before failing; `CYODA_TX_TOKEN_TTL` removed;
`CYODA_DISPATCH_FORWARD_TIMEOUT` no longer governs hand-overs; passes minted by
earlier versions are refused; a joined request arriving after its callout ended
is refused, where it was accepted until the transaction closed.

## 16. Out of scope

A cluster-wide list of individual cnodes. Enforcing that criteria and functions
have no effects. Choosing cnodes by load. `EPOCH_MISMATCH` being defined,
documented and never raised (R§6) — to be filed. The scheduler treating an empty
cluster view as "I am the coordinator": unreachable once a pnode is always in its
own view, noted for whoever next touches `scheduler/coordinator.go`. Parse
failures of settings falling back silently to defaults.

Seen by the specification's reviewer and **not verified**, to be checked and
filed on its own: a joined callback whose workflow contains a
`COMMIT_BEFORE_DISPATCH` processor appears able to commit the owner's
transaction — the engine has no guard, and the service's guard runs only
afterwards (`entity/service.go:336`).

## 17. Open — needs the product owner

**Scheduled fires and the 30-second redispatch.** A scheduled transition is
dispatched again if its fire has not finished within
`CYODA_SCHEDULER_REDISPATCH_BACKOFF` (30 s); that value is a throttle, not a
lease, and the fire it overtakes keeps running
(`scheduler/service.go:42-47, 174`; `cluster/scheduler_rpc.go:155, 273`). Both
fires run the transition's processors; one commit wins and the other is rolled
back, but the processors — and whatever they do outside cyoda — ran twice. That
is cyoda repeating a processor on its own initiative, which D1 exists to prevent.
It can happen today to any fire that takes longer than 30 s. This change makes it
ordinary: one timed-out try on an idempotent processor, at the default answer
limit, is already 30 s.

Two ways to settle it, not exclusive:

- **Now, small:** require at startup that the redispatch backoff exceed the
  longest a callout can take (§5's worst case) and raise its default to match —
  about five minutes. Cost: a fire that is genuinely lost (its pnode died) is
  picked up after five minutes instead of thirty seconds.
- **Properly, its own piece of work:** give a scheduled task a real lease, so
  that a fire in progress cannot be overtaken however long it runs, and a lost
  one is reclaimed promptly when its holder is seen to be gone.
