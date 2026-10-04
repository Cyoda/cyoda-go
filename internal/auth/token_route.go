package auth

import (
	"net/http"

	"github.com/cyoda-platform/cyoda-go/internal/tenantroute"
)

// TokenPattern is the token endpoint's route, relative to the context path.
// It has no method: the handler answers its own 405.
const TokenPattern = "/tenants/{tenant}/oauth/token"

// RegisterTokenRoute registers the token handler h on mux in the tenant
// group. A path the group refuses answers 400 invalid_request in the
// endpoint's OAuth format.
func RegisterTokenRoute(mux *http.ServeMux, h http.Handler) {
	tenantroute.Handle(mux, TokenPattern, h, func(w http.ResponseWriter, _ *http.Request) {
		writeTokenError(w, http.StatusBadRequest, "invalid_request", "invalid tenant")
	})
}
