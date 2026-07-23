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
slice** (`validator.go:118-123`). The presence of the `user_roles` vs `scopes` claim is the signal;
after collapse it is lost.

Kind is **not** equivalent to today's `ROLE_M2M` sniff: a client-credentials token *without*
`ROLE_M2M` was `user`, now `service`; a user token *with* `ROLE_M2M` was `service_account`, now
`user`. This is intended (matches the acceptance criteria) and must be called out in the PR.

### 3.3 AuthContext `authtype` — driven by Kind

`AttachAuthContext` (`internal/grpc/cloudevent.go:35-67`) stops sniffing `ROLE_M2M` and emits
`authtype = string(uc.Kind)`. **This changes the CloudEvents Auth-Context wire value
`service_account` → `service` and introduces `system`.** Deliberate contract change; see §10.

The `ROLE_M2M` **stream-join gate** (`internal/grpc/streaming.go:53-60`) stays role-based — it is
authorization, out of scope, unchanged. `CheckAccess` (`internal/contract/iam.go:16`) stays dead
(no production callers); not touched.

## 4. Cascade attribution — origin on the transaction (Join-based)

### 4.1 The seam is Join, not Begin

Ordinary nested/cascade writes **join** an existing transaction — they do **not** `Begin`
(`internal/domain/entity/handler.go` `beginOrJoin` ~110-116: if a `TransactionState` is already on
ctx it participates, `owned=false`, never Begins). So origin cannot be "resolved at Begin" for the
cascade write itself; it must already live on the joined `TransactionState`.

**Origin is set once, at the root `Begin`, and read by every joined write.**

```go
// At Begin (root of a causal chain):
origin := ambientOrigin(ctx)            // explicit ctx value; set by scheduled fire & detached dispatch
if origin == nil {
    if parent := spi.GetTransaction(ctx); parent != nil && parent.Origin != (spi.Principal{}) {
        origin = &parent.Origin           // CBD segment-continuation: inherit prior segment's origin
    }
}
if origin == nil {
    origin = principalOf(spi.GetUserContext(ctx))  // direct caller (root)
}
txState.Origin = *origin
```

Add `Origin Principal` to `spi.TransactionState` (`cyoda-go-spi/txcontext.go:85-96`), in the
**immutable-after-Begin** set alongside `ID`, `TenantID`, `SnapshotTime` — set once, read lock-free,
no `OpMu` participation.

### 4.2 Per-plugin capture & Join re-population

| Plugin | Begin | Join |
|---|---|---|
| memory | set `tx.Origin` (`plugins/memory/txmanager.go:130-158`) | re-attaches shared pointer (`:200`) → origin free |
| sqlite | set `tx.Origin` (`plugins/sqlite/txmanager.go:148-184`) | re-attaches shared pointer (`:213`) → origin free |
| postgres | set `tx.Origin` **and** store in a per-tx map (parallel to `tm.tenants`) (`plugins/postgres/transaction_manager.go:60-101`) | rebuilds `TransactionState{ID,TenantID}` (`:255-258`) → **must repopulate `Origin` from the per-tx map** |

Postgres is the critical case: it rebuilds state at every `Join`, so without explicit storage +
repopulation, origin is silently lost on the routed-callback path — the primary acceptance criterion.

### 4.3 Detached dispatch (COMMIT_BEFORE_DISPATCH `startNewTx=false`)

`engine_processors.go:~311` dispatches the processor with the tx **detached**
(`spi.WithTransaction(ctx, nil)`, empty txID `:315`). A callback write then has `GetTransaction ==
nil` → `beginOrJoin` Begins a **fresh independent tx** with no parent → would fall through to the
service account. **Fix:** seed an explicit **ambient-origin** ctx value into the detached
`dispatchCtx` (from the just-committed segment's origin) so the follow-on root `Begin` re-derives the
same origin. Covers the CBD acceptance case.

### 4.4 Two-hop / segmented cascade (X→Y→Z)

CBD segment-continuation (`engine_processors.go:~325/394`) opens the next segment with the prior
(just-committed) segment's `TransactionState` on ctx. The `Begin` rule (§4.1) inherits its `Origin`,
so Z still attributes to the original user. Combined with §4.3 this closes both segmented paths.

## 5. Scheduled attribution — durable arming principal

### 5.1 SPI

Add `ArmedBy Principal` to `spi.ScheduledTask` (`cyoda-go-spi/types.go:293-322`), **JSON-serialized
and durable** — cross-node fire reads the task from the store on a peer node that has no
request context. Legacy tasks armed pre-upgrade deserialize with zero-value `ArmedBy`; the fire path
treats a zero `ArmedBy` as the system principal (never the fake `"scheduler"`).

### 5.2 Capture at arm time

`internal/domain/workflow/arm.go` (~127 static, ~235 function-resolved) constructs the
`ScheduledTask` inside the triggering transition's ctx. Set `ArmedBy = origin(ctx)` — the same
origin the triggering write attributes to (from `GetTransaction(ctx).Origin`, fallback UserContext).
`armViaFunction` uses the Function result only for timing (`arm.go:227`) — **never** for origin
(negative-test invariant: no callout payload sets the attributed principal).

### 5.3 Fire

`internal/scheduler/executor.go` `Execute`: replace the synthetic `UserID="scheduler"`
`SystemUserContext` with:

- **executor** = a real `kind=system` platform principal (stable id, e.g. the app system principal
  id — configurable, defaulting to the platform system account; never the string `"scheduler"`).
- **ambient origin** = `task.ArmedBy`, seeded into `sysCtx` before `FireScheduledTransition`.

The fired transition's root `Begin` then stamps `Origin = ArmedBy` (§4.1 ambient branch), so the
anchor write and any cascade off it attribute to the arming principal, executed by system.

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
  versions** — sourcing executor from `Entity.Meta` would nil-panic the read.
- `EntityMeta` (`cyoda-go-spi/types.go:18-30`) gains the executor id+kind and the attributed kind so
  saves can persist them; the read path lifts them onto `EntityVersion`. Executor rides **inside the
  existing serialized meta blob** in every plugin (postgres `_meta` in `doc` JSONB, sqlite `meta`
  BLOB, memory in-struct) → **no schema migration**. (sqlite reads `User` from the `user_id` column;
  executor stays in the blob and is read from there.)

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

## 7. Complete stamp-site inventory

Every durable write that records a principal must set attributed = origin, executor = the immediate
principal. **Nil origin ⇒ use executor** (never panic, never elevate to a claimed user).

| # | Path | Site | Today | Change |
|---|---|---|---|---|
| 1 | Create | `service.go:289` | `ChangeUser=uc.UserID` | attributed=`GetTransaction(ctx).Origin`; executor=uc |
| 2 | Create collection | `service.go:1190` | `ChangeUser=uc.UserID` | same |
| 3 | Update | `service.go:1421` (read `:1404`) | guarded `uc.UserID` | attributed=tx.Origin; executor=uc |
| 4 | Update collection | `service.go:1755` (read `:1690`) | guarded `uc.UserID` | same |
| 5 | Scheduled-fire anchor | `fire_scheduled.go:321/325` | no user set | attributed=`ArmedBy`; executor=system |
| 6 | Delete flush (tx-commit) | `plugins/memory/txmanager.go:397-412` | executor `uc.UserID` | buffer attributed=tx.Origin at delete time; executor=committer |
| 7 | Delete flush (tx-commit) | `plugins/sqlite/txmanager.go:509` | hardcoded `''` | same (fixes memory/sqlite inconsistency — Gate-6) |
| 8 | Delete (non-tx) | `plugins/memory/entity_store.go:526-537, 593-613` | `uc.UserID` | attributed=origin/uc; executor=uc |
| 9 | Delete (non-tx) | `plugins/sqlite/entity_store.go:642-653` | `uc.UserID` | same |

The stamp must read origin from the **tx-carrying** ctx (joined/txCtx), not the pre-`Begin` outer
ctx (`service.go:1404` reads the outer ctx today — a naive swap to `GetTransaction(ctx).Origin`
there yields nil → executor fallback). Delete buffering mirrors how saves buffer
`entity.Meta.ChangeUser` at `memory/txmanager.go:382` / `sqlite/txmanager.go:467`.

## 8. Cross-node

Origin lives on the **owner node's** `TransactionState`; the cross-node claims token
(`internal/cluster/token/token.go:20-24`) is **unchanged** (carries no identity — D1 resolved). A
proxied join reaches the owner's per-tx origin via the existing proxy
(`internal/grpc/txroute_interceptor.go:132-151` → `txjoin.JoinFromToken` → `txMgr.Join`), so the
stamp executes on the owner node with origin in hand.

`internal/cluster/dispatch/cluster_dispatcher.go` (~329, 349, 367) currently forwards `uc.UserID`
to the remote node; reconcile so the forwarded identity is the **executor**, and origin travels on
the owner's transaction (not via the forwarded identity). Cross-node scheduled fire reads `ArmedBy`
from the durable task (§5.1).

## 9. Security & trust

- **Origin is trustworthy only on the joined platform-tx path** — it comes from server-side
  `TransactionState`, not worker input. This is the anti-forgery property.
- **Non-joined writes** (a callback that carries no `transactionId`) set origin = **executor** — the
  ordinary direct-write case (`attributed == executor`). They are **not** elevated to a claimed
  user. All cascade callbacks must carry the txID so they take the joined path.
- **No request field / callout payload can set the attributed principal** (negative test). Verified:
  no code path reads origin from a request body or callout result.
- **D3 (precedence):** a caller presenting its own user identity (an app still doing OBO) is used
  as-is for the executor; origin inheritance engages for the service-account-caller case within a
  user-originated tx. Additive / backward compatible.
- **AuthContext trust basis (pinned):** a compute node may rely on `authclaims` **only if it
  authenticates the cyoda server endpoint** (TLS server verification); otherwise forged claims are
  possible. Application authorization built on `authclaims` must fail **closed** when claims are
  absent/empty — which includes the `system` case.

## 10. AuthContext contract (cloud-parity) + SDK helper

### 10.1 Contract pin (`docs/cloud-parity/authcontext-attribution.md`, new)

The AuthContext extension (`authtype`/`authid`/`authclaims`) on every processor/criteria/function
callout is pinned:

- `authtype ∈ {user, service, system}`, driven by explicit principal-kind, always present.
- `authid` = principal id; `authclaims` = roles (comma-separated).
- Trust basis + fail-closed posture as §9.

**Breaking change management:** `service_account → service` breaks external compute nodes switching
on the old string. No in-repo test asserts `service_account` today (`dispatch_test.go:138-144` only
covers `user`), so CI stays green while consumers break. Mitigations:

- Update `cmd/cyoda/help/content/grpc.md:359` (names `service_account`).
- Add test coverage for the `service` and `system` branches (the M2M/service branch is currently
  untested).
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
3. `TransactionState.Origin` (immutable-after-Begin).
4. `ScheduledTask.ArmedBy` (durable JSON).
5. `EntityMeta` executor id+kind + attributed kind; `EntityVersion.Executor` + `AttributedKind`.

Conformance: extend `spitest` suites (transaction origin capture/join propagation; scheduled-task
`ArmedBy` round-trip; entity/audit executor round-trip). CHANGELOG `[Unreleased]` entry. Single
version tag cut at milestone end (coordinated-release: SPI tag first, then cyoda-go pin bump in one
commit, per `MAINTAINING.md`). Pseudo-version pin across all four `go.mod` files until then.

## 12. Backward compatibility

Additive throughout. Direct user calls unchanged (`attributed == executor`). Existing persisted rows
have no executor/kind → read API omits the new fields, `user` unchanged. No data migration (executor
rides in the existing meta blob). In-flight scheduled tasks armed pre-upgrade fire as system (zero
`ArmedBy`), never as `"scheduler"`. `authtype` value change is the one deliberate wire break (§10).

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

No gRPC status-code change; the CloudEvents attribute value set changes as above.

## 14. Test coverage matrix (scenario × layer)

Layers: **U**=unit, **E**=running-backend e2e (per backend), **P**=cross-backend parity,
**G**=gRPC/dispatch.

| Scenario | U | E | P | G |
|---|---|---|---|---|
| principal-kind: user/service/system for human, service acct, human+ROLE_M2M, scheduler | ✅ | | | ✅ (authtype) |
| `authtype` emits `service`/`system` (regression for the wire change) | ✅ | | | ✅ |
| Cascade: service processor writes 2nd entity in user tx → attributed=user, executor=service; node presents **no** user token | | ✅ | ✅ | ✅ |
| Two-hop cascade X→Y→Z (segmented) → attributed=user | | ✅ | ✅ | |
| CBD (`startNewTx=false`) cascade → attributed=user (detached-dispatch origin seed) | | ✅ | ✅ | |
| Cross-node (proxied-join) cascade → same attribution as same-node | | ✅ (multinode) | | ✅ |
| Delete cascade: processor deletes 2nd entity → attributed=user, executor=service | | ✅ | ✅ | |
| Delete tombstone attribution consistent memory vs sqlite (Gate-6 fix) | ✅ | | ✅ | |
| Scheduled: armed by user → fires as that user; executor=system; never `"scheduler"` | ✅ | ✅ | ✅ | |
| Scheduled: armed by system/service action → fires as that account | ✅ | ✅ | | |
| Scheduled anchor write stamped (not carrying prior version's user) | | ✅ | ✅ | |
| Scheduled cross-node fire reads durable `ArmedBy` | | ✅ (multinode) | | |
| Criteria/function callout write inherits attribution identically | | ✅ | ✅ | ✅ |
| Negative: no request field / callout payload can set attributed principal | ✅ | ✅ | | ✅ |
| Read API: both `user` and `executedBy` visible; legacy rows omit new fields | ✅ | ✅ | | |
| SPI conformance: tx origin, ScheduledTask.ArmedBy, EntityVersion.Executor round-trip | ✅ (spitest) | | | |
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
