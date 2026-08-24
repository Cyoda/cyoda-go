---
topic: errors.INVALID_LIMIT
title: "INVALID_LIMIT — grouped-stats limit is non-positive or above the ceiling"
stability: stable
see_also:
  - errors
  - crud
  - config
  - errors.GROUP_CARDINALITY_EXCEEDED
---

# errors.INVALID_LIMIT

## NAME

INVALID_LIMIT — the grouped-statistics request's `limit` is not a positive integer at or below `CYODA_STATS_GROUP_MAX`.

## SYNOPSIS

HTTP: `400` `Bad Request`. Retryable: `no`.

## DESCRIPTION

Raised by `POST /api/entity/stats/{entityName}/{modelVersion}/query`. `limit` is optional; when present it is a top-N cap on the buckets returned, and it must be greater than zero and no greater than the server's `CYODA_STATS_GROUP_MAX` (default `10000`). A value outside that range is rejected up front rather than clamped, so the response never silently covers a smaller set than the one asked for. The message carries the configured maximum.

Omit `limit` for all buckets, up to the cardinality ceiling. If the ceiling itself is too low for the deployment, an operator raises `CYODA_STATS_GROUP_MAX`.

## SEE ALSO

- errors
- crud
- config
- errors.GROUP_CARDINALITY_EXCEEDED
