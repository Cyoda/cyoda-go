# Task 6 Report: `txjoin.JoinFromToken` inbound helper + error mapping

## Status
DONE — all tests green, build and vet clean, committed.

## TDD Evidence

### RED
`go test ./internal/domain/txjoin/ -v` failed with:
```
undefined: JoinFromToken  (8 call sites)
FAIL	github.com/cyoda-platform/cyoda-go/internal/domain/txjoin [build failed]
```

### GREEN
After writing `txjoin.go`:
```
--- PASS: TestJoinFromToken_EmptyPassThrough
--- PASS: TestJoinFromToken_JoinsValid
--- PASS: TestJoinFromToken_ExpiredMaps410
--- PASS: TestJoinFromToken_ForgedMaps401
--- PASS: TestJoinFromToken_NotFoundMaps404
--- PASS: TestJoinFromToken_RolledBackMaps404
--- PASS: TestJoinFromToken_AlreadyCommittedMaps404
--- PASS: TestJoinFromToken_TenantMismatchMaps403
PASS  ok  github.com/cyoda-platform/cyoda-go/internal/domain/txjoin  0.419s
```

## Files Changed
- `internal/domain/txjoin/txjoin.go` (new)
- `internal/domain/txjoin/txjoin_test.go` (new)

## Commit
`10f6c98 feat(txjoin): token->Join helper with reused tx error-code mapping`

## Deviations from Brief
- Brief test scaffold used `*common.OperationalError` + `HTTPStatus()`/`Code()` methods — real type is `*common.AppError` with exported fields `Status` and `Code`; corrected.
- `fakeTM` embeds `spi.TransactionManager` interface (satisfies full interface; unimplemented methods panic if called) — matches task description exactly.
- Added three extra test cases (RolledBack→404, AlreadyCommitted→404) in addition to the brief's minimum four — full coverage of the error-mapping table.

## Review Fix: unknown Join errors mapped to 5xx

**Finding:** The Join-error `switch` used a `default:` branch that mapped every non-tenant-mismatch error to 404 TRANSACTION_NOT_FOUND, silently misclassifying genuine server faults (context.Canceled, DB I/O failure, timeout) as client errors.

**Edit:** `internal/domain/txjoin/txjoin.go` — replaced the `default:` 404 catch-all with explicit `errors.Is` cases for the three 404 sentinels (`ErrTxNotFound`, `ErrTxRolledBack`, `ErrTxAlreadyCommitted`) and a new `default:` that calls `common.Internal("failed to join transaction", err)` to return a 500 with a ticket UUID.

**New test:** `TestJoinFromToken_UnknownJoinErrorMaps5xx` — `fakeTM.Join` returns `errors.New("db unavailable")`; asserts `*common.AppError` with `Status == 500`. Confirmed RED (got 404) before the fix; GREEN after.

**Verification:**
```
go build ./... && go test ./internal/domain/txjoin/ -count=1 -v
--- PASS: TestJoinFromToken_EmptyPassThrough
--- PASS: TestJoinFromToken_JoinsValid
--- PASS: TestJoinFromToken_ExpiredMaps410
--- PASS: TestJoinFromToken_ForgedMaps401
--- PASS: TestJoinFromToken_NotFoundMaps404
--- PASS: TestJoinFromToken_RolledBackMaps404
--- PASS: TestJoinFromToken_AlreadyCommittedMaps404
--- PASS: TestJoinFromToken_UnknownJoinErrorMaps5xx
--- PASS: TestJoinFromToken_TenantMismatchMaps403
PASS  ok  github.com/cyoda-platform/cyoda-go/internal/domain/txjoin  0.283s
```

**Commit:** `fix(txjoin): map unknown Join errors to 5xx instead of masking as 404`

## Self-Review
- Token is never logged (only `claims.TxRef` is used after verify).
- Original `ctx` is returned alongside every error — callers always have a valid context.
- `errors.Is` used for all sentinel checks (handles wrapping: ErrTxRolledBack/ErrTxAlreadyCommitted wrap ErrTxTerminated; ErrTxNotFound wraps ErrNotFound).
- No concerns.
