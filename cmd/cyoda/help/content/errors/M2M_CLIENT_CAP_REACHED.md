---
topic: errors.M2M_CLIENT_CAP_REACHED
title: "M2M_CLIENT_CAP_REACHED — tenant M2M client cap reached"
stability: stable
see_also:
  - errors
  - auth.clients
  - config.auth
---

# errors.M2M_CLIENT_CAP_REACHED

## NAME

M2M_CLIENT_CAP_REACHED — the tenant has reached the maximum number of M2M clients.

## SYNOPSIS

HTTP: `400` `Bad Request`. Retryable: `no`.

## DESCRIPTION

`POST /clients` enforces a per-tenant cap (default 100, configurable via `CYODA_IAM_M2M_CLIENT_MAX_PER_TENANT`; 0 means no cap). Delete a client the tenant no longer uses, or raise the cap. Creates on several nodes at the same moment can each pass the check, so a tenant can exceed the cap by at most one client per node.

## SEE ALSO

- errors
- auth.clients
- config.auth
