package api

import (
	"net/http"

	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/health"
)

// RegisterHealthRoutes registers GET /health on the API listener.
// /health mirrors the node's readiness flag: 200 {"status":"UP"} while
// healthy, 503 {"status":"DOWN"} after a panic recovered in code doing
// engine or store work on the application's behalf — the API door, gRPC, or
// a background loop. The flag latches: nothing re-arms it, because the
// node's state after such a panic is unverified. Read the ticket in the log,
// then replace the node. A panic on the admin listener's own probes and
// scrapes is contained the same way but does not latch.
//
// This is not the deployment probe. Deployment probes are /livez
// (unconditional) and /readyz (same flag) on the admin listener; /health is
// for humans and simple scripts.
func RegisterHealthRoutes(mux *http.ServeMux, healthFlag *health.Latch) {
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		if healthFlag.Tripped() {
			common.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "DOWN"})
			return
		}
		common.WriteJSON(w, http.StatusOK, map[string]string{"status": "UP"})
	})
}
