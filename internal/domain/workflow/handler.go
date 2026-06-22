package workflow

import (
	"bytes"
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

// descLogPreviewRunes caps the per-workflow `desc` field in the audit-log
// digest. Description is operator-supplied free text with no upstream
// length cap; emitting it verbatim risks unbounded log lines from a
// multi-KB paste. 200 runes matches the convention used by
// logging.PayloadPreview for DEBUG-level payload previews.
const descLogPreviewRunes = 200

// truncateForLog returns s clipped to maxRunes runes (not bytes) with a
// "..." suffix when truncation occurred. Used for audit-log field
// previews. Rune-aware so multi-byte UTF-8 characters (CJK, emoji,
// accented Latin) are never split mid-codepoint — slicing a UTF-8 string
// at a byte offset that falls inside a multi-byte sequence produces
// invalid UTF-8, which downstream slog handlers or log viewers may
// reject or mangle.
func truncateForLog(s string, maxRunes int) string {
	// Fast path: byte length is a cheap upper bound on rune count
	// (every rune is at least one byte). When len(s) ≤ maxRunes the
	// rune count is also ≤ maxRunes, so no truncation is needed.
	if len(s) <= maxRunes {
		return s
	}
	// Walk runes. for ... range s yields the byte offset of each rune
	// start; once we've seen maxRunes runes, slicing at the next start
	// gives exactly maxRunes runes worth of bytes and the cut sits on
	// a valid UTF-8 boundary.
	count := 0
	for i := range s {
		if count == maxRunes {
			return s[:i] + "..."
		}
		count++
	}
	// Total rune count ≤ maxRunes despite len(s) > maxRunes — only
	// possible if the byte-length fast path didn't apply but the string
	// contained multi-byte runes that pushed bytes above the limit
	// while keeping runes at or below it.
	return s
}

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
	ImportMode  string              `json:"importMode"`
	AllowCycles bool                `json:"allowCycles,omitempty"`
	Workflows   []workflowImportDef `json:"workflows"`
}

// ImportEntityModelWorkflow handles POST /model/{entityName}/{modelVersion}/workflow/import.
func (h *Handler) ImportEntityModelWorkflow(w http.ResponseWriter, r *http.Request, entityName string, modelVersion int32) {
	r.Body = http.MaxBytesReader(w, r.Body, 10*1024*1024)

	data, err := io.ReadAll(r.Body)
	if err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "failed to read request body"))
		return
	}

	// Strict-decoder boundary: unknown fields in the import-request body —
	// top-level or nested in the workflow / state / transition / processor
	// sub-shapes — are rejected with 400 BAD_REQUEST rather than silently
	// dropped. This catches forward-compat extras and typos (e.g.
	// `transitionn` for `transitions`) that previously imported as no-op
	// workflows. Go's decoder emits `json: unknown field "X"` which names
	// the offending field directly in the response detail.
	//
	// Decode consumes exactly one JSON value, so anything after the first
	// object's closing brace is silently ignored unless we explicitly
	// check. dec.More() fences that trailing-garbage case — same class of
	// "client got the shape wrong" failure as an unknown field.
	var req importRequest
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, fmt.Sprintf("invalid JSON: %v", err)))
		return
	}
	if dec.More() {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid JSON: trailing data after request object"))
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

	// Default Active to true only when the field is absent; explicit
	// true/false pass through unchanged. This restores export → REPLACE
	// re-import idempotency and lets operators stage inactive workflows
	// for blue/green rollout. The bridge from request-shape (*bool
	// Active) to SPI-shape (bool Active) is hoisted above the schema-
	// version gate so all downstream validators see a single
	// []spi.WorkflowDefinition slice — the conversion is purely
	// in-memory and does not touch any store, preserving the gate's
	// before-any-store-access intent.
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

	// Schema-version gate — runs before any mutation or store access.
	// Surfaces with WORKFLOW_SCHEMA_VERSION_UNSUPPORTED so clients can
	// distinguish "wrong contract version" from generic validation
	// failures.
	if err := validateSchemaVersions(incoming); err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest,
			common.ErrCodeWorkflowSchemaVersionUnsupported, err.Error()))
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
	if len(incoming) == 0 && (mode == "REPLACE" || mode == "ACTIVATE") {
		common.WriteError(w, r, common.Operational(
			http.StatusBadRequest,
			common.ErrCodeValidationFailed,
			"empty workflows array not allowed in REPLACE/ACTIVATE mode — use MERGE if you intended a no-op"))
		return
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
	if err := validateWorkflows(result, req.AllowCycles); err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeValidationFailed, err.Error()))
		return
	}

	if req.AllowCycles {
		workflowNames := make([]string, len(result))
		for i, wf := range result {
			workflowNames[i] = wf.Name
		}
		slog.WarnContext(r.Context(), "workflow import: cycle validation bypassed",
			"pkg", "workflow",
			"tenant", common.TenantFromContext(r.Context()),
			"entityName", entityName,
			"modelVersion", ref.ModelVersion,
			"importMode", mode,
			"workflows", workflowNames)
	}

	if err := wfStore.Save(r.Context(), ref, result); err != nil {
		common.WriteError(w, r, common.Internal("failed to save workflows", err))
		return
	}

	// Audit log on success: workflow configuration is a high-impact, mutable
	// multi-tenant surface. One INFO line per import names the mode and the
	// digest of THIS CALL's incoming payload — the audit subject is the
	// change applied, not the resulting model state. `workflowCount` is the
	// incoming size; `storedWorkflowCount` reports the model's post-merge
	// total so operators can see both the diff and the resulting fan-out
	// without consulting the workflow JSON. Description is wired through
	// the per-workflow digest as its operator-visible consumer, truncated
	// per-entry to bound log volume (Description has no upstream length
	// cap).
	//
	// When the import leaves the model with zero workflows, log at WARN
	// instead — the engine will silently fall back to the embedded default
	// on every subsequent execution, and that "running on default" outcome
	// must be visible in operator logs. REPLACE/ACTIVATE empty is already
	// rejected upstream by the structural validator, so the only reachable
	// path is MERGE-empty on a model with no prior workflows; the canary
	// still defends against any future code path that lands there.
	wfDigest := make([]map[string]string, len(incoming))
	for i, wf := range incoming {
		wfDigest[i] = map[string]string{
			"name": wf.Name,
			"desc": truncateForLog(wf.Description, descLogPreviewRunes),
		}
	}
	logAttrs := []any{
		slog.String("pkg", "workflow"),
		slog.String("tenant", common.TenantFromContext(r.Context())),
		slog.String("entityName", entityName),
		slog.String("modelVersion", ref.ModelVersion),
		slog.String("importMode", mode),
		slog.Int("workflowCount", len(incoming)),
		slog.Int("storedWorkflowCount", len(result)),
		slog.Any("workflows", wfDigest),
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

	// Stamp the current schema version on every workflow on the wire.
	// The stored Version is the workflow content's record; the exported
	// Version is the serialiser's contract. Callers re-importing an
	// export always see the current contract.
	stamped := make([]spi.WorkflowDefinition, len(workflows))
	for i, wf := range workflows {
		wf.Version = CurrentSchemaVersion
		stamped[i] = wf
	}

	resp := map[string]any{
		"entityName":   entityName,
		"modelVersion": modelVersion,
		"workflows":    stamped,
	}

	common.WriteJSON(w, http.StatusOK, resp)
}
