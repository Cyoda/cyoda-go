package grpc

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// ceAttr returns the string value of a CloudEvent attribute and whether it is
// present.
func ceAttr(ce *cepb.CloudEvent, key string) (string, bool) {
	v, ok := ce.Attributes[key]
	if !ok {
		return "", false
	}
	return v.GetCeString(), true
}

// TestCalloutIdentity_EveryPath pins the auth context of a callout on each
// path a callout is made from: the identity is computed from the dispatching
// context once (IdentityFrom) and attached as computed. authid/authtype name
// the attributed principal, authexecid/authexectype the executor, and
// authclaims the executor's roles.
func TestCalloutIdentity_EveryPath(t *testing.T) {
	oboClient := spi.Principal{ID: "obo-client", Kind: spi.PrincipalService}
	tests := []struct {
		name                     string
		ctx                      context.Context
		wantID, wantType         string
		wantExecID, wantExecType string
		wantClaims               string // "" means absent
	}{
		{
			name: "on-behalf-of request",
			ctx: spi.WithUserContext(context.Background(), &spi.UserContext{
				UserID: "alice", Kind: spi.PrincipalUser, Executor: &oboClient, Roles: []string{"ROLE_M2M"},
			}),
			wantID: "alice", wantType: "user", wantExecID: "obo-client", wantExecType: "service", wantClaims: "ROLE_M2M",
		},
		{
			name: "client's own request",
			ctx: spi.WithUserContext(context.Background(), &spi.UserContext{
				UserID: "C1", Kind: spi.PrincipalService, Roles: []string{"ROLE_M2M"},
			}),
			wantID: "C1", wantType: "service", wantExecID: "C1", wantExecType: "service", wantClaims: "ROLE_M2M",
		},
		{
			name: "processor write-back in the origin's transaction",
			ctx: spi.WithTransaction(spi.WithUserContext(context.Background(), &spi.UserContext{
				UserID: "compute", Kind: spi.PrincipalService, Roles: []string{"ROLE_M2M"},
			}), &spi.TransactionState{ID: "tx-1", Origin: spi.Principal{ID: "alice", Kind: spi.PrincipalUser}}),
			wantID: "alice", wantType: "user", wantExecID: "compute", wantExecType: "service", wantClaims: "ROLE_M2M",
		},
		{
			name: "scheduled fire",
			ctx: spi.WithTransaction(spi.WithUserContext(context.Background(), &spi.UserContext{
				UserID: "system", Kind: spi.PrincipalSystem,
			}), &spi.TransactionState{ID: "tx-2", Origin: spi.Principal{ID: "bob", Kind: spi.PrincipalUser}}),
			wantID: "bob", wantType: "user", wantExecID: "system", wantExecType: "system",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ce := &cepb.CloudEvent{}
			if err := AttachAuthContext(ce, IdentityFrom(tt.ctx)); err != nil {
				t.Fatalf("AttachAuthContext: %v", err)
			}
			for key, want := range map[string]string{
				"authid": tt.wantID, "authtype": tt.wantType,
				"authexecid": tt.wantExecID, "authexectype": tt.wantExecType,
			} {
				if got, ok := ceAttr(ce, key); !ok || got != want {
					t.Errorf("%s = %q (present %v), want %q", key, got, ok, want)
				}
			}
			got, ok := ceAttr(ce, "authclaims")
			switch {
			case tt.wantClaims == "" && ok:
				t.Errorf("authclaims = %q, want absent", got)
			case tt.wantClaims != "" && got != tt.wantClaims:
				t.Errorf("authclaims = %q (present %v), want %q", got, ok, tt.wantClaims)
			}
		})
	}
}

// TestIdentityFrom_RolesAreACopy guards that the identity a callout carries
// does not share its roles with the request's UserContext.
func TestIdentityFrom_RolesAreACopy(t *testing.T) {
	uc := &spi.UserContext{UserID: "C1", Kind: spi.PrincipalService, Roles: []string{"ROLE_M2M"}}
	id := IdentityFrom(spi.WithUserContext(context.Background(), uc))
	uc.Roles[0] = "ROLE_CHANGED"
	if len(id.Roles) != 1 || id.Roles[0] != "ROLE_M2M" {
		t.Errorf("Roles = %v, want the roles as they were when the identity was computed", id.Roles)
	}
}

// TestAttachAuthContext_KindDriven guards that the type attributes are the
// principals' explicit kinds, never sniffed from roles: a user-kind principal
// carrying ROLE_M2M still emits authtype=user.
func TestAttachAuthContext_KindDriven(t *testing.T) {
	for _, kind := range []spi.PrincipalKind{spi.PrincipalUser, spi.PrincipalService, spi.PrincipalSystem} {
		t.Run(string(kind), func(t *testing.T) {
			p := spi.Principal{ID: "principal-1", Kind: kind}
			ce := &cepb.CloudEvent{}
			if err := AttachAuthContext(ce, CalloutIdentity{Attributed: p, Executor: p, Roles: []string{"ROLE_M2M"}}); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got, _ := ceAttr(ce, "authtype"); got != string(kind) {
				t.Errorf("authtype = %q, want %q", got, kind)
			}
			if got, _ := ceAttr(ce, "authexectype"); got != string(kind) {
				t.Errorf("authexectype = %q, want %q", got, kind)
			}
		})
	}
}

// TestAttachAuthContext_RefusesAnIncompleteIdentity guards the fail-loud rule:
// a callout is never sent without a faithful auth context. A missing id or an
// unset or unrecognized kind on either principal is refused, nothing is
// attached, and the error wraps contract.ErrAuthContextUnavailable — none of
// these can come from client input, so the failure maps to a sanitized 5xx.
func TestAttachAuthContext_RefusesAnIncompleteIdentity(t *testing.T) {
	alice := spi.Principal{ID: "alice", Kind: spi.PrincipalUser}
	svc := spi.Principal{ID: "C1", Kind: spi.PrincipalService}
	tests := []struct {
		name string
		id   CalloutIdentity
	}{
		{"no user context on the dispatching path", IdentityFrom(context.Background())},
		{"attributed id empty", CalloutIdentity{Attributed: spi.Principal{Kind: spi.PrincipalUser}, Executor: svc}},
		{"executor id empty", CalloutIdentity{Attributed: alice, Executor: spi.Principal{Kind: spi.PrincipalService}}},
		{"attributed kind unset", CalloutIdentity{Attributed: spi.Principal{ID: "alice"}, Executor: svc}},
		{"executor kind unset", CalloutIdentity{Attributed: alice, Executor: spi.Principal{ID: "C1"}}},
		{"attributed kind unrecognized", CalloutIdentity{Attributed: spi.Principal{ID: "alice", Kind: "bogus"}, Executor: svc}},
		{"executor kind unrecognized", CalloutIdentity{Attributed: alice, Executor: spi.Principal{ID: "C1", Kind: "bogus"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ce := &cepb.CloudEvent{}
			err := AttachAuthContext(ce, tt.id)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !errors.Is(err, contract.ErrAuthContextUnavailable) {
				t.Errorf("error does not wrap contract.ErrAuthContextUnavailable: %v", err)
			}
			if ce.Attributes != nil {
				t.Errorf("attributes attached: %v", ce.Attributes)
			}
		})
	}
}

// TestAttachAuthContext_NilCloudEvent guards against a nil CloudEvent even
// when the identity is complete.
func TestAttachAuthContext_NilCloudEvent(t *testing.T) {
	p := spi.Principal{ID: "principal-1", Kind: spi.PrincipalUser}
	err := AttachAuthContext(nil, CalloutIdentity{Attributed: p, Executor: p})
	if err == nil {
		t.Fatal("expected error for nil cloud event")
	}
	if !errors.Is(err, contract.ErrAuthContextUnavailable) {
		t.Errorf("expected error to wrap contract.ErrAuthContextUnavailable, got %v", err)
	}
}

// TestAttachAuthContext_AuthClaimsFromRoles guards that authclaims is the
// executor's roles joined by commas.
func TestAttachAuthContext_AuthClaimsFromRoles(t *testing.T) {
	p := spi.Principal{ID: "principal-1", Kind: spi.PrincipalService}
	ce := &cepb.CloudEvent{}
	if err := AttachAuthContext(ce, CalloutIdentity{Attributed: p, Executor: p, Roles: []string{"ROLE_M2M", "ROLE_ADMIN"}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, ok := ceAttr(ce, "authclaims"); !ok || got != "ROLE_M2M,ROLE_ADMIN" {
		t.Errorf("authclaims = %q (present %v), want ROLE_M2M,ROLE_ADMIN", got, ok)
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
