# Signing key pairs shared by the cluster — design (#285)

Facts: `docs/superpowers/research/2026-09-26-285-signing-keys-research.md`
(cited "R§n"). Companion issue: #624 (platform-operator role), which ships in the
same release. Prerequisite for the cassandra backend: cyoda-go-cassandra#102.

## 1. Summary

Signing key pairs are stored in the SYSTEM-tenant KV store, with the private key
sealed under a wrapping key derived from the bootstrap key. Every node keeps an
in-memory copy that follows the store through a gossip change message and a
periodic re-read. Admin changes read the store, not the copy, and a change that
touches several records is undone if it cannot be completed. The bootstrap key's
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
- **Key vault**: the component that creates and opens issued private keys. It
  hands out a `Signer`; no other code sees an issued key's private material.
- **Owner**: the value a vault writes into each record it seals and uses to decide
  whether it can open a record. For the wrapped vault: the bootstrap KID.
- **Node copy**: a node's in-memory copy of a store's records. All hot paths —
  signing, verification, JWKS, `GET current` — read only the node copy.
- **Re-read**: rebuilding the node copy from a KV `List` of the namespace.
- **Stale**: a node copy with no successful re-read for 10 × the re-read interval.

## 3. Requirements

1. A key pair issued, invalidated, reactivated or deleted on any node takes effect
   on every node within the bound in §6.
2. Issued key pairs and all changes to them survive a restart on a persistent
   backend (sqlite, postgres, cassandra). The memory backend persists nothing.
3. Invalidating, reactivating or deleting the bootstrap key takes effect on every
   node and survives a restart.
4. An issued private key is never stored unsealed.
5. A key vault backed by a KMS, Vault or HSM can sign issued key pairs without the
   private key leaving it. The bootstrap key remains a PEM from configuration
   (§5.10).
6. Unchanged: the bootstrap KID derivation, and the bootstrap key's default state
   (active, no window) while nobody has changed it through the API.
7. Correct on both entry points that verify first-party tokens: HTTP and gRPC
   (`internal/grpc/interceptor.go:17-54`, `authenticateFromMetadata` `:66-89`).
8. No admin change is left half-applied across several records (§5.7).

## 4. Threat model and what addresses each threat

| Threat | Addressed by |
|---|---|
| **Read exposure of the store** (a backup, a replica, a read-only injection) | Private keys are sealed (§5.2). Test: the raw stored value contains no DER of the private key (§8.2). |
| **The bootstrap PEM is exposed** | Replacing the PEM is the response, and the design makes it safe: it retires every issued key pair (§5.5) and the new bootstrap key signs at once, with no lockout. Tested end to end (§8.2, "restart with another bootstrap key"). The help topic gives the procedure (§9). A WARN at the moment an operator could believe otherwise (below). |
| **Write access to the store** | Out of scope: the same access already allows registering a trusted key or changing any stored data. The binding in §5.2 detects corruption and records mixed up with each other, not a writer. |

**Why replacing the PEM is the only response to its exposure.** Anyone who holds
the PEM and a copy of the store can open every issued private key it owns, whether
or not the bootstrap key is still active. Invalidating or deleting the bootstrap
key through the API stops it signing and verifying, but does not change what the
PEM can open.

**Where the false belief would arise, and the WARN there.** When the bootstrap key
is invalidated or deleted through the API while the wrapped vault still owns issued
key pairs, the node that took the call logs a WARN: the bootstrap key no longer
signs, `CYODA_JWT_SIGNING_KEY` still unseals N issued key pairs, and it must be
replaced if it may be exposed. At startup, while that state lasts, an INFO line
says the same. With a KMS vault the count is zero and nothing is logged.

**Procedure (help topic, §9) when the PEM may be exposed:** generate a new key;
update the secret for every node; restart every node. On each restarted node, every
token signed by the old bootstrap key or by an issued key pair stops verifying,
and clients fetch new tokens from `/oauth/token`, which the new bootstrap key
signs. Issue new key pairs if API-managed rotation is wanted. The retired records
stay inert in the store.

## 5. Design

### 5.1 Records

Namespace `signing-keys` in the SYSTEM-tenant KV store; KV key = KID; JSON.

**Issued record**

| Field | Content |
|---|---|
| `kind` | `"issued"` |
| `kid`, `audience`, `algorithm` | as today (`RS256` only) |
| `active`, `validFrom`, `validTo` | as today; RFC 3339 nano, UTC; UTC year 1..9999 |
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

A timestamp whose UTC year is outside 1..9999 is never written — by either store:
its RFC 3339 form would have no four-digit year, so no node could read the
record back. The adapters answer it with 400 (§7); the encoders refuse it too,
and the decoders treat a stored one as undecodable, so both sides accept the
same range.

No key material. Absent means the default state: active, zero `validFrom`, no
`validTo` (`service.go:70-85`). It is written when the bootstrap key is
invalidated, reactivated or deleted — directly, or as a sibling in a rotation
(§5.7). Every write of it copies the stored record and changes only the fields
the operation sets; `deleted` is never cleared.

### 5.2 The wrapped key vault

- **Wrapping key** = HKDF-SHA256(IKM, salt = none, info =
  `"cyoda-signing-key-wrap-v1"`), 32 bytes. IKM = the RSA primes of the parsed
  bootstrap key, sorted ascending, each left-padded to the byte length of the
  modulus, concatenated. The primes do not depend on the key's encoding: PKCS#1 or
  PKCS#8 (`internal/auth/jwt.go:156-171`), inline or `_FILE` (`app/config.go`
  `envPEMFromSecret`) give the same wrapping key. (The private exponent is not
  used: two different values are valid for the same key.)
- **Plaintext**: the private key as PKCS#8 DER.
- **Seal**: AES-256-GCM via `cipher.NewGCMWithRandomNonce`; sealed bytes =
  `nonce || ciphertext`.
- **Associated data**: the concatenation of these fields, each preceded by its
  length as a 4-byte big-endian integer: `"cyoda-signing-key-v1"`, KID, audience,
  algorithm, owner, SHA-256 of the SPKI DER. The mutable fields (active, window)
  are not bound, so invalidating or reactivating never re-seals.
- **Open** fails if decryption fails, if the key is not RSA, or if its public key
  differs from the record's `publicKey`.
- **Owner** = the bootstrap KID.

Rationale for deriving the wrapping key from the bootstrap key: no new secret;
every node already has it with the same value; while the bootstrap key is usable,
whoever holds it can already sign tokens every node accepts (see §4 for the case
where it is not). `CYODA_HMAC_SECRET` is not an option: a single node generates it
at random (R§6).

### 5.3 Signing interfaces (package `internal/auth`)

```go
// Signer signs a SHA-256 digest with RSASSA-PKCS1-v1_5 (RS256).
type Signer interface {
    Public() crypto.PublicKey
    Sign(ctx context.Context, digest []byte) ([]byte, error)
}

// NewRSASigner wraps an in-process RSA key: the bootstrap key, the wrapped
// vault's opened keys, and tests.
func NewRSASigner(k *rsa.PrivateKey) Signer

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

- `jwt.Sign(ctx, claims, s Signer, kid)` replaces `Sign(claims, *rsa.PrivateKey,
  kid)` (`jwt.go:18`). Every caller moves to it, tests included (they wrap their
  key with `NewRSASigner`), and `e2e/parity/fixtureutil` (its token minting).
  The token handler passes the request's context (`token.go:76` gains the
  request).
- A node opens a record once and keeps the `Signer` until any field bound to the
  sealed key (KID, audience, algorithm, owner, public key) or the sealed bytes
  change; opened signers of records no longer stored are dropped after each
  re-read.
- A KMS vault stores a key reference as `sealed`, uses its KMS scope as `Owner`,
  and checks the public key against `meta.SPKI` without a network call. AWS KMS
  (`MessageType=DIGEST`), GCP `AsymmetricSign` and Vault transit (`prehashed`) all
  sign a digest. Not built now.

### 5.4 The signing-key store

`KeyPair` carries public data only: KID, audience, algorithm, public key, active,
window, and whether it is the bootstrap key. The `PrivateKey` field is removed
(`store.go:22-31`).

```go
var (
    ErrKeyPairNotFound = errors.New("key pair not found")      // → 404
    ErrKeyPairBroken   = errors.New("key pair cannot be used")  // → 500
    ErrStoreStale      = ...                                     // → 503 (below)
)

type KeyStore interface {
    // Node copy.
    Signer(audience string) (*KeyPair, Signer, error)   // selection rule §5.8
    Current(audience string) (*KeyPair, error)          // same selection, no signer
    VerificationKey(kid string) (*rsa.PublicKey, error) // ErrKeyPairNotFound if unusable
    Published() ([]*KeyPair, error)                     // JWKS set

    // Store (KV) — admin.
    Issue(ctx context.Context, req IssueRequest) (*KeyPair, error)
    Invalidate(ctx context.Context, kid string, graceSec int64) error
    Reactivate(ctx context.Context, kid string, from, to time.Time) (*KeyPair, error)
    Delete(ctx context.Context, kid string) error
}
```

`KVKeyStore` also has `Start(ctx)`, which runs its re-read loop; it is not on the
interface, because no consumer of the interface starts loops.

- `IssueRequest`: audience, `validFrom`, `validTo`, `invalidateCurrent`, grace.
  Validation stays in the adapter (`keys_adapter.go:33-94`).
- `ErrStoreStale` satisfies the storage plugins' marker
  (`interface{ StorageUnavailable() bool }`, `internal/common/errors.go:169-177`),
  so `common.Internal` answers it with 503 `STORAGE_UNAVAILABLE`, retryable.
- Adapters: `ErrKeyPairNotFound` → 404 `KEYPAIR_NOT_FOUND`; everything else →
  `common.Internal` (500 with a ticket, or 503). Today every error is 404
  (`keys_adapter.go:150-153,166-168,200-203,244-247`).
- Callers: `token.go:83,221` → `Signer("client")`; `key_source.go:37` →
  `VerificationKey`; `jwks.go:42` → `Published`; `keys_adapter.go` → the admin
  methods and `Current`. The adapter's read after reactivate (`:248`) is the
  `KeyPair` that `Reactivate` returns.
- `AuthConfig` gains the SYSTEM-tenant KV store, the broadcaster, the re-read
  interval and metrics; `TrustedKeyStore` is no longer passed in.
  `NewAuthService` builds and loads both stores (a failed load fails startup, as
  today, `kv_trusted_store.go:189-191`). `app` calls `authSvc.Start(ctx)` with the
  process-lifetime system context, where it starts the trusted-key loop today
  (`app/app.go:307`).

### 5.5 Classification

Each record is classified when a node loads it:

| Class | Condition | Used for |
|---|---|---|
| **owned** | issued, known vault kind, owner = this vault's owner, opens | everything |
| **broken** | issued, owner = this vault's owner, does not open; or unknown vault kind | signer selection only (§5.8); never verifies; a rotation sibling |
| **retired** | issued, vault kind `wrapped`, another owner | nothing |
| **bootstrap state** | `kind = bootstrap`, KID = this node's bootstrap KID | applied to the configured bootstrap key |
| **foreign bootstrap state** | `kind = bootstrap`, another KID | nothing |
| **undecodable** | KV key is a key id; the value does not decode as a record, or an issued record sits at this node's bootstrap KID | see below |
| **ignored** | KV key cannot be a key id (not 32 lowercase hex: `newKID` and `DeriveKID` both give 32 lowercase hex, and cyoda writes no other key) | nothing: never signs, verifies, is published or blocks signing; not a rotation sibling; the admin endpoints refuse its key (400). An ERROR names the keys when their set changes |

- **Undecodable** records: absence of a record can mean "active" (bootstrap state)
  or can change which key signs, so an undecodable record is never skipped
  silently. While any exists, `Signer` fails with `ErrKeyPairBroken` for every
  audience; if its KV key is the bootstrap KID, the bootstrap key also stops
  verifying. An ERROR names the KV key. `DELETE` on that KID replaces it with a deleted bootstrap-state record: if it was some node's bootstrap state that key stays revoked, and otherwise the record is a foreign bootstrap record, which every node ignores. Startup
  does not fail on it (the API must stay usable to remove it). The trusted-key
  store skips an undecodable record with an ERROR, at startup as on every
  re-read — for it, absence only refuses that key, so it never stops a node
  starting — and `DELETE` removes it by its tenant-prefixed KV key.
- Invalidate, reactivate and delete answer 404 for retired records and foreign
  bootstrap-state records. A node deployed with the wrong bootstrap key therefore
  cannot change the cluster's key pairs.
- Retired records stay in the store and become owned again if the bootstrap key
  that owns them is restored. They are inert: they appear only after a PEM
  replacement, cost a few hundred bytes each, and there is no cleanup mechanism.
- Logging: a WARN when the set of retired KIDs changes (count and KIDs, once per
  start and per change); an ERROR when the set of broken or undecodable KIDs
  changes (KIDs and reason class). Never logged: sealed bytes, private keys, full
  records.

Consequences:
- Replacing `CYODA_JWT_SIGNING_KEY` retires every issued key pair, as it already
  ends every token the old bootstrap key signed. The new bootstrap key signs.
- A database copied to another environment: the source's key pairs are retired
  there.
- During a rolling replacement of the bootstrap key, JWKS differs between nodes
  with the old and the new configuration.

### 5.6 Replicated KV store component

The mechanism now inside `KVTrustedKeyStore` (`internal/auth/kv_trusted_store.go`)
becomes one component in `internal/auth`, used by the trusted-key store and the
signing-key store (and by M2M clients, #286, next). It owns:

- the node copy, its read-write lock and its generation counter;
- the initial load;
- re-read on a gossip change message (coalesced, never dropped, `coalesce.go`) and
  periodically every `CYODA_AUTH_CACHE_RECONCILE_INTERVAL` ±10 % jitter;
- the generation-guarded swap (a re-read that overlapped a local change retries,
  at most 5 times, `kv_trusted_store.go:276-333`);
- staleness, measured on the monotonic clock (today wall-clock,
  `kv_trusted_store.go:192,309,395`), true only once the loop has started;
- the change broadcast (payload empty, never read or logged by receivers);
- the admin write path and the multi-record write with compensation (§5.7);
- reconcile metrics, named per store: `auth.trustedkeys.reconcile_*` (unchanged)
  and `auth.signingkeys.reconcile_*`.

Each store supplies: the namespace, the gossip topic (`auth.trustedkeys`,
`auth.signingkeys`), a decode function from `(kvKey, bytes)` to `(copyKey,
record)` — an undecodable record is the signing-key store's own class, and one
the trusted-key store's decode refuses is left out of the copy with an ERROR
(§5.5) — and the metrics names. The
copy key is the store's choice: the trusted-key store keeps keying by bare KID.

**Admin write path** (both stores):

1. Take the store's **admin mutex** (one per store per node). It serialises admin
   writes on a node. It is not the node copy's lock: hot-path readers never wait
   for KV.
2. Read the current records from KV with the request's context: `Get` for one
   record; `List` for a rotation, the trusted-key cap count and the trusted-key
   cross-tenant KID check (today the cache, `kv_trusted_store.go:475,482`).
3. Decide; write KV (§5.7 when several records change).
4. Take the node copy's write lock only to apply the result, with a generation
   bump.
5. Broadcast — after every path that wrote KV, including a compensated failure.

This fixes, for trusted keys too:
- a node that has not yet received a delete writes the deleted record back on an
  invalidate or reactivate (today `kv_trusted_store.go:615,635,670` read the cache);
- a rotation misses a sibling issued on another node (today `:503`);
- a slow KV call blocks every token verification on the node (today admin writes
  hold the copy's lock across KV I/O, `:471-490,613-619,633-643,668-684`);
- a single-record load re-inserts a record a concurrent re-read removed (today
  `loadOne`, `:425-441`); the trusted-key admin `Get` keeps its read-through on a
  miss, now generation-guarded.

Two admin writes to the **same** record on two nodes at the same moment remain
last-writer-wins (the KV store has no conditional write, R§5): a reactivate that
read the record before a concurrent delete can write it back. Admin-only, rare,
documented. Likewise, two tenants registering the same trusted-key KID on two
nodes at the same moment can both pass the cross-tenant check; the node copy,
keyed by bare KID, then holds one of them until the next re-read, and a change
to one can drop the other from that copy. The effect is a refused key, never
cross-tenant verification (verification re-checks the tenant).

The trusted-key verification path stays node-copy-only
(`kv_trusted_store.go:592-608`).

### 5.7 Multi-record writes: rotation

Used by `Issue` with `invalidateCurrent` and by trusted-key `Register` with
`invalidatePrevious` (today best-effort, `kv_trusted_store.go:497-523`). The KV
store has no transactions. Order:

1. Build the new record (signing keys: `Generate`; no write yet).
2. From the `List` (§5.6 step 2), pick the siblings: records of the same partition
   (signing keys: audience; trusted keys: tenant) whose window is still open,
   excluding the new KID. For signing keys: owned and broken issued records, and
   the bootstrap key unless it is deleted. Retired, foreign and undecodable
   records are never siblings.
3. `Put` the new record.
4. `Put` each sibling with `active = false` and `validTo` = the grace expiry
   (`graceExpiry`, `store.go`), all other fields copied from the stored record.
5. If a write in step 4 fails: restore each sibling already written to its
   previous bytes (`Delete` a bootstrap-state record that was absent), `Delete`
   the new record, broadcast, return the error. If a compensating write fails, log
   an ERROR naming every record left changed, broadcast, return the error.
6. Apply to the node copy, broadcast, return 200.

The new record is written before the siblings so that no node that re-reads in
between is left without a signer.

Documented consequences:
- A crash between steps 3 and 5 leaves the new key and the old siblings active; the
  admin repeats the call.
- A peer that re-reads between steps 3 and 5 of a compensated rotation may sign
  with the new key until the compensation's broadcast arrives.
- The restore in step 5 can bring back a sibling that another node deleted at the
  same moment (the last-writer-wins case of §5.6).
- On cassandra, `List` currently returns success with rows missing when a per-key
  read fails (cyoda-go-cassandra#102). A missing sibling would stay active, and a
  missing bootstrap-state record would put the bootstrap key back to its default
  state. #102 must be fixed in the cassandra release that pins this version.

### 5.8 Signing, verification, JWKS

- **Signer selection**: among owned and broken issued records and the bootstrap
  key, those of the audience that are active and inside their window; the latest
  `validFrom` wins, then the greater KID (unchanged rule, `store.go:186-219`). If
  the winner is broken, or an undecodable record exists, `Signer` and `Current`
  fail with `ErrKeyPairBroken` and the log names the KID; another key is never
  chosen instead. None → `ErrKeyPairNotFound`.
- **Verification**: owned issued records and the bootstrap key, active and inside
  the window (unchanged rule, `key_source.go:34-52`). A KID not in the node copy
  is unknown; there is no KV read on the verification path.
- **JWKS**: owned issued records and the bootstrap key whose window has not ended
  (unchanged rule, `store.go` `ListForVerification`).
- **Stale**: `Signer`, `Current` and `Published` return `ErrStoreStale`;
  `VerificationKey` returns `ErrKeyPairNotFound` for every KID (401, unchanged
  mapping, `delegating.go:48-74` **[ruling]**). JWKS answers 503
  `STORAGE_UNAVAILABLE` (problem JSON, `Retry-After`), not an empty set, which
  would make external verifiers drop their cached keys.

### 5.9 Bootstrap key

- Built from configuration on every node, as today, with `NewRSASigner`. Its
  stored state (§5.1), if present, is applied after every load and re-read.
- Deleting it writes `deleted = true`. It then signs and verifies nowhere, is not
  in JWKS, and invalidate, reactivate and delete answer 404. No API call removes
  the record. Recovery: replace `CYODA_JWT_SIGNING_KEY` (new KID, no stored state).
- Reactivating it gives it a window (reactivate requires `validTo`,
  `keys_adapter.go:207-236`), as today — now persisted.

### 5.10 KMS and the bootstrap key

`CYODA_JWT_SIGNING_KEY` stays a required PEM in jwt mode (`app/app.go:264-270`).
A deployment that wants no exportable signing key uses a KMS vault for issued key
pairs and deletes the bootstrap key through the API once an issued key pair signs;
the PEM then only derives the wrapping key, which a KMS vault does not use.
Documented in the help topic; the KMS vault is not built now.

### 5.11 Deletions and exit checks

- Deleted: `InMemoryKeyStore` (the live store today, `service.go:51`),
  `InMemoryTrustedKeyStore` and `NewInMemoryTrustedKeyStoreWithCap` (reachable in
  production only as the fallback at `service.go:53-57`, which `app` never takes),
  and that fallback. Their tests move to the KV-backed stores over an in-memory
  KV.
- The stale comment in `e2e/parity/multinode/registry.go:7-10` that says cassandra
  runs the multi-node scenarios is corrected, and R§9 with it.
- Exit checks (non-test files):
  `grep -rn "InMemoryKeyStore\|InMemoryTrustedKeyStore" --include='*.go'` → empty;
  `grep -rn "rsa.PrivateKey" internal --include='*.go' | grep -v _test` → only
  `jwt.go` (PEM parsing), `NewRSASigner` and the wrapped vault;
  `grep -rn "rsa.GenerateKey" internal --include='*.go' | grep -v _test` → only the
  wrapped vault.
- M2M clients stay per node until #286.

## 6. Timing bound

| Change | Node that took the call | Every other node |
|---|---|---|
| issue, rotate, invalidate, reactivate, delete (issued or bootstrap) | before the 2xx | when the gossip message arrives (normally well under 1 s); if it is lost, at the next re-read (≤ 1.1 × interval, 66 s by default) |
| any change, seen from a node that cannot read its database | — | the node keeps its last copy — and so can still accept a revoked key — until it is stale (10 × interval, 10 min by default); then it refuses every issued key and the bootstrap key |

Consequences, documented in the help topic:
- A token signed with a newly issued or reactivated key can be refused by another
  node until that node has the change — behind a load balancer, likely for about a
  second. To rotate without such refusals, issue the new key pair with `validFrom`
  a few seconds ahead, then invalidate the old one once the window has opened (the
  existing scheduled-rotation practice, `auth.md:240-244`).
- After a rotation, another node can sign with the old key until it has the change;
  its tokens are refused by nodes that have it.
- An open gRPC stream is authenticated when it starts and is not re-checked after a
  revocation (existing behaviour).

A synchronous "every node confirms" design is rejected: revocation must work while
a node is unreachable.

## 7. Errors

| Endpoint | Status | Code | Cause | New? |
|---|---|---|---|---|
| all five `/oauth/keys/keypair` endpoints | 401 | `UNAUTHORIZED` | not authenticated | no |
| | 403 | `FORBIDDEN` | not `ROLE_ADMIN` (#624 changes the role) | no |
| | 501 | `NOT_IMPLEMENTED` | not jwt IAM mode (`keys_adapter.go:23-30`) | no |
| | 503 | `STORAGE_UNAVAILABLE` | storage marked unavailable (only postgres marks it: `plugins/postgres/ceilings.go:119,166`, `classifying_querier.go:157`) | yes; add to OpenAPI |
| | 500 | — | any other store or vault failure (ticket UUID, generic message) | yes (was 404) |
| `POST /oauth/keys/keypair` | 200 | — | issued (and siblings invalidated) | stored now |
| | 400 | `BAD_REQUEST`, `UNSUPPORTED_ALGORITHM` | validation (unchanged) | no |
| | 400 | `BAD_REQUEST` | `validFrom` or `validTo` (the default included) outside UTC years 1..9999 | yes (was 500) |
| | 500 / 503 | — | a write failed; compensated (§5.7) | yes |
| `GET /oauth/keys/keypair/current` | 200 | — | the selected signer | retired excluded |
| | 400 | `BAD_REQUEST` | invalid audience | no |
| | 404 | `KEYPAIR_NOT_FOUND` | no signer for the audience | no |
| | 500 | — | selected signer broken; an undecodable record | yes |
| | 503 | `STORAGE_UNAVAILABLE` | stale | yes |
| `POST .../{keyId}/invalidate` | 200 | — | invalidated | stored now |
| | 400 | `BAD_REQUEST` | grace out of range (unchanged) | no |
| | 400 | `BAD_REQUEST` | `keyId` not 32 lowercase hex characters | yes (was 404) |
| | 404 | `KEYPAIR_NOT_FOUND` | unknown, retired, foreign bootstrap state, deleted bootstrap | yes |
| `POST .../{keyId}/reactivate` | 200 | — | reactivated | stored now |
| | 400 | `BAD_REQUEST` | window validation (unchanged) | no |
| | 400 | `BAD_REQUEST` | `keyId` not 32 lowercase hex characters; `validFrom` or `validTo` outside UTC years 1..9999 | yes (was 404 / 500) |
| | 404 | `KEYPAIR_NOT_FOUND` | unknown, retired, foreign bootstrap state, deleted bootstrap | yes |
| `DELETE .../{keyId}` | 200 | — | issued record removed; undecodable record replaced by a deleted bootstrap-state record; bootstrap `deleted = true` | stored now |
| | 400 | `BAD_REQUEST` | `keyId` not 32 lowercase hex characters | yes (was 404) |
| | 404 | `KEYPAIR_NOT_FOUND` | unknown, retired, foreign bootstrap state, deleted bootstrap | yes |
| `POST /oauth/token` | 500 | `server_error` | signer broken; undecodable record; stale; no signer; store failure | new causes |
| `GET /.well-known/jwks.json` | 200 | — | owned issued keys and the bootstrap key | retired excluded |
| | 503 | `STORAGE_UNAVAILABLE` | stale (`Retry-After`) | yes; JWKS is not in the OpenAPI (it is excluded from conformance, `internal/e2e/openapivalidator/middleware.go:29`); documented in `auth/tokens.md` |
| JWT-authenticated HTTP call | 401 | `UNAUTHORIZED` | KID unknown on this node, inactive, out of window, or stale | new causes |
| gRPC call or stream start | `Unauthenticated` | — | same | new causes |

## 8. Tests

### 8.1 Fixture constraints and helpers

- **Multi-node runs on postgres only.** Cassandra runs the single-node parity list
  (`cyoda-go-cassandra e2e/cassandra_test.go:91`, `parity.AllTests()`), not the
  multi-node registry; its multi-node fixture is cyoda-go-cassandra#35. Recorded as
  a gap.
- **Own cluster.** The shared multi-node cluster signs its own admin and compute
  tokens with the bootstrap key (`e2e/parity/fixtureutil/fixtureutil.go:172-190,
  209-225`). Every scenario that invalidates or deletes the bootstrap key, or uses
  `invalidateCurrent` on `client` (which invalidates the bootstrap key as a
  sibling), runs in its own cluster: `MustSetupMultiNodeWithEnv`
  (`e2e/parity/postgres/multinode_fixture.go:199`) with a short
  `CYODA_AUTH_CACHE_RECONCILE_INTERVAL` and `CYODA_IAM_M2M_ADMIN_ROLE_ENABLED=true`.
  Token path, before touching the bootstrap key: issue a `client` key pair on A;
  create an admin M2M client on A; get a token from A's `/oauth/token`; poll until
  B accepts it; only then revoke the bootstrap key. All later calls use that token.
- **One token helper.** Admin tokens in the new multi-node tests come from one
  helper, so #624 changes one place.
- **M2M clients are per node** until #286: a scenario creates its M2M client on
  the node whose `/oauth/token` it calls.
- **Shared registries.** The single-node parity server and the shared multi-node
  cluster run every scenario in sequence. Key-pair scenarios there never use
  `invalidateCurrent` and delete their key pairs in `t.Cleanup` (on a fresh
  context: `t.Context()` is already cancelled when cleanups run). On the
  single-node server they use audience `human` (nothing signs with it,
  `token.go:83,221`); on the shared multi-node cluster, audience `client`, so B
  can be shown signing with the key issued on A. `e2e/parity/client` gains helpers for
  the key-pair endpoints, `/oauth/token` and JWKS (today only `ProbeAuthRaw`).
- **Restart in-process.** `newSchedDB` + `newStackOn`
  (`internal/e2e/scheduler_harness_test.go:85-117`) give a stack its own database
  and a restart on it. A harness option supplies the PEM; it sets both the
  configured key and `h.signKey` (`callback_harness_test.go:205,222`).
- **Broken signer end to end** needs a raw KV write on a stack's own database
  (pgx), never the shared TestMain server.
- **gRPC** is covered in single-node `internal/e2e` (same authenticator).

### 8.2 Coverage matrix

| Scenario | unit | e2e (postgres, in-process) | parity single-node (memory, sqlite, postgres, cassandra) | multi-node (postgres) |
|---|---|---|---|---|
| seal/open round trip; each associated-data field binds; public-key check; golden vector for the wrapping key and a fixed sealed record | ✓ | | | |
| the raw stored value contains no PKCS#1 or PKCS#8 DER of the private key (req 4) | ✓ | ✓ | | |
| a non-exporting fake vault signs through the store (req 5) | ✓ | | | |
| classification: owned, broken (each reason), retired, bootstrap state, foreign bootstrap state, undecodable | ✓ | | | |
| lifecycle 200s: issue, current, invalidate, reactivate, delete | | ✓ | ✓ (`human`) | |
| every admin endpoint: store error → 500 with ticket, marked-unavailable or stale → 503, never 404 | ✓ (faulty KV) | | | |
| 401 / 403 / 501 on every admin endpoint | | ✓ (existing) | | |
| rotation compensation: sibling write fails → new record deleted, siblings restored, broadcast, 5xx — both stores | ✓ (faulty KV) | | | |
| deleted bootstrap is never a rotation sibling; `deleted` survives every write | ✓ | | | |
| retired: 404 on invalidate / reactivate / delete; not in JWKS or current | ✓ | ✓ (restart with another PEM) | | |
| broken signer → `/oauth/token` 500 and `GET current` 500 | ✓ | ✓ (raw KV write) | | |
| undecodable record: signing fails; at the bootstrap KID the bootstrap key stops verifying; `DELETE` replaces it with a deleted bootstrap-state record | ✓ | | | |
| bootstrap state: absent → default; applied after re-read | ✓ | | | |
| admin writes read KV: a stale copy cannot bring back a deleted record; rotation sees a sibling issued elsewhere; hot-path reads do not wait on a slow KV call — both stores | ✓ | | | |
| generation-guarded single-record load (trusted keys) | ✓ | | | |
| staleness on the monotonic clock; JWKS 503 and verification refusal when stale | ✓ | | | |
| lost gossip repaired by the periodic re-read | ✓ | | | |
| gRPC: token from an issued key accepted; invalidated → `Unauthenticated` | | ✓ | | |
| issue on A → B verifies once it has the change; B signs with it; JWKS on B | | | | ✓ shared cluster |
| invalidate / reactivate / delete an issued pair on A → effect on B | | | | ✓ shared cluster |
| rotate with `invalidateCurrent` on A → B refuses the old key | | | | ✓ own cluster |
| invalidate, reactivate and delete bootstrap on A → effect on B; reactivate after delete → 404 | | | | ✓ own cluster |
| stale fails closed (`PauseDatabase`, `multinode_fixture.go:137`) | ✓ | | | ✓ own cluster |
| restart keeps an issued pair, a bootstrap invalidation and a bootstrap delete | | ✓ | | |
| restart with another bootstrap key retires issued pairs; the new key signs | | ✓ | | |
| WARN when the bootstrap key is invalidated or deleted while it still owns issued key pairs; none with zero | ✓ | | | |
| existing trusted-key unit and e2e tests pass unchanged | ✓ | ✓ | | |
| a timestamp outside UTC years 1..9999: both stores write nothing; 400 on issue and reactivate (key pair and trusted key) | ✓ | ✓ | | |
| `keyId` not 32 lowercase hex → 400 on delete, invalidate, reactivate; a well-formed unknown `keyId` → 404 | ✓ | ✓ | | |
| trusted-key store: an undecodable record is skipped at startup (ERROR); `DELETE` removes it | ✓ | | | |

Waivers:
- 500 and 503 on the admin endpoints are not tested end to end. The adapters pass
  errors through `common.Internal`, whose mapping is already tested; the unit rows
  assert every adapter reaches it with each error class.
- Cassandra multi-node: no fixture yet (cyoda-go-cassandra#35).
- Timestamp-range and `keyId`-format rows: no parity scenario. They are adapter
  validation, and a record the adapters refuse cannot be written through the
  API on any backend.

## 9. Documentation and parity

`cyoda help` topics (`cmd/cyoda/help/content/`):
- `config/auth.md`:
  - `:51-52` — `CYODA_JWT_SIGNING_KEY` also derives the wrapping key and is the
    root secret; replacing it retires issued key pairs.
  - `:180-185` — the bootstrap key has no window unless the API gave it one.
  - §"Auth cache reconciliation" (`:187-197`) — three caches; the fail-closed
    effect on first-party tokens.
  - §"JWT signing keypair rotation" (`:224-252`) — remove both limitations;
    sharing, persistence, the §6 bound and the no-refusal rotation practice,
    retire-on-replacement, the exposed-PEM procedure (§4), recovery from a broken
    signer (invalidate or `DELETE` it; replacing the PEM helps only when this node
    cannot open it under the current PEM), from an undecodable record (`DELETE`),
    and from a deleted or invalidated bootstrap key with no other signer (no
    first-party token then verifies: an admin from a federated OIDC provider —
    subject to #624 — reactivates an invalidated key or issues a key pair, or the
    PEM is replaced; a deleted bootstrap key cannot be reactivated),
    and the KMS path (§5.10).
- `auth/tokens.md:121,125` — the keystore is shared by the cluster; JWKS 503.
- `errors.md:84` and `errors/KEYPAIR_NOT_FOUND.md` — new causes (retired key pair,
  deleted bootstrap key).
- `errors/NOT_FOUND.md:16,24` — says the key-pair and trusted-key endpoints return
  `NOT_FOUND`; they return `KEYPAIR_NOT_FOUND` and `TRUSTED_KEY_NOT_FOUND`
  (`keys_adapter.go:152-245`, `trusted_adapter.go:35`). Corrected.
- `helm.md:313` and `quickstart.md:105` (signing-key sections) — one line each: the
  key is the root secret for issued key pairs; see `config.auth` for replacing it.
- `config_registry.go:87,97,99` (the `cyoda help config` variable table) — same
  corrections as `config/auth.md`.
- Checked and unaffected: `auth/oidc.md`, `auth/trusted-keys.md`, `cli/serve.md`,
  `errors/FEATURE_DISABLED.md`, `openapi.md`, `run.md`, `cluster.md`,
  `errors/STORAGE_UNAVAILABLE.md`.

Other documents:
- `README.md:169` — three caches.
- OpenAPI: 503 on the five key-pair endpoints; `go generate ./api`.
- `docs/ARCHITECTURE.md` §7.2 (`:1846-1873`), including `:1863`, which says
  first-party validation has no KV dependency.
- `docs/cloud-parity/signing-key-window.md:30` ("no window") and a new
  `docs/cloud-parity/signing-key-pairs.md`: key pairs shared and persisted,
  bootstrap revocation persisted, retire-on-replacement, 404 for retired, 503 on
  the endpoints and JWKS. At-rest sealing is an implementation property, not part
  of the contract (Cloud passes PKCS#8 through an encryption hook whose default encryptor is a no-op and none is wired, so it is stored unencrypted in practice, R§8). Matching CaaS ticket.
- `COMPATIBILITY.md`: the cassandra plugin must include the #102 fix to run this
  version.
- `CHANGELOG.md` — `### Breaking`: key-pair store failures answer 500/503, not 404;
  first-party token verification depends on the KV store (fails closed when a node
  cannot read it for 10 intervals); a restart no longer restores a revoked
  bootstrap key; replacing `CYODA_JWT_SIGNING_KEY` retires issued key pairs.
- No new environment variable, no new error code.

## 10. Out of scope

- The platform-operator role: #624, same release.
- M2M clients shared by the cluster: #286, next, on the §5.6 component.
- The OIDC provider registry's own re-read loop (it holds live provider objects).
- A KMS key vault.
- Cassandra multi-node fixture: cyoda-go-cassandra#35.
- Data migration: there are no production instances.
