// Package tenantroute is the /tenants/{tenant}/… route group: routes that
// address a tenant by name in their path. Every route of the group is
// registered through Handle, which checks the path before the route runs and
// hands the route the addressed tenant.
//
// Every route in the group today is token-free (the token endpoint). A route
// that carries a bearer token must also require the addressed tenant to equal
// the token's tenant, and answer a mismatch with 404, as a missing resource.
// That check is built with the first such route, in this package, so it
// applies to the whole group; app's route registration test fails until the
// new route is listed there.
package tenantroute

import (
	"context"
	"net/http"
	"strings"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// Prefix starts every pattern of the group.
const Prefix = "/tenants/{tenant}/"

type addressedKey struct{}

// Addressed returns the tenant the request's path addresses, set by Handle.
// ok is false outside the group.
func Addressed(ctx context.Context) (spi.TenantID, bool) {
	t, ok := ctx.Value(addressedKey{}).(spi.TenantID)
	return t, ok
}

// Handle registers h on mux at pattern, which must start with Prefix. Before
// h runs, the request is refused through refuse — which writes the route's
// own 400 — when its path carries any percent-encoding (Go's mux decodes
// every segment before matching, so an encoded path would otherwise reach the
// route under a spelling a gateway rule does not match; a valid path never
// needs encoding), or when the tenant segment is not an API tenant. The
// segment is taken verbatim: no folding, trimming or decoding.
func Handle(mux *http.ServeMux, pattern string, h http.Handler, refuse func(http.ResponseWriter, *http.Request)) {
	if !strings.HasPrefix(pattern, Prefix) {
		panic("tenantroute: pattern outside the group: " + pattern)
	}
	mux.Handle(pattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawPath != "" {
			refuse(w, r)
			return
		}
		tenant := spi.TenantID(r.PathValue("tenant"))
		if common.ValidateAPITenantID(tenant) != nil {
			refuse(w, r)
			return
		}
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), addressedKey{}, tenant)))
	}))
}
