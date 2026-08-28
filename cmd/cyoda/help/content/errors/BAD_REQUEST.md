---
topic: errors.BAD_REQUEST
title: "BAD_REQUEST — malformed or invalid request"
stability: stable
see_also:
  - errors
  - errors.VALIDATION_FAILED
---

# errors.BAD_REQUEST

## NAME

BAD_REQUEST — the request body, query parameter, or header is malformed or structurally invalid.

## SYNOPSIS

HTTP: `400` `Bad Request`. Retryable: `no`.

## DESCRIPTION

Fired when the server cannot parse or structurally process the incoming request. Common triggers include invalid JSON, unsupported format specifiers, a parameter outside its allowed range, and mutually exclusive parameters set together.

A payload that parses and then fails against the registered model is not this code — that is `VALIDATION_FAILED`, or `INCOMPATIBLE_TYPE` for a leaf whose DataType does not fit.

Not retryable. The same request produces the same error.

## SEE ALSO

- errors
- errors.VALIDATION_FAILED
