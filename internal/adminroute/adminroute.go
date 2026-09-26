// Package adminroute names the HTTP routes that administer a model or its
// workflows. Model and workflow administration never runs inside a
// transaction, and two layers refuse a request for one of these routes that
// carries a transaction token: the cluster routing layer, before it verifies
// the token or forwards the request to the transaction's owner, and the join
// layer, before it verifies the token or takes the transaction's lock. Both ask
// this one predicate, so the list is written once.
package adminroute

import (
	"net/http"
	"net/url"
	pathpkg "path"
	"strings"
)

// routes matches every state-changing model and workflow administration
// route: import, delete, change level, lock, unlock, unique keys and workflow
// import. The read-only model and workflow routes — list, export, validate,
// workflow export — are not administration and are absent. The patterns are
// the API document's paths without the context path.
var routes = func() *http.ServeMux {
	mux := http.NewServeMux()
	for _, route := range []string{
		"POST /model/import/{dataFormat}/{converter}/{entityName}/{modelVersion}",
		"DELETE /model/{entityName}/{modelVersion}",
		"POST /model/{entityName}/{modelVersion}/changeLevel/{changeLevel}",
		"PUT /model/{entityName}/{modelVersion}/lock",
		"PUT /model/{entityName}/{modelVersion}/unlock",
		"PUT /model/{entityName}/{modelVersion}/unique-keys",
		"POST /model/{entityName}/{modelVersion}/workflow/import",
	} {
		mux.Handle(route, http.NotFoundHandler())
	}
	return mux
}()

// IsAdministration reports whether r asks for model or workflow
// administration. contextPath is the prefix the request path still carries at
// the asking layer, with no trailing slash ("/api" above the strip, "" below
// it); a path outside it is not an API request and is not administration.
//
// A path in non-canonical form — an empty segment (/model//1/lock), a dot
// segment (/model/x/1/../1/lock) — is not administration. The API router (chi)
// would route such a path, but it never sees one: the ServeMux mounted above
// it answers a non-canonical path with a redirect to the canonical one, and
// the redirected request is what gets asked again. This predicate depends on
// that mount and says so here; ServeMux.Handler alone would not do, since it
// reports the pattern the CLEANED path matches together with its redirect. A
// method no route registers is an empty pattern.
func IsAdministration(r *http.Request, contextPath string) bool {
	path := r.URL.Path
	if contextPath != "" {
		if !strings.HasPrefix(path, contextPath+"/") {
			return false
		}
		path = strings.TrimPrefix(path, contextPath)
	}
	if cleanPath(path) != path {
		return false
	}
	probe := &http.Request{Method: r.Method, Host: r.Host, URL: &url.URL{Path: path}}
	_, pattern := routes.Handler(probe)
	return pattern != ""
}

// cleanPath is the canonical form net/http's ServeMux redirects to: path.Clean
// with a trailing slash kept.
func cleanPath(p string) string {
	if p == "" {
		return "/"
	}
	cleaned := pathpkg.Clean(p)
	if strings.HasSuffix(p, "/") && cleaned != "/" {
		cleaned += "/"
	}
	return cleaned
}
