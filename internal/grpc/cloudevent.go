package grpc

import (
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

// AttachAuthContext adds the CloudEvents Auth Context extension attributes for
// id to a CloudEvent:
//
//   - authtype / authid: the attributed principal — who the work is for;
//   - authexectype / authexecid: the executor — who does it;
//   - authclaims: the executor's roles, comma separated, when it has any.
//
// The type attributes are the principals' explicit kinds, never sniffed from
// roles, and always one of the pinned wire values {user,service,system}.
//
// Fails loud rather than emitting a bogus or absent principal: a nil event, or
// either principal with an empty id, an unset kind, or a kind outside
// {user,service,system} (e.g. a misconfigured mock) returns an error and
// attaches nothing. Callers must fail the dispatch on error — the callout must
// never be sent without a faithful AuthContext.
//
// Every failure returned here wraps contract.ErrAuthContextUnavailable: none of
// these conditions can originate from client-supplied input (the client does not
// control how a callout's identity is computed), so classifyWorkflowError
// (internal/domain/entity) matches the sentinel via errors.Is and maps it to a
// sanitized 5xx with a ticket UUID, never a 400 that would echo the raw message
// (including the principal id) to the client.
//
// See: https://github.com/cloudevents/spec/blob/main/cloudevents/extensions/authcontext.md
func AttachAuthContext(ce *cepb.CloudEvent, id CalloutIdentity) error {
	if err := checkPrincipal("attributed", id.Attributed); err != nil {
		return err
	}
	if err := checkPrincipal("executor", id.Executor); err != nil {
		return err
	}
	if ce == nil {
		return errors.Join(contract.ErrAuthContextUnavailable,
			errors.New("attach auth context: nil cloud event"))
	}

	if ce.Attributes == nil {
		ce.Attributes = make(map[string]*cepb.CloudEvent_CloudEventAttributeValue)
	}
	setStringAttr(ce, "authtype", string(id.Attributed.Kind))
	setStringAttr(ce, "authid", id.Attributed.ID)
	setStringAttr(ce, "authexectype", string(id.Executor.Kind))
	setStringAttr(ce, "authexecid", id.Executor.ID)
	if len(id.Roles) > 0 {
		setStringAttr(ce, "authclaims", strings.Join(id.Roles, ","))
	}
	return nil
}

// checkPrincipal refuses a principal a callout cannot faithfully name: an
// empty id, an unset kind, or a kind outside the pinned wire set.
func checkPrincipal(role string, p spi.Principal) error {
	if p.ID == "" {
		return errors.Join(contract.ErrAuthContextUnavailable,
			fmt.Errorf("attach auth context: %s principal has no id", role))
	}
	switch p.Kind {
	case spi.PrincipalUser, spi.PrincipalService, spi.PrincipalSystem:
		// pinned wire contract: {user,service,system}
		return nil
	case "":
		return errors.Join(contract.ErrAuthContextUnavailable,
			fmt.Errorf("attach auth context: principal kind unset for %s principal %q", role, p.ID))
	default:
		return errors.Join(contract.ErrAuthContextUnavailable,
			fmt.Errorf("attach auth context: unrecognized principal kind %q for %s principal %q", p.Kind, role, p.ID))
	}
}

func setStringAttr(ce *cepb.CloudEvent, key, value string) {
	ce.Attributes[key] = &cepb.CloudEvent_CloudEventAttributeValue{
		Attr: &cepb.CloudEvent_CloudEventAttributeValue_CeString{CeString: value},
	}
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
