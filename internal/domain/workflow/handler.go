package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// importRequest is the JSON body shape for workflow import.
type importRequest struct {
	ImportMode string                   `json:"importMode"`
	Workflows  []spi.WorkflowDefinition `json:"workflows"`
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

	// Schema-version gate — runs before any mutation or store access.
	// Surfaces with WORKFLOW_SCHEMA_VERSION_UNSUPPORTED so clients can
	// distinguish "wrong contract version" from generic validation
	// failures.
	if err := validateSchemaVersions(req.Workflows); err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest,
			common.ErrCodeWorkflowSchemaVersionUnsupported, err.Error()))
		return
	}

	ref := spi.ModelRef{
		EntityName:   entityName,
		ModelVersion: fmt.Sprintf("%d", modelVersion),
	}

	// Verify the target model exists before applying the workflow. Without this
	// guard, importing a workflow on a non-existent model silently succeeded
	// (issue #131); cyoda-cloud parity requires HTTP 404 + MODEL_NOT_FOUND.
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

	// Default imported workflows to active (Cyoda Cloud behavior).
	for i := range req.Workflows {
		req.Workflows[i].Active = true
	}

	result := applyImportMode(existing, req.Workflows, mode)

	// Static validation: detect definite infinite loops before saving.
	if err := validateWorkflows(result); err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeValidationFailed, err.Error()))
		return
	}

	if err := wfStore.Save(r.Context(), ref, result); err != nil {
		common.WriteError(w, r, common.Internal("failed to save workflows", err))
		return
	}

	common.WriteJSON(w, http.StatusOK, map[string]any{"success": true})
}

// ExportEntityModelWorkflow handles GET /model/{entityName}/{modelVersion}/workflow/export.
func (h *Handler) ExportEntityModelWorkflow(w http.ResponseWriter, r *http.Request, entityName string, modelVersion int32) {
	ref := spi.ModelRef{
		EntityName:   entityName,
		ModelVersion: fmt.Sprintf("%d", modelVersion),
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
