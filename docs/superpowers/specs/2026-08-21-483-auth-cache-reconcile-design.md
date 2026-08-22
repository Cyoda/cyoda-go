# Auth-cache reconciliation (#483) — design

**Status:** approved for implementation (two fresh-context design-review
rounds; all findings incorporated).
**Companions:**
`docs/superpowers/research/2026-08-21-483-auth-cache-reconcile-research.md`
(factual survey), `.../2026-08-21-483-design-brief.md` (review history).

## 1. Problem

Two per-node auth caches hold cluster-relevant state and never reconcile
against their authoritative KV store:

- **A. Trusted keys** (`internal/auth/kv_trusted_store.go`): cache
  populated once at construction; `Delete`/`Invalidate` mutate KV + the
  local map only; **no broadcast topic exists**; `ListForVerification` —
  the token-exchange verification path — reads only the local map.
  Revoking a compromised key on one node leaves every other node accepting
  it until that node restarts. Fail-open; needs nothing to go wrong.
- **B. OIDC providers** (`internal/auth/oidc/registry.go`): cluster
  propagation exists but is fire-and-forget gossip with no backstop; a
  dropped `invalidate` leaves a peer serving a deleted/deactivated
  provider's keys until restart.

Multi-node is the primary operating target; both halves are assessed on
cluster reasoning.

## 2. Architecture

**Broadcast fast path + periodic jittered KV-reconcile backstop +
fail-closed staleness bound** — the model-cache layering
(`internal/cluster/modelcache`: gossip + jittered TTL as dropped-message
fallback), adapted to enumeration-style caches. One mechanism, applied to
both caches.

Rejected alternatives:

- **Per-entry TTL lease** — structurally wrong here: verification
  *enumerates* these caches (`ListForVerification`, `ResolveKey`'s
  candidate walk), so an expired entry has no read-through fallthrough,
  and deletions must be noticed, not merely expired.
- **Read-through on verification** — a KV round-trip on every token
  verification; hot-path cost with no precedent.
- **Backstop-only (no trusted-key gossip)** — leaves registration and
  revocation propagating at reconcile granularity (~66 s worst case) with
  no fast path; `multi-node-primary.md` forbids settling for that on
  proportionality grounds, and the backstop removes the issue's stated
  objection to trusted-key gossip ("only worth it combined with a
  backstop").

## 3. Trusted keys — `KVTrustedKeyStore`

### 3.1 Reconcile

New `reconcile(ctx) error`:

1. Snapshot the **generation counter** (new field, see below).
2. KV `List` the `trusted-keys` namespace and build a fresh
   `map[kid]*TrustedKey` — **off-lock** (no KV I/O ever under `s.mu`).
3. Under `s.mu`: if the generation changed since the snapshot, release and
   retry from step 1 (bounded, 5 attempts); otherwise swap the map in and
   bump the generation (reconcile swaps count as mutations, so overlapping
   reconciles cannot commit snapshots out of order).

- **Serialization:** a dedicated `reconcileMu` serializes all reconcile
  executions (periodic tick, ping-triggered, any future explicit call).
  Together with the swap-bumps-generation rule this closes the
  reconcile-vs-reconcile resurrect race found in review round 2.
- **Generation counter:** bumped by every cache-mutating commit —
  `Register`, `Delete`, `Invalidate`, `Reactivate`, `loadOne`, and the
  reconcile swap itself. All point mutations already hold `s.mu` for
  KV-write-then-cache-commit, so mutation-vs-reconcile interleavings are
  fully covered (verified in review round 2).
- **Retry exhaustion** (only reachable under continuous mutation churn,
  which itself proves KV is reachable): WARN, do not swap; does **not**
  count toward the staleness breaker and does not update
  `lastSuccessfulReconcile`.
- **I/O context:** each attempt uses `context.WithTimeout(s.ctx, interval)`
  — never the raw deadline-free store ctx. A hung KV call must abort the
  attempt, not wedge the loop into a permanent breaker trip that outlives
  KV recovery.
- **Error taxonomy:** transport-level List failure → abort tick, keep
  last-known state, retry next tick. Per-record deserialize failure →
  **skip that record, log ERROR, reconcile the rest** (matches
  `KVOidcProviderStore.LoadAll`). A deterministic corrupt record must not
  disable reconciliation forever. Construction-time `loadAll` keeps its
  fail-fast.

### 3.2 Broadcast fast path

New gossip topic `auth.trustedkeys`:

- Every successful mutation (`Register`, `Delete`, `Invalidate`,
  `Reactivate`) publishes a **payload-free "changed" ping** — no key
  material, no tenant data, nothing injectable on the wire.
- Receive handler: runs on the broadcaster's shared receive goroutine, so
  it is non-blocking and wrapped in the same `recover()` layer as the OIDC
  handler; it **ignores the payload entirely and never logs it** (arbitrary
  peer bytes). It dispatches a reconcile through a **coalescing** gate: if
  a reconcile is in flight, set a dirty flag and run exactly one trailing
  reconcile when it finishes (a ping arriving mid-List would otherwise be
  dropped by plain singleflight, downgrading revocation bursts to
  backstop latency).
- Ping spam from a compromised peer degrades to back-to-back serialized KV
  Lists — bounded, no amplification (fixed topic, fixed coalescing key).
- Broadcaster nil (single-node): no pings; the loop still runs.

### 3.3 Reconcile loop

`StartReconcileLoop(ctx, interval)`: goroutine; per-tick wait is
`interval × [0.9, 1.1)` (herd-avoidance, model-cache precedent);
`atomic.Bool` once-guard (second call returns false); exits on ctx cancel.
Shape mirrors `Registry.StartWarmupRetryLoop`.

### 3.4 Known bounded window (accepted)

`Get`'s read-through (`loadOne`) KV-reads outside the lock; a `Get` racing
a peer's `Delete` can re-insert the just-deleted key after the
ping-triggered reconcile swapped. The stale entry survives at most until
the **next periodic tick** (~1.1 × interval worst case); after that tick a
fresh `Get` hits post-delete KV → not-found, so no re-insert loop.
Documented, not eliminated.

## 4. OIDC providers — `oidc.Registry`

`ReloadAll` is already the reconcile primitive (off-lock rebuild from KV,
swap, carry over warm key sources, drop vanished providers; deactivation
propagates because `Active()` gates every read and `LoadAll` returns
inactive records). Changes:

- **Serialization + generation guard inside `ReloadAll`:** a dedicated
  reconcile mutex serializes all `ReloadAll` executions (periodic tick,
  `_reload_all` broadcast, admin endpoint via `ReloadAllAndWarm` — which
  keeps `reloadWarmMu` for its warm phase). A registry generation counter
  is bumped by every direct map mutation (`addToProviderMap`,
  `invalidateOne`) **and by every `ReloadAll` swap**; `ReloadAll`
  snapshots it before `store.LoadAll` and retries the build (bounded, 5)
  if it changed by swap time. Closes both the mutation-resurrect race and
  the round-2 reconcile-vs-reconcile race (endpoint reload's older
  snapshot swapping after a periodic tick's newer one).
- **Skip-swap on no-change:** if the loaded snapshot is equal to the
  current providers map — comparison is `reflect.DeepEqual` per
  (tenant, uri) entry; false-negatives (e.g. monotonic-clock/Location
  differences on a locally-registered provider's first tick) are
  acceptable and merely cause one extra swap — skip the swap: `kidIndex`
  and sources untouched on quiet ticks, zero cold-path churn. A skipped
  swap still: updates `lastSuccessfulReconcile`, refreshes the provider
  metric, and **prunes orphaned sources** (sources whose (tenant, uri) is
  absent from the providers map — a `reloadOne` install racing an earlier
  swap can strand one; pre-fix the next always-swap tick collected it,
  with skip-swap the prune replaces that).
- **Reconcile loop:** `Registry.StartReconcileLoop(ctx, interval)` — same
  loop shape as §3.3, calling `ReloadAll` (NOT `ReloadAllAndWarm`: no
  per-tick outbound IdP traffic; JWKS key freshness stays with the
  5-minute per-source cache). Errors: WARN, keep state, next tick. I/O is
  bounded by a per-attempt `context.WithTimeout` (interval).
- **Composition property (tested):** reconcile ∘ warmup-retry closes the
  registration direction end-to-end — the reconcile discovers a provider
  registered on a peer (dropped `reload` ping), and `StartWarmupRetryLoop`
  (30 s tick, `coldActiveRefs`) warms it without any broadcast received.
- Existing `oidc.providers` gossip is unchanged.

## 5. Fail-closed staleness bound

Keeping last-known state on reconcile failure is correct for transient
blips, but unbounded staleness is a wrong-but-available answer
(`correctness-over-availability.md`); the model cache itself fails closed
once its lease expires.

- Both caches track `lastSuccessfulReconcile` (initialized by the
  construction-time load; a skipped OIDC swap counts as success).
- **The bound is enforced only once the reconcile loop has started**
  (loop-started flag) — a store constructed without a loop (tests, future
  callers) must not fail all federated auth after 10 min of healthy KV.
- If `now − lastSuccessfulReconcile > 10 × interval` (default 10 min):
  - `KVTrustedKeyStore.ListForVerification` returns an **empty slice**
    (interface unchanged — it has no error return; the in-memory
    implementation is trivially fresh and never trips). Consumer effect:
    token-exchange / JWT-bearer grants fail with the **existing
    400 `invalid_grant`** surface.
  - `KVTrustedKeyStore.Get` does **not** fail closed: when stale it
    bypasses the cache-hit path and forces `loadOne` read-through, so
    admin flows (`trusted_adapter`) keep working against KV ground truth
    or fail with a genuine KV error — never a synthetic 500.
  - `oidc.Registry.ResolveKey` returns an error → the validator chain
    hard-fails → **existing uniform 401** via `DelegatingAuthenticator`.
  - First-party JWTs are unaffected (in-process local key source, no KV
    dependency). Blast radius = exactly the KV-dependent federated
    surface.
- Escalation before the bound: WARN per failed tick; ERROR from the 4th
  consecutive failure; metrics for consecutive-failure count and
  seconds-since-last-success.
- The bound is a fixed 10× multiple of the interval — one knob, not two.
- Accepted residual: during a breaker trip the 400/401 responses reuse
  existing generic messages ("unknown trusted key" / uniform 401); the
  ERROR logs + metrics are the operator signal. No new error codes.

## 6. Configuration

| Env var | Type | Default | Notes |
|---|---|---|---|
| `CYODA_AUTH_CACHE_RECONCILE_INTERVAL` | duration | `60s` | Shared by both caches; jittered ±10% per tick; breaker fixed at 10×. Floor-validated: values < 1s rejected at startup (in `ValidateIAM`, checked unconditionally — a bad explicit value is a config error in any mode). No disable knob. |

Field: `IAMConfig.AuthCacheReconcileInterval`; help topic `auth`.

Why 60 s (not the model cache's 5 m): this bounds *revocation* staleness —
a security property, not a performance lease — and a tick costs one KV
List + one KV LoadAll per node with zero outbound traffic. With the ping
fast path, the loop is purely the dropped-message backstop.

## 7. Wiring (`app/app.go`, jwt-mode block)

- After `NewKVTrustedKeyStore`: subscribe the store's ping handler to the
  broadcaster (nil-safe) and start
  `trustedKeyStore.StartReconcileLoop(systemCtx, cfg.IAM.AuthCacheReconcileInterval)`.
- Inside `pendingWarmJWKS` (alongside `StartWarmupRetryLoop`):
  `oidcRegistry.StartReconcileLoop(systemCtx, ...)`.
- `systemCtx` is background-derived and never cancelled → loops run to
  process exit, the same lifecycle as `StartWarmupRetryLoop`.
- Loops run in single-node mode too (KV-dependent, not gossip-dependent).
- Mock IAM mode: neither store exists; nothing to start.

## 8. Affected response surfaces (no API change)

No new endpoints, parameters, or error codes. Existing statuses during a
staleness-breaker trip (>10 min of failed reconciles):

| Surface | Path | Status during trip | Mechanism |
|---|---|---|---|
| OIDC bearer token | any authenticated endpoint | 401 (existing uniform) | `ResolveKey` error → chain hard-fail |
| Token-exchange / JWT-bearer grant | `POST /oauth/token` | 400 `invalid_grant` (existing) | `ListForVerification` → empty |
| First-party JWT | any | unaffected | local key source, no KV |
| Trusted-key admin CRUD | `/oauth/keys/trusted/*` | unaffected | `Get` forces KV read-through |
| OIDC provider admin CRUD | provider endpoints | unaffected | store-backed, not registry-cached |

Gate-7: bug fix conforming to the existing contract — no cloud-parity doc.

## 9. Documentation (Gate 4)

- `docs/ARCHITECTURE.md` §7.2: rewrite the trusted-key cache paragraph —
  including correcting its existing false claim that a key registered on
  node A is invisible to node B "until B restarts" (`Get` has KV
  read-through; only the enumeration paths were restart-bound) — to
  describe ping + reconcile + staleness bound. §7.3 final paragraph:
  replace "no TTL lease behind the broadcast" with the backstop + bound.
  Both state that the staleness bound is conditional on KV health and that
  exceeding it fails closed.
- README env-var table; `cmd/cyoda/help/config_registry.go` +
  `cmd/cyoda/help/content/config/auth.md`; `DefaultConfig()`.
- CHANGELOG handled by the release-milestone convention (PR milestoned to
  v0.8.4).

## 10. Test plan / coverage matrix

All scenarios are unit-layer (no HTTP/gRPC surface change; cluster
semantics exercised at the two-stores-over-one-KV seam, the same seam the
existing multi-node trusted-key test uses). Concurrency tests are isolated
single-package tests inside the `make race` scope, never parity.

| # | Scenario | Layer / package |
|---|---|---|
| T1 | Delete / Invalidate / Register on store 1 → `reconcile()` on store 2 → `ListForVerification` / `Get` / `List` reflect it | unit `internal/auth` |
| T2 | Per-record corruption skipped with ERROR, rest reconciled; transport failure keeps prior state and returns error | unit `internal/auth` |
| T3 | Generation guard: mutation injected between List and swap (fake-KV hook) is not clobbered; bounded-retry exhaustion does not swap and does not touch breaker accounting | unit `internal/auth` |
| T4 | Reconcile-vs-reconcile: two overlapping reconciles (and: reconcile racing ping-handler dispatch) — later-started-earlier-snapshot never overwrites newer state | unit `internal/auth` (race scope) |
| T5 | Ping: mutation publishes on `auth.trustedkeys`; receiver reconciles; ping during in-flight reconcile coalesces to exactly one trailing run; malformed/oversized payload ignored, never logged | unit `internal/auth` |
| T6 | Staleness breaker: loop running + reconcile failing past 10× → `ListForVerification` empty, `Get` forces read-through (still serves KV truth); recovery restores; breaker inert when loop never started | unit `internal/auth` |
| T7 | Loop: short-interval eventually-converges (coarse, generous timeouts); once-guard; ctx-cancel stops | unit `internal/auth` |
| T8 | OIDC: store delete / deactivate with no broadcast → loop → `ResolveKey` stops resolving; surviving provider's carried-over source resolves with no re-fetch | unit `internal/auth/oidc` |
| T9 | OIDC skip-swap: identical snapshot keeps kidIndex + sources, still updates `lastSuccessfulReconcile` + metric; orphaned source pruned | unit `internal/auth/oidc` |
| T10 | OIDC generation guard under fault-KV; reconcile-vs-`ReloadAllAndWarm` (endpoint) race resurrects nothing | unit `internal/auth/oidc` (race scope) |
| T11 | Composition: provider registered on peer (no broadcast) → reconcile discovers → warmup-retry warms → `ResolveKey` succeeds | unit `internal/auth/oidc` |
| T12 | OIDC staleness breaker: `ResolveKey` fails closed past bound, recovers after successful reconcile | unit `internal/auth/oidc` |
| T13 | Config: default 60s; floor <1s rejected; wiring starts both loops in jwt mode, none in mock | unit `app` |

## 11. Settled decisions (do not re-litigate in implementation)

1. Reconcile backstop + ping fast path for **both** caches; no per-entry
   TTL; no read-through verification.
2. One shared interval env var, default 60 s, breaker fixed at 10×.
3. Breaker semantics per §5, including `Get` = read-through (never fail
   closed) and `ListForVerification` = empty slice (interface unchanged).
4. Trusted-key ping is payload-free; handler never reads or logs payload.
5. OIDC periodic path calls `ReloadAll`, never `ReloadAllAndWarm`.
6. Error taxonomy: skip corrupt records at reconcile time; abort tick on
   transport failure; keep fail-fast at construction.
7. No new error codes, endpoints, or wire surfaces beyond the ping topic.
