package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// Handler implements the workflow import/export HTTP endpoints.
type Handler struct {
	factory spi.StoreFactory
	engine  *Engine
}

// New returns a new Handler wired to the given StoreFactory and Engine.
func New(factory spi.StoreFactory, engine *Engine) *Handler {
	return &Handler{factory: factory, engine: engine}
}

// workflowImportDef mirrors spi.WorkflowDefinition but uses *bool for Active
// so the handler can distinguish "absent" (default to true, preserves OOTB
// contract) from explicit "false" (operator wants the workflow staged
// inactive). The SPI type stays plain bool — this distinction is purely a
// request-shape concern. An explicit JSON `null` decodes to (*bool)(nil)
// and is treated the same as the field being absent — both default to
// Active=true.
type workflowImportDef struct {
	Version      string                         `json:"version"`
	Name         string                         `json:"name"`
	Description  string                         `json:"desc,omitempty"`
	InitialState string                         `json:"initialState"`
	Active       *bool                          `json:"active"`
	Criterion    json.RawMessage                `json:"criterion,omitempty"`
	States       map[string]spi.StateDefinition `json:"states"`
}

// importRequest is the JSON body shape for workflow import.
type importRequest struct {
	ImportMode string              `json:"importMode"`
	Workflows  []workflowImportDef `json:"workflows"`
}

// ImportEntityModelWorkflow handles POST /model/{entityName}/{modelVersion}/workflow/import.
func (h *Handler) ImportEntityModelWorkflow(w http.ResponseWriter, r *http.Request, entityName string, modelVersion int32) {
	r.Body = http.MaxBytesReader(w, r.Body, 10*1024*1024)

	data, err := io.ReadAll(r.Body)
	if err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "failed to read request body"))
		return
	}

	var req importRequest
	if err := json.Unmarshal(data, &req); err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, fmt.Sprintf("invalid JSON: %v", err)))
		return
	}

	mode := strings.ToUpper(req.ImportMode)
	if mode == "" {
		mode = "MERGE"
	}
	if mode != "MERGE" && mode != "REPLACE" && mode != "ACTIVATE" {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, fmt.Sprintf("unknown importMode: %s", mode)))
		return
	}

	ref := spi.ModelRef{
		EntityName:   entityName,
		ModelVersion: fmt.Sprintf("%d", modelVersion),
	}

	// Verify the target model exists before applying the workflow. Without this
	// guard, importing a workflow on a non-existent model silently succeeded;
	// cyoda-cloud parity requires HTTP 404 + MODEL_NOT_FOUND.
	modelStore, err := h.factory.ModelStore(r.Context())
	if err != nil {
		common.WriteError(w, r, common.Internal("failed to get model store", err))
		return
	}
	if _, err := modelStore.Get(r.Context(), ref); err != nil {
		if errors.Is(err, spi.ErrNotFound) {
			appErr := common.Operational(http.StatusNotFound, common.ErrCodeModelNotFound,
				fmt.Sprintf("cannot find model entityName=%s, version=%d", entityName, modelVersion))
			appErr.Props = map[string]any{
				"entityName":    entityName,
				"entityVersion": modelVersion,
			}
			common.WriteError(w, r, appErr)
			return
		}
		common.WriteError(w, r, common.Internal("failed to load model", err))
		return
	}

	wfStore, err := h.factory.WorkflowStore(r.Context())
	if err != nil {
		common.WriteError(w, r, common.Internal("failed to get workflow store", err))
		return
	}

	// Load existing workflows (not-found is treated as empty).
	existing, err := wfStore.Get(r.Context(), ref)
	if err != nil && errors.Is(err, spi.ErrNotFound) {
		existing = nil
	} else if err != nil {
		common.WriteError(w, r, common.Internal("failed to load existing workflows", err))
		return
	}

	// REPLACE / ACTIVATE with an empty workflows array would silently
	// wipe or deactivate all stored workflows for the model; the engine
	// fallback to the embedded default then masks the destruction behind
	// HTTP 200. Reject explicitly. MERGE-empty stays a no-op.
	if len(req.Workflows) == 0 && (mode == "REPLACE" || mode == "ACTIVATE") {
		common.WriteError(w, r, common.Operational(
			http.StatusBadRequest,
			common.ErrCodeValidationFailed,
			"empty workflows array not allowed in REPLACE/ACTIVATE mode — use MERGE if you intended a no-op"))
		return
	}

	// Default Active to true only when the field is absent; explicit
	// true/false pass through unchanged. This restores export → REPLACE
	// re-import idempotency and lets operators stage inactive workflows
	// for blue/green rollout.
	incoming := make([]spi.WorkflowDefinition, len(req.Workflows))
	for i, w := range req.Workflows {
		active := true
		if w.Active != nil {
			active = *w.Active
		}
		incoming[i] = spi.WorkflowDefinition{
			Version:      w.Version,
			Name:         w.Name,
			Description:  w.Description,
			InitialState: w.InitialState,
			Active:       active,
			Criterion:    w.Criterion,
			States:       w.States,
		}
	}

	// Structural validation runs on the incoming request only — the
	// H4/H6 rules are deliberately not retroactive against legacy stored
	// shapes. Behavioural validation (loops + flag coherence) runs on
	// the merged result below, preserving pre-v0.8.0 semantics for those
	// specific invariants.
	if err := validateImportRequest(incoming); err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeValidationFailed, err.Error()))
		return
	}

	result := applyImportMode(existing, incoming, mode)

	// Behavioural validation on the merged result: definite-loop
	// detection and StartNewTxOnDispatch coherence. A pre-existing
	// stored workflow that violates these still surfaces here, matching
	// pre-structural-validation behaviour.
	if err := validateWorkflows(result); err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeValidationFailed, err.Error()))
		return
	}

	if err := wfStore.Save(r.Context(), ref, result); err != nil {
		common.WriteError(w, r, common.Internal("failed to save workflows", err))
		return
	}

	// Audit log on success: workflow configuration is a high-impact, mutable
	// multi-tenant surface. One INFO line per import names the mode, the
	// model, and the per-workflow {name, desc} pairs so operators can
	// correlate change intent in logs. Description is wired through this
	// payload as its operator-visible consumer.
	//
	// When the import leaves the model with zero workflows, log at WARN
	// instead — the engine will silently fall back to the embedded default
	// on every subsequent execution, and that "running on default" outcome
	// must be visible in operator logs. REPLACE/ACTIVATE empty is already
	// rejected upstream by the structural validator, so the only reachable
	// path is MERGE-empty on a model with no prior workflows; the canary
	// still defends against any future code path that lands there.
	wfDigest := make([]map[string]string, len(result))
	wfNames := make([]string, len(result))
	for i, wf := range result {
		wfDigest[i] = map[string]string{"name": wf.Name, "desc": wf.Description}
		wfNames[i] = wf.Name
	}
	logAttrs := []any{
		slog.String("pkg", "workflow"),
		slog.String("tenant", common.TenantFromContext(r.Context())),
		slog.String("entityName", entityName),
		slog.String("modelVersion", ref.ModelVersion),
		slog.String("importMode", mode),
		slog.Int("workflowCount", len(result)),
		slog.Any("workflows", wfDigest),
		slog.Any("workflowNames", wfNames),
	}
	if len(result) == 0 {
		slog.WarnContext(r.Context(), "workflow import resulted in zero workflows", logAttrs...)
	} else {
		slog.InfoContext(r.Context(), "workflow import applied", logAttrs...)
	}

	common.WriteJSON(w, http.StatusOK, map[string]any{"success": true})
}

// ExportEntityModelWorkflow handles GET /model/{entityName}/{modelVersion}/workflow/export.
func (h *Handler) ExportEntityModelWorkflow(w http.ResponseWriter, r *http.Request, entityName string, modelVersion int32) {
	ref := spi.ModelRef{
		EntityName:   entityName,
		ModelVersion: fmt.Sprintf("%d", modelVersion),
	}

	// Verify the model exists before reporting on its workflows. Without this
	// guard, exports against a non-existent model returned the same
	// WORKFLOW_NOT_FOUND as an existing-but-empty model, conflating two
	// distinct failure modes. The import handler enforces the same
	// distinction; export now mirrors it.
	modelStore, err := h.factory.ModelStore(r.Context())
	if err != nil {
		common.WriteError(w, r, common.Internal("failed to get model store", err))
		return
	}
	if _, err := modelStore.Get(r.Context(), ref); err != nil {
		if errors.Is(err, spi.ErrNotFound) {
			appErr := common.Operational(http.StatusNotFound, common.ErrCodeModelNotFound,
				fmt.Sprintf("cannot find model entityName=%s, version=%d", entityName, modelVersion))
			appErr.Props = map[string]any{
				"entityName":    entityName,
				"entityVersion": modelVersion,
			}
			common.WriteError(w, r, appErr)
			return
		}
		common.WriteError(w, r, common.Internal("failed to load model", err))
		return
	}

	wfStore, err := h.factory.WorkflowStore(r.Context())
	if err != nil {
		common.WriteError(w, r, common.Internal("failed to get workflow store", err))
		return
	}

	workflows, err := wfStore.Get(r.Context(), ref)
	if err != nil {
		common.WriteError(w, r, common.Internal("failed to load workflows", err))
		return
	}
	if len(workflows) == 0 {
		common.WriteError(w, r, common.Operational(http.StatusNotFound, common.ErrCodeWorkflowNotFound,
			fmt.Sprintf("no workflows found for model %s/%d", entityName, modelVersion)))
		return
	}

	resp := map[string]any{
		"entityName":   entityName,
		"modelVersion": modelVersion,
		"workflows":    workflows,
	}

	common.WriteJSON(w, http.StatusOK, resp)
}
