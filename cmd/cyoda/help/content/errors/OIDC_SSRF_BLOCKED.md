---
topic: errors.OIDC_SSRF_BLOCKED
title: "OIDC_SSRF_BLOCKED — wellKnownConfigUri blocked by SSRF policy"
stability: stable
see_also:
  - errors
  - config.auth
---

# errors.OIDC_SSRF_BLOCKED

## NAME

OIDC_SSRF_BLOCKED — the `wellKnownConfigUri` resolves to a private or link-local
address range that is blocked by the SSRF protection policy.

## SYNOPSIS

HTTP: `400` `Bad Request`. Retryable: `no`.

## DESCRIPTION

When registering an OIDC provider, cyoda validates the `wellKnownConfigUri`
against a blocklist of private address ranges (loopback, RFC1918, link-local,
IPv6 ULA). If the URI's hostname resolves to any blocked range, the
registration is rejected to prevent Server-Side Request Forgery attacks.

Blocked ranges:

- `127.0.0.0/8` — IPv4 loopback
- `169.254.0.0/16` — IPv4 link-local (cloud metadata endpoints)
- `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16` — RFC1918 private
- `::1/128` — IPv6 loopback
- `fe80::/10` — IPv6 link-local
- `fc00::/7` — IPv6 ULA

In development or controlled environments where private-network OIDC providers
are required, set `CYODA_OIDC_ALLOW_PRIVATE_NETWORKS=true` to bypass this check.
Never use this override in production.

## SEE ALSO

- errors
- config.auth
