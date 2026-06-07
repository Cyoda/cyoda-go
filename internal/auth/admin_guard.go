package auth

import (
	"net/http"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// requireAdmin gates administrative endpoints: the key pair handler, the
// trusted-key handler, and the M2M client handler. These routes are wrapped
// by the auth middleware, so a missing UserContext here means the middleware
// was bypassed or misconfigured — respond 401. A present UserContext lacking
// ROLE_ADMIN is a genuine authorization failure — respond 403.
//
// Both branches respond as RFC 9457 problem-detail JSON via common.WriteError
// so the wire shape (Content-Type, errorCode property) matches every other
// 4xx in the system.
//
// Returns true when the caller may proceed; otherwise writes the response
// and returns false.
func requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	uc := spi.GetUserContext(r.Context())
	if uc == nil {
		common.WriteError(w, r, common.Operational(
			http.StatusUnauthorized, common.ErrCodeUnauthorized, "authentication failed"))
		return false
	}
	if !spi.HasRole(uc.Roles, "ROLE_ADMIN") {
		common.WriteError(w, r, common.Operational(
			http.StatusForbidden, common.ErrCodeForbidden, "forbidden"))
		return false
	}
	return true
}
