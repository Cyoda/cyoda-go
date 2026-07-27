package grpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// NewCloudEvent creates a CloudEvent with JSON-marshalled payload as text data.
func NewCloudEvent(eventType string, payload any) (*cepb.CloudEvent, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal CloudEvent payload: %w", err)
	}

	return &cepb.CloudEvent{
		Id:          uuid.New().String(),
		Source:      "cyoda",
		SpecVersion: "1.0",
		Type:        eventType,
		Data:        &cepb.CloudEvent_TextData{TextData: string(data)},
	}, nil
}

// AttachAuthContext adds CloudEvents Auth Context extension attributes to a CloudEvent
// based on the UserContext in the request context. authtype is emitted verbatim from
// the principal's explicit Kind — never sniffed from roles — and is always one of the
// pinned wire values {user,service,system}.
//
// Fails loud rather than emitting a bogus or absent authtype: no UserContext on ctx,
// an unset Kind, or a Kind outside {user,service,system} (e.g. a misconfigured mock)
// all return an error. Callers must fail the dispatch on error — the callout must
// never be sent without a faithful AuthContext.
//
// Every failure returned here wraps contract.ErrAuthContextUnavailable: none of
// these conditions can originate from client-supplied input (the client does not
// control dispatch-path UserContext construction), so classifyWorkflowError
// (internal/domain/entity) matches the sentinel via errors.Is and maps it to a
// sanitized 5xx with a ticket UUID, never a 400 that would echo the raw message
// (including the principal id) to the client.
//
// See: https://github.com/cloudevents/spec/blob/main/cloudevents/extensions/authcontext.md
func AttachAuthContext(ctx context.Context, ce *cepb.CloudEvent) error {
	uc := spi.GetUserContext(ctx)
	if uc == nil {
		return errors.Join(contract.ErrAuthContextUnavailable,
			errors.New("attach auth context: no user context on dispatch path"))
	}
	if uc.Kind == "" {
		return errors.Join(contract.ErrAuthContextUnavailable,
			fmt.Errorf("attach auth context: principal kind unset for principal %q", uc.UserID))
	}
	switch uc.Kind {
	case spi.PrincipalUser, spi.PrincipalService, spi.PrincipalSystem:
		// pinned wire contract: authtype ∈ {user,service,system}
	default:
		return errors.Join(contract.ErrAuthContextUnavailable,
			fmt.Errorf("attach auth context: unrecognized principal kind %q for principal %q", uc.Kind, uc.UserID))
	}
	if ce == nil {
		return errors.Join(contract.ErrAuthContextUnavailable,
			errors.New("attach auth context: nil cloud event"))
	}

	if ce.Attributes == nil {
		ce.Attributes = make(map[string]*cepb.CloudEvent_CloudEventAttributeValue)
	}

	ce.Attributes["authtype"] = &cepb.CloudEvent_CloudEventAttributeValue{
		Attr: &cepb.CloudEvent_CloudEventAttributeValue_CeString{CeString: string(uc.Kind)},
	}
	ce.Attributes["authid"] = &cepb.CloudEvent_CloudEventAttributeValue{
		Attr: &cepb.CloudEvent_CloudEventAttributeValue_CeString{CeString: uc.UserID},
	}

	// Claims: roles as comma-separated string.
	if len(uc.Roles) > 0 {
		ce.Attributes["authclaims"] = &cepb.CloudEvent_CloudEventAttributeValue{
			Attr: &cepb.CloudEvent_CloudEventAttributeValue_CeString{CeString: strings.Join(uc.Roles, ",")},
		}
	}
	return nil
}

// ParseCloudEvent extracts the event type and raw JSON payload from a CloudEvent.
// Supports both TextData (string) and BinaryData (bytes) variants.
func ParseCloudEvent(ce *cepb.CloudEvent) (eventType string, payload json.RawMessage, err error) {
	if ce == nil {
		return "", nil, errors.New("cloud event is nil")
	}

	switch d := ce.Data.(type) {
	case *cepb.CloudEvent_TextData:
		return ce.Type, json.RawMessage(d.TextData), nil
	case *cepb.CloudEvent_BinaryData:
		return ce.Type, json.RawMessage(d.BinaryData), nil
	default:
		return "", nil, fmt.Errorf("unsupported CloudEvent data variant: %T", ce.Data)
	}
}

// ExtractTransactionID extracts the "transactionId" string field from a JSON payload.
// Returns "" if the field is absent or not a string.
func ExtractTransactionID(payload json.RawMessage) string {
	return ExtractStringField(payload, "transactionId")
}

// ExtractStringField extracts a string field by name from a JSON payload.
// Returns "" if the field is absent, not a string, or the payload is invalid JSON.
func ExtractStringField(payload json.RawMessage, field string) string {
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		return ""
	}
	v, ok := m[field]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}
