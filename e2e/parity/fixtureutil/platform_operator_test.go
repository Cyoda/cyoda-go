package fixtureutil

import (
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/auth"
)

func TestMintPlatformOperatorJWT_IsAPlatformAdminPersonToken(t *testing.T) {
	ks, err := GenerateJWTKeySet()
	if err != nil {
		t.Fatal(err)
	}
	op := MintPlatformOperatorJWT(t, ks)
	if op.ID != string(auth.PlatformTenantID) {
		t.Fatalf("ID = %q, want %q", op.ID, auth.PlatformTenantID)
	}
	parsed, err := auth.Parse(op.Token)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := auth.Verify(parsed.SigningInput, parsed.Signature, &ks.Key.PublicKey); err != nil {
		t.Fatalf("signature: %v", err)
	}
	if got := parsed.Header["kid"]; got != ks.Kid {
		t.Errorf("kid = %v, want %s", got, ks.Kid)
	}
	if got := parsed.Claims["caas_org_id"]; got != "PLATFORM" {
		t.Errorf("caas_org_id = %v, want PLATFORM", got)
	}
	roles, _ := parsed.Claims["user_roles"].([]any)
	if len(roles) != 1 || roles[0] != "ROLE_ADMIN" {
		t.Errorf("user_roles = %v, want [ROLE_ADMIN]", parsed.Claims["user_roles"])
	}
}
