# AuthContext contract + follow-on-action attribution

## 1. AuthContext contract (pinned)

Every processor/criteria/function callout carries CloudEvent Auth Context
extension attributes naming two principals — the **attributed** principal
(who the work is for) and the **executor** (who does it). The node that
dispatches the callout computes the pair once, with the same rule that
attributes a write (`AttributionFor`):

- `authtype` / `authid` — the attributed principal's kind and id.
- `authexectype` / `authexecid` — the executor's kind and id.
- `authclaims` — comma-separated roles of the executor; absent when it has
  none.

Kinds are `user`, `service`, or `system`, the principals' **explicit
kinds**, never sniffed from roles. Both principals are **always present and
faithful**: a missing id or an unset or unrecognized kind fails the callout
dispatch rather than emit a normalized/absent value — a wrong-but-available
principal would violate correctness-over-availability.

| Callout made by | `authid` / `authtype` | `authexecid` / `authexectype` |
|---|---|---|
| an on-behalf-of request, and its cascades | the user / `user` | the on-behalf-of client / `service` |
| a client's own request | the client / `service` | the client / `service` |
| a processor write-back joined to a transaction, and its cascades | the transaction's origin | the compute client / `service` |
| a CBD-detached callback of a compute client | that client / `service` | that client / `service` |
| a scheduled fire | `ArmedBy` | `system` / `system` |
| a callout forwarded to another node | as computed on the dispatching node | as computed on the dispatching node |

**Wire break:** `authtype` previously emitted `user` / `service_account`,
inferred by sniffing `ROLE_M2M`. It now emits exactly one of `user` /
`service` / `system`, driven by principal kind. `service_account` is
retired. This is a deliberate, compute-node-facing contract change —
external compute nodes switching on the old string break and must update.

**Trust basis.** A compute node may rely on `authclaims` only if it
authenticates the cyoda server endpoint (TLS server verification); over an
unauthenticated channel the attributes are forgeable. Application
authorization built on `authclaims` must fail **closed** when claims are
absent or empty — including the `system` case, which never carries
meaningful claims. In cluster mode the transaction's origin stays on the
node that holds the transaction; the attributed and executor principals
computed from it, with the executor's roles, cross the mutually-authenticated
peer channel in a callout hand-over, and the receiving node attaches them as
received and never recomputes them. The principals a compute node sees are
therefore only as trustworthy as the cluster's own peer trust — the same
boundary that already governs cross-node dispatch.

**SDK helper.** `api/grpc/authctx` gives compute-node authors `Type`/`ID`
(the attributed principal), `ExecutorType`/`ExecutorID` (the executor) and
`Roles` readers, plus `Require(ce, role)`, a fail-closed role gate: it
returns `true` only when `authexectype` is `service` and the role is present
in `authclaims`, and `false` for everything else — a nil event, empty/absent
claims, a `system` or `user` executor, an absent or unrecognized
`authexectype`. The attributed principal plays no part in the gate.

## 2. Attributed/executor pair on change history

`GET /entity/{entityId}/changes` metadata now returns, per change:

```json
{
  "user": "userY",
  "attributedKind": "user",
  "executedBy": { "id": "svc-compute-1", "kind": "service" }
}
```

- `user` — the attributed principal's id. Stays required; unchanged for
  existing consumers.
- `attributedKind` — the attributed principal's kind (`user`/`service`/
  `system`).
- `executedBy` — the immediate authenticated principal that performed the
  write: `{id, kind}`. Diverges from the attributed principal on an
  on-behalf-of request (the user, executed by the on-behalf-of client), on a
  compute node's joined write-back and its cascades, and on scheduled fires.
- **Legacy rows** (written before this change) omit `attributedKind` and
  `executedBy` entirely (never emitted as JSON `null`); `user` renders as
  today.

The gRPC door carries the same pair. The CloudEvents schema
`common/EntityChangeMeta.json` (the `changeMeta` of every
`EntityChangesMetadataResponse` to an `EntityChangesMetadataGetRequest`) has
two optional properties:

- `attributedKind` — string, open value set (`user`/`service`/`system`).
- `executedBy` — object `{id, kind}`, both strings and both required when
  the object is present; `kind` is an open value set like `attributedKind`.

Each is present exactly when the HTTP read returns it, and absent otherwise.
`user` stays required and unchanged.

## 2a. Attributed/executor pair on edge messages

`POST /message/new/{subject}` no longer reads a caller-supplied sender
header. `GET /message/{messageId}` renders the same {attributed, executor}
pair as change history, on the message's `header`:

```json
{
  "userId": "alice",
  "attributedKind": "user",
  "executedBy": { "id": "OBOCLIENT0000001", "kind": "service" }
}
```

- `userId` — the attributed principal's id, resolved from `AttributionFor`
  at save time (never from a request header). Stays optional; omitted when
  empty, as before.
- `attributedKind` — the attributed principal's kind. Omitted when empty.
- `executedBy` — `{id, kind}` of the immediate authenticated principal that
  sent the message. Equals the attributed principal for a direct request;
  diverges for an on-behalf-of request (the on-behalf-of client). Omitted
  when the executor has no id.

## 3. Attribution semantics per follow-on kind

- **On-behalf-of request** (an OBO token: a user stated by an on-behalf-of
  client, see `obo-only-user-identity.md`) — attributed to the **user**,
  executed by the **on-behalf-of client**. The user is recorded, never
  authorized. A transaction's origin never applies to it: an OBO request
  joins only a transaction begun for its own user, and is refused (`403
  FORBIDDEN`) otherwise. A transaction it begins has the user as its origin,
  so its cascades, callouts and the timers it arms carry the user.
- **Joined cascade** (a processor's write joins the triggering
  transaction) — attributed to the **transaction's origin**: the principal
  authenticated at the causal chain's root `Begin`, propagated unchanged
  through every joined write, including a cross-node proxied join (origin
  lives on the owning node's transaction state; the join token carries no
  identity). Executor is the immediate writer (e.g. the compute node's own
  client).
- **Scheduled fire** — attributed to the **durable arming principal**
  (`ArmedBy`: the arming write's attributed principal, stored on the
  scheduled task), executed by a real `system`-kind platform principal —
  never a fake `"scheduler"` user. The fire's transaction begins with the
  claimed task's `ArmedBy` as its origin; if the task's life changed since
  the claim, the run ends before anything is written, so a fire never
  attributes against a stale value. `system` is a reserved user id, so no
  caller can be recorded as that principal (`user-id-rule.md`).
- **CBD-detached** (`COMMIT_BEFORE_DISPATCH` with `startNewTxOnDispatch:
  false`) — handed over to the application. The dispatch carries no
  transaction token, so the processor's callback writes are **ordinary
  independent requests**, not part of any platform-tracked chain. The
  identity those callbacks present governs attribution as usual (a client's
  own token → that client; an OBO token → that user, executed by its OBO
  client). The
  callout's AuthContext (§1) carries the causal principal as its attributed
  principal (`authid`/`authtype`) so the application can self-attribute if
  it chooses; the platform adds no carrier mechanism for this mode.

## 4. Cloud obligation

Emit `authtype`/`authid` (attributed), `authexectype`/`authexecid`
(executor) and `authclaims` (the executor's roles) per §1, computed once by
the dispatching node and forwarded unchanged (including the
`service_account` → `service` rename and the fail-loud behaviour on a
missing id or an unset kind); surface `attributedKind`/`executedBy` on
change-history reads per §2 — over HTTP and in the gRPC `EntityChangeMeta`,
whose two new schema properties Cloud's generated types must add — and on
edge messages per §2a; and implement the
attribution paths in §3 identically — on-behalf-of attribution and its join
rule, cascade origin propagation, durable scheduled-arming attribution, and
the CBD-detached handover boundary. Tracked with
`obo-only-user-identity.md`.
