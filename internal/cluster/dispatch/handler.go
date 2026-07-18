package dispatch

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
)

// DispatchHandler serves the internal dispatch endpoints for processor and
// criteria execution. Authenticates each request via PeerAuth — today AEAD
// over a shared secret, tomorrow potentially mTLS — and annotates the
// request context with the authenticated PeerIdentity so downstream code
// can audit origin regardless of the transport.
type DispatchHandler struct {
	local contract.ExternalProcessingService
	auth  PeerAuth
}

// NewDispatchHandler constructs a DispatchHandler backed by the given local
// ExternalProcessingService and peer-authentication impl. Auth is already
// validated at construction time (NewAEADPeerAuth etc. check secret length),
// so this constructor does not return an error.
func NewDispatchHandler(local contract.ExternalProcessingService, auth PeerAuth) *DispatchHandler {
	return &DispatchHandler{local: local, auth: auth}
}

// Register registers the dispatch routes on the provided ServeMux.
func (h *DispatchHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /internal/dispatch/callout", h.handleCallout)
}

// handleCallout handles POST /internal/dispatch/callout. It dispatches to
// the local ExternalProcessingService method matching req.Kind and maps the
// result into the union DispatchCalloutResponse.
func (h *DispatchHandler) handleCallout(w http.ResponseWriter, r *http.Request) {
	body, identity, ok := h.verifyRequest(w, r)
	if !ok {
		return
	}

	var req DispatchCalloutRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	ctx := h.buildContext(r, identity, req.TenantID, req.UserID, req.Roles)
	ctx = internalgrpc.WithTxToken(ctx, req.TxToken)

	entity := &spi.Entity{
		Meta: req.EntityMeta,
		Data: []byte(req.Entity),
	}

	switch req.Kind {
	case "processor":
		var processor spi.ProcessorDefinition
		if req.Processor != nil {
			processor = *req.Processor
		}
		result, err := h.local.DispatchProcessor(ctx, entity, processor, req.WorkflowName, req.TransitionName, req.TxID)
		if err != nil {
			slog.Error("dispatch processor failed", "pkg", "dispatch", "err", err)
			writeJSON(w, http.StatusOK, DispatchCalloutResponse{
				Success: false,
				Error:   "dispatch processor failed",
			})
			return
		}
		writeJSON(w, http.StatusOK, DispatchCalloutResponse{
			Success:    true,
			EntityData: result.Data,
		})
	case "criteria":
		matches, reason, err := h.local.DispatchCriteria(ctx, entity, req.Criterion, req.Target, req.WorkflowName, req.TransitionName, req.ProcessorName, req.TxID)
		if err != nil {
			slog.Error("dispatch criteria failed", "pkg", "dispatch", "err", err)
			writeJSON(w, http.StatusOK, DispatchCalloutResponse{
				Success: false,
				Error:   "dispatch criteria failed",
			})
			return
		}
		writeJSON(w, http.StatusOK, DispatchCalloutResponse{
			Success: true,
			Matches: &matches,
			Reason:  reason,
		})
	case "function":
		var fn spi.ScheduleFunction
		if req.Function != nil {
			fn = *req.Function
		}
		result, err := h.local.DispatchFunction(ctx, entity, fn, req.WorkflowName, req.TransitionName, req.TxID)
		if err != nil {
			slog.Error("dispatch function failed", "pkg", "dispatch", "err", err)
			writeJSON(w, http.StatusOK, DispatchCalloutResponse{
				Success: false,
				Error:   "dispatch function failed",
			})
			return
		}
		writeJSON(w, http.StatusOK, DispatchCalloutResponse{
			Success:    true,
			Result:     result.Value,
			ResultKind: result.Kind,
		})
	default:
		http.Error(w, "unknown callout kind", http.StatusBadRequest)
	}
}

// verifyRequest runs peer authentication over the incoming request. On
// failure it writes 403 and returns ok=false; on success it returns the
// authenticated plaintext body and the peer's identity. Error messages
// are deliberately generic to avoid leaking which step failed.
func (h *DispatchHandler) verifyRequest(w http.ResponseWriter, r *http.Request) ([]byte, PeerIdentity, bool) {
	body, identity, err := h.auth.Verify(r)
	if err != nil {
		slog.Warn("dispatch request auth failed",
			"pkg", "dispatch",
			"remoteAddr", r.RemoteAddr,
			"err", err)
		http.Error(w, "forbidden", http.StatusForbidden)
		return nil, PeerIdentity{}, false
	}
	return body, identity, true
}

// buildContext constructs a context.Context carrying the UserContext from
// the dispatch request fields and the authenticated PeerIdentity. Even in
// the shared-key regime where PeerIdentity is degenerate, propagating it
// through context means downstream audit / tracing can read origin without
// being rewritten when transport evolves.
func (h *DispatchHandler) buildContext(r *http.Request, identity PeerIdentity, tenantID, userID string, roles []string) context.Context {
	uc := &spi.UserContext{
		UserID: userID,
		Tenant: spi.Tenant{
			ID: spi.TenantID(tenantID),
		},
		Roles: roles,
	}
	ctx := spi.WithUserContext(r.Context(), uc)
	ctx = WithPeerIdentity(ctx, identity)
	return ctx
}

// writeJSON encodes v as JSON and writes it with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("dispatch handler: failed to write JSON response", "pkg", "dispatch", "err", err)
	}
}
