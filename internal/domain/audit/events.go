package audit

import (
	"fmt"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// auditItem is one event of the merged list: its sort key and its wire body.
type auditItem struct {
	key  eventKey
	body map[string]any
}

// eventKey is the merge/sort/filter key shared by both event kinds, so the
// handler can order and filter without re-parsing the wire body's utcTime
// string or reaching into kind-specific fields.
type eventKey struct {
	at      time.Time // utcTime at stored precision
	kind    string    // "EntityChange" | "StateMachine"
	version int64     // EntityChange only
	eventID uuid.UUID // StateMachine only
}

// entityChangeItem builds one EntityChange audit event from a store version
// record.
func entityChangeItem(v spi.EntityVersionMeta, entityID, callerTenant string) auditItem {
	at := v.Timestamp.UTC()
	body := map[string]any{
		"auditEventType": "EntityChange",
		"changeType":     common.CanonicalChangeType(v.ChangeType),
		"severity":       "INFO",
		"utcTime":        at.Format(time.RFC3339Nano),
		"microsTime":     at.UnixMicro(),
		"system":         false,
		"entityId":       entityID,
		"version":        v.Version,
	}
	if v.TransactionID != "" {
		body["transactionId"] = v.TransactionID
	}
	if v.User != "" {
		actor := map[string]any{"id": v.User, "name": v.User}
		if callerTenant != "" {
			actor["legalId"] = callerTenant
		}
		body["actor"] = actor
	}
	return auditItem{key: eventKey{at: at, kind: "EntityChange", version: v.Version}, body: body}
}

// stateMachineItem builds one StateMachine audit event. The store assigns
// every event an id (spi.StateMachineAuditStore); one that does not parse,
// or parses to the nil UUID — a value no store ever assigns — is a store
// fault, reported rather than emitted with a blank identity.
func stateMachineItem(ev spi.StateMachineEvent) (auditItem, error) {
	id, err := uuid.Parse(ev.TimeUUID)
	if err != nil {
		return auditItem{}, fmt.Errorf("state machine event of entity %s has no valid id: %w", ev.EntityID, err)
	}
	if id == uuid.Nil {
		return auditItem{}, fmt.Errorf("state machine event of entity %s has no valid id: nil UUID", ev.EntityID)
	}
	at := ev.Timestamp.UTC()
	body := map[string]any{
		"auditEventType": "StateMachine",
		"eventType":      string(ev.EventType),
		"severity":       "INFO",
		"utcTime":        at.Format(time.RFC3339Nano),
		"microsTime":     at.UnixMicro(),
		"entityId":       ev.EntityID,
		"eventId":        id.String(),
		"details":        ev.Details,
		"data":           ev.Data,
	}
	if ev.TransactionID != "" {
		body["transactionId"] = ev.TransactionID
	}
	if ev.State != "" {
		body["state"] = ev.State
	}
	return auditItem{key: eventKey{at: at, kind: "StateMachine", eventID: id}, body: body}, nil
}
