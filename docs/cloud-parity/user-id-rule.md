# User-id rule — Cloud twin-alignment spec

cyoda-go defines the contract; Cyoda Cloud aligns to it.

This is a **wire-contract tightening**: a token whose user identity breaks the
rule, which authenticated before, is now rejected.

## Rule

A user identifier is valid UTF-8, 1 to 255 characters (Unicode code points, not
bytes), is not the reserved id `system` in any letter case, and contains none
of these:

- a control character: U+0000–U+001F and U+007F–U+009F;
- a noncharacter: U+FDD0–U+FDEF, and U+FFFE and U+FFFF in every plane;
- U+FFFD, the replacement character.

Any other character is admitted, non-ASCII included. Nothing is normalised — no
trimming, case folding or Unicode normalisation — because two spellings that
normalise to one value would then name one user.

The definition lives in `internal/common/user_id.go` as `ValidateUserID`. It
returns an error wrapping `common.ErrInvalidUserID` that gives the reason, and
for a rejected character its code point and position, but never the value.

### Why this shape

A user id is not a key or a path segment: it is attribution and display. So,
unlike a tenant id, it has no charset grammar.

- **255 characters** bounds a value that is attribution and display, not
  a key.
- **Control characters and noncharacters** are what the CloudEvents spec
  forbids in a String attribute. A user id is sent to compute nodes as the
  `authid` attribute, so a user id that passes the rule is always a legal
  attribute value.
- **U+FFFD**: a JSON decoder replaces every invalid UTF-8 byte and every lone
  surrogate escape with U+FFFD. Admitting it would let different signed claims
  (`"a\ud800"`, `"a\udfff"`, raw `a\xff`) all decode to one user id.
- **`system`** is the platform principal's own id, the executor of every
  scheduled firing (`common.ReservedSystemUserID`). Reserving it means no
  caller can be recorded as that principal.

## Where it is enforced

| Door | Surface | Failure |
| --- | --- | --- |
| User claim of an inbound token: `caas_user_id`, or `sub` when `caas_user_id` is absent | Every authenticated HTTP request and gRPC method | `401`, the uniform problem detail; `codes.Unauthenticated` over gRPC |
| User assertion `sub`, which becomes the OBO token's user id | `POST /tenants/{tenant}/oauth/token`, token-exchange grant | `400 invalid_request` |

`cyoda token --user`, which signs an admin token offline with the signing key,
checks the same rule, `system` included, before it signs (exit code `2`); the
claim is checked again at the first door when the token is used. A stored M2M
client whose user id fails the rule is treated as damaged. No configuration
variable carries a user id.

A `caas_user_id` that is present names the user. If it is empty, not a string,
or outside the rule, the token is rejected. It never falls back to `sub`, which
would put a different identity in place of the one the token carries. Only an
absent `caas_user_id` falls back to `sub`.

## Cloud today

Checked against `~/dev/cyoda` and `~/dev/cyoda-platform`:

- Cloud does not read `caas_user_id` in its Kotlin or Java code. The Auth0
  Action `scripts/auth0/action-assign-cyoda-user-id.js:89-91` mints it as a
  UUID without dashes (32 hex characters), which passes the rule.
- Cloud keys users on `sub`. Auto-enrollment builds
  `userName = "<providerId>|<sub>"`
  (`backend/.../iam/integration/AbstractCaasOidcComponents.kt:68`).
  `CSUser.userName` is capped at 100 characters (`CSUser.kt:26-29`). No
  character check is applied to `sub` anywhere.
- Ids Cloud generates itself — user UUIDs, M2M client ids, the minted `sub` —
  all pass the rule.

## Cloud action

1. Apply the same rule to an inbound `sub` before auto-enrollment. Today an
   external IdP's `sub` reaches `userName` and Cloud-minted tokens unchecked.
2. Cloud's effective limit on a `sub` is 100 characters minus the provider
   prefix, because of the `userName` column. cyoda-go admits 255. A `sub`
   between the two works in cyoda-go and fails enrollment in Cloud. Cloud
   should either admit 255 or record it as a declared divergence.
3. Reserve `system` (any letter case) on every user id that comes from
   outside the platform: a token's user claim and an OBO assertion's `sub`.
4. Apply the rule to the `sub` of every OBO user assertion (see
   `obo-only-user-identity.md`), which becomes the issued token's user id.
