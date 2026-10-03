package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/logging"
	"github.com/cyoda-platform/cyoda-go/internal/observability"
)

// maxAdminBodyBytes bounds the two admin POST bodies, matching the 1 MiB
// bound internal/domain/account uses for its POST bodies.
const maxAdminBodyBytes = 1 << 20

// acceptedLogLevels is the LookupLevel-recognised set, in the order the
// 400 response names them. "warning" is an alias of "warn".
var acceptedLogLevels = []string{"debug", "info", "warn", "warning", "error"}

// AdminHandlers serves the node's runtime controls: log level and trace
// sampler. They change process-wide state, so only a platform operator may
// call them.
type AdminHandlers struct {
	operator auth.OperatorGuard
}

// NewAdminHandlers returns the runtime-control handlers behind operator.
func NewAdminHandlers(operator auth.OperatorGuard) *AdminHandlers {
	return &AdminHandlers{operator: operator}
}

// GetLogLevel returns the current log level as JSON.
// Requires a platform operator.
func (a *AdminHandlers) GetLogLevel(w http.ResponseWriter, r *http.Request) {
	if !a.operator.Require(w, r) {
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]string{
		"level": logging.LevelString(logging.Level.Level()),
	}); err != nil {
		slog.Debug("failed to encode response", "error", err)
	}
}

// GetTraceSampler returns the current OTel trace sampler configuration.
// Requires a platform operator. The response is round-trippable via POST.
func (a *AdminHandlers) GetTraceSampler(w http.ResponseWriter, r *http.Request) {
	if !a.operator.Require(w, r) {
		return
	}

	cfg := observability.Sampler.Config()

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(cfg); err != nil {
		slog.Debug("failed to encode response", "error", err)
	}
}

// SetTraceSampler changes the runtime OTel trace sampler configuration.
// Requires a platform operator. Returns the new configuration on success.
//
// Note: when parent_based is true (the default), upstream traceparent
// sampling decisions are honored — "sampler: always" does NOT force 100%
// capture of all spans if upstream has already decided "do not sample".
// Set parent_based: false to override upstream decisions locally.
func (a *AdminHandlers) SetTraceSampler(w http.ResponseWriter, r *http.Request) {
	if !a.operator.Require(w, r) {
		return
	}

	// Pointer fields distinguish omitted from explicit false/zero.
	var req struct {
		Sampler     string   `json:"sampler"`
		Ratio       *float64 `json:"ratio,omitempty"`
		ParentBased *bool    `json:"parent_based,omitempty"`
	}
	if err := common.DecodeBoundedJSON(w, r, maxAdminBodyBytes, &req); err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid request body"))
		return
	}

	// Build the canonical SamplerConfig.
	// - parent_based defaults to true when the field is omitted.
	// - ratio is only allowed when sampler == "ratio".
	pb := true
	if req.ParentBased != nil {
		pb = *req.ParentBased
	}

	cfg := observability.SamplerConfig{
		Sampler:     req.Sampler,
		ParentBased: pb,
	}
	if req.Ratio != nil {
		cfg.Ratio = *req.Ratio
	}

	previous := observability.Sampler.Config()
	if err := observability.Sampler.SetSampler(cfg); err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, err.Error()))
		return
	}

	slog.Info("trace sampler changed",
		"previous_sampler", previous.Sampler,
		"previous_ratio", previous.Ratio,
		"previous_parent_based", previous.ParentBased,
		"sampler", cfg.Sampler,
		"ratio", cfg.Ratio,
		"parent_based", cfg.ParentBased,
	)

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(cfg); err != nil {
		slog.Debug("failed to encode response", "error", err)
	}
}

// SetLogLevel changes the runtime log level and returns the previous and
// current levels. Requires a platform operator.
func (a *AdminHandlers) SetLogLevel(w http.ResponseWriter, r *http.Request) {
	if !a.operator.Require(w, r) {
		return
	}

	var req struct {
		Level string `json:"level"`
	}
	if err := common.DecodeBoundedJSON(w, r, maxAdminBodyBytes, &req); err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid request body"))
		return
	}
	if req.Level == "" {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "level is required"))
		return
	}

	level, ok := logging.LookupLevel(req.Level)
	if !ok {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest,
			fmt.Sprintf("unknown level: accepted values are %s", strings.Join(acceptedLogLevels, ", "))))
		return
	}

	previous := logging.LevelString(logging.Level.Level())
	logging.Level.Set(level)
	current := logging.LevelString(logging.Level.Level())

	slog.Info("log level changed", "previous", previous, "current", current)

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]string{
		"level":    current,
		"previous": previous,
	}); err != nil {
		slog.Debug("failed to encode response", "error", err)
	}
}
