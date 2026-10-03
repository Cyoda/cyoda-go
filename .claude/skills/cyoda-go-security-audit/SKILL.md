---
name: cyoda-go-security-audit
description: Security audit of a cyoda-go change before its PR — the security gate in the CLAUDE.md workflow. Use after code review, on the exact head that will be merged.
---

# Security audit

Audit the change in the context of the code around it. A hole usually sits
where changed code meets a path that already existed, so read past the diff:
every caller of what changed, and every path the changed code reaches.

## Trust boundaries

Find the boundaries the change touches by reading the code, not from a list:
where requests enter, where identity is established, and where data leaves the
process. Expect at least these kinds:

- Client-facing APIs (HTTP and gRPC), including operator/admin endpoints.
- Credential and token issuance.
- Compute nodes answering processor and criterion callouts. Their responses
  are untrusted input.
- Other cluster nodes: forwarded and proxied requests.
- Storage backends, in-tree and external, behind the SPI.

Trace each request the change affects from its entry point to storage and back
to the response.

## What to check at each boundary

- **Authentication is wired.** Every new or changed route, handler or RPC sits
  behind the authentication and authorization it needs. Check how it is
  registered, not only what the handler does.
- **Tokens are validated in full.** Signature algorithm, issuer, audience,
  expiry and key selection are no weaker than on the base commit, including
  across key rotation.
- **Tenant isolation.** The tenant comes from the authenticated identity, never
  from a path, body or header the caller controls. Every read, write, list,
  count, search and error path is scoped by it. No tenant can read, change, or
  infer the existence of another tenant's data — including through error
  codes, 404-versus-403 differences, or counts.
- **Identifier aliasing.** Ids are compared exactly. Normalising an id for
  lookup (case folding, trimming, path cleaning) lets two distinct ids reach
  the same record, which is a tenant hole.
- **Privileged paths.** Code that acts as the system principal or with operator
  authority — background work, cluster-internal calls, admin operations,
  bootstrap — re-checks ownership instead of trusting that a caller upstream
  checked it. Look for IDOR on any resource addressed by an id that is not
  tenant-scoped.
- **Outbound calls.** Any fetch to an address a caller can influence is an SSRF
  surface.
- **Credentials.** No token, client secret or signing key reaches a log at any
  level, an error message, a response, or persistent storage in clear.
- **Input.** Validated at the boundary; malformed input is rejected early.
- **Errors.** 4xx responses carry domain detail and an error code; 5xx
  responses carry a generic message and a ticket UUID, with no stack trace,
  SQL, connection string or internal path.
- **Fail closed.** An auth, scope or dependency failure rejects the request.
  Nothing falls back to a less-checked path.

Compare against the base commit: report any check the change makes weaker,
even where the result still looks acceptable.

## Report

Report only issues you have traced: for each, the concrete request sequence
that exploits it (who sends what, and what they gain), or drop it. Rank
findings by severity, each with `file:line` and the fix. List the boundaries
you checked and found clean. A clean audit is a valid result.
