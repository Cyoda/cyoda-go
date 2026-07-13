package workflow

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/cyoda-platform/cyoda-go/internal/txgate"
	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// ErrCommitBeforeDispatchInfra wraps every infrastructure-layer failure
// (begin / commit / store-factory) inside a COMMIT_BEFORE_DISPATCH segment
// boundary. Handlers use errors.Is(err, ErrCommitBeforeDispatchInfra) to
// distinguish these from processor-domain failures and map them to a
// sanitized 5xx (with ticket UUID) instead of leaking internal text via
// 4xx WORKFLOW_FAILED. Processor errors and CAS conflicts (spi.ErrConflict)
// are NOT wrapped — they remain client-attributable and stay 4xx.
var ErrCommitBeforeDispatchInfra = errors.New("commit-before-dispatch infrastructure failure")

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
// On a fatal processor failure (SYNC / ASYNC_SAME_TX / COMMIT_BEFORE_DISPATCH)
// AFTER the engine has segmented (currentTxID != txID), the engine rolls back
// the current open segment before returning. The caller can no longer reach
// the open TX (it only knows the original txID) — leaving it open would leak
// the connection until postgres' idle-in-transaction timeout reclaimed it.
func (e *Engine) executeProcessors(ctx context.Context, processors []spi.ProcessorDefinition, entity *spi.Entity, auditStore spi.StateMachineAuditStore, workflow string, transition string, txID string) (context.Context, string, error) {
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

	currentCtx := ctx
	currentTxID := txID

	for _, proc := range processors {
		// Execution-location axis. Rejection is fatal and self-contained:
		// emit the per-processor SMEventStateProcessResult audit row
		// explicitly (mirroring the post-dispatch emit lower in this loop),
		// roll back any open segment, then return. The post-dispatch abort
		// gate keys on proc.ExecutionMode and would silently swallow the
		// rejection if proc.ExecutionMode == ExecutionModeAsyncNewTx, so the
		// rejection must short-circuit the loop entirely.
		if proc.Type == ProcessorTypeInternalized {
			auditData := map[string]any{
				"success": false,
				"mode":    proc.ExecutionMode,
			}
			e.recordEvent(auditStore, currentCtx, entity.Meta.ID, txID, entity.Meta.State,
				spi.SMEventStateProcessResult,
				fmt.Sprintf("Processor %q completed", proc.Name), auditData)
			e.rollbackOpenSegmentOnFailure(currentCtx, currentTxID, txID, proc.Name)
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
			nCtx, nTxID, procErr = e.executeCommitBeforeDispatch(currentCtx, entity, proc, workflow, transition, currentTxID, auditStore, txID)
			success = procErr == nil
			if procErr == nil {
				currentCtx = nCtx
				currentTxID = nTxID
			}

		default: // SYNC, ASYNC_SAME_TX — both inline in caller's transaction.
			procErr = e.executeSyncProcessor(currentCtx, entity, proc, workflow, transition, currentTxID)
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
			// If the engine has already segmented (txID advanced past the
			// cascade-entry txID), the caller can't see the new TX — roll it
			// back here to avoid leaking an idle TX.
			e.rollbackOpenSegmentOnFailure(currentCtx, currentTxID, txID, proc.Name)
			return currentCtx, currentTxID, fmt.Errorf("processor %s failed: %w", proc.Name, procErr)
		}
	}
	return currentCtx, currentTxID, nil
}

// rollbackOpenSegmentOnFailure rolls back currentTxID iff the engine has
// segmented (currentTxID != entryTxID). The caller-side handler tracks the
// original entryTxID and will roll that back via its own deferred-rollback
// path; without this rollback the post-segment TX would leak until postgres'
// idle-in-transaction timeout reclaimed it.
func (e *Engine) rollbackOpenSegmentOnFailure(ctx context.Context, currentTxID, entryTxID, procName string) {
	if e.txMgr == nil || currentTxID == entryTxID || currentTxID == "" {
		return
	}
	if rbErr := e.txMgr.Rollback(ctx, currentTxID); rbErr != nil {
		slog.Warn("failed to rollback engine-opened segment after processor failure",
			"pkg", "workflow", "processor", procName,
			"txID", currentTxID, "rollbackError", rbErr)
	}
}

// executeSyncProcessor runs a SYNC or ASYNC_SAME_TX processor inline in the
// caller's transaction. On success the entity's Data is updated with the
// processor's returned modifications.
func (e *Engine) executeSyncProcessor(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition, workflow, transition, txID string) error {
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
		entity.Data = modifiedEntity.Data
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
		_, err := e.extProc.DispatchProcessor(ctx, entity, proc, workflow, transition, txID)
		return err
	}

	spID, err := e.txMgr.Savepoint(ctx, txID)
	if err != nil {
		return fmt.Errorf("savepoint creation failed: %w", err)
	}

	_, dispatchErr := e.extProc.DispatchProcessor(ctx, entity, proc, workflow, transition, txID)
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
func (e *Engine) executeCommitBeforeDispatch(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition, workflow, transition, txID string, auditStore spi.StateMachineAuditStore, entryTxID string) (newCtx context.Context, newTxID string, err error) {
	tPre := txID

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
				_ = e.txMgr.Rollback(newCtx, newTxID)
				return nil, "", dispatchErr
			}
			if modified != nil && modified.Data != nil {
				entity.Data = modified.Data
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
			entity.Data = modified.Data
		}

		// Begin TX_post.
		newTxID, newCtx, err = e.txMgr.Begin(context.WithoutCancel(ctx))
		if err != nil {
			return nil, "", fmt.Errorf("commit-before-dispatch: begin TX_post: %w", errors.Join(ErrCommitBeforeDispatchInfra, err))
		}
	}

	// Apply result via CAS against tPre — works in both branches.
	es, casErr := e.factory.EntityStore(newCtx)
	if casErr != nil {
		_ = e.txMgr.Rollback(newCtx, newTxID)
		return nil, "", fmt.Errorf("commit-before-dispatch: get entity store for CAS: %w", errors.Join(ErrCommitBeforeDispatchInfra, casErr))
	}
	if _, saveErr := es.CompareAndSave(newCtx, entity, tPre); saveErr != nil {
		_ = e.txMgr.Rollback(newCtx, newTxID)
		return nil, "", saveErr // ErrConflict bubbles through unchanged
	}

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
// commits and before any external dispatch fires. CAS failures (incl.
// spi.ErrConflict) bubble unwrapped so the handler maps them to 412.
//
// Infrastructure failures (EntityStore lookup, plain Save, Commit) are
// wrapped with ErrCommitBeforeDispatchInfra so classifyWorkflowError routes
// them to a sanitized 5xx with ticket UUID instead of leaking internal text
// via 4xx WORKFLOW_FAILED.
func (e *Engine) flushAndCommitSegment(ctx context.Context, entity *spi.Entity, txID, expectedTxID string, applyIfMatch bool) error {
	es, err := e.factory.EntityStore(ctx)
	if err != nil {
		return fmt.Errorf("commit-before-dispatch: get entity store: %w", errors.Join(ErrCommitBeforeDispatchInfra, err))
	}
	if applyIfMatch {
		if _, err := es.CompareAndSave(ctx, entity, expectedTxID); err != nil {
			return err // ErrConflict / domain errors bubble unwrapped
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
