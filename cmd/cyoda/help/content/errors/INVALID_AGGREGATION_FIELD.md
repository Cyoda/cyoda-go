---
topic: errors.INVALID_AGGREGATION_FIELD
title: "INVALID_AGGREGATION_FIELD — aggregation field is not a scalar JSONPath"
stability: stable
see_also:
  - errors
  - crud
  - errors.INVALID_GROUP_BY_PATH
  - errors.INVALID_AGGREGATION_OP
---

# errors.INVALID_AGGREGATION_FIELD

## NAME

INVALID_AGGREGATION_FIELD — an aggregation's `field` is not a JSONPath that denotes a single scalar.

## SYNOPSIS

HTTP: `400` `Bad Request`. Retryable: `no`.

## DESCRIPTION

Raised by `POST /api/entity/stats/{entityName}/{modelVersion}/query`. An aggregation `field` obeys exactly the grammar a `groupBy` path does — a required `$.` leader followed by dot-separated segments of ASCII letters, digits, `_` and `-`, with no bracket-quoted access, array subscript or projection, recursive descent, whitespace or non-ASCII character. See `cyoda help errors INVALID_GROUP_BY_PATH` for the grammar and the rejected spellings; the message names the offending field and the reason.

The reserved token `state` is groupBy-only. There is no defined aggregate over lifecycle state, so `state` as an aggregation `field` is just an identifier missing its `$.` leader and is rejected here.

A well-formed path whose value is absent or non-numeric at runtime is not an error: those entities are skipped for that aggregation, and a bucket with no numeric samples reports `null`.

## SEE ALSO

- errors
- crud
- errors.INVALID_GROUP_BY_PATH
- errors.INVALID_AGGREGATION_OP
