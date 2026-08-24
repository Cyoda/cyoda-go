---
topic: errors.NOT_IMPLEMENTED_BY_BACKEND
title: "NOT_IMPLEMENTED_BY_BACKEND — storage backend lacks the grouped-stats capability"
stability: stable
see_also:
  - errors
  - crud
  - errors.NOT_IMPLEMENTED
---

# errors.NOT_IMPLEMENTED_BY_BACKEND

## NAME

NOT_IMPLEMENTED_BY_BACKEND — the configured storage backend implements neither of the optional SPI capabilities the grouped-statistics endpoint can execute against.

## SYNOPSIS

HTTP: `501` `Not Implemented`. Retryable: `no`.

## DESCRIPTION

Raised by `POST /api/entity/stats/{entityName}/{modelVersion}/query`. The endpoint runs a query either by pushing the grouping down to the backend (`GroupedAggregator`) or by streaming entities and tallying them in process (`Iterable`). Both are optional SPI interfaces, detected at request time. A backend that implements neither offers no execution path, so the request fails rather than returning an empty or approximated result.

The `memory`, `sqlite` and `postgres` backends shipped with cyoda-go implement both; this code indicates a backend that has not yet adopted the capability. Every other endpoint continues to work normally.

Not retryable — the request is well-formed. Switch to a backend that implements at least one of the two interfaces, or wait for the backend to add one. Distinct from `NOT_IMPLEMENTED`, which is the endpoint itself having no implementation regardless of backend.

## SEE ALSO

- errors
- crud
- errors.NOT_IMPLEMENTED
