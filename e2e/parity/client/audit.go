package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// AuditEvent is the discriminated-union base for audit events from
// GET /audit/entity/{entityId}.
//
// Mirrors AuditEvent in docs/cyoda/api/openapi-audit.yml line 604.
// The canonical schema uses auditEventType as the discriminator with
// three concrete subtypes:
//
//   - StateMachineAuditEvent (auditEventType == "StateMachine")
//   - EntityChangeAuditEvent (auditEventType == "EntityChange")
//   - SystemAuditEvent       (auditEventType == "System")
//
// Decoding strategy:
//  1. The HTTP client decodes audit responses into []AuditEvent.
//  2. AuditEvent.UnmarshalJSON captures the raw bytes into Raw and
//     decodes the base fields permissively (without DisallowUnknownFields)
//     because subtype-specific fields are by-design "unknown" at the
//     base level.
//  3. Scenarios call event.AsStateMachine() / .AsEntityChange() /
//     .AsSystem() to re-decode Raw into the typed subtype with
//     DisallowUnknownFields enforced — this is where strict drift
//     detection happens for subtype-specific fields.
//
// Required base fields (canonical): auditEventType, severity, utcTime,
// microsTime. Other base fields are optional.
type AuditEvent struct {
	// AuditEventType is one of "StateMachine" | "EntityChange" | "System".
	// Typed as string (not an enum type) to tolerate server-side enum
	// extensions; schema-level drift on field names is caught by the
	// flat-alias DisallowUnknownFields pattern instead.
	AuditEventType string `json:"auditEventType"`

	// Severity is one of "ERROR" | "INFO" | "WARN" | "DEBUG". Same
	// design rationale as AuditEventType — typed as string for value-
	// level tolerance, schema-level strictness elsewhere.
	Severity        string          `json:"severity"`
	UtcTime         time.Time       `json:"utcTime"`
	MicrosTime      int64           `json:"microsTime"`
	ConsistencyTime *time.Time      `json:"consistencyTime,omitempty"`
	EntityID        string          `json:"entityId,omitempty"`
	EntityModel     string          `json:"entityModel,omitempty"`
	TransactionID   string          `json:"transactionId,omitempty"`
	Actor           *AuditActorInfo `json:"actor,omitempty"`
	Details         string          `json:"details,omitempty"`
	System          bool            `json:"system,omitempty"`

	// Raw holds the original bytes captured during UnmarshalJSON. It
	// is used by the AsX() methods to re-decode into the typed
	// subtype. Tagged json:"-" so it does not participate in
	// marshalling.
	Raw json.RawMessage `json:"-"`
}

// AuditActorInfo mirrors AuditActorInfo in
// docs/cyoda/api/openapi-audit.yml line 584.
type AuditActorInfo struct {
	ID         string `json:"id"`
	LegalID    string `json:"legalId"`
	Name       string `json:"name,omitempty"`
	ExternalID string `json:"externalId,omitempty"`
}

// auditEventBase is the alias used inside UnmarshalJSON to break
// recursion (otherwise json.Unmarshal would call our custom
// UnmarshalJSON again forever).
type auditEventBase AuditEvent

// UnmarshalJSON captures the raw bytes into e.Raw and decodes the
// base fields permissively. Subtype-specific fields are silently
// ignored at this level — they get decoded strictly when a scenario
// calls one of the AsX() methods.
//
// Note: the Raw assignment must come AFTER the *e = AuditEvent(base)
// assignment, because that assignment overwrites the entire struct
// (including any earlier Raw value). Future maintainers: do NOT move
// the Raw capture above the base-field assignment.
func (e *AuditEvent) UnmarshalJSON(b []byte) error {
	// Decode the base fields. Use the alias type to avoid recursing
	// into this same UnmarshalJSON. We do NOT use DisallowUnknownFields
	// here because the subtype-specific fields (state, eventType,
	// changeType, etc.) would be rejected.
	var base auditEventBase
	if err := json.Unmarshal(b, &base); err != nil {
		return fmt.Errorf("decode AuditEvent base: %w", err)
	}
	*e = AuditEvent(base)

	// Capture the raw bytes AFTER the base-field assignment, because
	// that assignment overwrote whatever Raw had previously.
	e.Raw = append(e.Raw[:0], b...)
	return nil
}

// StateMachineAuditEvent mirrors StateMachineAuditEvent in
// docs/cyoda/api/openapi-audit.yml line 658.
//
// The embedded AuditEvent has a custom UnmarshalJSON that captures raw
// bytes and decodes base fields permissively. The subtype's own
// UnmarshalJSON uses a flat alias struct with DisallowUnknownFields to
// enforce strict drift detection on subtype-specific fields.
type StateMachineAuditEvent struct {
	AuditEvent
	State     string `json:"state"`     // canonical-required
	EventType string `json:"eventType"` // canonical-required

	// Data carries the StateMachineEvent payload as raw JSON. The
	// canonical schema (docs/cyoda/api/openapi-audit.yml line 823) is
	// itself a discriminated union over a dozen state-machine event
	// subtypes. Parity scenarios that need typed access can decode
	// these bytes themselves; for the parity contract layer, knowing
	// the bytes are present is sufficient.
	Data json.RawMessage `json:"data,omitempty"`
}

// stateMachineAuditEventFlat is a flat struct (no embedded types with
// custom UnmarshalJSON) used by the strict-decode path. All known
// fields from AuditEvent base and StateMachineAuditEvent are listed
// explicitly so that DisallowUnknownFields catches any unrecognised
// field added by a future canonical-schema update.
type stateMachineAuditEventFlat struct {
	AuditEventType  string          `json:"auditEventType"`
	Severity        string          `json:"severity"`
	UtcTime         time.Time       `json:"utcTime"`
	MicrosTime      int64           `json:"microsTime"`
	ConsistencyTime *time.Time      `json:"consistencyTime,omitempty"`
	EntityID        string          `json:"entityId,omitempty"`
	EntityModel     string          `json:"entityModel,omitempty"`
	TransactionID   string          `json:"transactionId,omitempty"`
	Actor           *AuditActorInfo `json:"actor,omitempty"`
	Details         string          `json:"details,omitempty"`
	System          bool            `json:"system,omitempty"`
	State           string          `json:"state"`
	EventType       string          `json:"eventType"`
	Data            json.RawMessage `json:"data,omitempty"`
}

// UnmarshalJSON decodes StateMachineAuditEvent with
// DisallowUnknownFields enforced via a flat alias struct (bypassing
// AuditEvent.UnmarshalJSON to avoid recursion and preserve strict
// field checking).
func (e *StateMachineAuditEvent) UnmarshalJSON(b []byte) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var flat stateMachineAuditEventFlat
	if err := dec.Decode(&flat); err != nil {
		return fmt.Errorf("strict decode StateMachineAuditEvent: %w", err)
	}
	e.AuditEvent = AuditEvent{
		AuditEventType:  flat.AuditEventType,
		Severity:        flat.Severity,
		UtcTime:         flat.UtcTime,
		MicrosTime:      flat.MicrosTime,
		ConsistencyTime: flat.ConsistencyTime,
		EntityID:        flat.EntityID,
		EntityModel:     flat.EntityModel,
		TransactionID:   flat.TransactionID,
		Actor:           flat.Actor,
		Details:         flat.Details,
		System:          flat.System,
	}
	e.State = flat.State
	e.EventType = flat.EventType
	e.Data = flat.Data
	return nil
}

// EntityChangeAuditEvent mirrors EntityChangeAuditEvent in
// docs/cyoda/api/openapi-audit.yml line 695.
type EntityChangeAuditEvent struct {
	AuditEvent
	ChangeType string `json:"changeType"` // canonical-required: CREATE|UPDATE|DELETE

	// Changes carries the before/after diff as raw JSON. The shape
	// varies by entity model; parity scenarios that need typed access
	// can decode these bytes themselves.
	Changes json.RawMessage `json:"changes,omitempty"`
}

// entityChangeAuditEventFlat is the flat alias used for strict decoding.
type entityChangeAuditEventFlat struct {
	AuditEventType  string          `json:"auditEventType"`
	Severity        string          `json:"severity"`
	UtcTime         time.Time       `json:"utcTime"`
	MicrosTime      int64           `json:"microsTime"`
	ConsistencyTime *time.Time      `json:"consistencyTime,omitempty"`
	EntityID        string          `json:"entityId,omitempty"`
	EntityModel     string          `json:"entityModel,omitempty"`
	TransactionID   string          `json:"transactionId,omitempty"`
	Actor           *AuditActorInfo `json:"actor,omitempty"`
	Details         string          `json:"details,omitempty"`
	System          bool            `json:"system,omitempty"`
	ChangeType      string          `json:"changeType"`
	Changes         json.RawMessage `json:"changes,omitempty"`
}

// UnmarshalJSON decodes EntityChangeAuditEvent with
// DisallowUnknownFields enforced via a flat alias struct.
func (e *EntityChangeAuditEvent) UnmarshalJSON(b []byte) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var flat entityChangeAuditEventFlat
	if err := dec.Decode(&flat); err != nil {
		return fmt.Errorf("strict decode EntityChangeAuditEvent: %w", err)
	}
	e.AuditEvent = AuditEvent{
		AuditEventType:  flat.AuditEventType,
		Severity:        flat.Severity,
		UtcTime:         flat.UtcTime,
		MicrosTime:      flat.MicrosTime,
		ConsistencyTime: flat.ConsistencyTime,
		EntityID:        flat.EntityID,
		EntityModel:     flat.EntityModel,
		TransactionID:   flat.TransactionID,
		Actor:           flat.Actor,
		Details:         flat.Details,
		System:          flat.System,
	}
	e.ChangeType = flat.ChangeType
	e.Changes = flat.Changes
	return nil
}

// SystemAuditEvent mirrors SystemAuditEvent in
// docs/cyoda/api/openapi-audit.yml line 725.
type SystemAuditEvent struct {
	AuditEvent
	ErrorTime *time.Time `json:"errorTime,omitempty"`
	DoneTime  *time.Time `json:"doneTime,omitempty"`
	QueueName string     `json:"queueName,omitempty"`
	ShardID   string     `json:"shardId,omitempty"`
	Status    string     `json:"status,omitempty"`

	// Data carries system-event-specific payload as raw JSON. The shape
	// is opaque at the parity contract layer; parity scenarios that need
	// typed access can decode these bytes themselves.
	Data json.RawMessage `json:"data,omitempty"`
}

// systemAuditEventFlat is the flat alias used for strict decoding.
type systemAuditEventFlat struct {
	AuditEventType  string          `json:"auditEventType"`
	Severity        string          `json:"severity"`
	UtcTime         time.Time       `json:"utcTime"`
	MicrosTime      int64           `json:"microsTime"`
	ConsistencyTime *time.Time      `json:"consistencyTime,omitempty"`
	EntityID        string          `json:"entityId,omitempty"`
	EntityModel     string          `json:"entityModel,omitempty"`
	TransactionID   string          `json:"transactionId,omitempty"`
	Actor           *AuditActorInfo `json:"actor,omitempty"`
	Details         string          `json:"details,omitempty"`
	System          bool            `json:"system,omitempty"`
	ErrorTime       *time.Time      `json:"errorTime,omitempty"`
	DoneTime        *time.Time      `json:"doneTime,omitempty"`
	QueueName       string          `json:"queueName,omitempty"`
	ShardID         string          `json:"shardId,omitempty"`
	Status          string          `json:"status,omitempty"`
	Data            json.RawMessage `json:"data,omitempty"`
}

// UnmarshalJSON decodes SystemAuditEvent with DisallowUnknownFields
// enforced via a flat alias struct.
func (e *SystemAuditEvent) UnmarshalJSON(b []byte) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var flat systemAuditEventFlat
	if err := dec.Decode(&flat); err != nil {
		return fmt.Errorf("strict decode SystemAuditEvent: %w", err)
	}
	e.AuditEvent = AuditEvent{
		AuditEventType:  flat.AuditEventType,
		Severity:        flat.Severity,
		UtcTime:         flat.UtcTime,
		MicrosTime:      flat.MicrosTime,
		ConsistencyTime: flat.ConsistencyTime,
		EntityID:        flat.EntityID,
		EntityModel:     flat.EntityModel,
		TransactionID:   flat.TransactionID,
		Actor:           flat.Actor,
		Details:         flat.Details,
		System:          flat.System,
	}
	e.ErrorTime = flat.ErrorTime
	e.DoneTime = flat.DoneTime
	e.QueueName = flat.QueueName
	e.ShardID = flat.ShardID
	e.Status = flat.Status
	e.Data = flat.Data
	return nil
}

// AsStateMachine decodes the raw audit-event bytes into a
// StateMachineAuditEvent with DisallowUnknownFields enforced. Returns
// an error if the auditEventType is not "StateMachine" or if the
// strict re-decode finds an unknown field (which would indicate a
// drift between the parity type and the canonical schema for
// state-machine events).
func (e *AuditEvent) AsStateMachine() (*StateMachineAuditEvent, error) {
	if e.AuditEventType != "StateMachine" {
		return nil, fmt.Errorf("audit event has type %q, not \"StateMachine\"", e.AuditEventType)
	}
	return decodeAuditSubtype[StateMachineAuditEvent](e.Raw)
}

// AsEntityChange decodes the raw audit-event bytes into an
// EntityChangeAuditEvent with DisallowUnknownFields enforced.
func (e *AuditEvent) AsEntityChange() (*EntityChangeAuditEvent, error) {
	if e.AuditEventType != "EntityChange" {
		return nil, fmt.Errorf("audit event has type %q, not \"EntityChange\"", e.AuditEventType)
	}
	return decodeAuditSubtype[EntityChangeAuditEvent](e.Raw)
}

// AsSystem decodes the raw audit-event bytes into a SystemAuditEvent
// with DisallowUnknownFields enforced.
func (e *AuditEvent) AsSystem() (*SystemAuditEvent, error) {
	if e.AuditEventType != "System" {
		return nil, fmt.Errorf("audit event has type %q, not \"System\"", e.AuditEventType)
	}
	return decodeAuditSubtype[SystemAuditEvent](e.Raw)
}

// decodeAuditSubtype is the shared decode helper used by the AsX()
// methods. It decodes the captured raw bytes into the typed subtype.
// DisallowUnknownFields strictness is enforced by each subtype's own
// UnmarshalJSON (via the flat-struct pattern), ensuring drift detection
// even when called from this generic helper.
func decodeAuditSubtype[T any](raw json.RawMessage) (*T, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("audit event raw bytes are empty (UnmarshalJSON did not run)")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var out T
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("strict decode of audit subtype %T: %w", out, err)
	}
	return &out, nil
}

// EntityAuditEventsResponse mirrors EntityAuditEventsResponse in
// docs/cyoda/api/openapi-audit.yml line 520.
//
// Returned by HTTP GET /audit/entity/{entityId}.
type EntityAuditEventsResponse struct {
	Items      []AuditEvent         `json:"items,omitempty"`
	Pagination CursorPaginationInfo `json:"pagination"`
}

// UnmarshalJSON enforces strict drift detection on the audit response
// envelope, mirroring the flat-alias pattern used on AuditEvent subtypes.
// Without this, an unknown top-level field in EntityAuditEventsResponse
// (or its nested Pagination) would be silently dropped by the default
// json decoder. With this, any drift between the canonical
// EntityAuditEventsResponse schema and what Cyoda Cloud actually emits
// fails loudly.
//
// The Items slice still goes through AuditEvent.UnmarshalJSON (which is
// permissive at the base level by design — subtype-specific fields are
// validated strictly via the AsX() helpers).
func (r *EntityAuditEventsResponse) UnmarshalJSON(b []byte) error {
	type flat struct {
		Items      []AuditEvent         `json:"items,omitempty"`
		Pagination CursorPaginationInfo `json:"pagination"`
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var f flat
	if err := dec.Decode(&f); err != nil {
		return fmt.Errorf("strict decode EntityAuditEventsResponse: %w", err)
	}
	r.Items = f.Items
	r.Pagination = f.Pagination
	return nil
}

// CursorPaginationInfo mirrors CursorPaginationInfo in
// docs/cyoda/api/openapi-audit.yml line 504.
type CursorPaginationInfo struct {
	HasNext    bool   `json:"hasNext"`
	NextCursor string `json:"nextCursor,omitempty"`
}

// UnmarshalJSON enforces strict drift detection on the pagination
// envelope.
func (p *CursorPaginationInfo) UnmarshalJSON(b []byte) error {
	type flat struct {
		HasNext    bool   `json:"hasNext"`
		NextCursor string `json:"nextCursor,omitempty"`
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var f flat
	if err := dec.Decode(&f); err != nil {
		return fmt.Errorf("strict decode CursorPaginationInfo: %w", err)
	}
	p.HasNext = f.HasNext
	p.NextCursor = f.NextCursor
	return nil
}
