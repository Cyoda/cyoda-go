# Signing key pairs — Cloud twin-alignment spec

cyoda-go **defines** the contract; Cyoda Cloud aligns to it. See also
`signing-key-window.md` for the window a signing key pair must be inside to
sign or verify — this document covers what happens to a key pair across nodes,
across a restart, and when its issuing bootstrap key is replaced.

## What cyoda-go does

- **Key pairs are shared and persisted.** Issuing, invalidating, reactivating
  or deleting a key pair (`/oauth/keys/keypair*`) on one node of a cluster
  takes effect on every other node, and the change survives a restart on a
  persistent backend (sqlite, postgres, or a commercial backend). The memory
  backend persists nothing, by design. A node applies the change locally before
  answering; other nodes apply it when the change reaches them (normally under
  a second) or, if it is missed, at the next periodic reconcile.
- **The bootstrap key's revocation is persisted too.** Invalidating,
  reactivating or deleting the bootstrap signing key
  (`CYODA_JWT_SIGNING_KEY`) through the API is stored and cluster-wide, and it
  survives a restart. Deleting it is terminal for that key: no API call
  reactivates a deleted bootstrap key, and replacing
  `CYODA_JWT_SIGNING_KEY` afterwards mints a fresh KID with no stored
  state — the old, deleted key stays deleted.
- **A rotation never ends the bootstrap key.** `invalidateCurrent` on
  `POST /oauth/keys/keypair` ends the issued key pairs of the audience whose
  window is open, the first rotation included; the bootstrap key is not one
  of them. It stays active, keeps verifying, and signs again whenever no
  issued key pair of its audience is active and inside its window. Only an
  invalidate or `DELETE` that names its key id ends it, after the grace
  period if one is given. Cloud's configured key
  (`KeyPairStrategy.LOCAL_FILE`, `JwtSigningKeyProvider.kt`) is not stored
  and is never a rotation sibling either, so the tiers agree.
- **Replacing the bootstrap key retires every key pair it owned.** A key pair
  is sealed under the bootstrap key configured at the time it was issued.
  Once `CYODA_JWT_SIGNING_KEY` is replaced, those key pairs stop signing,
  verifying and appearing in JWKS — this is the deliberate response to the
  bootstrap key's PEM being exposed, not a side effect to work around.
  Restoring the old bootstrap key brings them back. **No Cloud equivalent**:
  Cloud's configured signing key is not stored, so it has nothing to retire
  when it is replaced.
- **A retired key pair answers 404, not 200 with stale data.** A retired key
  pair is never a candidate for signing, verification or JWKS — it is not
  merely excluded after being selected, it is never considered. `current`
  and token issuance therefore either return a different, usable key pair for
  the audience or answer as if none exists (`current`: `404
  KEYPAIR_NOT_FOUND`) if every key pair for that audience is retired.
  `DELETE`/`invalidate`/`reactivate` on a retired key pair's own KID answer
  `404 KEYPAIR_NOT_FOUND` directly. A retired key pair becomes usable again
  only if the bootstrap key that owns it is restored.
- **A broken key pair is manageable, not stuck.** Unlike a retired one,
  `invalidate`, `reactivate` and `delete` all answer `200` for a broken key
  pair — by KID, the admin API treats it like any owned key pair, whether or
  not its vault can actually open it. Only `current` (and token issuance) can
  fail on it, and only if it wins signer selection for its audience:
  `current` then answers `500`, not `404`. On `invalidate`/`reactivate`,
  `404` is reserved for a KID that is genuinely absent, retired, a foreign
  bootstrap-state record, an undecodable record, or (at this node's own
  bootstrap key id specifically) already deleted. `DELETE` treats an
  undecodable record differently from every other 404 cause: it always
  succeeds (`200`), replacing the record with a deleted bootstrap-state
  record instead of leaving it in place — the one way to clear it. A record
  at a key that cannot be a key id (not 32 lowercase hex) is ignored and
  logged; it never blocks signing.
- **A storage-unavailable failure is 503, never a stale or wrong answer** —
  but which operations can even reach one differs by endpoint. `current` and
  `GET /.well-known/jwks.json` read only the node's own copy and never call
  the store at all; a stale copy (no successful reconcile for 10 reconcile
  intervals) is their only route to `503 STORAGE_UNAVAILABLE` (JWKS: with
  `Retry-After`) — neither can answer it for any other reason, because
  neither makes a live store call that could fail that way. Token issuance
  refuses a stale copy the same node-copy-only way, but always as a plain
  `500 server_error` on `/oauth/token` — that endpoint's OAuth-shaped error
  body never distinguishes a storage cause with `503`. `invalidate`,
  `reactivate` and `delete` read one record from the store directly on every
  call and never consult the node's own copy, so they never fail merely
  because that copy is stale; they answer `503 STORAGE_UNAVAILABLE` only when
  the store call itself reports the backend unavailable. `issue` behaves the
  same way when it rotates a sibling, but a plain issue (no
  `invalidateCurrent`) reads nothing from the store at all before writing the
  new record, so only the write itself can produce a `503` there. None of
  these cases is ever downgraded to a `404` or to an empty/partial answer.
- **Malformed input is `400`, never stored.** `DELETE`, `invalidate` and
  `reactivate` answer `400 BAD_REQUEST` for a `keyId` that is not 32
  lowercase hex characters (every issued and bootstrap KID has that form).
  A `validFrom` or `validTo` whose UTC year is outside 1..9999 — e.g.
  `9999-12-31T23:59:59-05:00`, which is year 10000 in UTC — is `400
  BAD_REQUEST` on key-pair issue and reactivate and on trusted-key register
  and reactivate, including a default `validTo` that lands there; no such
  record is ever written, because RFC 3339 cannot represent it for reading
  back.
- **At-rest sealing is an implementation property, not part of the contract.**
  cyoda-go seals an issued key pair's private key at rest (AES-256-GCM under a
  key derived from the bootstrap key's RSA primes) so a copy of the store
  alone — a backup, a replica, a read-only injection — does not expose private
  key material. Cloud passes the PKCS#8 bytes through a pluggable encryption
  hook whose default encryptor returns its input unchanged, and no encryptor
  is wired, so in practice an issued key pair is stored unencrypted there.
  What both tiers owe callers is identical (a private key that never leaves
  the server in a signing or verification response, and a key pair's
  lifecycle behaving as above); how — or whether — it is protected at rest is
  each tier's own storage decision, not something Cloud must match.

## Cloud action

Tracked in CP-3979. Confirm, or record where Cloud differs:

1. A key pair issued, invalidated, reactivated or deleted on one node is
   visible and effective on every node, and survives a restart.
2. Invalidating, reactivating or deleting the bootstrap signing key is stored
   cluster-wide and survives a restart; deleting it is terminal until the
   configured key is replaced.
3. Replacing the configured signing key does not carry old issued key pairs
   forward as still-usable — if Cloud has no notion of "retiring" a key pair
   because it does not store the configured key, record that as the
   difference rather than as a gap.
4. A retired, foreign-bootstrap-state or already-deleted-bootstrap key pair
   answers `404` on every lifecycle endpoint, never a stale `200`; a key pair
   that merely cannot be opened (broken) still answers `200` there and fails
   only where it would actually be used to sign.
5. A storage or availability failure on a key-pair endpoint or JWKS answers a
   retryable `5xx`, never a `404` and never an empty or partial JWKS set.
6. A malformed key-pair `keyId`, and a `validFrom`/`validTo` outside UTC
   years 1..9999 on the key-pair and trusted-key endpoints, answer `400
   BAD_REQUEST`, and nothing is stored.
7. A rotation (`invalidateCurrent`) ends stored key pairs only, never the
   configured signing key. The grace-period rules — verify until the end of
   the grace period, never sign again — are in `signing-key-window.md`.
