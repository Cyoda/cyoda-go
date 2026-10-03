---
paths:
  - "internal/**/*.go"
---
# Error Handling Conventions

## Error Classification

- `AppError` with levels: Operational (4xx), Internal (5xx), Fatal (5xx).
- 4xx: full domain detail with error code prefix (`MODEL_NOT_FOUND: model X not found`). The client configured the domain — they should see what went wrong.
- 5xx: generic message + ticket UUID for correlation. Full detail logged server-side at ERROR with the same ticket.
- `retryable: true` for transaction conflicts (409).

## Error Codes

Defined in `internal/common/error_codes.go`. Refer to that file — do not maintain a separate list.

## Warnings & Diagnostics

- Accumulate via `common.AddWarning(ctx, msg)` and `common.AddError(ctx, msg)`.
- Surfaced in gRPC `warnings` array and HTTP response body.
- Propagate processor/criteria response warnings and errors to the caller.

## Rules

- Never expose stack traces, connection strings, or credentials in any error response.
