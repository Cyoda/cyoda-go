---
topic: config.cors
title: "CORS configuration"
stability: stable
see_also:
  - config
  - run
---

# config.cors

## NAME

config.cors — Cross-Origin Resource Sharing (CORS) controls for the public HTTP surface.

## SYNOPSIS

cyoda supports four CORS modes: `disabled`, `loopback` (default), `wildcard`, and `allowlist`.
Configure the mode via `CYODA_CORS_ENABLED` and `CYODA_CORS_ALLOWED_ORIGINS`.

## OPTIONS

- `CYODA_CORS_ENABLED` — enable CORS middleware; set to `false` to disable and handle CORS at
  an upstream ingress/proxy layer (default: `true`)
- `CYODA_CORS_ALLOWED_ORIGINS` — comma-separated list of allowed origins, or `*` for wildcard
  mode (default: empty — loopback mode)

## MODES

The effective mode is determined by the combination of the two env vars:

- `CYODA_CORS_ENABLED=false` — **disabled** (regardless of `CYODA_CORS_ALLOWED_ORIGINS`)
- `CYODA_CORS_ENABLED=true`, `CYODA_CORS_ALLOWED_ORIGINS` empty — **loopback** (default)
- `CYODA_CORS_ENABLED=true`, `CYODA_CORS_ALLOWED_ORIGINS=*` — **wildcard**
- `CYODA_CORS_ENABLED=true`, `CYODA_CORS_ALLOWED_ORIGINS=https://example.com,...` — **allowlist**

### disabled

CORS middleware is not installed. No `Access-Control-*` headers are emitted. OPTIONS requests
return chi's default 405. Use this when CORS is handled at your ingress layer (nginx, Envoy,
cloud load balancer).

### loopback (default)

Only loopback origins are permitted: `http(s)://localhost`, `http(s)://127.0.0.1`, and
`http(s)://[::1]` on any port. Suitable for local development. Set
`CYODA_CORS_ALLOWED_ORIGINS` to permit additional origins.

### wildcard

`Access-Control-Allow-Origin: *` is emitted for all cross-origin requests. Credentials
(cookies, `Authorization` header) cannot be used with wildcard mode. Appropriate only for
fully public, stateless read APIs.

### allowlist

Only the origins listed in `CYODA_CORS_ALLOWED_ORIGINS` are permitted. Exact scheme+host+port
matching (no wildcards in individual entries). Origins must be absolute URIs with scheme and
host; paths and query strings are not permitted.

## EXAMPLES

**Loopback only (local dev, default):**

```
# nothing to set — loopback is the default when CYODA_CORS_ENABLED=true
```

**Single production origin:**

```
CYODA_CORS_ENABLED=true
CYODA_CORS_ALLOWED_ORIGINS=https://app.example.com
```

**Multiple origins:**

```
CYODA_CORS_ENABLED=true
CYODA_CORS_ALLOWED_ORIGINS=https://app.example.com,https://admin.example.com
```

**Wildcard (public read API):**

```
CYODA_CORS_ENABLED=true
CYODA_CORS_ALLOWED_ORIGINS=*
```

**Disabled (CORS at ingress):**

```
CYODA_CORS_ENABLED=false
```

## SEE ALSO

- config
- run
