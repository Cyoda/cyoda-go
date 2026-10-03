package api

import (
	"net/http"
	"sort"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// allowList names the authenticated operations a caller may reach without
// ROLE_M2M, as "METHOD /path" in the generated router's patterns (base path
// excluded). Each one is decided by its own rule instead: the admin rule
// (clients, trusted keys), the operator rule (key pairs), or none (GET
// /account). The /admin routes app.go registers on its own mux are
// operator-guarded and never wrapped, so they are not listed here.
var allowList = map[string]bool{
	"GET /account": true,
	"GET /clients": true, "POST /clients": true,
	"DELETE /clients/{clientId}": true, "PUT /clients/{clientId}/secret": true,
	"GET /oauth/keys/trusted": true, "POST /oauth/keys/trusted": true,
	"DELETE /oauth/keys/trusted/{keyId}":          true,
	"POST /oauth/keys/trusted/{keyId}/invalidate": true, "POST /oauth/keys/trusted/{keyId}/reactivate": true,
	"POST /oauth/keys/keypair": true, "GET /oauth/keys/keypair/current": true,
	"DELETE /oauth/keys/keypair/{keyId}":          true,
	"POST /oauth/keys/keypair/{keyId}/invalidate": true, "POST /oauth/keys/keypair/{keyId}/reactivate": true,
}

// IsAllowListed reports whether method pattern is reachable without
// ROLE_M2M. method is matched as registered (upper case).
func IsAllowListed(method, pattern string) bool {
	return allowList[method+" "+pattern]
}

// AllowList returns the allow-list entries, sorted.
func AllowList() []string {
	out := make([]string, 0, len(allowList))
	for k := range allowList {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// RequireM2M wraps next so it runs only for a caller holding ROLE_M2M.
// Otherwise it writes 401 UNAUTHORIZED (no UserContext: the auth middleware
// did not run) or 403 FORBIDDEN, and next does not run.
func RequireM2M(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uc := spi.GetUserContext(r.Context())
		if uc == nil {
			common.WriteError(w, r, common.Operational(
				http.StatusUnauthorized, common.ErrCodeUnauthorized, "authentication failed"))
			return
		}
		if !spi.HasRole(uc.Roles, "ROLE_M2M") {
			common.WriteError(w, r, common.Operational(
				http.StatusForbidden, common.ErrCodeForbidden, "this operation requires ROLE_M2M"))
			return
		}
		next.ServeHTTP(w, r)
	})
}
