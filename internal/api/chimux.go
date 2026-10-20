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
	query QueryRules
	inner []func(http.Handler) http.Handler
}

// NewChiMux returns a ChiMux backed by a fresh chi router. Every route checks
// its query against query before the generated binding reads it. inner wraps
// every route's handler, outermost first, and runs after the route's ROLE_M2M
// check: app.go passes the transaction-join middleware here, so a caller the
// check refuses joins nothing.
func NewChiMux(query QueryRules, inner ...func(http.Handler) http.Handler) *ChiMux {
	return &ChiMux{r: chi.NewRouter(), query: query, inner: inner}
}

// HandleFunc parses the "METHOD /path" pattern and registers it on chi.
// Every route not on the allow-list requires ROLE_M2M (see RequireM2M). The
// generated code registers every operation with its method, which names the
// operation whose query rules the route checks; a pattern without one is a
// wiring mistake, found at startup.
func (c *ChiMux) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	method, path, ok := strings.Cut(pattern, " ")
	if !ok {
		panic("api: route pattern without a method: " + pattern)
	}
	h := c.query.Route(method, path)(http.HandlerFunc(handler))
	for i := len(c.inner) - 1; i >= 0; i-- {
		h = c.inner[i](h)
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
