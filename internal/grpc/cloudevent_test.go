package grpc

import (
	"context"
	"encoding/json"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
)

// TestAttachAuthContext_KindDriven guards that authtype is emitted verbatim
// from the explicit principal Kind, not sniffed from roles. The ROLE_M2M
// case is a regression guard: a user-kind principal carrying ROLE_M2M (e.g.
// an OBO-style delegated call) must still emit authtype=user — role-sniffing
// is dead.
func TestAttachAuthContext_KindDriven(t *testing.T) {
	tests := []struct {
		name         string
		kind         spi.PrincipalKind
		roles        []string
		wantAuthType string
	}{
		{"user kind", spi.PrincipalUser, nil, "user"},
		{"service kind", spi.PrincipalService, nil, "service"},
		{"system kind", spi.PrincipalSystem, nil, "system"},
		{"user kind with ROLE_M2M regression", spi.PrincipalUser, []string{"ROLE_M2M"}, "user"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := spi.WithUserContext(context.Background(), &spi.UserContext{
				UserID: "principal-1",
				Kind:   tt.kind,
				Roles:  tt.roles,
			})
			ce := &cepb.CloudEvent{}

			if err := AttachAuthContext(ctx, ce); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			authType, ok := ce.Attributes["authtype"]
			if !ok {
				t.Fatal("expected authtype attribute")
			}
			if got := authType.GetCeString(); got != tt.wantAuthType {
				t.Errorf("expected authtype=%s, got %s", tt.wantAuthType, got)
			}

			authID, ok := ce.Attributes["authid"]
			if !ok {
				t.Fatal("expected authid attribute")
			}
			if got := authID.GetCeString(); got != "principal-1" {
				t.Errorf("expected authid=principal-1, got %s", got)
			}
		})
	}
}

// TestAttachAuthContext_NilUserContext guards the fail-loud rule: a dispatch
// path with no UserContext on ctx must fail rather than emit a bogus or
// absent authtype.
func TestAttachAuthContext_NilUserContext(t *testing.T) {
	ce := &cepb.CloudEvent{}
	err := AttachAuthContext(context.Background(), ce)
	if err == nil {
		t.Fatal("expected error for nil user context")
	}
	if ce.Attributes != nil {
		t.Errorf("expected no attributes to be attached, got %v", ce.Attributes)
	}
}

// TestAttachAuthContext_UnsetKind guards that an unset Kind (legacy/unmigrated
// auth constructor) fails loud rather than emitting an empty authtype.
func TestAttachAuthContext_UnsetKind(t *testing.T) {
	ctx := spi.WithUserContext(context.Background(), &spi.UserContext{UserID: "principal-1"})
	ce := &cepb.CloudEvent{}
	err := AttachAuthContext(ctx, ce)
	if err == nil {
		t.Fatal("expected error for unset principal kind")
	}
	if ce.Attributes != nil {
		t.Errorf("expected no attributes to be attached, got %v", ce.Attributes)
	}
}

// TestAttachAuthContext_InvalidKind guards the pinned wire contract:
// authtype must always be one of {user,service,system}. A misconfigured
// mock/test double (e.g. CYODA_IAM_MOCK_KIND=bogus) producing an out-of-set
// Kind must fail the dispatch, not emit the bogus value onto the wire.
func TestAttachAuthContext_InvalidKind(t *testing.T) {
	ctx := spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID: "principal-1",
		Kind:   spi.PrincipalKind("bogus"),
	})
	ce := &cepb.CloudEvent{}
	err := AttachAuthContext(ctx, ce)
	if err == nil {
		t.Fatal("expected error for invalid principal kind")
	}
	if ce.Attributes != nil {
		t.Errorf("expected no attributes to be attached, got %v", ce.Attributes)
	}
}

// TestAttachAuthContext_NilCloudEvent guards against a nil CloudEvent even
// when the UserContext is well-formed.
func TestAttachAuthContext_NilCloudEvent(t *testing.T) {
	ctx := spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID: "principal-1",
		Kind:   spi.PrincipalUser,
	})
	if err := AttachAuthContext(ctx, nil); err == nil {
		t.Fatal("expected error for nil cloud event")
	}
}

// TestAttachAuthContext_AuthClaimsFromRoles guards that authclaims is
// populated from roles when present, and omitted when absent.
func TestAttachAuthContext_AuthClaimsFromRoles(t *testing.T) {
	ctx := spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID: "principal-1",
		Kind:   spi.PrincipalUser,
		Roles:  []string{"ROLE_USER", "ROLE_ADMIN"},
	})
	ce := &cepb.CloudEvent{}
	if err := AttachAuthContext(ctx, ce); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	claims, ok := ce.Attributes["authclaims"]
	if !ok {
		t.Fatal("expected authclaims attribute")
	}
	if got := claims.GetCeString(); got != "ROLE_USER,ROLE_ADMIN" {
		t.Errorf("expected authclaims=ROLE_USER,ROLE_ADMIN, got %s", got)
	}
}

func TestNewCloudEvent_ParseCloudEvent_RoundTrip(t *testing.T) {
	type testPayload struct {
		TransactionID string `json:"transactionId"`
		Name          string `json:"name"`
	}

	input := testPayload{TransactionID: "txn-123", Name: "alice"}

	ce, err := NewCloudEvent(EntityCreateRequest, input)
	if err != nil {
		t.Fatalf("NewCloudEvent returned error: %v", err)
	}

	if ce.Id == "" {
		t.Error("expected non-empty ID")
	}
	if ce.Source != "cyoda" {
		t.Errorf("expected source 'cyoda', got %q", ce.Source)
	}
	if ce.SpecVersion != "1.0" {
		t.Errorf("expected spec_version '1.0', got %q", ce.SpecVersion)
	}
	if ce.Type != EntityCreateRequest {
		t.Errorf("expected type %q, got %q", EntityCreateRequest, ce.Type)
	}

	eventType, payload, err := ParseCloudEvent(ce)
	if err != nil {
		t.Fatalf("ParseCloudEvent returned error: %v", err)
	}
	if eventType != EntityCreateRequest {
		t.Errorf("expected event type %q, got %q", EntityCreateRequest, eventType)
	}

	var result testPayload
	if err := json.Unmarshal(payload, &result); err != nil {
		t.Fatalf("failed to unmarshal payload: %v", err)
	}
	if result.TransactionID != "txn-123" {
		t.Errorf("expected transactionId 'txn-123', got %q", result.TransactionID)
	}
	if result.Name != "alice" {
		t.Errorf("expected name 'alice', got %q", result.Name)
	}
}

func TestExtractTransactionID_Present(t *testing.T) {
	payload := json.RawMessage(`{"transactionId":"txn-456","other":"value"}`)
	got := ExtractTransactionID(payload)
	if got != "txn-456" {
		t.Errorf("expected 'txn-456', got %q", got)
	}
}

func TestExtractTransactionID_Absent(t *testing.T) {
	payload := json.RawMessage(`{"other":"value"}`)
	got := ExtractTransactionID(payload)
	if got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

func TestParseCloudEvent_Nil(t *testing.T) {
	_, _, err := ParseCloudEvent(nil)
	if err == nil {
		t.Fatal("expected error for nil CloudEvent")
	}
}

func TestParseCloudEvent_BinaryData(t *testing.T) {
	ce := &cepb.CloudEvent{
		Id:          "test-id",
		Source:      "test",
		SpecVersion: "1.0",
		Type:        "test.type",
		Data:        &cepb.CloudEvent_BinaryData{BinaryData: []byte(`{"key":"value"}`)},
	}

	eventType, payload, err := ParseCloudEvent(ce)
	if err != nil {
		t.Fatalf("unexpected error for binary_data: %v", err)
	}
	if eventType != "test.type" {
		t.Errorf("eventType = %q, want %q", eventType, "test.type")
	}
	if string(payload) != `{"key":"value"}` {
		t.Errorf("payload = %q, want %q", string(payload), `{"key":"value"}`)
	}
}

func TestExtractStringField(t *testing.T) {
	payload := json.RawMessage(`{"foo":"bar","count":42}`)

	if got := ExtractStringField(payload, "foo"); got != "bar" {
		t.Errorf("expected 'bar', got %q", got)
	}
	if got := ExtractStringField(payload, "missing"); got != "" {
		t.Errorf("expected empty string for missing field, got %q", got)
	}
	if got := ExtractStringField(payload, "count"); got != "" {
		t.Errorf("expected empty string for non-string field, got %q", got)
	}
}
