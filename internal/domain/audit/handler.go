package audit

import (
	"errors"
	"net/http"
	"slices"
	"strconv"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	openapi_types "github.com/oapi-codegen/runtime/types"

	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

type Handler struct {
	factory spi.StoreFactory
}

func New(factory spi.StoreFactory) *Handler {
	return &Handler{factory: factory}
}

func (h *Handler) SearchEntityAuditEvents(w http.ResponseWriter, r *http.Request, entityId openapi_types.UUID, params genapi.SearchEntityAuditEventsParams) {
	ctx := r.Context()

	// Determine which event types to include.
	// Default (no filter): include EntityChange and StateMachine but NOT System.
	// System is a reserved/commercial audit source retained in the eventType
	// enum contract — do NOT remove it from the OpenAPI spec even though OSS
	// backends never emit it.
	includeEntityChange := true
	includeStateMachine := true
	if params.EventType != nil {
		includeEntityChange = false
		includeStateMachine = false
		for _, et := range *params.EventType {
			switch et {
			case genapi.EntityChange:
				includeEntityChange = true
			case genapi.StateMachine:
				includeStateMachine = true
			}
		}
	}

	store, err := h.factory.EntityStore(ctx)
	if err != nil {
		common.WriteError(w, r, common.Internal("failed to get entity store", err))
		return
	}

	// Push the request's time window down to the store rather than fetching
	// the whole history and relying solely on the post-merge in-memory
	// filter below. The in-memory From/To filter still runs afterward
	// (unchanged) because it also has to cover StateMachine events, which
	// this store call cannot bound — GetVersionMetadata only ever sees the
	// EntityChange side of the merge. It also stays load-bearing for a
	// subtler reason: spi.VersionMetadataOptions.Until is documented
	// INCLUSIVE, while this endpoint's toUtcTime contract is
	// EXCLUSIVE-upper — dropping the in-memory filter would silently flip
	// an event stamped exactly at toUtcTime from excluded to included.
	opts := spi.VersionMetadataOptions{}
	if params.FromUtcTime != nil {
		opts.From = params.FromUtcTime
	}
	if params.ToUtcTime != nil {
		opts.Until = params.ToUtcTime
	}
	versions, err := store.GetVersionMetadata(ctx, entityId.String(), opts)
	if err != nil {
		if errors.Is(err, spi.ErrNotFound) {
			common.WriteError(w, r, common.Operational(http.StatusNotFound, common.ErrCodeEntityNotFound, "entity not found"))
		} else {
			common.WriteError(w, r, common.Internal("failed to get version history", err))
		}
		return
	}
	if len(versions) == 0 {
		common.WriteError(w, r, common.Operational(http.StatusNotFound, common.ErrCodeEntityNotFound, "entity not found"))
		return
	}

	// Build combined event list.
	items := make([]auditItem, 0)

	// EntityChange events from version history.
	if includeEntityChange {
		callerTenant := common.TenantFromContext(ctx)
		for _, v := range versions {
			items = append(items, entityChangeItem(v, entityId.String(), callerTenant))
		}
	}

	// StateMachine events from SM audit store.
	if includeStateMachine {
		smStore, smErr := h.factory.StateMachineAuditStore(ctx)
		if smErr == nil {
			smEvents, smErr := smStore.GetEvents(ctx, entityId.String())
			if smErr == nil {
				for _, smEvent := range smEvents {
					item, err := stateMachineItem(smEvent)
					if err != nil {
						common.WriteError(w, r, common.Internal("invalid state machine event", err))
						return
					}
					items = append(items, item)
				}
			}
		}
	}

	// One total order: newest instant first, then compareKeys' tie-break
	// chain (kind, then version or event id) so events of one instant come
	// back in a fixed, deterministic order across reads.
	slices.SortFunc(items, func(x, y auditItem) int { return compareKeys(x.key, y.key) })

	// Apply filters.
	if params.Severity != nil {
		requested := string(*params.Severity)
		filtered := make([]auditItem, 0, len(items))
		for _, item := range items {
			if sev, ok := item.body["severity"].(string); ok && sev == requested {
				filtered = append(filtered, item)
			}
		}
		items = filtered
	}

	if params.FromUtcTime != nil {
		from := *params.FromUtcTime
		filtered := make([]auditItem, 0, len(items))
		for _, item := range items {
			if !item.key.at.Before(from) {
				filtered = append(filtered, item)
			}
		}
		items = filtered
	}

	if params.ToUtcTime != nil {
		to := *params.ToUtcTime
		filtered := make([]auditItem, 0, len(items))
		for _, item := range items {
			if item.key.at.Before(to) {
				filtered = append(filtered, item)
			}
		}
		items = filtered
	}

	if params.TransactionId != nil {
		txFilter := params.TransactionId.String()
		filtered := make([]auditItem, 0, len(items))
		for _, item := range items {
			if txID, ok := item.body["transactionId"].(string); ok && txID == txFilter {
				filtered = append(filtered, item)
			}
		}
		items = filtered
	}

	// Parse pagination params.
	limit := 20
	if params.Limit != nil {
		parsed, err := strconv.Atoi(*params.Limit)
		if err != nil || parsed < 1 {
			common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid limit parameter"))
			return
		}
		if parsed > 1000 {
			parsed = 1000
		}
		limit = parsed
	}

	cursor := 0
	if params.Cursor != nil {
		if parsed, err := strconv.Atoi(*params.Cursor); err == nil && parsed >= 0 {
			cursor = parsed
		}
	}

	// Slice for pagination.
	total := len(items)
	start := cursor
	if start > total {
		start = total
	}
	end := start + limit
	if end > total {
		end = total
	}
	page := make([]map[string]any, end-start)
	for i, item := range items[start:end] {
		page[i] = item.body
	}
	hasNext := end < total

	paginationMap := map[string]any{
		"hasNext": hasNext,
	}
	if hasNext {
		paginationMap["nextCursor"] = strconv.Itoa(end)
	}

	resp := map[string]any{
		"items":      page,
		"pagination": paginationMap,
	}
	common.WriteJSON(w, http.StatusOK, resp)
}

func (h *Handler) GetStateMachineFinishedEvent(w http.ResponseWriter, r *http.Request, entityId openapi_types.UUID, transactionId openapi_types.UUID) {
	ctx := r.Context()

	smStore, err := h.factory.StateMachineAuditStore(ctx)
	if err != nil {
		common.WriteError(w, r, common.Internal("failed to get state machine audit store", err))
		return
	}

	// A read that failed is not a transaction with nothing recorded against it:
	// every backend reports the latter as an empty slice, which falls through to
	// the 404 at the end of this handler. An error here is the store failing, so
	// it routes to common.Internal — a storage outage then answers with a
	// retryable 503 instead of telling the caller its workflow left no trace.
	smEvents, err := smStore.GetEventsByTransaction(ctx, entityId.String(), transactionId.String())
	if err != nil {
		common.WriteError(w, r, common.Internal("failed to get state machine events", err))
		return
	}

	for _, smEvent := range smEvents {
		if smEvent.EventType == spi.SMEventFinished {
			item, err := stateMachineItem(smEvent)
			if err != nil {
				common.WriteError(w, r, common.Internal("invalid state machine event", err))
				return
			}
			common.WriteJSON(w, http.StatusOK, item.body)
			return
		}
	}

	common.WriteError(w, r, common.Operational(http.StatusNotFound, common.ErrCodeEntityNotFound, "finished event not found"))
}
