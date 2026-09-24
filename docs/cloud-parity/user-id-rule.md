# User-id rule — Cloud twin-alignment spec

cyoda-go defines the contract; Cyoda Cloud aligns to it.

This is a **wire-contract tightening**: a token whose user identity breaks the
rule, which authenticated before, is now rejected.

## Rule

A user identifier is valid UTF-8, 1 to 255 characters (Unicode code points, not
bytes), and contains none of these:

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

- **255 characters** was already the OIDC `sub` limit.
- **Control characters and noncharacters** are what the CloudEvents spec
  forbids in a String attribute. A user id is sent to compute nodes as the
  `authid` attribute, so a user id that passes the rule is always a legal
  attribute value. The OIDC `sub` already banned C0 and DEL; C1 and
  noncharacters are new for it too.
- **U+FFFD**: a JSON decoder replaces every invalid UTF-8 byte and every lone
  surrogate escape with U+FFFD. Admitting it would let different signed claims
  (`"a\ud800"`, `"a\udfff"`, raw `a\xff`) all decode to one user id.

## Where it is enforced

| Door | Surface | Failure |
| --- | --- | --- |
| First-party user claim: `caas_user_id`, or `sub` when `caas_user_id` is absent | Every authenticated HTTP request and gRPC method | `401`, the uniform problem detail; `codes.Unauthenticated` over gRPC |
| OIDC `sub` | Federated tokens | `401`, as before |
| Token-exchange subject token `sub`, which becomes the issued token's user id | `POST /oauth/token`, token-exchange grant | `400 invalid_grant` |
| `CYODA_BOOTSTRAP_USER_ID` | Process startup, only when a bootstrap client is configured | Non-zero exit; the chart's `values.schema.json` rejects it at `helm install`, and a unit test checks that its pattern agrees with the rule on every character |

A `caas_user_id` that is present names the user. If it is empty, not a string,
or outside the rule, the token is rejected. It never falls back to `sub`, which
would put a different identity in place of the one the token carries. Only an
absent `caas_user_id` falls back to `sub`.

For an OIDC principal the user id is `oidc:<providerId>:<sub>`. The rule applies
to `sub`, so the full id can be longer than 255 characters.

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
