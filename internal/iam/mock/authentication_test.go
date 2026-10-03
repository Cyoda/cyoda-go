package mock_test

import (
	"context"
	"net/http/httptest"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	"github.com/cyoda-platform/cyoda-go/internal/contract"
	mockiam "github.com/cyoda-platform/cyoda-go/internal/iam/mock"
)

// TestMockAuthentication_CarriesClientTokenMarker: in mock mode every request
// carries the default principal and a client-token marker for it, so the
// compute-stream guard admits a mock-mode compute node without a client store.
func TestMockAuthentication_CarriesClientTokenMarker(t *testing.T) {
	defaultUser := &spi.UserContext{
		UserID: "mock-user-001",
		Kind:   spi.PrincipalService,
		Tenant: spi.Tenant{ID: "mock-tenant"},
		Roles:  []string{"ROLE_M2M"},
	}
	svc := mockiam.NewAuthenticationService(defaultUser)

	ctx, err := svc.Authenticate(context.Background(), httptest.NewRequest("GET", "/", nil))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	uc := spi.GetUserContext(ctx)
	if uc == nil || uc.UserID != "mock-user-001" || uc.Kind != spi.PrincipalService {
		t.Fatalf("UserContext = %+v, want the default principal", uc)
	}
	want := contract.ClientToken{ClientID: "mock-user-001", Gen: 0}
	if ct, ok := contract.ClientTokenFrom(ctx); !ok || ct != want {
		t.Fatalf("ClientTokenFrom = %+v, %v; want %+v, true", ct, ok, want)
	}
}
