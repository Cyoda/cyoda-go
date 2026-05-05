package entity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/importer"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda-platform/cyoda-go/internal/domain/pagination"
	wfengine "github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
)

// maxEntityBodySize is the maximum allowed request body size for entity operations (10 MB).
const maxEntityBodySize = 10 * 1024 * 1024

// errInternalSchema tags schema-processing errors inside validateOrExtend
// that represent internal failures (codec decode/encode, Diff computation,
// plugin-layer ExtendSchema write) rather than client-contract violations.
// The handler classifier uses errors.Is to route these to 5xx with a
// logged ticket. Using a sentinel rather than string-matching the wrap
// messages makes classification robust to future wording changes — the
// prior string-match classifier would have silently shifted a renamed
// "failed to extend schema" to 4xx.
var errInternalSchema = errors.New("internal schema processing failure")

// incompatibleTypeError is the typed validation failure surfaced when at
// least one ValidationError carries ErrKindIncompatibleType (the
// dictionary-aligned "wrong DataType" signal — Cloud's
// FoundIncompatibleTypeWithEntityModelException).
//
// Rendered by classifyValidateOrExtendErr into a 400 INCOMPATIBLE_TYPE
// AppError with Props {fieldPath, expectedType, actualType} so SDKs can
// branch on the precondition without scraping the message string.
type incompatibleTypeError struct {
	path          string
	expectedTypes []schema.DataType
	actualType    schema.DataType
	message       string
	entityName    string // populated by enrichWithModelRef post-validation
	entityVersion string // populated by enrichWithModelRef post-validation
}

func (e *incompatibleTypeError) Error() string { return e.message }

// enrichWithModelRef threads model identification (entity name, version)
// onto an *incompatibleTypeError so the classifier can render those Props
// alongside the validator-supplied (path, expected/actualType). For all
// other error types the input is returned unchanged.
func enrichWithModelRef(err error, ref spi.ModelRef) error {
	var incompatErr *incompatibleTypeError
	if errors.As(err, &incompatErr) {
		incompatErr.entityName = ref.EntityName
		incompatErr.entityVersion = ref.ModelVersion
	}
	return err
}

// maxStatesFilterSize bounds the cardinality of the user-supplied ?states= query
// parameter on stats-by-state endpoints. Without this cap, an oversized list would
// reach SQL backends and either exceed driver parameter limits (SQLite's
// SQLITE_MAX_VARIABLE_NUMBER, default 32766) or stress the planner with a giant
// IN/ANY clause, surfacing as an opaque 5xx instead of a clean 4xx.
const maxStatesFilterSize = 1000

// deterministicModelID derives a stable UUID v5 from a ModelRef, matching the
// model handler's deterministic ID generation.
func deterministicModelID(ref spi.ModelRef) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte(ref.String()))
}

type Handler struct {
	factory spi.StoreFactory
	txMgr   spi.TransactionManager
	uuids   spi.UUIDGenerator
	engine  *wfengine.Engine
}

func New(factory spi.StoreFactory, txMgr spi.TransactionManager, uuids spi.UUIDGenerator, engine *wfengine.Engine) *Handler {
	return &Handler{factory: factory, txMgr: txMgr, uuids: uuids, engine: engine}
}

// validateOrExtend validates parsedData against the model schema. When
// changeLevel is set, it computes an additive schema delta via schema.Diff
// and appends it to the model's extension log via ModelStore.ExtendSchema.
// That call participates in the ambient entity transaction, so visibility
// is commit-bound and concurrent entity writes on the same model do not
// contend on a single "models" row — the hot-row regression that
// ModelStore.Save would otherwise produce under REPEATABLE READ.
// Returns an error on validation or extension failure.
func (h *Handler) validateOrExtend(ctx context.Context, modelStore spi.ModelStore, desc *spi.ModelDescriptor, parsedData any) error {
	modelNode, err := schema.Unmarshal(desc.Schema)
	if err != nil {
		return fmt.Errorf("%w: failed to unmarshal model schema: %w", errInternalSchema, err)
	}

	if desc.ChangeLevel == "" {
		errs := schema.Validate(modelNode, parsedData)
		if len(errs) > 0 {
			return enrichWithModelRef(validationErrorsToError(errs), desc.Ref)
		}
		return nil
	}

	incomingModel, err := importer.Walk(parsedData)
	if err != nil {
		return fmt.Errorf("failed to walk data: %w", err)
	}
	extended, err := schema.Extend(modelNode, incomingModel, desc.ChangeLevel)
	if err != nil {
		// Polymorphic-slot rejections cannot be resolved by raising ChangeLevel
		// and so must not wear the "change level violation" prefix — the phrase
		// misleads clients into tuning a setting that wouldn't help.
		if errors.Is(err, schema.ErrPolymorphicSlot) {
			return err
		}
		return fmt.Errorf("change level violation: %w", err)
	}

	// Compute the additive delta. Diff returns (nil, nil) when the
	// extension is a semantic no-op, which is the common case on
	// every entity write.
	delta, err := schema.Diff(modelNode, extended)
	if err != nil {
		return fmt.Errorf("%w: failed to compute schema delta: %w", errInternalSchema, err)
	}
	if delta == nil {
		return nil
	}
	// Append to the extension log via the plugin. Participates in the
	// ambient entity transaction so visibility is commit-bound.
	if err := modelStore.ExtendSchema(ctx, desc.Ref, delta); err != nil {
		return fmt.Errorf("%w: failed to extend schema: %w", errInternalSchema, err)
	}
	return nil
}

// ValidateWithRefresh runs strict schema validation with a bounded
// refresh-on-stale safety net. One refresh attempt, only on unknown-
// schema-element errors — the signal that our cached schema is behind
// a peer's ExtendSchema. Other validation failures surface directly.
// Stores that don't implement RefreshAndGet (no caching layer) skip
// the refresh and return the original errors. See spec §4.3.
func (h *Handler) ValidateWithRefresh(ctx context.Context, modelStore spi.ModelStore, ref spi.ModelRef, data any) error {
	desc, err := modelStore.Get(ctx, ref)
	if err != nil {
		return err
	}
	errs := validateDescriptor(desc, data)
	if errs == nil {
		return nil
	}
	if !schema.HasUnknownSchemaElement(errs) {
		return validationErrorsToError(errs)
	}
	refresher, ok := modelStore.(interface {
		RefreshAndGet(context.Context, spi.ModelRef) (*spi.ModelDescriptor, error)
	})
	if !ok {
		return validationErrorsToError(errs) // plugin has no cache
	}
	freshDesc, rErr := refresher.RefreshAndGet(ctx, ref)
	if rErr != nil {
		return rErr
	}
	if errs2 := validateDescriptor(freshDesc, data); errs2 != nil {
		return validationErrorsToError(errs2)
	}
	return nil
}

// validateDescriptor unmarshals desc.Schema and runs schema.Validate.
// Returns nil on success, or a []ValidationError on failure (including
// a descriptive entry if desc itself is malformed or nil).
func validateDescriptor(desc *spi.ModelDescriptor, data any) []schema.ValidationError {
	if desc == nil {
		return []schema.ValidationError{{Message: "nil descriptor"}}
	}
	node, err := schema.Unmarshal(desc.Schema)
	if err != nil {
		return []schema.ValidationError{{Message: fmt.Sprintf("unmarshal schema: %v", err)}}
	}
	return schema.Validate(node, data)
}

// validationErrorsToError converts a []ValidationError to a single error,
// preserving the concatenation style used by validateOrExtend.
//
// When at least one entry classifies as ErrKindIncompatibleType (the
// dictionary-aligned "wrong DataType" signal), the function returns a
// typed *incompatibleTypeError carrying the first such entry's structured
// fields so classifyValidateOrExtendErr can render INCOMPATIBLE_TYPE Props
// without scraping the message string. Other validation errors fall back
// to the generic "validation failed: ..." wrap, classified as
// BAD_REQUEST downstream.
func validationErrorsToError(errs []schema.ValidationError) error {
	msgs := make([]string, len(errs))
	for i, e := range errs {
		msgs[i] = e.Error()
	}
	joined := fmt.Sprintf("validation failed: %s", strings.Join(msgs, "; "))
	if first := schema.FirstIncompatibleType(errs); first != nil {
		return &incompatibleTypeError{
			path:          first.Path,
			expectedTypes: first.ExpectedTypes,
			actualType:    first.ActualType,
			message:       joined,
		}
	}
	return fmt.Errorf("%s", joined)
}

// classifyValidateOrExtendErr determines whether a validateOrExtend error is
// internal (5xx) or operational (4xx) and returns the appropriate AppError.
//
// Classification is sentinel-based to keep it robust against wording drift
// in the wrap strings:
//
//   - ErrPolymorphicSlot      → 4xx POLYMORPHIC_SLOT (client normalizes payload)
//   - *incompatibleTypeError  → 4xx INCOMPATIBLE_TYPE with structured Props
//     (fieldPath, expectedType, actualType) — Cloud's
//     FoundIncompatibleTypeWithEntityModelException equivalent
//   - errInternalSchema       → 5xx with logged ticket (codec/diff/store failure)
//   - anything else           → 4xx BAD_REQUEST (change-level violation,
//     other validation failure, malformed walk input)
func classifyValidateOrExtendErr(err error) *common.AppError {
	if errors.Is(err, schema.ErrPolymorphicSlot) {
		return common.Operational(http.StatusBadRequest, common.ErrCodePolymorphicSlot, err.Error())
	}
	var incompatErr *incompatibleTypeError
	if errors.As(err, &incompatErr) {
		appErr := common.Operational(http.StatusBadRequest, common.ErrCodeIncompatibleType, err.Error())
		expected := make([]string, len(incompatErr.expectedTypes))
		for i, dt := range incompatErr.expectedTypes {
			expected[i] = dt.String()
		}
		props := map[string]any{
			"fieldPath":    incompatErr.path,
			"expectedType": expected,
			"actualType":   incompatErr.actualType.String(),
		}
		if incompatErr.entityName != "" {
			props["entityName"] = incompatErr.entityName
		}
		if incompatErr.entityVersion != "" {
			props["entityVersion"] = incompatErr.entityVersion
		}
		appErr.Props = props
		return appErr
	}
	if errors.Is(err, errInternalSchema) {
		return common.Internal("failed to process model schema", err)
	}
	return common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, err.Error())
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request, format genapi.CreateParamsFormat, entityName string, modelVersion int32, params genapi.CreateParams) {
	// Resolve transactionWindow up-front so an out-of-range value rejects
	// before we burn any I/O. Mirrors CreateCollection — see the array-body
	// branch below for where the window is actually applied.
	window, paramErr := resolveTransactionWindow(params.TransactionWindow)
	if paramErr != nil {
		common.WriteError(w, r, paramErr)
		return
	}

	// Read request body (with size limit)
	r.Body = http.MaxBytesReader(w, r.Body, maxEntityBodySize)
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "failed to read body"))
		return
	}

	// Detect JSON array body — chunk via the same transactionWindow contract
	// as POST /api/entity/{format} (CreateCollection). Issue #227 pass 3.
	if string(format) == "JSON" && len(bodyBytes) > 0 && bodyBytes[0] == '[' {
		var rawItems []json.RawMessage
		if err := json.Unmarshal(bodyBytes, &rawItems); err != nil {
			common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid JSON array"))
			return
		}

		items := make([]CollectionItem, 0, len(rawItems))
		for _, raw := range rawItems {
			items = append(items, CollectionItem{
				ModelName:    entityName,
				ModelVersion: modelVersion,
				Payload:      raw,
			})
		}

		// Empty array preserves the historical single-empty-call shape so the
		// service-layer empty-collection contract is exercised (no chunks).
		if len(items) == 0 {
			result, err := h.CreateEntityCollection(r.Context(), items)
			if err != nil {
				common.WriteError(w, r, classifyError(err))
				return
			}
			common.WriteJSON(w, http.StatusOK, []collectionChunkResult{{
				TransactionID: result.TransactionID,
				EntityIDs:     result.EntityIDs,
			}})
			return
		}

		results, firstChunkErr := h.runChunkedCreate(r.Context(), items, window)
		if firstChunkErr != nil {
			common.WriteError(w, r, firstChunkErr)
			return
		}
		common.WriteJSON(w, http.StatusOK, results)
		return
	}

	result, err := h.CreateEntity(r.Context(), CreateEntityInput{
		EntityName:   entityName,
		ModelVersion: fmt.Sprintf("%d", modelVersion),
		Format:       string(format),
		Data:         bodyBytes,
	})
	if err != nil {
		common.WriteError(w, r, classifyError(err))
		return
	}

	resp := map[string]any{
		"transactionId": result.TransactionID,
		"entityIds":     result.EntityIDs,
	}
	common.WriteJSON(w, http.StatusOK, []any{resp})
}

func (h *Handler) GetOneEntity(w http.ResponseWriter, r *http.Request, entityId openapi_types.UUID, params genapi.GetOneEntityParams) {
	// Reject if both pointInTime and transactionId are set — the two
	// scopes are mutually exclusive on the dictionary contract.
	if params.PointInTime != nil && params.TransactionId != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "cannot specify both pointInTime and transactionId"))
		return
	}

	input := GetOneEntityInput{
		EntityID:    entityId.String(),
		PointInTime: params.PointInTime,
	}
	// Propagate transactionId scope. Issue #150: previously this query
	// param was parsed by the generated server interface but never plumbed
	// into the service input, so the handler silently returned the latest
	// entity regardless of transactionId.
	if params.TransactionId != nil {
		input.TransactionID = params.TransactionId.String()
	}

	envelope, err := h.GetEntity(r.Context(), input)
	if err != nil {
		common.WriteError(w, r, classifyError(err))
		return
	}

	resp := map[string]any{
		"type": envelope.Type,
		"data": envelope.Data,
		"meta": envelope.Meta,
	}
	common.WriteJSON(w, http.StatusOK, resp)
}

func (h *Handler) GetEntityStatistics(w http.ResponseWriter, r *http.Request, params genapi.GetEntityStatisticsParams) {
	stats, err := h.GetStatistics(r.Context())
	if err != nil {
		common.WriteError(w, r, classifyError(err))
		return
	}

	result := make([]genapi.ModelStatsDto, 0, len(stats))
	for _, s := range stats {
		ver, _ := strconv.Atoi(s.ModelVersion)
		result = append(result, genapi.ModelStatsDto{
			ModelName:    s.ModelName,
			ModelVersion: int32(ver),
			Count:        s.Count,
		})
	}

	common.WriteJSON(w, http.StatusOK, result)
}

func (h *Handler) GetEntityStatisticsByState(w http.ResponseWriter, r *http.Request, params genapi.GetEntityStatisticsByStateParams) {
	if params.States != nil && len(*params.States) > maxStatesFilterSize {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest,
			fmt.Sprintf("states filter has %d entries; maximum is %d", len(*params.States), maxStatesFilterSize)))
		return
	}
	stats, err := h.GetStatisticsByState(r.Context(), params.States)
	if err != nil {
		common.WriteError(w, r, classifyError(err))
		return
	}

	result := make([]genapi.ModelStateStatsDto, 0, len(stats))
	for _, s := range stats {
		ver, _ := strconv.Atoi(s.ModelVersion)
		result = append(result, genapi.ModelStateStatsDto{
			ModelName:    s.ModelName,
			ModelVersion: int32(ver),
			State:        s.State,
			Count:        s.Count,
		})
	}

	common.WriteJSON(w, http.StatusOK, result)
}

func (h *Handler) GetEntityStatisticsByStateForModel(w http.ResponseWriter, r *http.Request, entityName string, modelVersion int32, params genapi.GetEntityStatisticsByStateForModelParams) {
	if params.States != nil && len(*params.States) > maxStatesFilterSize {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest,
			fmt.Sprintf("states filter has %d entries; maximum is %d", len(*params.States), maxStatesFilterSize)))
		return
	}
	stats, err := h.GetStatisticsByStateForModel(r.Context(), entityName, fmt.Sprintf("%d", modelVersion), params.States)
	if err != nil {
		common.WriteError(w, r, classifyError(err))
		return
	}

	result := make([]genapi.ModelStateStatsDto, 0, len(stats))
	for _, s := range stats {
		result = append(result, genapi.ModelStateStatsDto{
			ModelName:    s.ModelName,
			ModelVersion: modelVersion,
			State:        s.State,
			Count:        s.Count,
		})
	}

	common.WriteJSON(w, http.StatusOK, result)
}

func (h *Handler) GetEntityStatisticsForModel(w http.ResponseWriter, r *http.Request, entityName string, modelVersion int32, params genapi.GetEntityStatisticsForModelParams) {
	stat, err := h.GetStatisticsForModel(r.Context(), entityName, fmt.Sprintf("%d", modelVersion))
	if err != nil {
		common.WriteError(w, r, classifyError(err))
		return
	}

	result := genapi.ModelStatsDto{
		ModelName:    stat.ModelName,
		ModelVersion: modelVersion,
		Count:        stat.Count,
	}

	common.WriteJSON(w, http.StatusOK, result)
}

func (h *Handler) DeleteSingleEntity(w http.ResponseWriter, r *http.Request, entityId openapi_types.UUID) {
	result, err := h.DeleteEntity(r.Context(), entityId.String())
	if err != nil {
		common.WriteError(w, r, classifyError(err))
		return
	}

	resp := map[string]any{
		"id": result.EntityID,
		"modelKey": map[string]any{
			"name":    result.ModelName,
			"version": result.ModelVersion,
		},
		"transactionId": result.TransactionID,
	}
	common.WriteJSON(w, http.StatusOK, resp)
}

func (h *Handler) GetEntityChangesMetadata(w http.ResponseWriter, r *http.Request, entityId openapi_types.UUID, params genapi.GetEntityChangesMetadataParams) {
	entries, err := h.GetChangesMetadata(r.Context(), entityId.String(), params.PointInTime)
	if err != nil {
		common.WriteError(w, r, classifyError(err))
		return
	}

	result := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		entry := map[string]any{
			"changeType":   e.ChangeType,
			"timeOfChange": e.TimeOfChange,
			"user":         e.User,
		}
		if e.HasEntity {
			entry["transactionId"] = e.TransactionID
		}
		result = append(result, entry)
	}

	common.WriteJSON(w, http.StatusOK, result)
}

func (h *Handler) DeleteEntities(w http.ResponseWriter, r *http.Request, entityName string, modelVersion int32, params genapi.DeleteEntitiesParams) {
	result, err := h.DeleteAllEntities(r.Context(), entityName, fmt.Sprintf("%d", modelVersion))
	if err != nil {
		common.WriteError(w, r, classifyError(err))
		return
	}

	// Spec declares StreamDeleteResult as a single object (not an array).
	// The object has: entityModelClassId (uuid), deleteResult (nested object),
	// and optional ids ([]uuid). Server is source of truth per design §3.
	resp := map[string]any{
		"entityModelClassId": result.EntityModelID,
		"deleteResult": map[string]any{
			"idToError":                map[string]any{},
			"numberOfEntitites":        result.TotalCount,
			"numberOfEntititesRemoved": result.TotalCount,
		},
	}
	common.WriteJSON(w, http.StatusOK, resp)
}

func (h *Handler) GetAllEntities(w http.ResponseWriter, r *http.Request, entityName string, modelVersion int32, params genapi.GetAllEntitiesParams) {
	// Apply pagination defaults
	pageSize := int32(20)
	pageNumber := int32(0)
	if params.PageSize != nil {
		pageSize = *params.PageSize
	}
	if params.PageNumber != nil {
		pageNumber = *params.PageNumber
	}

	// Reject negative / over-cap / overflow-prone values BEFORE the
	// storage lookup. Without this guard, an attacker-supplied
	// pageNumber=MaxInt32 panics in ListEntities (slice bounds out of
	// range) and surfaces as 500 — see PR #149 follow-up. ValidateOffset
	// returns *common.AppError as error; classifyError routes it to the
	// 400 BAD_REQUEST response.
	if err := pagination.ValidateOffset(int64(pageNumber), int64(pageSize)); err != nil {
		common.WriteError(w, r, classifyError(err))
		return
	}

	envelopes, err := h.ListEntities(r.Context(), entityName, fmt.Sprintf("%d", modelVersion), PaginationParams{
		PageSize:   pageSize,
		PageNumber: pageNumber,
	})
	if err != nil {
		common.WriteError(w, r, classifyError(err))
		return
	}

	result := make([]map[string]any, 0, len(envelopes))
	for _, env := range envelopes {
		result = append(result, map[string]any{
			"type": env.Type,
			"data": env.Data,
			"meta": env.Meta,
		})
	}

	common.WriteJSON(w, http.StatusOK, result)
}

// collectionDefaultWindow is the default batch cap applied when a client
// does not pass `transactionWindow`. Matches what the docs have always
// advertised (100). collectionMaxWindow is the hard upper bound the server
// will accept from a client — larger values are rejected with 400 rather
// than silently clamped, so misuse is visible.
const (
	collectionDefaultWindow = 100
	collectionMaxWindow     = 1000
)

// resolveTransactionWindow returns the effective window for a collection
// request. Returns 400 BAD_REQUEST when the client supplies a value
// outside (0, collectionMaxWindow].
func resolveTransactionWindow(window *int32) (int, *common.AppError) {
	if window == nil {
		return collectionDefaultWindow, nil
	}
	if *window <= 0 || *window > collectionMaxWindow {
		return 0, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest,
			fmt.Sprintf("transactionWindow must be in (0, %d]", collectionMaxWindow))
	}
	return int(*window), nil
}

// collectionChunkResult is one element of the collection-endpoint response
// array. Successful chunks carry transactionId + entityIds. Failed chunks
// carry the Error field with code/message and the chunk's index. Chunks with
// per-item ENTITY_MODIFIED isolation (issue #228) carry transactionId +
// entityIds for the successful items plus a Failed slice for the conflicted
// items.
//
// Wire contract per the docs: a collection request committed in transactional
// batches of at most `transactionWindow` items returns one element per chunk
// in commit order; chunks committed before any failure remain durable, and
// chunk-wide failures surface as an error element marking chunkIndex.
// Issue #227, extended by #228.
type collectionChunkResult struct {
	TransactionID string                       `json:"transactionId,omitempty"`
	EntityIDs     []string                     `json:"entityIds,omitempty"`
	Error         *collectionChunkError        `json:"error,omitempty"`
	Failed        []collectionChunkItemFailure `json:"failed,omitempty"`
}

// collectionChunkError carries the per-chunk failure shape. ChunkIndex is
// the zero-based position of the failing chunk in commit order so a client
// can pinpoint where partial progress stopped.
type collectionChunkError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	ChunkIndex int    `json:"chunkIndex"`
}

// collectionChunkItemFailure documents a single per-item failure that did NOT
// roll the chunk back. Reserved for ENTITY_MODIFIED conflicts on items
// carrying an IfMatch precondition (issue #228). ItemIndex is the failing
// item's zero-based position within its chunk's request slice.
type collectionChunkItemFailure struct {
	EntityID string                  `json:"entityId"`
	Error    collectionChunkItemErr  `json:"error"`
}

// collectionChunkItemErr is the per-item failure inner object — code, message,
// and per-chunk-relative item index.
type collectionChunkItemErr struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	ItemIndex int    `json:"itemIndex"`
}

// runChunkedCreate splits items into chunks of size `window` and dispatches
// each through CreateEntityCollection in commit order, collecting one
// collectionChunkResult per chunk. Returns:
//
//   - (results, nil) — the full per-chunk result array. May contain an
//     error element on a later-chunk failure (committed chunks before it
//     are durable; subsequent chunks are NOT attempted).
//   - (nil, appErr)  — the FIRST chunk failed, no durable progress was
//     made; the caller writes the conventional 4xx error envelope.
//
// Caller must have already resolved `window` via resolveTransactionWindow.
// Callers in this file guard `len(items) == 0` before invoking; the helper
// itself does no empty-items handling (the loop emits zero elements when
// items is empty, which would produce an empty success array — usually not
// what the empty-batch contract intends; see CreateCollection). The helper
// is internal-only and the empty-items guard is by convention.
//
// Single chunking primitive shared by CreateCollection (POST /entity/{format})
// and Create (POST /entity/{format}/{entityName}/{modelVersion} array body).
// Issue #227.
func (h *Handler) runChunkedCreate(ctx context.Context, items []CollectionItem, window int) ([]collectionChunkResult, *common.AppError) {
	results := make([]collectionChunkResult, 0)
	for chunkIdx, start := 0, 0; start < len(items); chunkIdx, start = chunkIdx+1, start+window {
		end := start + window
		if end > len(items) {
			end = len(items)
		}
		result, err := h.CreateEntityCollection(ctx, items[start:end])
		if err != nil {
			appErr := classifyError(err)
			if chunkIdx == 0 {
				return nil, appErr
			}
			results = append(results, collectionChunkResult{
				Error: &collectionChunkError{
					Code:       appErr.Code,
					Message:    appErr.Message,
					ChunkIndex: chunkIdx,
				},
			})
			return results, nil
		}
		results = append(results, collectionChunkResult{
			TransactionID: result.TransactionID,
			EntityIDs:     result.EntityIDs,
		})
	}
	return results, nil
}

func (h *Handler) CreateCollection(w http.ResponseWriter, r *http.Request, format genapi.CreateCollectionParamsFormat, params genapi.CreateCollectionParams) {
	window, paramErr := resolveTransactionWindow(params.TransactionWindow)
	if paramErr != nil {
		common.WriteError(w, r, paramErr)
		return
	}

	// Read raw body and parse as JSON array (with size limit).
	r.Body = http.MaxBytesReader(w, r.Body, maxEntityBodySize)
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "failed to read body"))
		return
	}

	var rawItems []struct {
		Model struct {
			Name    string `json:"name"`
			Version int32  `json:"version"`
		} `json:"model"`
		Payload string `json:"payload"`
	}
	if err := json.Unmarshal(bodyBytes, &rawItems); err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid JSON array"))
		return
	}

	items := make([]CollectionItem, 0, len(rawItems))
	for _, raw := range rawItems {
		items = append(items, CollectionItem{
			ModelName:    raw.Model.Name,
			ModelVersion: raw.Model.Version,
			Payload:      json.RawMessage(raw.Payload),
		})
	}

	// Empty body keeps the existing single-empty-call shape so we exercise
	// any service-layer empty-collection contract (no chunks emitted).
	if len(items) == 0 {
		result, err := h.CreateEntityCollection(r.Context(), items)
		if err != nil {
			common.WriteError(w, r, classifyError(err))
			return
		}
		common.WriteJSON(w, http.StatusOK, []collectionChunkResult{{
			TransactionID: result.TransactionID,
			EntityIDs:     result.EntityIDs,
		}})
		return
	}

	results, firstChunkErr := h.runChunkedCreate(r.Context(), items, window)
	if firstChunkErr != nil {
		common.WriteError(w, r, firstChunkErr)
		return
	}
	common.WriteJSON(w, http.StatusOK, results)
}

func (h *Handler) UpdateCollection(w http.ResponseWriter, r *http.Request, format genapi.UpdateCollectionParamsFormat, params genapi.UpdateCollectionParams) {
	// Only JSON is wired up today — parity with CreateCollection, which
	// also accepts the format path param but consumes JSON. XML parity
	// for collection update is tracked as a follow-up; single-item PUT
	// endpoints still accept XML via importer.ParseXML.
	if format != genapi.UpdateCollectionParamsFormatJSON {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "collection update accepts JSON only (single-item endpoints accept XML)"))
		return
	}

	window, paramErr := resolveTransactionWindow(params.TransactionWindow)
	if paramErr != nil {
		common.WriteError(w, r, paramErr)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxEntityBodySize)
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "failed to read body"))
		return
	}

	// Per docs: `payload` is a JSON-encoded STRING (not a nested object).
	// Match CreateCollection's wire contract exactly. Optional per-item
	// `ifMatch` carries the cross-request precondition (issue #228).
	var rawItems []struct {
		ID         string `json:"id"`
		Payload    string `json:"payload"`
		Transition string `json:"transition"`
		IfMatch    string `json:"ifMatch"`
	}
	if err := json.Unmarshal(bodyBytes, &rawItems); err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid JSON array (payload must be a JSON-encoded string)"))
		return
	}

	items := make([]UpdateCollectionItem, 0, len(rawItems))
	for _, raw := range rawItems {
		items = append(items, UpdateCollectionItem{
			EntityID:   raw.ID,
			Payload:    json.RawMessage(raw.Payload),
			Transition: raw.Transition,
			IfMatch:    raw.IfMatch,
		})
	}

	// Empty body: defer to the service layer's empty-batch contract
	// (it returns 400 BAD_REQUEST, see UpdateEntityCollection).
	if len(items) == 0 {
		_, err := h.UpdateEntityCollection(r.Context(), items)
		if err != nil {
			common.WriteError(w, r, classifyError(err))
			return
		}
		// Service-layer contract precludes nil-error empty result, but be
		// defensive — emit empty array rather than nil.
		common.WriteJSON(w, http.StatusOK, []collectionChunkResult{})
		return
	}

	results := make([]collectionChunkResult, 0)
	for chunkIdx, start := 0, 0; start < len(items); chunkIdx, start = chunkIdx+1, start+window {
		end := start + window
		if end > len(items) {
			end = len(items)
		}
		result, err := h.UpdateEntityCollection(r.Context(), items[start:end])
		if err != nil {
			appErr := classifyError(err)
			if chunkIdx == 0 {
				common.WriteError(w, r, appErr)
				return
			}
			results = append(results, collectionChunkResult{
				Error: &collectionChunkError{
					Code:       appErr.Code,
					Message:    appErr.Message,
					ChunkIndex: chunkIdx,
				},
			})
			common.WriteJSON(w, http.StatusOK, results)
			return
		}
		entry := collectionChunkResult{
			TransactionID: result.TransactionID,
			EntityIDs:     result.EntityIDs,
		}
		if len(result.Failed) > 0 {
			entry.Failed = make([]collectionChunkItemFailure, 0, len(result.Failed))
			for _, f := range result.Failed {
				entry.Failed = append(entry.Failed, collectionChunkItemFailure{
					EntityID: f.EntityID,
					Error: collectionChunkItemErr{
						Code:      f.Code,
						Message:   f.Message,
						ItemIndex: f.ItemIndex,
					},
				})
			}
		}
		results = append(results, entry)
	}
	common.WriteJSON(w, http.StatusOK, results)
}

func (h *Handler) UpdateSingleWithLoopback(w http.ResponseWriter, r *http.Request, format genapi.UpdateSingleWithLoopbackParamsFormat, entityId openapi_types.UUID, params genapi.UpdateSingleWithLoopbackParams) {
	// Read request body (with size limit) -- outside transaction.
	r.Body = http.MaxBytesReader(w, r.Body, maxEntityBodySize)
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "failed to read body"))
		return
	}

	ifMatch := ""
	if params.IfMatch != nil {
		ifMatch = *params.IfMatch
	}

	result, err := h.UpdateEntity(r.Context(), UpdateEntityInput{
		EntityID:   entityId.String(),
		Format:     string(format),
		Data:       bodyBytes,
		Transition: "", // loopback
		IfMatch:    ifMatch,
	})
	if err != nil {
		common.WriteError(w, r, classifyError(err))
		return
	}

	resp := map[string]any{
		"transactionId": result.TransactionID,
		"entityIds":     result.EntityIDs,
	}
	common.WriteJSON(w, http.StatusOK, resp)
}

func (h *Handler) UpdateSingle(w http.ResponseWriter, r *http.Request, format genapi.UpdateSingleParamsFormat, entityId openapi_types.UUID, transition string, params genapi.UpdateSingleParams) {
	// Read request body (with size limit) -- outside transaction.
	r.Body = http.MaxBytesReader(w, r.Body, maxEntityBodySize)
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "failed to read body"))
		return
	}

	ifMatch := ""
	if params.IfMatch != nil {
		ifMatch = *params.IfMatch
	}

	result, err := h.UpdateEntity(r.Context(), UpdateEntityInput{
		EntityID:   entityId.String(),
		Format:     string(format),
		Data:       bodyBytes,
		Transition: transition,
		IfMatch:    ifMatch,
	})
	if err != nil {
		common.WriteError(w, r, classifyError(err))
		return
	}

	resp := map[string]any{
		"transactionId": result.TransactionID,
		"entityIds":     result.EntityIDs,
	}
	common.WriteJSON(w, http.StatusOK, resp)
}
