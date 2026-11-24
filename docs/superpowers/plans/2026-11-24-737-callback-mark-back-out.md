# Back out the rollback-only mark on a failed callback write — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A callback write that fails with an answer that is not retryable keeps nothing and leaves the transaction usable; the compute member gets the real answer and its processor decides whether the operation fails.

**Architecture:** Row 7 of the scope table (`internal/txcoord/end.go` `decide`) is the only place a rollback-only mark starts; rows 4, the refusal of later requests under a marked scope, the `ASYNC_NEW_TX` stop, the callout cancel on a mark, the owner's `Seal` refusal for a mark, and the SPI's mark surface only pass a mark on. All of it is removed. What stays: a scope is all-or-nothing (savepoint in-process, `DiscardScope` on a capable backend), and a discard that fails leaves the transaction unusable so `Seal` fails.

**Tech Stack:** Go 1.27.4; `github.com/cyoda/cyoda-go-spi` (sibling worktree `/Users/paul/go-projects/cyoda-light/cyoda-go-spi-737`, branch `fix/737-no-mark` off SPI `release/v0.9.1` = tag `v0.9.0`); cyoda-go worktree `.claude/worktrees/737`, branch `fix/737-callback-mark` off `release/v0.9.1`.

**Spec:** issue #737 (problem, ruling, fix). Background: `docs/superpowers/specs/2026-11-08-704-callback-scopes-design.md` (section 2 "How a scope ends", section 8 the SPI capability) — this plan removes its row 4, row 7's mark, D-A and D-B.

## Global Constraints

- Ships in the v0.9.1 PATCH, as a **bugfix** (Paul, 2026-11-24): CHANGELOG `### Fixed`, never `### Breaking`. SPI goes v0.9.0 → v0.9.1 (versions linked; Paul's ruling — a back-out of a change that should never have happened).
- `TRANSACTION_ROLLBACK_ONLY` was introduced in v0.9.0 by this same change (9058cb92). Backed out with it: the error code, its help topic, its OpenAPI response component and every `$ref` to it, and its error-matrix cells.
- Unchanged: rows 1 (superseded → 410), 2 (lost race → 409 CONFLICT not retryable for the callback, retryable for the owner), 3 (ended session → 404, transaction broken), success, row 6 (retryable → discarded), row 8 (read 4xx → completed), row 9 (read 5xx → discarded); `TRANSACTION_BUSY`; `COMMIT_IN_JOINED_TRANSACTION`'s refusal itself; schema extensions stay whatever happens.
- A discard that fails is a 500 for the request and leaves the transaction unusable: in-process it is recorded as `broken` (already so); on a capable backend the SPI obliges `Seal` to fail after any `DiscardScope` that returned an error.
- No issue numbers in shipped artefacts (code, comments, help, OpenAPI). Help prose: plain English, the client's view, present tense, no history.
- go.work carries an uncommitted `use /Users/paul/go-projects/cyoda-light/cyoda-go-spi-737` line: never `git add -A` / `git add .`; stage files by name.
- Verification per task: the touched packages' tests and `go vet ./...`; `make test` at the end of tasks 2 and 5; e2e only via `go test -timeout 30m -run '<names>' ./internal/e2e/...` for the tests a task touches (CI runs the whole suite).

## Review Focus

1. A failed callback write followed by more callbacks under the same pass: they are served, and the commit holds the kept ones and none of the failed one's writes (entities, audit events, scheduled-task arms of its cascade). → Task 2 unit + Task 3 e2e `TestCallbackScope_FailedJoinedWrite_ProcessorHandlesIt_*`.
2. A failed discard on the durable path with no mark to carry it: `Seal` must still fail. → Task 1 SPI doc obligation; Task 2 `TestDurable_DiscardFailureFailsSeal` against the `txcoordtest` double, which must implement the obligation.
3. A failed callback write inside a transaction that lost a write race: the callback still answers 409 CONFLICT (row 2), and the owner gets the retryable 409, not a commit. → Task 2 keeps `TestEnd_Row5_ReleaseFailsLostRaceAnswersRow2`; add `TestEnd_Row2_LostRaceBeatsNonRetryableWrite`.
4. A panicking handler on a write: discarded, 500, transaction usable. → Task 2 `TestEnd_PanicOutcome_DiscardsNoMark`.
5. A processor that answers failure after its callback failed: the operation fails exactly as any failed processor does (no rewritten code). → Task 3 `TestCallbackScope_FailedJoinedWrite_ProcessorFails_HTTP`.

---

### Task 1: SPI — remove the mark surface, keep the failed-discard obligation

**Files (in `/Users/paul/go-projects/cyoda-light/cyoda-go-spi-737`):**
- Modify: `coordination.go` (doc "# Rollback-only" section, `OpenScope`/`CheckScope`/`CompleteScope`/`DiscardScope`/`Seal` docs, `ErrTxRollbackOnly`, `RollbackOnlyError`)
- Modify: `coordination_test.go` (drop the `ErrTxRollbackOnly` sentinel row and `TestRollbackOnlyError`)
- Modify: `spitest/coordination.go` (drop `RollbackOnlyMarksParent`, `requireRollbackOnly`, the `SupersededBeforeRollbackOnly` / `RollbackOnlyBeforeScopeConflict` subtests; every `DiscardScope(x, "")` → `DiscardScope(x)`), `spitest/entity.go` (any reference the grep finds)
- Modify: `CHANGELOG.md` (`[Unreleased]` → `### Fixed`)

**Interfaces:**
- Produces: `DiscardScope(ctx context.Context) error`. `ErrTxRollbackOnly` and `RollbackOnlyError` no longer exist. `CompleteScope` refuses with `ErrCalloutSuperseded`, then `ErrScopeConflict`. `Seal` fails when a `DiscardScope` of the transaction returned an error (any error type; cyoda-go answers it as a 5xx).

- [ ] **Step 1:** Replace the "# Rollback-only" doc section with "# A discard that fails": *A coordinator must never let Seal succeed after a DiscardScope of the transaction that returned an error: the scope's writes may still be part of the transaction. For example, it keeps the scope unresolved so that Seal fails, or Seal waits until the scope's lease expires and then fails.* Drop every mark sentence from the method docs; `DiscardScope`'s doc keeps "a scope whose completion was refused is allowed; removes nothing more".
- [ ] **Step 2:** Delete `ErrTxRollbackOnly`, `RollbackOnlyError` and its methods; change the signature; fix `spitest` call sites and delete the mark cases.
- [ ] **Step 3:** `go vet ./... && go test ./...` in the SPI worktree. Expected: ok (spitest compiles; it does not run here).
- [ ] **Step 4:** CHANGELOG `[Unreleased]` / `### Fixed`: "`TransactionCoordinator` no longer marks scopes rollback-only. `DiscardScope(ctx)` drops `failedCode`; `ErrTxRollbackOnly` and `RollbackOnlyError` are removed; the `Coordination/RollbackOnly*` cases are gone. A coordinator must make `Seal` fail after any `DiscardScope` that returned an error." Commit; push branch; open PR into SPI `release/v0.9.1`.

### Task 2: cyoda-go — the scope ends without a mark

**Files:**
- Modify: `internal/txcoord/end.go`, `inprocess.go`, `durable.go`, `txcoord.go`, `txcoordtest/double.go`
- Modify: `internal/common/txcoord_errors.go` (delete `ErrRollbackOnly`, `TransactionRollbackOnly`, `TransactionRollbackOnlyJoined`), `txcoord_errors_test.go`
- Modify: `internal/callout/coordinator.go` (the `common.ErrRollbackOnly` cancel case and the mark comments at ~165-192, ~337, ~479-501)
- Modify: `internal/domain/workflow/engine_processors.go` (comments ~138, ~284: no mark to absorb), `internal/domain/entity/txscope.go:97` comment, `internal/httpmw/routeclass.go` comment (class still decides row 8)
- Modify: `e2e/parity/callback_scope.go`, `e2e/parity/callback_joined_commit.go`, `e2e/parity/registry.go`, `e2e/parity/multinode/callout_handover.go`, `multinode/callback_joined_commit.go`, `multinode/txctl_joined_forward.go`
- Test: `internal/txcoord/end_test.go`, `inprocess_test.go`, `durable_test.go`, `internal/callout/coordinator_test.go`, `internal/domain/txjoin/run_test.go`, `internal/grpc/txroute_interceptor_test.go`, `internal/httpmw/txjoin_mw_test.go`, `internal/domain/entity/txscope_test.go`, `internal/domain/workflow/engine_seal_test.go`

**Interfaces:**
- Consumes: Task 1's `DiscardScope(ctx)`.
- Produces: `decide(e ending, class RequestClass, f *failure) (*common.AppError, error)`; `ending.complete(class) (*common.AppError, error)`; `ending.discard() error`; `standing() (superseded error, failed error)`; `inProcess.leave(s *scope, broken error)`; `parentMark` type deleted. `common.ErrCodeTransactionRollbackOnly` still exists after this task (Task 4 removes it).

- [ ] **Step 1: RED.** In `end_test.go` replace `TestEnd_Row7_NonRetryableWriteMarksParent` and `TestEnd_Row7_EmptyCodeStillMarks` with `TestEnd_Row7_NonRetryableWriteDiscardsNoMark` (a write answered non-retryable 4xx: the handler's answer is returned unchanged, the scope is discarded, and after it a new scope under the same pass opens and completes and `Seal` succeeds). In `inprocess_test.go` add `TestSeal_AfterFailedJoinedWriteSucceeds`. Run `go test ./internal/txcoord/ -run 'Row7|AfterFailedJoinedWrite'`: FAIL (409 refusal / marked).
- [ ] **Step 2:** Remove the mark: delete row 4; row 7 becomes "discarded" like row 6; `standing()` loses `code`/`marked`; in-process drops `scope.marked`, `failedCode`, `markLocked`, `markedLocked`, `markedCode`, the marked refusal in enter (`inprocess.go:444`), the cancel in BeginCallout (`:200`), `sealRefusal`'s mark branch, `rollbackOnly`, `lostRaceError`'s mark context; durable drops every `spi.ErrTxRollbackOnly` case and `failedCodeOf`; processor-scope `Close` keeps discard-on-error / complete-on-success. The double implements the new SPI obligation (a failed `DiscardScope` makes `Seal` fail).
- [ ] **Step 3:** Rewrite or delete the unit tests that pin a mark: delete `TestEnd_Row4_*`, `TestEnd_Row1_SupersededWithMarkedDescendantNoClimb`, `TestEnter_RefusedUnderMarkedAncestor`, `TestMark_*`, `TestSeal_LostRaceBeforeMark`, `TestCheck_LostRaceBeforeMark`, `TestOpenChild_CloseMarked*`, `TestCheck_CalloutContextReportsItsParentsMark`, `TestDurable_AdvanceStopsACalloutOfAMarkedScope`, `TestDurable_SealLostRaceBeforeMark`, `TestCoordinator_RollbackOnlyCauseMapsTo409`, `TestCoordinator_AnsweredAfterTheMark_FailsRollbackOnly`, `TestCoordinator_FailedAfterTheMark_FailsWithTheScopesRefusal`, `TestTxRoute_RawHandlerErrorMarks` (→ `TestTxRoute_RawHandlerErrorDiscards`: next request served), the mark cases in `txjoin/run_test.go`. Rename `TestEnd_Row5_ReleaseFails500AndMarks` → `…500NoMark`, `TestEnd_PanicOutcome_DiscardsAndMarks` → `TestEnd_PanicOutcome_DiscardsNoMark`, `TestDurable_FailedWriteOnAnotherPnodeDoomsTheOwner` → `…OwnerCommits`. Add `TestEnd_Row2_LostRaceBeatsNonRetryableWrite` and `TestDurable_DiscardFailureFailsSeal`.
- [ ] **Step 4: parity.** `RunCallbackScope_FailedJoinedWriteDoomsOwner` → `RunCallbackScope_FailedJoinedWriteHandledByProcessor` (member's write fails 4xx, member answers success; owner 200; entity of the failed write absent, the member's later write present); `RunCallbackScope_AsyncNewTxAbsorbs` → `RunCallbackScope_AsyncNewTxFailedCallbackWriteHandled` (processor kept); `RunCallbackJoinedCommitBeforeDispatchRefused`: the callback still gets 409 `COMMIT_IN_JOINED_TRANSACTION`; the owner's outcome follows the member's answer; multinode: delete `Callout_HandOverStopsOnMark`, adjust the other two files' assertions. Update `registry.go` names.
- [ ] **Step 5:** `go vet ./... && go test ./internal/txcoord/... ./internal/callout/... ./internal/domain/... ./internal/grpc/... ./internal/httpmw/... ./internal/common/...`, then `make test`. Expected: all pass, parity suites executed.
- [ ] **Step 6:** Commit (`fix(txcoord): a failed callback write no longer makes the transaction rollback-only`).

### Task 3: e2e — the processor decides

**Files:**
- Modify: `internal/e2e/callback_scope_test.go`, `callback_scope_engine_test.go`, `callback_scope_nested_test.go`, `callback_scope_owner_test.go`, `callback_joined_commit_test.go`, `tx_lifecycle_e2e_test.go`, `transaction_control_test.go`

**Interfaces:** Consumes Task 2's behaviour; uses the existing `callbackHarness`.

- [ ] **Step 1: tests.** Replace `TestCallbackScope_FailedJoinedWrite_OwnerRollbackOnly_HTTP/_GRPC` with `TestCallbackScope_FailedJoinedWrite_ProcessorHandlesIt_HTTP/_GRPC` (callback create answered `409 UNIQUE_VIOLATION`; member then writes another entity and answers success; owner `200`; failed entity absent, other present, owner's own write present) and add `TestCallbackScope_FailedJoinedWrite_ProcessorFails_HTTP` (member answers failure; owner fails as a failed SYNC processor does today — assert that status/code, nothing of the callbacks kept). `TestCallbackScope_FailedWrite_LaterRequestsOnThePassRefused` → `…Served` (read and write after the failure answer 2xx). `TestCallbackScope_WriteOverResponseCeiling_413AndMarked` → `…413Discarded` (owner commits when the member answers success). `TestCallbackScope_AsyncNewTxAbsorbsFailedCallbackWrite` → `TestCallbackScope_AsyncNewTx_FailedCallbackWriteHandled`. `TestCallbackScope_MarkedByDescendant_ClimbsToTheOwner` → `TestCallbackScope_FailedWriteOfDescendant_StaysWithIt`. `TestCallback_CommitBeforeDispatchInJoinedTransaction_OwnerRollbackOnly` → `…_ProcessorHandlesIt` (member makes the write without the token and answers success; owner commits). `TestE2E_JoinedCallbackFailure_OwnerRollbackOnly` → `…_OwnerCommits`. Delete `TestCallbackScope_OwnerOps_RootMarked`, `TestCallbackScope_ScheduledFire_RootMarked`, `TestCallbackScope_MarkStopsFurtherTries`; fix the remaining `TRANSACTION_ROLLBACK_ONLY` assertions the grep finds in `transaction_control_test.go`.
- [ ] **Step 2:** Prove RED once on the true base: a scratch worktree at `origin/release/v0.9.1` (before Task 2) with the new test file copied in; the new tests must fail there with the owner's `409 TRANSACTION_ROLLBACK_ONLY` (record which). Then run them on this branch: `go test -timeout 30m -run 'CallbackScope|Callback_CommitBeforeDispatch|JoinedCallbackFailure|TransactionControl' ./internal/e2e/...`. Expected: PASS.
- [ ] **Step 3:** Commit (`test(e2e): a failed callback write leaves the decision to the processor`).

### Task 4: back out the `TRANSACTION_ROLLBACK_ONLY` code

**Files:**
- Modify: `internal/common/error_codes.go` (const and its set entry), `api/openapi.yaml` (component `responses/TransactionRollbackOnly`, every `$ref` to it, the four response-description sentences at ~858, ~1068, ~1262, ~2125), `api/generated.go` (`go generate ./api`), `internal/e2e/errorcodematrix/matrix.go` (the five cells)
- Delete: `cmd/cyoda/help/content/errors/TRANSACTION_ROLLBACK_ONLY.md`; Modify `cmd/cyoda/help/content/errors.md` (its row)

- [ ] **Step 1:** Delete and regenerate; where a `"409"` response held only that `$ref`, drop the response key.
- [ ] **Step 2:** `go vet ./... && go test ./internal/common/... ./internal/e2e/errorcodematrix/... ./api/... ./cmd/...` (TestErrCode_Parity, the matrix-vs-spec test, codegen sync). Expected: PASS. `grep -rn 'ROLLBACK_ONLY\|RollbackOnly' --include='*.go' --include='*.yaml' .` → empty.
- [ ] **Step 3:** Commit (`fix(api): remove TRANSACTION_ROLLBACK_ONLY`).

### Task 5: docs and parity record

**Files:**
- Modify help: `cmd/cyoda/help/content/workflows.md` (~216, ~325 "A callback that failed"), `cluster.md:172`, `grpc.md:241,244`, `crud.md:350`, `errors/COMMIT_IN_JOINED_TRANSACTION.md:28`, `errors/JOINED_RESPONSE_TOO_LARGE.md:28`, `errors/TRANSACTION_BUSY.md`
- Modify: `docs/ARCHITECTURE.md` (§ "Marks" ~648, §3.8, the `ASYNC_NEW_TX` row ~2110), `docs/PROCESSOR_EXECUTION_MODES.md` (~136 and every mark sentence)
- Modify: `docs/cloud-parity/callback-scopes.md`, `commit-in-joined-transaction.md`, `callout-failover.md`, `README.md` rows 69-70
- Modify: `CHANGELOG.md` (new `[Unreleased]` → `### Fixed`), `COMPATIBILITY.md` (pin row text for SPI v0.9.1 when Task 6 pins)
- Not touched: `docs/release-notes/v0-9-0.md`, `CHANGELOG.md` `[0.9.0]`, `docs/superpowers/**` (history)

- [ ] **Step 1:** State the rule once, everywhere it appears: *A failed callback keeps nothing, and the transaction goes on whatever the failure: a retryable answer may be sent again under the same token; an answer that is not retryable is the member's to handle — its processor (criterion, schedule function) decides whether the operation fails. An answer whose outcome is unknown (`503 TRANSACTION_NODE_UNAVAILABLE` after the request may have reached the owner, or no answer at all) does not tell the member whether its write was kept: the processor must then fail.* `COMMIT_IN_JOINED_TRANSACTION`: the refused write keeps nothing; the member may make it as an independent request and go on.
- [ ] **Step 2:** `grep -rn -i 'rollback-only\|ROLLBACK_ONLY\|marks the\|is marked' cmd/cyoda/help docs/ARCHITECTURE.md docs/PROCESSOR_EXECUTION_MODES.md docs/cloud-parity api/openapi.yaml` → only intended hits (none for the callback mark). `go test ./cmd/...` (help tree tests).
- [ ] **Step 3:** CHANGELOG `### Fixed`: "**A failed callback write no longer dooms the transaction.** A callback that fails keeps nothing and the transaction goes on: the compute member gets the answer and its processor decides whether the operation fails. Before, a write that failed with an answer that was not retryable made the operation fail with `409 TRANSACTION_ROLLBACK_ONLY` whatever the member answered; that code is gone." Commit.

### Task 6: pins

- [ ] **Step 1:** After Task 1's SPI PR merges into SPI `release/v0.9.1`: in one commit, pseudo-pin `github.com/cyoda/cyoda-go-spi` to that commit in root `go.mod` and `make repin-plugins`; `make check-spi-pin-sync`; `go mod tidy` where MAINTAINING.md says. Stage `go.mod`/`go.sum` files by name (go.work stays uncommitted).
- [ ] **Step 2:** `make test`; push; open the cyoda-go PR into `release/v0.9.1` with milestone v0.9.1.

### After the tasks (not subagent work)

- Comment on CP-3995 (Cloud: do not build the mark; the code is removed), cyoda/cyoda-go-cassandra#121 (no mark in the durable coordinator; the failed-discard obligation instead), and close #737 by hand on merge.
- Forward-port to `release/v0.10.0` and SPI `main` after 0.9.1 merges.
- Gates: whole-branch code review (fresh reviewer) and `cyoda-go-security-audit` on the exact PR head.
