package middleware

import (
	"net/http"
	"net/url"
	"strings"
)

// Static preflight-response values per spec.
const (
	corsAllowMethods = "GET, POST, PUT, PATCH, DELETE, OPTIONS"
	corsAllowHeaders = "Authorization, Content-Type, traceparent, tracestate"
	corsMaxAge       = "86400"
	internalPrefix   = "/_internal/"
)

// loopbackHosts is the set of host strings (post-Hostname()-extraction,
// post-lowercase) that the default loopback mode allows. String compare
// only — no IP-equivalence, so non-canonical forms like 127.000.000.001
// or 0:0:0:0:0:0:0:1 do NOT match.
var loopbackHosts = map[string]struct{}{
	"localhost": {},
	"127.0.0.1": {},
	"::1":       {},
}

// CORSPolicy holds the validated, resolved CORS state derived from
// CYODA_CORS_ENABLED and CYODA_CORS_ALLOWED_ORIGINS. Build once at
// startup via NewCORSPolicy. The middleware closure captures the policy
// pointer; per-request work is allocation-free apart from header writes.
type CORSPolicy struct {
	enabled  bool
	wildcard bool
	// allowed is non-nil iff !wildcard && len(allowedOrigins) > 0
	// (allowlist mode). Nil in loopback mode and disabled mode.
	allowed map[string]struct{}
}

// NewCORSPolicy builds a CORSPolicy. Inputs must already have been
// validated by app.ValidateCORS — this constructor performs no
// validation and assumes the caller is the app startup path.
func NewCORSPolicy(enabled, wildcard bool, allowedOrigins []string) *CORSPolicy {
	p := &CORSPolicy{enabled: enabled, wildcard: wildcard}
	if !wildcard && len(allowedOrigins) > 0 {
		p.allowed = make(map[string]struct{}, len(allowedOrigins))
		for _, o := range allowedOrigins {
			p.allowed[o] = struct{}{}
		}
	}
	return p
}

// CORS returns the CORS middleware constructor. When the policy is
// disabled, returns an identity wrapper (no-op).
func CORS(p *CORSPolicy) func(http.Handler) http.Handler {
	if !p.enabled {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// /_internal/* is excluded from CORS entirely — no headers, not
			// even Vary: Origin. Defence-in-depth alongside the cluster
			// proxy stripping Origin on outbound peer-to-peer requests.
			if strings.HasPrefix(r.URL.Path, internalPrefix) {
				next.ServeHTTP(w, r)
				return
			}

			// Vary: Origin always — even on no-Origin pass-through and
			// even in wildcard mode. Closes the cache-poisoning vector
			// where a CDN serves a no-Origin response (without Vary) to
			// a later Origin-bearing request whose policy has changed.
			w.Header().Add("Vary", "Origin")

			origin := r.Header.Get("Origin")
			allowed := p.match(origin)

			isPreflight := r.Method == http.MethodOptions &&
				origin != "" &&
				r.Header.Get("Access-Control-Request-Method") != ""

			if isPreflight {
				if allowed != "" {
					w.Header().Set("Access-Control-Allow-Origin", allowed)
				}
				w.Header().Set("Access-Control-Allow-Methods", corsAllowMethods)
				w.Header().Set("Access-Control-Allow-Headers", corsAllowHeaders)
				w.Header().Set("Access-Control-Max-Age", corsMaxAge)
				w.WriteHeader(http.StatusNoContent)
				return
			}

			if origin != "" && allowed != "" {
				w.Header().Set("Access-Control-Allow-Origin", allowed)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// match returns the value to emit in Access-Control-Allow-Origin, or
// "" if the origin is not allowed.
func (p *CORSPolicy) match(origin string) string {
	if p.wildcard {
		return "*"
	}
	if origin == "" {
		return ""
	}
	if p.allowed != nil {
		if _, ok := p.allowed[origin]; ok {
			return origin
		}
		return ""
	}
	// Loopback mode.
	if matchLoopback(origin) {
		return origin
	}
	return ""
}

// matchLoopback implements the spec's "Loopback matching contract":
// parse with net/url, exact (lowercase) host string-equal to one of
// the three loopback names, scheme http or https, no userinfo/path/
// query/fragment, any port permitted.
func matchLoopback(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	scheme := u.Scheme
	if scheme != strings.ToLower(scheme) {
		return false
	}
	if scheme != "http" && scheme != "https" {
		return false
	}
	host := u.Hostname()
	if host != strings.ToLower(host) {
		return false
	}
	if _, ok := loopbackHosts[host]; !ok {
		return false
	}
	if u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	return true
}
