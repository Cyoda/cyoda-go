package oidc

import (
	"context"
	"encoding/json"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

const topicOidcProviders = "oidc.providers"

// Field-length caps for broadcast envelopes. Values exceeding these limits
// are dropped to prevent amplification attacks: a malicious cluster peer
// could otherwise craft envelopes whose concatenated fields persist as
// megabyte-scale keys in singleflightDebouncer.inFlight for the duration of
// the dispatched work (audit finding C1).
const (
	maxBroadcastOpLen       = 50   // longest known op is "reload_all" (10 chars); 50 gives headroom
	maxBroadcastTenantIDLen = 36   // UUID canonical form is exactly 36 chars
	maxBroadcastURILen      = 2048 // doubled from wellKnownConfigUri spec cap of 1000 chars
)

// broadcastEnvelope is the wire format for cluster-wide OIDC provider events.
type broadcastEnvelope struct {
	Op       string `json:"op"` // "reload" | "invalidate" | "reload_all"
	TenantID string `json:"t,omitempty"`
	URI      string `json:"u,omitempty"`
}

// handleBroadcast is the registry's Subscribe callback. Runs on the
// broadcaster's receive goroutine — must be non-blocking and panic-safe.
//
// Panic safety has two layers:
//  1. The synchronous path (json.Unmarshal + switch dispatch) is guarded by
//     the recover() in the defer below.
//  2. Each goroutine spawned by singleflight.Dispatch is wrapped via
//     safeDispatch so that panics inside reloadOne / invalidateOne / ReloadAll
//     are also caught and counted rather than crashing the process.
func (r *Registry) handleBroadcast(payload []byte) {
	start := time.Now()
	defer func() {
		if rec := recover(); rec != nil {
			r.logger.Error("oidc broadcast handler panic",
				"pkg", "oidc", "panic", rec)
			r.metrics.IncBroadcastPanic()
		}
		r.metrics.ObserveBroadcastReceive(time.Since(start).Seconds())
	}()

	var env broadcastEnvelope
	if err := json.Unmarshal(payload, &env); err != nil {
		r.logger.Debug("oidc broadcast: malformed envelope", "pkg", "oidc", "error", err.Error())
		r.metrics.IncBroadcastDrop("malformed_envelope")
		return
	}

	// Drop envelopes with overlong fields to prevent singleflight-key amplification.
	// Log lengths only — never the strings themselves (audit finding C1).
	var dropReason string
	switch {
	case len(env.Op) > maxBroadcastOpLen:
		dropReason = "oversized_op"
	case len(env.TenantID) > maxBroadcastTenantIDLen:
		dropReason = "oversized_tenantid"
	case len(env.URI) > maxBroadcastURILen:
		dropReason = "oversized_uri"
	}
	if dropReason != "" {
		r.logger.Debug("oidc broadcast: envelope field length exceeded",
			"pkg", "oidc",
			"op_len", len(env.Op),
			"tenant_len", len(env.TenantID),
			"uri_len", len(env.URI),
		)
		r.metrics.IncBroadcastDrop(dropReason)
		return
	}

	switch env.Op {
	case "reload":
		r.singleflight.Dispatch(env.TenantID+":"+env.URI, r.safeDispatch(func() {
			r.reloadOne(context.Background(), spi.TenantID(env.TenantID), env.URI)
		}))
	case "invalidate":
		r.singleflight.Dispatch(env.TenantID+":"+env.URI, r.safeDispatch(func() {
			r.invalidateOne(spi.TenantID(env.TenantID), env.URI)
		}))
	case "reload_all":
		r.singleflight.Dispatch("_reload_all", r.safeDispatch(func() {
			_ = r.ReloadAll(context.Background())
		}))
	default:
		r.logger.Debug("oidc broadcast: unknown op", "pkg", "oidc", "op", env.Op)
	}
}

// safeDispatch wraps fn with a recover() so that panics inside goroutines
// spawned by singleflight.Dispatch are caught and counted rather than crashing
// the process. This is the second panic-safety layer required by the D7 spec.
func (r *Registry) safeDispatch(fn func()) func() {
	return func() {
		defer func() {
			if rec := recover(); rec != nil {
				r.logger.Error("oidc broadcast dispatch panic",
					"pkg", "oidc", "panic", rec)
				r.metrics.IncBroadcastPanic()
			}
		}()
		fn()
	}
}

// broadcastOp is invoked by the Service write paths. Fire-and-forget per D7.
func (r *Registry) broadcastOp(op, tenant, uri string) {
	if r.broadcast == nil {
		return
	}
	payload, err := json.Marshal(broadcastEnvelope{Op: op, TenantID: tenant, URI: uri})
	if err != nil {
		r.logger.Error("oidc: marshal broadcast envelope", "pkg", "oidc", "error", err.Error())
		return
	}
	r.broadcast.Broadcast(topicOidcProviders, payload)
}
