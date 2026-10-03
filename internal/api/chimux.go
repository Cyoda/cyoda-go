package api

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

// ChiMux adapts a chi.Router to satisfy the generated api.ServeMux interface.
// The generated code registers routes using "METHOD /path" patterns (Go 1.22
// style). Go's standard http.ServeMux panics when overlapping wildcard
// segments appear (e.g. /model/{entityName}/… vs /model/export/…).  Chi
// handles those patterns without conflict.
type ChiMux struct {
	r     chi.Router
	inner []func(http.Handler) http.Handler
}

// NewChiMux returns a ChiMux backed by a fresh chi router. inner wraps every
// route's handler, outermost first, and runs after the route's ROLE_M2M check:
// app.go passes the transaction-join middleware here, so a caller the check
// refuses joins nothing.
func NewChiMux(inner ...func(http.Handler) http.Handler) *ChiMux {
	return &ChiMux{r: chi.NewRouter(), inner: inner}
}

// HandleFunc parses the "METHOD /path" pattern and registers it on chi.
// Every route not on the allow-list requires ROLE_M2M (see RequireM2M).
func (c *ChiMux) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	var h http.Handler = http.HandlerFunc(handler)
	for i := len(c.inner) - 1; i >= 0; i-- {
		h = c.inner[i](h)
	}
	method, path, ok := strings.Cut(pattern, " ")
	if !ok {
		// No method prefix — register for all methods. The allow-list names
		// methods, so such a route is always guarded.
		c.r.Handle(pattern, RequireM2M(h))
		return
	}
	if !IsAllowListed(method, path) {
		h = RequireM2M(h)
	}
	c.r.Method(method, path, h)
}

// ServeHTTP delegates to the underlying chi router.
func (c *ChiMux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.r.ServeHTTP(w, r)
}
