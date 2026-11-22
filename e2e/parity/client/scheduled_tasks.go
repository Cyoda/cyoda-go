package client

import (
	"net/http"
	"net/url"
	"testing"
	"time"
)

// ScheduledTask mirrors ScheduledTaskDto in api/openapi.yaml.
type ScheduledTask struct {
	TaskID          string                `json:"taskId"`
	EntityID        string                `json:"entityId"`
	ModelName       string                `json:"modelName"`
	ModelVersion    int                   `json:"modelVersion"`
	SourceState     string                `json:"sourceState"`
	Transition      string                `json:"transition"`
	Status          string                `json:"status"`
	ScheduledTime   time.Time             `json:"scheduledTime"`
	ArmedTime       time.Time             `json:"armedTime"`
	ExpiresTime     *time.Time            `json:"expiresTime,omitempty"`
	Attempts        int                   `json:"attempts"`
	LostOwners      int                   `json:"lostOwners"`
	NextAttemptTime *time.Time            `json:"nextAttemptTime,omitempty"`
	LastAttemptTime *time.Time            `json:"lastAttemptTime,omitempty"`
	LastError       string                `json:"lastError,omitempty"`
	FailureReason   string                `json:"failureReason,omitempty"`
	FailedTime      *time.Time            `json:"failedTime,omitempty"`
	ArmedBy         *ScheduledTaskArmedBy `json:"armedBy,omitempty"`
}

// ScheduledTaskArmedBy mirrors ScheduledTaskArmedByDto.
type ScheduledTaskArmedBy struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
}

// ScheduledTaskPage mirrors ScheduledTaskPageDto.
type ScheduledTaskPage struct {
	Items      []ScheduledTask      `json:"items"`
	Pagination CursorPaginationInfo `json:"pagination"`
}

// ListScheduledTasks issues GET /api/scheduled-tasks with the given query
// (status, modelName, modelVersion, entityId, cursor, limit).
func (c *Client) ListScheduledTasks(t *testing.T, query url.Values) (ScheduledTaskPage, error) {
	t.Helper()
	path := "/api/scheduled-tasks"
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	var page ScheduledTaskPage
	if _, err := c.doJSON(t, http.MethodGet, path, nil, &page); err != nil {
		return ScheduledTaskPage{}, err
	}
	return page, nil
}
