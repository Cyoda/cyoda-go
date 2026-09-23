package memory

import (
	"context"
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
// caller's TimeUUID is ignored.
func (s *StateMachineAuditStore) Record(ctx context.Context, entityID string, event spi.StateMachineEvent) error {
	if s.factory.uuids == nil {
		return fmt.Errorf("failed to record state machine event for entity %s: no id generator configured", entityID)
	}

	s.factory.smAuditMu.Lock()
	defer s.factory.smAuditMu.Unlock()
	// Minted under the lock, so id-assignment order and append order are the
	// same atomic step: a concurrent Record cannot mint an earlier id yet
	// append after one minted later (see
	// TestSMAudit_ConcurrentRecord_IDOrderMatchesAppendOrder).
	event.TimeUUID = uuid.UUID(s.factory.uuids.NewTimeUUID()).String()
	if s.factory.smAudit[s.tenant] == nil {
		s.factory.smAudit[s.tenant] = make(map[string][]spi.StateMachineEvent)
	}
	cp := copyEvent(event)
	events := append(s.factory.smAudit[s.tenant][entityID], cp)
	s.factory.smAudit[s.tenant][entityID] = events

	// Index this event's position under its transaction label, in the same
	// critical section as the append, so the commit-phase stamp is a lookup
	// rather than a walk over the tenant. See smAuditTxIndex's field comment
	// for why a position is a stable address here.
	if cp.TransactionID != "" {
		if s.factory.smAuditTxIndex[s.tenant] == nil {
			s.factory.smAuditTxIndex[s.tenant] = make(map[string][]auditEventRef)
		}
		s.factory.smAuditTxIndex[s.tenant][cp.TransactionID] = append(
			s.factory.smAuditTxIndex[s.tenant][cp.TransactionID],
			auditEventRef{entityID: entityID, index: len(events) - 1})
	}
	return nil
}

// auditEventRef addresses one recorded audit event: the entity whose slice
// holds it, and its position in that slice.
type auditEventRef struct {
	entityID string
	index    int
}

func (s *StateMachineAuditStore) GetEvents(ctx context.Context, entityID string) ([]spi.StateMachineEvent, error) {
	s.factory.smAuditMu.RLock()
	defer s.factory.smAuditMu.RUnlock()
	tenantData, ok := s.factory.smAudit[s.tenant]
	if !ok {
		return []spi.StateMachineEvent{}, nil
	}
	events, ok := tenantData[entityID]
	if !ok {
		return []spi.StateMachineEvent{}, nil
	}
	return copyEvents(events), nil
}

func (s *StateMachineAuditStore) GetEventsByTransaction(ctx context.Context, entityID string, transactionID string) ([]spi.StateMachineEvent, error) {
	s.factory.smAuditMu.RLock()
	defer s.factory.smAuditMu.RUnlock()
	tenantData, ok := s.factory.smAudit[s.tenant]
	if !ok {
		return []spi.StateMachineEvent{}, nil
	}
	events, ok := tenantData[entityID]
	if !ok {
		return []spi.StateMachineEvent{}, nil
	}
	filtered := []spi.StateMachineEvent{}
	for _, e := range events {
		if e.TransactionID == transactionID {
			filtered = append(filtered, copyEvent(e))
		}
	}
	return filtered, nil
}

// stampAuditEventsForTx moves every recorded event LABELLED with txID onto
// instant — the commit instant of the transaction being committed.
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
// Called from Commit inside the factory's entityMu critical section. That
// establishes entityMu → smAuditMu as a lock order; no path takes them in the
// opposite order (the audit store's own methods take smAuditMu alone), so it
// introduces no cycle. The work done under both locks is bounded by THIS
// transaction's own event count, via smAuditTxIndex — it is a lookup, not a
// walk over the tenant, which is what makes holding the global write lock
// across it acceptable rather than a scalability trap in the commit path.
//
// This is a point-in-time sweep, not a write barrier: an event recorded AFTER
// it runs but before Commit returns keeps the clock its recorder read. Record
// takes only smAuditMu, which Commit's entityMu section does not exclude it
// from, so such an event is possible in principle. It does not arise in the
// normal path — recordEvent runs on the goroutine driving the transaction,
// which is inside Commit at this moment and so cannot be recording — but the
// property this provides is "every event recorded before the commit phase",
// not "every event the transaction will ever be labelled with". The SQL
// backends have the identical window for the identical reason.
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
// other staged maps (supersededSaves, deletedBufferModels, scheduledTaskOps),
// at the same points and for the same reason.
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
