package search

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda-platform/cyoda-go/internal/domain/pagination"
)

const (
	maxSearchBodySize = 10 * 1024 * 1024 // 10 MiB
)

// jobLookupError maps a service-level error to a handler response. Job-not-
// found is reported as 404 + SEARCH_JOB_NOT_FOUND (issue #93); any other
// lookup error is treated as an internal failure.
func jobLookupError(err error) *common.AppError {
	if errors.Is(err, ErrSearchJobNotFound) {
		return common.Operational(http.StatusNotFound, common.ErrCodeSearchJobNotFound, err.Error())
	}
	return common.Internal("job lookup failed", err)
}

// Handler handles search-related HTTP endpoints.
type Handler struct {
	searchSvc *SearchService
	factory   spi.StoreFactory
}

// NewHandler creates a new search handler wired to the given SearchService.
func NewHandler(searchSvc *SearchService) *Handler {
	return &Handler{searchSvc: searchSvc}
}

// NewHandlerWithModel creates a search handler that additionally validates
// condition value types against the model schema before executing search.
// Pass a nil factory to disable condition-type validation (e.g. in tests
// that don't need it).
func NewHandlerWithModel(searchSvc *SearchService, factory spi.StoreFactory) *Handler {
	return &Handler{searchSvc: searchSvc, factory: factory}
}

// lookupModelSchema fetches the parsed model schema for the given entity/version.
// Returns nil (with no error) when the model is not found or cannot be parsed —
// callers treat this as "no type constraints available".
func (h *Handler) lookupModelSchema(r *http.Request, entityName string, modelVersion int32) *schema.ModelNode {
	if h.factory == nil {
		return nil
	}
	modelStore, err := h.factory.ModelStore(r.Context())
	if err != nil {
		return nil
	}
	ref := spi.ModelRef{
		EntityName:   entityName,
		ModelVersion: fmt.Sprintf("%d", modelVersion),
	}
	desc, err := modelStore.Get(r.Context(), ref)
	if err != nil || len(desc.Schema) == 0 {
		return nil
	}
	node, err := schema.Unmarshal(desc.Schema)
	if err != nil {
		return nil
	}
	return node
}

// validateConditionTypes checks all simple clauses in cond against the model
// schema for the given entity. Returns a non-nil AppError (HTTP 400) if any
// clause has a type-mismatched value. Returns nil when the model is not found
// or the condition has no type violations.
func (h *Handler) validateConditionTypes(r *http.Request, entityName string, modelVersion int32, cond predicate.Condition) *common.AppError {
	node := h.lookupModelSchema(r, entityName, modelVersion)
	if node == nil {
		return nil // model not found or unparseable — no constraints to apply
	}
	if err := ValidateConditionValueTypes(node, cond); err != nil {
		return common.Operational(http.StatusBadRequest, common.ErrCodeConditionTypeMismatch,
			err.Error())
	}
	return nil
}

// ---------------------------------------------------------------------------
// Direct (synchronous) search
// ---------------------------------------------------------------------------

func (h *Handler) SearchEntities(w http.ResponseWriter, r *http.Request, entityName string, modelVersion int32, params genapi.SearchEntitiesParams) {
	r.Body = http.MaxBytesReader(w, r.Body, maxSearchBodySize)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "failed to read request body"))
		return
	}

	cond, err := predicate.ParseCondition(body)
	if err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, fmt.Sprintf("invalid condition: %v", err)))
		return
	}
	if err := ValidateCondition(cond); err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, err.Error()))
		return
	}
	if appErr := h.validateConditionTypes(r, entityName, modelVersion, cond); appErr != nil {
		common.WriteError(w, r, appErr)
		return
	}

	opts := SearchOptions{
		PointInTime: params.PointInTime,
	}

	// Parse limit from string parameter.
	if params.Limit != nil {
		lim, err := strconv.Atoi(*params.Limit)
		if err != nil || lim < 0 {
			common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid limit"))
			return
		}
		// Reject (don't silently clamp): the async path does the same.
		// Silent clamping would hide misuse from clients and mask bugs
		// where a caller assumed a larger window than the server allows.
		if lim > pagination.MaxPageSize {
			common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, fmt.Sprintf("limit exceeds maximum %d", pagination.MaxPageSize)))
			return
		}
		opts.Limit = lim
	}

	modelRef := spi.ModelRef{
		EntityName:   entityName,
		ModelVersion: fmt.Sprintf("%d", modelVersion),
	}

	results, err := h.searchSvc.Search(r.Context(), modelRef, cond, opts)
	if err != nil {
		// Pre-execution validation (issue #77) returns a classified
		// *common.AppError directly; forward it so the 4xx surfaces
		// instead of being shrouded as a 5xx ticket.
		var appErr *common.AppError
		if errors.As(err, &appErr) {
			common.WriteError(w, r, appErr)
			return
		}
		common.WriteError(w, r, common.Internal("search failed", err))
		return
	}

	// Per canonical openapi-entity-search.yml line 587, sync search returns
	// application/x-ndjson — a stream of EntityResult JSON objects, one per line.
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	for _, e := range results {
		if err := enc.Encode(entityEnvelope(e)); err != nil {
			// Header is already written; we can only log and stop. The
			// client sees a truncated stream and a connection error,
			// which is the correct failure mode for a streaming endpoint.
			slog.Error("ndjson encode failed mid-stream",
				"pkg", "search", "error", err.Error())
			return
		}
	}
}

// ---------------------------------------------------------------------------
// Async search: submit
// ---------------------------------------------------------------------------

func (h *Handler) SubmitAsyncSearchJob(w http.ResponseWriter, r *http.Request, entityName string, modelVersion int32, params genapi.SubmitAsyncSearchJobParams) {
	r.Body = http.MaxBytesReader(w, r.Body, maxSearchBodySize)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "failed to read request body"))
		return
	}

	cond, err := predicate.ParseCondition(body)
	if err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, fmt.Sprintf("invalid condition: %v", err)))
		return
	}
	if err := ValidateCondition(cond); err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, err.Error()))
		return
	}
	if appErr := h.validateConditionTypes(r, entityName, modelVersion, cond); appErr != nil {
		common.WriteError(w, r, appErr)
		return
	}

	opts := SearchOptions{
		PointInTime: params.PointInTime,
	}

	modelRef := spi.ModelRef{
		EntityName:   entityName,
		ModelVersion: fmt.Sprintf("%d", modelVersion),
	}

	jobID, err := h.searchSvc.SubmitAsync(r.Context(), modelRef, cond, opts)
	if err != nil {
		// Pre-execution validation (issue #77) returns a classified
		// *common.AppError directly; forward it so the 4xx surfaces
		// instead of being shrouded as a 5xx ticket.
		var appErr *common.AppError
		if errors.As(err, &appErr) {
			common.WriteError(w, r, appErr)
			return
		}
		common.WriteError(w, r, common.Internal("failed to submit async search", err))
		return
	}

	// Return bare job ID string (matches Cyoda Cloud response).
	common.WriteJSON(w, http.StatusOK, jobID)
}

// ---------------------------------------------------------------------------
// Async search: status
// ---------------------------------------------------------------------------

func (h *Handler) GetAsyncSearchStatus(w http.ResponseWriter, r *http.Request, jobId openapi_types.UUID) {
	status, err := h.searchSvc.GetAsyncStatus(r.Context(), jobId.String())
	if err != nil {
		common.WriteError(w, r, jobLookupError(err))
		return
	}

	resp := buildStatusResponse(status)
	common.WriteJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// Async search: results
// ---------------------------------------------------------------------------

func (h *Handler) GetAsyncSearchResults(w http.ResponseWriter, r *http.Request, jobId openapi_types.UUID, params genapi.GetAsyncSearchResultsParams) {
	opts := ResultOptions{}

	pageSize := 1000 // default
	if params.PageSize != nil {
		ps, err := strconv.Atoi(*params.PageSize)
		if err != nil {
			common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid pageSize"))
			return
		}
		pageSize = ps
	}

	pageNumber := 0
	if params.PageNumber != nil {
		pn, err := strconv.Atoi(*params.PageNumber)
		if err != nil {
			common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid pageNumber"))
			return
		}
		pageNumber = pn
	}

	// Cap + overflow check via the shared helper (issue #98, #68 item
	// 10): rejects negative values, pageSize > MaxPageSize, pageNumber >
	// MaxPageNumber, and any pageNumber*pageSize that overflows int64.
	// Apply the cap to the *effective* pageSize (with the 1000 default
	// substituted for non-positive values) so the bound matches what is
	// actually used downstream.
	effectivePageSize := pageSize
	if effectivePageSize <= 0 {
		effectivePageSize = 1000
	}
	if vErr := pagination.ValidateOffset(int64(pageNumber), int64(effectivePageSize)); vErr != nil {
		common.WriteError(w, r, vErr.(*common.AppError))
		return
	}
	if params.PageSize != nil {
		opts.Limit = pageSize
	}
	if params.PageNumber != nil {
		opts.Offset = pageNumber * effectivePageSize
	}

	page, err := h.searchSvc.GetAsyncResults(r.Context(), jobId.String(), opts)
	if err != nil {
		if errors.Is(err, ErrSearchJobNotFound) {
			common.WriteError(w, r, jobLookupError(err))
			return
		}
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, fmt.Sprintf("failed to get results: %v", err)))
		return
	}

	envelopes := make([]map[string]any, 0, len(page.Results))
	for _, e := range page.Results {
		envelopes = append(envelopes, entityEnvelope(e))
	}

	if pageSize <= 0 {
		pageSize = 1000
	}
	totalPages := 0
	if page.Total > 0 {
		totalPages = (page.Total + pageSize - 1) / pageSize
	}

	resp := map[string]any{
		"content": envelopes,
		"page": map[string]any{
			"number":        pageNumber,
			"size":          pageSize,
			"totalElements": page.Total,
			"totalPages":    totalPages,
		},
	}

	common.WriteJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// Async search: cancel
// ---------------------------------------------------------------------------

func (h *Handler) CancelAsyncSearch(w http.ResponseWriter, r *http.Request, jobId openapi_types.UUID) {
	result, err := h.searchSvc.CancelAsync(r.Context(), jobId.String())
	if err != nil {
		common.WriteError(w, r, jobLookupError(err))
		return
	}

	if !result.Cancelled {
		// Job was already completed (SUCCESSFUL/FAILED) — Cloud returns 400.
		appErr := common.Operational(http.StatusBadRequest, common.ErrCodeSearchJobAlreadyTerminal,
			fmt.Sprintf("snapshot by id=%s is not running. current status=%s", jobId.String(), result.CurrentStatus))
		appErr.Props = map[string]any{
			"currentStatus": result.CurrentStatus,
			"snapshotId":    jobId.String(),
		}
		common.WriteError(w, r, appErr)
		return
	}

	resp := map[string]any{
		"isCancelled":            true,
		"cancelled":              true,
		"currentSearchJobStatus": result.CurrentStatus,
	}

	common.WriteJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func buildStatusResponse(status SearchJobStatus) map[string]any {
	resp := map[string]any{
		"searchJobStatus":       status.Status,
		"createTime":            status.CreateTime.UTC().Format(time.RFC3339Nano),
		"entitiesCount":         status.Total,
		"calculationTimeMillis": status.CalcTimeMs,
		"expirationDate":        status.CreateTime.Add(24 * time.Hour).UTC().Format(time.RFC3339Nano),
	}
	if status.FinishTime != nil {
		resp["finishTime"] = status.FinishTime.UTC().Format(time.RFC3339Nano)
	}
	return resp
}

func entityEnvelope(e *spi.Entity) map[string]any {
	meta := map[string]any{
		"id":             e.Meta.ID,
		"state":          e.Meta.State,
		"creationDate":   e.Meta.CreationDate.UTC().Format(time.RFC3339Nano),
		"lastUpdateTime": e.Meta.LastModifiedDate.UTC().Format(time.RFC3339Nano),
	}
	if e.Meta.TransactionID != "" {
		meta["transactionId"] = e.Meta.TransactionID
	}
	if e.Meta.TransitionForLatestSave != "" {
		meta["transitionForLatestSave"] = e.Meta.TransitionForLatestSave
	}

	var data any
	dec := json.NewDecoder(bytes.NewReader(e.Data))
	dec.UseNumber()
	_ = dec.Decode(&data)

	return map[string]any{
		"type": "ENTITY",
		"data": data,
		"meta": meta,
	}
}
