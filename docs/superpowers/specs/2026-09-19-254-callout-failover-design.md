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
| D8 | A cnode that was replaced is **fenced**, on the model the commercial backend uses for a returning processing node: each callout has a number that rises when the work is given to another cnode; the pass carries it; the owner admits a callback only under the current number, checks it again under the transaction's write lock, and waits for a write in progress to finish before the work goes to the next cnode or the engine carries on. No database statement is interrupted. |
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
`RepeatSafe`, `OwnerNodeID`, `TxID`, `Outer` (the enclosing pairs, §7) and
`Number`. `LocalResult` holds the mapped result or a `*CalloutFailure`,
`TriesUsed`, and `Attempts`.

```go
// TryNumberer gives the fencing number for the next try (§7). RunLocal calls
// it once before each try, before it mints that try's pass.
type TryNumberer interface {
    Next() (major, minor uint32)
}
```

On the owner the `Coordinator` supplies it: `Next` raises `major`, calls
`Fence.Advance` — which shuts the earlier cnode out and waits for its write in
progress — and returns `(major, 0)`. On a pnode that received a hand-over it
counts `minor = 1, 2, …` under the `major` the request carried and touches no
fence: the arbiter is the owner's. This is what lets one `RunLocal` make several
tries while the number still rises before each.

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
resolve tries from retryPolicy (D3); create RequestID
ctx, end := fence.Begin(ctx, RequestID, TxID, fence.Pairs(ctx)); defer end()   # §7
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
        number.Next()                                     # the same counter RunLocal draws from; §7
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
  `CALLOUT_FAILED` with the list). `NO_COMPUTE_MEMBER_FOR_TAG` is
  returned only when no try was ever made.
- **A hard limit on time, though not on tries (D6).** A lost hand-over answer
  counts one try while the peer may have made more, so the *number* of tries can
  exceed the setting. The *time* cannot: the deadline above is fixed when the
  callout starts, no try or hand-over starts after it, and one in progress is
  cut off at it (as `NoAnswer`). Without it a peer that hangs on every hand-over
  would cost `(4+3+2+1) × 30 s + 4 × 30 s` = 7 minutes at the defaults, past
  PostgreSQL's five-minute ceiling.
- **The deadline is a context derived from the caller's**, never the caller's
  own. A try cut off by it is `NoAnswer`; only the caller's context ending — the
  client went away, or its `transactionTimeoutMillis` fired — returns
  `ctx.Err()` unchanged (408 or a cancelled request, as today). This is how
  `dispatch.go:186-192` already tells a per-try timeout from a dead parent.
  `context.Cause` tells the fence's cancellation (§7) from both.
- A cancelled caller ends the loop at once, from a wait as from a try; the
  callout's `end` still runs, because it is deferred.
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
| `major` | the fencing number of this hand-over; the peer numbers its tries `minor = 1, 2, …` under it (§7) |
| `outer` | the enclosing pairs, copied into every pass the peer mints (§7); empty unless the callout was made from inside a callback |
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

What the owner can prove *before* it connects — a peer address that fails
validation, a request that cannot be marshalled or signed
(`forwarder.go:58, 82, 99`) — would fail identically for every try and is
`Terminal`, not a lost answer. And where the peer refuses a request it has
already opened and authenticated — a replayed nonce, a full replay cache — it
answers with an authenticated `no_handoff` rather than a bare 403, so that a
saturated cache does not fail non-repeat-safe operations.

**Transport.**
- **Every hand-over opens its own connection** (`DisableKeepAlives` on the
  hand-over transport). With a kept-alive connection a peer that died is
  discovered only when the read fails — after the whole wait, and
  indistinguishable from a peer that took the work and then died. With a fresh
  connection the rule "could not connect → no hand-off" is the whole rule. The
  cost is one TCP handshake per hand-over. The rule holds for pnodes that reach
  each other directly. Behind a sidecar or an ingress the connection always
  opens and a dead peer shows as a 502–504, which is `no_answer`; with an
  `https://` node address a failed TLS handshake is not a dial error either, and
  the transport gets a TLS handshake timeout equal to the connect timeout. Both
  err on the safe side, and the help text says so. The transport's `Proxy`
  stays nil: with a proxy the failure is reported as `proxyconnect`, not `dial`.
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
  gain `"request"` — a change to the request format that also applies to the
  scheduler RPC, which signs and verifies through the same `PeerAuth`; both
  sides change together), the path, and the request's nonce and timestamp, so an
  answer cannot be forged, reflected back as a request, or replayed onto another
  request. Responses do not enter the replay cache: the binding to a request
  nonce the owner chose is what makes them single-use. An owner asking the same
  peer again on a later pass uses a new request nonce, so the fail-closed cache
  does not get in the way. `Content-Type: application/cyoda-dispatch-v1` on both
  legs. An answer that fails to open is `no_answer`.

## 7. Fencing a cnode that was replaced

**The problem.** Giving up on a cnode does not stop it. Its late *answer* is
already discarded (R§11). But while it works a cnode makes callbacks — ordinary
API requests carrying the pass — and a pass names only the transaction, so a
callback from a cnode that was replaced is accepted for as long as the
transaction is open. The relevant case: a slow cnode's callback arrives after
its replacement answered and a *later* processor of the same transition has
run; its write lands last and is committed. The same is possible today in
`ASYNC_NEW_TX`, where a failed callout does not fail the operation: the write of
a processor that *failed* lands after its savepoint was undone, and is
committed.

**The model** is the one the commercial storage backend uses to fence a
processing node that returns after its shards were taken over: a number that
only goes up decides who holds the work; the check is made in the same step
that gives the right to write; whoever takes the work over waits until the
earlier holder is out before it proceeds; "too slow" is kept distinct from
"replaced"; and what cannot be stopped is stated rather than denied. Here the
arbiter needs no database step: every callback is already routed to the pnode
that holds the transaction, so that pnode decides, in memory, for as long as
the callout lasts.

**The fencing number.** Each callout has one, a pair `(major, minor)` ordered
lexicographically. The owner raises `major` each time it gives the work to a
cnode of its own and each time it hands the callout over to another pnode; its
own tries carry `minor = 0`. A pnode that receives a hand-over numbers the tries
it makes `minor = 1, 2, …` under the `major` it was given. The number rises
**only** on the way to giving the work to another cnode — just before the
hand-off is attempted, so also when that hand-off then fails, which is harmless.
A timeout that fails the operation rolls the transaction back and needs no
fencing. The Coordinator holds the one `major` counter of a callout; its
`TryNumberer` (§4) and its hand-overs (§5) both draw from it.

**Claims.** `token.Claims` gains `Callout` (the callout's `RequestID`), `Major`,
`Minor`, and `Outer` — the `(callout, major, minor)` of every enclosing callout,
for a callout made from inside a callback. A pass is minted per try by the pnode
that makes the hand-off (every pnode holds the cluster secret), with `NodeID` =
the owner's id and `ExpiresAt = now + answer limit +
CYODA_CALLOUT_PASS_ALLOWANCE` (§9). `resolveTxToken`'s rule "a token already on
ctx wins" is removed, with `DispatchCalloutRequest.TxToken`, `WithTxToken` and
`TxTokenFromContext`. `Signer.Issue` changes signature; its two production
callers (`grpc/dispatch.go:67`, `cluster_dispatcher.go:213`) and the ten test
files that call it change with it.

**The arbiter** is a leaf package, `internal/fence`. It cannot live in
`internal/callout`: `internal/grpc` imports `domain/entity`, which imports
`domain/workflow`, and `internal/grpc` imports `domain/txjoin`; a package that
imports `internal/grpc` for `RunLocal` cannot be imported by any of the places
that enforce the fence. `internal/fence` imports only `internal/txgate` and
`internal/common`.

```go
// Pair names one callout at one fencing number.
type Pair struct {
    Callout      string
    Major, Minor uint32
}

// Begin registers a callout on transaction txID, under the enclosing callouts
// named by outer, and returns the context the callout runs under and the func
// that ends it. The context is cancelled, with ErrSuperseded as its cause, when
// one of the outer pairs stops being current. The Coordinator defers end, so a
// callout is ended on every exit path, a panic included.
func (f *Fence) Begin(ctx context.Context, calloutID, txID string, outer []Pair) (context.Context, func())

// Advance raises the callout's number to (major, 0), which shuts out every
// pass issued under a lower one, and then waits until no joined write is in
// progress on the transaction (see "The wait"). It is called before each local
// try and before each hand-over.
func (f *Fence) Advance(calloutID string, major uint32)

// Admit is the check on entry. Under one lock it verifies that every pair the
// pass names is current, absorbs a higher minor, and returns a context that
// carries the pairs. It cancels no callback's context; absorbing a higher minor
// does release the Coordinators of callouts begun under the lower one.
func (f *Fence) Admit(ctx context.Context, pairs []Pair) (context.Context, error)

// Check reports CALLOUT_SUPERSEDED if ctx carries pairs (it was admitted) and
// one of them is no longer current. A context that was never admitted — the
// owner's own chain, an ordinary request — always passes.
func Check(ctx context.Context) error

// Pairs returns the pairs ctx was admitted under: the outer of a callout begun
// from inside this callback.
func Pairs(ctx context.Context) []Pair
```

A pair is current when its callout is registered, its `major` equals the
registered one, and its `minor` is not lower than the highest seen under that
`major`. A higher `minor` is **absorbed**: it becomes the current one, and
passes carrying a lower one are refused from then on. That is how the owner
learns that another pnode moved on to its next try, with no message between
them: from the first callback that carries the new number. Two rules make this
sound. The highest `minor` seen is reset to zero by `Advance`, so the first
callback of a *second* hand-over (`minor = 1`) is not measured against the last
try of the first. And a higher `minor` is absorbed only after *every* pair on
the pass has been verified, so a pass that is refused for an enclosing callout
changes nothing. The `minor` is covered by the pass's HMAC like every other
claim. Until a callback with the higher `minor` arrives, or the callout ends,
the earlier cnode of a hand-over is still admitted — so the `idempotent`
declaration keeps the words "possibly at the same time on two cnodes".

`end` unregisters the callout: all its passes are refused from then on. Nothing
is kept per transaction or per request, and nothing depends on the transaction
id staying the same across an operation, which it does not under
`COMMIT_BEFORE_DISPATCH`; a callout's own `txID` — the one its passes carry —
is fixed for its life.

**The fence never cancels a callback's context.** A callback runs on the
transaction of the operation it belongs to — on PostgreSQL, on the owner's own
connection (`plugins/postgres/store_factory.go:135-148`). Cancelling a context
in the middle of a statement makes the driver close that connection
(`transaction_manager.go:138-141` records the same hazard for rollback), and the
owner's next statement, or its commit, then fails: a successful failover would
turn into a failed operation because the replaced cnode happened to be reading.
An in-transaction write on the memory backend never consults the context
(`plugins/memory/entity_store.go:229-277`), and on SQLite it is a buffer write
whose one context-bound step is a read that is skipped once the entity is in the
buffer (`plugins/sqlite/entity_store.go:267-327`), so cancellation would stop
nothing there either (this settles **V-4**). The fence therefore works by **checks** and
by **the wait**, which hold on every backend alike, and the only context it ever
cancels is the one `Begin` returns — a Coordinator's own, under which no
statement of the shared transaction runs.

**Where it is enforced.**

1. *On entry.* `txjoin.JoinFromToken` is the one function every callback passes
   on the owner — both doors, and a request that arrived at another pnode and was
   proxied (`httpmw/txjoin_mw.go:33`; `grpc/txroute_interceptor.go:147, 188`; the
   proxy forwards before auth or join run, `proxy/http.go:46-80`). Order: verify
   the pass → `txMgr.Join`, which checks the tenant → `Fence.Admit`
   (`JoinFromToken` gains the fence as a parameter; its three callers change
   with it). The tenant
   check comes first so that a stolen pass tells another tenant nothing about
   which callouts exist. The request then runs under the context `Admit`
   returned. A refusal is `CALLOUT_SUPERSEDED` (§8). This covers every joined
   operation, reads and searches included. Because `Join` comes first, a
   callback that arrives after the *transaction* has ended is answered
   `TRANSACTION_NOT_FOUND`, as today; `CALLOUT_SUPERSEDED` is the answer while
   the transaction is still open.
2. *Every time a joined chain takes the transaction's write lock.* That is
   `acquireJoinedGate` (`entity/handler.go:121-125`), which every joined entity
   write calls (nine sites in `entity/service.go`), **and** the re-acquisition
   after a callout of the callback's own (`txgate`'s `resume`, reached from
   `engine_processors.go:204, 233, 249`, `engine.go:1014` and `arm.go:239`).
   Once the lock is held, `fence.Check` runs. This is the check that gives the
   right to write: it is made *under* the lock, and the owner's wait (below)
   takes the same lock. `acquireJoinedGate` gains an error return. `resume`
   stays a plain func; each of the five sites calls `fence.Check` right after
   it — `engine.go:1014`, which today has only a deferred `resume`, gains the
   explicit call the other four have.

   **A refused chain performs no store operation of any kind** — no write, no
   read, no audit row. The engine's error path records an audit event
   (`engine.go:832, 955`), which on PostgreSQL is an `INSERT` on the
   operation's connection; it is skipped when the error is the supersede error.
   In `executeAsyncNewTx` the check comes immediately after `resume`
   (`engine_processors.go:252`), *before* the savepoint is looked at, and on
   refusal the savepoint is neither undone nor released: by then the
   replacement cnode may have written, and undoing a savepoint restores the
   whole buffer on memory and SQLite (`plugins/memory/txmanager.go:1096-1100`)
   and everything since on PostgreSQL. An abandoned savepoint is harmless on
   every backend (an id-keyed entry on memory and SQLite; PostgreSQL's stack
   copes, `plugins/postgres/txstate.go:214-239, 272-287`).
3. *In the engine, after each processor returns* and before the mode-specific
   handling of its result. It is needed for `ASYNC_NEW_TX`, whose "log and
   continue" would swallow the refusal from point 2 and carry a superseded
   chain on to the next processor — writing after the owner's wait has already
   passed — and past the last processor to the handler's final save
   (`engine_processors.go:137-145, 186`). In the other modes the refusal is
   already fatal. A check at the top of the loop alone would never see the last
   processor.
4. *A callback waiting on a callout of its own* is released: the inner
   `Coordinator` runs under the context `Begin` returned, which is cancelled
   with cause `ErrSuperseded` when an enclosing pair stops being current —
   through `Advance`, through `end`, or through a higher `minor` absorbed by
   `Admit`. It returns `CALLOUT_SUPERSEDED` and ends the inner callout, which
   shuts out the inner cnode in turn. The pairs travel as a context *value*, so
   an inner callout is begun under them, and released with them, even where the
   engine has detached the context from cancellation under
   `COMMIT_BEFORE_DISPATCH` (`engine_processors.go:361, 376, 503`).

There is deliberately no check before a joined operation's final write. A chain
that reaches it has held the lock since its last check, so the owner is still
waiting for it, and its write lands before anything the owner does next. It is
answered 200 because what it wrote is in the transaction — for a joined
collection, all of it rather than the items up to the moment of the check. The
case is a cnode that answers before its own callback has finished.

**The wait.** After shutting out the earlier pass — in `Advance`, and in `end` —
the fence acquires the transaction's write lock once and releases it, outside
its own lock. A joined write either made its check under the write lock *before*
the number rose, in which case it still holds the lock and the owner waits for
it to finish; or it takes the lock afterwards, and its check refuses it. There
is no third case, and no dependence on which waiter the mutex favours
(`Registry.Acquire` counts a waiter before it blocks, `txgate.go:34-43`, so a
lock with waiters is never discarded). So:

- when the owner gives the work to the next cnode, no locked write of the
  earlier cnode is in progress on the owner and none can start;
- when a callout has ended — answered, failed or abandoned — the same holds
  before the engine does anything else: the savepoint of a failed `ASYNC_NEW_TX`
  processor is undone *after* that processor's last write, never before it;
- between callouts no joined chain holds the write lock.

The wait costs the joined writes that hold or are queued for the lock ahead of
it — statements bounded by the database. It comes on top of the callout's
deadline (§5). A callback gives the lock up for the length of any callout of its
own, so the wait is not a wait on a cnode — with one exception, a
`COMMIT_BEFORE_DISPATCH` processor reached inside a callback, whose dispatch
keeps the lock (`engine_processors.go:333, 365`); that is part of the defect
filed separately (below), and when the chain holding the lock is the superseded
one, point 4 has already released it. The wait cannot deadlock: the chain that
runs a Coordinator holds no write lock (the owner never does during
`engine.Execute`, `entity/service.go:356-361`; a callback has given it up for
the callout, and an inner `end` runs before its `resume`), a holder of the lock
waits on nothing but the database, and the fence never waits under its own
mutex.

The owner's own chain carries no pairs and is never subject to any check.

**One connection, several users (PostgreSQL).** A joined request runs on the
operation's own `pgx.Tx` (`plugins/postgres/store_factory.go:135-148`), and the
write lock covers entity writes only: a joined read, the model load and model
extension of a joined create (`entity/service.go:206, 246, 1908`), and every
non-entity handler behind the join middleware (`app/app.go:771`) issue their
statements without it, while the owner — which takes no lock during
`engine.Execute` — issues its own. pgx refuses a second concurrent user with
`conn busy` and guards its status field with nothing. This exists today: two
callbacks of one cnode are enough. Failover widens it, because the owner now
carries on productively while a cnode it gave up on may still be reading — the
very outcome this section avoids by not cancelling statements. Two changes close
it:

- **The postgres plugin serialises the statements of one transaction.** The
  querier it hands out for a transaction (`resolveRaw`) and the transaction
  manager's own statements on it (commit, rollback, savepoints) take one mutex
  per transaction; `Query` holds it until its rows are closed, `QueryRow` until
  `Scan`. No code can be holding rows open while issuing a second statement on
  the same transaction today — that is `conn busy`. **V-5**: no rows of a
  transaction-bound query are held across a network write to a client (a
  slow reader would otherwise hold the owner's next statement); the plan reads
  every `Query` call site that can run joined and states the answer. The
  memory and SQLite transaction buffers already carry their own locks; the
  commercial backend is asked the same question in its own repository.
- **A savepoint that cannot be created, undone or released fails the
  operation.** Today a failed `RollbackToSavepoint` is a warning
  (`engine_processors.go:254-258`), which commits the writes of a processor
  that failed, and a failed `Savepoint` counts as the `ASYNC_NEW_TX` processor's
  own non-fatal failure (`:138-145`), which silently skips a processor. Neither
  is a processor failure; both are the transaction being unusable
  (`correctness-over-availability.md`).

**What is not stopped, and why that is acceptable.**

- For tries made by another pnode, the earlier cnode stays admitted until the
  first callback of the later one arrives (above). Its writes are repeats of an
  idempotent processor's own writes, and the wait at the end of the callout
  still puts all of them before anything the engine does next.
- A joined *read* in progress when its cnode is replaced runs to completion, and
  its result goes to a cnode whose next callback is refused. Reads take no write
  lock; on PostgreSQL the owner's next statement queues behind it.
- A joined create loads and, where the model allows it, extends the model before
  it takes the write lock (`entity/service.go:246, 1908`), so a create admitted
  just before the number rises can issue that statement after the wait. The
  extension is additive and belongs to the transaction; a superseded create can
  leave one behind in an operation that commits.

**Not part of this design, by ruling.** An EdgeMessage saved by a superseded
callback: the application must make that save idempotent and attach the id to
the owning entity, so an orphan costs disk space only. Changing a model or a
workflow from inside a processor is not supported, and the processor help says
so. A workflow that runs inside a callback and contains a
`COMMIT_BEFORE_DISPATCH` processor commits the *outer* operation's transaction
(`engine_processors.go:311, 349, 481-486`; the service's guard runs only
afterwards) — a defect of its own, filed separately.

Callouts with no transaction (`COMMIT_BEFORE_DISPATCH` with
`startNewTxOnDispatch: false`) carry no pass, as today. They are still begun and
ended, with an empty `txID`, so that a callout made from inside a callback is
released with the callback; the wait is then a no-op.

## 8. What the client sees

### 8.1 By processor mode

The brief's "the operation fails, the transaction is rolled back, cyoda's own
state is clean for a re-run" is true only where this table says so.

| Mode | Another cnode after `NoAnswer` | Callout fails → | cyoda state after the failure |
|---|---|---|---|
| `SYNC`, `ASYNC_SAME_TX` | if `idempotent` | operation fails; error per §8.2 | rolled back; clean for a re-run, **unless** an earlier `COMMIT_BEFORE_DISPATCH` processor in the same cascade already committed |
| `ASYNC_NEW_TX` | if `idempotent` | **operation continues**; savepoint undone; a WARN is logged; nothing reaches the client, not even `MemberFailed`'s verdict. A savepoint that cannot be created, undone or released is not a processor failure: it **fails the operation** (§7) | the rest of the transaction commits; the failed cnode's pass is closed, and its write in progress lands before the savepoint is undone (§7) |
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
| Every try used, more than one attempt | 503 | **`CALLOUT_FAILED`** (new) | yes | `the callout could not be completed, got N failures: [member<id>: cause], [member<id>: cause (2 times)]` — Cloud's shape (R§5): N counts before collapsing; identical entries collapse. The same code and shape when the patience or the deadline ran out with attempts on record and tries still left |
| Every try used, exactly one attempt recorded | 503 | that attempt's own code | yes | not wrapped |
| `MemberFailed`, verdict true | 400 | `WORKFLOW_FAILED` | **yes** | `processor <name> failed: <cnode message>`; for a criterion `failed to evaluate transition criterion: <cnode message>` (or `…workflow criterion for "<wf>"…`); for a function the arming wrap. Today's inner `processor dispatch failed:` segment goes |
| `MemberFailed`, verdict false or absent | 400 | `WORKFLOW_FAILED` | no | same |
| `Terminal` | as today (500 ticketed for auth-context; 400 `WORKFLOW_FAILED` otherwise) | | no | as today |
| A hand-over's answer was lost — no reply, a broken connection, an answer that does not authenticate — and the callout is not repeat-safe, or it was the only attempt | 503 | `DISPATCH_FORWARD_FAILED` (kept) | yes | the sanitised message it has today; recorded as an attempt with member `-` |
| No peer could be connected to, and no local cnode | 503 | `NO_COMPUTE_MEMBER_FOR_TAG` | yes | as today |
| Callback from a cnode that was replaced, or whose callout has ended, while the transaction is still open (refused on entry, at the write lock, or between processors) | 410 | **`CALLOUT_SUPERSEDED`** (new) | no | `this compute node was replaced, or its callout has ended` |
| The same callback after the transaction has ended | 404 | `TRANSACTION_NOT_FOUND` | no | as today: the transaction is looked up, and its tenant checked, before the fence is consulted |
| Callback bearing a pass with no callout and number | 401 | `UNAUTHORIZED` | no | `invalid transaction token`, as any malformed pass today |
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
idempotent processor is enough. **Not changed here — §17, #598.**

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
  sender's metadata, or is a later `seq` of that epoch **and** later than the
  `seq` already held for it (the list can arrive before the metadata does; two
  lists can arrive out of order). Anything else is dropped. A held list that is
  a later `seq` of the announced epoch counts as current — the metadata is on
  its way — so it triggers no fetch. After a drop the pnode fetches again at once
  only when the dropped list is of the *announced* epoch; a list of another
  epoch (a peer that restarted, whose new metadata has not arrived yet) waits
  for the metadata event that must follow, or for the scan. Read without these
  two rules, "fetch whenever held differs from announced" is a tight loop in
  both cases. The
  sender takes version and tags in one locked step and writes its metadata under
  the same lock, so one version never names two different lists.
- *Catch up.* An `EventDelegate` is registered. On `NotifyJoin` and
  `NotifyUpdate`, a pnode that holds no list for the node, or one whose version
  differs from the announced one, sends `cluster.tags.request`; the node answers
  with its list. One path covers a lost message, a late joiner, a restarted
  pnode and a healed partition. A request that gets no list is repeated on the
  next metadata event for that node. As a floor, a timer shorter than the
  patience scans every member and fetches wherever held and announced differ —
  a full scan, not a list of outstanding requests, because an event dropped from
  a full queue leaves no request behind. The worker's queue never blocks a
  callback: a full queue drops the event and the scan makes it good. Twenty pnodes starting together exchange about 380 small
  messages; no throttling is needed.
- *Leave.* `NotifyLeave` drops the node's list.
- **Nothing calls into memberlist from inside one of its callbacks.**
  `NotifyJoin`, `NotifyUpdate` and `NotifyLeave` run under memberlist's node lock
  (`state.go:941-943`): a `SendReliable` there stalls all membership processing
  for up to the TCP timeout, and `Members()` or `UpdateNode` deadlocks.
  `NotifyMsg` runs on the receive goroutine. All four only copy what they were
  given — `NotifyMsg`'s buffer is reused by the library — and enqueue it for one
  worker goroutine, which does the sending, fetching and storing. The three
  membership callbacks also copy the member into a **directory the registry
  owns**, and `List`, `Lookup` and the worker read that directory; nothing in the
  package calls `Members()`. `Members()` returns pointers to nodes whose `Meta`
  memberlist rewrites under its own lock (`state.go:1131`), so reading them is a
  data race — hidden today only because `UpdateTags` blocks in `UpdateNode(0)`
  until the broadcast is out, and exposed the moment it stops blocking. `NotifyJoin`
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
  and for how long; `CALLOUT_SUPERSEDED` refusals; failed list sends and outstanding
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
  stop extra compute clients, each given a **tenant**, tags, a behaviour and, in
  the multi-pnode fixture, the pnode to attach to. A tag alone does not isolate
  a scenario: a callout whose `calculationNodesTags` is empty matches every
  cnode of its tenant, and a stopped client is evicted asynchronously. Each
  failover scenario therefore uses a tenant and a tag of its own and
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
| Every try used → 503 `CALLOUT_FAILED`, message shape | ✓ | ✓ | ✓ | ✓ | |
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
| Late callback after the callout ended, transaction still open (a later processor of the same transition is in progress) → 410 `CALLOUT_SUPERSEDED`, HTTP and gRPC doors, write and read | ✓ | ✓ | ✓ | | |
| Late callback after the transaction ended → 404 `TRANSACTION_NOT_FOUND`, as today | ✓ | ✓ | ✓ | | |
| Late callback in `ASYNC_NEW_TX` after failure → 410 | | ✓ | | | |
| Owner gives the work to a second cnode of its own → the first cnode's callback is refused at once, while the callout is still in progress | ✓ | ✓ | ✓ | | |
| A write queued for the transaction's lock when its cnode is replaced → refused on taking the lock, nothing written | ✓ | ✓ | | | |
| A joined write in progress when its cnode is replaced → the next cnode is not given the work, and the engine does not carry on, until that write has finished (the wait) | ✓ | ✓ | | | |
| `ASYNC_NEW_TX`: a failed processor's write in progress lands before its savepoint is undone — it is not in the committed result, on any backend | ✓ | ✓ | | w⁴ | |
| A callback waiting on a callout of its own when its cnode is replaced → released; the inner callout ends; refused on re-taking the lock; nothing further is written — no audit row either — in every processor mode incl. `ASYNC_NEW_TX` as the *last* processor; the callback is answered 410, not 200 | ✓ | ✓ | | | |
| The same in `ASYNC_NEW_TX`: the superseded chain neither undoes nor releases its savepoint; what the replacement cnode wrote meanwhile is kept | ✓ | | | | |
| A callback past its last check when its callout ends → its write lands, it is answered 200, and the owner proceeds only afterwards; a joined collection lands whole | ✓ | ✓ | | | |
| A joined read in progress when its cnode is replaced → completes; no statement is interrupted; the owner's operation succeeds (PostgreSQL) | | ✓ | | | |
| Two goroutines on one PostgreSQL transaction (a joined read against the owner's statements, commit and savepoints) are serialised: no `conn busy`, clean under `-race` | ✓ | | | | |
| `ASYNC_NEW_TX`: a savepoint that cannot be created, undone or released → the operation fails, nothing is committed | ✓ | | | | |
| Tries made by another pnode: a higher `minor` is absorbed from the first callback that carries it, and lower ones are refused from then on; a second hand-over's `minor = 1` is admitted | ✓ | | | | ✓ |
| A pass refused for an enclosing pair absorbs nothing | ✓ | | | | |
| A pass naming an enclosing callout that is no longer current → refused | ✓ | ✓ | | | |
| A Coordinator released by the fence reports `CALLOUT_SUPERSEDED`, a client that went away as today — `context.Cause` tells them apart | ✓ | | | | |
| The wait cannot deadlock: a callout made from inside a callback, replaced while its own inner cnode's callback holds the lock | ✓ | ✓ | | | |
| Callout ended by a panic → its passes are refused | ✓ | | | | |
| Pass without callout and number → 401; expired pass → 410 `TRANSACTION_EXPIRED` | ✓ | ✓ | | | |
| Stolen pass presented by another tenant → 403 before any fencing answer is given | ✓ | ✓ | | | |
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
| Pass minted by another pnode joins the owner's transaction; refused once the callout ended | ✓ | | | | ✓ |
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
⁴ The scenario is an interleaving of a callback against the owner, and
interleavings stay out of the shared parity suite. The unit layer runs it on the
memory backend, whose in-transaction writes ignore the context — the backend on
which a cancelled context would have stopped nothing — and the e2e layer on
PostgreSQL.

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
  `errors/CALLOUT_FAILED.md`, `errors/CALLOUT_SUPERSEDED.md` (new);
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
  the late-callback hole, concurrent statements on one PostgreSQL transaction,
  a savepoint failure no longer passing as a processor failure), and the retired
  setting semantics.
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

Whether the commercial backend tolerates two users of one transaction at once (a
joined read against the owner's statements, §7) is asked in its own repository.

Seen by the specification's reviewer and **not verified**, to be checked and
filed on its own: a joined callback whose workflow contains a
`COMMIT_BEFORE_DISPATCH` processor appears able to commit the owner's
transaction — the engine has no guard, and the service's guard runs only
afterwards (`entity/service.go:336`).

## 17. Scheduled transitions — not changed here

A scheduled run that takes longer than `CYODA_SCHEDULER_REDISPATCH_BACKOFF`
(30 s) is started a second time while it is still running, because that value
is a throttle and nothing owns a run. Only one of the two can commit, so cyoda's
data stays correct, but the transition's processors run twice. One timed-out try
on an idempotent processor, at the default answer limit, is already 30 s, so
this change makes that overlap ordinary where it was rare.

It is not patched here. The execution of scheduled transitions is redesigned as
its own piece of work: **#598** (milestone v0.9.0). The order in which #254 and
#598 land is the product owner's; this specification changes nothing in
`internal/scheduler`.
