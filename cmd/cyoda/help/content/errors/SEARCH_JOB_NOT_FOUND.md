---
topic: errors.SEARCH_JOB_NOT_FOUND
title: "SEARCH_JOB_NOT_FOUND — search job does not exist"
stability: stable
see_also:
  - errors
  - errors.SEARCH_JOB_ALREADY_TERMINAL
---

# errors.SEARCH_JOB_NOT_FOUND

## NAME

SEARCH_JOB_NOT_FOUND — the referenced asynchronous search job does not exist in the current tenant.

## SYNOPSIS

HTTP: `404` `Not Found`. gRPC: `success` false, `error.code` `CLIENT_ERROR`, message prefix `SEARCH_JOB_NOT_FOUND:`. Retryable: `no`.

## DESCRIPTION

Reading a search job's status or results, or cancelling it, returns this error when the job ID is unknown or belongs to a different tenant — over HTTP and over gRPC (`SnapshotGetStatusRequest`, `SnapshotGetRequest`, `SnapshotCancelRequest`). Jobs are tenant-scoped; a valid job ID from one tenant is not visible to another.

Not retryable. The job ID and tenant credentials must match the values used at job submission time.

## SEE ALSO

- errors
- errors.SEARCH_JOB_ALREADY_TERMINAL
