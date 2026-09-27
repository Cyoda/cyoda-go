package workflow

import (
	"context"
	"fmt"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// SMEventTransitionAborted is the audit event the engine records when a
// caller's If-Match rejects a transition it has started. It pairs the
// STATE_MACHINE_START recorded just before it, so the audit trail holds no
// start without a matching finish or abort.
//
// The event's Data payload carries:
//
//	{
//	  "reason":         "ENTITY_MODIFIED",   // why the transition aborted
//	  "transitionName": "<name>",            // the transition that aborted
//	  "expectedTxId":   "<supplied-txid>",   // the caller's If-Match
//	  "actualTxId":     "<entity-txid>",     // the version the request starts from
//	}
//
// This is a cyoda-go local extension to the spi.StateMachineEventType
// open-string taxonomy; the audit handler emits eventType as a raw string.
const SMEventTransitionAborted spi.StateMachineEventType = "TRANSITION_ABORTED"

// TransitionAbortedReasonEntityModified is the only reason today: a stale
// If-Match.
const TransitionAbortedReasonEntityModified = "ENTITY_MODIFIED"

// IfMatch is a caller's If-Match precondition. It states the version the
// request starts from. Expected is the caller's value; Current is the
// transaction id of the entity as the request's transaction read it before the
// engine ran. An empty Expected is no precondition.
//
// The engine checks it once, at the start of the transition, and never again:
// a write to the entity later in the same transaction — a joined callback's,
// or a COMMIT_BEFORE_DISPATCH segment's — is the request's own and does not
// break it. A write another transaction commits after the request's read is
// not the precondition's business: the transaction's commit refuses it, as it
// refuses every such race.
type IfMatch struct {
	Expected string
	Current  string
}

// checkIfMatch records TRANSITION_ABORTED and fails with spi.ErrConflict when
// the caller's If-Match does not name the version the request starts from. The
// conflict is unmarked: it is the request's own precondition, which a batching
// caller isolates to its item.
func (e *Engine) checkIfMatch(ctx context.Context, auditStore spi.StateMachineAuditStore, entity *spi.Entity, txID, transitionName string, p IfMatch) error {
	if p.Expected == "" || p.Expected == p.Current {
		return nil
	}
	e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, entity.Meta.State,
		SMEventTransitionAborted,
		fmt.Sprintf("Transition %q aborted: entity has been modified since last read", transitionName),
		map[string]any{
			"reason":         TransitionAbortedReasonEntityModified,
			"transitionName": transitionName,
			"expectedTxId":   p.Expected,
			"actualTxId":     p.Current,
		})
	return fmt.Errorf("entity %s: If-Match precondition failed: %w", entity.Meta.ID, spi.ErrConflict)
}
