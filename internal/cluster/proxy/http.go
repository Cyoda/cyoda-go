package proxy

import (
	"errors"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/cluster/peeraddr"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// TxTokenHeader is the HTTP header carrying the transaction routing token.
const TxTokenHeader = "X-Tx-Token"

// HTTPRouting returns middleware that routes requests to the correct cluster
// node based on the transaction token. Requests without a token, or with a
// token targeting the local node, are served locally. Requests targeting a
// remote node are reverse-proxied transparently.
//
// allowLoopback gates the peer-address SSRF guard on the proxy path, matching
// the same flag on the dispatch forwarder. Set true only in test fixtures that
// run cluster nodes on 127.0.0.1; keep false in production.
func HTTPRouting(signer *token.Signer, registry contract.NodeRegistry, selfNodeID string, proxyTimeout time.Duration, allowLoopback bool) func(http.Handler) http.Handler {
	// Shared transport reused across all proxied requests.
	transport := &http.Transport{
		ResponseHeaderTimeout: proxyTimeout,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tok := r.Header.Get(TxTokenHeader)
			if tok == "" {
				next.ServeHTTP(w, r)
				return
			}

			claims, err := signer.Verify(tok)
			if err != nil {
				handleTokenError(w, r, err)
				return
			}

			if claims.NodeID == selfNodeID {
				next.ServeHTTP(w, r)
				return
			}

			addr, alive, err := registry.Lookup(r.Context(), claims.NodeID)
			if err != nil {
				slog.Error("registry lookup failed",
					"pkg", "proxy",
					"nodeID", claims.NodeID,
					"error", err,
				)
				common.WriteError(w, r, common.Internal("registry lookup failed", err))
				return
			}
			if !alive || addr == "" {
				slog.Warn("transaction node unavailable",
					"pkg", "proxy",
					"nodeID", claims.NodeID,
				)
				common.WriteError(w, r, common.Operational(
					http.StatusServiceUnavailable,
					common.ErrCodeTransactionNodeUnavailable,
					"transaction node is not available",
				))
				return
			}

			proxyTo(w, r, addr, transport, allowLoopback)
		})
	}
}

func handleTokenError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, token.ErrTokenExpired):
		common.WriteError(w, r, common.Operational(
			http.StatusGone,
			common.ErrCodeTransactionExpired,
			"transaction token has expired",
		))
	case errors.Is(err, token.ErrTokenTampered), errors.Is(err, token.ErrTokenInvalid):
		common.WriteError(w, r, common.Operational(
			http.StatusUnauthorized,
			common.ErrCodeUnauthorized,
			"invalid transaction token",
		))
	default:
		common.WriteError(w, r, common.Operational(
			http.StatusUnauthorized,
			common.ErrCodeUnauthorized,
			"transaction token verification failed",
		))
	}
}

func proxyTo(w http.ResponseWriter, r *http.Request, addr string, transport http.RoundTripper, allowLoopback bool) {
	// SSRF guard: validate the resolved peer address before building the reverse
	// proxy. Matches the same guard on the dispatch forwarder. On failure the
	// request is rejected with 503 TRANSACTION_NODE_UNAVAILABLE — the node
	// address is untrusted (same semantics as a dead node from the client's view).
	if err := peeraddr.Validate(addr, allowLoopback); err != nil {
		slog.Warn("proxy target address rejected by SSRF guard",
			"pkg", "proxy",
			"addr", addr,
			"error", err,
		)
		common.WriteError(w, r, common.Operational(
			http.StatusServiceUnavailable,
			common.ErrCodeTransactionNodeUnavailable,
			"transaction node address is not reachable",
		))
		return
	}

	if !strings.Contains(addr, "://") {
		addr = "http://" + addr
	}
	target, err := url.Parse(addr)
	if err != nil {
		slog.Error("invalid proxy target URL",
			"pkg", "proxy",
			"addr", addr,
			"error", err,
		)
		common.WriteError(w, r, common.Internal("invalid proxy target", err))
		return
	}

	// ReverseProxy is created per request. It is lightweight (Director func +
	// shared Transport + ErrorHandler) with no per-instance state worth caching.
	// Caching was considered but rejected: an unbounded cache leaks memory as
	// nodes join/leave during rolling updates.
	rp := &httputil.ReverseProxy{
		Director:  makeProxyDirector(target),
		Transport: transport,
		ErrorHandler: func(rw http.ResponseWriter, req *http.Request, err error) {
			slog.Error("proxy request failed",
				"pkg", "proxy",
				"target", addr,
				"error", err,
			)
			common.WriteError(rw, req, common.Operational(
				http.StatusServiceUnavailable,
				common.ErrCodeTransactionNodeUnavailable,
				"transaction node is unreachable",
			))
		},
	}

	rp.ServeHTTP(w, r)
}

// makeProxyDirector returns the Director that the per-request
// httputil.ReverseProxy uses to rewrite an outbound peer-to-peer
// request. It rewrites the URL onto the target node and strips any
// CORS-related request headers — those are the responsibility of the
// outermost CORS middleware on the receiving node, not the destination
// peer. See docs/superpowers/specs/2026-05-01-issue-196-cors-design.md §"Cluster proxy interaction".
func makeProxyDirector(target *url.URL) func(*http.Request) {
	return func(req *http.Request) {
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		req.Host = target.Host
		// Strip CORS request headers — see spec §"Cluster proxy
		// interaction". Owned by the outermost CORS middleware.
		req.Header.Del("Origin")
		req.Header.Del("Access-Control-Request-Method")
		req.Header.Del("Access-Control-Request-Headers")
	}
}
