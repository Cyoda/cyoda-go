# Attribute deferred & cascaded workflow actions to the user who caused them

- **Status:** Design agreed; ready for implementation plan.
- **Milestone:** v0.8.3 (single PR onto `release/v0.8.3`).
- **SPI:** requires additive changes to `github.com/cyoda-platform/cyoda-go-spi`; tagged once at milestone end (coordinated-release window; pseudo-version pin until then).

## 1. Problem & goal

When a user's action triggers a **later** (scheduled timer) or **indirect** (cascade — a
processor writes *other* entities) workflow action, cyoda-go records the follow-on as done by a
service or system account, losing the human who actually caused it.

- **Cascade:** a processor reacting to a user's transition writes to entity Y. Y's change is
  recorded as the compute **service account** — should be the **user**.
- **Scheduled:** a timer armed by a user's transition fires later and auto-transitions. Recorded as
  the fake user `"scheduler"` — should be the **arming principal**.

**Principle: attribution and authorization are separate.** A follow-on action is **authorized** as
the system/service that runs it, but **attributed** to the principal who caused it. *Who caused it*
is captured by the platform when the follow-on is created — never supplied by the worker. This
issue is attribution-only: it changes *who a follow-on action is recorded as*, never *what anyone is
allowed to do*. cyoda-go performs no per-user authorization and this design adds none.

## 2. The model — one `Principal`, two roles

New SPI type:

```go
// Principal identifies an actor and its kind.
type PrincipalKind string

const (
    PrincipalUser    PrincipalKind = "user"
    PrincipalService PrincipalKind = "service"
    PrincipalSystem  PrincipalKind = "system"
)

type Principal struct {
    ID   string        `json:"id"`
    Kind PrincipalKind `json:"kind"`
}
```

Every durable change now carries two principals:

- **attributed** — *who caused it*. The **origin** of the causal chain, propagated by the platform.
  Never worker-supplied.
- **executor** — *who ran it*. The immediate authenticated principal that performed the write.

For a direct user call, `attributed == executor == the user`. They diverge only for cascades
(executor = service account) and scheduled fires (executor = system).

**One propagation rule everywhere:** origin = the authenticated principal at the **root** of the
causal chain, propagated unchanged through cascades and scheduled fires. The human who *deploys* a
workflow is never in the causal chain of a later automated fire — the origin is whoever's
authenticated action set that chain in motion (a user, or a service/system account). This resolves
the issue's "scheduled-arms-scheduled → system" example: that chain is rooted in a system/service
action, so it attributes to that account — not because origin resets at the scheduler boundary.

## 3. Principal-kind (prerequisite) — replaces role-sniffing

### 3.1 SPI

Add `Kind PrincipalKind` to `spi.UserContext` (`cyoda-go-spi/context.go:17-23`). Additive; zero
value `""` is tolerated (treated as `user` by consumers for backward compat, but every in-repo
constructor sets it explicitly).

### 3.2 Set `Kind` at every constructor

| Constructor | Site | Kind |
|---|---|---|
| First-party JWT validator | `internal/auth/validator.go` `buildUserContext` (~104-134) | `user` if token carries `user_roles` (OBO); `service` if it carries `scopes` (client-credentials) |
| OIDC | `internal/auth/oidc/usercontext.go` `buildOIDCUserContext` (~32-69) | `user` |
| M2M client mint (roles) | `internal/domain/account/m2m_adapter.go` (~130) | n/a for auth; clients authenticate via client-credentials → validator assigns `service` |
| Mock/dev IAM default | `app/app.go` (~376-385) | config-driven; default `user` (mock represents a human dev by default) |
| App system principal | `app/app.go` (~243-247) | `system` |
| Scheduler system principal | `internal/scheduler/executor.go` `SystemUserContext` (~29-36) | `system` |

**Kind must be branched BEFORE `buildUserContext` collapses `user_roles`/`scopes` into one `Roles`
slice** (`validator.go:118-123`). The signal is **claim-key presence** — `_, ok :=
claims["user_roles"]` — **not** the len-based collapse (`if len(roles)==0 { roles = scopes }`): an
OBO user token with an *empty* `user_roles` array must classify as `user`, not `service`.

Kind is **not** equivalent to today's `ROLE_M2M` sniff: a client-credentials token *without*
`ROLE_M2M` was `user`, now `service`; a user token *with* `ROLE_M2M` was `service_account`, now
`user`. This is intended (matches the acceptance criteria) and must be called out in the PR.

### 3.3 AuthContext `authtype` — driven by Kind

`AttachAuthContext` (`internal/grpc/cloudevent.go:35-67`) stops sniffing `ROLE_M2M` and emits
`authtype` from `uc.Kind`. **This changes the CloudEvents Auth-Context wire value
`service_account` → `service` and introduces `system`.** Deliberate contract change; see §10.

**Zero-Kind emission rule — fail loud.** The pinned contract requires `authtype ∈ {user, service,
system}`, always present and **faithful**. An unset Kind at the emission point means a missed
constructor or missed cross-node forwarding — an internal bug — and emitting a normalized `user`
for what may be a service would be a wrong-but-available substitute, which the
correctness-over-availability rule prohibits. `AttachAuthContext` (via its dispatch call site)
**fails the callout dispatch** on unset Kind; the callout is not sent with an unfaithful
AuthContext. The cross-node G-layer test asserts the correct kind arrives on a peer-dispatched
callout, so this failure mode cannot ship silently. (§3.1's zero-value tolerance is for *reading*
legacy data, never for emitting the pinned wire contract.)

The `ROLE_M2M` **stream-join gate** (`internal/grpc/streaming.go:53-60`) stays role-based — it is
authorization, out of scope, unchanged. `CheckAccess` (`internal/contract/iam.go:16`) stays dead
(no production callers); not touched.

**Cross-node `Kind` forwarding (required).** `AttachAuthContext` runs on the node that performs the
callout (`dispatch.go:113`), which for a peer-dispatched processor/criteria/function is the **target
node** (`internal/cluster/dispatch/handler.go:69`). The dispatch wire request
(`internal/cluster/dispatch/cluster_dispatcher.go:318-371`) forwards only `UserID` + `Roles`, and
`buildContext` (`internal/cluster/dispatch/handler.go:134-145`) rebuilds `UserContext` without
`Kind`. Without change, every **cross-node** callout would emit `authtype = ""` — corrupting the
contract this issue pins, on the primary (multi-node) deployment. **Add `Kind` to
`DispatchCalloutRequest` and to `buildContext`**, forwarded and reconstructed. G-layer test asserts
correct `authtype` on a cross-node callout.

## 4. Cascade attribution — origin on the transaction (Join-based)

### 4.1 The seam is Join, not Begin

Ordinary nested/cascade writes **join** an existing transaction — they do **not** `Begin`
(`internal/domain/entity/handler.go` `beginOrJoin` ~110-116: if a `TransactionState` is already on
ctx it participates, `owned=false`, never Begins). So origin cannot be "resolved at Begin" for the
cascade write itself; it must already live on the joined `TransactionState`.

**Origin is set once, at the root `Begin`, and read by every joined write.**

```go
// At Begin — precedence: parent-tx > ambient > UserContext.
// Exported as spi.ResolveOrigin(ctx) so all backends (incl. the
// out-of-tree commercial one) share ONE implementation; backend
// divergence here would be an attribution bug.
func ResolveOrigin(ctx context.Context) Principal {
    if parent := GetTransaction(ctx); parent != nil && parent.Origin != (Principal{}) {
        return parent.Origin // segment continuation: a live tx is authoritative for its chain
    }
    if amb := GetAmbientOrigin(ctx); amb != (Principal{}) {
        return amb           // root seed with no tx yet: scheduled fire (§5.3)
    }                        // or verified attribution token (§4.3) — both server-verified

    return principalOf(GetUserContext(ctx)) // direct caller (root)
}
```

A **zero `Principal` is "absent"** at every branch (a zero ambient value never stamps a zero
`Origin`). The precedence is parent-tx first: ambient is strictly a *root* seed for a chain that has
no transaction yet; once a transaction exists, its `Origin` is authoritative.

Add `Origin Principal` to `spi.TransactionState` (`cyoda-go-spi/txcontext.go:85-96`), in the
**immutable-after-Begin** set alongside `ID`, `TenantID`, `SnapshotTime` — set once, read lock-free,
no `OpMu` participation. **Update the immutable-set godoc at `cyoda-go-spi/txcontext.go:53`** (and
the concurrency contract) to list `Origin`, or reviewers will flag the new field.

The ambient origin is a **new SPI-exported** ctx value (§11 item 7): the plugin `Begin`
implementations are separate Go modules importing only `cyoda-go-spi`, so
`WithAmbientOrigin` / `GetAmbientOrigin` / `ResolveOrigin` live in the SPI. Exactly **two seed
sites** exist, both server-verified: the scheduled fire (§5.3, from the durable task row) and the
CBD attribution token (§4.3, HMAC-verified at the callback boundary). Each plugin `Begin` calls
`spi.ResolveOrigin(ctx)`.

### 4.2 Per-plugin capture & Join re-population

| Plugin | Begin | Join |
|---|---|---|
| memory | set `tx.Origin` (`plugins/memory/txmanager.go:130-158`) | re-attaches shared pointer (`:200`) → origin free |
| sqlite | set `tx.Origin` (`plugins/sqlite/txmanager.go:148-184`) | re-attaches shared pointer (`:213`) → origin free |
| postgres | set `tx.Origin` **and** store in a per-tx map (parallel to `tm.tenants`) (`plugins/postgres/transaction_manager.go:60-101`) | rebuilds `TransactionState{ID,TenantID}` (`:255-258`) → **must repopulate `Origin` from the per-tx map** |

Postgres is the critical case: it rebuilds state at every `Join`, so without explicit storage +
repopulation, origin is silently lost on the routed-callback path — the primary acceptance criterion.
The per-tx origin map follows the `tm.tenants` lifecycle exactly: populate at `Begin`, delete at
`Commit`/`Rollback`.

### 4.3 Detached dispatch (COMMIT_BEFORE_DISPATCH `startNewTx=false`) — attribution token

`engine_processors.go:~311` dispatches the processor with the tx **detached**
(`spi.WithTransaction(ctx, nil)`, empty txID `:315`): no tx token is minted today
(`cluster_dispatcher.go:74-82`), and a callback write arrives as a **separate network request** that
Begins a fresh independent tx — context values do not cross the network, so without a carrier the
origin is lost and the write would fall to the service account.

**The carrier is the same pattern the tx-join already uses**: a server-signed token that transits
the worker. The worker is a courier, not a source — it echoes what the server minted and cannot
forge it. D1's "origin never rides an M2M relay hop" rejects *worker-supplied* origin, not a
server-signed assertion the server itself verifies on return.

- **Mint:** at detached dispatch, instead of no token, attach an **attribution token** — HMAC-signed
  by the cluster signer (same trust root as `token.Claims`), carrying `{origin Principal,
  expiresAt}`. Stateless by design: any node verifies with the shared secret — no origin store, no
  cross-node resolution (unlike a reference-token design, which would need both).
- **Echo:** the worker returns it on callback writes in the same token slot it already echoes for
  tx joins. Compute-node contract addition — documented in `grpc.md` + the cloud-parity doc (§10.1).
- **Verify & seed:** at the callback boundary (alongside the tx-token path in
  `txroute_interceptor`/HTTP middleware), a valid attribution token seeds
  `spi.WithAmbientOrigin(ctx, origin)`; the callback's fresh `Begin` picks it up via the ambient
  branch of `ResolveOrigin` (§4.1). An **invalid/expired** token fails the request (fail-loud, like
  a bad tx token — never silently downgraded); an **absent** token is the ordinary non-joined case →
  attributed = executor (old SDKs keep working; additive).
- **Disclosure:** none new — the same dispatch already hands the worker `authid` = the origin in its
  AuthContext.
- **Replay surface:** a worker can attach the token to unrelated writes within its TTL,
  mis-attributing them to the origin. Bounded and analogous to the joined case (a worker can write
  anything inside a joined tx, attributed to origin); the executor recorded alongside keeps it
  auditable. TTL is the knob (plan: align with the tx-token expiry discipline).

Semantic note, preserved deliberately: `startNewTx=false` severs the *transaction*, not the *cause*.
Attribution follows the causal chain (§2), so the detached follow-on still attributes to the origin
when the token is echoed; the executor-attributed case is reduced to "worker didn't echo."

### 4.4 Two-hop / segmented cascade (X→Y→Z)

CBD segment-continuation (`engine_processors.go:~325/394`) opens the next segment with the prior
(just-committed) segment's `TransactionState` still on ctx. The `Begin` rule (§4.1, parent-tx
branch) inherits its `Origin`, so Z still attributes to the original user.

## 5. Scheduled attribution — durable arming principal

### 5.1 SPI

Add `ArmedBy Principal` to `spi.ScheduledTask` (`cyoda-go-spi/types.go:293-322`), **JSON-serialized
and durable** — cross-node fire reads the task from the store on a peer node that has no
request context. Legacy tasks armed pre-upgrade deserialize with zero-value `ArmedBy`; the fire path
treats a zero `ArmedBy` as the system principal (never the fake `"scheduler"`). Legacy-vs-new
detection relies on this **zero-value check on read**, never on field absence: `omitempty` does not
omit a zero struct, so `ArmedBy` may serialize as `{"id":"","kind":""}` — which round-trips to zero
and is handled identically.

### 5.2 Capture at arm time

`internal/domain/workflow/arm.go` (~127 static, ~235 function-resolved) constructs the
`ScheduledTask` inside the triggering transition's ctx. Set `ArmedBy = origin(ctx)` — the same
origin the triggering write attributes to (from `GetTransaction(ctx).Origin`, fallback UserContext).
`armViaFunction` uses the Function result only for timing (`arm.go:227`) — **never** for origin
(negative-test invariant: no callout payload sets the attributed principal).

### 5.3 Fire

Two changes:

- **executor** = a real `kind=system` platform principal (stable id, e.g. the app system principal
  id — configurable, defaulting to the platform system account; never the string `"scheduler"`).
  Replace the synthetic `UserID="scheduler"` `SystemUserContext` (`internal/scheduler/executor.go`).
- **ambient origin** = the arming principal, seeded via `spi.WithAmbientOrigin` **inside
  `FireScheduledTransition`** (`internal/domain/workflow/fire_scheduled.go:53,75`) — its documented
  single engine door — **not** in `executor.go`. There are two doors into the engine and only one
  goes through `Execute`: the **cross-node peer fire** calls `FireScheduledTransition` directly
  (`internal/cluster/scheduler_rpc.go:271-273`), bypassing `Execute`. Seeding at the engine door
  covers both local and peer fire in one place.

**The seed source is the durable row, never the argument.** `FireScheduledTransition`'s own trust
contract (`fire_scheduled.go:61-71`) treats only `task.ID` as trusted — and on the peer path the
argument is deserialized from an RPC body, so seeding from `task.ArmedBy` would let a forged or
stale peer dispatch set the attributed principal (violating §9). Instead:

1. **Pre-`Begin` point-read** of the task row by `task.ID` → seed ambient origin from the durable
   `ArmedBy` (zero ⇒ no seed ⇒ system, §5.1).
2. The existing **in-tx re-read guard** additionally verifies the re-read `ArmedBy` equals the
   seeded value; on mismatch (a concurrent re-arm between point-read and fire tx) the fire
   **aborts and rolls back** — fail-closed; the scan loop's redispatch retries it. Origin stays
   immutable-after-`Begin`; a fire never commits with an origin that doesn't match the durable row.

The fire's root `Begin` then stamps `Origin = ArmedBy` (§4.1 ambient branch), so the anchor write and
any cascade off it attribute to the arming principal, executed by system.

### 5.4 The scheduled-fire anchor write is a separate stamp site

The fired entity is persisted by `internal/domain/workflow/fire_scheduled.go` (~321 `Save` / ~325
`CompareAndSave`) — **not** through the `service.go` sites, and today it sets **no** `ChangeUser`
(carries the previously-loaded version's user). This path must stamp attributed = `ArmedBy`,
executor = system. Added to the stamp-site inventory (§7).

## 6. Executor on the audit record + read API

### 6.1 SPI

- `EntityVersion` (`cyoda-go-spi/types.go:37-45`) gains `Executor Principal` **and**
  `AttributedKind PrincipalKind` (the `User` field stays = attributed id). Executor lives on
  `EntityVersion`, **not** `Entity.Meta`, because `EntityVersion.Entity` is **nil for DELETED
  versions** on memory/sqlite — sourcing executor from `Entity.Meta` would nil-panic the read.
  (Postgres tombstones carry a non-nil Entity rebuilt from the *prior* doc — which is why row 9 of
  §7 must stamp the tombstone at delete time, or a DELETED row would surface the previous writer's
  executor.)
- `EntityMeta` (`cyoda-go-spi/types.go:18-30`) gains the executor id+kind and the attributed kind so
  saves can persist them; the read path lifts them onto `EntityVersion`. Executor rides **inside the
  existing serialized meta blob** in every plugin (postgres `_meta` in `doc` JSONB, sqlite `meta`
  BLOB, memory in-struct) → **no schema migration**. (sqlite reads `User` from the `user_id` column;
  executor stays in the blob and is read from there.)
- **Per-plugin serialization is not automatic** — no plugin does `json.Marshal(spi.EntityMeta)`; each
  owns a private meta DTO with hand-written marshal/unmarshal in **both** directions. Postgres alone
  (`plugins/postgres/entity_doc.go`): `entityMeta` struct (13-30), `marshalEntityDoc` (35-52),
  `unmarshalEntityDoc` (116-131), **and** `unmarshalEntityVersion` (154-161). sqlite (`entityMetaDB`
  + read-supplement `entity_store.go:39-53,1002-1012`) and memory (`entityVersion` + every copy site)
  are analogous. `unmarshalEntityVersion` must populate `EntityVersion.Executor` **independently of
  `Entity`** (nil for DELETED versions). This is a 3-backend × (DTO + marshal + unmarshal) change;
  the plan must enumerate every site so no backend is silently missed.

### 6.2 Read API

`GET /entity/{entityId}/changes` metadata (`GetEntityChangesMetadata`,
`internal/domain/entity/handler.go:590-611`) surfaces both actors. `user` stays **required**
(backward compat + OpenAPI breaking-change CI):

```json
{
  "changeType": "UPDATED",
  "timeOfChange": "2026-07-22T10:00:00Z",
  "user": "userY",
  "attributedKind": "user",
  "executedBy": { "id": "svc-compute-1", "kind": "service" },
  "transactionId": "..."
}
```

- Legacy rows (no stored executor/kind): omit `executedBy` and `attributedKind` (do not emit JSON
  `null`); `user` renders as today.
- `internal/domain/entity/service.go` `EntityChangeEntry` (~111-118) gains `Executor` +
  `AttributedKind`; the mapping (~755-763) lifts them from `EntityVersion`.
- The audit "actor" read (`internal/domain/audit/handler.go:89-97`) derives from version-history
  `v.User` and inherits the attributed principal for free — no extra work; confirm intended.
- `StateMachineEvent` (`cyoda-go-spi/types.go:351-360`) has **no** actor field — no executor leak
  there, and no attribution added (out of scope).

## 7. Attribution rule + complete stamp-site inventory

**The stamp rule (implements D3 exactly):**

```
executor   = the immediate authenticated principal (UserContext) at the write
attributed = executor,                        if executor.Kind ∈ {user, ""}   // D3: a presented user
                                              // identity is used as-is; "" (legacy) is conservative
           = GetTransaction(ctx).Origin,      if executor.Kind ∈ {service, system} and a joined tx
                                              // with non-zero Origin is present
           = executor,                        otherwise (non-joined / zero origin — never panic,
                                              // never elevate to a claimed user)
```

Origin inheritance engages **only for service/system executors** within a platform transaction. An
app still doing OBO itself (presenting a user token on its callback) keeps today's behavior — its
writes record that user even inside another user's transaction. This supersedes nothing: it is the
issue's D3, applied per-write.

| # | Path | Site | Today | Change |
|---|---|---|---|---|
| 1 | Create | `service.go:289` | `ChangeUser=uc.UserID` | stamp rule above |
| 2 | Create collection | `service.go:1190` | `ChangeUser=uc.UserID` | same |
| 3 | Update | `service.go:1421` (read `:1404`) | guarded `uc.UserID` | same |
| 4 | Update collection | `service.go:1755` (read `:1690`) | guarded `uc.UserID` | same |
| 5 | Scheduled-fire anchor | `fire_scheduled.go:321/325` | no user set | attributed=`ArmedBy` (§5.3); executor=system |
| 6 | Delete stage (tx) | `plugins/memory/entity_store.go:479+`, `plugins/sqlite/entity_store.go:587+` | buffers only `Deletes[id]=true` — **no principal** | capture `{attributed, executor}` per delete at **stage time** via new SPI `TransactionState.DeleteAttribution` (§11) |
| 7 | Delete flush (tx-commit) | `plugins/memory/txmanager.go:397-412`, `plugins/sqlite/txmanager.go:509` | memory=committer's `uc.UserID`; sqlite=hardcoded `''` | stamp from `DeleteAttribution` (fallback: stamp rule at commit ctx) |
| 8 | Delete (non-tx) | `plugins/memory/entity_store.go:526-537, 593-613`; `plugins/sqlite/entity_store.go:642-653` | `uc.UserID` | stamp rule above |
| 9 | Delete (postgres, tx + non-tx) | `plugins/postgres/entity_store.go:266-337` | tombstone re-marshals the **prior** doc → records the **previous writer** | stamp rule at delete time into the tombstone doc |
| 10 | DeleteAll | all three backends' `DeleteAll` paths | as rows 6-9 per backend | same treatment as the corresponding delete path |

Today's tombstone user is a **three-way backend divergence** (memory=committer, sqlite=`''`,
postgres=previous writer) — a live bug this change fixes with one uniform rule (Gate-6). Two
delete-specific notes:

- **Executor = the stager, not the committer.** Saves persist the meta stamped at stage time; a
  joined service-account delete committed later by the user-owned request must likewise record
  executor=service. That is why row 6 captures principals at stage time — `Deletes` is
  `map[string]bool` and cannot carry them. `DeleteAttribution` is **additive** to
  `TransactionState` (the `Deletes` shape is untouched — no break for the out-of-tree backend);
  absent entries fall back to the commit-ctx stamp rule.
- **sqlite tombstones are inserted with `meta = NULL`** (`entity_store.go:649-653`) — there is no
  blob for the executor to ride in. New tombstones write a meta blob (the column exists; legacy
  NULL rows read as legacy). Still no schema migration.

The stamp must read origin from the **tx-carrying** ctx (joined/txCtx), not the pre-`Begin` outer
ctx (`service.go:1404` reads the outer ctx today — a naive swap to `GetTransaction(ctx).Origin`
there yields nil → executor fallback).

## 8. Cross-node

Origin lives on the **owner node's** `TransactionState`; the cross-node claims token
(`internal/cluster/token/token.go:20-24`) is **unchanged** (carries no identity — D1 resolved). A
proxied join reaches the owner's per-tx origin via the existing proxy
(`internal/grpc/txroute_interceptor.go:132-151` → `txjoin.JoinFromToken` → `txMgr.Join`), so the
stamp executes on the owner node with origin in hand.

`internal/cluster/dispatch/cluster_dispatcher.go` (~329, 349, 367) currently forwards `uc.UserID` +
`Roles` to the remote node; reconcile so the forwarded identity is the **executor**, and origin
travels on the owner's transaction (not via the forwarded identity). The forwarded request must also
carry the executor **`Kind`** (§3.3) — otherwise the target node's callout emits `authtype = ""`.
Cross-node scheduled fire reads `ArmedBy` from the durable task and seeds the ambient origin inside
`FireScheduledTransition` (§5.3), which the peer path (`scheduler_rpc.go:271`) reaches directly.

## 9. Security & trust

- **Origin is trustworthy only on the joined platform-tx path** — it comes from server-side
  `TransactionState`, not worker input. This is the anti-forgery property.
- **Non-joined writes with no verified server-signed carrier** set origin = **executor** — the
  ordinary direct-write case (`attributed == executor`). They are **not** elevated to a claimed
  user. Joined-mode cascade callbacks carry the txID; CBD-detached callbacks carry the attribution
  token (§4.3) — in both cases the origin is a server-side assertion (stored state or verified
  signature), never worker-supplied.
- **No request field / callout payload can set the attributed principal** (negative test). Verified:
  no code path reads origin from a request body or callout result; the peer scheduled-fire RPC's
  task body is likewise never trusted for origin — the seed comes from the durable row with an
  in-tx verify-or-abort (§5.3).
- **D3 (precedence):** attributed = executor whenever the executor is a **user** (an app doing OBO
  keeps recording its presented user, even inside another principal's tx); origin inheritance
  engages only for **service/system** executors within a platform tx (§7 stamp rule). Additive /
  backward compatible.
- **AuthContext trust basis (pinned):** a compute node may rely on `authclaims` **only if it
  authenticates the cyoda server endpoint** (TLS server verification); otherwise forged claims are
  possible. Application authorization built on `authclaims` must fail **closed** when claims are
  absent/empty — which includes the `system` case.

## 10. AuthContext contract (cloud-parity) + SDK helper

### 10.1 Contract pin (`docs/cloud-parity/authcontext-attribution.md`, new)

The AuthContext extension (`authtype`/`authid`/`authclaims`) on every processor/criteria/function
callout is pinned:

- `authtype ∈ {user, service, system}`, driven by explicit principal-kind, always present (dispatch
  fails loud rather than emit an unfaithful value — §3.3).
- `authid` = principal id; `authclaims` = roles (comma-separated).
- **Attribution-token echo** (new, additive): a CBD-detached dispatch carries a server-signed
  attribution token in the same slot as the tx-join token; a compute node echoes whichever token it
  received on its callback writes (§4.3). Not echoing is valid (writes then attribute to the
  executor); forging is not possible (HMAC).
- Trust basis + fail-closed posture as §9.

**Breaking change management:** `service_account → service` breaks external compute nodes switching
on the old string. No in-repo test asserts `service_account` today (`dispatch_test.go:138-144` only
covers `user`), so CI stays green while consumers break. Mitigations:

- Update `cmd/cyoda/help/content/grpc.md:359` (names `service_account`).
- Add test coverage for the `service` and `system` branches (the M2M/service branch is currently
  untested).
- `internal/scheduler/executor_test.go:33-34` (`TestSystemUserContext_HasTenant`) asserts
  `UserID == "scheduler"` — update it for the new system-principal identity (§5.3).
- Document in release notes as a compute-node-facing contract change.

### 10.2 SDK helper (`authctx`)

Small helper for compute-node authors to read AuthContext roles and apply a role gate, with a
**fail-closed** default:

```go
roles := authctx.Roles(ce)          // []string from authclaims (empty if absent)
ok := authctx.Require(ce, "ROLE_X") // false when claims absent/empty OR authtype==system
```

Empty/absent claims and `system` origin are **denied by default** (must be explicitly allowed).
Ships in the SDK helper location used by compute-node authors (confirm path in plan); its
empty-roles posture is pinned as part of this contract.

## 11. SPI change summary & versioning

Additive SPI changes (all in `cyoda-go-spi`):

1. `Principal` + `PrincipalKind` (new).
2. `UserContext.Kind`.
3. `TransactionState.Origin` (immutable-after-Begin; update the immutable-set godoc `txcontext.go:53`).
4. `TransactionState.DeleteAttribution` (per-delete `{attributed, executor}` captured at stage time,
   §7 row 6 — additive; `Deletes` shape untouched).
5. `ScheduledTask.ArmedBy` (durable JSON).
6. `EntityMeta` executor id+kind + attributed kind; `EntityVersion.Executor` + `AttributedKind`.
7. `WithAmbientOrigin` / `GetAmbientOrigin` ctx helpers + **`ResolveOrigin(ctx)`** (single shared
   precedence implementation, §4.1) — consumed by plugin `Begin`; seeded by the scheduled fire
   (§5.3) and the verified attribution token (§4.3).

Conformance: extend `spitest` suites — transaction origin capture/join propagation covering **all
three `ResolveOrigin` precedence branches** (parent-tx, ambient, UserContext); delete-attribution
stage/flush round-trip; scheduled-task `ArmedBy` round-trip; entity/audit executor round-trip. New
suites construct `UserContext` with `Kind` set **explicitly** (the existing `spitest` harness builds
it without Kind — fine under the tolerance rule, but kind round-trip assertions must not rely on
zero values). These suites are what carry the contract to the out-of-tree commercial backend.
CHANGELOG `[Unreleased]` entry. Single
version tag cut at milestone end (coordinated-release: SPI tag first, then cyoda-go pin bump in one
commit, per `MAINTAINING.md`). Pseudo-version pin across all four `go.mod` files until then.

## 12. Backward compatibility

Additive throughout. Direct user calls unchanged (`attributed == executor`). Existing persisted rows
have no executor/kind → read API omits the new fields, `user` unchanged. No data migration (executor
rides in the existing meta blob). In-flight scheduled tasks armed pre-upgrade fire as system (zero
`ArmedBy`), never as `"scheduler"`. Compute nodes that don't echo the attribution token keep working
(their CBD-detached writes attribute to the executor). `authtype` value change is the one deliberate
wire break (§10).

## 13. Error / status-code tables

### 13.1 `GET /entity/{entityId}/changes` (metadata) — additive; no new error paths

| Scenario | Status | Code | Notes |
|---|---|---|---|
| Success (with/without executor fields) | 200 | — | `executedBy`/`attributedKind` omitted for legacy rows |
| Unauthenticated | 401 | `unauthorized` | unchanged (auth middleware) |
| Entity not found / no history | 200 `[]` | — | unchanged behavior |
| Malformed `entityId` | 400 | binding error → ProblemDetail | unchanged |
| Backend error | 500 | classified | unchanged |

No status-code changes; response body gains optional fields only.

### 13.2 gRPC callout AuthContext (processor / criteria / function) — attribute value change

| Scenario | `authtype` before | `authtype` after |
|---|---|---|
| Human user (OBO token, `user_roles`) | `user` | `user` |
| Service account (client-credentials, `scopes`) | `service_account` | `service` |
| Human holding `ROLE_M2M` | `service_account` | `user` |
| Scheduler / system fire | `user` (`UserID="scheduler"`) | `system` |

No gRPC status-code change for well-formed dispatches; the CloudEvents attribute value set changes
as above. New internal failure mode: a dispatch with an unset principal-kind **fails loud** (§3.3)
instead of emitting an unfaithful `authtype`; a callback presenting a forged/expired attribution
token is rejected like a bad tx token (§4.3).

## 14. Test coverage matrix (scenario × layer)

Layers: **U**=unit, **E**=running-backend e2e (per backend), **P**=cross-backend parity,
**G**=gRPC/dispatch.

| Scenario | U | E | P | G |
|---|---|---|---|---|
| principal-kind: user/service/system for human, service acct, human+ROLE_M2M, scheduler | ✅ | | | ✅ (authtype) |
| `authtype` emits `service`/`system` (regression for the wire change) | ✅ | | | ✅ |
| Cascade: service processor writes 2nd entity in user tx → attributed=user, executor=service; node presents **no** user token | | ✅ | ✅ | ✅ |
| Two-hop cascade X→Y→Z (segmented) → attributed=user | | ✅ | ✅ | |
| CBD (`startNewTx=false`) detached callback echoing the attribution token → attributed=user | | ✅ | ✅ | ✅ |
| CBD detached callback **without** token (old SDK) → attributed=executor; forged/expired token → request fails | ✅ | ✅ | | ✅ |
| D3: OBO user-token callback joined into another user's tx keeps recording **its own** user | | ✅ | ✅ | |
| Cross-node (proxied-join) cascade → same attribution as same-node | | ✅ (multinode) | | ✅ |
| Delete cascade: processor deletes 2nd entity → attributed=user, executor=**service (stager, not committer)** | | ✅ | ✅ | |
| Delete tombstone attribution uniform across memory/sqlite/postgres (fixes 3-way divergence, Gate-6) | ✅ | | ✅ | |
| Scheduled: armed by user → fires as that user; executor=system; never `"scheduler"` | ✅ | ✅ | ✅ | |
| Scheduled: armed by system/service action → fires as that account | ✅ | ✅ | | |
| Scheduled anchor write stamped (not carrying prior version's user) | | ✅ | ✅ | |
| Scheduled cross-node fire reads durable `ArmedBy` (never the RPC body) | | ✅ (multinode) | | |
| Scheduled fire aborts on `ArmedBy` mismatch (concurrent re-arm between point-read and fire tx) | ✅ | ✅ | | |
| Criteria/function callout write inherits attribution identically | | ✅ | ✅ | ✅ |
| Negative: no request field / callout payload can set attributed principal | ✅ | ✅ | | ✅ |
| Read API: both `user` and `executedBy` visible; legacy rows omit new fields | ✅ | ✅ | | |
| SPI conformance: ResolveOrigin precedence branches, DeleteAttribution, ScheduledTask.ArmedBy, EntityVersion.Executor round-trips | ✅ (spitest) | | | |
| `authctx` SDK helper: fail-closed on empty claims & system origin | ✅ | | | |

Concurrency race tests (goroutine-storm) — if any — are isolated single-backend e2e, not in the
shared-backend parity suite.

## 15. Out of scope

- Platform-side authorization inheritance (authorizing cascade writes against the origin's roles).
- Per-user authorization of any kind; `CheckAccess` stays dead.
- Any cross-node token/claims change (origin never rides the M2M relay hop).
- `StateMachineEvent` actor attribution.

## 16. Open items for the plan

- Confirm the SDK `authctx` helper location (compute-node SDK path).
- Confirm the configurable system-principal id used by the scheduler fire (reuse app system
  principal vs a dedicated config key).
- Confirm audit-handler actor read (`internal/domain/audit/handler.go:89-97`) inheriting attributed
  is the intended behavior (expected: yes).
