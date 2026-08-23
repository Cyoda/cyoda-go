package audit

import (
	"errors"
	"net/http"
	"sort"
	"strconv"
	"time"

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
	events := make([]map[string]any, 0)

	// EntityChange events from version history.
	if includeEntityChange {
		callerTenant := common.TenantFromContext(ctx)
		for _, v := range versions {
			event := map[string]any{
				"auditEventType": "EntityChange",
				"changeType":     common.CanonicalChangeType(v.ChangeType),
				"severity":       "INFO",
				"utcTime":        v.Timestamp.UTC().Format(time.RFC3339Nano),
				"microsTime":     v.Timestamp.UnixMicro(),
				"system":         false,
				"entityId":       entityId.String(),
			}
			if v.TransactionID != "" {
				event["transactionId"] = v.TransactionID
			}
			if v.User != "" {
				actor := map[string]any{
					"id":   v.User,
					"name": v.User,
				}
				if callerTenant != "" {
					actor["legalId"] = callerTenant
				}
				event["actor"] = actor
			}
			events = append(events, event)
		}
	}

	// StateMachine events from SM audit store.
	if includeStateMachine {
		smStore, smErr := h.factory.StateMachineAuditStore(ctx)
		if smErr == nil {
			smEvents, smErr := smStore.GetEvents(ctx, entityId.String())
			if smErr == nil {
				for _, smEvent := range smEvents {
					event := map[string]any{
						"auditEventType": "StateMachine",
						"eventType":      string(smEvent.EventType),
						"severity":       "INFO",
						"utcTime":        smEvent.Timestamp.UTC().Format(time.RFC3339Nano),
						"microsTime":     smEvent.Timestamp.UnixMicro(),
						"entityId":       smEvent.EntityID,
						"details":        smEvent.Details,
						"data":           smEvent.Data,
					}
					if smEvent.TransactionID != "" {
						event["transactionId"] = smEvent.TransactionID
					}
					if smEvent.State != "" {
						event["state"] = smEvent.State
					}
					events = append(events, event)
				}
			}
		}
	}

	// Sort by timestamp: newest first.
	sort.Slice(events, func(i, j int) bool {
		tsI, _ := time.Parse(time.RFC3339Nano, events[i]["utcTime"].(string))
		tsJ, _ := time.Parse(time.RFC3339Nano, events[j]["utcTime"].(string))
		return tsI.After(tsJ)
	})

	// Apply filters.
	if params.Severity != nil {
		requested := string(*params.Severity)
		filtered := make([]map[string]any, 0, len(events))
		for _, ev := range events {
			if sev, ok := ev["severity"].(string); ok && sev == requested {
				filtered = append(filtered, ev)
			}
		}
		events = filtered
	}

	if params.FromUtcTime != nil {
		from := *params.FromUtcTime
		filtered := make([]map[string]any, 0, len(events))
		for _, ev := range events {
			ts, _ := time.Parse(time.RFC3339Nano, ev["utcTime"].(string))
			if !ts.Before(from) {
				filtered = append(filtered, ev)
			}
		}
		events = filtered
	}

	if params.ToUtcTime != nil {
		to := *params.ToUtcTime
		filtered := make([]map[string]any, 0, len(events))
		for _, ev := range events {
			ts, _ := time.Parse(time.RFC3339Nano, ev["utcTime"].(string))
			if ts.Before(to) {
				filtered = append(filtered, ev)
			}
		}
		events = filtered
	}

	if params.TransactionId != nil {
		txFilter := params.TransactionId.String()
		filtered := make([]map[string]any, 0, len(events))
		for _, ev := range events {
			if txID, ok := ev["transactionId"].(string); ok && txID == txFilter {
				filtered = append(filtered, ev)
			}
		}
		events = filtered
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
	total := len(events)
	start := cursor
	if start > total {
		start = total
	}
	end := start + limit
	if end > total {
		end = total
	}
	page := events[start:end]
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
			event := map[string]any{
				"auditEventType": "StateMachine",
				"eventType":      string(smEvent.EventType),
				"severity":       "INFO",
				"utcTime":        smEvent.Timestamp.UTC().Format(time.RFC3339Nano),
				"microsTime":     smEvent.Timestamp.UnixMicro(),
				"entityId":       smEvent.EntityID,
				"details":        smEvent.Details,
				"data":           smEvent.Data,
			}
			if smEvent.TransactionID != "" {
				event["transactionId"] = smEvent.TransactionID
			}
			if smEvent.State != "" {
				event["state"] = smEvent.State
			}
			common.WriteJSON(w, http.StatusOK, event)
			return
		}
	}

	common.WriteError(w, r, common.Operational(http.StatusNotFound, common.ErrCodeEntityNotFound, "finished event not found"))
}
