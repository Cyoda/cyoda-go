package middleware

import (
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"sync/atomic"

	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// Recovery converts a handler panic into a sanitized 500 carrying a ticket
// UUID (full value and stack logged server-side under the same ticket) and
// latches healthFlag false. Nothing resets the flag: GET /health and the
// admin /readyz both report 503 from then on, so the node is removed from
// its Service endpoints and stops receiving traffic. It is not restarted —
// /livez is unconditional, deliberately, so a deterministic panic (a poisoned
// entity, a bad workflow definition) does not become a restart loop.
func Recovery(healthFlag *atomic.Bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					stack := string(debug.Stack())
					err := fmt.Errorf("panic: %v", rec)
					slog.Error("panic recovered", "pkg", "middleware", "err", err, "stack", stack)
					appErr := common.Fatal("internal server error", err)
					appErr.Detail = "panic recovered; check server logs for details"
					healthFlag.Store(false)
					common.WriteError(w, r, appErr)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}
