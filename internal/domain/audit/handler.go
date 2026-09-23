package audit

import (
	"errors"
	"net/http"
	"slices"
	"sort"
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

	// Validate request parameters (limit, cursor) before any store call, so
	// a malformed one answers 400 even against a nonexistent entity rather
	// than falling through to a 404 from the lookup below.
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

	var after *eventKey
	if params.Cursor != nil {
		k, err := decodeCursor(*params.Cursor)
		if err != nil {
			common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid cursor parameter"))
			return
		}
		after = &k
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
		smStore, err := h.factory.StateMachineAuditStore(ctx)
		if err != nil {
			common.WriteError(w, r, common.Internal("failed to get state machine audit store", err))
			return
		}
		// A failed read is not an entity without workflow events: answering
		// 200 without them would present a partial trail as complete.
		smEvents, err := smStore.GetEvents(ctx, entityId.String())
		if err != nil {
			common.WriteError(w, r, common.Internal("failed to get state machine events", err))
			return
		}
		for _, smEvent := range smEvents {
			item, err := stateMachineItem(smEvent)
			if err != nil {
				common.WriteError(w, r, common.Internal("invalid state machine event", err))
				return
			}
			items = append(items, item)
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

	// Page from a position in the total order, not an offset into this
	// query's filtered result set: a cursor from one filter still locates
	// the right position after the filter changes, and events committed
	// after the page was fetched are neither repeated nor skipped.
	start := 0
	if after != nil {
		start = sort.Search(len(items), func(i int) bool { return compareKeys(items[i].key, *after) > 0 })
	}
	end := min(start+limit, len(items))
	page := make([]map[string]any, end-start)
	for i, item := range items[start:end] {
		page[i] = item.body
	}
	hasNext := end < len(items)

	paginationMap := map[string]any{
		"hasNext": hasNext,
	}
	if hasNext {
		paginationMap["nextCursor"] = encodeCursor(items[end-1].key)
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

	// A joined callback's loopback save emits its own START/FINISH pair
	// (internal/domain/workflow/engine.go Loopback), so a transaction can
	// carry more than one STATE_MACHINE_FINISH event for this entity. The
	// one that sorts first under compareKeys — newest instant, then the
	// eventId's time field DESC, then bytes DESC — is the entity's final
	// state machine outcome in that transaction; every backend agrees on
	// this pick even though the store's listing order is unspecified when
	// two events share an instant (SQL orders only by timestamp).
	var latest *auditItem
	for _, smEvent := range smEvents {
		if smEvent.EventType != spi.SMEventFinished {
			continue
		}
		item, err := stateMachineItem(smEvent)
		if err != nil {
			common.WriteError(w, r, common.Internal("invalid state machine event", err))
			return
		}
		if latest == nil || compareKeys(item.key, latest.key) < 0 {
			latest = &item
		}
	}

	if latest == nil {
		common.WriteError(w, r, common.Operational(http.StatusNotFound, common.ErrCodeEntityNotFound, "finished event not found"))
		return
	}

	common.WriteJSON(w, http.StatusOK, latest.body)
}
