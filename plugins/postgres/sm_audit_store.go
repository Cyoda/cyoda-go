package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// smAuditStore implements spi.StateMachineAuditStore backed by PostgreSQL.
type smAuditStore struct {
	q        Querier
	tenantID spi.TenantID
	uuids    spi.UUIDGenerator
}

// Record assigns the event its id (see spi.StateMachineAuditStore): a
// caller's TimeUUID is ignored. It is append-only; no upsert is performed.
func (s *smAuditStore) Record(ctx context.Context, entityID string, event spi.StateMachineEvent) error {
	if s.uuids == nil {
		return fmt.Errorf("failed to record state machine event for entity %s: no id generator configured", entityID)
	}
	event.TimeUUID = uuid.UUID(s.uuids.NewTimeUUID()).String()

	doc, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("failed to marshal state machine event: %w", err)
	}

	_, err = s.q.Exec(ctx,
		`INSERT INTO sm_audit_events (tenant_id, entity_id, event_id, transaction_id, timestamp, doc)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		string(s.tenantID), entityID, event.TimeUUID, event.TransactionID, event.Timestamp, doc)
	if err != nil {
		return fmt.Errorf("failed to record state machine event %s for entity %s: %w", event.TimeUUID, entityID, err)
	}
	return nil
}

// GetEvents returns all audit events for the given entity, ordered by
// timestamp ascending. Returns an empty slice (not an error) when no events
// exist for the entity.
func (s *smAuditStore) GetEvents(ctx context.Context, entityID string) ([]spi.StateMachineEvent, error) {
	rows, err := s.q.Query(ctx,
		`SELECT event_id, doc, timestamp FROM sm_audit_events
		 WHERE tenant_id = $1 AND entity_id = $2
		 ORDER BY timestamp ASC`,
		string(s.tenantID), entityID)
	if err != nil {
		return nil, fmt.Errorf("failed to query events for entity %s: %w", entityID, err)
	}
	defer rows.Close()

	events, err := scanEventRows(rows)
	if err != nil {
		return nil, fmt.Errorf("failed to scan events for entity %s: %w", entityID, err)
	}
	if events == nil {
		events = []spi.StateMachineEvent{}
	}
	return events, nil
}

// GetEventsByTransaction returns audit events for the given entity that belong
// to the specified transaction, ordered by timestamp ascending.
// Unlike GetEvents, it returns an empty slice (not an error) when no events
// match the transaction.
func (s *smAuditStore) GetEventsByTransaction(ctx context.Context, entityID string, transactionID string) ([]spi.StateMachineEvent, error) {
	rows, err := s.q.Query(ctx,
		`SELECT event_id, doc, timestamp FROM sm_audit_events
		 WHERE tenant_id = $1 AND entity_id = $2 AND transaction_id = $3
		 ORDER BY timestamp ASC`,
		string(s.tenantID), entityID, transactionID)
	if err != nil {
		return nil, fmt.Errorf("failed to query events for entity %s, transaction %s: %w", entityID, transactionID, err)
	}
	defer rows.Close()

	events, err := scanEventRows(rows)
	if err != nil {
		return nil, fmt.Errorf("failed to scan events for entity %s, transaction %s: %w", entityID, transactionID, err)
	}
	return events, nil
}

// scanEventRows reads all rows from a (event_id, doc, timestamp) query and
// unmarshals each into a StateMachineEvent. The caller is responsible for
// closing rows.
//
// The timestamp COLUMN overrides the copy inside the document. The column is
// what the commit phase stamps with the transaction's instant
// (TransactionManager.stampCommitInstant), while the document keeps whatever
// clock the recording process read — so reporting the document's copy would
// leave the audit trail dated by a different clock from the version history
// it accompanies, and ordered by a value it does not report. The event_id
// column likewise overrides the document's TimeUUID: it is the key the row
// was stored under.
func scanEventRows(rows pgx.Rows) ([]spi.StateMachineEvent, error) {
	var events []spi.StateMachineEvent
	for rows.Next() {
		var eventID string
		var doc []byte
		var timestamp time.Time
		if err := rows.Scan(&eventID, &doc, &timestamp); err != nil {
			return nil, fmt.Errorf("failed to scan row: %w", err)
		}
		var e spi.StateMachineEvent
		if err := json.Unmarshal(doc, &e); err != nil {
			return nil, fmt.Errorf("failed to unmarshal event doc: %w", err)
		}
		e.TimeUUID = eventID
		e.Timestamp = timestamp
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("row iteration error: %w", err)
	}
	return events, nil
}
