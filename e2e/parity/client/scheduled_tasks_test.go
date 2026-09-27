package client

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

const scheduledTaskPageBody = `{"items":[{"taskId":"t1","entityId":"5f1c1b0e-6d1a-41f1-8000-000000000001",` +
	`"modelName":"m","modelVersion":1,"sourceState":"OPEN","transition":"AutoClose","status":"FAILED",` +
	`"scheduledTime":"2023-11-14T22:13:20.123Z","armedTime":"2023-11-14T22:13:10Z","attempts":2,"lostOwners":1,` +
	`"lastError":"PROCESSOR_ERROR: boom","failureReason":"UNSAFE_WORK_NOT_COMPLETED",` +
	`"failedTime":"2023-11-14T22:16:40Z","armedBy":{"id":"alice","kind":"user"}}],` +
	`"pagination":{"hasNext":true,"nextCursor":"abc"}}`

func TestListScheduledTasks_SendsQueryAndDecodesPage(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, scheduledTaskPageBody)
	}))
	defer srv.Close()

	page, err := NewClient(srv.URL, "tok").ListScheduledTasks(t, url.Values{"status": {"FAILED", "WAITING"}, "limit": {"1"}})
	if err != nil {
		t.Fatalf("ListScheduledTasks: %v", err)
	}
	if gotPath != "/api/scheduled-tasks" || gotQuery != "limit=1&status=FAILED&status=WAITING" {
		t.Errorf("request = %s?%s", gotPath, gotQuery)
	}
	if len(page.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(page.Items))
	}
	it := page.Items[0]
	if it.FailureReason != "UNSAFE_WORK_NOT_COMPLETED" || it.Attempts != 2 || it.LostOwners != 1 ||
		it.ArmedBy == nil || it.ArmedBy.Kind != "user" || it.FailedTime == nil || it.NextAttemptTime != nil {
		t.Errorf("decoded item = %+v", it)
	}
	if !page.Pagination.HasNext || page.Pagination.NextCursor != "abc" {
		t.Errorf("pagination = %+v", page.Pagination)
	}
}

// An undeclared field in an item is drift, as for every parity client type.
func TestListScheduledTasks_UnknownFieldIsDrift(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"items":[{"taskId":"t1","armToken":"x"}],"pagination":{"hasNext":false}}`)
	}))
	defer srv.Close()
	if _, err := NewClient(srv.URL, "tok").ListScheduledTasks(t, nil); err == nil {
		t.Fatal("an item with an undeclared field decoded without error")
	}
}
