---
topic: errors.MALFORMED_REQUEST
title: "MALFORMED_REQUEST — grouped-stats request body could not be decoded"
stability: stable
see_also:
  - errors
  - crud
  - errors.BAD_REQUEST
---

# errors.MALFORMED_REQUEST

## NAME

MALFORMED_REQUEST — the grouped-statistics request body could not be read or decoded.

## SYNOPSIS

HTTP: `400` `Bad Request`. Retryable: `no`.

## DESCRIPTION

Raised by `POST /api/entity/stats/{entityName}/{modelVersion}/query` when the body cannot be read from the connection, is not valid JSON, or does not decode into the request shape. Decoding is strict: an unrecognised top-level field is rejected rather than ignored, so a misspelled `agregations` fails here instead of quietly running with no aggregations. A `pointInTime` that is not RFC 3339 also lands here, because it is rejected while the body is being decoded — there is no separate code for it.

The message carries the decoder's reason. Correct the body and resend.

A body over the 10 MiB ceiling is a different outcome: `413` with `BAD_REQUEST`. Field-level problems that survive decoding — an empty `groupBy`, a bad JSONPath, an unknown aggregation operator — have their own codes.

Other entity endpoints report an undecodable body as `BAD_REQUEST`; this code is specific to the grouped-statistics endpoint.

## SEE ALSO

- errors
- crud
- errors.BAD_REQUEST
