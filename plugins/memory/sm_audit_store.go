package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

type StateMachineAuditStore struct {
	tenant  spi.TenantID
	factory *StoreFactory
}

// Record assigns the event its id (see spi.StateMachineAuditStore): a
// caller's TimeUUID is ignored. With a transaction on ctx the event is staged
// on it and joins the trail only if the transaction commits.
func (s *StateMachineAuditStore) Record(ctx context.Context, entityID string, event spi.StateMachineEvent) error {
	if s.factory.uuids == nil {
		return fmt.Errorf("failed to record state machine event for entity %s: no id generator configured: %w", entityID, spi.ErrStoreRejected)
	}
	// The SQL stores write the event as JSON; one they cannot write is
	// refused here too, so no backend keeps what another refuses.
	if _, err := json.Marshal(event); err != nil {
		return fmt.Errorf("failed to record state machine event for entity %s: %w: %w", entityID, spi.ErrStoreRejected, err)
	}

	if tx := spi.GetTransaction(ctx); tx != nil {
		tx.OpMu.RLock()
		defer tx.OpMu.RUnlock()
		if tx.RolledBack {
			return fmt.Errorf("Record: %w (txID=%s)", spi.ErrTxRolledBack, tx.ID)
		}
		if tx.Closed {
			return fmt.Errorf("Record: %w (txID=%s)", spi.ErrTxAlreadyCommitted, tx.ID)
		}
		if tx.TenantID != s.tenant {
			return fmt.Errorf("Record: %w (txID=%s)", spi.ErrTxTenantMismatch, tx.ID)
		}
		event.TimeUUID = uuid.UUID(s.factory.uuids.NewTimeUUID()).String()
		s.factory.txManager.stageAuditEvent(tx.ID, stagedAuditEvent{entityID: entityID, event: copyEvent(event)})
		return nil
	}

	s.factory.smAuditMu.Lock()
	defer s.factory.smAuditMu.Unlock()
	// Minted under the lock, so id-assignment order and append order are the
	// same atomic step for events recorded outside a transaction (see
	// TestSMAudit_ConcurrentRecord_IDOrderMatchesAppendOrder).
	event.TimeUUID = uuid.UUID(s.factory.uuids.NewTimeUUID()).String()
	// Recorded and labelled by the same call, with no staging in between, so
	// the label — when set — always names a live correlation target: always
	// indexable. See appendStagedAuditEvents for the staged case, where a
	// label can outlive the transaction it names.
	s.factory.appendEventLocked(s.tenant, entityID, copyEvent(event), true)
	return nil
}

// appendEventLocked appends cp to the trail of (tenant, entityID) and, when
// indexable, indexes it under its transaction label too, in one critical
// section, so the commit-phase stamp is a lookup. Caller holds smAuditMu.
func (f *StoreFactory) appendEventLocked(tenant spi.TenantID, entityID string, cp spi.StateMachineEvent, indexable bool) {
	if f.smAudit[tenant] == nil {
		f.smAudit[tenant] = make(map[string][]spi.StateMachineEvent)
	}
	events := append(f.smAudit[tenant][entityID], cp)
	f.smAudit[tenant][entityID] = events
	if indexable && cp.TransactionID != "" {
		if f.smAuditTxIndex[tenant] == nil {
			f.smAuditTxIndex[tenant] = make(map[string][]auditEventRef)
		}
		f.smAuditTxIndex[tenant][cp.TransactionID] = append(
			f.smAuditTxIndex[tenant][cp.TransactionID],
			auditEventRef{entityID: entityID, index: len(events) - 1})
	}
}

// appendStagedAuditEvents appends a committing transaction's staged events,
// stamped with submitTime — the transaction's own commit instant — before
// they are appended, so there is no window in which a reader can observe one
// of them appended but still carrying its pre-commit recording-time
// timestamp (contrast stampAuditEventsForTx's already-appended events, which
// necessarily have such a window).
//
// open reports, for each distinct non-empty TransactionID label among
// staged, whether that label names a transaction still active. An event's
// label does not always name the transaction that recorded it — a
// COMMIT_BEFORE_DISPATCH cascade's later segment labels its own events with
// the entry transaction, for GetEventsByTransaction correlation — and a
// label naming a transaction that has already committed or rolled back will
// never be pruned by discardAuditTxIndex again (that transaction's own
// commit/rollback already ran its one and only discard). Indexing under
// such a label would recreate a smAuditTxIndex entry nothing will ever
// remove, leaking one slice per such event for the life of the process. So
// an event whose label is not open is still appended to the trail — it is
// not lost — but left out of the index; a lookup by that stale label simply
// finds nothing, the same as on a backend where the label's transaction is
// already gone.
//
// open must be computed by the caller BEFORE this function takes smAuditMu:
// smAuditMu is documented as a leaf (see discardAuditTxIndex), and mu is
// already taken while holding smAuditMu elsewhere in this package (the
// conflict-abort branch and Commit's step-6 cleanup both call
// discardAuditTxIndex from inside an m.mu section) — this function taking mu
// itself would reverse that into a cycle.
//
// Called by Commit inside its entityMu section; entityMu → smAuditMu, and
// smAuditMu is a leaf.
func (f *StoreFactory) appendStagedAuditEvents(tenant spi.TenantID, staged []stagedAuditEvent, submitTime time.Time, open map[string]bool) {
	if len(staged) == 0 {
		return
	}
	f.smAuditMu.Lock()
	defer f.smAuditMu.Unlock()
	for _, st := range staged {
		ev := st.event
		ev.Timestamp = submitTime
		f.appendEventLocked(tenant, st.entityID, ev, open[ev.TransactionID])
	}
}

// stagedFor returns the events the transaction on ctx has recorded for
// entityID and not yet committed, so a read inside the transaction sees them.
func (s *StateMachineAuditStore) stagedFor(ctx context.Context, entityID string) []spi.StateMachineEvent {
	tx := spi.GetTransaction(ctx)
	if tx == nil || tx.TenantID != s.tenant {
		return nil
	}
	var out []spi.StateMachineEvent
	for _, st := range s.factory.txManager.stagedAuditEvents(tx.ID) {
		if st.entityID == entityID {
			out = append(out, copyEvent(st.event))
		}
	}
	return out
}

// auditEventRef addresses one recorded audit event: the entity whose slice
// holds it, and its position in that slice.
type auditEventRef struct {
	entityID string
	index    int
}

// GetEvents returns entityID's committed events, plus — when ctx carries a
// transaction — that transaction's own not-yet-committed events, so a read
// inside a transaction sees what it staged there.
func (s *StateMachineAuditStore) GetEvents(ctx context.Context, entityID string) ([]spi.StateMachineEvent, error) {
	staged := s.stagedFor(ctx, entityID)
	s.factory.smAuditMu.RLock()
	defer s.factory.smAuditMu.RUnlock()
	tenantData, ok := s.factory.smAudit[s.tenant]
	if !ok {
		if len(staged) == 0 {
			return []spi.StateMachineEvent{}, nil
		}
		return staged, nil
	}
	events, ok := tenantData[entityID]
	if !ok {
		if len(staged) == 0 {
			return []spi.StateMachineEvent{}, nil
		}
		return staged, nil
	}
	return append(copyEvents(events), staged...), nil
}

func (s *StateMachineAuditStore) GetEventsByTransaction(ctx context.Context, entityID string, transactionID string) ([]spi.StateMachineEvent, error) {
	staged := s.stagedFor(ctx, entityID)
	s.factory.smAuditMu.RLock()
	defer s.factory.smAuditMu.RUnlock()
	events := s.factory.smAudit[s.tenant][entityID]
	filtered := []spi.StateMachineEvent{}
	for _, e := range events {
		if e.TransactionID == transactionID {
			filtered = append(filtered, copyEvent(e))
		}
	}
	for _, e := range staged {
		if e.TransactionID == transactionID {
			filtered = append(filtered, e)
		}
	}
	return filtered, nil
}

// stampAuditEventsForTx moves every already-appended event LABELLED with
// txID onto instant — the commit instant of the transaction being
// committed.
//
// An audit event's timestamp is the clock of whichever process recorded it,
// read while the transaction was still open. Reporting that value leaves the
// audit trail on a different clock from the version history it accompanies,
// and able to invert against it; every backend therefore reports the commit
// instant instead, and a parity scenario holds the three to it.
//
// "Labelled with", not "written by": the engine records some events under a
// cascade entry's transaction id rather than the recording transaction's
// (EmitTransitionAborted). Such an event is stamped here only if its label
// names a transaction that later commits, matching what the SQL backends do
// with the same WHERE.
//
// This transaction's own staged events are already stamped with instant by
// appendStagedAuditEvents, called just before this — restamping them here is
// an idempotent no-op. What this sweep additionally reaches is an event
// Record's untransacted branch already appended and indexed under this
// transaction's label directly (see appendEventLocked): a caller that
// records without carrying the transaction on ctx, yet labels the event with
// a txID for correlation, appends and indexes it immediately rather than
// staging it — Record takes smAuditMu itself in that branch; with a
// transaction on ctx it takes the transaction manager's mu instead (see
// stageAuditEvent), not smAuditMu.
//
// Called from Commit inside the factory's entityMu critical section. That
// establishes entityMu → smAuditMu as a lock order; no path takes them in the
// opposite order (the audit store's own methods take smAuditMu alone), so it
// introduces no cycle. The work done under both locks is bounded by THIS
// transaction's own indexed event count, via smAuditTxIndex — it is a
// lookup, not a walk over the tenant, which is what makes holding the global
// write lock across it acceptable rather than a scalability trap in the
// commit path.
//
// This is a point-in-time sweep, not a write barrier: an event Record's
// untransacted branch appends AFTER this runs but before Commit returns
// keeps the clock its recorder read. It does not arise in the normal path —
// recordEvent runs on the goroutine driving the transaction, which is inside
// Commit at this moment and so cannot be recording — but the property this
// provides is "every already-appended event labelled with this transaction,
// as of this sweep", not "every event the transaction will ever be labelled
// with". The SQL backends have the identical window for the identical
// reason.
func (f *StoreFactory) stampAuditEventsForTx(tenant spi.TenantID, txID string, instant time.Time) {
	if txID == "" {
		return
	}
	f.smAuditMu.Lock()
	defer f.smAuditMu.Unlock()
	for _, ref := range f.smAuditTxIndex[tenant][txID] {
		f.smAudit[tenant][ref.entityID][ref.index].Timestamp = instant
	}
}

// discardAuditTxIndex drops txID's entry from smAuditTxIndex.
//
// The index exists for exactly one reader — stampAuditEventsForTx, which runs
// once per transaction — so an entry is dead the moment the transaction
// reaches a terminal state: stamped and committed, aborted mid-commit, or
// rolled back. Nothing can read it again. Without this, a long-running
// process accumulates one reference per audited event forever. It is
// therefore discarded on exactly the paths that discard the transaction's
// other staged maps (supersededSaves, deletedBufferModels, scheduledTaskOps,
// auditOps), at the same points and for the same reason.
//
// This drops the INDEX, not the trail: the audit events themselves are
// untouched, keeping whatever timestamp they were stamped or recorded with.
//
// Locking: takes smAuditMu, and callers hold more than one lock above it —
// the conflict-detection abort and the commit-log block both hold entityMu and
// the transaction manager's mu, giving entityMu → mu → smAuditMu. That is safe
// without documenting each chain because smAuditMu is a leaf: every other
// holder (Record, GetEvents, GetEventsByTransaction) takes it alone and
// acquires nothing under it, so no cycle is reachable whatever is held above.
func (f *StoreFactory) discardAuditTxIndex(tenant spi.TenantID, txID string) {
	if txID == "" {
		return
	}
	f.smAuditMu.Lock()
	defer f.smAuditMu.Unlock()
	byTx := f.smAuditTxIndex[tenant]
	if byTx == nil {
		return
	}
	delete(byTx, txID)
	if len(byTx) == 0 {
		delete(f.smAuditTxIndex, tenant)
	}
}

func copyEvent(e spi.StateMachineEvent) spi.StateMachineEvent {
	cp := e
	if e.Data != nil {
		cp.Data = make(map[string]any, len(e.Data))
		for k, v := range e.Data {
			cp.Data[k] = v
		}
	}
	return cp
}

func copyEvents(events []spi.StateMachineEvent) []spi.StateMachineEvent {
	out := make([]spi.StateMachineEvent, len(events))
	for i, e := range events {
		out[i] = copyEvent(e)
	}
	return out
}
