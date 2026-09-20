package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
)

// DispatchHandler serves the internal dispatch endpoint (POST
// /internal/dispatch/callout) that a peer node forwards processor, criteria,
// and function callouts to. Authenticates each request via PeerAuth — today
// AEAD over a shared secret, tomorrow potentially mTLS — and annotates the
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
	body, identity, binding, ok := h.verifyRequest(w, r)
	if !ok {
		return
	}

	var req DispatchCalloutRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	// A dispatch request carries two tenants: TenantID, which becomes the
	// UserContext this callout runs as, and EntityMeta.TenantID, which is
	// handed to the local dispatcher as the entity's own. They must agree, or
	// the callout runs as one tenant over another's entity.
	//
	// The equality is unconditional, including for an absent
	// EntityMeta.TenantID. Every callout kind — processor, criteria and
	// function alike — is built from a live stored entity whose Meta.TenantID
	// is set at construction and carried forward on update, so the field is
	// never empty on the real wire; an empty one can only come from a
	// hand-crafted peer body, and exempting it would be exempting exactly the
	// caller this check exists for.
	//
	// The response names neither value: both are peer-supplied.
	if string(req.EntityMeta.TenantID) != req.TenantID {
		http.Error(w, "entity tenant does not match request tenant", http.StatusBadRequest)
		return
	}

	ctx := h.buildContext(r, identity, req.TenantID, req.UserID, req.PrincipalKind, req.Roles)
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
			h.writeSealed(w, binding, dispatchErrorResponse("dispatch processor failed", err))
			return
		}
		h.writeSealed(w, binding, DispatchCalloutResponse{
			Success:    true,
			EntityData: result.Data,
		})
	case "criteria":
		matches, reason, err := h.local.DispatchCriteria(ctx, entity, req.Criterion, req.Target, req.WorkflowName, req.TransitionName, req.ProcessorName, req.TxID)
		if err != nil {
			slog.Error("dispatch criteria failed", "pkg", "dispatch", "err", err)
			h.writeSealed(w, binding, dispatchErrorResponse("dispatch criteria failed", err))
			return
		}
		h.writeSealed(w, binding, DispatchCalloutResponse{
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
			h.writeSealed(w, binding, dispatchErrorResponse("dispatch function failed", err))
			return
		}
		h.writeSealed(w, binding, DispatchCalloutResponse{
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
// authenticated plaintext body, the peer's identity and the binding the
// answer to this request is sealed under. Error messages are deliberately
// generic to avoid leaking which step failed.
func (h *DispatchHandler) verifyRequest(w http.ResponseWriter, r *http.Request) ([]byte, PeerIdentity, ResponseBinding, bool) {
	body, identity, binding, err := h.auth.Verify(r)
	if err != nil {
		slog.Warn("dispatch request auth failed",
			"pkg", "dispatch",
			"remoteAddr", r.RemoteAddr,
			"err", err)
		http.Error(w, "forbidden", http.StatusForbidden)
		return nil, PeerIdentity{}, ResponseBinding{}, false
	}
	return body, identity, binding, true
}

// buildContext constructs a context.Context carrying the UserContext from
// the dispatch request fields and the authenticated PeerIdentity. Even in
// the shared-key regime where PeerIdentity is degenerate, propagating it
// through context means downstream audit / tracing can read origin without
// being rewritten when transport evolves.
//
// principalKind is forwarded verbatim from the originating node's
// DispatchCalloutRequest.PrincipalKind so the peer's local dispatch — which
// calls AttachAuthContext just like single-node dispatch — reconstructs the
// SAME faithful auth context the originating node had, rather than an
// unset Kind that would fail the dispatch closed (see
// internal/grpc/cloudevent.go).
func (h *DispatchHandler) buildContext(r *http.Request, identity PeerIdentity, tenantID, userID string, principalKind spi.PrincipalKind, roles []string) context.Context {
	uc := &spi.UserContext{
		UserID: userID,
		Kind:   principalKind,
		Tenant: spi.Tenant{
			ID: spi.TenantID(tenantID),
		},
		Roles: roles,
	}
	ctx := spi.WithUserContext(r.Context(), uc)
	ctx = WithPeerIdentity(ctx, identity)
	return ctx
}

// dispatchErrorResponse builds the client-facing DispatchCalloutResponse for
// a failed local dispatch (processor/criteria/function). genericMsg is the
// sanitized, kind-specific message that goes in Error — never internal
// detail from err (matches the existing sanitization the surrounding tests
// enforce). The ErrorCode/ErrorStatus/ErrorRetryable trio classifies err
// using the same taxonomy single-node dispatch uses, so a forwarding node
// can re-mint the identical *common.AppError instead of collapsing every
// peer failure into a generic 400 (see B1, scheduled-transition-function
// final review):
//   - An *common.AppError (matched via errors.As, so it's found even
//     several layers deep behind %w-wrapping) contributes its own
//     Code/Status/Retryable.
//   - contract.ErrNoMatchingMember (the peer lost its matching calculation
//     member between gossip advertisement and forward) maps to the same
//     NO_COMPUTE_MEMBER_FOR_TAG/503/retryable trio single-node dispatch
//     uses for the equivalent condition.
//   - Anything else leaves the trio zero-valued; the forwarding node falls
//     back to its historical plain-error behavior.
func dispatchErrorResponse(genericMsg string, err error) DispatchCalloutResponse {
	resp := DispatchCalloutResponse{Success: false, Error: genericMsg}

	var appErr *common.AppError
	if errors.As(err, &appErr) {
		resp.ErrorCode = appErr.Code
		resp.ErrorStatus = appErr.Status
		resp.ErrorRetryable = appErr.Retryable
		return resp
	}
	if errors.Is(err, contract.ErrNoMatchingMember) {
		resp.ErrorCode = common.ErrCodeNoComputeMemberForTag
		resp.ErrorStatus = http.StatusServiceUnavailable
		resp.ErrorRetryable = true
	}
	return resp
}

// writeSealed answers the request binding names, under seal. The owner trusts
// nothing else: a status line or a body it cannot open tells it only that the
// answer was lost.
func (h *DispatchHandler) writeSealed(w http.ResponseWriter, binding ResponseBinding, v DispatchCalloutResponse) {
	plain, err := json.Marshal(v)
	if err != nil {
		slog.Error("failed to marshal dispatch answer", "pkg", "dispatch", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	wire, err := h.auth.SealResponse(w.Header(), binding, plain)
	if err != nil {
		slog.Error("failed to seal dispatch answer", "pkg", "dispatch", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(wire); err != nil {
		slog.Warn("failed to write dispatch answer", "pkg", "dispatch", "err", err)
	}
}
