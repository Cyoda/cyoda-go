---
topic: errors.NOT_IMPLEMENTED
title: "NOT_IMPLEMENTED — endpoint is not yet implemented"
stability: stable
see_also:
  - errors
---

# errors.NOT_IMPLEMENTED

## NAME

NOT_IMPLEMENTED — the requested endpoint or operation exists in the API contract but is not served by this server, in this version or in its current IAM mode.

## SYNOPSIS

HTTP: `501` `Not Implemented`. Retryable: `no`.

## DESCRIPTION

The route is defined and accepted by the server but the handler returns this error. Two causes:

- The operation is not implemented in this version. The response is identical until a new version is deployed.
- The server runs in mock IAM mode (`CYODA_IAM_MODE=mock`), which has no client store: `POST /oauth/token`, the `/clients` operations and the `/oauth/keys/trusted*` operations (while their flag is on) answer `501`. Run the server in jwt mode to use them (see `cyoda help auth integration`, *RUNNING IT LOCALLY*).

Distinct from a `404` — the endpoint exists without a functional implementation here. Not retryable.

## SEE ALSO

- errors
