---
topic: errors.INVALID_GROUP_BY_PATH
title: "INVALID_GROUP_BY_PATH — groupBy entry is not a scalar JSONPath"
stability: stable
see_also:
  - errors
  - crud
  - errors.INVALID_AGGREGATION_FIELD
  - errors.INVALID_FIELD_PATH
---

# errors.INVALID_GROUP_BY_PATH

## NAME

INVALID_GROUP_BY_PATH — a `groupBy` entry is neither the reserved `state` token nor a JSONPath that denotes a single scalar.

## SYNOPSIS

HTTP: `400` `Bad Request`. Retryable: `no`.

## DESCRIPTION

Raised by `POST /api/entity/stats/{entityName}/{modelVersion}/query`. Every `groupBy` entry other than `state` must match the wire JSONPath grammar — a required `$.` leader followed by dot-separated segments of ASCII letters, digits, `_` and `-`:

```
path    = "$." segment ( "." segment )*
segment = 1*( ALPHA / DIGIT / "_" / "-" )
```

Rejected here: a missing leader (`country` — write `$.country`), bracket-quoted property access (`$['country']`, `$.['country']`), array projections and subscripts (`$.items[*]`, `$.items[0]` — address a position as an ordinary segment, `$.items.0`), recursive descent (`$..name`), filter expressions, empty, leading, trailing or doubled dots, whitespace, quotes, and non-ASCII characters.

Array subscripts are rejected even though a `condition` `jsonPath` accepts them: a group-by dimension must resolve to one scalar, and a projection has no such value. The check runs at the API boundary before any storage backend, so a request is rejected identically whichever execution path would have served it. The message names the offending entry and the reason.

The token `state` is accepted in `groupBy` only, and carries no `$.` leader — it names lifecycle state, not a path. A payload field literally called `state` is written `$.state`.

## SEE ALSO

- errors
- crud
- errors.INVALID_AGGREGATION_FIELD
- errors.INVALID_FIELD_PATH
