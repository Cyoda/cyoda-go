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
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/importer"
	"github.com/cyoda-platform/cyoda-go/internal/domain/pagination"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
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

// updateOptions tunes the shared update flow. Zero value = plain replace (PUT).
type updateOptions struct {
	// merge, when non-nil, transforms the parsed request body against the
	// existing entity's stored data inside the transaction, before validation
	// (RFC 7386 for PATCH).
	merge func(existing json.RawMessage, incoming any) (any, error)
	// strictValidate forces validate-only — never extend the model schema (PATCH).
	strictValidate bool
}

// PatchEntityInput holds parameters for a partial (RFC 7386) entity update.
// IfMatch is required in some form (a transactionId, or "*" for unconditional);
// its absence is rejected as 428 at the HTTP/gRPC edge, not here.
type PatchEntityInput struct {
	EntityID    string
	Patch       json.RawMessage
	PatchFormat string // "MERGE_PATCH" | "JSON_PATCH"
	Transition  string
	IfMatch     string
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
// IfMatch is the optional cross-request optimistic-concurrency precondition
// (the entity's meta.transactionId from the caller's last read). When
// supplied, a per-item ENTITY_MODIFIED conflict is isolated within its chunk
// rather than rolling the whole chunk back. Issue #228.
type UpdateCollectionItem struct {
	EntityID   string
	Payload    json.RawMessage
	Transition string
	IfMatch    string
}

// UpdateCollectionItemFailure documents a single per-item failure that did
// NOT cause its chunk to roll back. Today this is reserved for ENTITY_MODIFIED
// conflicts on items carrying an IfMatch precondition; other per-item failures
// (missing entity, validation, non-conflict engine errors) still roll the
// entire chunk back. ItemIndex is the failing item's zero-based position
// within its chunk's request slice.
type UpdateCollectionItemFailure struct {
	EntityID  string
	Code      string
	Message   string
	ItemIndex int
}

// UpdateCollectionResult holds the result of a batch update. EntityIDs lists
// items that committed successfully within the chunk; Failed lists items that
// were isolated by per-item conflict handling. TransactionID is the
// cascade-entry txID (matches CreateEntityCollection's choice for
// segmenting-cascade audit correlation).
type UpdateCollectionResult struct {
	TransactionID string
	EntityIDs     []string
	Failed        []UpdateCollectionItemFailure
}

// --- Service methods ---

// withUniqueKeys attaches the model's unique-key definitions to ctx and
// pre-checks the input document before any transaction begins. If all
// fields of any declared key are present and well-formed the keyed ctx is
// returned; callers thread it into Begin/Execute/Save so the store can
// enforce uniqueness later (Phases 5-7).
//
// Returns (ctx, 422 INVALID_UNIQUE_KEY) when the document satisfies only
// some fields of a composite key (partial match). Returns (ctx, 5xx) for
// malformed JSON or other internal failures. Returns (ctx, nil) unchanged
// when the model has no unique keys or all keys are fully absent/null.
func (h *Handler) withUniqueKeys(ctx context.Context, desc *spi.ModelDescriptor, inputDoc []byte) (context.Context, error) {
	if len(desc.UniqueKeys) == 0 {
		return ctx, nil
	}
	if _, err := spi.ComputeClaims(desc.UniqueKeys, inputDoc); err != nil {
		if errors.Is(err, spi.ErrPartialUniqueKey) {
			return ctx, common.Operational(http.StatusUnprocessableEntity, common.ErrCodeInvalidUniqueKey, "composite unique key incomplete")
		}
		return ctx, common.Internal("failed to evaluate unique keys", err)
	}
	return spi.WithUniqueKeys(ctx, desc.UniqueKeys), nil
}

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

	// Pre-check unique keys and thread them onto ctx. This runs before Begin so
	// the keyed ctx propagates into Begin → txCtx → Execute → FinalCtx → Save.
	// Partial keys are rejected here (422) before any transaction is opened.
	ctx, err = h.withUniqueKeys(ctx, desc, bodyBytes)
	if err != nil {
		return nil, err
	}

	// Begin a fresh transaction, or PARTICIPATE in a joined tx already on ctx
	// (a routed compute-node callback — #287). A joined callback does not Begin
	// and does not commit; the owner does. Its whole body is one gated critical
	// section on the shared tx buffer (acquired below).
	txID, txCtx, owned, err := h.beginOrJoin(ctx)
	if err != nil {
		return nil, common.Internal("failed to begin transaction", err)
	}
	if !owned {
		var releaseGate func()
		txCtx, releaseGate = h.acquireJoinedGate(txCtx, txID)
		defer releaseGate()
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

	// Run workflow engine within transaction context. The engine returns
	// FinalCtx/FinalTxID — for cascades that segment via COMMIT_BEFORE_DISPATCH
	// this is TX_post (a fresh TX opened by the engine after committing
	// TX_pre); for non-segmenting cascades it equals the handler's input
	// txID. CreateEntity has no prior version, so no IfMatch is involved.
	result, err := h.engine.Execute(txCtx, entity, "")
	if err != nil {
		h.rollbackOwned(txCtx, txID, owned)
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

	finalCtx, finalTxID := result.FinalCtx, result.FinalTxID

	// A joined callback is a plain single-segment op; the engine must not have
	// advanced the segment for a participating call. If it did, our gate/commit
	// reasoning (owner commits finalTxID; callback joined txID) is broken.
	if !owned && finalTxID != txID {
		return nil, common.Internal("joined callback unexpectedly segmented transaction",
			fmt.Errorf("entry txID %s advanced to %s on a joined call", txID, finalTxID))
	}

	// Save entity within the engine's final-segment transaction (goes to
	// buffer). For non-segmenting cascades finalCtx/finalTxID equal the
	// handler's input; for segmenting cascades the engine has already
	// committed TX_pre and finalCtx/finalTxID address TX_post.
	entityStore, err := h.factory.EntityStore(finalCtx)
	if err != nil {
		h.rollbackOwned(finalCtx, finalTxID, owned)
		return nil, common.Internal("failed to access entity store", err)
	}

	// Finalize: for the OWNER, gate the final Save+Commit so an in-flight joined
	// callback's Save cannot race the owner's buffer mutation/commit (the SPI
	// delegates within-tx serialisation to the application). The joined path
	// already holds the gate for its whole body, so it must NOT re-acquire here.
	// The gate is NEVER held across engine.Execute (above) — that would deadlock
	// against callbacks that need the gate.
	if appErr := func() *common.AppError {
		if owned {
			defer h.gate.Acquire(finalTxID)()
		}
		if _, err := entityStore.Save(finalCtx, entity); err != nil {
			h.rollbackOwned(finalCtx, finalTxID, owned)
			return common.Internal("failed to save entity", err)
		}
		if err := h.commitOwned(finalCtx, finalTxID, owned); err != nil {
			if errors.Is(err, spi.ErrConflict) {
				return common.Operational(http.StatusConflict, common.ErrCodeConflict, "transaction conflict — retry").AsRetryable()
			}
			return common.Internal("failed to commit transaction", err)
		}
		return nil
	}(); appErr != nil {
		return nil, appErr
	}

	// Surface the cascade-entry txID for client correlation (spec §8) — even
	// when the engine segmented and TX_post carried the durable apply-result,
	// audit lookups via /audit/entity/{id}/workflow/{txId}/finished use the
	// entry txID.
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

	modelStore, err := h.factory.ModelStore(ctx)
	if err != nil {
		return nil, common.Internal("failed to access model store", err)
	}
	if appErr := common.EnsureModelRegistered(ctx, modelStore, ref); appErr != nil {
		return nil, appErr
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

	modelStore, err := h.factory.ModelStore(ctx)
	if err != nil {
		return nil, common.Internal("failed to access model store", err)
	}
	if appErr := common.EnsureModelRegistered(ctx, modelStore, ref); appErr != nil {
		return nil, appErr
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
	// Begin a fresh tx, or PARTICIPATE in a joined tx already on ctx (#287).
	txID, txCtx, owned, err := h.beginOrJoin(ctx)
	if err != nil {
		return nil, common.Internal("failed to begin transaction", err)
	}
	if !owned {
		var releaseGate func()
		txCtx, releaseGate = h.acquireJoinedGate(txCtx, txID)
		defer releaseGate()
	}

	entityStore, err := h.factory.EntityStore(txCtx)
	if err != nil {
		h.rollbackOwned(txCtx, txID, owned)
		return nil, common.Internal("failed to access entity store", err)
	}

	// Load entity before deleting to get ModelRef for response (adds to read set).
	entity, err := entityStore.Get(txCtx, entityID)
	if err != nil {
		h.rollbackOwned(txCtx, txID, owned)
		appErr := common.Operational(http.StatusNotFound, common.ErrCodeEntityNotFound, fmt.Sprintf("entity id=%s not found", entityID))
		appErr.Props = map[string]any{
			"entityId": entityID,
		}
		return nil, appErr
	}

	// Finalize: gate the OWNER's Delete+Commit against a concurrent joined
	// callback's buffer write; the joined path already holds the gate.
	if appErr := func() *common.AppError {
		if owned {
			defer h.gate.Acquire(txID)()
		}
		// Soft delete within transaction.
		if err := entityStore.Delete(txCtx, entityID); err != nil {
			h.rollbackOwned(txCtx, txID, owned)
			return common.Internal("failed to delete entity", err)
		}
		// Commit transaction (no-op when participating in a joined tx).
		if err := h.commitOwned(txCtx, txID, owned); err != nil {
			if errors.Is(err, spi.ErrConflict) {
				return common.Operational(http.StatusConflict, common.ErrCodeConflict, "transaction conflict — retry").AsRetryable()
			}
			return common.Internal("failed to commit transaction", err)
		}
		return nil
	}(); appErr != nil {
		return nil, appErr
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

	// Begin a fresh tx, or PARTICIPATE in a joined tx already on ctx (#287).
	txID, txCtx, owned, err := h.beginOrJoin(ctx)
	if err != nil {
		return nil, common.Internal("failed to begin transaction", err)
	}
	if !owned {
		var releaseGate func()
		txCtx, releaseGate = h.acquireJoinedGate(txCtx, txID)
		defer releaseGate()
	}

	entityStore, err := h.factory.EntityStore(txCtx)
	if err != nil {
		h.rollbackOwned(txCtx, txID, owned)
		return nil, common.Internal("failed to access entity store", err)
	}

	// The model must exist; a request to delete all entities of a model
	// that was never registered is a 404. But a registered model with
	// zero entities is a successful no-op (idempotent delete-before-
	// recreate flows depend on this).
	modelStore, err := h.factory.ModelStore(txCtx)
	if err != nil {
		h.rollbackOwned(txCtx, txID, owned)
		return nil, common.Internal("failed to access model store", err)
	}
	if _, err := modelStore.Get(txCtx, ref); err != nil {
		h.rollbackOwned(txCtx, txID, owned)
		if errors.Is(err, spi.ErrNotFound) {
			return nil, common.Operational(404, common.ErrCodeModelNotFound,
				fmt.Sprintf("cannot find model entityName=%s, version=%s", entityName, modelVersion))
		}
		return nil, common.Internal("failed to load model", err)
	}

	// Get all entities before deleting (for verbose response and IDs).
	entities, err := entityStore.GetAll(txCtx, ref)
	if err != nil {
		h.rollbackOwned(txCtx, txID, owned)
		return nil, common.Internal("failed to get entities", err)
	}

	// Finalize: gate the OWNER's DeleteAll+Commit against a concurrent joined
	// callback's buffer write; the joined path already holds the gate.
	if appErr := func() *common.AppError {
		if owned {
			defer h.gate.Acquire(txID)()
		}
		if err := entityStore.DeleteAll(txCtx, ref); err != nil {
			h.rollbackOwned(txCtx, txID, owned)
			return common.Internal("failed to delete entities", err)
		}
		// Commit transaction (no-op when participating in a joined tx).
		if err := h.commitOwned(txCtx, txID, owned); err != nil {
			if errors.Is(err, spi.ErrConflict) {
				return common.Operational(http.StatusConflict, common.ErrCodeConflict, "transaction conflict — retry").AsRetryable()
			}
			return common.Internal("failed to commit transaction", err)
		}
		return nil
	}(); appErr != nil {
		return nil, appErr
	}

	modelID := deterministicModelID(ref)
	return &DeleteAllResult{
		TotalCount:    len(entities),
		ModelID:       modelID.String(),
		EntityModelID: modelID.String(),
	}, nil
}

// DeleteResult reports a conditional delete: MatchedCount entities matched the
// condition (or all, for an empty body), RemovedCount were actually deleted,
// IDToError maps any per-id delete failures, and IDs lists the matched ids when
// verbose was requested.
type DeleteResult struct {
	EntityModelID string
	MatchedCount  int
	RemovedCount  int
	IDToError     map[string]string
	IDs           []string
}

// DeleteEntitiesConditional deletes entities of a model. An empty condBody
// deletes all (backward-compatible). A present condBody is parsed and only
// matching entities (as-at pointInTime, when supplied) are deleted — reusing
// the search condition primitive so no special engine rights are claimed
// (design §6.1). Selection and deletion run inside one transaction; because
// SearchService.Search bypasses backend pushdown when a tx is on the context,
// buffered writes are visible to the selection.
func (h *Handler) DeleteEntitiesConditional(ctx context.Context, entityName, modelVersion string, condBody []byte, pointInTime *time.Time, verbose bool) (*DeleteResult, error) {
	ref := spi.ModelRef{EntityName: entityName, ModelVersion: modelVersion}

	// Parse the condition (if any) BEFORE opening a tx — a parse error is a
	// 400 that must not start a transaction. Empty/whitespace body ⇒ delete-all.
	var cond predicate.Condition
	if len(bytes.TrimSpace(condBody)) > 0 {
		c, err := predicate.ParseCondition(condBody)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidCondition, err)
		}
		cond = c
	}

	// Delete-all fast path preserves existing behaviour + response shape.
	// IDs is always a non-nil empty slice: delete-all does not enumerate IDs even
	// when verbose=true (enumerating a whole-model wipe is impractical at scale).
	if cond == nil {
		all, err := h.DeleteAllEntities(ctx, entityName, modelVersion)
		if err != nil {
			return nil, err
		}
		return &DeleteResult{
			EntityModelID: all.EntityModelID,
			MatchedCount:  all.TotalCount,
			RemovedCount:  all.TotalCount,
			IDToError:     map[string]string{},
			IDs:           []string{},
		}, nil
	}

	txID, txCtx, owned, err := h.beginOrJoin(ctx)
	if err != nil {
		return nil, common.Internal("failed to begin transaction", err)
	}
	if !owned {
		var releaseGate func()
		txCtx, releaseGate = h.acquireJoinedGate(txCtx, txID)
		defer releaseGate()
	}

	modelStore, err := h.factory.ModelStore(txCtx)
	if err != nil {
		h.rollbackOwned(txCtx, txID, owned)
		return nil, common.Internal("failed to access model store", err)
	}
	if _, err := modelStore.Get(txCtx, ref); err != nil {
		h.rollbackOwned(txCtx, txID, owned)
		if errors.Is(err, spi.ErrNotFound) {
			return nil, common.Operational(http.StatusNotFound, common.ErrCodeModelNotFound,
				fmt.Sprintf("cannot find model entityName=%s, version=%s", entityName, modelVersion))
		}
		return nil, common.Internal("failed to load model", err)
	}

	entityStore, err := h.factory.EntityStore(txCtx)
	if err != nil {
		h.rollbackOwned(txCtx, txID, owned)
		return nil, common.Internal("failed to access entity store", err)
	}

	// Select ALL matching ids (tx-visible; honours pointInTime). Limit=-1 disables
	// the in-memory fallback's default 1000-entity cap so a scoped delete is never
	// silently partial regardless of match-set size.
	matched, err := h.searchSvc.Search(txCtx, ref, cond, search.SearchOptions{PointInTime: pointInTime, Limit: -1})
	if err != nil {
		h.rollbackOwned(txCtx, txID, owned)
		return nil, common.Internal("failed to select entities for delete", err)
	}

	result := &DeleteResult{
		EntityModelID: deterministicModelID(ref).String(),
		MatchedCount:  len(matched),
		IDToError:     map[string]string{},
		IDs:           []string{},
	}

	// Finalize: gate the per-id deletes + commit against a concurrent joined
	// callback's buffer write (mirror DeleteAllEntities).
	if appErr := func() *common.AppError {
		if owned {
			defer h.gate.Acquire(txID)()
		}
		for _, e := range matched {
			id := e.Meta.ID
			if verbose {
				result.IDs = append(result.IDs, id)
			}
			if err := entityStore.Delete(txCtx, id); err != nil {
				result.IDToError[id] = err.Error()
				continue
			}
			result.RemovedCount++
		}
		if err := h.commitOwned(txCtx, txID, owned); err != nil {
			// Do NOT roll back here — a failed commit has already aborted the
			// tx. Mirrors DeleteAllEntities (service.go), which returns
			// the AppError directly on this path without an extra rollback.
			if errors.Is(err, spi.ErrConflict) {
				return common.Operational(http.StatusConflict, common.ErrCodeConflict, "transaction conflict — retry").AsRetryable()
			}
			return common.Internal("failed to commit transaction", err)
		}
		return nil
	}(); appErr != nil {
		return nil, appErr
	}

	return result, nil
}

// ListEntities retrieves all entities for a model with pagination.
// When pointInTime is non-nil the read is issued against the as-at snapshot
// via GetAllAsAt, and meta.pointInTime is stamped on every envelope.
func (h *Handler) ListEntities(ctx context.Context, entityName string, modelVersion string, page PaginationParams, pointInTime *time.Time) ([]EntityEnvelope, error) {
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

	modelStore, err := h.factory.ModelStore(ctx)
	if err != nil {
		return nil, common.Internal("failed to access model store", err)
	}
	if appErr := common.EnsureModelRegistered(ctx, modelStore, ref); appErr != nil {
		return nil, appErr
	}

	var entities []*spi.Entity
	if pointInTime != nil {
		entities, err = entityStore.GetAllAsAt(ctx, ref, *pointInTime)
	} else {
		entities, err = entityStore.GetAll(ctx, ref)
	}
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

	result := make([]EntityEnvelope, 0, len(pageSlice))
	for _, ent := range pageSlice {
		var data any
		if err := decodeJSONPreservingNumbers(ent.Data, &data); err != nil {
			return nil, common.Internal("failed to parse entity data", err)
		}

		modelVersion, _ := strconv.Atoi(ent.Meta.ModelRef.ModelVersion)
		entMeta := map[string]any{
			"id":             ent.Meta.ID,
			"modelKey":       map[string]any{"name": ent.Meta.ModelRef.EntityName, "version": modelVersion},
			"state":          ent.Meta.State,
			"creationDate":   ent.Meta.CreationDate.UTC().Format(time.RFC3339Nano),
			"lastUpdateTime": ent.Meta.LastModifiedDate.UTC().Format(time.RFC3339Nano),
			"transactionId":  ent.Meta.TransactionID,
		}
		if ent.Meta.TransitionForLatestSave != "" {
			entMeta["transitionForLatestSave"] = ent.Meta.TransitionForLatestSave
		}
		if pointInTime != nil {
			entMeta["pointInTime"] = pointInTime.UTC().Format(time.RFC3339Nano)
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
		uniqueKeys   []spi.UniqueKey
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

		// Pre-check for partial unique key (pre-tx, fast path). Desc is in hand
		// here so no extra read is needed. A partial key (some but not all fields
		// present) is rejected immediately before any transaction is opened.
		if len(desc.UniqueKeys) > 0 {
			if _, err := spi.ComputeClaims(desc.UniqueKeys, payloadBytes); err != nil {
				if errors.Is(err, spi.ErrPartialUniqueKey) {
					return nil, common.Operational(http.StatusUnprocessableEntity, common.ErrCodeInvalidUniqueKey,
						fmt.Sprintf("item %d: composite unique key incomplete", i))
				}
				return nil, common.Internal(fmt.Sprintf("item %d: failed to evaluate unique keys", i), err)
			}
		}

		parsed = append(parsed, parsedItem{ref: ref, payloadBytes: payloadBytes, uniqueKeys: desc.UniqueKeys})
	}

	// Begin transaction -- all entities in one transaction, all-or-nothing
	// in the common (non-segmenting) case. The "current" TX is threaded
	// through the loop because each item's engine call may segment via
	// COMMIT_BEFORE_DISPATCH, in which case the engine commits TX_pre and
	// returns a fresh TX_post on FinalCtx/FinalTxID — subsequent items must
	// continue saving against that new TX rather than the now-closed
	// initial one. Mirrors UpdateEntityCollection's segment-aware loop.
	//
	// Atomicity caveat for segmenting cascades: once the engine has
	// committed TX_pre durably, the handler can no longer roll back that
	// item's pre-callout state. A failure on item N+M will still rollback
	// the still-open final TX, but earlier segments' TX_pre commits remain
	// durable. This is a fundamental consequence of CBD and applies
	// uniformly anywhere the engine segments; non-CBD batches retain the
	// original all-or-nothing semantic.
	txID, txCtx, owned, err := h.beginOrJoin(ctx)
	if err != nil {
		return nil, common.Internal("failed to begin transaction", err)
	}
	if !owned {
		var releaseGate func()
		txCtx, releaseGate = h.acquireJoinedGate(txCtx, txID)
		defer releaseGate()
	}

	now := time.Now()

	// Pre-generate entity IDs so they are stable across the per-item engine
	// execution and Save. The IDs are also written into the audit trail by
	// the engine via entity.Meta.ID, so they must be assigned before
	// engine.Execute runs.
	entityIDs := make([]string, len(parsed))
	for i := range parsed {
		entityIDs[i] = uuid.UUID(h.uuids.NewTimeUUID()).String()
	}

	currentCtx, currentTxID := txCtx, txID

	for i, item := range parsed {
		entity := &spi.Entity{
			Meta: spi.EntityMeta{
				ID:                      entityIDs[i],
				TenantID:                uc.Tenant.ID,
				ModelRef:                item.ref,
				State:                   "",
				CreationDate:            now,
				LastModifiedDate:        now,
				TransactionID:           currentTxID,
				TransitionForLatestSave: "",
				ChangeType:              "CREATED",
				ChangeUser:              uc.UserID,
			},
			Data: item.payloadBytes,
		}

		// Stamp the current item's unique-key definitions onto the context
		// BEFORE engine execution. Called unconditionally (even for models with
		// no keys) so a keyed item never leaks its keys to the next item via the
		// reused currentCtx. spi.WithUniqueKeys overwrites any previously set
		// value, so passing nil/empty correctly clears a prior item's keys.
		currentCtx = spi.WithUniqueKeys(currentCtx, item.uniqueKeys)

		// Run workflow engine within the current segment's transaction
		// context. Mirrors single CreateEntity's flow so initial-state
		// derivation, automated cascade and state-machine audit events all
		// apply per item. Issue #227.
		result, err := h.engine.Execute(currentCtx, entity, "")
		if err != nil {
			h.rollbackOwned(currentCtx, currentTxID, owned)
			slog.Error("workflow execution failed", "error", err.Error(), "entityId", entity.Meta.ID, "itemIndex", i)
			return nil, classifyWorkflowError(fmt.Errorf("item %d: %w", i, err))
		}

		// If no workflow was found, engine returns forced success and
		// entity state stays empty — fall back to "CREATED" to match
		// single CreateEntity.
		if entity.Meta.State == "" {
			entity.Meta.State = "CREATED"
		}

		// CREATE path runs without an explicit transition; canonical
		// marker is "loopback" (issue #94), matching single CreateEntity.
		if result != nil && result.StopReason == "" {
			entity.Meta.TransitionForLatestSave = "loopback"
		}

		// Advance the loop's TX to whichever segment is now open. For
		// non-segmenting cascades these are unchanged; for segmenting
		// cascades the engine committed TX_pre and opened TX_post on
		// FinalCtx — subsequent items must save against that new TX.
		if result != nil {
			currentCtx, currentTxID = result.FinalCtx, result.FinalTxID
		}

		// A joined callback is a plain single-segment op; a participating batch
		// must not segment (the owner, not the callback, owns commit boundaries).
		if !owned && currentTxID != txID {
			return nil, common.Internal("joined callback unexpectedly segmented transaction",
				fmt.Errorf("item %d: entry txID %s advanced to %s on a joined call", i, txID, currentTxID))
		}

		// Finalize this item's Save. For the OWNER, gate each per-item Save so a
		// callback in-flight from this item's dispatch cannot race the buffer
		// write; the joined path already holds the gate for its whole body. The
		// gate is never held across engine.Execute (above).
		if appErr := func() *common.AppError {
			if owned {
				defer h.gate.Acquire(currentTxID)()
			}
			// Re-resolve the entity store on the now-current segment context;
			// the per-segment factory may bind storage handles to ctx.
			finalEntityStore, err := h.factory.EntityStore(currentCtx)
			if err != nil {
				h.rollbackOwned(currentCtx, currentTxID, owned)
				return common.Internal("failed to access entity store", err)
			}
			if _, err := finalEntityStore.Save(currentCtx, entity); err != nil {
				h.rollbackOwned(currentCtx, currentTxID, owned)
				return common.Internal(fmt.Sprintf("item %d: failed to save entity", i), err)
			}
			return nil
		}(); appErr != nil {
			return nil, appErr
		}
	}

	// Commit the final still-open TX. Equals the entry txID for
	// non-segmenting batches; for batches with at least one segmenting
	// cascade this is the post-segment TX (earlier segments already
	// committed their TX_pre durably). For the OWNER, gate the commit against a
	// concurrent joined callback's buffer write; a joined batch skips the commit
	// entirely (owner commits).
	if appErr := func() *common.AppError {
		if owned {
			defer h.gate.Acquire(currentTxID)()
		}
		if err := h.commitOwned(currentCtx, currentTxID, owned); err != nil {
			if errors.Is(err, spi.ErrConflict) {
				return common.Operational(http.StatusConflict, common.ErrCodeConflict, "transaction conflict — retry").AsRetryable()
			}
			return common.Internal("failed to commit transaction", err)
		}
		return nil
	}(); appErr != nil {
		return nil, appErr
	}

	// Surface the cascade-entry txID for audit correlation (spec §8).
	return &EntityTransactionResult{
		TransactionID: txID,
		EntityIDs:     entityIDs,
	}, nil
}

// UpdateEntity updates a single entity with an optional named transition or loopback.
func (h *Handler) UpdateEntity(ctx context.Context, input UpdateEntityInput) (*EntityTransactionResult, error) {
	return h.updateEntityCore(ctx, input, updateOptions{})
}

// updateEntityCore is the shared implementation for UpdateEntity and PatchEntity.
// opts.merge, when non-nil, applies an RFC 7386 merge before validation.
// opts.strictValidate forces validate-only (never extends the model schema).
func (h *Handler) updateEntityCore(ctx context.Context, input UpdateEntityInput, opts updateOptions) (*EntityTransactionResult, error) {
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

	// Begin a fresh tx, or PARTICIPATE in a joined tx already on ctx (#287).
	// A joined callback does not Begin/commit; its whole body is one gated
	// critical section on the shared tx buffer.
	txID, txCtx, owned, err := h.beginOrJoin(ctx)
	if err != nil {
		return nil, common.Internal("failed to begin transaction", err)
	}
	if !owned {
		var releaseGate func()
		txCtx, releaseGate = h.acquireJoinedGate(txCtx, txID)
		defer releaseGate()
	}

	// Load existing entity within transaction (adds to read set).
	entityStore, err := h.factory.EntityStore(txCtx)
	if err != nil {
		h.rollbackOwned(txCtx, txID, owned)
		return nil, common.Internal("failed to access entity store", err)
	}

	existing, err := entityStore.Get(txCtx, input.EntityID)
	if err != nil {
		h.rollbackOwned(txCtx, txID, owned)
		return nil, common.Operational(http.StatusNotFound, common.ErrCodeEntityNotFound, "entity not found")
	}

	// Load model descriptor
	desc, err := modelStore.Get(txCtx, existing.Meta.ModelRef)
	if err != nil {
		h.rollbackOwned(txCtx, txID, owned)
		return nil, common.Internal("failed to load model for entity", err)
	}

	// PATCH: merge the sparse body onto the stored data before validation,
	// inside this transaction so the merge base is the version being overwritten.
	if opts.merge != nil {
		merged, mErr := opts.merge(existing.Data, parsedData)
		if mErr != nil {
			h.rollbackOwned(txCtx, txID, owned)
			return nil, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid patch: "+mErr.Error())
		}
		parsedData = merged
		bodyBytes, err = json.Marshal(parsedData)
		if err != nil {
			h.rollbackOwned(txCtx, txID, owned)
			return nil, common.Internal("failed to serialize merged entity", err)
		}
	}

	// Validate the (possibly merged) result. PATCH validates strictly and never
	// extends the model; PUT may extend per the model's ChangeLevel.
	if opts.strictValidate {
		if vErr := h.validateStrict(desc, parsedData); vErr != nil {
			h.rollbackOwned(txCtx, txID, owned)
			return nil, classifyValidateOrExtendErr(vErr)
		}
	} else {
		if vErr := h.validateOrExtend(txCtx, modelStore, desc, parsedData); vErr != nil {
			h.rollbackOwned(txCtx, txID, owned)
			return nil, classifyValidateOrExtendErr(vErr)
		}
	}

	// Pre-check unique keys on the (possibly merged) document and thread them onto
	// txCtx so they propagate into Execute and Save. Partial keys are rejected
	// here (422) inside the already-open transaction so the TX is rolled back.
	txCtx, err = h.withUniqueKeys(txCtx, desc, bodyBytes)
	if err != nil {
		h.rollbackOwned(txCtx, txID, owned)
		return nil, err
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
			h.rollbackOwned(txCtx, txID, owned)
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
			h.rollbackOwned(txCtx, txID, owned)
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

	// A joined callback is a plain single-segment op; a participating update
	// must not segment (the owner owns commit boundaries, not the callback).
	if !owned && finalTxID != txID {
		return nil, common.Internal("joined callback unexpectedly segmented transaction",
			fmt.Errorf("entry txID %s advanced to %s on a joined call", txID, finalTxID))
	}

	finalEntityStore, err := h.factory.EntityStore(finalCtx)
	if err != nil {
		h.rollbackOwned(finalCtx, finalTxID, owned)
		return nil, common.Internal("failed to access entity store", err)
	}

	// Distinguish: did the engine segment (and therefore consume IfMatch)?
	// Segmented is true iff at least one COMMIT_BEFORE_DISPATCH segment
	// committed; the engine's first-segment flush already applied the
	// caller's IfMatch. For non-segmenting cascades the handler still owns
	// the IfMatch precondition.
	segmented := engineResult.Segmented

	// Finalize: gate the OWNER's Save/CompareAndSave + Commit (and the abort-
	// audit buffer write on the conflict path) against a concurrent joined
	// callback's write; the joined path already holds the gate for its whole
	// body. The gate is NEVER held across engine.Execute (above).
	if appErr := func() *common.AppError {
		if owned {
			defer h.gate.Acquire(finalTxID)()
		}
		if input.IfMatch != "" && !segmented {
			if _, err := finalEntityStore.CompareAndSave(finalCtx, updated, input.IfMatch); err != nil {
				if errors.Is(err, spi.ErrConflict) {
					// Reviewer S1 (#228): emit the compensating
					// TRANSITION_ABORTED into the same TX buffer as the
					// entry-side audit events BEFORE rolling back, so on
					// stores where audit is TX-bound the abort event rolls
					// back together with the entry events (audit log
					// remains empty, consistent), and on stores where audit
					// is not TX-bound the abort event is preserved as a
					// pair with the entry events.
					h.emitTransitionAborted(finalCtx, updated, txID, input.Transition, input.IfMatch)
					h.rollbackOwned(finalCtx, finalTxID, owned)
					appErr := common.Operational(
						http.StatusPreconditionFailed,
						common.ErrCodeEntityModified,
						"entity has been modified since last read")
					appErr.Props = map[string]any{"entityId": input.EntityID}
					return appErr
				}
				h.rollbackOwned(finalCtx, finalTxID, owned)
				return common.Internal("failed to save entity", err)
			}
		} else {
			// Plain Save: either no IfMatch was provided, or the engine already
			// consumed it at first-segment flush (segmented == true). In the
			// segmented case the row's current TransactionID has advanced through
			// TX_pre's commit, so a handler-side CAS against input.IfMatch would
			// fail spuriously — Save lands the post-cascade state in TX_post's
			// buffer and the segment's own intra-TX guards handle concurrency.
			if _, err := finalEntityStore.Save(finalCtx, updated); err != nil {
				h.rollbackOwned(finalCtx, finalTxID, owned)
				return common.Internal("failed to save entity", err)
			}
		}

		// Commit FinalTxID — the still-open TX after the cascade (no-op when
		// participating in a joined tx; the owner commits). For non-segmenting
		// cascades this is the handler's original txID; for segmenting cascades
		// this is TX_post (TX_pre was committed by the engine before the callout).
		if err := h.commitOwned(finalCtx, finalTxID, owned); err != nil {
			if errors.Is(err, spi.ErrConflict) {
				return common.Operational(http.StatusConflict, common.ErrCodeConflict, "transaction conflict — retry").AsRetryable()
			}
			return common.Internal("failed to commit transaction", err)
		}
		return nil
	}(); appErr != nil {
		return nil, appErr
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

// PatchEntity applies a partial update. RFC 7386 merge patch is implemented;
// RFC 6902 (JSON_PATCH) is scaffolded and returns 501.
func (h *Handler) PatchEntity(ctx context.Context, input PatchEntityInput) (*EntityTransactionResult, error) {
	switch input.PatchFormat {
	case "MERGE_PATCH":
		// handled below
	case "JSON_PATCH":
		return nil, common.Operational(http.StatusNotImplemented, common.ErrCodeNotImplemented,
			"RFC 6902 JSON Patch is not implemented; use application/merge-patch+json")
	default:
		return nil, common.Operational(http.StatusUnsupportedMediaType, common.ErrCodeUnsupportedMediaType,
			"unsupported patch format")
	}

	// "*" = unconditional: drop the CAS token. Existence is still guaranteed by
	// the in-transaction Get inside updateEntityCore (404 if the entity is gone).
	ifMatch := input.IfMatch
	if ifMatch == "*" {
		ifMatch = ""
	}

	return h.updateEntityCore(ctx, UpdateEntityInput{
		EntityID:   input.EntityID,
		Format:     "JSON",
		Data:       input.Patch,
		Transition: input.Transition,
		IfMatch:    ifMatch,
	}, updateOptions{
		merge:          mergeMergePatch,
		strictValidate: true,
	})
}

// UpdateEntityCollection updates multiple entities in a single transaction
// (PUT /api/entity/{format}). Loopback updates (empty Transition) and
// named-transition updates may be mixed within the same batch. Issue #92.
//
// Per-item failure handling:
//
//   - Items WITHOUT IfMatch retain the documented all-or-nothing semantic:
//     any failure (missing entity, validation, engine error) rolls the
//     entire chunk back. Issue #92.
//   - Items WITH IfMatch isolate ENTITY_MODIFIED conflicts (spi.ErrConflict)
//     to a per-chunk Failed slice; the chunk still commits its remaining
//     successful items. Other per-item failures still roll the chunk back.
//     Issue #228.
//
// Returning from this function:
//
//   - (*UpdateCollectionResult, nil) on chunk-success — even when every
//     item failed via per-item ENTITY_MODIFIED isolation (zero-write commit).
//   - (nil, *common.AppError) when a non-isolated failure aborts the chunk.
//
// The TransactionID returned is the cascade-entry txID; for batches with at
// least one COMMIT_BEFORE_DISPATCH cascade, this still names the original
// entry tx (earlier segments already committed their TX_pre durably) — same
// convention as CreateEntityCollection.
func (h *Handler) UpdateEntityCollection(ctx context.Context, items []UpdateCollectionItem) (*UpdateCollectionResult, error) {
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
		ifMatch    string
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
			ifMatch:    item.IfMatch,
			bodyBytes:  []byte(item.Payload),
			parsedData: data,
		})
	}

	// Begin transaction — all items share one TX in the common
	// (non-segmenting) case, preserving the documented all-or-nothing
	// semantic. The "current" TX is threaded through the loop because each
	// item's engine call may segment via COMMIT_BEFORE_DISPATCH, in which
	// case the engine commits TX_pre and returns a fresh TX_post on
	// FinalCtx — subsequent items continue on that new TX.
	//
	// Atomicity caveat for segmenting cascades: once the engine has
	// committed TX_pre durably, the handler can no longer roll back that
	// item's pre-callout state. A failure on item N+M will still rollback
	// the still-open final TX, but earlier segments' TX_pre commits remain
	// durable. This is a fundamental consequence of CBD and applies
	// uniformly anywhere the engine segments; non-CBD batches retain the
	// original all-or-nothing semantic.
	txID, txCtx, owned, err := h.beginOrJoin(ctx)
	if err != nil {
		return nil, common.Internal("failed to begin transaction", err)
	}
	if !owned {
		var releaseGate func()
		txCtx, releaseGate = h.acquireJoinedGate(txCtx, txID)
		defer releaseGate()
	}

	now := time.Now()
	uc := spi.GetUserContext(ctx)
	changeUser := ""
	if uc != nil {
		changeUser = uc.UserID
	}

	entityIDs := make([]string, 0, len(parsed))
	failed := make([]UpdateCollectionItemFailure, 0)

	currentCtx, currentTxID := txCtx, txID

	for i, item := range parsed {
		entityStore, err := h.factory.EntityStore(currentCtx)
		if err != nil {
			h.rollbackOwned(currentCtx, currentTxID, owned)
			return nil, common.Internal("failed to access entity store", err)
		}

		existing, err := entityStore.Get(currentCtx, item.id)
		if err != nil {
			h.rollbackOwned(currentCtx, currentTxID, owned)
			return nil, common.Operational(http.StatusNotFound, common.ErrCodeEntityNotFound,
				fmt.Sprintf("item %d: entity %s not found", i, item.id))
		}

		desc, err := modelStore.Get(currentCtx, existing.Meta.ModelRef)
		if err != nil {
			h.rollbackOwned(currentCtx, currentTxID, owned)
			return nil, common.Internal(fmt.Sprintf("item %d: failed to load model for entity", i), err)
		}

		// Pre-check for partial unique key. Called while desc is in hand so
		// no extra model read is needed. Rolls back and fails the whole batch.
		if len(desc.UniqueKeys) > 0 {
			if _, err := spi.ComputeClaims(desc.UniqueKeys, item.bodyBytes); err != nil {
				h.rollbackOwned(currentCtx, currentTxID, owned)
				if errors.Is(err, spi.ErrPartialUniqueKey) {
					return nil, common.Operational(http.StatusUnprocessableEntity, common.ErrCodeInvalidUniqueKey,
						fmt.Sprintf("item %d: composite unique key incomplete", i))
				}
				return nil, common.Internal(fmt.Sprintf("item %d: failed to evaluate unique keys", i), err)
			}
		}

		// Stamp this item's unique-key definitions onto the context before
		// engine execution. Called unconditionally so a keyed item never leaks
		// its keys to the next item; spi.WithUniqueKeys overwrites any prior value.
		currentCtx = spi.WithUniqueKeys(currentCtx, desc.UniqueKeys)

		if err := h.validateOrExtend(currentCtx, modelStore, desc, item.parsedData); err != nil {
			h.rollbackOwned(currentCtx, currentTxID, owned)
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
				TransactionID:    currentTxID,
				ChangeType:       "UPDATED",
				ChangeUser:       changeUser,
			},
			Data: item.bodyBytes,
		}

		// Run the engine on the current TX. When the item supplies an
		// IfMatch precondition we route to the *WithIfMatch entry-points so
		// that, for COMMIT_BEFORE_DISPATCH cascades, the precondition is
		// applied at the first-segment flush — strictly BEFORE any external
		// dispatch fires (spec §4.1). For non-segmenting cascades the
		// engine leaves IfMatch untouched and the handler's CompareAndSave
		// below applies it post-engine. Mirrors single UpdateEntity's
		// routing (issue #27 / #228).
		var engineResult *wfengine.EngineResult
		var engineErr error
		if item.transition == "" {
			engineResult, engineErr = h.engine.LoopbackWithIfMatch(currentCtx, updated, item.ifMatch)
		} else {
			engineResult, engineErr = h.engine.ManualTransitionWithIfMatch(currentCtx, updated, item.transition, item.ifMatch)
		}
		if engineErr != nil {
			// Per-item ENTITY_MODIFIED isolation: the engine's CBD
			// first-segment flush rejected the IfMatch precondition before
			// committing TX_pre or firing any external dispatch. The engine
			// has already emitted a compensating TRANSITION_ABORTED audit
			// event before returning ErrConflict (#228 reviewer S1) so the
			// audit trail for this item is paired (entry + abort) and lands
			// alongside successful siblings on commit.
			if item.ifMatch != "" && errors.Is(engineErr, spi.ErrConflict) {
				slog.Info("collection update item precondition failed",
					"source", "engine", "entityId", updated.Meta.ID, "itemIndex", i)
				failed = append(failed, UpdateCollectionItemFailure{
					EntityID:  updated.Meta.ID,
					Code:      common.ErrCodeEntityModified,
					Message:   "entity has been modified since last read",
					ItemIndex: i,
				})
				continue
			}
			h.rollbackOwned(currentCtx, currentTxID, owned)
			slog.Error("workflow execution failed", "error", engineErr.Error(), "entityId", updated.Meta.ID, "transition", item.transition)
			return nil, classifyWorkflowError(fmt.Errorf("item %d: %w", i, engineErr))
		}
		if item.transition == "" {
			updated.Meta.TransitionForLatestSave = "loopback"
		} else {
			updated.Meta.TransitionForLatestSave = item.transition
		}

		// Distinguish: did the engine segment (and therefore consume IfMatch)?
		// Segmented is true iff at least one CBD segment committed; in that
		// case the engine's first-segment flush already applied this item's
		// IfMatch. For non-segmenting cascades the handler still owns the
		// precondition — apply it via CompareAndSave below. Mirrors the
		// single-UpdateEntity routing post-#27.
		segmented := engineResult.Segmented

		// Advance the loop's TX to whichever segment is now open. For
		// non-segmenting cascades these are unchanged; for segmenting
		// cascades the engine committed TX_pre and opened TX_post.
		currentCtx, currentTxID = engineResult.FinalCtx, engineResult.FinalTxID

		// A joined callback is a plain single-segment op; a participating batch
		// must not segment (the owner owns commit boundaries, not the callback).
		if !owned && currentTxID != txID {
			return nil, common.Internal("joined callback unexpectedly segmented transaction",
				fmt.Errorf("item %d: entry txID %s advanced to %s on a joined call", i, txID, currentTxID))
		}

		// Finalize this item's Save. For the OWNER, gate the Save/CompareAndSave
		// (and the abort-audit buffer write on the isolated-conflict path)
		// against a concurrent joined callback's write; the joined path already
		// holds the gate for its whole body. Never gated across engine.Execute.
		// The closure returns (isolatedFailure, appErr): a non-nil isolated
		// failure means "continue this loop" (per-item ENTITY_MODIFIED isolation),
		// a non-nil appErr means "abort the batch".
		isolated, appErr := func() (*UpdateCollectionItemFailure, *common.AppError) {
			if owned {
				defer h.gate.Acquire(currentTxID)()
			}
			// Re-resolve the entity store on the now-current segment context.
			finalEntityStore, err := h.factory.EntityStore(currentCtx)
			if err != nil {
				h.rollbackOwned(currentCtx, currentTxID, owned)
				return nil, common.Internal("failed to access entity store", err)
			}

			// IfMatch routing for the post-engine save:
			//   - With IfMatch AND non-segmenting cascade: handler owns the
			//     precondition → CompareAndSave. ErrConflict → isolate to
			//     `failed` (no chunk rollback).
			//   - With IfMatch AND segmenting cascade: engine consumed IfMatch
			//     at first-segment flush; row's transactionId has advanced
			//     through TX_pre's commit. CompareAndSave against item.IfMatch
			//     would now fail spuriously — fall back to plain Save.
			//   - Without IfMatch: plain Save (existing behavior).
			applyHandlerCAS := item.ifMatch != "" && !segmented
			var saveErr error
			if applyHandlerCAS {
				_, saveErr = finalEntityStore.CompareAndSave(currentCtx, updated, item.ifMatch)
			} else {
				_, saveErr = finalEntityStore.Save(currentCtx, updated)
			}
			if saveErr != nil {
				if applyHandlerCAS && errors.Is(saveErr, spi.ErrConflict) {
					slog.Info("collection update item precondition failed",
						"source", "handler", "entityId", updated.Meta.ID, "itemIndex", i)
					// Reviewer S1 (#228): emit a compensating TRANSITION_ABORTED
					// audit event so the entry-side audit events recorded by the
					// engine for this item (STATE_MACHINE_START / WORKFLOW_FOUND
					// / TRANSITION_MAKE) have a paired terminal event in the
					// audit log. Best-effort; routed through the engine's
					// audit-store handle so it lands in the same TX buffer as
					// the entry events on stores where audit is TX-bound.
					h.emitTransitionAborted(currentCtx, updated, currentTxID, item.transition, item.ifMatch)
					return &UpdateCollectionItemFailure{
						EntityID:  updated.Meta.ID,
						Code:      common.ErrCodeEntityModified,
						Message:   "entity has been modified since last read",
						ItemIndex: i,
					}, nil
				}
				h.rollbackOwned(currentCtx, currentTxID, owned)
				return nil, common.Internal(fmt.Sprintf("item %d: failed to save entity", i), saveErr)
			}
			return nil, nil
		}()
		if appErr != nil {
			return nil, appErr
		}
		if isolated != nil {
			failed = append(failed, *isolated)
			continue
		}
		entityIDs = append(entityIDs, updated.Meta.ID)
	}

	// Commit the final still-open TX. Equals the entry txID for
	// non-segmenting batches; for batches with at least one segmenting
	// cascade this is the post-segment TX (earlier segments already
	// committed their TX_pre durably). When every item failed via per-item
	// isolation, the commit is a zero-write commit but still validates the
	// read-set and stamps a database timestamp; the txID it commits remains
	// meaningful for audit correlation. For the OWNER, gate the commit against a
	// concurrent joined callback; a joined batch skips the commit (owner commits).
	if appErr := func() *common.AppError {
		if owned {
			defer h.gate.Acquire(currentTxID)()
		}
		if err := h.commitOwned(currentCtx, currentTxID, owned); err != nil {
			if errors.Is(err, spi.ErrConflict) {
				return common.Operational(http.StatusConflict, common.ErrCodeConflict, "transaction conflict — retry").AsRetryable()
			}
			return common.Internal("failed to commit transaction", err)
		}
		return nil
	}(); appErr != nil {
		return nil, appErr
	}

	// Surface the cascade-entry txID for audit correlation (spec §8).
	return &UpdateCollectionResult{
		TransactionID: txID,
		EntityIDs:     entityIDs,
		Failed:        failed,
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
// error code:
//
//   - ErrCommitBeforeDispatchInfra (Begin/Commit/Save plugin failure inside
//     the engine's segment-boundary code) → sanitized 5xx via common.Internal,
//     so internal pgx text never leaks to clients via 4xx WORKFLOW_FAILED.
//   - ErrTransitionNotFound → 400 TRANSITION_NOT_FOUND (client-attributable).
//   - Everything else (processor-domain failures, criterion mismatches, CAS
//     conflicts already mapped upstream) → 400 WORKFLOW_FAILED.
func classifyWorkflowError(err error) *common.AppError {
	if errors.Is(err, wfengine.ErrCommitBeforeDispatchInfra) {
		return common.Internal("workflow segment boundary failed", err)
	}
	if errors.Is(err, spi.ErrUniqueViolation) {
		return common.Operational(http.StatusConflict, common.ErrCodeUniqueViolation, "a composite unique key constraint was violated")
	}
	if errors.Is(err, spi.ErrPartialUniqueKey) {
		return common.Operational(http.StatusUnprocessableEntity, common.ErrCodeInvalidUniqueKey, "one or more unique key fields are null or invalid")
	}
	if errors.Is(err, wfengine.ErrTransitionNotFound) {
		return common.Operational(http.StatusBadRequest, common.ErrCodeTransitionNotFound, err.Error())
	}
	return common.Operational(http.StatusBadRequest, common.ErrCodeWorkflowFailed, err.Error())
}

// emitTransitionAborted writes a TRANSITION_ABORTED audit event for a
// post-engine CompareAndSave conflict on the supplied entity. Routes
// through the same audit-store handle the engine uses so the abort lands
// in the same TX buffer as the entry-side audit events emitted earlier in
// the cascade.
//
// transitionName may be "" for loopback updates — kept verbatim in the
// event payload so downstream consumers can distinguish loopback aborts
// from named-transition aborts. Best-effort: any failure to load the
// audit store is logged at DEBUG and swallowed (an audit-emission failure
// must not break the per-item-isolated commit path).
func (h *Handler) emitTransitionAborted(
	ctx context.Context,
	entity *spi.Entity,
	cascadeEntryTxID string,
	transitionName string,
	expectedTxID string,
) {
	auditStore, err := h.factory.StateMachineAuditStore(ctx)
	if err != nil {
		slog.Debug("transition-aborted: audit store unavailable",
			"pkg", "entity", "entityId", entity.Meta.ID, "error", err)
		return
	}
	transitionForAudit := transitionName
	if transitionForAudit == "" {
		transitionForAudit = "loopback"
	}
	actualTxID := wfengine.LookupActualTxID(ctx, h.factory, entity.Meta.ID)
	wfengine.EmitTransitionAborted(ctx, auditStore, h.uuids,
		entity.Meta.ID, cascadeEntryTxID, entity.Meta.State,
		transitionForAudit, expectedTxID, actualTxID)
}
