# Signing key pairs shared by the cluster — design (#285)

Facts: `docs/superpowers/research/2026-09-26-285-signing-keys-research.md`
(cited "R§n"). Companion issue: #624 (platform-operator role), which ships in the
same release.

## 1. Summary

Signing key pairs are stored in the SYSTEM-tenant KV store, with the private key
sealed under a wrapping key derived from the bootstrap key. Every node keeps an
in-memory copy that follows the store through a gossip change message and a
periodic re-read. Admin changes read the store, not the copy. The bootstrap key's
revocation is stored the same way, so it is cluster-wide and survives a restart.
The replication code the trusted-key store uses becomes one shared component, and
both stores use it.

## 2. Terms

- **Key pair**: an RSA private key that signs tokens and its public key that
  verifies them, with an id (KID), an audience (`client` | `human`), a validity
  window (`validFrom`, `validTo`) and an active flag.
- **Bootstrap key**: the key pair built from `CYODA_JWT_SIGNING_KEY` on every node.
  Its KID is derived from its public key (`internal/auth/service.go:63-68`), so it
  is the same on every node configured with the same key.
- **Issued key pair**: a key pair created by `POST /oauth/keys/keypair`.
- **KV store**: the SPI `KeyValueStore` of the SYSTEM tenant (R§5).
- **Wrapping key**: an AES-256 key used only to seal private keys at rest.
- **Key vault**: the component that creates and opens private keys. Nothing
  outside it holds private-key material; it hands out a `Signer`.
- **Owner**: the value a vault writes into each record it seals and uses to decide
  whether it can open a record. For the wrapped vault: the bootstrap KID.
- **Node copy**: a node's in-memory copy of a store's records. All hot paths —
  signing, verification, JWKS, `GET current` — read only the node copy.
- **Re-read**: rebuilding the node copy from a KV `List` of the namespace.

## 3. Requirements

1. A key pair issued, invalidated, reactivated or deleted on any node takes effect
   on every node within the bound in §6.
2. Issued key pairs and all changes to them survive a restart on a persistent
   backend (sqlite, postgres, cassandra). The memory backend persists nothing.
3. Invalidating, reactivating or deleting the bootstrap key takes effect on every
   node and survives a restart.
4. Private keys are never stored unsealed.
5. A key vault backed by a KMS, Vault or HSM can sign without the private key
   leaving it. This applies to issued key pairs; the bootstrap key remains a PEM
   from configuration (§5.9).
6. Unchanged: the bootstrap KID derivation, and the bootstrap key's default state
   (active, no window) while nobody has changed it through the API.
7. Correct on both entry points that verify first-party tokens: HTTP and gRPC
   (`internal/grpc/interceptor.go:56-83`).

## 4. Threat model

Sealing protects against **read** exposure of the store: backups, replicas, a
read-only injection. **Write** access to the store is out of scope: it already
allows registering a trusted key or changing any stored data. The binding in §5.2
detects corruption and records mixed up with each other; it is not a defence
against a writer.

## 5. Design

### 5.1 Records

Namespace `signing-keys` in the SYSTEM-tenant KV store; KV key = KID; JSON.

**Issued record**

| Field | Content |
|---|---|
| `kind` | `"issued"` |
| `kid`, `audience`, `algorithm` | as today (`RS256` only) |
| `active`, `validFrom`, `validTo` | as today; RFC 3339 nano, UTC |
| `publicKey` | SPKI DER, base64 |
| `vault.kind` | `"wrapped"` |
| `vault.owner` | owner value (§2) |
| `vault.sealed` | base64 of the vault's sealed bytes |

**Bootstrap-state record**

| Field | Content |
|---|---|
| `kind` | `"bootstrap"` |
| `kid` | the bootstrap KID |
| `active`, `validFrom`, `validTo` | the state the API set |
| `deleted` | `true` once deleted; terminal |

No key material. Absent means the default state: active, zero `validFrom`, no
`validTo` (`service.go:70-85`). A bootstrap-state record is written when the
bootstrap key is invalidated, reactivated or deleted — directly, or as a sibling
in a rotation (§5.6).

### 5.2 The wrapped key vault

- **Wrapping key** = HKDF-SHA256(IKM, salt = none, info =
  `"cyoda-signing-key-wrap-v1"`), 32 bytes. IKM = the RSA primes of the parsed
  bootstrap key, sorted ascending, each left-padded to the byte length of the
  modulus, concatenated. The primes do not depend on the key's encoding: PKCS#1
  or PKCS#8, inline or `_FILE` (`internal/auth/jwt.go:156-171`) give the same
  wrapping key. (The private exponent is not used: two different values are valid
  for the same key.)
- **Plaintext**: the private key as PKCS#8 DER.
- **Seal**: AES-256-GCM, random 12-byte nonce; sealed bytes = `nonce || ciphertext`.
- **Associated data**: the concatenation of these fields, each preceded by its
  length as a 4-byte big-endian integer: `"cyoda-signing-key-v1"`, KID, audience,
  algorithm, owner, SHA-256 of the SPKI DER. The mutable fields (active, window)
  are not bound.
- **Open** fails if decryption fails, if the key is not RSA, or if its public key
  differs from the record's `publicKey`.
- **Owner** = the bootstrap KID.
- A golden-vector test pins the wrapping key derived from a fixed PEM, and a
  sealed record produced from fixed inputs opens.

Rationale for deriving the wrapping key from the bootstrap key: no new secret;
every node already has it with the same value; no added exposure, because whoever
holds the bootstrap key can already sign tokens every node accepts.
`CYODA_HMAC_SECRET` is not an option: a single node generates it at random (R§6).

### 5.3 Key vault interface (package `internal/auth`)

```go
// Signer signs a SHA-256 digest with RSASSA-PKCS1-v1_5 (RS256).
type Signer interface {
    Public() crypto.PublicKey
    Sign(ctx context.Context, digest []byte) ([]byte, error)
}

// KeyMeta is what a vault binds to a sealed key.
type KeyMeta struct {
    KID, Audience, Algorithm, Owner string
    SPKI                            []byte // empty for Generate
}

type KeyVault interface {
    Kind() string
    Owner() string
    Generate(ctx context.Context, meta KeyMeta) (spki, sealed []byte, s Signer, err error)
    Open(ctx context.Context, meta KeyMeta, sealed []byte) (Signer, error)
}
```

- `jwt.Sign` takes a `Signer` (today `*rsa.PrivateKey`, `jwt.go:18`).
- The key-pair adapter no longer generates keys (`keys_adapter.go:96-111`); the
  store calls `Generate`.
- A node opens a record once and keeps the `Signer` until the record's KID or
  sealed bytes change.
- A KMS vault stores a key reference as `sealed`, uses its KMS scope as `Owner`,
  and checks the public key against `meta.SPKI` without a network call.
  AWS KMS (`MessageType=DIGEST`), GCP `AsymmetricSign` and Vault transit
  (`prehashed`) all sign a digest. Not built now.

### 5.4 Classification

Each record is classified when a node loads it:

| Class | Condition | Used for |
|---|---|---|
| **owned** | issued, known vault kind, owner = this vault's owner, opens | everything |
| **broken** | issued, owner = this vault's owner, does not open; or unknown vault kind | signer selection only (§5.7); never verifies |
| **retired** | issued, vault kind `wrapped`, another owner | nothing |
| **bootstrap state** | `kind = bootstrap`, KID = this node's bootstrap KID | applied to the configured bootstrap key |
| **foreign bootstrap state** | `kind = bootstrap`, another KID | nothing |

- Invalidate, reactivate and delete answer 404 for retired records and foreign
  bootstrap-state records. A node deployed with the wrong bootstrap key therefore
  cannot change the cluster's key pairs.
- Retired records stay in the store. They become owned again if the operator
  restores the bootstrap key that owns them.
- Logging: a WARN when the set of retired KIDs changes (count and KIDs); an ERROR
  when the set of broken KIDs changes (KIDs and the reason class: decryption,
  public-key mismatch, unknown vault kind). Not logged on every re-read.
- Never logged: sealed bytes, private keys, full records.

Consequences:
- Replacing `CYODA_JWT_SIGNING_KEY` retires every issued key pair, as it already
  ends every token the old bootstrap key signed. The new bootstrap key signs.
- A database copied to another environment: the source's key pairs are retired
  there.
- During a rolling replacement of the bootstrap key, JWKS differs between nodes
  with the old and the new configuration.

### 5.5 Replicated KV store component

The mechanism now inside `KVTrustedKeyStore` (`internal/auth/kv_trusted_store.go`)
becomes one component in `internal/auth`, used by the trusted-key store and the
signing-key store (and by M2M clients, #286, next). It owns:

- the node copy and its generation counter;
- the initial load at startup (a failed load fails startup, as today,
  `kv_trusted_store.go:189-191`);
- re-read on a gossip change message (coalesced, never dropped, `coalesce.go`),
  and periodically every `CYODA_AUTH_CACHE_RECONCILE_INTERVAL` ±10 % jitter;
- the generation-guarded swap (a re-read that overlapped a local change retries,
  at most 5 times, `kv_trusted_store.go:276-333`);
- the staleness bound: after 10 intervals without a successful re-read, `Stale()`
  is true (only once the loop has started);
- the change broadcast (payload empty, never read or logged by receivers);
- reconcile metrics, named per store: `auth.trustedkeys.reconcile_*` (unchanged)
  and `auth.signingkeys.reconcile_*`.

Each store supplies: the namespace, the gossip topic (`auth.trustedkeys`,
`auth.signingkeys`), a decode function from `(kvKey, bytes)` to `(copyKey,
record)`, and the metrics names. The copy key is the store's choice: the
trusted-key store keeps keying by bare KID, so its cross-tenant KID-collision
check (`kv_trusted_store.go:475`) is unchanged.

**Admin writes read the store, not the node copy.** Every admin mutation of both
stores reads the current records from KV — `Get` for one record, `List` for a
rotation or a cap count — decides, writes KV, applies the result to the node copy
with a generation bump, then broadcasts. This fixes, for trusted keys too:

- a node that has not yet received a delete writes the deleted record back on an
  invalidate or reactivate (today `kv_trusted_store.go:615,635,670` read the cache);
- a rotation misses a sibling issued on another node (today `:503`);
- a single-record load re-inserts a record a concurrent re-read removed (today
  `loadOne`, `:425-441`, commits without a generation check). The load becomes
  generation-guarded.

Two admin writes to the **same** record at the same moment remain last-writer-wins
(the KV store has no conditional write, R§5); a reactivate that read the record
before a concurrent delete can write it back. Admin-only, rare, documented.

The trusted-key store's verification path stays node-copy-only
(`kv_trusted_store.go:592-608`) and its admin `Get` keeps its read-through on a
miss, now generation-guarded.

### 5.6 Rotation (issue with `invalidateCurrent`)

The KV store has no transactions. Order, with compensation:

1. `Generate` the new key pair (no write).
2. `List` the namespace. Siblings = owned issued records and the configured
   bootstrap key (with its stored state) of the same audience whose window is
   still open, excluding the new KID. Retired and foreign records are ignored.
3. `Put` the new record.
4. `Put` each sibling with `active = false` and `validTo` = the grace expiry
   (`graceExpiry`, `store.go`). The bootstrap key as a sibling means writing its
   bootstrap-state record.
5. If any write in step 4 fails: restore every sibling already written to its
   previous bytes (`Delete` for a bootstrap-state record that was absent),
   `Delete` the new record, and return the error (5xx). If a compensating write
   fails too, log an ERROR naming every KID left changed, and return the error.
6. Apply to the node copy, broadcast, return 200.

An issue without `invalidateCurrent` is step 1, step 3 and step 6.

Rotation depends on `List` returning every record. On cassandra it currently
returns success with rows missing when a per-key read fails
(cyoda-go-cassandra#102); fixing #102 is a prerequisite of this change for that
backend.

### 5.7 Signing, verification, JWKS

- **Signer selection** (`GetActive` today): among owned and broken issued records
  and the bootstrap key, those of the audience that are active and inside their
  window; the latest `validFrom` wins, then the greater KID (unchanged rule). If
  the winner is broken, signing fails (5xx) and the log names the KID; another key
  is never chosen instead.
- **Verification** (`localKeySource.GetKey`): owned issued records and the
  bootstrap key, active and inside the window (unchanged rule). A KID not in the
  node copy is unknown; there is no KV read on the verification path.
- **JWKS**: owned issued records and the bootstrap key whose window has not ended
  (unchanged rule, `store.go` `ListForVerification`).
- **Stale**: signer selection and `GET current` fail with a store error;
  verification treats every issued KID and the bootstrap key as unknown (401,
  unchanged mapping, `delegating.go:48-74`); JWKS answers 503 with `Retry-After`
  (an empty 200 would make external verifiers drop their cached keys).

### 5.8 Bootstrap key

- Built from configuration on every node, as today. Its stored state (§5.1), if
  present, is applied after every load and re-read.
- Deleting it writes `deleted = true`. It then signs and verifies nowhere, is not
  in JWKS, and reactivate answers 404. No API call removes the record. Recovery:
  replace `CYODA_JWT_SIGNING_KEY`, which gives a new KID with no stored state.
- Invalidating it and later reactivating it gives it a window (reactivate requires
  `validTo`, `keys_adapter.go:207-236`), as today — now persisted.

### 5.9 KMS and the bootstrap key

`CYODA_JWT_SIGNING_KEY` stays a required PEM in jwt mode (`app/app.go:264-270`).
A deployment that wants no exportable signing key uses a KMS vault for issued key
pairs and deletes the bootstrap key through the API once an issued key pair signs.
Documented in the help topic; not built now.

### 5.10 Wiring and deletions

- `AuthConfig` receives the SYSTEM-tenant KV store, the broadcaster and the
  replication options; `NewAuthService` builds the signing-key store and the
  trusted-key store from them (`app/app.go:271-382` builds these today for trusted
  keys only).
- Deleted, with their tests moved to the KV-backed stores over an in-memory KV:
  `InMemoryKeyStore`, `InMemoryTrustedKeyStore`,
  `NewInMemoryTrustedKeyStoreWithCap`, and the in-memory fallback in
  `NewAuthService` (`service.go:51-57`). They are reachable in production only as
  that fallback, which `app` never takes.
- Exit checks: `grep -rn "InMemoryKeyStore\|InMemoryTrustedKeyStore" --include='*.go'`
  → empty; `grep -rn "\.PrivateKey\b" internal/auth internal/domain` → only the
  vault; `grep -rn "rsa.GenerateKey" internal --include='*.go' | grep -v _test`
  → only the wrapped vault.
- M2M clients stay per node until #286.

## 6. Timing bound

| Change | Node that took the call | Every other node |
|---|---|---|
| issue, rotate, invalidate, reactivate, delete (issued or bootstrap) | before the 2xx | when the gossip message arrives (normally well under 1 s); if it is lost, at the next re-read (≤ 1.1 × interval, 66 s by default) |
| any change, on a node that cannot read its database | — | the node refuses every issued key and the bootstrap key after 10 × interval (10 min by default) |

Consequences, documented: a token signed with a newly issued or reactivated key
can be refused by another node until that node has the change; after a rotation,
another node can sign with the old key until it has the change, and its tokens are
refused by the nodes that have it. An open gRPC stream is authenticated when it
starts and is not re-checked after a revocation (existing behaviour).

A synchronous "every node confirms" design is rejected: revocation must work while
a node is unreachable.

## 7. Errors

`ErrKeyPairNotFound` (like `ErrTrustedKeyNotFound`, `store.go:83-89`) → 404
`KEYPAIR_NOT_FOUND`. Every other store or vault error goes through
`common.Internal`: 503 `STORAGE_UNAVAILABLE` when the storage plugin marks the
error as unavailable — today only postgres does (`plugins/postgres/ceilings.go:119,166`,
`classifying_querier.go:157`) — otherwise 500 with a ticket UUID and a generic
message.

| Endpoint | Status | Code | Cause | New? |
|---|---|---|---|---|
| all five `/oauth/keys/keypair` endpoints | 401 | `UNAUTHORIZED` | not authenticated | no |
| | 403 | `FORBIDDEN` | not `ROLE_ADMIN` (#624 changes the role) | no |
| | 501 | `NOT_IMPLEMENTED` | not jwt IAM mode (`keys_adapter.go:23-30`) | no |
| | 503 | `STORAGE_UNAVAILABLE` | storage marked unavailable (postgres) | yes; add to OpenAPI |
| | 500 | — | any other store or vault failure | yes (was 404) |
| `POST /oauth/keys/keypair` | 200 | — | issued (and siblings invalidated) | stored now |
| | 400 | `BAD_REQUEST`, `UNSUPPORTED_ALGORITHM` | validation (unchanged) | no |
| | 500 / 503 | — | a write failed; compensated (§5.6) | yes |
| `GET /oauth/keys/keypair/current` | 200 | — | the selected signer | retired excluded |
| | 400 | `BAD_REQUEST` | invalid audience | no |
| | 404 | `KEYPAIR_NOT_FOUND` | no signer for the audience | no |
| | 500 | — | selected signer is broken; stale | yes |
| `POST .../{keyId}/invalidate` | 200 | — | invalidated | stored now |
| | 400 | `BAD_REQUEST` | grace out of range (unchanged) | no |
| | 404 | `KEYPAIR_NOT_FOUND` | unknown, retired, foreign bootstrap state, deleted bootstrap | yes |
| `POST .../{keyId}/reactivate` | 200 | — | reactivated | stored now |
| | 400 | `BAD_REQUEST` | window validation (unchanged) | no |
| | 404 | `KEYPAIR_NOT_FOUND` | unknown, retired, foreign bootstrap state, deleted bootstrap | yes |
| `DELETE .../{keyId}` | 200 | — | deleted (issued: record removed; bootstrap: `deleted = true`) | stored now |
| | 404 | `KEYPAIR_NOT_FOUND` | unknown, retired, foreign bootstrap state, deleted bootstrap | yes |
| `POST /oauth/token` | 500 | `server_error` | selected signer broken; stale; no signer; store failure | new causes |
| `GET /.well-known/jwks.json` | 200 | — | owned issued keys and the bootstrap key | retired excluded |
| | 503 | — | stale, with `Retry-After` | yes; add to OpenAPI |
| JWT-authenticated HTTP call | 401 | `UNAUTHORIZED` | KID unknown here, inactive, out of window, or stale | new causes |
| gRPC call or stream start | `Unauthenticated` | — | same | new causes |

## 8. Tests

### 8.1 Fixture constraints

- The shared multi-node parity cluster signs its own tokens with the bootstrap key
  (`e2e/parity/fixtureutil/fixtureutil.go:190`) and runs every registered scenario
  (`e2e/parity/postgres/multinode_test.go:20-26`). Scenarios that invalidate or
  delete the bootstrap key, or use `invalidateCurrent` on `client`, run in their
  own cluster (`MustSetupMultiNodeWithEnv`, short
  `CYODA_AUTH_CACHE_RECONCILE_INTERVAL`), postgres only. Cassandra does not run
  those; recorded as a gap.
- `MultiNodeFixture` exposes no gRPC endpoint (`e2e/parity/multinode/fixture.go:14-33`).
  gRPC is covered in single-node `internal/e2e`; it uses the same authenticator.
- M2M clients are per node until #286: a scenario creates its M2M client on the
  node whose `/oauth/token` it calls.
- The single-node parity server is shared by all scenarios: key-pair scenarios
  there use audience `human` (nothing signs with it, `token.go:83,221`), never
  `invalidateCurrent`, and delete their key pairs in `t.Cleanup`.
- In-process e2e stacks each generate a fresh bootstrap key
  (`internal/e2e/callback_harness_test.go:190-205`): restart tests get a harness
  option to reuse one PEM.
- Admin tokens in multi-node tests come from one helper, so #624 changes one place.

### 8.2 Coverage matrix

| Scenario | unit | e2e (postgres, in-process) | parity single-node (memory, sqlite, postgres) | multi-node (postgres; cassandra via registry) |
|---|---|---|---|---|
| seal/open round trip; each associated-data field binds; public-key check; golden vector | ✓ | | | |
| classification: owned, broken (each reason), retired, foreign bootstrap state | ✓ | | | |
| lifecycle 200s: issue, current, invalidate, reactivate, delete | | ✓ | ✓ (`human`) | |
| every admin endpoint: store error → 500, marked-unavailable error → 503, not 404 | ✓ (faulty KV) | | | |
| 401 / 403 / 501 on every admin endpoint | | ✓ (existing) | | |
| rotation compensation: sibling write fails → new record deleted, siblings restored, 5xx | ✓ (faulty KV) | | | |
| retired: 404 on invalidate / reactivate / delete; not in JWKS or current | ✓ | ✓ (restart with another PEM) | | |
| broken signer → `/oauth/token` 500 and `GET current` 500 | ✓ | ✓ | | |
| bootstrap state: absent → default; applied after re-read; deleted is terminal | ✓ | | | |
| admin writes read KV: stale copy cannot bring back a deleted record; rotation sees a sibling issued elsewhere — both stores | ✓ | | | |
| generation-guarded single-record load (trusted keys) | ✓ | | | |
| JWKS 503 when stale; verification refuses when stale | ✓ | | | |
| gRPC: token from an issued key accepted; invalidated → `Unauthenticated` | | ✓ | | |
| issue on A → B verifies after the change arrives; B signs with it; JWKS on B | | | | ✓ registry |
| invalidate / reactivate / delete an issued pair on A → effect on B | | | | ✓ registry |
| rotate with `invalidateCurrent` on A → B refuses the old key | | | | ✓ own cluster |
| invalidate and delete bootstrap on A → refused on B; reactivate after delete → 404 | | | | ✓ own cluster |
| lost gossip repaired by the re-read (`SignalNode` SIGSTOP B, change on A, SIGCONT) | ✓ | | | ✓ own cluster |
| stale fails closed (`PauseDatabase`) | ✓ | | | ✓ own cluster |
| restart keeps an issued pair, a bootstrap invalidation and a bootstrap delete | | ✓ | | |
| restart with another bootstrap key retires issued pairs; the new key signs | | ✓ | | |
| existing trusted-key unit and e2e tests pass unchanged | ✓ | ✓ | | |

Waiver: 503 is not tested end to end on the key-pair endpoints. The adapters pass
errors through `common.Internal`, whose 503 mapping is already tested; the unit
rows assert the adapters reach it.

## 9. Documentation and parity

- `cmd/cyoda/help/content/config/auth.md`: §"JWT signing keypair rotation"
  (`:224-252`) — remove both limitations; describe sharing, persistence, the §6
  bound, retire-on-replacement, recovery from a broken signer and from a deleted
  or invalidated bootstrap key (an unexpired token, an admin from a federated OIDC
  provider — subject to #624 — or replacing the key), and the KMS path (§5.9).
  `:180-185` — the bootstrap key has no window unless the API gave it one.
  §"Auth cache reconciliation" (`:187-197`) — three caches, and the fail-closed
  effect on first-party tokens.
- `README.md:169` and `cmd/cyoda/help/config_registry.go:97,99`: same corrections.
- OpenAPI: 503 on the five key-pair endpoints and on JWKS; `go generate ./api`.
- `docs/ARCHITECTURE.md` §7.2 (`:1846-1873`), including `:1863`, which says
  first-party validation has no KV dependency.
- `CHANGELOG.md`.
- `docs/cloud-parity/signing-key-pairs.md`: key pairs shared and persisted,
  bootstrap revocation persisted, retire-on-replacement, 404 for retired, 503 on
  the endpoints and JWKS. Matching CaaS ticket.
- No new environment variable, no new error code.

## 10. Out of scope

- The platform-operator role: #624, same release.
- M2M clients shared by the cluster: #286, next, on the §5.5 component.
- The OIDC provider registry's own re-read loop (it holds live provider objects).
- A KMS key vault.
- Data migration: there are no production instances.
