package entity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/ingest"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda-platform/cyoda-go/internal/domain/pagination"
	wfengine "github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
)

// maxEntityBodySize is the maximum allowed request body size for entity operations (10 MB).
const maxEntityBodySize = 10 * 1024 * 1024

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
	gate    *txgate.Registry
}

func New(factory spi.StoreFactory, txMgr spi.TransactionManager, uuids spi.UUIDGenerator, engine *wfengine.Engine, gate *txgate.Registry) *Handler {
	return &Handler{factory: factory, txMgr: txMgr, uuids: uuids, engine: engine, gate: gate}
}

// beginOrJoin decides whether this inbound request OWNS a fresh transaction or
// PARTICIPATES in a transaction already on ctx.
//
// A joined tx on ctx (spi.GetTransaction(ctx) != nil) means we are servicing a
// routed compute-node callback that a later task joined onto the owner's tx
// (#287). In that case we return the joined tx's ID with owned=false and DO NOT
// Begin — the write lands in the shared buffer for the owner to commit. When
// there is no joined tx (the normal inbound case) we Begin our own tx and
// return owned=true. The txCtx returned in the joined case is the caller's ctx
// unchanged (it already carries the TransactionState); in the owned case it is
// the Begin-derived context.
func (h *Handler) beginOrJoin(ctx context.Context) (string, context.Context, bool, error) {
	if tx := spi.GetTransaction(ctx); tx != nil {
		return tx.ID, ctx, false, nil
	}
	txID, txCtx, err := h.txMgr.Begin(ctx)
	return txID, txCtx, true, err
}

// acquireJoinedGate acquires the per-tx gate for a joined (owned==false) call
// and installs a suspendable handle on txCtx so the engine can release the gate
// across a blocking external dispatch (SYNC processor / FUNCTION criterion) and
// re-acquire it afterward. This generalises the owner's H3 invariant — "never
// hold the gate across engine.Execute" — to the joined-callback path: without
// it, a depth-2 cascade (a joined callback whose own SYNC processor drives a
// further joined write on the same tx) hold-and-waits on the non-reentrant gate
// and deadlocks until the 30s dispatch timeout.
//
// It returns the ctx to pass to engine.Execute and a release func the caller
// MUST defer. Both the returned release closure and the installed handle alias
// the same release variable, so a mid-dispatch Suspend/resume that re-acquires
// the gate is observed by the caller's deferred release.
func (h *Handler) acquireJoinedGate(txCtx context.Context, txID string) (context.Context, func()) {
	release := h.gate.Acquire(txID)
	txCtx, _ = txgate.WithHeld(txCtx, h.gate, txID, &release)
	return txCtx, func() { release() }
}

// commitOwned commits the transaction only when this request owns it. For a
// joined callback (owned==false) the owner is responsible for the commit, so
// this is a no-op. Callers gate the whole final Save+Commit critical section
// (see the per-flow finalize blocks): the gate is acquired by the flow around
// the final buffer mutation and released after this commit, so commitOwned
// itself must NOT touch the gate (the gate is a non-reentrant per-tx mutex).
//
// The commit runs shielded via common.ShieldedCommit — WithoutCancel plus its
// own bounded budget — so a client-requested deadline or disconnect on ctx
// can never interrupt a commit already in flight (spec D2: an interrupted
// commit is an in-doubt outcome, never a rollback-able one).
// common.ShieldedCommit marks the narrow case where the commit's own shielded
// ctx (budget/cancellation) is what failed the commit, so it can never be
// misclassified as the client's clean 408 "nothing was committed" at the
// handler seam; a commit that fails cleanly while the shielded ctx is still
// live (e.g. spi.ErrConflict) is unaffected and keeps its existing
// classification. Shared with the workflow engine's flushAndCommitSegment —
// the other call site that commits under this same shielding.
func (h *Handler) commitOwned(ctx context.Context, txID string, owned bool) error {
	if !owned {
		return nil
	}
	return common.ShieldedCommit(ctx, func(commitCtx context.Context) error {
		return h.txMgr.Commit(commitCtx, txID)
	})
}

// validateOrExtend validates parsedData against the model schema. When
// changeLevel is set, it computes an additive schema delta via schema.Diff
// and appends it to the model's extension log via ModelStore.ExtendSchema.
// That call participates in the ambient entity transaction, so visibility
// is commit-bound. Writes whose data already fits the schema (nil delta —
// the steady state) touch no "models" row and cannot contend; writes that
// genuinely extend the same model serialise per (tenant, model) inside the
// plugin, and a concurrent extender surfaces a retryable conflict rather
// than folding a savepoint over a delta it cannot yet see.
// Returns an error on validation or extension failure.
// ValidateWithRefresh runs strict schema validation with a bounded
// refresh-on-stale safety net. One refresh attempt, only on unknown-
// schema-element errors — the signal that our cached schema is behind
// a peer's ExtendSchema. Other validation failures surface directly.
// Stores that don't implement RefreshAndGet (no caching layer) skip
// the refresh and return the original errors. See spec §4.3.
//
// Both model-store reads are marked with ingest.ErrInternalSchema on failure.
// Callers classify this function's errors with classifyValidateOrExtendErr,
// whose catch-all is a 400 BAD_REQUEST carrying err.Error() verbatim: unmarked,
// a store outage would be reported to the caller as a fault in THEIR payload,
// with the driver's own text and SQLSTATE in the response body. Neither read
// can fail for a reason the caller caused, so both are 5xx-with-a-ticket.
func (h *Handler) ValidateWithRefresh(ctx context.Context, modelStore spi.ModelStore, ref spi.ModelRef, data any) error {
	desc, err := modelStore.Get(ctx, ref)
	if err != nil {
		return fmt.Errorf("%w: load model %s/%s: %w", ingest.ErrInternalSchema, ref.EntityName, ref.ModelVersion, err)
	}
	errs := ingest.ValidateDescriptor(desc, data)
	if errs == nil {
		return nil
	}
	if !schema.HasUnknownSchemaElement(errs) {
		return ingest.ValidationErrorsToError(errs)
	}
	refresher, ok := modelStore.(interface {
		RefreshAndGet(context.Context, spi.ModelRef) (*spi.ModelDescriptor, error)
	})
	if !ok {
		return ingest.ValidationErrorsToError(errs) // plugin has no cache
	}
	freshDesc, rErr := refresher.RefreshAndGet(ctx, ref)
	if rErr != nil {
		return fmt.Errorf("%w: refresh model %s/%s: %w", ingest.ErrInternalSchema, ref.EntityName, ref.ModelVersion, rErr)
	}
	if errs2 := ingest.ValidateDescriptor(freshDesc, data); errs2 != nil {
		return ingest.ValidationErrorsToError(errs2)
	}
	return nil
}

// classifyBeginErr maps a transaction-Begin failure to a status code.
//
// common.Internal now recognises the storage-unavailability marker itself, so
// this is no longer the only thing standing between a 503 and an opaque 500. It
// is kept as the named entry point every Begin site calls, and because the
// message it would otherwise pass — "failed to begin transaction" — is not what
// a transient pool outage should be reported as.
func classifyBeginErr(err error) *common.AppError {
	if appErr := common.StorageUnavailable(err); appErr != nil {
		return appErr
	}
	return common.Internal("failed to begin transaction", err)
}

// classifyValidateOrExtendErr determines whether a validateOrExtend error is
// internal (5xx) or operational (4xx) and returns the appropriate AppError.
//
// Classification is sentinel-based to keep it robust against wording drift
// in the wrap strings:
//
//   - ErrPolymorphicSlot      → 4xx POLYMORPHIC_SLOT (client normalizes payload)
//   - *ingest.IncompatibleTypeError  → 4xx INCOMPATIBLE_TYPE with structured Props
//     (fieldPath, expectedType, actualType) — Cloud's
//     FoundIncompatibleTypeWithEntityModelException equivalent
//   - ingest.ErrInternalSchema       → 5xx with logged ticket (codec/diff/store failure)
//   - anything else           → 4xx BAD_REQUEST (change-level violation,
//     other validation failure, malformed walk input)
//
// The catch-all puts err.Error() in the response body verbatim — a 4xx carries
// full domain detail by contract. That makes it a leak the moment a feeder
// hands it something infrastructural, so every feeder marks its store failures
// with ErrInternalSchema: validateOrExtend does, and so does ValidateWithRefresh
// (a ready-to-use wrapper with no production call site yet — the marking is what
// lets it be wired to a door without re-opening the hole).
func classifyValidateOrExtendErr(err error) *common.AppError {
	// Pass-through: validateOrExtend may return a *common.AppError directly
	// for pre-classified operational errors (e.g. unique-key widening guard).
	var preClassified *common.AppError
	if errors.As(err, &preClassified) {
		return preClassified
	}
	if errors.Is(err, schema.ErrPolymorphicSlot) {
		return common.Operational(http.StatusBadRequest, common.ErrCodePolymorphicSlot, err.Error())
	}
	var incompatErr *ingest.IncompatibleTypeError
	if errors.As(err, &incompatErr) {
		appErr := common.Operational(http.StatusBadRequest, common.ErrCodeIncompatibleType, err.Error())
		expected := make([]string, len(incompatErr.ExpectedTypes))
		for i, dt := range incompatErr.ExpectedTypes {
			expected[i] = dt.String()
		}
		props := map[string]any{
			"fieldPath":    incompatErr.Path,
			"expectedType": expected,
			"actualType":   incompatErr.ActualType.String(),
		}
		if incompatErr.EntityName != "" {
			props["entityName"] = incompatErr.EntityName
		}
		if incompatErr.EntityVersion != "" {
			props["entityVersion"] = incompatErr.EntityVersion
		}
		appErr.Props = props
		return appErr
	}
	if errors.Is(err, ingest.ErrInternalSchema) {
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

	opCtx, cancelTimeout, paramErr := resolveRequestTimeout(r.Context(), params.TransactionTimeoutMillis)
	if paramErr != nil {
		common.WriteError(w, r, paramErr)
		return
	}
	defer cancelTimeout()

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
			result, err := h.CreateEntityCollection(opCtx, items)
			if err != nil {
				if appErr := common.ClassifyRequestTimeout(opCtx, err, common.ErrCodeTransactionTimeout); appErr != nil {
					common.WriteError(w, r, appErr)
					return
				}
				common.WriteError(w, r, classifyError(err))
				return
			}
			common.WriteJSON(w, http.StatusOK, []collectionChunkResult{{
				TransactionID: result.TransactionID,
				EntityIDs:     result.EntityIDs,
			}})
			return
		}

		results, firstChunkErr := h.runChunkedCreate(opCtx, items, window)
		if firstChunkErr != nil {
			common.WriteError(w, r, firstChunkErr)
			return
		}
		common.WriteJSON(w, http.StatusOK, results)
		return
	}

	result, err := h.CreateEntity(opCtx, CreateEntityInput{
		EntityName:   entityName,
		ModelVersion: fmt.Sprintf("%d", modelVersion),
		Format:       string(format),
		Data:         bodyBytes,
	})
	if err != nil {
		if appErr := common.ClassifyRequestTimeout(opCtx, err, common.ErrCodeTransactionTimeout); appErr != nil {
			common.WriteError(w, r, appErr)
			return
		}
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
		// ParseInt with a 32-bit width: a model version that does not fit in
		// int32 is reported as 0 rather than silently truncated by a int->int32
		// conversion. The error is deliberately ignored — a malformed stored
		// version should not fail the whole statistics response.
		ver64, _ := strconv.ParseInt(s.ModelVersion, 10, 32)
		ver := int32(ver64)
		result = append(result, genapi.ModelStatsDto{
			ModelName:    s.ModelName,
			ModelVersion: ver,
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
		// ParseInt with a 32-bit width: a model version that does not fit in
		// int32 is reported as 0 rather than silently truncated by a int->int32
		// conversion. The error is deliberately ignored — a malformed stored
		// version should not fail the whole statistics response.
		ver64, _ := strconv.ParseInt(s.ModelVersion, 10, 32)
		ver := int32(ver64)
		result = append(result, genapi.ModelStateStatsDto{
			ModelName:    s.ModelName,
			ModelVersion: ver,
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
			"changeType":   common.CanonicalChangeType(e.ChangeType),
			"timeOfChange": e.TimeOfChange,
			"user":         e.User,
		}
		if e.HasEntity {
			entry["transactionId"] = e.TransactionID
		}
		if e.AttributedKind != "" {
			entry["attributedKind"] = e.AttributedKind
		}
		if e.Executor.ID != "" {
			entry["executedBy"] = map[string]any{"id": e.Executor.ID, "kind": string(e.Executor.Kind)}
		}
		result = append(result, entry)
	}

	common.WriteJSON(w, http.StatusOK, result)
}

func (h *Handler) DeleteEntities(w http.ResponseWriter, r *http.Request, entityName string, modelVersion int32, params genapi.DeleteEntitiesParams) {
	// Resolve transactionSize BEFORE reading the body (spec D4/D7): a
	// validation failure must not read (let alone act on) the request body.
	// A joined request (spi.GetTransaction(ctx) != nil — how a routed
	// compute-node callback presents at param-resolution time) is rejected
	// rather than silently honoring or ignoring transactionSize: honoring it
	// would let a participant unilaterally fragment a transaction the owner
	// still controls.
	batchSize := 0
	if params.TransactionSize != nil {
		if *params.TransactionSize < 1 {
			common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest,
				"transactionSize must be a positive integer"))
			return
		}
		if spi.GetTransaction(r.Context()) != nil {
			common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest,
				"transactionSize is not supported on a request that joins an open transaction"))
			return
		}
		batchSize = int(*params.TransactionSize)
	}

	condBody, err := io.ReadAll(r.Body)
	if err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "failed to read request body"))
		return
	}

	verbose := params.Verbose != nil && *params.Verbose
	result, err := h.DeleteEntitiesConditional(r.Context(), entityName, fmt.Sprintf("%d", modelVersion), condBody, params.PointInTime, verbose, batchSize)
	if err != nil {
		if errors.Is(err, ErrInvalidCondition) {
			common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeInvalidCondition, err.Error()))
			return
		}
		common.WriteError(w, r, classifyError(err))
		return
	}

	// StreamDeleteResult: single object with entityModelClassId, deleteResult,
	// and optional ids (verbose). numberOfEntitites = matched, ...Removed =
	// actually deleted (decoupled — a condition may match more than it removes
	// if a per-id delete fails). Reconciled to the per-finding policy (design §2).
	deleteResult := map[string]any{
		"idToError":                result.IDToError,
		"numberOfEntitites":        result.MatchedCount,
		"numberOfEntititesRemoved": result.RemovedCount,
	}
	resp := map[string]any{
		"entityModelClassId": result.EntityModelID,
		"deleteResult":       deleteResult,
	}
	if verbose {
		resp["ids"] = result.IDs
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
	}, params.PointInTime)
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

// resolveRequestTimeout applies spec D7/D10 for the write ops: validate,
// reject on a joined transaction, attach the feature-owned deadline.
//
// A nil millis is a no-op — (ctx, no-op cancel, nil) — so a caller that never
// sends transactionTimeoutMillis observes zero behavior change (the PATCH
// contract). A joined (tx-token'd) request is rejected rather than silently
// ignored: spi.GetTransaction(ctx) != nil is how a routed compute-node
// callback presents at param-resolution time (see beginOrJoin), and honoring
// a client-supplied deadline on a participant would let it unilaterally
// abandon a transaction the owner still controls.
func resolveRequestTimeout(ctx context.Context, millis *int64) (context.Context, context.CancelFunc, *common.AppError) {
	if millis == nil {
		return ctx, func() {}, nil
	}
	if appErr := common.ValidateRequestTimeoutMillis(*millis); appErr != nil {
		return nil, nil, appErr
	}
	if spi.GetTransaction(ctx) != nil {
		return nil, nil, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest,
			"transactionTimeoutMillis is not supported on a request that joins an open transaction")
	}
	ctx, cancel := common.WithRequestTimeout(ctx, *millis)
	return ctx, cancel, nil
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
	TransactionID string `json:"transactionId,omitempty"`
	// EntityIDs is intentionally NOT omitempty so the wire shape stays
	// stable across "fully successful" and "all-stale per-item-isolated"
	// chunks (issue #228). Construction sites must initialise this non-nil
	// (e.g. `make([]string, 0)`) so json.Marshal emits `entityIds: []`
	// rather than `null` for a chunk with zero successful items. This
	// matches the documented contract in OpenAPI / cmd/cyoda/help/content/crud.md.
	EntityIDs []string                     `json:"entityIds"`
	Error     *collectionChunkError        `json:"error,omitempty"`
	Failed    []collectionChunkItemFailure `json:"failed,omitempty"`
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
	EntityID string                 `json:"entityId"`
	Error    collectionChunkItemErr `json:"error"`
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

		// Generic cancellation check at the iteration head (spec D9) — fires
		// on ANY ctx cancellation, not only our own feature deadline. Routed
		// through the identical error-element path a genuine chunk failure
		// takes (D3): a later-chunk expiry never becomes a request-level
		// error, it marks chunkIndex and stops without attempting the chunk.
		var result *EntityTransactionResult
		var err error
		if ctxErr := ctx.Err(); ctxErr != nil {
			err = fmt.Errorf("operation aborted: %w", ctxErr)
		} else {
			result, err = h.CreateEntityCollection(ctx, items[start:end])
		}
		if err != nil {
			var appErr *common.AppError
			if tErr := common.ClassifyRequestTimeout(ctx, err, common.ErrCodeTransactionTimeout); tErr != nil {
				appErr = tErr
			} else {
				appErr = classifyError(err)
			}
			if chunkIdx == 0 {
				return nil, appErr
			}
			// A later-chunk expiry surfaces as a TRANSACTION_TIMEOUT-coded
			// error element, never a request-level 408 (spec D3): chunks
			// before this one already committed and are durable.
			results = append(results, collectionChunkResult{
				EntityIDs: make([]string, 0),
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

	opCtx, cancelTimeout, paramErr := resolveRequestTimeout(r.Context(), params.TransactionTimeoutMillis)
	if paramErr != nil {
		common.WriteError(w, r, paramErr)
		return
	}
	defer cancelTimeout()

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
		result, err := h.CreateEntityCollection(opCtx, items)
		if err != nil {
			if appErr := common.ClassifyRequestTimeout(opCtx, err, common.ErrCodeTransactionTimeout); appErr != nil {
				common.WriteError(w, r, appErr)
				return
			}
			common.WriteError(w, r, classifyError(err))
			return
		}
		common.WriteJSON(w, http.StatusOK, []collectionChunkResult{{
			TransactionID: result.TransactionID,
			EntityIDs:     result.EntityIDs,
		}})
		return
	}

	results, firstChunkErr := h.runChunkedCreate(opCtx, items, window)
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

	opCtx, cancelTimeout, paramErr := resolveRequestTimeout(r.Context(), params.TransactionTimeoutMillis)
	if paramErr != nil {
		common.WriteError(w, r, paramErr)
		return
	}
	defer cancelTimeout()

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
		_, err := h.UpdateEntityCollection(opCtx, items)
		if err != nil {
			if appErr := common.ClassifyRequestTimeout(opCtx, err, common.ErrCodeTransactionTimeout); appErr != nil {
				common.WriteError(w, r, appErr)
				return
			}
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

		// Generic cancellation check at the iteration head (spec D9) — see
		// runChunkedCreate's identical comment; same D3 routing applies here.
		var result *UpdateCollectionResult
		var err error
		if ctxErr := opCtx.Err(); ctxErr != nil {
			err = fmt.Errorf("operation aborted: %w", ctxErr)
		} else {
			result, err = h.UpdateEntityCollection(opCtx, items[start:end])
		}
		if err != nil {
			var appErr *common.AppError
			if tErr := common.ClassifyRequestTimeout(opCtx, err, common.ErrCodeTransactionTimeout); tErr != nil {
				appErr = tErr
			} else {
				appErr = classifyError(err)
			}
			if chunkIdx == 0 {
				common.WriteError(w, r, appErr)
				return
			}
			// A later-chunk expiry surfaces as a TRANSACTION_TIMEOUT-coded
			// error element, never a request-level 408 (spec D3): chunks
			// before this one already committed and are durable.
			results = append(results, collectionChunkResult{
				EntityIDs: make([]string, 0),
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
	opCtx, cancelTimeout, paramErr := resolveRequestTimeout(r.Context(), params.TransactionTimeoutMillis)
	if paramErr != nil {
		common.WriteError(w, r, paramErr)
		return
	}
	defer cancelTimeout()

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

	result, err := h.UpdateEntity(opCtx, UpdateEntityInput{
		EntityID:   entityId.String(),
		Format:     string(format),
		Data:       bodyBytes,
		Transition: "", // loopback
		IfMatch:    ifMatch,
	})
	if err != nil {
		if appErr := common.ClassifyRequestTimeout(opCtx, err, common.ErrCodeTransactionTimeout); appErr != nil {
			common.WriteError(w, r, appErr)
			return
		}
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
	opCtx, cancelTimeout, paramErr := resolveRequestTimeout(r.Context(), params.TransactionTimeoutMillis)
	if paramErr != nil {
		common.WriteError(w, r, paramErr)
		return
	}
	defer cancelTimeout()

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

	result, err := h.UpdateEntity(opCtx, UpdateEntityInput{
		EntityID:   entityId.String(),
		Format:     string(format),
		Data:       bodyBytes,
		Transition: transition,
		IfMatch:    ifMatch,
	})
	if err != nil {
		if appErr := common.ClassifyRequestTimeout(opCtx, err, common.ErrCodeTransactionTimeout); appErr != nil {
			common.WriteError(w, r, appErr)
			return
		}
		common.WriteError(w, r, classifyError(err))
		return
	}

	resp := map[string]any{
		"transactionId": result.TransactionID,
		"entityIds":     result.EntityIDs,
	}
	common.WriteJSON(w, http.StatusOK, resp)
}

// PatchSingleWithLoopback handles PATCH /entity/{format}/{entityId} (loopback).
func (h *Handler) PatchSingleWithLoopback(w http.ResponseWriter, r *http.Request, format genapi.PatchSingleWithLoopbackParamsFormat, entityId openapi_types.UUID, params genapi.PatchSingleWithLoopbackParams) {
	h.patch(w, r, string(format), entityId, "", params.IfMatch, params.TransactionTimeoutMillis)
}

// PatchSingle handles PATCH /entity/{format}/{entityId}/{transition}.
func (h *Handler) PatchSingle(w http.ResponseWriter, r *http.Request, format genapi.PatchSingleParamsFormat, entityId openapi_types.UUID, transition string, params genapi.PatchSingleParams) {
	h.patch(w, r, string(format), entityId, transition, params.IfMatch, params.TransactionTimeoutMillis)
}

// patch is the shared PATCH implementation. Error precedence: media-type/format
// (415) -> If-Match presence (428) -> transactionTimeoutMillis validation (400) ->
// service (404/412/409/501/4xx).
func (h *Handler) patch(w http.ResponseWriter, r *http.Request, format string, entityId openapi_types.UUID, transition string, ifMatchHeader *string, millis *int64) {
	if format != "JSON" {
		common.WriteError(w, r, common.Operational(http.StatusUnsupportedMediaType, common.ErrCodeUnsupportedMediaType, "patch supports the JSON format only"))
		return
	}
	patchFormat, ok := patchFormatFromContentType(r.Header.Get("Content-Type"))
	if !ok {
		common.WriteError(w, r, common.Operational(http.StatusUnsupportedMediaType, common.ErrCodeUnsupportedMediaType,
			"unsupported Content-Type; use application/merge-patch+json or application/json-patch+json"))
		return
	}
	if ifMatchHeader == nil {
		common.WriteError(w, r, common.Operational(http.StatusPreconditionRequired, common.ErrCodePreconditionRequired,
			"missing If-Match: send If-Match: <transactionId> from your last GET of this entity to patch safely, or If-Match: * to explicitly accept last-writer-wins"))
		return
	}
	opCtx, cancelTimeout, paramErr := resolveRequestTimeout(r.Context(), millis)
	if paramErr != nil {
		common.WriteError(w, r, paramErr)
		return
	}
	defer cancelTimeout()
	r.Body = http.MaxBytesReader(w, r.Body, maxEntityBodySize)
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "failed to read body"))
		return
	}
	result, err := h.PatchEntity(opCtx, PatchEntityInput{
		EntityID:    entityId.String(),
		Patch:       bodyBytes,
		PatchFormat: patchFormat,
		Transition:  transition,
		IfMatch:     *ifMatchHeader,
	})
	if err != nil {
		if appErr := common.ClassifyRequestTimeout(opCtx, err, common.ErrCodeTransactionTimeout); appErr != nil {
			common.WriteError(w, r, appErr)
			return
		}
		common.WriteError(w, r, classifyError(err))
		return
	}
	common.WriteJSON(w, http.StatusOK, map[string]any{
		"transactionId": result.TransactionID,
		"entityIds":     result.EntityIDs,
	})
}

// patchFormatFromContentType maps the request Content-Type to a patch dialect.
func patchFormatFromContentType(ct string) (string, bool) {
	if ct == "" {
		return "", false
	}
	mediaType, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return "", false
	}
	switch mediaType {
	case "application/merge-patch+json":
		return "MERGE_PATCH", true
	case "application/json-patch+json":
		return "JSON_PATCH", true
	default:
		return "", false
	}
}
