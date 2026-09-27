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
- **A storage-unavailable failure is 503, never a stale or wrong answer** —
  but which operations can produce it, and why, differs by endpoint. `issue`,
  `invalidate`, `reactivate` and `delete` read the store directly on every
  call and never consult the node's own copy, so they never fail merely
  because that copy is stale; they can still answer `503
  STORAGE_UNAVAILABLE` if the store call itself reports the backend
  unavailable. `current` and `GET /.well-known/jwks.json` read the node's
  copy and do check its staleness (no successful reconcile for 10 reconcile
  intervals): a stale copy answers `503 STORAGE_UNAVAILABLE` there too (JWKS:
  with `Retry-After`), on top of the same direct-store-unavailable case.
  Token issuance also refuses a stale copy, but always as a plain `500
  server_error` on `/oauth/token` — that endpoint's OAuth-shaped error body
  never distinguishes a storage cause with `503`. None of these cases is ever
  downgraded to a `404` or to an empty/partial answer.
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

Confirm, or record where Cloud differs:

1. A key pair issued, invalidated, reactivated or deleted on one node is
   visible and effective on every node, and survives a restart.
2. Invalidating, reactivating or deleting the bootstrap signing key is stored
   cluster-wide and survives a restart; deleting it is terminal until the
   configured key is replaced.
3. Replacing the configured signing key does not carry old issued key pairs
   forward as still-usable — if Cloud has no notion of "retiring" a key pair
   because it does not store the configured key, record that as the
   difference rather than as a gap.
4. A retired, deleted-bootstrap or otherwise unusable key pair answers `404`
   on every lifecycle endpoint, never a stale `200`.
5. A storage or availability failure on a key-pair endpoint or JWKS answers a
   retryable `5xx`, never a `404` and never an empty or partial JWKS set.

## CaaS ticket to file

**Title:** `[CaaS] Signing key pairs: persisted bootstrap revocation, 503 on key-pair endpoints and JWKS, 404 for retired key pairs`

**Description:**

cyoda-go now shares signing key pairs across every node of a cluster and
persists them (and the bootstrap key's invalidate/reactivate/delete state)
across a restart. Confirm Cloud's equivalent behaviour, or record the
divergence, against these points (full contract:
`docs/cloud-parity/signing-key-pairs.md` in cyoda-go):

- A key pair change (issue/invalidate/reactivate/delete) and a bootstrap-key
  revocation change are effective everywhere Cloud's request could land, and
  persist across a restart.
- Deleting the bootstrap key is terminal until the configured key changes; no
  reactivate call restores a deleted bootstrap key.
- A key pair that cannot be used because its owning key was replaced answers
  `404`, not a stale `200`.
- A storage or availability failure on a key-pair endpoint, or on the JWKS
  endpoint, answers a retryable `5xx`, never `404` and never an empty or
  partial key set.
- At-rest sealing of a private key is an implementation detail, not part of
  the contract — Cloud need not match cyoda-go's AES-256-GCM sealing scheme,
  only the visible behaviour above.
