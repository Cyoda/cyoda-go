package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/adminroute"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/peeraddr"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// TxTokenHeader is the HTTP header carrying the transaction routing token.
const TxTokenHeader = "X-Tx-Token"

// HTTPRouting returns middleware that routes requests to the correct cluster
// node based on the transaction token. Requests without a token, with a token
// targeting the local node, or with a token that names no owner (a
// location-independent transaction, which any node serves) are served locally.
// Requests targeting a remote node are reverse-proxied transparently.
//
// A request that cannot reach the owner is answered 503
// TRANSACTION_NODE_UNAVAILABLE, retryable only with proof that nothing was
// sent: the owner is not live, the peer address guard refused its address, or
// the dial to it failed with no connection to it held for the request. Past
// that, the owner may have run the request, and a retry could repeat a write it
// kept.
//
// Model and workflow administration never runs inside a transaction: a request
// for one of those routes (adminroute) that carries a token is refused 400
// MODEL_ADMIN_IN_JOINED_TRANSACTION before the token is verified and before
// anything is forwarded, so the answer is the same on every node, whatever the
// token. This layer sits above the context-path strip, so it matches with
// contextPath applied (the configured context path with no trailing slash, ""
// when there is none).
//
// allowLoopback gates the peer-address SSRF guard on the proxy path, matching
// the same flag on the dispatch forwarder. Set true only in test fixtures that
// run cluster nodes on 127.0.0.1; keep false in production.
func HTTPRouting(signer *token.Signer, registry contract.NodeRegistry, selfNodeID string, proxyTimeout time.Duration, allowLoopback bool, contextPath string) func(http.Handler) http.Handler {
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
			if adminroute.IsAdministration(r, contextPath) {
				common.WriteError(w, r, common.ModelAdminInJoinedTransaction())
				return
			}

			claims, err := signer.Verify(tok)
			if err != nil {
				handleTokenError(w, r, err)
				return
			}

			// A pass that names no owner is one of a location-independent
			// transaction: any pnode serves it.
			if claims.NodeID == selfNodeID || claims.NodeID == "" {
				next.ServeHTTP(w, r)
				return
			}

			addr, alive, err := registry.Lookup(r.Context(), claims.NodeID)
			if err != nil {
				common.WriteError(w, r, common.Internal("registry lookup failed", fmt.Errorf("registry lookup of node %s: %w", claims.NodeID, err)))
				return
			}
			if !alive || addr == "" {
				// Nothing is sent to a node that is not live: retryable.
				common.WriteError(w, r, common.Operational(
					http.StatusServiceUnavailable,
					common.ErrCodeTransactionNodeUnavailable,
					"transaction node is not available",
				).AsRetryable().WithCause(fmt.Errorf("transaction node %s is not live", claims.NodeID)).WithLogLevel(slog.LevelWarn))
				return
			}

			proxyTo(w, r, addr, transport, allowLoopback)
		})
	}
}

func handleTokenError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, token.ErrTokenExpired) {
		common.WriteError(w, r, common.Operational(
			http.StatusGone,
			common.ErrCodeTransactionExpired,
			"transaction token has expired",
		))
		return
	}
	// Verify refuses a token as expired, tampered or invalid; the last two
	// answer alike.
	common.WriteError(w, r, common.Operational(
		http.StatusUnauthorized,
		common.ErrCodeUnauthorized,
		"invalid transaction token",
	))
}

func proxyTo(w http.ResponseWriter, r *http.Request, addr string, transport http.RoundTripper, allowLoopback bool) {
	// SSRF guard: validate the resolved peer address before building the reverse
	// proxy. Matches the same guard on the dispatch forwarder. On failure the
	// request is rejected with the retryable 503 TRANSACTION_NODE_UNAVAILABLE —
	// the node address is untrusted (same semantics as a dead node from the
	// client's view), and nothing was sent.
	// The lookup runs under the client's own request context, bounded: a client
	// that has gone must not leave a handler waiting on a resolver, and a client
	// that stays must not either — an inbound request carries no deadline of its
	// own, so peeraddr's own bound for a lookup that precedes a dial applies.
	lookupCtx, cancel := context.WithTimeout(r.Context(), peeraddr.LookupTimeout)
	defer cancel()
	if err := peeraddr.Validate(lookupCtx, addr, allowLoopback); err != nil {
		common.WriteError(w, r, common.Operational(
			http.StatusServiceUnavailable,
			common.ErrCodeTransactionNodeUnavailable,
			"transaction node address is not reachable",
		).AsRetryable().WithCause(fmt.Errorf("proxy target address %s rejected by SSRF guard: %w", addr, err)).WithLogLevel(slog.LevelWarn))
		return
	}

	if !strings.Contains(addr, "://") {
		addr = "http://" + addr
	}
	target, err := url.Parse(addr)
	if err != nil {
		common.WriteError(w, r, common.Internal("invalid proxy target", fmt.Errorf("proxy target URL %q: %w", addr, err)))
		return
	}

	// connected records whether the transport held a connection to the owner
	// for this request at any point. A dial error alone is not proof that
	// nothing was sent: the transport sends a replayable request again on a new
	// connection when a reused one breaks after the request was written, and
	// that second dial can fail after the owner received the first.
	var connected atomic.Bool
	r = r.WithContext(httptrace.WithClientTrace(r.Context(), &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) { connected.Store(true) },
	}))

	// ReverseProxy is created per request. It is lightweight (Director func +
	// shared Transport + ErrorHandler) with no per-instance state worth caching.
	// Caching was considered but rejected: an unbounded cache leaks memory as
	// nodes join/leave during rolling updates.
	rp := &httputil.ReverseProxy{
		Director:  makeProxyDirector(target),
		Transport: transport,
		ErrorHandler: func(rw http.ResponseWriter, req *http.Request, err error) {
			// One line, at the level of what happened: the caller going away
			// cancels the proxied request and is routine (DEBUG), not an
			// outage of the owner; an owner that could not be reached is an
			// ERROR.
			level, cause := slog.LevelError, fmt.Errorf("proxy request to %s failed: %w", addr, err)
			if req.Context().Err() != nil {
				level, cause = slog.LevelDebug, fmt.Errorf("client gone before the request proxied to %s completed: %w", addr, err)
			}
			appErr := common.Operational(
				http.StatusServiceUnavailable,
				common.ErrCodeTransactionNodeUnavailable,
				"transaction node is unreachable",
			)
			// Retryable only with proof that nothing was sent: the dial failed
			// and no connection to the owner was ever held for this request.
			// Any other failure leaves the outcome unknown.
			var opErr *net.OpError
			if errors.As(err, &opErr) && opErr.Op == "dial" && !connected.Load() {
				appErr.AsRetryable()
			}
			common.WriteError(rw, req, appErr.WithCause(cause).WithLogLevel(level))
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
