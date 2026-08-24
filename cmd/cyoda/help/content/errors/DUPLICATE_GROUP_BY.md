---
topic: errors.DUPLICATE_GROUP_BY
title: "DUPLICATE_GROUP_BY — the same groupBy dimension was listed twice"
stability: stable
see_also:
  - errors
  - crud
  - errors.MISSING_GROUP_BY
  - errors.INVALID_GROUP_BY_PATH
---

# errors.DUPLICATE_GROUP_BY

## NAME

DUPLICATE_GROUP_BY — two `groupBy` entries name the same dimension.

## SYNOPSIS

HTTP: `400` `Bad Request`. Retryable: `no`.

## DESCRIPTION

Raised by `POST /api/entity/stats/{entityName}/{modelVersion}/query`. The reserved token `state` appearing twice, or the same JSONPath appearing twice, is rejected rather than deduplicated: a repeated dimension adds a second copy of the same value to every bucket's `groupKey` and changes nothing about the grouping, so it is more likely a mistake in the request than an intent.

The message names the offending entry. Remove the repeat. Two different paths that happen to resolve to the same value at runtime are not duplicates and are accepted — the comparison is on the request entries, which are validated but never rewritten.

## SEE ALSO

- errors
- crud
- errors.MISSING_GROUP_BY
- errors.INVALID_GROUP_BY_PATH
