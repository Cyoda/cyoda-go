---
topic: errors.UNSUPPORTED_KEY_TYPE
title: "UNSUPPORTED_KEY_TYPE — JWK kty not supported"
stability: stable
see_also:
  - errors
  - errors.UNSUPPORTED_ALGORITHM
---

# errors.UNSUPPORTED_KEY_TYPE

## NAME

UNSUPPORTED_KEY_TYPE — the JWK `kty` is not supported by this cyoda-go version.

## SYNOPSIS

HTTP: `400` `Bad Request`. Retryable: `no`.

## DESCRIPTION

`POST /oauth/keys/trusted` accepts only `kty: "RSA"` in v0.8.0. Cloud also supports `kty: "EC"` and `kty: "OKP"`; cyoda-go parity is tracked in a v0.8.1 follow-up.

## SEE ALSO

- errors
- errors.UNSUPPORTED_ALGORITHM
