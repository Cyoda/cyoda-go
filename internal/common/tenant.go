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
// subsystems act as — one identity shared by every one of them (the
// scheduler's fire path, the async-search stale-job reaper, the app layer's
// own system context), not a per-subsystem fake. Attribution downstream
// (audit trails, ChangeUser/ChangeUserKind) must see one system principal
// regardless of which subsystem drove the write.
//
// It lives in common because its consumers span the tree — internal/scheduler,
// internal/cluster and internal/domain/search all need the identical identity,
// and a domain package reaching into internal/scheduler for it was a
// dependency edge with no other justification.
func SystemPrincipal() spi.Principal {
	return spi.Principal{ID: systemPrincipalID, Kind: spi.PrincipalSystem}
}

// SystemUserContext derives, from context.Background(), a context.Context
// carrying a synthesised system UserContext scoped to tenant. It exists
// because TransactionManager.Begin rejects any context whose UserContext
// has no tenant (plugins/memory/txmanager.go Begin), and a background path
// — the search reaper's per-job write — has no caller-derived UserContext at
// all: there is no inbound HTTP/gRPC request to inherit one from.
//
// Always builds from context.Background(), not a caller-supplied parent, so
// no request-scoped values (deadlines, trace IDs) a caller didn't intend to
// share leak into the background work. Scoping to the tenant the work
// belongs to is what keeps one tenant's task from ever being written under
// another tenant's (or no tenant's) context.
func SystemUserContext(tenant spi.TenantID) context.Context {
	return spi.WithUserContext(context.Background(), SystemUserContextValue(tenant))
}

// SystemUserContextValue returns the synthesised system *spi.UserContext
// scoped to tenant that SystemUserContext carries. It exists for the one
// caller that needs the value itself, not a context wrapping it — the search
// reaper's reclaim path builds several contexts (a scan-scoped executor
// context and a tenant-scoped write context) from one principal, and must
// reuse the SAME construction SystemUserContext uses rather than spelling a
// second bespoke system principal. Kind=system; never a real end-user.
func SystemUserContextValue(tenant spi.TenantID) *spi.UserContext {
	return &spi.UserContext{
		UserID:   systemPrincipalID,
		UserName: systemPrincipalID,
		Kind:     spi.PrincipalSystem,
		Tenant:   spi.Tenant{ID: tenant},
	}
}
