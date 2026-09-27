package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

type smAuditStore struct {
	db       *sql.DB
	tenantID spi.TenantID
	uuids    spi.UUIDGenerator
	tm       *transactionManager
}

// insertAuditEventSQL writes one audit event. Record uses it outside a
// transaction; flushToSQLite uses it for the events a transaction staged.
const insertAuditEventSQL = `INSERT INTO sm_audit_events (tenant_id, entity_id, event_id, transaction_id, timestamp, doc)
	 VALUES (?, ?, ?, ?, ?, jsonb(?))`

// Record assigns the event its id (see spi.StateMachineAuditStore): a
// caller's TimeUUID is ignored. With a transaction on ctx the event is staged
// on it and written only if the transaction commits.
func (s *smAuditStore) Record(ctx context.Context, entityID string, event spi.StateMachineEvent) error {
	if s.uuids == nil {
		return fmt.Errorf("failed to record state machine event for entity %s: no id generator configured: %w", entityID, spi.ErrStoreRejected)
	}
	event.TimeUUID = uuid.UUID(s.uuids.NewTimeUUID()).String()

	doc, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("failed to marshal state machine event: %w: %w", spi.ErrStoreRejected, err)
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
		if tx.TenantID != s.tenantID {
			return fmt.Errorf("Record: %w (txID=%s)", spi.ErrTxTenantMismatch, tx.ID)
		}
		s.tm.stageAuditEvent(tx.ID, stagedAuditEvent{entityID: entityID, event: event, doc: doc})
		return nil
	}

	if _, err := s.db.ExecContext(ctx, insertAuditEventSQL,
		string(s.tenantID), entityID, event.TimeUUID, event.TransactionID,
		event.Timestamp.UnixMicro(), doc); err != nil {
		return fmt.Errorf("failed to record state machine event %s for entity %s: %w", event.TimeUUID, entityID, classifyRejection(err))
	}
	return nil
}

// stagedFor returns the events the transaction on ctx has recorded for
// entityID and not yet committed, so a read inside the transaction sees them.
func (s *smAuditStore) stagedFor(ctx context.Context, entityID string, keep func(spi.StateMachineEvent) bool) []spi.StateMachineEvent {
	tx := spi.GetTransaction(ctx)
	if tx == nil || tx.TenantID != s.tenantID || s.tm == nil {
		return nil
	}
	var out []spi.StateMachineEvent
	for _, st := range s.tm.stagedAuditEvents(tx.ID) {
		if st.entityID == entityID && keep(st.event) {
			out = append(out, st.event)
		}
	}
	return out
}

// withStaged appends staged events after committed ones, unsorted. A staged
// event's Timestamp is its recording time — read by whichever process called
// Record, before its transaction committed; a committed event's timestamp is
// a commit instant. The two are different clocks, so they are never compared
// against each other: sorting the combined slice by Timestamp would place a
// staged event before an already-committed one whenever its recording time
// happens to read earlier than the committed row's commit instant, which
// says nothing about which one is actually settled. Appending, not merging,
// mirrors the memory plugin's GetEvents.
func withStaged(committed, staged []spi.StateMachineEvent) []spi.StateMachineEvent {
	if len(staged) == 0 {
		return committed
	}
	return append(committed, staged...)
}

func (s *smAuditStore) GetEvents(ctx context.Context, entityID string) ([]spi.StateMachineEvent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT event_id, json(doc), timestamp FROM sm_audit_events
		 WHERE tenant_id = ? AND entity_id = ?
		 ORDER BY timestamp ASC`,
		string(s.tenantID), entityID)
	if err != nil {
		return nil, fmt.Errorf("failed to query events for entity %s: %w", entityID, err)
	}
	defer rows.Close()

	events, err := scanSMEventRows(rows)
	if err != nil {
		return nil, fmt.Errorf("failed to scan events for entity %s: %w", entityID, err)
	}
	if events == nil {
		events = []spi.StateMachineEvent{}
	}
	return withStaged(events, s.stagedFor(ctx, entityID, func(spi.StateMachineEvent) bool { return true })), nil
}

func (s *smAuditStore) GetEventsByTransaction(ctx context.Context, entityID string, transactionID string) ([]spi.StateMachineEvent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT event_id, json(doc), timestamp FROM sm_audit_events
		 WHERE tenant_id = ? AND entity_id = ? AND transaction_id = ?
		 ORDER BY timestamp ASC`,
		string(s.tenantID), entityID, transactionID)
	if err != nil {
		return nil, fmt.Errorf("failed to query events for entity %s, transaction %s: %w", entityID, transactionID, err)
	}
	defer rows.Close()

	events, err := scanSMEventRows(rows)
	if err != nil {
		return nil, fmt.Errorf("failed to scan events for entity %s, transaction %s: %w", entityID, transactionID, err)
	}
	return withStaged(events, s.stagedFor(ctx, entityID, func(e spi.StateMachineEvent) bool { return e.TransactionID == transactionID })), nil
}

// scanSMEventRows reads (event_id, doc, timestamp) rows into StateMachineEvents.
//
// The timestamp COLUMN overrides the copy inside the document: the column is
// what the commit phase stamps with the transaction's instant (flushToSQLite),
// while the document keeps whatever clock the recording process read. Reporting
// the document's copy would leave the audit trail dated by a different clock
// from the version history it accompanies, and ordered by a value it does not
// report. The event_id column likewise overrides the document's TimeUUID: it is
// the key the row was stored under.
func scanSMEventRows(rows *sql.Rows) ([]spi.StateMachineEvent, error) {
	var events []spi.StateMachineEvent
	for rows.Next() {
		var eventID string
		var doc []byte
		var timestampMicro int64
		if err := rows.Scan(&eventID, &doc, &timestampMicro); err != nil {
			return nil, fmt.Errorf("failed to scan row: %w", err)
		}
		var e spi.StateMachineEvent
		if err := json.Unmarshal(doc, &e); err != nil {
			return nil, fmt.Errorf("failed to unmarshal event doc: %w", err)
		}
		e.TimeUUID = eventID
		e.Timestamp = microToTime(timestampMicro).UTC()
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("row iteration error: %w", err)
	}
	return events, nil
}
