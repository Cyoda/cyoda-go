package entity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/importer"
	"github.com/cyoda-platform/cyoda-go/internal/domain/pagination"
	wfengine "github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
)

// decodeJSONPreservingNumbers is the precision-preserving counterpart to
// json.Unmarshal: numeric leaves arrive as json.Number rather than float64,
// so callers can choose Int64()/Float64()/string preservation. Mirrors
// importer.ParseJSON's UseNumber() behavior.
func decodeJSONPreservingNumbers(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return dec.Decode(v)
}

// --- Input/Output types ---

// CreateEntityInput holds parameters for creating an entity.
type CreateEntityInput struct {
	EntityName   string
	ModelVersion string
	Format       string
	Data         json.RawMessage
}

// EntityTransactionResult holds the result of an entity operation.
type EntityTransactionResult struct {
	TransactionID string
	EntityIDs     []string
}

// GetOneEntityInput holds parameters for getting an entity.
//
// At most one of PointInTime and TransactionID may be set; the handler
// rejects requests carrying both with HTTP 400 BAD_REQUEST. When
// TransactionID is non-empty, GetEntity scans the entity's version
// history and returns the version whose meta.TransactionID matches; if
// no version matches, ENTITY_NOT_FOUND (404) is returned. Issue #150.
type GetOneEntityInput struct {
	EntityID      string
	PointInTime   *time.Time
	TransactionID string
}

// EntityEnvelope holds a single entity in its response envelope format.
type EntityEnvelope struct {
	Type string
	Data any
	Meta map[string]any
}

// UpdateEntityInput holds parameters for updating an entity.
type UpdateEntityInput struct {
	EntityID   string
	Format     string
	Data       json.RawMessage
	Transition string // optional, empty for loopback
	IfMatch    string // optional ETag for CAS
}

// DeleteAllResult holds the result of deleting all entities for a model.
type DeleteAllResult struct {
	TotalCount    int
	ModelID       string
	EntityModelID string
}

// EntityChangeEntry holds a single entry in version history.
type EntityChangeEntry struct {
	ChangeType    string
	TimeOfChange  string
	User          string
	TransactionID string
	HasEntity     bool
}

// PaginationParams holds pagination parameters.
type PaginationParams struct {
	PageSize   int32
	PageNumber int32
}

// CollectionItem holds a parsed item for batch create.
type CollectionItem struct {
	ModelName    string
	ModelVersion int32
	Payload      json.RawMessage
}

// UpdateCollectionItem holds a parsed item for batch update
// (PUT /api/entity/{format}). Transition is empty for a loopback update.
type UpdateCollectionItem struct {
	EntityID   string
	Payload    json.RawMessage
	Transition string
}

// --- Service methods ---

// CreateEntity creates a single entity with workflow execution and returns
// the transaction result.
func (h *Handler) CreateEntity(ctx context.Context, input CreateEntityInput) (*EntityTransactionResult, error) {
	uc := spi.MustGetUserContext(ctx)

	modelStore, err := h.factory.ModelStore(ctx)
	if err != nil {
		return nil, common.Internal("failed to access model store", err)
	}

	ref := spi.ModelRef{
		EntityName:   input.EntityName,
		ModelVersion: input.ModelVersion,
	}

	// Load model descriptor. Distinguish genuine not-found (404) from other
	// infrastructure errors (5xx) — a schema fold/apply failure or a pgx
	// connection blip must not masquerade as a missing model.
	desc, err := modelStore.Get(ctx, ref)
	if err != nil {
		if errors.Is(err, spi.ErrNotFound) {
			return nil, common.Operational(http.StatusNotFound, common.ErrCodeModelNotFound, "model not found")
		}
		return nil, common.Internal("failed to load model", err)
	}

	// Reject if model not locked
	if desc.State != spi.ModelLocked {
		return nil, common.Operational(http.StatusConflict, common.ErrCodeModelNotLocked, "model is not locked")
	}

	// Parse body based on format
	bodyBytes := []byte(input.Data)
	var parsedData any
	switch input.Format {
	case "JSON":
		if err := decodeJSONPreservingNumbers(bodyBytes, &parsedData); err != nil {
			return nil, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid JSON")
		}
	case "XML":
		parsed, err := importer.ParseXML(strings.NewReader(string(bodyBytes)))
		if err != nil {
			return nil, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid XML")
		}
		parsedData = parsed
		bodyBytes, err = json.Marshal(parsedData)
		if err != nil {
			return nil, common.Internal("failed to serialize parsed XML", err)
		}
	default:
		return nil, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "unsupported format")
	}

	// Validate or extend model schema
	if err := h.validateOrExtend(ctx, modelStore, desc, parsedData); err != nil {
		return nil, classifyValidateOrExtendErr(err)
	}

	// Begin transaction
	txID, txCtx, err := h.txMgr.Begin(ctx)
	if err != nil {
		return nil, common.Internal("failed to begin transaction", err)
	}

	entityID := uuid.UUID(h.uuids.NewTimeUUID())
	now := time.Now()

	entity := &spi.Entity{
		Meta: spi.EntityMeta{
			ID:                      entityID.String(),
			TenantID:                uc.Tenant.ID,
			ModelRef:                ref,
			State:                   "",
			CreationDate:            now,
			LastModifiedDate:        now,
			TransactionID:           txID,
			TransitionForLatestSave: "",
			ChangeType:              "CREATED",
			ChangeUser:              uc.UserID,
		},
		Data: bodyBytes,
	}

	// Run workflow engine within transaction context.
	result, err := h.engine.Execute(txCtx, entity, "")
	if err != nil {
		h.txMgr.Rollback(txCtx, txID)
		slog.Error("workflow execution failed", "error", err.Error(), "entityId", entity.Meta.ID)
		return nil, classifyWorkflowError(err)
	}

	// If no workflow was found, engine returns forced success and entity state stays empty.
	// Set a default state.
	if entity.Meta.State == "" {
		entity.Meta.State = "CREATED"
	}

	// The CREATE path runs the workflow engine without an explicit
	// client-supplied transition name (Execute(..., "")). From the caller's
	// viewpoint this is a save without a named transition — the canonical
	// marker for that is "loopback", not the literal "workflow" (issue #94).
	if result != nil && result.StopReason == "" {
		entity.Meta.TransitionForLatestSave = "loopback"
	}

	// Save entity within transaction (goes to buffer).
	entityStore, err := h.factory.EntityStore(txCtx)
	if err != nil {
		h.txMgr.Rollback(txCtx, txID)
		return nil, common.Internal("failed to access entity store", err)
	}
	if _, err := entityStore.Save(txCtx, entity); err != nil {
		h.txMgr.Rollback(txCtx, txID)
		return nil, common.Internal("failed to save entity", err)
	}

	// Commit transaction.
	if err := h.txMgr.Commit(txCtx, txID); err != nil {
		if errors.Is(err, spi.ErrConflict) {
			return nil, common.Operational(http.StatusConflict, common.ErrCodeConflict, "transaction conflict — retry").AsRetryable()
		}
		return nil, common.Internal("failed to commit transaction", err)
	}

	return &EntityTransactionResult{
		TransactionID: txID,
		EntityIDs:     []string{entityID.String()},
	}, nil
}

// getEntityByTransactionID returns the entity version whose meta.TransactionID
// matches txID. It scans the version history (which carries the full Entity
// payload per version) and returns the matching snapshot. spi.ErrNotFound is
// returned both when the entity itself is unknown to the store and when no
// version matches the supplied transactionId — the caller maps both to
// ENTITY_NOT_FOUND (404), which mirrors Cyoda Cloud's contract for issue #150
// (and matches dictionary scenario 12/neg/05). The caller treats other errors
// as infrastructure failures (5xx).
func getEntityByTransactionID(ctx context.Context, store spi.EntityStore, entityID, txID string) (*spi.Entity, error) {
	versions, err := store.GetVersionHistory(ctx, entityID)
	if err != nil {
		return nil, err
	}
	for _, v := range versions {
		if v.Entity == nil {
			continue
		}
		if v.Entity.Meta.TransactionID == txID {
			return v.Entity, nil
		}
	}
	return nil, spi.ErrNotFound
}

// GetEntity retrieves a single entity, optionally at a point in time or
// scoped to a specific transaction. Exactly one of input.PointInTime and
// input.TransactionID may be set; the handler enforces mutual exclusion at
// the request boundary.
func (h *Handler) GetEntity(ctx context.Context, input GetOneEntityInput) (*EntityEnvelope, error) {
	entityStore, err := h.factory.EntityStore(ctx)
	if err != nil {
		return nil, common.Internal("failed to access entity store", err)
	}

	var ent *spi.Entity
	switch {
	case input.TransactionID != "":
		ent, err = getEntityByTransactionID(ctx, entityStore, input.EntityID, input.TransactionID)
	case input.PointInTime != nil:
		ent, err = entityStore.GetAsAt(ctx, input.EntityID, *input.PointInTime)
	default:
		ent, err = entityStore.Get(ctx, input.EntityID)
	}
	if err != nil {
		if errors.Is(err, spi.ErrNotFound) {
			appErr := common.Operational(http.StatusNotFound, common.ErrCodeEntityNotFound, fmt.Sprintf("entity id=%s not found", input.EntityID))
			appErr.Props = map[string]any{
				"entityId": input.EntityID,
			}
			return nil, appErr
		}
		return nil, common.Internal("failed to retrieve entity", err)
	}

	// Parse entity data to any for response
	var data any
	if err := decodeJSONPreservingNumbers(ent.Data, &data); err != nil {
		return nil, common.Internal("failed to parse entity data", err)
	}

	// Parse model version to int
	versionInt, _ := strconv.Atoi(ent.Meta.ModelRef.ModelVersion)

	meta := map[string]any{
		"id": ent.Meta.ID,
		"modelKey": map[string]any{
			"name":    ent.Meta.ModelRef.EntityName,
			"version": versionInt,
		},
		"state":          ent.Meta.State,
		"creationDate":   ent.Meta.CreationDate.UTC().Format(time.RFC3339Nano),
		"lastUpdateTime": ent.Meta.LastModifiedDate.UTC().Format(time.RFC3339Nano),
		"transactionId":  ent.Meta.TransactionID,
	}
	if ent.Meta.TransitionForLatestSave != "" {
		meta["transitionForLatestSave"] = ent.Meta.TransitionForLatestSave
	}

	return &EntityEnvelope{
		Type: "ENTITY",
		Data: data,
		Meta: meta,
	}, nil
}

// GetStatistics retrieves entity count statistics for all models.
func (h *Handler) GetStatistics(ctx context.Context) ([]EntityStat, error) {
	modelStore, err := h.factory.ModelStore(ctx)
	if err != nil {
		return nil, common.Internal("failed to access model store", err)
	}

	entityStore, err := h.factory.EntityStore(ctx)
	if err != nil {
		return nil, common.Internal("failed to access entity store", err)
	}

	refs, err := modelStore.GetAll(ctx)
	if err != nil {
		return nil, common.Internal("failed to list models", err)
	}

	result := make([]EntityStat, 0, len(refs))
	for _, ref := range refs {
		count, err := entityStore.Count(ctx, ref)
		if err != nil {
			return nil, common.Internal("failed to count entities", err)
		}
		result = append(result, EntityStat{
			ModelName:    ref.EntityName,
			ModelVersion: ref.ModelVersion,
			Count:        count,
		})
	}

	return result, nil
}

// EntityStat holds entity count for a model.
type EntityStat struct {
	ModelName    string
	ModelVersion string
	Count        int64
}

// EntityStatByState holds entity count for a model and state.
type EntityStatByState struct {
	ModelName    string
	ModelVersion string
	State        string
	Count        int64
}

// GetStatisticsByState retrieves entity count statistics by state for all models.
//
// Known limitation (follow-up): this still iterates every model definition and
// issues one CountByState call per model. For tenants with many models, the
// per-model fan-out is the next pressure point now that the per-entity loading
// bottleneck is gone. Possible directions for a follow-up: a batched
// CountByStateAll SPI method, or bounded parallelism over models.
func (h *Handler) GetStatisticsByState(ctx context.Context, states *[]string) ([]EntityStatByState, error) {
	modelStore, err := h.factory.ModelStore(ctx)
	if err != nil {
		return nil, common.Internal("failed to access model store", err)
	}

	entityStore, err := h.factory.EntityStore(ctx)
	if err != nil {
		return nil, common.Internal("failed to access entity store", err)
	}

	refs, err := modelStore.GetAll(ctx)
	if err != nil {
		return nil, common.Internal("failed to list models", err)
	}

	// Dereference the optional filter. Distinguish nil-pointer (no filter)
	// from pointer-to-empty-slice — per the SPI contract, the latter yields
	// an empty map without a storage call.
	var filterStates []string
	if states != nil {
		filterStates = *states
	}

	result := make([]EntityStatByState, 0)
	for _, ref := range refs {
		counts, err := entityStore.CountByState(ctx, ref, filterStates)
		if err != nil {
			return nil, common.Internal("failed to count entities by state", err)
		}
		for state, count := range counts {
			result = append(result, EntityStatByState{
				ModelName:    ref.EntityName,
				ModelVersion: ref.ModelVersion,
				State:        state,
				Count:        count,
			})
		}
	}

	return result, nil
}

// GetStatisticsByStateForModel retrieves entity count statistics by state for a specific model.
func (h *Handler) GetStatisticsByStateForModel(ctx context.Context, entityName string, modelVersion string, states *[]string) ([]EntityStatByState, error) {
	entityStore, err := h.factory.EntityStore(ctx)
	if err != nil {
		return nil, common.Internal("failed to access entity store", err)
	}

	ref := spi.ModelRef{
		EntityName:   entityName,
		ModelVersion: modelVersion,
	}

	// Dereference the optional filter. Distinguish nil-pointer (no filter)
	// from pointer-to-empty-slice — per the SPI contract, the latter yields
	// an empty map without a storage call.
	var filterStates []string
	if states != nil {
		filterStates = *states
	}

	counts, err := entityStore.CountByState(ctx, ref, filterStates)
	if err != nil {
		return nil, common.Internal("failed to count entities by state", err)
	}

	result := make([]EntityStatByState, 0, len(counts))
	for state, count := range counts {
		result = append(result, EntityStatByState{
			ModelName:    entityName,
			ModelVersion: modelVersion,
			State:        state,
			Count:        count,
		})
	}

	return result, nil
}

// GetStatisticsForModel retrieves entity count statistics for a specific model.
func (h *Handler) GetStatisticsForModel(ctx context.Context, entityName string, modelVersion string) (*EntityStat, error) {
	entityStore, err := h.factory.EntityStore(ctx)
	if err != nil {
		return nil, common.Internal("failed to access entity store", err)
	}

	ref := spi.ModelRef{
		EntityName:   entityName,
		ModelVersion: modelVersion,
	}

	count, err := entityStore.Count(ctx, ref)
	if err != nil {
		return nil, common.Internal("failed to count entities", err)
	}

	return &EntityStat{
		ModelName:    entityName,
		ModelVersion: modelVersion,
		Count:        count,
	}, nil
}

// DeleteEntity deletes a single entity by ID within a transaction.
// Returns the deleted entity's metadata for the response.
func (h *Handler) DeleteEntity(ctx context.Context, entityID string) (*deleteEntityResult, error) {
	// Begin transaction.
	txID, txCtx, err := h.txMgr.Begin(ctx)
	if err != nil {
		return nil, common.Internal("failed to begin transaction", err)
	}

	entityStore, err := h.factory.EntityStore(txCtx)
	if err != nil {
		h.txMgr.Rollback(txCtx, txID)
		return nil, common.Internal("failed to access entity store", err)
	}

	// Load entity before deleting to get ModelRef for response (adds to read set).
	entity, err := entityStore.Get(txCtx, entityID)
	if err != nil {
		h.txMgr.Rollback(txCtx, txID)
		appErr := common.Operational(http.StatusNotFound, common.ErrCodeEntityNotFound, fmt.Sprintf("entity id=%s not found", entityID))
		appErr.Props = map[string]any{
			"entityId": entityID,
		}
		return nil, appErr
	}

	// Soft delete within transaction.
	if err := entityStore.Delete(txCtx, entityID); err != nil {
		h.txMgr.Rollback(txCtx, txID)
		return nil, common.Internal("failed to delete entity", err)
	}

	// Commit transaction.
	if err := h.txMgr.Commit(txCtx, txID); err != nil {
		if errors.Is(err, spi.ErrConflict) {
			return nil, common.Operational(http.StatusConflict, common.ErrCodeConflict, "transaction conflict — retry").AsRetryable()
		}
		return nil, common.Internal("failed to commit transaction", err)
	}

	ver, _ := strconv.Atoi(entity.Meta.ModelRef.ModelVersion)
	return &deleteEntityResult{
		EntityID:      entityID,
		ModelName:     entity.Meta.ModelRef.EntityName,
		ModelVersion:  ver,
		TransactionID: txID,
	}, nil
}

type deleteEntityResult struct {
	EntityID      string
	ModelName     string
	ModelVersion  int
	TransactionID string
}

// GetChangesMetadata retrieves version history metadata for an entity.
//
// If pointInTime is non-nil, the result is truncated to versions whose
// Timestamp is at or before pointInTime — the caller sees the change
// history exactly as it would have appeared at that moment. A nil
// pointInTime returns the full history.
func (h *Handler) GetChangesMetadata(ctx context.Context, entityID string, pointInTime *time.Time) ([]EntityChangeEntry, error) {
	entityStore, err := h.factory.EntityStore(ctx)
	if err != nil {
		return nil, common.Internal("failed to access entity store", err)
	}

	versions, err := entityStore.GetVersionHistory(ctx, entityID)
	if err != nil {
		if errors.Is(err, spi.ErrNotFound) {
			appErr := common.Operational(http.StatusNotFound, common.ErrCodeEntityNotFound, fmt.Sprintf("entity id=%s not found", entityID))
			appErr.Props = map[string]any{
				"entityId": entityID,
			}
			return nil, appErr
		}
		return nil, common.Internal("failed to get version history", err)
	}

	// Truncate to versions at-or-before pointInTime when set.
	if pointInTime != nil && !pointInTime.IsZero() {
		cutoff := *pointInTime
		filtered := versions[:0]
		for _, v := range versions {
			if !v.Timestamp.After(cutoff) {
				filtered = append(filtered, v)
			}
		}
		versions = filtered
	}

	// Sort newest first (descending by timestamp)
	sort.Slice(versions, func(i, j int) bool {
		return versions[i].Timestamp.After(versions[j].Timestamp)
	})

	// Hard cap to prevent unbounded response size.
	const maxChangesMetadata = 1000
	if len(versions) > maxChangesMetadata {
		versions = versions[:maxChangesMetadata]
	}

	result := make([]EntityChangeEntry, 0, len(versions))
	for _, v := range versions {
		entry := EntityChangeEntry{
			ChangeType:   v.ChangeType,
			TimeOfChange: v.Timestamp.UTC().Format(time.RFC3339Nano),
			User:         v.User,
			HasEntity:    v.Entity != nil,
		}
		if v.Entity != nil {
			entry.TransactionID = v.Entity.Meta.TransactionID
		}
		result = append(result, entry)
	}

	return result, nil
}

// DeleteAllEntities deletes all entities for a model within a transaction.
func (h *Handler) DeleteAllEntities(ctx context.Context, entityName string, modelVersion string) (*DeleteAllResult, error) {
	ref := spi.ModelRef{
		EntityName:   entityName,
		ModelVersion: modelVersion,
	}

	// Begin transaction.
	txID, txCtx, err := h.txMgr.Begin(ctx)
	if err != nil {
		return nil, common.Internal("failed to begin transaction", err)
	}

	entityStore, err := h.factory.EntityStore(txCtx)
	if err != nil {
		h.txMgr.Rollback(txCtx, txID)
		return nil, common.Internal("failed to access entity store", err)
	}

	// The model must exist; a request to delete all entities of a model
	// that was never registered is a 404. But a registered model with
	// zero entities is a successful no-op (idempotent delete-before-
	// recreate flows depend on this).
	modelStore, err := h.factory.ModelStore(txCtx)
	if err != nil {
		h.txMgr.Rollback(txCtx, txID)
		return nil, common.Internal("failed to access model store", err)
	}
	if _, err := modelStore.Get(txCtx, ref); err != nil {
		h.txMgr.Rollback(txCtx, txID)
		if errors.Is(err, spi.ErrNotFound) {
			return nil, common.Operational(404, common.ErrCodeModelNotFound,
				fmt.Sprintf("cannot find model entityName=%s, version=%s", entityName, modelVersion))
		}
		return nil, common.Internal("failed to load model", err)
	}

	// Get all entities before deleting (for verbose response and IDs).
	entities, err := entityStore.GetAll(txCtx, ref)
	if err != nil {
		h.txMgr.Rollback(txCtx, txID)
		return nil, common.Internal("failed to get entities", err)
	}

	if err := entityStore.DeleteAll(txCtx, ref); err != nil {
		h.txMgr.Rollback(txCtx, txID)
		return nil, common.Internal("failed to delete entities", err)
	}

	// Commit transaction.
	if err := h.txMgr.Commit(txCtx, txID); err != nil {
		if errors.Is(err, spi.ErrConflict) {
			return nil, common.Operational(http.StatusConflict, common.ErrCodeConflict, "transaction conflict — retry").AsRetryable()
		}
		return nil, common.Internal("failed to commit transaction", err)
	}

	modelID := deterministicModelID(ref)
	return &DeleteAllResult{
		TotalCount:    len(entities),
		ModelID:       modelID.String(),
		EntityModelID: modelID.String(),
	}, nil
}

// ListEntities retrieves all entities for a model with pagination.
func (h *Handler) ListEntities(ctx context.Context, entityName string, modelVersion string, page PaginationParams) ([]EntityEnvelope, error) {
	// Defense-in-depth: HTTP and gRPC handlers SHOULD validate before
	// reaching the service, but enforce the same caps here so the
	// `start := int(PageNumber * PageSize)` multiplication below cannot
	// be reached with attacker-supplied values that overflow on 32-bit
	// platforms or yield negative slice indices.
	if appErr := pagination.ValidateOffset(int64(page.PageNumber), int64(page.PageSize)); appErr != nil {
		return nil, appErr
	}

	entityStore, err := h.factory.EntityStore(ctx)
	if err != nil {
		return nil, common.Internal("failed to access entity store", err)
	}

	ref := spi.ModelRef{
		EntityName:   entityName,
		ModelVersion: modelVersion,
	}

	entities, err := entityStore.GetAll(ctx, ref)
	if err != nil {
		return nil, common.Internal("failed to get entities", err)
	}

	// Sort by entity ID for deterministic pagination
	sort.Slice(entities, func(i, j int) bool {
		return entities[i].Meta.ID < entities[j].Meta.ID
	})

	// Apply pagination — caps above guarantee start/end are non-negative
	// and within int range.
	start := int(page.PageNumber) * int(page.PageSize)
	if start > len(entities) {
		start = len(entities)
	}
	end := start + int(page.PageSize)
	if end > len(entities) {
		end = len(entities)
	}
	pageSlice := entities[start:end]

	// Build envelopes without modelKey in meta
	result := make([]EntityEnvelope, 0, len(pageSlice))
	for _, ent := range pageSlice {
		var data any
		if err := decodeJSONPreservingNumbers(ent.Data, &data); err != nil {
			return nil, common.Internal("failed to parse entity data", err)
		}

		entMeta := map[string]any{
			"id":             ent.Meta.ID,
			"state":          ent.Meta.State,
			"creationDate":   ent.Meta.CreationDate.UTC().Format(time.RFC3339Nano),
			"lastUpdateTime": ent.Meta.LastModifiedDate.UTC().Format(time.RFC3339Nano),
			"transactionId":  ent.Meta.TransactionID,
		}
		if ent.Meta.TransitionForLatestSave != "" {
			entMeta["transitionForLatestSave"] = ent.Meta.TransitionForLatestSave
		}

		result = append(result, EntityEnvelope{
			Type: "ENTITY",
			Data: data,
			Meta: entMeta,
		})
	}

	return result, nil
}

// CreateEntityCollection creates multiple entities in a single transaction.
func (h *Handler) CreateEntityCollection(ctx context.Context, items []CollectionItem) (*EntityTransactionResult, error) {
	uc := spi.MustGetUserContext(ctx)

	modelStore, err := h.factory.ModelStore(ctx)
	if err != nil {
		return nil, common.Internal("failed to access model store", err)
	}

	// Validate all items before starting the transaction.
	type parsedItem struct {
		ref          spi.ModelRef
		payloadBytes []byte
	}
	parsed := make([]parsedItem, 0, len(items))
	for i, item := range items {
		ref := spi.ModelRef{
			EntityName:   item.ModelName,
			ModelVersion: fmt.Sprintf("%d", item.ModelVersion),
		}

		// Load and validate model. Distinguish genuine not-found (404) from
		// other infrastructure errors (5xx) so a schema fold/apply failure
		// does not masquerade as a missing model for a batch item.
		desc, err := modelStore.Get(ctx, ref)
		if err != nil {
			if errors.Is(err, spi.ErrNotFound) {
				return nil, common.Operational(http.StatusNotFound, common.ErrCodeModelNotFound,
					fmt.Sprintf("item %d: model not found", i))
			}
			return nil, common.Internal(fmt.Sprintf("item %d: failed to load model", i), err)
		}
		if desc.State != spi.ModelLocked {
			return nil, common.Operational(http.StatusConflict, common.ErrCodeModelNotLocked,
				fmt.Sprintf("item %d: model is not locked", i))
		}

		// Parse payload
		var parsedData any
		payloadBytes := []byte(item.Payload)
		if err := decodeJSONPreservingNumbers(payloadBytes, &parsedData); err != nil {
			return nil, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest,
				fmt.Sprintf("item %d: invalid JSON payload", i))
		}

		// Validate or extend model schema
		if err := h.validateOrExtend(ctx, modelStore, desc, parsedData); err != nil {
			return nil, classifyValidateOrExtendErr(err)
		}

		parsed = append(parsed, parsedItem{ref: ref, payloadBytes: payloadBytes})
	}

	// Begin transaction -- all entities in one transaction.
	txID, txCtx, err := h.txMgr.Begin(ctx)
	if err != nil {
		return nil, common.Internal("failed to begin transaction", err)
	}

	entityStore, err := h.factory.EntityStore(txCtx)
	if err != nil {
		h.txMgr.Rollback(txCtx, txID)
		return nil, common.Internal("failed to access entity store", err)
	}

	now := time.Now()

	// Pre-generate entity IDs so the iterator has no side effects.
	// This decouples ID generation from SaveAll's consumption pattern,
	// making it safe even if a future SaveAll consumes from multiple goroutines.
	entityIDs := make([]string, len(parsed))
	for i := range parsed {
		entityIDs[i] = uuid.UUID(h.uuids.NewTimeUUID()).String()
	}

	entities := func(yield func(*spi.Entity) bool) {
		for i, item := range parsed {
			entity := &spi.Entity{
				Meta: spi.EntityMeta{
					ID:               entityIDs[i],
					TenantID:         uc.Tenant.ID,
					ModelRef:         item.ref,
					State:            "CREATED",
					CreationDate:     now,
					LastModifiedDate: now,
					TransactionID:    txID,
					ChangeType:       "CREATED",
					ChangeUser:       uc.UserID,
				},
				Data: item.payloadBytes,
			}
			if !yield(entity) {
				return
			}
		}
	}

	if _, err := entityStore.SaveAll(txCtx, entities); err != nil {
		h.txMgr.Rollback(txCtx, txID)
		return nil, common.Internal("failed to save entities", err)
	}

	// Commit transaction.
	if err := h.txMgr.Commit(txCtx, txID); err != nil {
		if errors.Is(err, spi.ErrConflict) {
			return nil, common.Operational(http.StatusConflict, common.ErrCodeConflict, "transaction conflict — retry").AsRetryable()
		}
		return nil, common.Internal("failed to commit transaction", err)
	}

	return &EntityTransactionResult{
		TransactionID: txID,
		EntityIDs:     entityIDs,
	}, nil
}

// UpdateEntity updates a single entity with an optional named transition or loopback.
func (h *Handler) UpdateEntity(ctx context.Context, input UpdateEntityInput) (*EntityTransactionResult, error) {
	modelStore, err := h.factory.ModelStore(ctx)
	if err != nil {
		return nil, common.Internal("failed to access model store", err)
	}

	// Parse body based on format
	bodyBytes := []byte(input.Data)
	var parsedData any
	switch input.Format {
	case "JSON":
		if err := decodeJSONPreservingNumbers(bodyBytes, &parsedData); err != nil {
			return nil, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid JSON")
		}
	case "XML":
		parsed, err := importer.ParseXML(strings.NewReader(string(bodyBytes)))
		if err != nil {
			return nil, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid XML")
		}
		parsedData = parsed
		bodyBytes, err = json.Marshal(parsedData)
		if err != nil {
			return nil, common.Internal("failed to serialize parsed XML", err)
		}
	default:
		return nil, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "unsupported format")
	}

	// Begin transaction.
	txID, txCtx, err := h.txMgr.Begin(ctx)
	if err != nil {
		return nil, common.Internal("failed to begin transaction", err)
	}

	// Load existing entity within transaction (adds to read set).
	entityStore, err := h.factory.EntityStore(txCtx)
	if err != nil {
		h.txMgr.Rollback(txCtx, txID)
		return nil, common.Internal("failed to access entity store", err)
	}

	existing, err := entityStore.Get(txCtx, input.EntityID)
	if err != nil {
		h.txMgr.Rollback(txCtx, txID)
		return nil, common.Operational(http.StatusNotFound, common.ErrCodeEntityNotFound, "entity not found")
	}

	// Load model descriptor
	desc, err := modelStore.Get(txCtx, existing.Meta.ModelRef)
	if err != nil {
		h.txMgr.Rollback(txCtx, txID)
		return nil, common.Internal("failed to load model for entity", err)
	}

	// Validate or extend model schema
	if err := h.validateOrExtend(txCtx, modelStore, desc, parsedData); err != nil {
		h.txMgr.Rollback(txCtx, txID)
		return nil, classifyValidateOrExtendErr(err)
	}

	now := time.Now()

	uc := spi.GetUserContext(ctx)
	changeUser := ""
	if uc != nil {
		changeUser = uc.UserID
	}

	updated := &spi.Entity{
		Meta: spi.EntityMeta{
			ID:                      existing.Meta.ID,
			TenantID:                existing.Meta.TenantID,
			ModelRef:                existing.Meta.ModelRef,
			State:                   existing.Meta.State,
			Version:                 existing.Meta.Version,
			CreationDate:            existing.Meta.CreationDate,
			LastModifiedDate:        now,
			TransactionID:           txID,
			ChangeType:              "UPDATED",
			ChangeUser:              changeUser,
			TransitionForLatestSave: input.Transition,
		},
		Data: bodyBytes,
	}

	// Execute workflow: loopback or named manual transition. Each variant
	// returns FinalCtx/FinalTxID — for cascades that segment via
	// COMMIT_BEFORE_DISPATCH this is TX_post (a fresh TX opened by the engine
	// after committing TX_pre); for non-segmenting cascades it equals the
	// handler's input txID.
	//
	// We hand input.IfMatch to the engine via the *WithIfMatch entry-points so
	// that, when a COMMIT_BEFORE_DISPATCH segment exists, the precondition is
	// applied at the first-segment flush — strictly BEFORE any external
	// dispatch fires (spec §4.1). The engine consumes IfMatch only on
	// segmenting cascades; for non-segmenting cascades the handler still
	// applies CompareAndSave-with-IfMatch post-engine below.
	var engineResult *wfengine.EngineResult
	if input.Transition == "" {
		res, lbErr := h.engine.LoopbackWithIfMatch(txCtx, updated, input.IfMatch)
		if lbErr != nil {
			h.txMgr.Rollback(txCtx, txID)
			slog.Error("workflow loopback failed", "error", lbErr.Error(), "entityId", updated.Meta.ID)
			if errors.Is(lbErr, spi.ErrConflict) {
				appErr := common.Operational(
					http.StatusPreconditionFailed,
					common.ErrCodeEntityModified,
					"entity has been modified since last read")
				appErr.Props = map[string]any{"entityId": input.EntityID}
				return nil, appErr
			}
			return nil, classifyWorkflowError(lbErr)
		}
		updated.Meta.TransitionForLatestSave = "loopback"
		engineResult = res
	} else {
		res, mtErr := h.engine.ManualTransitionWithIfMatch(txCtx, updated, input.Transition, input.IfMatch)
		if mtErr != nil {
			h.txMgr.Rollback(txCtx, txID)
			slog.Error("workflow manual transition failed", "error", mtErr.Error(), "entityId", updated.Meta.ID, "transition", input.Transition)
			if errors.Is(mtErr, spi.ErrConflict) {
				appErr := common.Operational(
					http.StatusPreconditionFailed,
					common.ErrCodeEntityModified,
					"entity has been modified since last read")
				appErr.Props = map[string]any{"entityId": input.EntityID}
				return nil, appErr
			}
			return nil, classifyWorkflowError(mtErr)
		}
		updated.Meta.TransitionForLatestSave = input.Transition
		engineResult = res
	}

	finalCtx, finalTxID := engineResult.FinalCtx, engineResult.FinalTxID
	finalEntityStore, err := h.factory.EntityStore(finalCtx)
	if err != nil {
		_ = h.txMgr.Rollback(finalCtx, finalTxID)
		return nil, common.Internal("failed to access entity store", err)
	}

	// Distinguish: did the engine segment (and therefore consume IfMatch)?
	// FinalTxID != txID iff at least one COMMIT_BEFORE_DISPATCH segment
	// committed; the engine's first-segment flush already applied the
	// caller's IfMatch. For non-segmenting cascades (FinalTxID == txID) the
	// handler still owns the IfMatch precondition.
	segmented := finalTxID != txID

	if input.IfMatch != "" && !segmented {
		if _, err := finalEntityStore.CompareAndSave(finalCtx, updated, input.IfMatch); err != nil {
			_ = h.txMgr.Rollback(finalCtx, finalTxID)
			if errors.Is(err, spi.ErrConflict) {
				appErr := common.Operational(
					http.StatusPreconditionFailed,
					common.ErrCodeEntityModified,
					"entity has been modified since last read")
				appErr.Props = map[string]any{"entityId": input.EntityID}
				return nil, appErr
			}
			return nil, common.Internal("failed to save entity", err)
		}
	} else {
		// Plain Save: either no IfMatch was provided, or the engine already
		// consumed it at first-segment flush (segmented == true). In the
		// segmented case the row's current TransactionID has advanced through
		// TX_pre's commit, so a handler-side CAS against input.IfMatch would
		// fail spuriously — Save lands the post-cascade state in TX_post's
		// buffer and the segment's own intra-TX guards handle concurrency.
		if _, err := finalEntityStore.Save(finalCtx, updated); err != nil {
			_ = h.txMgr.Rollback(finalCtx, finalTxID)
			return nil, common.Internal("failed to save entity", err)
		}
	}

	// Commit FinalTxID — the still-open TX after the cascade. For
	// non-segmenting cascades this is the handler's original txID; for
	// segmenting cascades this is TX_post (TX_pre was committed by the
	// engine before the external callout).
	if err := h.txMgr.Commit(finalCtx, finalTxID); err != nil {
		if errors.Is(err, spi.ErrConflict) {
			return nil, common.Operational(http.StatusConflict, common.ErrCodeConflict, "transaction conflict — retry").AsRetryable()
		}
		return nil, common.Internal("failed to commit transaction", err)
	}

	// Surface the cascade-entry txID to the caller for client correlation
	// (spec §8): even when the engine segmented and TX_post is what carried
	// the durable apply-result, the response advertises the original entry
	// txID so audit lookups via /audit/entity/{id}/workflow/{txId}/finished
	// continue to work.
	return &EntityTransactionResult{
		TransactionID: txID,
		EntityIDs:     []string{input.EntityID},
	}, nil
}

// UpdateEntityCollection updates multiple entities in a single transaction
// (PUT /api/entity/{format}). Per the documented contract: if any entity
// in the collection is missing (or any step fails), the entire batch is
// rolled back and no entity is modified. Loopback updates (empty
// Transition) and named-transition updates may be mixed within the same
// batch. Issue #92.
func (h *Handler) UpdateEntityCollection(ctx context.Context, items []UpdateCollectionItem) (*EntityTransactionResult, error) {
	if len(items) == 0 {
		return nil, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "collection is empty")
	}

	modelStore, err := h.factory.ModelStore(ctx)
	if err != nil {
		return nil, common.Internal("failed to access model store", err)
	}

	// Parse all payloads up-front — fail fast on malformed JSON before
	// starting the transaction so a bad item doesn't leave a partially
	// locked batch in flight.
	type parsedItem struct {
		id         string
		transition string
		bodyBytes  []byte
		parsedData any
	}
	parsed := make([]parsedItem, 0, len(items))
	for i, item := range items {
		if item.EntityID == "" {
			return nil, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest,
				fmt.Sprintf("item %d: missing id", i))
		}
		var data any
		if err := decodeJSONPreservingNumbers([]byte(item.Payload), &data); err != nil {
			return nil, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest,
				fmt.Sprintf("item %d: invalid JSON payload", i))
		}
		parsed = append(parsed, parsedItem{
			id:         item.EntityID,
			transition: item.Transition,
			bodyBytes:  []byte(item.Payload),
			parsedData: data,
		})
	}

	// Begin transaction — all items in one transaction, all-or-nothing.
	txID, txCtx, err := h.txMgr.Begin(ctx)
	if err != nil {
		return nil, common.Internal("failed to begin transaction", err)
	}

	entityStore, err := h.factory.EntityStore(txCtx)
	if err != nil {
		h.txMgr.Rollback(txCtx, txID)
		return nil, common.Internal("failed to access entity store", err)
	}

	now := time.Now()
	uc := spi.GetUserContext(ctx)
	changeUser := ""
	if uc != nil {
		changeUser = uc.UserID
	}

	entityIDs := make([]string, 0, len(parsed))

	for i, item := range parsed {
		existing, err := entityStore.Get(txCtx, item.id)
		if err != nil {
			h.txMgr.Rollback(txCtx, txID)
			return nil, common.Operational(http.StatusNotFound, common.ErrCodeEntityNotFound,
				fmt.Sprintf("item %d: entity %s not found", i, item.id))
		}

		desc, err := modelStore.Get(txCtx, existing.Meta.ModelRef)
		if err != nil {
			h.txMgr.Rollback(txCtx, txID)
			return nil, common.Internal(fmt.Sprintf("item %d: failed to load model for entity", i), err)
		}

		if err := h.validateOrExtend(txCtx, modelStore, desc, item.parsedData); err != nil {
			h.txMgr.Rollback(txCtx, txID)
			return nil, classifyValidateOrExtendErr(err)
		}

		updated := &spi.Entity{
			Meta: spi.EntityMeta{
				ID:               existing.Meta.ID,
				TenantID:         existing.Meta.TenantID,
				ModelRef:         existing.Meta.ModelRef,
				State:            existing.Meta.State,
				Version:          existing.Meta.Version,
				CreationDate:     existing.Meta.CreationDate,
				LastModifiedDate: now,
				TransactionID:    txID,
				ChangeType:       "UPDATED",
				ChangeUser:       changeUser,
			},
			Data: item.bodyBytes,
		}

		if item.transition == "" {
			if _, err := h.engine.Loopback(txCtx, updated); err != nil {
				h.txMgr.Rollback(txCtx, txID)
				slog.Error("workflow loopback failed", "error", err.Error(), "entityId", updated.Meta.ID)
				return nil, classifyWorkflowError(fmt.Errorf("item %d: %w", i, err))
			}
			updated.Meta.TransitionForLatestSave = "loopback"
		} else {
			if _, err := h.engine.ManualTransition(txCtx, updated, item.transition); err != nil {
				h.txMgr.Rollback(txCtx, txID)
				slog.Error("workflow manual transition failed", "error", err.Error(), "entityId", updated.Meta.ID, "transition", item.transition)
				return nil, classifyWorkflowError(fmt.Errorf("item %d: %w", i, err))
			}
			updated.Meta.TransitionForLatestSave = item.transition
		}

		if _, err := entityStore.Save(txCtx, updated); err != nil {
			h.txMgr.Rollback(txCtx, txID)
			return nil, common.Internal(fmt.Sprintf("item %d: failed to save entity", i), err)
		}
		entityIDs = append(entityIDs, updated.Meta.ID)
	}

	if err := h.txMgr.Commit(txCtx, txID); err != nil {
		if errors.Is(err, spi.ErrConflict) {
			return nil, common.Operational(http.StatusConflict, common.ErrCodeConflict, "transaction conflict — retry").AsRetryable()
		}
		return nil, common.Internal("failed to commit transaction", err)
	}

	return &EntityTransactionResult{
		TransactionID: txID,
		EntityIDs:     entityIDs,
	}, nil
}

// classifyError converts an error to an *common.AppError if it isn't already one.
func classifyError(err error) *common.AppError {
	var appErr *common.AppError
	if errors.As(err, &appErr) {
		return appErr
	}
	return common.Internal("unexpected error", err)
}

// classifyWorkflowError maps a workflow-engine error to the appropriate HTTP
// error code. The transition-not-found case (ErrTransitionNotFound sentinel)
// receives the specific TRANSITION_NOT_FOUND code; all other engine errors
// fall back to the generic WORKFLOW_FAILED code.
func classifyWorkflowError(err error) *common.AppError {
	if errors.Is(err, wfengine.ErrTransitionNotFound) {
		return common.Operational(http.StatusBadRequest, common.ErrCodeTransitionNotFound, err.Error())
	}
	return common.Operational(http.StatusBadRequest, common.ErrCodeWorkflowFailed, err.Error())
}
