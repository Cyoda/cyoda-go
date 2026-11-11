package httpmw

import (
	"net/http"
	"strings"

	"github.com/cyoda-platform/cyoda-go/internal/txcoord"
)

// routeClasses is what each route that reaches the join layer does to the
// transaction a joined request runs in, by method and path pattern as the
// router records it (below the context path; see routeKey). A route missing
// here has no class; TestJoinedRequestClass_EveryRouteClassified fails when one
// is added without one.
//
//   - Write: the entity writes. A non-retryable failure discards the request's
//     scope and makes its parent rollback-only.
//   - Outside: routes whose writes do not go into the transaction — messages,
//     async search jobs, the key-value-backed administration routes (M2M
//     clients, signing and trusted keys, the token endpoint), the platform
//     clock — and model administration, which the door refuses before the
//     pass is read. Their scope takes no savepoint, their answer is never
//     rewritten, and they never mark.
//   - Read: everything else.
var routeClasses = func() map[string]txcoord.RequestClass {
	classes := make(map[string]txcoord.RequestClass)
	add := func(class txcoord.RequestClass, routes ...string) {
		for _, route := range routes {
			method, pattern, _ := strings.Cut(route, " ")
			classes[routeKey(method, pattern)] = class
		}
	}
	add(txcoord.Write,
		"POST /entity/{format}/{entityName}/{modelVersion}",
		"POST /entity/{format}",
		"PUT /entity/{format}",
		"PUT /entity/{format}/{entityId}",
		"PATCH /entity/{format}/{entityId}",
		"PUT /entity/{format}/{entityId}/{transition}",
		"PATCH /entity/{format}/{entityId}/{transition}",
		"DELETE /entity/{entityId}",
		"DELETE /entity/{entityName}/{modelVersion}",
	)
	add(txcoord.Outside,
		"POST /message/new/{subject}",
		"DELETE /message/{messageId}",
		"DELETE /message",
		"POST /search/async/{entityName}/{modelVersion}",
		"PUT /search/async/{jobId}/cancel",
		"POST /clients",
		"DELETE /clients/{clientId}",
		"PUT /clients/{clientId}/secret",
		"POST /oauth/keys/keypair",
		"DELETE /oauth/keys/keypair/{keyId}",
		"POST /oauth/keys/keypair/{keyId}/invalidate",
		"POST /oauth/keys/keypair/{keyId}/reactivate",
		"POST /oauth/keys/trusted",
		"DELETE /oauth/keys/trusted/{keyId}",
		"POST /oauth/keys/trusted/{keyId}/invalidate",
		"POST /oauth/keys/trusted/{keyId}/reactivate",
		"POST /platform/clock",
		"POST /tenants/{tenant}/oauth/token",
		"POST /model/import/{dataFormat}/{converter}/{entityName}/{modelVersion}",
		"DELETE /model/{entityName}/{modelVersion}",
		"POST /model/{entityName}/{modelVersion}/changeLevel/{changeLevel}",
		"PUT /model/{entityName}/{modelVersion}/lock",
		"PUT /model/{entityName}/{modelVersion}/unlock",
		"PUT /model/{entityName}/{modelVersion}/unique-keys",
		"POST /model/{entityName}/{modelVersion}/workflow/import",
	)
	add(txcoord.Read,
		"GET /account",
		"GET /account/subscriptions",
		"GET /audit/entity/{entityId}",
		"GET /audit/entity/{entityId}/workflow/{transactionId}/finished",
		"GET /clients",
		"GET /entity/consistency-time",
		"GET /entity/stats",
		"GET /entity/stats/states",
		"GET /entity/stats/states/{entityName}/{modelVersion}",
		"GET /entity/stats/{entityName}/{modelVersion}",
		"POST /entity/stats/{entityName}/{modelVersion}/query",
		"GET /entity/{entityId}",
		"GET /entity/{entityId}/changes",
		"GET /entity/{entityId}/transitions",
		"GET /entity/{entityName}/{modelVersion}",
		"GET /message/{messageId}",
		"GET /model/",
		"GET /model/export/{converter}/{entityName}/{modelVersion}",
		"POST /model/validate/{entityName}/{modelVersion}",
		"GET /model/{entityName}/{modelVersion}/workflow/export",
		"GET /oauth/keys/keypair/current",
		"GET /oauth/keys/trusted",
		"GET /platform-api/entity/fetch/transitions",
		"GET /platform/clock",
		"GET /platform/cluster",
		"GET /scheduled-tasks",
		"GET /search/async/{jobId}",
		"GET /search/async/{jobId}/status",
		"POST /search/direct/{entityName}/{modelVersion}",
	)
	return classes
}()

// joinedRequestClass is the class of the route r was routed to, from the
// pattern the router recorded on it (chi records the path, without a trailing
// slash; net/http's ServeMux records the method and the path). 0 for a route
// with no class.
func joinedRequestClass(r *http.Request) txcoord.RequestClass {
	pattern := r.Pattern
	if method, path, ok := strings.Cut(pattern, " "); ok && method == r.Method {
		pattern = path
	}
	return routeClasses[routeKey(r.Method, pattern)]
}

// routeKey is the key of a route in routeClasses: the method and the path
// pattern without a trailing slash, as chi records it.
func routeKey(method, pattern string) string {
	if pattern != "/" {
		pattern = strings.TrimSuffix(pattern, "/")
	}
	return method + " " + pattern
}
