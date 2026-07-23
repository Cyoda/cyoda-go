# AuthContext contract + follow-on-action attribution

## 1. AuthContext contract (pinned)

Every processor/criteria/function callout carries CloudEvent Auth Context
extension attributes:

- `authtype` — `user`, `service`, or `system`, driven by the originating
  principal's **explicit kind**, never sniffed from roles. `authtype` is
  **always present and faithful**: an unset or unrecognized kind fails the
  callout dispatch rather than emit a normalized/absent value — a
  wrong-but-available `authtype` would violate correctness-over-availability.
- `authid` — the originating principal's id.
- `authclaims` — comma-separated roles of the originating principal.

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
meaningful claims.

**SDK helper.** `api/grpc/authctx` gives compute-node authors `Type`/`ID`/
`Roles` readers plus `Require(ce, role)`, a fail-closed role gate: it
returns `false` for a nil event, empty/absent claims, or `authtype ==
system`, and `true` only when `authtype` is `user` or `service` and the
role is present in `authclaims`.

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
  write: `{id, kind}`. Diverges from the attributed principal only on
  cascades and scheduled fires.
- **Legacy rows** (written before this change) omit `attributedKind` and
  `executedBy` entirely (never emitted as JSON `null`); `user` renders as
  today.

## 3. Attribution semantics per follow-on kind

- **Joined cascade** (a processor's write joins the triggering
  transaction) — attributed to the **transaction's origin**: the principal
  authenticated at the causal chain's root `Begin`, propagated unchanged
  through every joined write, including a cross-node proxied join (origin
  lives on the owning node's transaction state; the join token carries no
  identity). Executor is the immediate writer (e.g. the compute service
  account).
- **Scheduled fire** — attributed to the **durable arming principal**
  (`ArmedBy`, captured at arm time and stored on the scheduled task),
  executed by a real `system`-kind platform principal — never the fake
  `"scheduler"` user. A fire whose durable `ArmedBy` doesn't match what was
  seeded pre-transaction aborts and retries on a later scan rather than
  attribute against a stale/forged value.
- **CBD-detached** (`COMMIT_BEFORE_DISPATCH` with `startNewTxOnDispatch:
  false`) — handed over to the application. The dispatch carries no
  transaction token, so the processor's callback writes are **ordinary
  independent requests**, not part of any platform-tracked chain. The
  identity those callbacks present governs attribution as usual (service
  credentials → that service; an OBO user token → that user). The
  callout's AuthContext (§1) carries the causal principal so the
  application can self-attribute if it chooses; the platform adds no
  carrier mechanism for this mode.

## 4. Cloud obligation

Emit `authtype`/`authid`/`authclaims` per §1 (including the `service_account`
→ `service` rename and the fail-loud unset-kind behaviour), surface
`attributedKind`/`executedBy` on change-history reads per §2, and implement
the three attribution paths in §3 identically — cascade origin propagation,
durable scheduled-arming attribution, and the CBD-detached handover boundary.
