package common

import (
	"context"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// TenantFromContext returns the bound tenant ID from the request's
// UserContext, or empty string when no UserContext is on the context
// (unauthenticated paths, internal callers, test setups without
// spi.WithUserContext).
//
// Use this anywhere observability or cache-keying needs a tenant
// discriminator and the caller doesn't already have a typed accessor.
// Three internal packages (modelcache, search, workflow) all needed the
// same nil-safe extractor; this is the rule-of-three extraction point.
func TenantFromContext(ctx context.Context) string {
	uc := spi.GetUserContext(ctx)
	if uc == nil {
		return ""
	}
	return string(uc.Tenant.ID)
}
