package workflow

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
)

// ErrCommitBeforeDispatchInfra wraps every infrastructure-layer failure
// (begin / commit / store-factory) inside a COMMIT_BEFORE_DISPATCH segment
// boundary. Handlers use errors.Is(err, ErrCommitBeforeDispatchInfra) to
// distinguish these from processor-domain failures and map them to a
// sanitized 5xx (with ticket UUID) instead of leaking internal text via
// 4xx WORKFLOW_FAILED. Processor errors and CAS conflicts (spi.ErrConflict)
// are NOT wrapped — they remain client-attributable and stay 4xx.
var ErrCommitBeforeDispatchInfra = errors.New("commit-before-dispatch infrastructure failure")

// ErrPostSegmentConflict marks a CAS conflict raised AFTER a
// COMMIT_BEFORE_DISPATCH segment has committed and its external dispatch has
// fired — the apply-result CAS below. It is not a precondition failure a caller
// can isolate and skip past: the segment that would have carried the rest of the
// work is gone, and the caller's cascade cursor was never advanced.
//
// A conflict from the FIRST-segment flush is deliberately left unmarked. That one
// happens before any commit and before any dispatch, so it is cleanly isolable —
// which is the whole distinction this sentinel exists to draw.
//
// Chained alongside the conflict, never in place of it, so
// errors.Is(err, spi.ErrConflict) stays true and the single-entity 412 mapping is
// unaffected.
var ErrPostSegmentConflict = errors.New("conflict after a committed segment")

// clientAttributableStoreErr reports whether a store error is an outcome the
// caller can act on — a stale precondition, a unique-key clash — as opposed to
// an infrastructure failure that merely surfaced through the same call.
//
// It is a deliberate allow-list rather than an "is this pgx?" heuristic,
// because the consequence of guessing wrong runs one way only: an unrecognised
// error reaches the entity service's catch-all, which mints a 400
// WORKFLOW_FAILED whose detail is the raw text — driver wording, table,
// constraint and index names, a SQLSTATE. The three sentinels below are exactly
// the ones that classifier (and common.Internal underneath it) maps to a
// specific status; anything else has no client-facing meaning to preserve.
//
// The list is what keeps the segment-boundary CAS sites honest in BOTH
// directions: marking an infrastructure failure is required (it is the leak),
// and marking a caller's stale If-Match is forbidden — UpdateEntityCollection
// excludes ErrCommitBeforeDispatchInfra from per-item isolation, so a
// mis-marked precondition failure would abort the whole request and take its
// successful siblings with it.
func clientAttributableStoreErr(err error) bool {
	return errors.Is(err, spi.ErrConflict) ||
		errors.Is(err, spi.ErrUniqueViolation) ||
		errors.Is(err, spi.ErrPartialUniqueKey)
}

// executeProcessors runs each processor in the transition's processor pipeline
// sequentially. Processors are dispatched according to their ExecutionMode:
// ASYNC_NEW_TX runs within a savepoint (failures are non-fatal); SYNC and
// ASYNC_SAME_TX run inline in the caller's transaction context;
// COMMIT_BEFORE_DISPATCH commits the current segment before dispatch and
// continues the cascade in a fresh segment.
//
// The function returns the (possibly mutated) ctx and txID that subsequent
// processors / transitions should use. For SYNC / ASYNC_SAME_TX / ASYNC_NEW_TX
// these are unchanged from the inputs; for COMMIT_BEFORE_DISPATCH the segment
// boundary shifts (currentCtx, currentTxID) to TX_post.
//
// Per spec §8: every audit event for a single cascade carries the cascade-entry
// txID for client-correlation continuity, regardless of which segment commits
// the event.
//
// This function does not release segments on a failure return: it hands the
// current (ctx, txID) back on every path, so the caller always knows which
// segment is open and the entry-point guard in engine.go releases it — a guard
// that also covers the criterion failures and store errors invisible from here.
// The deferred guard below covers only the panic case, where no caller ever gets
// that hand-back.
func (e *Engine) executeProcessors(ctx context.Context, processors []spi.ProcessorDefinition, entity *spi.Entity, auditStore spi.StateMachineAuditStore, workflow string, transition string, txID string) (retCtx context.Context, retTxID string, retErr error) {
	if len(processors) == 0 {
		return ctx, txID, nil
	}

	// Record processing pause (in TX_pre, with cascade-entry txID — correct).
	names := make([]string, len(processors))
	for i, p := range processors {
		names[i] = p.Name
	}
	e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, entity.Meta.State,
		spi.SMEventProcessingPaused,
		fmt.Sprintf("Paused for processors: %v", names), nil)

	// Resolved lazily and at most once across the pipeline — see modelDescMemo.
	desc := &modelDescMemo{}

	currentCtx := ctx
	currentTxID := txID

	// Panic-only guard (see rollbackSegment). A later processor in this pipeline
	// can blow up while TX_post is live, before any caller has seen it.
	defer func() {
		if retCtx == nil {
			e.rollbackSegment(currentCtx, currentTxID, txID)
		}
	}()

	for _, proc := range processors {
		// Execution-location axis. Rejection is fatal and self-contained:
		// emit the per-processor SMEventStateProcessResult audit row
		// explicitly (mirroring the post-dispatch emit lower in this loop),
		// then return. The post-dispatch abort gate keys on
		// proc.ExecutionMode and would silently swallow the rejection if
		// proc.ExecutionMode == ExecutionModeAsyncNewTx, so the rejection
		// must short-circuit the loop entirely.
		if proc.Type == ProcessorTypeInternalized {
			auditData := map[string]any{
				"success": false,
				"mode":    proc.ExecutionMode,
			}
			e.recordEvent(auditStore, currentCtx, entity.Meta.ID, txID, entity.Meta.State,
				spi.SMEventStateProcessResult,
				fmt.Sprintf("Processor %q completed", proc.Name), auditData)
			return currentCtx, currentTxID, fmt.Errorf(
				"processor %s failed: execution type %q is not yet implemented",
				proc.Name, proc.Type)
		}

		var success bool
		var procErr error

		switch proc.ExecutionMode {
		case ExecutionModeAsyncNewTx:
			procErr = e.executeAsyncNewTx(currentCtx, entity, proc, workflow, transition, currentTxID)
			success = procErr == nil

			// ASYNC_NEW_TX failures are non-fatal: log warning, continue pipeline.
			if procErr != nil {
				slog.Warn("ASYNC_NEW_TX processor failed, continuing pipeline",
					"pkg", "workflow", "processor", proc.Name, "error", procErr)
			}

		case ExecutionModeCommitBeforeDispatch:
			var nCtx context.Context
			var nTxID string
			nCtx, nTxID, procErr = e.executeCommitBeforeDispatch(currentCtx, entity, desc, proc, workflow, transition, currentTxID, auditStore, txID)
			success = procErr == nil
			if procErr == nil {
				currentCtx = nCtx
				currentTxID = nTxID
			}

		default: // SYNC, ASYNC_SAME_TX — both inline in caller's transaction.
			procErr = e.executeSyncProcessor(currentCtx, entity, desc, proc, workflow, transition, currentTxID)
			success = procErr == nil
		}

		auditData := map[string]any{
			"success": success,
			"mode":    proc.ExecutionMode,
		}
		// Note: we deliberately do NOT include procErr.Error() in audit data —
		// engine-wrapped CBD error strings ("commit-before-dispatch: commit
		// TX_pre: <pgx-error>") would leak internals to same-tenant audit
		// readers. The success=false flag is sufficient for clients; the
		// request-scoped slog log captures full error detail for operators.
		// Per spec §8: audit events use the cascade-entry txID for
		// correlation continuity, even though they physically land in
		// whichever segment's TX is current. The event still records via
		// currentCtx so that pre-segment-boundary events (in TX_pre) write
		// to TX_pre's buffer and post-segment-boundary events write to
		// TX_post's buffer.
		e.recordEvent(auditStore, currentCtx, entity.Meta.ID, txID, entity.Meta.State,
			spi.SMEventStateProcessResult,
			fmt.Sprintf("Processor %q completed", proc.Name), auditData)

		// For SYNC/ASYNC_SAME_TX/COMMIT_BEFORE_DISPATCH, failure kills the pipeline.
		if procErr != nil && proc.ExecutionMode != ExecutionModeAsyncNewTx {
			return currentCtx, currentTxID, fmt.Errorf("processor %s failed: %w", proc.Name, procErr)
		}
	}
	return currentCtx, currentTxID, nil
}

// executeSyncProcessor runs a SYNC or ASYNC_SAME_TX processor inline in the
// caller's transaction. On success the entity's Data is updated with the
// processor's returned modifications.
func (e *Engine) executeSyncProcessor(ctx context.Context, entity *spi.Entity, desc *modelDescMemo, proc spi.ProcessorDefinition, workflow, transition, txID string) error {
	if e.extProc == nil {
		return nil
	}
	// Release any per-tx gate this call chain holds across the blocking dispatch
	// (H3 invariant, generalised to the joined-callback path): the dispatch
	// touches no local buffer but can re-enter with a descendant joined callback
	// on the same txID, which would otherwise deadlock waiting for the gate this
	// chain holds. resume() re-acquires before we apply the processor's result.
	// No-op for the owner and for plain non-joined calls. Deferred resume keeps
	// the re-acquire panic-safe; the explicit call re-acquires before touching
	// entity so the buffer write below is gated.
	resume := txgate.Suspend(ctx)
	defer resume()
	modifiedEntity, err := e.extProc.DispatchProcessor(ctx, entity, proc, workflow, transition, txID)
	resume()
	if err != nil {
		return err
	}
	if modifiedEntity != nil && modifiedEntity.Data != nil {
		return e.applyProcessorData(ctx, entity, desc, modifiedEntity.Data)
	}
	return nil
}

// executeAsyncNewTx runs an ASYNC_NEW_TX processor within a savepoint. The
// processor's returned entity modifications are intentionally discarded —
// ASYNC_NEW_TX processors perform side-effects only. On dispatch failure the
// savepoint is rolled back and the error is returned; on success the savepoint
// is released.
func (e *Engine) executeAsyncNewTx(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition, workflow, transition, txID string) error {
	if e.extProc == nil {
		return nil
	}

	// Without a transaction manager, fall back to plain dispatch (no savepoint).
	if e.txMgr == nil {
		// Release any per-tx gate this chain holds across the blocking dispatch
		// (H3 invariant) — this dispatch reuses the gated txID and can re-enter
		// with a descendant joined callback on the same tx, so holding the gate
		// here deadlocks exactly like the SYNC path.
		resume := txgate.Suspend(ctx)
		defer resume()
		_, err := e.extProc.DispatchProcessor(ctx, entity, proc, workflow, transition, txID)
		resume()
		return err
	}

	spID, err := e.txMgr.Savepoint(ctx, txID)
	if err != nil {
		return fmt.Errorf("savepoint creation failed: %w", err)
	}

	// Release the per-tx gate across the blocking dispatch (H3 invariant). The
	// savepoint create above and the rollback/release below are buffer ops that
	// must stay gated, so suspend only spans the callout: re-acquire before
	// touching the savepoint again. No-op for the owner / non-joined calls.
	resume := txgate.Suspend(ctx)
	defer resume()
	_, dispatchErr := e.extProc.DispatchProcessor(ctx, entity, proc, workflow, transition, txID)
	resume()
	if dispatchErr != nil {
		if rbErr := e.txMgr.RollbackToSavepoint(ctx, txID, spID); rbErr != nil {
			slog.Warn("failed to rollback savepoint after processor error",
				"pkg", "workflow", "processor", proc.Name,
				"savepointID", spID, "rollbackError", rbErr)
		}
		return dispatchErr
	}

	if err := e.txMgr.ReleaseSavepoint(ctx, txID, spID); err != nil {
		return fmt.Errorf("savepoint release failed: %w", err)
	}
	return nil
}

// executeCommitBeforeDispatch implements processor execution mode
// COMMIT_BEFORE_DISPATCH (issue #27). The cascade's parent transaction
// (txID == T_pre) is committed first; the processor is dispatched with no
// transaction context (default) or with TX_post's token
// (startNewTxOnDispatch=true); the result is applied via CompareAndSave
// against T_pre. The caller MUST replace its (ctx, txID) with the returned
// (newCtx, newTxID) to continue the cascade in TX_post.
//
// Per spec §3, §10.3: in the startNewTxOnDispatch=true branch, processors
// must not save the cascade-anchor entity themselves AND also return
// mutations for it (last-writer-wins inside TX_post's buffer).
func (e *Engine) executeCommitBeforeDispatch(ctx context.Context, entity *spi.Entity, desc *modelDescMemo, proc spi.ProcessorDefinition, workflow, transition, txID string, auditStore spi.StateMachineAuditStore, entryTxID string) (newCtx context.Context, newTxID string, err error) {
	tPre := txID
	// This function opens TX_post and can panic before handing it to
	// executeProcessors. Its own guard covers that window; the entry-point guard
	// covers everything after the hand-off.
	segCtx, segTxID := ctx, txID
	segHandedOff := false
	defer func() {
		if !segHandedOff {
			e.rollbackSegment(segCtx, segTxID, txID)
		}
	}()
	// Staged here rather than assigned: in the startNewTxOnDispatch=false
	// branch, ctx still carries the committed TX_pre at the point the result
	// arrives, so a schema extension would run outside any transaction. Both
	// branches converge below on TX_post, which is the correct place to check.
	var pending []byte

	// Read the flag. Nil pointer == default == false.
	startNewTx := proc.Config.StartNewTxOnDispatch != nil && *proc.Config.StartNewTxOnDispatch

	// Per spec §4.1: a caller-supplied If-Match expected-txID (single-shot,
	// stashed via ManualTransitionWithIfMatch) is applied to the FIRST
	// segment-flush of the cascade — i.e. this exact call's pre-dispatch
	// flush. Consume here so subsequent CBD segments in the same cascade fall
	// back to the chained-CAS path against the prior segment's commit-stamped
	// txID.
	expectedFirstFlushTxID, ifMatchConsumed := consumeIfMatch(ctx)

	if startNewTx {
		// =true: commit TX_pre, begin TX_post, dispatch with TX_post token,
		// apply result in TX_post.
		newTxID, newCtx, err = e.commitAndBeginNextSegment(ctx, entity, txID, expectedFirstFlushTxID, ifMatchConsumed)
		// Advance before checking err so the deferred rollback always targets the
		// segment actually open. commitAndBeginNextSegment returns ("", nil, err)
		// on failure, so segTxID becomes "" and rollbackSegment no-ops — correct,
		// since no segment was opened.
		segCtx, segTxID = newCtx, newTxID
		if err != nil {
			// Reviewer S1 (#228): if the engine's first-segment flush rejected
			// the caller's IfMatch precondition we have already recorded
			// entry-side audit events (STATE_MACHINE_START, WORKFLOW_FOUND).
			// Emit a compensating TRANSITION_ABORTED so the audit trail
			// remains self-consistent. Best-effort — auditStore is the
			// engine's own handle so this lands in the same TX buffer as the
			// entry events (rolls back together with them on a chunk-wide
			// rollback, commits together on per-item-isolated paths).
			if ifMatchConsumed && errors.Is(err, spi.ErrConflict) {
				e.recordAbortForIfMatchConflict(ctx, auditStore, entity, entryTxID, transition, expectedFirstFlushTxID)
			}
			return nil, "", err
		}

		if e.extProc != nil {
			modified, dispatchErr := e.extProc.DispatchProcessor(newCtx, entity, proc, workflow, transition, newTxID)
			if dispatchErr != nil {
				return nil, "", dispatchErr
			}
			if modified != nil && modified.Data != nil {
				pending = modified.Data
			}
		}
	} else {
		// =false: Save+Commit TX_pre, dispatch outside any transaction, then
		// begin a fresh TX_post for the apply-result phase.
		//
		// We deliberately don't reuse commitAndBeginNextSegment here — that
		// helper opens a TX immediately after committing, which would leak
		// TX_post's token into the dispatch context. Splitting Save+Commit
		// from Begin keeps both modes clean.
		if fcErr := e.flushAndCommitSegment(ctx, entity, txID, expectedFirstFlushTxID, ifMatchConsumed); fcErr != nil {
			// See the matching block in the startNewTx==true branch above
			// for the rationale (#228 reviewer S1).
			if ifMatchConsumed && errors.Is(fcErr, spi.ErrConflict) {
				e.recordAbortForIfMatchConflict(ctx, auditStore, entity, entryTxID, transition, expectedFirstFlushTxID)
			}
			return nil, "", fcErr
		}

		// Dispatch with NO tx token in ctx. context.WithoutCancel preserves
		// values (tenant/user) but strips cancellation; we additionally
		// detach the tx token so the processor sees no transaction.
		dispatchCtx := spi.WithTransaction(context.WithoutCancel(ctx), nil)
		var modified *spi.Entity
		var dispatchErr error
		if e.extProc != nil {
			modified, dispatchErr = e.extProc.DispatchProcessor(dispatchCtx, entity, proc, workflow, transition, "")
		}
		if dispatchErr != nil {
			return nil, "", dispatchErr
		}
		if modified != nil && modified.Data != nil {
			pending = modified.Data
		}

		// Begin TX_post. Advance before checking err, same reasoning as the
		// startNewTx branch above.
		newTxID, newCtx, err = e.txMgr.Begin(context.WithoutCancel(ctx))
		segCtx, segTxID = newCtx, newTxID
		if err != nil {
			return nil, "", fmt.Errorf("commit-before-dispatch: begin TX_post: %w", errors.Join(ErrCommitBeforeDispatchInfra, err))
		}
	}

	if pending != nil {
		if applyErr := e.applyProcessorData(newCtx, entity, desc, pending); applyErr != nil {
			return nil, "", applyErr
		}
	}

	// Apply result via CAS against tPre — works in both branches.
	es, casErr := e.factory.EntityStore(newCtx)
	if casErr != nil {
		return nil, "", fmt.Errorf("commit-before-dispatch: get entity store for CAS: %w", errors.Join(ErrCommitBeforeDispatchInfra, casErr))
	}
	if _, saveErr := es.CompareAndSave(newCtx, entity, tPre); saveErr != nil {
		if !clientAttributableStoreErr(saveErr) {
			// Not a conflict at all — the store failed. Marked infra so the text
			// takes the sanitized-5xx path instead of the 4xx body below.
			// ErrPostSegmentConflict is dropped: the marker exists to say "a
			// CONFLICT landed past the commit", and the infra marker already
			// carries the only consequence a caller acts on (do not isolate this
			// item — the transaction it would continue in is gone).
			return nil, "", fmt.Errorf("commit-before-dispatch: apply result after dispatch: %w",
				errors.Join(ErrCommitBeforeDispatchInfra, saveErr))
		}
		// Both sentinels stay matchable — ErrConflict for the 412 mapping,
		// ErrPostSegmentConflict to tell a batching caller this landed on the far
		// side of TX_pre's commit. Chained rather than errors.Join'd because this
		// text reaches a 4xx response body verbatim, and a 4xx detail is one
		// `CODE: message` line (see .claude/rules/error-handling.md); Join would
		// put a newline through the middle of it.
		return nil, "", fmt.Errorf("%w: %w", ErrPostSegmentConflict, saveErr)
	}

	segHandedOff = true
	return newCtx, newTxID, nil
}

// flushAndCommitSegment is the shared primitive for the COMMIT_BEFORE_DISPATCH
// segment boundary's "flush + commit TX_pre" half. It writes the in-memory
// entity to txID's buffer (CompareAndSave when applyIfMatch is true,
// plain Save otherwise) and commits txID. The caller decides whether to Begin
// a new TX afterward (=true uses commitAndBeginNextSegment; =false splits the
// Begin around the dispatch).
//
// When applyIfMatch is true the flush uses CompareAndSave with expectedTxID,
// applying the caller's If-Match precondition (spec §4.1) before TX_pre
// commits and before any external dispatch fires. Client-attributable CAS
// failures (spi.ErrConflict and the unique-key sentinels) bubble unwrapped so
// the handler maps them to 412 / 409 / 422; the CAS's OTHER failure mode — the
// store itself — is marked infra like every other one below.
//
// Infrastructure failures (EntityStore lookup, CAS store failure, plain Save,
// Commit) are wrapped with ErrCommitBeforeDispatchInfra so
// classifyWorkflowError routes them to a sanitized 5xx with ticket UUID
// instead of leaking internal text via 4xx WORKFLOW_FAILED.
func (e *Engine) flushAndCommitSegment(ctx context.Context, entity *spi.Entity, txID, expectedTxID string, applyIfMatch bool) error {
	es, err := e.factory.EntityStore(ctx)
	if err != nil {
		return fmt.Errorf("commit-before-dispatch: get entity store: %w", errors.Join(ErrCommitBeforeDispatchInfra, err))
	}
	if applyIfMatch {
		if _, err := es.CompareAndSave(ctx, entity, expectedTxID); err != nil {
			if clientAttributableStoreErr(err) {
				return err // ErrConflict / unique-key outcomes bubble unwrapped
			}
			return fmt.Errorf("commit-before-dispatch: apply If-Match precondition: %w",
				errors.Join(ErrCommitBeforeDispatchInfra, err))
		}
	} else {
		if _, err := es.Save(ctx, entity); err != nil {
			return fmt.Errorf("commit-before-dispatch: flush pre-callout state: %w", errors.Join(ErrCommitBeforeDispatchInfra, err))
		}
	}
	if err := e.txMgr.Commit(ctx, txID); err != nil {
		return fmt.Errorf("commit-before-dispatch: commit TX_pre: %w", errors.Join(ErrCommitBeforeDispatchInfra, err))
	}
	return nil
}

// commitAndBeginNextSegment is the COMMIT_BEFORE_DISPATCH segment-boundary
// primitive for the startNewTxOnDispatch=true branch. It flushes the in-memory
// entity (via flushAndCommitSegment) and begins a fresh TX (TX_post). The
// caller continues the cascade in (newCtx, newTxID).
//
// On any failure after TX_pre commits, the segment may already be durable —
// the caller cannot rollback prior work. Infrastructure failures are wrapped
// with ErrCommitBeforeDispatchInfra; CAS conflicts bubble through unchanged
// so the handler can map them to 412.
func (e *Engine) commitAndBeginNextSegment(ctx context.Context, entity *spi.Entity, txID, expectedTxID string, applyIfMatch bool) (newTxID string, newCtx context.Context, err error) {
	if fcErr := e.flushAndCommitSegment(ctx, entity, txID, expectedTxID, applyIfMatch); fcErr != nil {
		return "", nil, fcErr
	}
	newTxID, newCtx, err = e.txMgr.Begin(context.WithoutCancel(ctx))
	if err != nil {
		return "", nil, fmt.Errorf("commit-before-dispatch: begin TX_post: %w", errors.Join(ErrCommitBeforeDispatchInfra, err))
	}
	return newTxID, newCtx, nil
}
