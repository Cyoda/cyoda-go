package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
)

// LocalRunner is the local procedure: the callout tried on this pnode's own
// cnodes. It is everything a pnode that receives a hand-over may do with one —
// the handler holds nothing through which it could hand the callout on.
type LocalRunner interface {
	RunLocal(ctx context.Context, call internalgrpc.Callout, maxTries int) internalgrpc.LocalResult
}

// DispatchHandler serves POST /internal/dispatch/callout: a callout handed over
// by the pnode that owns its transaction. Requests are authenticated via
// PeerAuth and every answer is sealed for its request; the owner believes
// nothing else.
//
// How many tries the hand-over may make and how long a cnode is given to answer
// are the OWNER's decisions, and the receiving pnode runs them as sent. It does
// not hold them against its own settings: the owner resolves the answer limit
// precisely so that the two pnodes cannot disagree about it, and refusing a
// serviceable callout because the two nodes' configurations differ — as they do
// through any rolling change — would fail it for no reason the caller can act
// on. What the receiver does check are the values the callout cannot run
// without at all, which need no configuration to judge; they are in validate.
type DispatchHandler struct {
	local LocalRunner
	auth  PeerAuth
}

// NewDispatchHandler constructs a DispatchHandler over the local procedure and
// the peer-authentication impl. Auth is already validated at construction time
// (NewAEADPeerAuth etc. check secret length), so this constructor returns no
// error.
func NewDispatchHandler(local LocalRunner, auth PeerAuth) *DispatchHandler {
	return &DispatchHandler{local: local, auth: auth}
}

// Register registers the dispatch routes on the provided ServeMux.
func (h *DispatchHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /internal/dispatch/callout", h.handleCallout)
}

// handleCallout runs the local procedure over a hand-over and answers what
// became of it. Every answer to a request that opened is sealed for that
// request: a bare status would reach the owner as a lost answer, which for a
// callout that is not repeat-safe fails the operation.
func (h *DispatchHandler) handleCallout(w http.ResponseWriter, r *http.Request) {
	body, identity, binding, err := h.auth.Verify(r)
	switch {
	case errors.Is(err, ErrReplayCacheFull):
		// Opened and authenticated, then refused by the replay cache's
		// capacity: nothing was handed to a cnode, and the owner can be told so
		// under seal. A bare status would read as a lost answer and fail an
		// operation that is not repeat-safe — which a saturated cache must not
		// do. A replayed nonce is a different matter: it gets the bare 403
		// below, because there is no request to bind that answer to but the one
		// the replay copies.
		slog.Warn("hand-over refused: the replay cache is full", "pkg", "dispatch", "remoteAddr", r.RemoteAddr)
		h.writeSealed(w, binding, refusal(noCnodeFailure()))
		return
	case err != nil:
		slog.Warn("dispatch request auth failed", "pkg", "dispatch", "remoteAddr", r.RemoteAddr, "err", err)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	var req DispatchCalloutRequest
	if err := json.Unmarshal(body, &req); err != nil {
		h.refuse(w, binding, fmt.Errorf("failed to parse hand-over: %w", err))
		return
	}
	if err := req.validate(); err != nil {
		h.refuse(w, binding, fmt.Errorf("failed to validate hand-over: %w", err))
		return
	}
	call, failure := req.toCallout()
	if failure != nil {
		// Through the same helper as the checks above, so that the owner renders
		// every refusal of a hand-over it made identically.
		h.refuse(w, binding, fmt.Errorf("failed to build the callout from the hand-over: %w", failure))
		return
	}

	ctx := common.WithDiagnostics(h.buildContext(r, identity, req.TenantID, req.UserID, req.PrincipalKind, req.Roles))
	res := h.local.RunLocal(ctx, call, req.TriesLeft)

	diag := common.GetDiagnostics(ctx)
	resp := responseFromLocal(call, res, diag.GetWarnings(), diag.GetErrors())
	slog.Debug("hand-over answered", "pkg", "dispatch", "kind", req.Kind, "requestId", req.RequestID,
		"owner", req.OwnerNodeID, "outcome", resp.Outcome, "triesUsed", res.TriesUsed)
	h.writeSealed(w, binding, resp)
}

// refuse answers a hand-over that was authenticated but cannot be run. It would
// be refused identically by every pnode: terminal, with no try made. The reason
// is logged here; the answer carries none of it, since the body is peer-supplied.
func (h *DispatchHandler) refuse(w http.ResponseWriter, binding ResponseBinding, err error) {
	slog.Error("hand-over refused", "pkg", "dispatch", "err", err)
	appErr := common.Internal("the hand-over was refused", err)
	h.writeSealed(w, binding, refusal(&contract.CalloutFailure{Kind: contract.Terminal, Code: appErr.Code, Message: appErr.Message, Err: appErr}))
}

// buildContext constructs the context.Context the callout runs under: the
// UserContext the hand-over named, and the authenticated PeerIdentity. Even in
// the shared-key regime where PeerIdentity is degenerate, propagating it
// through context means downstream audit / tracing can read origin without
// being rewritten when transport evolves.
//
// The tenant is the wire's, named by its id alone: a peer is authenticated by
// the cluster-wide key and its identity carries no tenant, so nothing about the
// tenant may be filled in here that the peer did not send. validate has already
// held the entity's own tenant to the same value.
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
	if len(wire) > MaxEnvelopeSize {
		// The owner reads at most the ceiling, so writing these bytes would
		// send it a truncated answer while this node recorded that it had
		// answered. A status instead: the owner reads the lost answer it is,
		// and an operator is told which callout produced an answer too large.
		slog.Error("the answer to a hand-over does not fit the envelope and was not sent",
			"pkg", "dispatch", "outcome", v.Outcome, "sealedBytes", len(wire), "maxBytes", MaxEnvelopeSize)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(wire); err != nil {
		slog.Warn("failed to write dispatch answer", "pkg", "dispatch", "err", err)
	}
}
