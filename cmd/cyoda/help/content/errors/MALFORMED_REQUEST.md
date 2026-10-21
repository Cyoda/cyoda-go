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

Raised by `POST /api/entity/stats/{entityName}/{modelVersion}/query` when the body cannot be read from the connection, is not valid JSON, or does not decode into the request shape. Decoding is strict: a member the request does not declare, at any depth, is rejected rather than ignored, as is a member name in another letter case or written twice, so a misspelled `agregations` fails here instead of quietly running with no aggregations. An unknown or case-variant member inside `condition` is `INVALID_CONDITION` instead; a duplicate there is refused here. A `pointInTime` that is not RFC 3339 also lands here, because it is rejected while the body is being decoded — there is no separate code for it.

The message carries the decoder's reason and the member's location as a JSON Pointer, or a byte offset where the text is not JSON. Correct the body and resend.

A body over the 10 MiB ceiling is a different outcome: `413` with `BAD_REQUEST`. Field-level problems that survive decoding — an empty `groupBy`, a bad JSONPath, an unknown aggregation operator — have their own codes.

Other entity endpoints report an undecodable body as `BAD_REQUEST`; this code is specific to the grouped-statistics endpoint.

## SEE ALSO

- errors
- crud
- errors.BAD_REQUEST
