// Package scheduledtask serves GET /scheduled-tasks: the caller's tenant's
// view of its scheduled-transition tasks.
package scheduledtask

import (
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

const (
	defaultLimit    = 20
	maxLimit        = 1000
	maxModelNameLen = 256 // characters, as OpenAPI maxLength counts them
)

// Handler lists scheduled tasks.
type Handler struct {
	factory spi.StoreFactory
}

// NewHandler returns a Handler that reads through factory's ScheduledTaskStore.
func NewHandler(factory spi.StoreFactory) *Handler {
	return &Handler{factory: factory}
}

// ListScheduledTasks answers GET /scheduled-tasks. The tenant is the token's;
// no parameter names one. Every parameter is checked before the store is
// touched, so a malformed request never costs a query.
func (h *Handler) ListScheduledTasks(w http.ResponseWriter, r *http.Request, params genapi.ListScheduledTasksParams) {
	ctx := r.Context()
	tenant := spi.MustGetUserContext(ctx).Tenant.ID

	q, appErr := queryFromParams(params)
	if appErr != nil {
		common.WriteError(w, r, appErr)
		return
	}
	store, err := h.factory.ScheduledTaskStore(ctx)
	if err != nil {
		common.WriteError(w, r, common.Internal("failed to get scheduled task store", err))
		return
	}
	page, err := store.Query(ctx, tenant, q)
	if err != nil {
		common.WriteError(w, r, common.Internal("failed to query scheduled tasks", err))
		return
	}

	items := make([]genapi.ScheduledTaskDto, 0, len(page.Items))
	for _, t := range page.Items {
		dto, err := toDTO(t)
		if err != nil {
			common.WriteError(w, r, common.Internal("failed to render scheduled task", err))
			return
		}
		items = append(items, dto)
	}
	resp := genapi.ScheduledTaskPageDto{
		Items:      items,
		Pagination: genapi.CursorPaginationInfoDto{HasNext: page.Next != nil},
	}
	if page.Next != nil {
		next := encodeCursor(*page.Next)
		resp.Pagination.NextCursor = &next
	}
	common.WriteJSON(w, http.StatusOK, resp)
}

func badRequest(msg string) *common.AppError {
	return common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, msg)
}

// queryFromParams applies the parameter rules of the published contract. A
// bad value is never echoed: the messages name the parameter and its rule.
func queryFromParams(p genapi.ListScheduledTasksParams) (spi.ScheduledTaskQuery, *common.AppError) {
	q := spi.ScheduledTaskQuery{Limit: defaultLimit}

	if p.Status != nil {
		for _, s := range *p.Status {
			switch st := spi.ScheduledTaskStatus(s); st {
			case spi.ScheduledTaskWaiting, spi.ScheduledTaskRunning, spi.ScheduledTaskFailed:
				q.Statuses = append(q.Statuses, st)
			default:
				return spi.ScheduledTaskQuery{}, badRequest("invalid status parameter: must be WAITING, RUNNING or FAILED")
			}
		}
	}
	if p.ModelName != nil {
		n := *p.ModelName
		if n == "" || !utf8.ValidString(n) || strings.ContainsRune(n, 0) || utf8.RuneCountInString(n) > maxModelNameLen {
			return spi.ScheduledTaskQuery{}, badRequest("invalid modelName parameter: must be 1 to 256 characters of valid UTF-8, without NUL")
		}
		q.ModelName = n
	}
	if p.ModelVersion != nil {
		if p.ModelName == nil {
			return spi.ScheduledTaskQuery{}, badRequest("modelVersion parameter requires modelName")
		}
		if *p.ModelVersion < 1 {
			return spi.ScheduledTaskQuery{}, badRequest("invalid modelVersion parameter: must be an integer of at least 1")
		}
		q.ModelVersion = int(*p.ModelVersion)
	}
	if p.EntityId != nil {
		q.EntityID = p.EntityId.String()
	}
	if p.Limit != nil {
		if *p.Limit < 1 || *p.Limit > maxLimit {
			return spi.ScheduledTaskQuery{}, badRequest("invalid limit parameter: must be an integer from 1 to 1000")
		}
		q.Limit = int(*p.Limit)
	}
	if p.Cursor != nil {
		c, err := decodeCursor(*p.Cursor)
		if err != nil {
			return spi.ScheduledTaskQuery{}, badRequest("invalid cursor parameter")
		}
		q.After = &c
	}
	return q, nil
}

func msTime(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

// toDTO renders one task. Arm and claim tokens, the claim owner, the tenant,
// the mark and the partial-commit flag are never rendered. Each optional field
// is present exactly when the published contract says it is.
func toDTO(t spi.ScheduledTask) (genapi.ScheduledTaskDto, error) {
	eid, err := uuid.Parse(t.EntityID)
	if err != nil {
		return genapi.ScheduledTaskDto{}, fmt.Errorf("failed to parse entity id of scheduled task %s: %w", t.ID, err)
	}
	d := genapi.ScheduledTaskDto{
		TaskId:        t.ID,
		EntityId:      eid,
		ModelName:     t.ModelName,
		ModelVersion:  int32(t.ModelVersion),
		SourceState:   t.SourceState,
		Transition:    t.Transition,
		Status:        string(t.Status),
		ScheduledTime: msTime(t.ScheduledTime),
		ArmedTime:     msTime(t.ArmedAt),
		Attempts:      int32(t.Attempts),
		LostOwners:    int32(t.LostOwners),
	}
	if t.TimeoutMs != nil {
		expires := msTime(t.ScheduledTime + *t.TimeoutMs)
		d.ExpiresTime = &expires
	}
	if t.Status == spi.ScheduledTaskWaiting {
		next := msTime(t.NextAttemptTime)
		d.NextAttemptTime = &next
	}
	if t.LastAttemptTime != nil {
		last := msTime(*t.LastAttemptTime)
		d.LastAttemptTime = &last
	}
	if t.LastError != "" {
		msg := t.LastError
		d.LastError = &msg
	}
	if t.Status == spi.ScheduledTaskFailed {
		reason := string(t.FailureReason)
		d.FailureReason = &reason
		if t.FailedTime != nil {
			failed := msTime(*t.FailedTime)
			d.FailedTime = &failed
		}
	}
	if t.ArmedBy.ID != "" {
		d.ArmedBy = &genapi.ScheduledTaskArmedByDto{Id: t.ArmedBy.ID, Kind: string(t.ArmedBy.Kind)}
	}
	return d, nil
}
