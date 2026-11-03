package dispatch

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/cyoda-platform/cyoda-go/internal/platformclock"
)

// ClockPath is the sealed peer route a clock setting is pushed to.
const ClockPath = "/internal/dispatch/clock"

// ClockHolder holds a pushed clock setting and answers with the setting the
// node then holds.
type ClockHolder interface {
	Hold(payload []byte) (platformclock.Setting, error)
}

// ClockPushAnswer is the sealed answer to a clock push: the setting the peer
// holds, or the reason it refused.
type ClockPushAnswer struct {
	Held    *platformclock.Setting `json:"held,omitempty"`
	Refused string                 `json:"refused,omitempty"`
}

// ClockHandler serves POST /internal/dispatch/clock under the same peer
// authentication as the callout route; the envelope binds the recipient node
// and this path. Every answer to a request that opened is sealed.
type ClockHandler struct {
	auth   PeerAuth
	holder ClockHolder
}

// NewClockHandler constructs a ClockHandler.
func NewClockHandler(auth PeerAuth, holder ClockHolder) *ClockHandler {
	return &ClockHandler{auth: auth, holder: holder}
}

// Register registers the clock route on mux.
func (h *ClockHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST "+ClockPath, h.handle)
}

func (h *ClockHandler) handle(w http.ResponseWriter, r *http.Request) {
	body, _, binding, err := h.auth.Verify(r)
	switch {
	case errors.Is(err, ErrReplayCacheFull):
		slog.Warn("clock push refused by the replay cache", "pkg", "dispatch", "remoteAddr", r.RemoteAddr, "reason", err)
		writeSealed(w, h.auth, binding, ClockPushAnswer{Refused: "the replay cache refused the request"})
		return
	case err != nil:
		slog.Warn("clock push auth failed", "pkg", "dispatch", "remoteAddr", r.RemoteAddr, "err", err)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	held, err := h.holder.Hold(body)
	if err != nil {
		slog.Warn("clock push refused", "pkg", "dispatch", "remoteAddr", r.RemoteAddr, "reason", err.Error(), "size", len(body))
		writeSealed(w, h.auth, binding, ClockPushAnswer{Refused: err.Error()})
		return
	}
	writeSealed(w, h.auth, binding, ClockPushAnswer{Held: &held})
}
