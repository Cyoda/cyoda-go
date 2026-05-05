package workflow

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// SMEventTransitionAborted is the compensating audit event emitted when an
// in-flight transition is aborted by a stale ifMatch precondition.
//
// Whenever the engine has already recorded entry-side audit events (e.g.
// STATE_MACHINE_START, WORKFLOW_FOUND) for a transition that is then
// short-circuited by a CompareAndSave conflict — at the engine's
// COMMIT_BEFORE_DISPATCH first-segment flush, or at the handler's
// post-engine save — we emit this event so the audit trail remains
// self-consistent (no orphaned start without a matching finish or abort).
//
// The event's Data payload carries:
//
//	{
//	  "reason":         "ENTITY_MODIFIED",   // why the abort fired
//	  "transitionName": "<name>",            // the transition that aborted
//	  "expectedTxId":   "<supplied-txid>",   // caller's stale ifMatch value
//	  "actualTxId":     "<row-current-txid>",// entity row's actual txID
//	}
//
// This is a cyoda-go local extension to the spi.StateMachineEventType
// open-string taxonomy. The cyoda-go-spi module is consumed as a versioned
// external dependency (no replace), so the constant is defined here today;
// it can migrate upstream into spi.SMEventTransitionAborted on the next
// SPI release without breaking either side (the audit handler already
// emits eventType as a raw string).
const SMEventTransitionAborted spi.StateMachineEventType = "TRANSITION_ABORTED"

// TransitionAbortedReasonEntityModified is the only reason today — a stale
// ifMatch precondition. Carved out as a constant so future reasons (e.g.
// processor-cancellation, criterion-aborted) can be added without
// stringly-typed drift across emission sites. Exported so handler-side
// emitters in internal/domain/entity can reuse the same constant.
const TransitionAbortedReasonEntityModified = "ENTITY_MODIFIED"

// EmitTransitionAborted writes a TRANSITION_ABORTED audit event into
// auditStore for the given entity, attributing the abort to a stale ifMatch
// precondition. Best effort — audit failures must not break the workflow or
// handler execution path. actualTxID is the entity row's current
// transactionId at the moment the conflict was detected; if the storage
// layer cannot supply it the caller passes "".
//
// Used by both the engine (CBD first-segment-flush conflict) and the entity
// handler (post-engine CompareAndSave conflict on non-segmenting cascades).
// The TimeUUID for the event is generated via the supplied uuids generator
// so engine and handler call sites share the same monotonic ordering.
func EmitTransitionAborted(
	ctx context.Context,
	auditStore spi.StateMachineAuditStore,
	uuids spi.UUIDGenerator,
	entityID, cascadeEntryTxID, state, transitionName, expectedTxID, actualTxID string,
) {
	if auditStore == nil {
		return
	}
	data := map[string]any{
		"reason":         TransitionAbortedReasonEntityModified,
		"transitionName": transitionName,
		"expectedTxId":   expectedTxID,
		"actualTxId":     actualTxID,
	}
	event := spi.StateMachineEvent{
		EventType:     SMEventTransitionAborted,
		EntityID:      entityID,
		TimeUUID:      uuid.UUID(uuids.NewTimeUUID()).String(),
		State:         state,
		TransactionID: cascadeEntryTxID,
		Details:       fmt.Sprintf("Transition %q aborted: entity has been modified since last read", transitionName),
		Data:          data,
		Timestamp:     time.Now(),
	}
	if err := auditStore.Record(ctx, entityID, event); err != nil {
		slog.Debug("transition-aborted audit emission failed",
			"pkg", "workflow", "entityId", entityID, "error", err)
	}
}

// LookupActualTxID best-effort fetches the entity row's current
// transactionId via the supplied factory. Returns "" on any error — the
// abort event still carries the rest of its payload and downstream
// consumers can fall back to the expectedTxId mismatch as the conflict
// signal. Detaches from any in-flight TX via WithTransaction(nil) so the
// read goes against the most recently committed snapshot rather than the
// rolled-back / about-to-rollback in-flight TX.
func LookupActualTxID(ctx context.Context, factory spi.StoreFactory, entityID string) string {
	if factory == nil {
		return ""
	}
	readCtx := spi.WithTransaction(context.WithoutCancel(ctx), nil)
	es, err := factory.EntityStore(readCtx)
	if err != nil {
		slog.Debug("transition-aborted: entity-store lookup failed",
			"pkg", "workflow", "entityId", entityID, "error", err)
		return ""
	}
	row, err := es.Get(readCtx, entityID)
	if err != nil || row == nil {
		slog.Debug("transition-aborted: entity Get failed",
			"pkg", "workflow", "entityId", entityID, "error", err)
		return ""
	}
	return row.Meta.TransactionID
}

// recordAbortForIfMatchConflict is the engine-internal helper invoked at the
// CBD first-segment-flush boundary when CompareAndSave rejected the
// caller-supplied IfMatch precondition.
func (e *Engine) recordAbortForIfMatchConflict(
	ctx context.Context,
	auditStore spi.StateMachineAuditStore,
	entity *spi.Entity,
	cascadeEntryTxID string,
	transitionName string,
	expectedTxID string,
) {
	actualTxID := LookupActualTxID(ctx, e.factory, entity.Meta.ID)
	EmitTransitionAborted(ctx, auditStore, e.uuids,
		entity.Meta.ID, cascadeEntryTxID, entity.Meta.State,
		transitionName, expectedTxID, actualTxID)
}
