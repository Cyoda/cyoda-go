package dispatch

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/cyoda-platform/cyoda-go/internal/clusterstatus"
)

// StatusPath is the sealed peer route a node's own status is asked on.
const StatusPath = "/internal/dispatch/status"

// StatusSource produces this node's own status.
type StatusSource interface {
	Status(ctx context.Context) clusterstatus.NodeStatus
}

// StatusAnswer is the sealed answer to a status ask: the node's status, or the
// reason it refused.
type StatusAnswer struct {
	Status  *clusterstatus.NodeStatus `json:"status,omitempty"`
	Refused string                    `json:"refused,omitempty"`
}

// StatusHandler serves POST /internal/dispatch/status under the same peer
// authentication as the callout route; the envelope binds the recipient node
// and this path. Every answer to a request that opened is sealed.
type StatusHandler struct {
	auth PeerAuth
	src  StatusSource
}

// NewStatusHandler constructs a StatusHandler.
func NewStatusHandler(auth PeerAuth, src StatusSource) *StatusHandler {
	return &StatusHandler{auth: auth, src: src}
}

// Register registers the status route on mux.
func (h *StatusHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST "+StatusPath, h.handle)
}

func (h *StatusHandler) handle(w http.ResponseWriter, r *http.Request) {
	_, _, binding, err := h.auth.Verify(r)
	switch {
	case errors.Is(err, ErrReplayCacheFull):
		slog.Warn("status ask refused by the replay cache", "pkg", "dispatch", "remoteAddr", r.RemoteAddr, "reason", err)
		writeSealed(w, h.auth, binding, StatusAnswer{Refused: "the replay cache refused the request"})
		return
	case err != nil:
		slog.Warn("status ask auth failed", "pkg", "dispatch", "remoteAddr", r.RemoteAddr, "err", err)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	s := h.src.Status(r.Context())
	writeSealed(w, h.auth, binding, StatusAnswer{Status: &s})
}
