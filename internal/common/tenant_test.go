package common

import (
	"context"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func TestTenantFromContext_NoUserContext_ReturnsEmpty(t *testing.T) {
	if got := TenantFromContext(context.Background()); got != "" {
		t.Errorf("expected empty string for bare context, got %q", got)
	}
}

func TestTenantFromContext_WithUserContext_ReturnsTenantID(t *testing.T) {
	uc := &spi.UserContext{
		UserID: "u1",
		Tenant: spi.Tenant{ID: spi.TenantID("acme")},
	}
	ctx := spi.WithUserContext(context.Background(), uc)
	if got := TenantFromContext(ctx); got != "acme" {
		t.Errorf("expected 'acme', got %q", got)
	}
}
