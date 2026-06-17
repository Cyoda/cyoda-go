package oidc

import (
	"context"
	"encoding/json"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

const topicOidcProviders = "oidc.providers"

// broadcastEnvelope is the wire format for cluster-wide OIDC provider events.
type broadcastEnvelope struct {
	Op       string `json:"op"`           // "reload" | "invalidate" | "reload_all"
	TenantID string `json:"t,omitempty"`
	URI      string `json:"u,omitempty"`
}

// handleBroadcast is the registry's Subscribe callback. Runs on the
// broadcaster's receive goroutine — must be non-blocking and panic-safe.
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
		return
	}

	switch env.Op {
	case "reload":
		r.singleflight.Dispatch(env.TenantID+":"+env.URI, func() {
			r.reloadOne(context.Background(), spi.TenantID(env.TenantID), env.URI)
		})
	case "invalidate":
		r.singleflight.Dispatch(env.TenantID+":"+env.URI, func() {
			r.invalidateOne(spi.TenantID(env.TenantID), env.URI)
		})
	case "reload_all":
		r.singleflight.Dispatch("_reload_all", func() {
			_ = r.ReloadAll(context.Background())
		})
	default:
		r.logger.Debug("oidc broadcast: unknown op", "pkg", "oidc", "op", env.Op)
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
