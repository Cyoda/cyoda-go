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

	versions, err := store.GetVersionHistory(ctx, entityId.String())
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
		for _, v := range versions {
			event := map[string]any{
				"auditEventType": "EntityChange",
				"changeType":     v.ChangeType,
				"severity":       "INFO",
				"utcTime":        v.Timestamp.UTC().Format(time.RFC3339Nano),
				"microsTime":     v.Timestamp.UnixMicro(),
				"system":         false,
			}
			if v.Entity != nil {
				event["entityId"] = v.Entity.Meta.ID
				if v.Entity.Meta.TransactionID != "" {
					event["transactionId"] = v.Entity.Meta.TransactionID
				}
			}
			if v.User != "" {
				actor := map[string]any{
					"id":   v.User,
					"name": v.User,
				}
				if v.Entity != nil {
					actor["legalId"] = string(v.Entity.Meta.TenantID)
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

	smEvents, err := smStore.GetEventsByTransaction(ctx, entityId.String(), transactionId.String())
	if err != nil {
		common.WriteError(w, r, common.Operational(http.StatusNotFound, common.ErrCodeEntityNotFound, "no events found"))
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
