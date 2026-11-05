---
paths:
  - "internal/**/*.go"
---
# Error Handling Conventions

## Error Classification

- `AppError` with levels: Operational (client-visible: 4xx, 501, 503), Internal (5xx), Fatal (5xx).
- 4xx: full domain detail with error code prefix (`MODEL_NOT_FOUND: model X not found`). The client configured the domain — they should see what went wrong.
- 5xx: generic message + ticket UUID for correlation. Full detail logged server-side at ERROR with the same ticket.
- `retryable: true` when the same request can succeed on a fresh attempt: transaction conflicts (409), an unavailable dependency or exhausted capacity (503).

## Error Codes

Defined in `internal/common/error_codes.go`. Refer to that file — do not maintain a separate list.

## Warnings & Diagnostics

- Accumulate warnings via `common.AddWarning(ctx, msg)`.
- Surfaced in the gRPC `warnings` array and, on an HTTP data route, in `Cyoda-Warning` response headers, one per warning (`internal/httpmw.Warnings`, which installs the bag; encoding and bounds there). A warning raised after the HTTP response has started is not sent.
- Propagate processor/criteria/function response warnings to the caller; a member's failure reaches the caller through its callout failure.

## Rules

- Never expose stack traces, connection strings, or credentials in any error response.
