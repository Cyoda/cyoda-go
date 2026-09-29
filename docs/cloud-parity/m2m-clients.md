# M2M clients: a per-tenant cap — Cloud twin-alignment spec

cyoda-go defines the contract; Cyoda Cloud aligns to it.

## Rule

A tenant holds at most a configured number of M2M clients (technical users).
`POST /clients` in a tenant that already holds that many is refused:

| Status | `errorCode` | Retryable | When |
|---|---|---|---|
| `400` | `M2M_CLIENT_CAP_REACHED` | no | the caller's tenant already holds the maximum number of M2M clients |

The body is the usual RFC 9457 problem detail; `detail` begins
`M2M_CLIENT_CAP_REACHED:`. No client is created and no secret is issued.

- The cap is per tenant. A client of another tenant never counts.
- Every client the tenant holds counts, whatever its roles (`withAdminRole`
  makes no difference).
- Deleting a client frees a slot at once.
- The cap does not apply to a secret reset, which creates no client.

In cyoda-go the cap is `CYODA_IAM_M2M_CLIENT_MAX_PER_TENANT`: default `100`,
`0` means no cap, and a negative value refuses to start. The value is
deployment configuration; the contract is the refusal and its code, not the
number.

cyoda-go checks the cap on each node before it writes, with no
compare-and-set in its store, so creates on several nodes at the same moment
can exceed the cap by at most one client per node. Creates on one node never
exceed it. Cloud may enforce the cap exactly; if it overshoots, by no more
than one client per node.

## Not a contract change

M2M clients in cyoda-go are stored in the cluster's database: a create, reset
or delete takes effect on every node when it returns, and clients survive a
restart. Cloud already behaves so — it saves a technical user as a user
record in its store
(`backend/src/main/kotlin/net/cyoda/saas/account/TechnicalUserService.kt:441-472`
in the Cloud repository). Nothing changes for Cloud here.

## Cloud today

Cloud has no cap. `TechnicalUserService.addTechnicalUser`
(`TechnicalUserService.kt:86-115`) checks the admin-role flag, generates an
id and a secret, and saves the user; nothing counts the tenant's existing
technical users.

## Cloud action

Add a per-tenant cap on technical users. At the cap, `POST /clients` answers
`400` with `errorCode` `M2M_CLIENT_CAP_REACHED` and creates nothing. Choose a
default that suits Cloud's tiers, or record here where Cloud differs.
