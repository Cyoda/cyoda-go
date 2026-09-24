# User-id rule — Cloud twin-alignment spec

cyoda-go defines the contract; Cyoda Cloud aligns to it.

This is a **wire-contract tightening**: a first-party token whose user claim
breaks the rule, which authenticated before, is now rejected.

## Rule

A user identifier is 1 to 255 characters (Unicode code points, not bytes) and
contains no control character: U+0000–U+001F and U+007F. Any other character is
admitted, non-ASCII included. Nothing is normalised — no trimming, case folding
or Unicode normalisation — because two spellings that normalise to one value
would then name one user.

The definition lives in `internal/common/user_id.go` as `ValidateUserID`. It
returns an error wrapping `common.ErrInvalidUserID` that gives the reason and a
character position, never the value.

### Why this shape

This was already the rule for the OIDC `sub`. It is now the rule for every user
id, through one function.

A user id is not a key or a path segment: it is attribution and display. So,
unlike a tenant id, it has no charset grammar. The control-character ban keeps a
user id from breaking a log line, an audit record or a display. The 255 cap
bounds it.

## Where it is enforced

| Door | Surface | Failure |
| --- | --- | --- |
| First-party user claim: `caas_user_id`, or `sub` when `caas_user_id` is absent or empty | Every authenticated HTTP request and gRPC method | `401`, the uniform problem detail; `codes.Unauthenticated` over gRPC |
| OIDC `sub` | Federated tokens | `401`, as before |
| Token-exchange subject token `sub`, which becomes the issued token's user id | `POST /oauth/token`, token-exchange grant | `400 invalid_grant` |
| `CYODA_BOOTSTRAP_USER_ID` | Process startup, only when a bootstrap client is configured | Non-zero exit; the chart's `values.schema.json` rejects it at `helm install` |

Two related changes on the first-party claim:

- A `caas_user_id` that is present but not a string is rejected. Before, it was
  treated as absent and `sub` was used — a different identity in place of the
  one the token carried.
- A `caas_user_id` that is present but breaks the rule is rejected. It does not
  fall back to `sub`.

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

1. Apply the same rule to an inbound `sub` before auto-enrollment, and reject
   a control character. Today an external IdP's `sub` reaches `userName` and
   Cloud-minted tokens unchecked.
2. Cloud's effective limit on a `sub` is 100 characters minus the provider
   prefix, because of the `userName` column. cyoda-go admits 255. A `sub`
   between the two works in cyoda-go and fails enrollment in Cloud. Cloud
   should either admit 255 or say so as a declared divergence.
