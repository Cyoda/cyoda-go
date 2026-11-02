package middleware

import (
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/health"
)

// Recovery converts a handler panic into a sanitized 500 carrying a ticket
// UUID (full value and stack logged server-side under the same ticket) and
// latches healthFlag false. Nothing resets the flag: GET /health and the
// admin /readyz both report 503 from then on, so the pod leaves its Service
// endpoints and new client connections stop reaching it.
//
// That is the extent of it. Peer-forwarded work continues — the chart always
// runs cluster mode and peers route through the gossip registry, not the
// Service — and established connections are not closed. The node is also not
// restarted: /livez is unconditional, deliberately, so a deterministic panic
// (a poisoned entity, a bad workflow definition) does not become a restart
// loop. Replacing the node is an operator action.
//
// healthFlag is optional. Passing nil keeps the containment — ticket, log,
// sanitized 500 — and skips the latch, which is how a surface that does no
// engine or store work on the application's behalf opts out: the admin
// listener, whose /livez writes a constant, whose /readyz reads two flags,
// and whose /metrics gathers registered collectors. A panic there says
// nothing about whether this node's entity state is still correct, so
// latching would take a healthy node out of service over a broken probe.
// Same criterion as internal/grpc/recovery.go's per-member goroutines.
func Recovery(healthFlag *health.Latch) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					// net/http's own abort sentinel: a handler (ReverseProxy on a
					// client hang-up mid-body, for one) panics with it to end the
					// response silently. Re-raise so the server handles it as
					// designed; it is not a defect and must not latch the node.
					if rec == http.ErrAbortHandler {
						panic(rec)
					}
					stack := string(debug.Stack())
					err := fmt.Errorf("panic: %v", rec)
					// Minted here, not left to WriteError, so the ONLY line
					// carrying the panic value and stack is the one an operator
					// reaches from the client-visible ticket. The Detail
					// overwrite below is what keeps the panic value out of a
					// verbose-mode response, and it also strips the FATAL line
					// of anything worth finding — so without this the ticket
					// names a line that says nothing. Matches
					// internal/grpc/recovery.go, which already logs the two
					// together.
					ticket := uuid.New().String()
					slog.Error("panic recovered", "pkg", "middleware",
						"ticket", ticket, "err", err, "stack", stack)
					appErr := common.Fatal("internal server error", err).WithTicket(ticket)
					appErr.Detail = "panic recovered; check server logs for details"
					healthFlag.Trip()
					common.WriteError(w, r, appErr)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}
