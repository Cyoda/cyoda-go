# Research: auth-cache reconciliation (#483)

Factual survey of the code paths involved, written before the design. All line
numbers as of `release/v0.8.4` (c1cd595).

## A. Trusted keys — `internal/auth/kv_trusted_store.go`

- `KVTrustedKeyStore` holds `keys map[string]*TrustedKey` (keyed by KID, all
  tenants in one map) guarded by `sync.RWMutex`. KV layout:
  namespace `trusted-keys`, key `<tenantID>:<kid>`.
- Populated once by `loadAll()` at construction (`NewKVTrustedKeyStore`).
  `loadAll` lists the whole namespace, skips legacy un-tenanted entries,
  errors out on deserialization failure (construction-time fail-fast).
- Mutations (`Register`, `Delete`, `Invalidate`, `Reactivate`) hold `s.mu`
  for their whole body: KV write first, cache commit second, all under the
  same exclusive lock. So **all cache mutations are serialized by `s.mu`**.
- Read paths:
  - `Get(tenant, kid)` — cache hit, else **read-through** `loadOne` from KV.
    Registration on another node is therefore visible to `Get` immediately.
    A *deletion* on another node is NOT: the stale entry remains and hits.
  - `List(tenant)` — cache only.
  - `ListForVerification()` — cache only, filters `ValidTo`. This is the
    token-verification path: `internal/auth/verification.go:12`
    (`getTrustedKeyByKID`, used by token.go JWT-bearer/token-exchange) and
    `internal/auth/jwks.go:42` (JWKS endpoint) iterate it.
- **No broadcast topic exists for trusted keys** (grep confirms: no
  broadcaster reference anywhere in `internal/auth/` outside oidc/).
- No TTL, no ticker, no reconcile. Cross-node convergence = restart only.
- `InMemoryTrustedKeyStore` (`internal/auth/store.go`) is the non-persistent
  variant used in tests/dev; single-node by nature, out of scope.
- The store has no injected clock (`time.Now()` used directly).

## B. OIDC providers — `internal/auth/oidc/`

- `Registry` (`registry.go`) holds `providers`/`sources` nested maps
  (tenant → wellKnownConfigUri → value) + `kidIndex`, all under one RWMutex.
- `ReloadAll(ctx)` (`registry.go:488`) is a **complete reconcile primitive
  that already exists**: `store.LoadAll` from KV, rebuild maps off-lock,
  swap under write lock, carry over key sources for still-present providers
  (a reload is never a JWKS cache flush), drop sources for vanished
  providers, reset `kidIndex` (rebuilt lazily by ResolveKey cold path).
  Deactivated providers survive in the map but `prov.Active()` gates every
  read path, so deactivation propagates through a plain `ReloadAll` too.
- `ReloadAllAndWarm` = ReloadAll + synchronous discovery+JWKS re-fetch for
  every active provider (outbound traffic to every tenant's IdP), serialized
  by `reloadWarmMu`. Used by the admin reload endpoint and the `reload_all`
  broadcast op.
- Cluster propagation (`broadcast.go`): topic `oidc.providers`, ops
  `reload` / `invalidate` / `reload_all`, sent fire-and-forget from Service
  write paths. Receive path dispatches via singleflight debouncer.
- The broadcaster contract (`internal/cluster/registry/gossip_broadcast.go:29`)
  is explicitly best-effort: no ordering, no persistence, drops possible.
- **No ticker, no TTL, no periodic anything** in the registry — except
  `StartWarmupRetryLoop` (`registry.go:600`), which IS a periodic loop
  (default 30s tick, `atomic.Bool` call-once guard, ctx-cancelled), but it
  only re-warms *cold* active providers; it never re-reads KV. It is the
  in-package precedent for the loop shape.
- JWKS key freshness is separate: `NewHTTPJWKSSource(..., 5*time.Minute)`
  gives per-source key-cache TTL. Key rotation converges; provider
  *existence/activation* does not.

## C. The precedent — `internal/cluster/modelcache/`

- `CachingModelStore` (cache.go): per-entry lease, `expiresAt` checked on
  lookup, **±10% jittered TTL** (`jitteredLeaseLocked`, seeded PCG rand
  guarded by mu) documented as "the fallback when gossip drops a message"
  (factory.go:17-19). Injectable `Clock` interface for deterministic tests.
- Config: `CYODA_MODEL_CACHE_LEASE`, default 5m, `Config.ModelCacheLease`
  (app/config.go:41,245), help topic `server`.
- Crucial structural difference from the auth caches: the model cache is
  **read-through** — an expired entry falls through to the inner store on
  next Get. The trusted-key verification path (`ListForVerification`) and
  the OIDC resolve path enumerate the cache; there is no per-key fallthrough,
  and deletions must be *noticed*, not just expired. So a per-entry lease
  does not transplant; a periodic full reconcile against KV does.

## D. Wiring — `app/app.go` (jwt mode block, ~line 240-330)

- `systemCtx` = background ctx + system UserContext; used for KV stores,
  `NewKVTrustedKeyStore`, `LoadProvidersFromKV`, and passed to
  `WarmJWKS`/`StartWarmupRetryLoop` via `pendingWarmJWKS`. It is the
  process-lifetime ctx a reconcile loop would use.
- `gossipReg` may be nil (single-node); D7 invariant: cluster mode requires
  non-nil broadcaster, enforced at startup.
- Both stores are constructed only in `cfg.IAM.Mode == "jwt"`.

## E. Config / docs surface

- `IAMConfig` (app/config.go:111) holds trusted-key + OIDC settings;
  `OIDCConfig` nested. `DefaultConfig()` at :245ff.
- Help registry: `cmd/cyoda/help/config_registry.go` — trusted-key and IAM
  vars live under topic `auth`; `CYODA_MODEL_CACHE_LEASE` under `server`.
- `docs/ARCHITECTURE.md`:
  - §7.2 (line 1119): documents the boot-populated, never-propagated
    trusted-key cache as current behaviour.
  - §7.3 (last paragraph): documents "no TTL lease behind the broadcast;
    a peer that misses the message keeps serving its cached keys until the
    next explicit reload."
- No new error codes / endpoints are implied by any reconcile fix ⇒ no
  OpenAPI, no `errors/*.md`, no cloud-parity doc (bug fix to an existing
  contract, per project convention).

## F. Existing test fixtures

- `internal/auth/kv_trusted_store_test.go` uses an in-memory
  `spi.KeyValueStore` fake; two stores over one shared KV already simulate
  two nodes (that is how the read-through `Get` multi-node test works).
- `internal/auth/oidc/` has `fixture_test.go`, `fault_kv_test.go`,
  `reload_warmup_test.go` (exercises StartWarmupRetryLoop with short
  intervals), `kv_store_test.go`. `installForTest` injects providers.
- Race scope: `make race` covers `internal/auth/...` (not e2e).

## G. Constraints carried into the design

- Mutex discipline: every Lock immediately followed by defer Unlock; IIFE
  for early release (.claude/rules/go-mutex-discipline.md).
- Correctness-over-availability: no fallback values; but "keep serving the
  last KV-confirmed state when a periodic reconcile attempt fails" is the
  model-cache-consistent reading (entries survive within lease when gossip
  is silent), and strictly bounds the pre-fix behaviour (stale forever).
- Multi-node is primary; single-node (nil broadcaster) must still run the
  reconcile loops — they depend on KV, not on gossip.
- No issue IDs in shipped code/comments.
