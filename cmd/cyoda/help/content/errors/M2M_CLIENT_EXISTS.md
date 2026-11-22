---
topic: errors.M2M_CLIENT_EXISTS
title: "M2M_CLIENT_EXISTS — the tenant already holds this client id"
stability: stable
see_also:
  - errors
  - auth.clients
---

# errors.M2M_CLIENT_EXISTS

## NAME

M2M_CLIENT_EXISTS — the tenant already holds a client with the requested id.

## SYNOPSIS

HTTP: `409` `Conflict`. Retryable: `no`.

## DESCRIPTION

`POST /clients?clientId=<id>` creates a client with a chosen id. Client ids are unique within a tenant, so a create of an id the tenant already holds is refused and creates nothing. Of two creates of one id at the same moment, on any nodes, exactly one succeeds; the other gets this error. A provisioning script can treat it as "already there": the existing client's secret is not returned again, so keep the secret from the create that made it, or reset it (`PUT /clients/{clientId}/secret`).

## SEE ALSO

- errors
- auth.clients
