# Callout failover and fencing a replaced compute member — Cloud twin-alignment spec

This document is the contract Cyoda Cloud implements to stay aligned with
cyoda-go's handling of a callout — a processor, criterion or function request to
a compute member — that cannot be delivered, is not answered, or is answered by
a member that has been given up on. cyoda-go is the authoritative
implementation.

Words used here. A **node** is one platform process. A **compute member** is a
customer's compute program attached to one node. A **callout** is one processor,
criterion or function request. A **try** is one attempt to hand a callout's work
to one compute member. A **hand-off** is the moment the work is on the member's
connection: before it, the work provably never left the node; after it, the work
may have reached the member. A **callback** is an API request a compute member
makes while it works, carrying the transaction token it was given, and carried
out inside that transaction.

Why cyoda-go cannot simply do what Cloud does: in Cloud a compute member's
callbacks are transactions of their own. In cyoda-go they join the operation's
transaction, so a second member given the same work can commit a processor's
writes twice, and a member that was given up on can still write.

Sections 1 to 5 are the visible contract and have a direct counterpart in
Cloud. Sections 6 and 7 follow from the joined transaction; section 8 says what
that leaves Cloud to decide.

## 1. One dividing line: was there a hand-off?

| What happened to a try | Criterion, function, or processor with `idempotent: true` | Processor, `idempotent` false (the default) |
|---|---|---|
| No hand-off: the member had gone, or its connection did not take the work | try another member | try another member |
| Hand-off, then no answer within the answer limit, or the connection dropped | try another member | **stop**; the operation fails |
| The member answered `success: false` | **stop**, whatever `error.retryable` says | **stop** |
| A failure that would be identical on any member — the request cannot be built, the principal cannot be attached, the member's answer cannot be read | stop | stop |

"The member's answer cannot be read" includes a criteria answer that reports
`success: true` without a `matches` verdict: a missing verdict is not read as
`false`, because that would be an answer the platform invented to a criterion
that decides a transition. It is the same conclusion the platform reaches for a
verdict relayed by another node, and it applies to a member of the node holding
the transaction and to a member of a node that received the callout alike.

**A difference that is gone — an omitted `success` means success.** The
published schema (`docs/cyoda/schema/common/BaseEvent.json`) declares `success`
optional with the default `true`. Cloud applied that default; cyoda-go read an
absent key as `false` and turned the answer into a failed callout, so one
payload meant opposite things on the two tiers. cyoda-go now applies the
default: a member that omits the key has answered success, and a member
reporting a failure sends `success: false` explicitly. The same bytes now mean
the same thing on both tiers, so this asks nothing of Cloud and appears in
neither the table above nor §8.

The default fills in a flag, never a verdict: a criteria answer that omits
`success` still owes its `matches`, and the criteria response schema states
that as "required on any response but an explicit `success: false` one".

**Departure 1 — `idempotent`, default `false`.** A boolean on a processor's
`config` (workflow schema 1.5). It is the author's declaration that the
processor may be run more than once for one callout — possibly only partly,
possibly at the same time on two compute members — with the same outcome as
running it once, in the platform and in every system the processor touches.
Cloud behaves as if every processor carried it: its retry loop moves on after a
timeout or a dropped connection without asking. Criteria and functions are
repeat-safe by rule: they must have no effects.

**Departure 2 — `error.retryable: true` does not fail over.** Cloud gives the
work to another member — only a `success: false` answer whose `retryable` is
absent or false stops its loop. cyoda-go stops on any `success: false` and
passes the verdict to the client: another member inside the same transaction
would be likely to fail the same way, while the client running the whole
operation again can succeed. The published schema's `retryable` description
(`docs/cyoda/schema/common/BaseEvent.json`) now states cyoda-go's own
behaviour rather than Cloud's — see `cyoda help grpc`.

## 2. What the client sees

The surfaces are the HTTP entity endpoints that run workflows — create,
create-collection, update, transition, loopback — and the gRPC `EntityManage` /
`EntityManageCollection` envelopes. In an envelope the class is `Error.Code`
(`CLIENT_ERROR` for an operational failure, `SERVER_ERROR` for a ticketed one),
the code below is the prefix of `Error.Message`, and `Error.Retryable` is set
only when it is true.

| Situation | Status | Code | Retryable |
|---|---|---|---|
| No compute member appeared within the wait (§4) | 503 | `NO_COMPUTE_MEMBER_FOR_TAG` | yes |
| Exactly one try on record, and nothing more can be done | 503 | that try's own code: `DISPATCH_TIMEOUT`, `COMPUTE_MEMBER_DISCONNECTED`, `DISPATCH_FORWARD_FAILED` | yes |
| More than one try on record, and nothing more can be done | 503 | `CALLOUT_FAILED` | yes |
| The member answered `success: false`, `error.retryable: true` | 400 | `WORKFLOW_FAILED` | **yes** |
| The member answered `success: false`, `error.retryable` false or absent | 400 | `WORKFLOW_FAILED` | no |
| A failure identical on any member: the workflow's criterion cannot be parsed, the member's answer cannot be read, the stored answer limit is above the bound | 400 | `WORKFLOW_FAILED` | no |
| A failure identical on any member, in the platform's own work: the request cannot be built, the principal cannot be attached, an `ASYNC_NEW_TX` savepoint cannot be created, undone or released | 500 | generic message and a ticket | no |
| The client's own `transactionTimeoutMillis` ended the operation | 408 | `TRANSACTION_TIMEOUT` | yes |
| Import: `retryPolicy` on a processor, criterion function or schedule function outside `NONE` / `FIXED` / absent | 400 | `VALIDATION_FAILED` | no |
| Import: `responseTimeoutMs` negative, or above the server's bound (§5) | 400 | `VALIDATION_FAILED` | no |

A processor whose execution mode is `ASYNC_NEW_TX` is the one exception to the
table: its callout's own failure reaches nobody — not even the member's verdict.
The savepoint is undone, a warning is logged, the operation carries on, and the
rest of the transaction commits. Two failures around it are not swallowed: a
savepoint that cannot be created, undone or released fails the operation, as the
500 row above says; and when the processor is running inside a compute member's
callback whose token has stopped being current, that refusal ends the workflow
rather than being logged and passed over — see §6.

**Departure 3 — the member's message and verdict reach the client**, from
whichever node the member was attached to. `WORKFLOW_FAILED` reads
`processor <name> failed: <the member's message>`; for a criterion,
`failed to evaluate transition criterion: <the member's message>` or
`failed to evaluate workflow criterion for "<workflow>": …`. The member's own
`retryable` is what sets the response's retryable flag. In Cloud the member's
code and message reach the processor's fail reason and its audit trace; once more
than one try has failed the *code* becomes `-`, while the members' own *texts*
stay embedded in the aggregate message; and no `retryable` verdict reaches the
caller at all.

Each piece of a member's own free text is cut to 512 characters where it becomes
client text: the message of a `success: false` answer, and each of its warnings.
A cut piece ends with `…`, so a reader can tell a shortened message from a short
one. A member may contribute at most 32 warnings to one try's response; past
that, one warning says the rest were left out.

`CALLOUT_FAILED` uses the shape of Cloud's exhaustion message:

```
the callout could not be completed, got 3 failures: [member<ID>: cause], [member<->: cause (2 times)]
```

The angle brackets are literal and enclose the member id, which is never
shortened; `member<->` is a hand-over whose answer was lost, where no member is
known. The count is taken
before identical entries are collapsed, an entry that occurs more than once is
written once with `(k times)`, and entries keep the order in which they first
occurred. A single failure is not wrapped — the client gets that try's own code.
Member ids are the tenant's own connections and are already known to the member
from its greet; node ids and node addresses never appear.

`retryable: true` on a try that got no answer speaks for the platform's state
only, and only in the execution modes in which the failed operation is rolled
back whole (`SYNC`, `ASYNC_SAME_TX`). Under `COMMIT_BEFORE_DISPATCH` the part
of the cascade before the callout stays committed, so a re-run is at-least-once.
What the processor did outside the platform is the application's to reconcile.

## 3. Tries

`retryPolicy: NONE` selects one try. `FIXED`, or absent, selects one plus
`CYODA_RETRY_FIXED_NUM_RETRIES` (default 3). A member is never tried twice
within one run over a node's members, and the matching members are looked up
afresh before every try, so one that attaches during the run is seen. Every try
carries the same `requestId` and the same transaction id, as in Cloud; a
de-duplicating compute member can therefore recognise a repeat.

**Departure 4 — no pause, and no delay setting.** Cloud sleeps its
`delayMs` (500 ms by default) before every retry. cyoda-go moves straight to the
next member: a pause helps only when about to look at the same thing again. The
workflow carries neither the number of tries nor a delay — `retryPolicy` selects
between "one try" and "the server's number".

**Departure 5 — `retryPolicy` on a scheduled transition's function.** Cloud's
parameter set carries `retryPolicy` on a processor and on a criterion. cyoda-go
accepts it, validates it at import and acts on it for all three callout kinds,
`schedule.function` included; a value outside `NONE` / `FIXED` / absent is
refused at import rather than ignored.

**Departure 6 — the number of tries is the normal number, not a hard limit; the
time is the hard limit.** The node holding the transaction tries its own
members, then offers the callout to one other node after another — every node
that advertises the tag, each asked at most once before the callout waits again
(§4) — handing over the tries that are left. A node that receives the callout
tries its own members and never hands it on. A node that cannot be reached, and a
node that answers that it has no matching member, costs no try. If a node's
answer is lost, one try is counted though it may have made more. The time a
callout may take is
fixed when it starts — `tries × answer limit + the wait of §4 +
CYODA_CALLOUT_HANDOVER_ALLOWANCE`, 155 s at the defaults — and no try starts
after it; one in progress is cut off at it and counts as "no answer".

## 4. Waiting for a member to exist

**Departure 7 — waiting is separate from trying.** Cloud counts "no member
available" as a failed try, spending the retry budget on re-querying. In
cyoda-go a callout with nothing to try waits, up to
`CYODA_DISPATCH_WAIT_TIMEOUT` (default 5 s) in total, and is woken by a member
attaching or by another node announcing one — a signal, never a poll. The wait
costs no try, applies with `retryPolicy: NONE`, and applies on a single node. A
callout that has already made tries may still wait: a member that dropped and is
coming back is the case the wait exists for, and every node may be offered the
work again once something has changed. A wait that the allowance ends offers the
callout to nobody further; nothing changed, so the same members would only be
tried again. `0` disables waiting.

A wait that ends with no try ever made is `NO_COMPUTE_MEMBER_FOR_TAG`. Tries on
record beat it: once any try has been made, the client is told about the tries.

## 5. The answer limit has an upper bound

**Departure 8.** `responseTimeoutMs` on a processor, on a criterion function or
on a schedule function must be `0 ≤ value ≤
CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS` (default 60000); import answers
`400 VALIDATION_FAILED` otherwise, naming the bound. Absent or `0` means
`CYODA_CALLOUT_RESPONSE_TIMEOUT_MS` (default 30000). A stored value above a
bound that a deployment set lower is not clamped: the callout fails, naming the
setting, because a substituted limit is a wrong-but-available answer. Because
the bound is a server setting, a workflow exported from one deployment can be
refused by another.

The bound is what keeps a callout inside the one ceiling a transaction has:
tries multiply the answer limit, and §3's hard limit on time is computed from
it — 275 s with the answer limit at its upper bound and everything else at its
default.

## 6. A compute member that was replaced is fenced

Giving up on a member does not stop it, and its callbacks would otherwise be
accepted for as long as the transaction is open — after its replacement has
answered, and after later processors of the same transition have run.

**Departure 9.**

- Each callout is numbered by a **pair**: a **round**, and a **try within the
  round**. The owner opens the next round before every try it makes itself and
  before every hand-over to another node, and it waits for the transaction's lock
  each time it does (below). A node that received a hand-over opens no round: it
  numbers its own tries within the round it was given — 1, then 2, and so on —
  and touches nothing on the owner.
- The transaction token is minted per try. It names the owner node, the
  transaction, the callout, that try's pair, and the enclosing callouts when the
  callout was made from inside a callback. It lives for that try's answer limit
  plus `CYODA_CALLOUT_PASS_ALLOWANCE`.
- **The admission rule.** A token is admitted if and only if all three of these
  hold:
  1. the callout it names is still registered on the owner — the callout has not
     ended;
  2. its round **equals** the callout's current round. Equality, not "at least":
     the owner opens the round before it mints the token, so a round higher than
     the current one is never presented;
  3. its try within the round is **not below** the highest the owner has seen for
     that round.

  A token whose try within the round is *higher* than any the owner has seen is
  admitted and raises that floor — which is how the owner learns that a node
  running a hand-over has moved on to a further try. Anything else is refused.
- A refused callback is answered, in each case not retryable:
  - `410 CALLOUT_SUPERSEDED`, message `this compute node was replaced, or its
    callout has ended` — the token fails the admission rule while the
    transaction is still open.
  - `404 TRANSACTION_NOT_FOUND` — the transaction itself has ended.
  - `410 TRANSACTION_EXPIRED` — the token is past the life stated above. The try
    it was minted for cannot still be waiting for an answer, so a member that
    sees it stops: the work is not its to finish, and no fresh token is issued
    for the same try.
  - `403 FORBIDDEN` — the token is presented with the credentials of a tenant
    other than the one it was issued for.
  - `401 UNAUTHORIZED`, `invalid transaction token` — the token does not verify,
    or names no callout and pair, as any malformed token.
- The order of the checks is fixed: the token is verified (a malformed one is
  `401`, one past its life `410 TRANSACTION_EXPIRED`), then the transaction is
  joined — which checks the tenant (`403`) and whether the transaction is still
  active (`404`) — and only then is the admission rule applied
  (`410 CALLOUT_SUPERSEDED`). A stolen or forged token therefore tells another
  tenant nothing about which callouts exist.
- The rule is applied again under the transaction's lock: when the callback
  takes it, each time it takes it back after a callout of its own, and between
  the processors of a workflow it started. A refused callback performs no store
  operation of any kind — no write, no read, no audit record.
- When the owner opens a round, and when a callout ends, the platform takes the
  transaction's lock once before it proceeds: whatever the earlier member had in
  progress finishes first, and nothing of it can start afterwards.
- No database statement is interrupted — not by the fence, and not by the
  member's connection dropping. A callback that was already reading or writing
  when the work moved on is allowed to finish and is answered normally; the wait
  above is what makes that safe.
- "Too slow" is not "replaced": a timeout that fails the operation rolls the
  transaction back and opens no round.

A compute member that receives `CALLOUT_SUPERSEDED` must stop working on that
callout: nothing further it sends under that token is accepted, and an answer it
does send for the callout is discarded.

**Departure 10 — one transaction, one user at a time.** Every callback, a read
or a search as much as a write, holds its transaction's lock for the whole time
the platform works on it. The pass is verified before the request is read, the
request is read in full before the lock is taken and the response is sent after
it is released, so a member that stalls holds nothing, and a member that
disconnects once its callback holds the lock does not cancel it. A callback
still waiting for the lock is the one that its member can still call off: it has
touched nothing, so it is simply dropped. This extends
`nested-join-tx-serialisation.md`: the lock is still given up for the length of
any callout the callback itself makes.

What is not stopped, stated plainly: the owner's own next try shuts the member
before it out at once, because the owner opens the round before it mints. A try
made by a node running a hand-over does not: that node numbers its tries within
the round it was given and tells the owner nothing, so the owner learns of a
later try only from the first callback that carries it. Until then the member of
the earlier try is still admitted. Its writes are repeats of an idempotent
processor's own, and the wait at the end of the callout still puts them before
anything the workflow does next.

## 7. Joined-request wire behaviour Cloud must replicate

These follow from taking the transaction's lock for every joined request (reads
included), not only entity writes, and from reading each request whole before
the lock is taken:

- A refused gRPC **server-streaming** joined request
  (`EntityManageCollection`, `EntitySearchCollection`) carries the request id
  in its error envelope even for `TRANSACTION_NOT_FOUND` and `FORBIDDEN`, not
  only for `CALLOUT_SUPERSEDED` — the request message is received, and its id
  therefore known, before the token is judged. A refusal produced instead while
  resolving which node should serve the call — a malformed or expired token,
  before the message is read at all — carries no id.
- An over-size body on a joined HTTP request is refused with `413`, the same
  as an unjoined one: the body is read into memory, under the same size cap a
  handler would apply, before the lock is taken, so the join layer never
  accepts a body a handler would reject. A pass that fails verification —
  `401 UNAUTHORIZED`, `410 TRANSACTION_EXPIRED` — is refused before the body is
  read at all, so it costs no buffer.
- A held gRPC server-streaming response is not all-or-nothing: if the handler
  fails partway through a joined chunked collection, the frames already
  produced are sent before the handler's error is returned, exactly as an
  unjoined stream would deliver them.
- A joined answer is held in memory while the transaction's lock is held, under
  a ceiling of its own: `CYODA_CALLOUT_JOINED_RESPONSE_MAX_BYTES`, default
  10 MiB. An answer past it fails the request — `413
  JOINED_RESPONSE_TOO_LARGE`, naming the ceiling, on either door — and is never
  sent in part: a truncated answer would be a wrong one.

## 8. What Cloud has to decide

Cloud's callbacks are separate transactions, so §6 and §7 have no direct
counterpart there; §1 to §5 do. The visible contract to match is:

1. The `idempotent` field on a processor's config and its default of `false`,
   and the dividing line of §1 drawn by the hand-off rather than by the kind of
   failure.
2. Stopping on `success: false` whatever its `retryable` says, and passing the
   member's message and that verdict to the client.
3. The statuses, codes and retryable flags of §2, the `CALLOUT_FAILED` message
   shape, and the rule that a single failure is reported as itself.
4. No pause between one member and the next, and no delay setting.
5. Waiting for a member to exist as an allowance of its own that costs no try
   and applies whatever the `retryPolicy`.
6. The upper bound on `responseTimeoutMs`, refused at import rather than
   clamped.
7. `retryPolicy` accepted, validated and acted on for all three callout kinds.
8. A criteria answer with no `matches` treated as unreadable rather than as
   `false`.

The three wire behaviours of §7 are what a client written against either server
relies on identically, so they hold wherever a joined request exists. Where
Cloud adopts the joined-callback model, §6's fencing becomes its contract too:
the pair, the admission rule as stated, and who may raise which half of it; the
tenant checked before the rule is applied; every joined request, a read as much
as a write, serialising on its transaction; and the owner waiting for one in
progress before moving the callout on or committing past it.
