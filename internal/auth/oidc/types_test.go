package oidc

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestOidcProvider_Active(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		p    OidcProvider
		want bool
	}{
		{"active-when-InvalidatedAt-nil", OidcProvider{InvalidatedAt: nil}, true},
		{"inactive-when-InvalidatedAt-set", OidcProvider{InvalidatedAt: &now}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.p.Active(); got != c.want {
				t.Errorf("Active() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestOidcProvider_JSONRoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second) // JSON time has second precision
	rc := "cognito:groups"
	p := OidcProvider{
		ID:                 uuid.MustParse("11111111-2222-3333-4444-555555555555"),
		WellKnownConfigURI: "https://idp.example/.well-known/openid-configuration",
		Issuers:            []string{"https://idp.example"},
		ExpectedAudiences:  []string{"api1"},
		RolesClaim:         &rc,
		InvalidatedAt:      nil,
		CreatedAt:          now,
		OwnerLegalEntityID: uuid.MustParse("66666666-7777-8888-9999-aaaaaaaaaaaa"),
	}

	blob, err := json.Marshal(&p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back OidcProvider
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.ID != p.ID || back.WellKnownConfigURI != p.WellKnownConfigURI {
		t.Errorf("round trip lost fields: %+v vs %+v", p, back)
	}
	if back.RolesClaim == nil || *back.RolesClaim != rc {
		t.Errorf("RolesClaim lost: %v", back.RolesClaim)
	}
	if len(back.Issuers) != 1 || back.Issuers[0] != "https://idp.example" {
		t.Errorf("Issuers lost: %v", back.Issuers)
	}
	if len(back.ExpectedAudiences) != 1 || back.ExpectedAudiences[0] != "api1" {
		t.Errorf("ExpectedAudiences lost: %v", back.ExpectedAudiences)
	}
	if !back.CreatedAt.Equal(now) {
		t.Errorf("CreatedAt lost: got %v want %v", back.CreatedAt, now)
	}
}

func TestUriOwnershipHistory_RoundTrip(t *testing.T) {
	reg := time.Now().UTC().Truncate(time.Second)
	del := reg.Add(time.Hour)
	h := UriOwnershipHistory{
		CurrentOwner: &Owner{
			TenantID:     "tenant-a",
			ProviderUUID: "uuid-a",
			RegisteredAt: reg,
		},
		Past: []Owner{
			{TenantID: "tenant-b", ProviderUUID: "uuid-b", RegisteredAt: reg.Add(-time.Hour), DeletedAt: &del},
		},
	}
	blob, _ := json.Marshal(&h)
	var back UriOwnershipHistory
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.CurrentOwner == nil || back.CurrentOwner.TenantID != "tenant-a" {
		t.Errorf("CurrentOwner lost: %+v", back.CurrentOwner)
	}
	if len(back.Past) != 1 || back.Past[0].DeletedAt == nil || back.Past[0].TenantID != "tenant-b" {
		t.Errorf("Past lost: %+v", back.Past)
	}
}

func TestOidcProvider_OmitEmptyFields(t *testing.T) {
	// Minimal provider: only required fields populated.
	p := OidcProvider{
		ID:                 uuid.MustParse("11111111-2222-3333-4444-555555555555"),
		WellKnownConfigURI: "https://idp.example",
		CreatedAt:          time.Now().UTC().Truncate(time.Second),
		OwnerLegalEntityID: uuid.MustParse("66666666-7777-8888-9999-aaaaaaaaaaaa"),
	}
	blob, _ := json.Marshal(&p)
	// Optional fields should not appear in the JSON when empty/nil.
	s := string(blob)
	for _, field := range []string{`"issuers"`, `"expectedAudiences"`, `"rolesClaim"`, `"invalidatedAt"`} {
		if strings.Contains(s, field) {
			t.Errorf("expected %s to be omitempty, got JSON: %s", field, s)
		}
	}
}
