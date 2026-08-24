---
topic: errors.MISSING_GROUP_BY
title: "MISSING_GROUP_BY — grouped-stats request supplied no groupBy dimension"
stability: stable
see_also:
  - errors
  - crud
  - errors.DUPLICATE_GROUP_BY
  - errors.INVALID_GROUP_BY_PATH
---

# errors.MISSING_GROUP_BY

## NAME

MISSING_GROUP_BY — the grouped-statistics request omitted `groupBy`, or sent it as an empty array.

## SYNOPSIS

HTTP: `400` `Bad Request`. Retryable: `no`.

## DESCRIPTION

Raised by `POST /api/entity/stats/{entityName}/{modelVersion}/query`. `groupBy` is required and must carry at least one dimension: the endpoint returns one bucket per distinct group key, and there is no reading of the request that groups by nothing.

Supply at least one entry — either the reserved token `state` (lifecycle state) or a scalar JSONPath into the payload, such as `$.country`. For an ungrouped count of a model's entities, use the model statistics endpoint instead.

## SEE ALSO

- errors
- crud
- errors.DUPLICATE_GROUP_BY
- errors.INVALID_GROUP_BY_PATH
