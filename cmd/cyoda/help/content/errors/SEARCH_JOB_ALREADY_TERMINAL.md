---
topic: errors.SEARCH_JOB_ALREADY_TERMINAL
title: "SEARCH_JOB_ALREADY_TERMINAL — search job has already completed or failed"
stability: stable
see_also:
  - errors
  - errors.SEARCH_JOB_NOT_FOUND
---

# errors.SEARCH_JOB_ALREADY_TERMINAL

## NAME

SEARCH_JOB_ALREADY_TERMINAL — an operation was attempted on a search job that has already reached a terminal state (completed, failed, or cancelled).

## SYNOPSIS

HTTP: `400` `Bad Request`. gRPC: `success` false, `error.code` `CLIENT_ERROR`, message prefix `SEARCH_JOB_ALREADY_TERMINAL:`. Retryable: `no`.

## DESCRIPTION

Search jobs are long-running asynchronous operations. Once a job reaches a terminal state it cannot be cancelled, resumed, or otherwise modified. This error is returned when such an operation is attempted on a finished job — a cancel (`PUT /search/async/{jobId}/cancel`, gRPC `SnapshotCancelRequest`) is the only operation that raises it. A job that finishes while the cancel is landing is refused the same way.

The HTTP response body carries `currentStatus` (`SUCCESSFUL`, `FAILED`, or `CANCELLED`) and `snapshotId`; the gRPC answer carries the snapshot's full status (`status.status`, `status.snapshotId`, and `status.failureReason` for a `FAILED` snapshot).

Not retryable on the same job. Results from a successfully completed job remain available for retrieval.

## SEE ALSO

- errors
- errors.SEARCH_JOB_NOT_FOUND
