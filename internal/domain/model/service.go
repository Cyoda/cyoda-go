package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/exporter"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/importer"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

// classifyGetErr maps ModelStore.Get errors to AppError: genuine not-found
// → 404 MODEL_NOT_FOUND (via modelNotFound), everything else → 5xx with
// the original error preserved in the chain for ticket-correlated logging.
// Callers pass the per-operation verb (e.g. "export model") for the 5xx
// message so operators can identify the failing path from the ticket log.
func classifyGetErr(verb string, entityName string, ver int32, err error) *common.AppError {
	if errors.Is(err, spi.ErrNotFound) {
		return modelNotFound(entityName, ver)
	}
	return common.Internal(fmt.Sprintf("failed to %s", verb), err)
}

// ImportModelInput carries the parameters for importing a model.
type ImportModelInput struct {
	EntityName   string
	ModelVersion string
	Format       string
	Converter    string
	Data         []byte
}

// ImportModelResult carries the result of a model import.
type ImportModelResult struct {
	ModelID string
}

// ExportModelResult carries the result of a model export.
type ExportModelResult struct {
	Payload    json.RawMessage
	UniqueKeys []spi.UniqueKey
}

// ModelTransitionResult carries the result of a model state transition.
type ModelTransitionResult struct {
	ModelID string
	State   string
}

// ModelInfo carries summary information about a model.
type ModelInfo struct {
	ID         string
	Name       string
	Version    int
	State      string
	UpdateDate time.Time
}

// parseVersion converts a string model version to int32.
func parseVersion(v string) int32 {
	n, _ := strconv.ParseInt(v, 10, 32)
	return int32(n)
}

// getModelFresh returns the model descriptor, bypassing any per-request
// cache layer when the store supports RefreshAndGet. In multi-node
// cluster deployments this eliminates a stale-cache race window
// between a peer's mutation and its gossip-borne invalidation.
// Admin-path gating reads (ImportModel, LockModel, UnlockModel,
// DeleteModel, SetChangeLevel) use this; routine display/listing
// reads (ExportModel, ListModels, ValidateModel) keep the cache for
// throughput — eventual consistency is acceptable for those paths.
func getModelFresh(ctx context.Context, store spi.ModelStore, ref spi.ModelRef) (*spi.ModelDescriptor, error) {
	type refresher interface {
		RefreshAndGet(ctx context.Context, ref spi.ModelRef) (*spi.ModelDescriptor, error)
	}
	if r, ok := store.(refresher); ok {
		return r.RefreshAndGet(ctx, ref)
	}
	return store.Get(ctx, ref)
}

// ImportModel imports a model from sample data, merging with any existing schema.
func (h *Handler) ImportModel(ctx context.Context, input ImportModelInput) (*ImportModelResult, error) {
	if input.Converter != "SAMPLE_DATA" {
		return nil, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "unsupported import converter")
	}

	store, err := h.factory.ModelStore(ctx)
	if err != nil {
		return nil, common.Internal("failed to access model store", err)
	}

	ver := parseVersion(input.ModelVersion)
	ref := modelRef(input.EntityName, ver)

	// Bypass the per-request cache: in a multi-node cluster the cache
	// can briefly serve a stale LOCKED descriptor in the window between a
	// peer's delete and its gossip-borne invalidation. Admin operations
	// are low-frequency, so one forced round-trip is fine.
	existing, err := getModelFresh(ctx, store, ref)
	if err != nil {
		existing = nil
	}

	if existing != nil && existing.State == spi.ModelLocked {
		appErr := common.Operational(
			http.StatusConflict,
			common.ErrCodeModelAlreadyLocked,
			fmt.Sprintf("cannot save entityModel{name=%s, version=%d} because this model has already been registered", input.EntityName, ver))
		appErr.Props = map[string]any{
			"entityName":    input.EntityName,
			"entityVersion": ver,
		}
		return nil, appErr
	}

	newNode, err := importer.NewSampleDataImporter().Import(
		bytes.NewReader(input.Data), input.Format)
	if err != nil {
		return nil, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, err.Error())
	}

	var finalNode *schema.ModelNode
	if existing != nil && len(existing.Schema) > 0 {
		existingNode, err := schema.Unmarshal(existing.Schema)
		if err != nil {
			return nil, common.Internal("failed to unmarshal existing schema", err)
		}
		finalNode = schema.Merge(existingNode, newNode)
	} else {
		finalNode = newNode
	}

	schemaBytes, err := schema.Marshal(finalNode)
	if err != nil {
		return nil, common.Internal("failed to marshal schema", err)
	}

	desc := &spi.ModelDescriptor{
		Ref:        ref,
		State:      spi.ModelUnlocked,
		UpdateDate: time.Now(),
		Schema:     schemaBytes,
	}
	if existing != nil {
		desc.ChangeLevel = existing.ChangeLevel
		desc.UniqueKeys = existing.UniqueKeys

		// Defensive guard: re-validate carried-forward keys against the merged
		// schema. schema.Merge is additive and cannot drop an existing field, so
		// this targets out-of-band descriptor corruption or future
		// merge-semantics changes — not a scenario reachable via normal API use.
		if len(desc.UniqueKeys) > 0 {
			if valErr := schema.ValidateUniqueKeys(finalNode, desc.UniqueKeys); valErr != nil {
				var keyDefErr *schema.UniqueKeyDefError
				if errors.As(valErr, &keyDefErr) {
					appErr := common.Operational(
						http.StatusUnprocessableEntity,
						common.ErrCodeInvalidUniqueKeyDefinition,
						fmt.Sprintf("re-import invalidates existing unique key definitions: %s", keyDefErr.Reason))
					appErr.Props = map[string]any{
						"entityName":    input.EntityName,
						"entityVersion": ver,
					}
					return nil, appErr
				}
				return nil, common.Internal("failed to validate existing unique key definitions against new schema", valErr)
			}
		}
	}

	if err := store.Save(ctx, desc); err != nil {
		return nil, common.Internal("failed to save model", err)
	}

	return &ImportModelResult{ModelID: deterministicID(ref).String()}, nil
}

// ExportModel exports a model schema using the specified converter.
func (h *Handler) ExportModel(ctx context.Context, entityName, modelVersion, converter string) (*ExportModelResult, error) {
	store, err := h.factory.ModelStore(ctx)
	if err != nil {
		return nil, common.Internal("failed to access model store", err)
	}

	ver := parseVersion(modelVersion)
	ref := modelRef(entityName, ver)
	desc, err := store.Get(ctx, ref)
	if err != nil {
		return nil, classifyGetErr("export model", entityName, ver, err)
	}

	node, err := schema.Unmarshal(desc.Schema)
	if err != nil {
		return nil, common.Internal("failed to unmarshal schema", err)
	}

	var exp exporter.Exporter
	switch converter {
	case "JSON_SCHEMA":
		exp = exporter.NewJSONSchemaExporter(string(desc.State))
	case "SIMPLE_VIEW":
		exp = exporter.NewSimpleViewExporter(string(desc.State))
	default:
		return nil, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "unsupported export converter")
	}

	exported, err := exp.Export(node)
	if err != nil {
		return nil, common.Internal("export failed", err)
	}

	// Inject uniqueKeys into the exported payload as a sibling field.
	// Always present (empty slice when no keys defined) for consumer consistency.
	uks := desc.UniqueKeys
	if uks == nil {
		uks = []spi.UniqueKey{}
	}
	type uniqueKeyExport struct {
		ID     string   `json:"id"`
		Fields []string `json:"fields"`
	}
	ukExports := make([]uniqueKeyExport, 0, len(uks))
	for _, k := range uks {
		ukExports = append(ukExports, uniqueKeyExport{ID: k.ID, Fields: k.Fields})
	}
	var m map[string]any
	if err2 := json.Unmarshal(exported, &m); err2 == nil {
		m["uniqueKeys"] = ukExports
		if b, err2 := json.Marshal(m); err2 == nil {
			exported = b
		}
	}

	return &ExportModelResult{Payload: exported, UniqueKeys: uks}, nil
}

// LockModel locks a model, preventing further imports.
func (h *Handler) LockModel(ctx context.Context, entityName, modelVersion string) (*ModelTransitionResult, error) {
	store, err := h.factory.ModelStore(ctx)
	if err != nil {
		return nil, common.Internal("failed to access model store", err)
	}

	ver := parseVersion(modelVersion)
	ref := modelRef(entityName, ver)

	// Admin-path gating read — bypass the per-request cache; see
	// getModelFresh for the multi-node rationale.
	desc, err := getModelFresh(ctx, store, ref)
	if err != nil {
		return nil, classifyGetErr("lock model", entityName, ver, err)
	}
	if desc == nil {
		return nil, modelNotFound(entityName, ver)
	}

	if desc.State == spi.ModelLocked {
		appErr := common.Operational(
			http.StatusConflict,
			common.ErrCodeModelAlreadyLocked,
			fmt.Sprintf("cannot process entityModel{entityName=%s, entityVersion=%d}. expectedState=UNLOCKED, actualState=LOCKED", entityName, ver))
		appErr.Props = map[string]any{
			"entityName":    entityName,
			"entityVersion": ver,
			"expectedState": "UNLOCKED",
			"actualState":   "LOCKED",
		}
		return nil, appErr
	}

	if err := store.Lock(ctx, ref); err != nil {
		return nil, common.Internal("failed to lock model", err)
	}

	return &ModelTransitionResult{
		ModelID: deterministicID(ref).String(),
		State:   "LOCKED",
	}, nil
}

// UnlockModel unlocks a model, allowing further imports. Blocked if entities exist.
func (h *Handler) UnlockModel(ctx context.Context, entityName, modelVersion string) (*ModelTransitionResult, error) {
	modelStore, err := h.factory.ModelStore(ctx)
	if err != nil {
		return nil, common.Internal("failed to access model store", err)
	}

	ver := parseVersion(modelVersion)
	ref := modelRef(entityName, ver)

	// Admin-path gating read — bypass the per-request cache; see
	// getModelFresh for the multi-node rationale.
	desc, err := getModelFresh(ctx, modelStore, ref)
	if err != nil {
		return nil, classifyGetErr("unlock model", entityName, ver, err)
	}
	if desc == nil {
		return nil, modelNotFound(entityName, ver)
	}

	if desc.State != spi.ModelLocked {
		appErr := common.Operational(
			http.StatusConflict,
			common.ErrCodeModelAlreadyUnlocked,
			fmt.Sprintf("cannot process entityModel{entityName=%s, entityVersion=%d}. expectedState=LOCKED, actualState=UNLOCKED", entityName, ver))
		appErr.Props = map[string]any{
			"entityName":    entityName,
			"entityVersion": ver,
			"expectedState": "LOCKED",
			"actualState":   "UNLOCKED",
		}
		return nil, appErr
	}

	entityStore, err := h.factory.EntityStore(ctx)
	if err != nil {
		return nil, common.Internal("failed to access entity store", err)
	}

	count, err := entityStore.Count(ctx, ref)
	if err != nil {
		return nil, common.Internal("failed to count entities", err)
	}
	if count > 0 {
		appErr := common.Operational(
			http.StatusConflict,
			common.ErrCodeModelHasEntities,
			fmt.Sprintf("cannot unlock: %d entities exist", count))
		appErr.Props = map[string]any{
			"entityName":    entityName,
			"entityVersion": ver,
			"entityCount":   count,
		}
		return nil, appErr
	}

	if err := modelStore.Unlock(ctx, ref); err != nil {
		return nil, common.Internal("failed to unlock model", err)
	}

	return &ModelTransitionResult{
		ModelID: deterministicID(ref).String(),
		State:   "UNLOCKED",
	}, nil
}

// DeleteModel deletes a model. Blocked if model is locked or entities exist.
func (h *Handler) DeleteModel(ctx context.Context, entityName, modelVersion string) error {
	modelStore, err := h.factory.ModelStore(ctx)
	if err != nil {
		return common.Internal("failed to access model store", err)
	}

	ver := parseVersion(modelVersion)
	ref := modelRef(entityName, ver)

	// Admin-path gating read — bypass the per-request cache; see
	// getModelFresh for the multi-node rationale.
	desc, err := getModelFresh(ctx, modelStore, ref)
	if err != nil {
		return classifyGetErr("delete model", entityName, ver, err)
	}
	if desc == nil {
		return modelNotFound(entityName, ver)
	}

	entityStore, err := h.factory.EntityStore(ctx)
	if err != nil {
		return common.Internal("failed to access entity store", err)
	}

	count, err := entityStore.Count(ctx, ref)
	if err != nil {
		return common.Internal("failed to count entities", err)
	}
	if count > 0 {
		appErr := common.Operational(
			http.StatusConflict,
			common.ErrCodeModelHasEntities,
			fmt.Sprintf("cannot delete: %d entities exist", count))
		appErr.Props = map[string]any{
			"entityName":    entityName,
			"entityVersion": ver,
			"entityCount":   count,
		}
		return appErr
	}

	if err := modelStore.Delete(ctx, ref); err != nil {
		return common.Internal("failed to delete model", err)
	}

	return nil
}

// ListModels returns summary information for all models.
func (h *Handler) ListModels(ctx context.Context) ([]ModelInfo, error) {
	store, err := h.factory.ModelStore(ctx)
	if err != nil {
		return nil, common.Internal("failed to access model store", err)
	}

	refs, err := store.GetAll(ctx)
	if err != nil {
		return nil, common.Internal("failed to list models", err)
	}

	models := make([]ModelInfo, 0, len(refs))
	for _, ref := range refs {
		desc, err := store.Get(ctx, ref)
		if err != nil {
			return nil, common.Internal("failed to load model", err)
		}

		ver, _ := strconv.ParseInt(ref.ModelVersion, 10, 32)
		models = append(models, ModelInfo{
			ID:         deterministicID(ref).String(),
			Name:       ref.EntityName,
			Version:    int(ver),
			State:      string(desc.State),
			UpdateDate: desc.UpdateDate,
		})
	}

	return models, nil
}

// ValidateModel validates data against a model's schema.
func (h *Handler) ValidateModel(ctx context.Context, entityName, modelVersion string, data json.RawMessage) error {
	store, err := h.factory.ModelStore(ctx)
	if err != nil {
		return common.Internal("failed to access model store", err)
	}

	ver := parseVersion(modelVersion)
	ref := modelRef(entityName, ver)
	desc, err := store.Get(ctx, ref)
	if err != nil {
		return classifyGetErr("validate model", entityName, ver, err)
	}

	modelNode, err := schema.Unmarshal(desc.Schema)
	if err != nil {
		return common.Internal("failed to unmarshal schema", err)
	}

	var parsedData any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&parsedData); err != nil {
		return common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "failed to parse request body")
	}

	validationErrors := schema.Validate(modelNode, parsedData)
	if len(validationErrors) == 0 {
		return nil
	}

	msgs := make([]string, len(validationErrors))
	for i, ve := range validationErrors {
		msgs[i] = ve.Error()
	}
	return &ValidationError{
		Message: "Validation failed: " + strings.Join(msgs, "; "),
	}
}

// validChangeLevelValues returns the canonical set of accepted ChangeLevel
// strings, used for the structured `validValues` property on
// INVALID_CHANGE_LEVEL responses. Kept in sync with spi.ValidateChangeLevel.
func validChangeLevelValues() []string {
	return []string{
		string(spi.ChangeLevelArrayLength),
		string(spi.ChangeLevelArrayElements),
		string(spi.ChangeLevelType),
		string(spi.ChangeLevelStructural),
	}
}

// SetChangeLevel sets the change level on a model.
func (h *Handler) SetChangeLevel(ctx context.Context, entityName, modelVersion, changeLevel string) error {
	store, err := h.factory.ModelStore(ctx)
	if err != nil {
		return common.Internal("failed to access model store", err)
	}

	ver := parseVersion(modelVersion)
	ref := modelRef(entityName, ver)

	// Admin-path gating read — bypass the per-request cache; see
	// getModelFresh for the multi-node rationale.
	desc, err := getModelFresh(ctx, store, ref)
	if err != nil {
		return classifyGetErr("set change level", entityName, ver, err)
	}
	if desc == nil {
		return modelNotFound(entityName, ver)
	}

	cl, err := spi.ValidateChangeLevel(changeLevel)
	if err != nil {
		// spi.ValidateChangeLevel returns a fmt.Errorf-wrapped message that
		// happens to start with "BAD_REQUEST: " — strip it so the
		// AppError.Operational prefix doesn't double up. The detail string
		// is informative ("invalid change level %q; valid values: ...").
		msg := strings.TrimPrefix(err.Error(), "BAD_REQUEST: ")
		appErr := common.Operational(http.StatusBadRequest, common.ErrCodeInvalidChangeLevel, msg)
		appErr.Props = map[string]any{
			"entityName":    entityName,
			"entityVersion": ver,
			"suppliedValue": changeLevel,
			"validValues":   validChangeLevelValues(),
		}
		return appErr
	}

	if err := store.SetChangeLevel(ctx, ref, cl); err != nil {
		return common.Internal("failed to set change level", err)
	}

	return nil
}

// SetUniqueKeys sets composite unique-key definitions on an unlocked model.
// The backend must advertise spi.CompositeUniqueKeyCapable; the model must be
// UNLOCKED; and every key must reference only known scalar leaf fields.
func (h *Handler) SetUniqueKeys(ctx context.Context, entityName, modelVersion string, keys []spi.UniqueKey) (*ModelTransitionResult, error) {
	// Capability gate FIRST — fast path for unsupported backends.
	if c, ok := h.factory.(spi.CompositeUniqueKeyCapable); !ok || !c.SupportsCompositeUniqueKeys() {
		return nil, common.Operational(
			http.StatusUnprocessableEntity,
			common.ErrCodeCompositeKeyUnsupported,
			"backend does not support composite unique keys")
	}

	store, err := h.factory.ModelStore(ctx)
	if err != nil {
		return nil, common.Internal("failed to access model store", err)
	}

	ver := parseVersion(modelVersion)
	ref := modelRef(entityName, ver)

	// Admin-path gating read — bypass the per-request cache; see
	// getModelFresh for the multi-node rationale.
	desc, err := getModelFresh(ctx, store, ref)
	if err != nil {
		return nil, classifyGetErr("set unique keys", entityName, ver, err)
	}
	if desc == nil {
		return nil, modelNotFound(entityName, ver)
	}

	if desc.State == spi.ModelLocked {
		appErr := common.Operational(
			http.StatusConflict,
			common.ErrCodeModelAlreadyLocked,
			fmt.Sprintf("cannot process entityModel{entityName=%s, entityVersion=%d}. expectedState=UNLOCKED, actualState=LOCKED", entityName, ver))
		appErr.Props = map[string]any{
			"entityName":    entityName,
			"entityVersion": ver,
			"expectedState": "UNLOCKED",
			"actualState":   "LOCKED",
		}
		return nil, appErr
	}

	node, err := schema.Unmarshal(desc.Schema)
	if err != nil {
		return nil, common.Internal("failed to unmarshal schema", err)
	}

	if valErr := schema.ValidateUniqueKeys(node, keys); valErr != nil {
		var keyDefErr *schema.UniqueKeyDefError
		if errors.As(valErr, &keyDefErr) {
			appErr := common.Operational(
				http.StatusUnprocessableEntity,
				common.ErrCodeInvalidUniqueKeyDefinition,
				keyDefErr.Reason)
			appErr.Props = map[string]any{
				"entityName":    entityName,
				"entityVersion": ver,
			}
			return nil, appErr
		}
		return nil, common.Internal("failed to validate unique key definitions", valErr)
	}

	desc.UniqueKeys = keys
	if err := store.Save(ctx, desc); err != nil {
		return nil, common.Internal("failed to save model", err)
	}

	return &ModelTransitionResult{
		ModelID: deterministicID(ref).String(),
		State:   string(desc.State),
	}, nil
}

// ValidationError is a non-AppError that signals validation failure (not an HTTP error).
type ValidationError struct {
	Message string
}

func (e *ValidationError) Error() string { return e.Message }
