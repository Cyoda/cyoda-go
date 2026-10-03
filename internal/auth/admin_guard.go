package auth

import (
	"net/http"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// RequireAdmin gates the tenant-scoped admin endpoints: trusted keys, M2M
// clients. These routes are wrapped by the auth middleware, so a missing
// UserContext here means the middleware was bypassed or misconfigured —
// respond 401. A present UserContext lacking ROLE_ADMIN is a genuine
// authorization failure — respond 403. An on-behalf-of principal (one with an
// Executor) never administers, whatever roles it holds — respond 403.
//
// Both branches respond as RFC 9457 problem-detail JSON via common.WriteError
// so the wire shape (Content-Type, errorCode property) matches every other
// 4xx in the system.
//
// Returns true when the caller may proceed; otherwise writes the response
// and returns false.
func RequireAdmin(w http.ResponseWriter, r *http.Request) bool {
	uc := spi.GetUserContext(r.Context())
	if uc == nil {
		common.WriteError(w, r, common.Operational(
			http.StatusUnauthorized, common.ErrCodeUnauthorized, "authentication failed"))
		return false
	}
	if refuseOnBehalfOf(w, r, uc) {
		return false
	}
	if !spi.HasRole(uc.Roles, "ROLE_ADMIN") {
		common.WriteError(w, r, common.Operational(
			http.StatusForbidden, common.ErrCodeForbidden, "forbidden"))
		return false
	}
	return true
}

// refuseOnBehalfOf writes 403 FORBIDDEN and reports true when uc is an
// on-behalf-of principal: a user a client states. Such a principal acts only
// on data; it never administers tenants, clients, keys or the platform.
func refuseOnBehalfOf(w http.ResponseWriter, r *http.Request, uc *spi.UserContext) bool {
	if uc.Executor == nil {
		return false
	}
	common.WriteError(w, r, common.Operational(
		http.StatusForbidden, common.ErrCodeForbidden, "on-behalf-of tokens cannot administer"))
	return true
}
