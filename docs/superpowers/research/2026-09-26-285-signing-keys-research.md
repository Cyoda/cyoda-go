# #285 research — signing key pairs across the cluster

Facts the #285 design rests on, each checked in the code on 2026-09-26
(cyoda-go `release/v0.9.0` at `df6ad2c7`; SPI pin
`v0.8.5-0.20260927003224-1b0c3780762e`; Cloud checkout `~/dev/cyoda` with
`~/dev/cyoda-platform` at `e0a4dc1fe`; cassandra plugin at `494a334`).
"Not found" claims list the search that was run.

## R1. What a signing key pair is today

- `KeyPair` holds the private key in the clear: `PrivateKey *rsa.PrivateKey`
  (`internal/auth/store.go:22-31`).
- `NewAuthService` builds a fresh `NewInMemoryKeyStore()` on every node
  (`internal/auth/service.go:51`). Nothing else stores key pairs.
- The bootstrap key comes from `CYODA_JWT_SIGNING_KEY` (+`_FILE`), resolved in
  `app/config.go:344` via `envPEMFromSecret` (`app/config.go:506-517`). Its KID is
  the hex of the first 16 bytes of SHA-256 over the PKIX public key
  (`internal/auth/service.go:63-68`), so every node configured with the same key
  derives the same KID (`docs/ARCHITECTURE.md:1865-1873`). It has no window
  (`service.go:70-83`).
- Key pairs issued by `POST /oauth/keys/keypair` are generated in the handler with
  `rsa.GenerateKey(rand.Reader, 2048)` and a random 16-byte hex KID
  (`internal/domain/account/keys_adapter.go:96-111`), then `keyStore.Save`.
- Key pairs are global, not per tenant: `KeyPair` has no tenant field
  (`store.go:22-31`).

## R2. Who uses a key pair, and for what

| Use | Code | Needs private key? |
|---|---|---|
| Sign M2M token (`client_credentials`) | `token.go:83` `GetActive("client")`, `token.go:102` `Sign(claims, kp.PrivateKey, kp.KID)` | yes |
| Sign token-exchange token | `token.go:221`, `token.go:241` | yes |
| Verify any first-party token | `key_source.go:34-52` `localKeySource.GetKey` → `ks.Get(kid)`, rejects `!Active` and out-of-window | no (public key) |
| Publish JWKS | `jwks.go:42` `ListForVerification()` | no |
| Admin: issue, current, delete, invalidate, reactivate | `keys_adapter.go:113,150,166,200,244,248` | no |

- Only the `client` audience ever signs (`grep GetActive(` → `token.go:83,221`,
  `keys_adapter.go:150`). `human`-audience key pairs can be issued and verify,
  but nothing signs with them. Cloud is the same (Cloud research, §4).
- `Sign` takes `*rsa.PrivateKey` (`internal/auth/jwt.go:18`). The only other
  `.PrivateKey` reads are the two token sites above
  (`grep -rn "\.PrivateKey\b" internal app`).
- Every admin adapter maps every store error to `404 KEYPAIR_NOT_FOUND`
  (`keys_adapter.go:165-168,200-203,244-247`). With an in-memory store the only
  error is "not found"; with a persistent store a storage failure would read as
  404. The trusted-key store already fixed the same problem with
  `ErrTrustedKeyNotFound` (`store.go:83-89`).
- Admin endpoints are gated by `RequireAdmin`: `ROLE_ADMIN` in any tenant
  (`internal/auth/admin_guard.go:22-36`). There is no platform-wide admin role:
  `grep -rn "ROLE_SUPER\|SUPER_USER\|RequireSystemAdmin" internal app` → nothing.
  So an admin of any tenant can issue, invalidate or delete the key pairs that
  sign every tenant's tokens. The #281 spec chose ROLE_ADMIN only
  (`docs/superpowers/specs/2026-06-04-281-oauth-keys-openapi-design.md:59,102`).

## R3. The defect #285 now owns (issue comment of 2026-09-24)

- A key pair issued on node A exists only on A. `GetActive` picks the newest key,
  so A signs with a KID the other nodes do not have, and they answer `401`.
- The HTTP cluster proxy forwards only requests that carry a transaction token
  (`internal/cluster/proxy/http.go:41-45`), so the key-pair admin calls act on
  whichever node receives them. Invalidate and reactivate change one node only.
- A restart drops every issued key pair. Also (not in the issue comment): a
  restart **brings back** a bootstrap key an admin had invalidated or deleted,
  because the bootstrap key is re-created active from configuration
  (`service.go:75-85`) and its revocation was only in memory.
- The help topic tells operators not to manage key pairs in cluster mode
  (`cmd/cyoda/help/content/config/auth.md:246-252`).

## R4. The pattern already used for shared auth state: trusted keys

`KVTrustedKeyStore` (`internal/auth/kv_trusted_store.go`):

- Stores records in the SYSTEM-tenant KV (`app/app.go:272-285`), namespace
  `trusted-keys`, key `<tenant>:<kid>` (`kv_trusted_store.go:23,94-99`).
- Keeps an in-memory copy (the cache). A write goes to KV first, then to the
  cache (`:444-450`, `:486-495`).
- After a write it broadcasts an empty message on gossip topic
  `auth.trustedkeys` (`:29`, `:414-421`). A node that receives it re-reads the
  whole namespace from KV (`:338-345`); triggers that arrive during a re-read are
  folded into one more re-read, never dropped (`internal/auth/coalesce.go:5-62`).
- Every `CYODA_AUTH_CACHE_RECONCILE_INTERVAL` (default 60 s, ±10 % jitter) each
  node re-reads the namespace anyway, to repair a lost gossip message
  (`:369-391`).
- If the re-read has failed for more than 10 intervals, verification refuses
  every key (fail closed) (`:222-226`, `:396-406`, `:571-578`).
- A read of one key that misses the cache reads KV (`Get`, `:520-565`).
- Metrics: two OTel gauges named `auth.trustedkeys.*`
  (`internal/auth/reconcile_metrics.go:33-41`).

The OIDC provider registry has the same broad shape but separate code
(own loop, own jitter, own staleness constant, a drop-style debouncer):
`internal/auth/oidc/registry.go:232,578-697,818-851`,
`internal/auth/oidc/singleflight.go:5-40`. Nothing is shared between the two.

## R5. The KV store (the only shared storage auth can use today)

- SPI interface: `Put`, `Get`, `Delete`, `List(namespace)`
  (`cyoda-go-spi persistence.go:545-550`). No conditional write, no version, no
  TTL. `Get` of a missing key wraps `spi.ErrNotFound` (`spitest/keyvalue.go:31-36`).
- Scoped to the tenant of the ctx given to `StoreFactory.KeyValueStore`
  (memory `plugins/memory/store_factory.go:191-224`, postgres
  `plugins/postgres/store_factory.go:111-120,230-236`, sqlite
  `plugins/sqlite/store_factory.go:341-374`).
- memory: process maps only; nothing survives a restart; not shared between
  processes (`plugins/memory/kv_store.go:15-66`).
- sqlite: `INSERT OR REPLACE` into `kv_store` (`plugins/sqlite/kv_store.go:16-24`).
- postgres: upsert into `kv_store` with row-level security on the tenant
  (`plugins/postgres/kv_store.go:18-28`,
  `plugins/postgres/migrations/000001_initial_schema.up.sql:71-77,150-152`).
- cassandra: tables `data_store`, `data_store_meta`, `data_store_listing`,
  tenant in every partition key; session consistency (default QUORUM) for all
  KV reads and writes; two concurrent `Put`s to the **same key** can lose one
  update (`cyoda-go-cassandra internal/store/data_store.go:40-48,65-122`);
  `List` costs 1 + 2N reads (`:210-238`). No encryption.
- No backend encrypts KV values. No backend enforces a value-size limit.

## R6. Cryptography already in cyoda-go

- Dispatch hand-overs: AES-256-GCM with a key from
  `HKDF-SHA256(CYODA_HMAC_SECRET, salt=nil, info="cyoda-dispatch-v1")`
  (`internal/cluster/dispatch/aead_peer_auth.go:72,142-149,174-183`). The helper
  is unexported with a fixed label.
- Tx-routing tokens: HMAC-SHA256 keyed on the raw `CYODA_HMAC_SECRET`
  (`internal/cluster/token/token.go:44-49,120-123`).
- Gossip: memberlist AES keyed on the raw `CYODA_HMAC_SECRET`
  (`app/app.go:1212`, `internal/cluster/registry/gossip.go:136`).
- `CYODA_HMAC_SECRET` is required in cluster mode (`app/app.go:1146-1158`). In
  single-node mode it is not used; each process generates 32 random bytes
  (`app/app.go:157-175`). So it is **not** a stable secret on a single node.
- `CYODA_JWT_SIGNING_KEY` is required in jwt mode (`app/app.go:264-270`) on
  every node, single or cluster, and must be the same on every node for tokens to
  verify cluster-wide (R1).
- No at-rest encryption of any stored value exists
  (`grep -rn "cipher.NewGCM\|aes.NewCipher\|chacha20poly1305\|secretbox" --include='*.go'`
  outside tests → only the dispatch file).

## R7. Gossip

- Best-effort, unordered, no persistence (`cyoda-go-spi cluster.go:3-19`).
- Encrypted and authenticated with the cluster secret; no per-sender identity.
- Practical message size about 1400 bytes (memberlist UDP budget). An empty ping
  fits; a key record would not be a good fit.
- Topics in use: `model.invalidate`, `auth.trustedkeys`, `oidc.providers`,
  `cluster.tags`, `cluster.tags.request`.

## R8. How Cyoda Cloud does it

Source: `~/dev/cyoda` and `~/dev/cyoda-platform`. "[code]" = seen in code.

- Issued key pairs are rows of `JWKEntity` in Cloud's Cassandra store
  (`platform-service-iam/.../JWKEntity.kt:25-29`); the private key is a separate
  blob (`CyodaBlobEntity`), PKCS#8 DER (`StoredJWKService.kt:796-821`) [code].
- The blob passes through a pluggable encryptor hook whose default returns its
  input unchanged (`EncryptorForTypeCodecHolder.java:14,32-46`). No encryptor is
  wired in Cloud; the only implementation is a sample app. So in Cloud the private
  key is stored **unencrypted**, although its content type says
  `application/pkcs8-encrypted` [code + inferred from the missing bean].
- The platform library has an envelope design (data key + master key, with the
  master key's id stored in each value so old master keys can still decrypt,
  `DefaultEncryptorImpl.java:18-49,70-110`) but nothing implements its master-key
  provider [code].
- Each node loads the private key into memory and signs locally (jjwt
  `signWith(privateKey)`, `JwtTokenFactory.java:165-206`). No remote-sign
  abstraction [code].
- Cross-node: a per-node cache that loads from the database on a miss, plus a
  fire-and-forget cache-invalidation message on invalidate / reactivate / delete
  (`StoredJWKService.kt:257-288,598-605`; `RemoteNodesCacheCtrl.java:37-49`).
  Issuing a key pair without `invalidateCurrent` sends no message, so other nodes
  keep signing with their cached key [code]. No periodic re-read [not found].
- The configured (keystore) key is never stored; its KID is a configured
  literal, not derived (`JwtLocalFileSecretService.java:138-146`) [code].
- Issued keys survive restart (database rows) [code].
- Global, not per tenant (`StoredJWKService.kt:373-378`) [code].
- Who may manage them: no role check in the controller, interactor or service
  (`OAuthKeysController.kt:37-71`, `JwtKeyPairInteractor.kt:38-77`); the only
  guard is the access-rule tree, which gives `ADMIN` full access with no owner or
  tenant condition (`DecisionTreeConfig.kt:36-107`, `adminUser` at `:43-44`). So a
  plain tenant ADMIN can issue, invalidate, reactivate and delete the global key
  pairs [inferred from code; no test covers it]. `SUPER_USER` exists
  (`CyodaSecurityManagerExtension.kt:30-34`) but is not tied to any tenant and is
  not required here. Trusted keys and M2M technical users are tenant-scoped in
  Cloud (`TrustedKeyRegistrationService.kt:233-259`,
  `TechnicalUserService.kt:195-222,441-457`) [code].
- Cloud has no `/.well-known/jwks.json` [not found: grep for `jwks`,
  `well-known/jwks`, `JWKSet` across both repos and `~/dev/api-specification`].
- Incidental: Cloud's `helm/volumes/tmp/config/*/common/security.properties`
  holds keystore passwords in plaintext in the repo, next to a
  `cyoda-http-api.keystore` file [code].

## R9. Tests and fixtures

- Key-pair E2E tests are single-node: `internal/e2e/oauth_keys_test.go:85-296`,
  `internal/e2e/keys_trusted_reconciliation_test.go:19-90`.
- Multi-node parity scenarios run on postgres (in-tree) and cassandra (plugin):
  `e2e/parity/multinode/registry.go:1-45`; fixture
  `e2e/parity/postgres/multinode_fixture.go:27,119-177`. The fixture can kill a
  node but cannot restart one into the cluster
  (`e2e/parity/fixtureutil/fixtureutil.go:717-724`).
- A single node can be restarted on the same postgres in-process:
  `newStackOn` (`internal/e2e/scheduler_harness_test.go:106-117`) and
  `newStandaloneApp` (`internal/e2e/async_stream_test.go:1150,690-693`).
- No multi-node test of any auth store exists
  (`grep -rln "trusted\|keypair" e2e/parity/multinode` → nothing).
