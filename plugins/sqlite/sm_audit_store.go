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
}

func (s *smAuditStore) Record(ctx context.Context, entityID string, event spi.StateMachineEvent) error {
	eventID := event.TimeUUID
	if eventID == "" {
		id, _ := uuid.NewUUID()
		eventID = id.String()
	}

	doc, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("failed to marshal state machine event: %w", err)
	}

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO sm_audit_events (tenant_id, entity_id, event_id, transaction_id, timestamp, doc)
		 VALUES (?, ?, ?, ?, ?, jsonb(?))`,
		string(s.tenantID), entityID, eventID, event.TransactionID,
		event.Timestamp.UnixMicro(), doc)
	if err != nil {
		return fmt.Errorf("failed to record state machine event %s for entity %s: %w", eventID, entityID, err)
	}
	return nil
}

func (s *smAuditStore) GetEvents(ctx context.Context, entityID string) ([]spi.StateMachineEvent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT json(doc), timestamp FROM sm_audit_events
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
	return events, nil
}

func (s *smAuditStore) GetEventsByTransaction(ctx context.Context, entityID string, transactionID string) ([]spi.StateMachineEvent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT json(doc), timestamp FROM sm_audit_events
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
	return events, nil
}

// scanSMEventRows reads (doc, timestamp) rows into StateMachineEvents.
//
// The timestamp COLUMN overrides the copy inside the document: the column is
// what the commit phase stamps with the transaction's instant (flushToSQLite),
// while the document keeps whatever clock the recording process read. Reporting
// the document's copy would leave the audit trail dated by a different clock
// from the version history it accompanies, and ordered by a value it does not
// report.
func scanSMEventRows(rows *sql.Rows) ([]spi.StateMachineEvent, error) {
	var events []spi.StateMachineEvent
	for rows.Next() {
		var doc []byte
		var timestampMicro int64
		if err := rows.Scan(&doc, &timestampMicro); err != nil {
			return nil, fmt.Errorf("failed to scan row: %w", err)
		}
		var e spi.StateMachineEvent
		if err := json.Unmarshal(doc, &e); err != nil {
			return nil, fmt.Errorf("failed to unmarshal event doc: %w", err)
		}
		e.Timestamp = microToTime(timestampMicro).UTC()
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("row iteration error: %w", err)
	}
	return events, nil
}
