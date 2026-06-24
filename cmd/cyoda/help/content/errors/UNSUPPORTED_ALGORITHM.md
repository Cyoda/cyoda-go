---
topic: errors.UNSUPPORTED_ALGORITHM
title: "UNSUPPORTED_ALGORITHM — algorithm not supported"
stability: stable
see_also:
  - errors
---

# errors.UNSUPPORTED_ALGORITHM

## NAME

UNSUPPORTED_ALGORITHM — the requested JWT algorithm is not implemented in this version.

## SYNOPSIS

HTTP: `400` `Bad Request`. Retryable: `no`.

## DESCRIPTION

cyoda-go v0.8.0 signs and verifies only `RS256`. Other enum values declared in the OpenAPI spec (`RS384`, `RS512`, `PS256`, `PS384`, `PS512`, `ES256`, `ES384`, `ES512`, `EdDSA`) are rejected with this error. Cyoda Cloud supports the full enum; parity is tracked in a v0.8.1 follow-up.

Use `algorithm: RS256` or omit the field.

## SEE ALSO

- errors
