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

// systemPrincipalID identifies the platform system principal. Never a real
// end-user; kind=system.
const systemPrincipalID = "system"

// SystemPrincipal returns the platform system principal background
// subsystems act as — one identity shared by every one of them, not a
// per-subsystem fake. Attribution downstream (audit trails,
// ChangeUser/ChangeUserKind, ChangeExecutor) must see one system principal
// regardless of which subsystem drove the write.
//
// The scheduled fire path (internal/domain/workflow) stamps it as the
// executor. SystemUserContextValue carries the same identity for the paths
// that need a UserContext.
func SystemPrincipal() spi.Principal {
	return spi.Principal{ID: systemPrincipalID, Kind: spi.PrincipalSystem}
}

// SystemUserContextValue returns a synthesised system *spi.UserContext scoped
// to tenant, with the identity SystemPrincipal returns. Kind=system; never a
// real end-user.
//
// A background path has no inbound HTTP/gRPC request to inherit a
// UserContext from, and TransactionManager.Begin rejects a context whose
// UserContext has no tenant. The scheduler (its run and failure-audit contexts)
// and the search reaper (its scan and per-job write contexts) put this value
// on the contexts they build, so every one of them uses the same system
// principal. Scoping to the tenant the work belongs to is what keeps one
// tenant's task from ever being written under another tenant's (or no
// tenant's) context.
func SystemUserContextValue(tenant spi.TenantID) *spi.UserContext {
	return &spi.UserContext{
		UserID:   systemPrincipalID,
		UserName: systemPrincipalID,
		Kind:     spi.PrincipalSystem,
		Tenant:   spi.Tenant{ID: tenant},
	}
}
