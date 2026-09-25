package scheduledtask_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/common/commontest"
	"github.com/cyoda-platform/cyoda-go/internal/domain/scheduledtask"
)

const tenantA spi.TenantID = "tenant-a"

// fakeStore answers Query with a canned page or error and records the call.
// Every other method is unimplemented; the handler reaches none of them.
type fakeStore struct {
	spi.ScheduledTaskStore
	page   spi.ScheduledTaskPage
	err    error
	calls  int
	tenant spi.TenantID
	query  spi.ScheduledTaskQuery
}

func (f *fakeStore) Query(_ context.Context, tenant spi.TenantID, q spi.ScheduledTaskQuery) (spi.ScheduledTaskPage, error) {
	f.calls++
	f.tenant = tenant
	f.query = q
	return f.page, f.err
}

type fakeFactory struct {
	spi.StoreFactory
	store spi.ScheduledTaskStore
	err   error
}

func (f fakeFactory) ScheduledTaskStore(context.Context) (spi.ScheduledTaskStore, error) {
	return f.store, f.err
}

func ptr[T any](v T) *T { return &v }

func call(t *testing.T, f spi.StoreFactory, params genapi.ListScheduledTasksParams) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/scheduled-tasks", nil)
	r = r.WithContext(spi.WithUserContext(r.Context(),
		&spi.UserContext{UserID: "u1", Kind: spi.PrincipalUser, Tenant: spi.Tenant{ID: tenantA}}))
	w := httptest.NewRecorder()
	scheduledtask.NewHandler(f).ListScheduledTasks(w, r, params)
	return w
}

func statuses(s ...string) *[]genapi.ListScheduledTasksParamsStatus {
	out := make([]genapi.ListScheduledTasksParamsStatus, len(s))
	for i, v := range s {
		out[i] = genapi.ListScheduledTasksParamsStatus(v)
	}
	return &out
}

func TestList_Defaults(t *testing.T) {
	st := &fakeStore{}
	w := call(t, fakeFactory{store: st}, genapi.ListScheduledTasksParams{})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if st.calls != 1 || st.tenant != tenantA {
		t.Fatalf("store called %d times for tenant %q; want once for %q", st.calls, st.tenant, tenantA)
	}
	if want := (spi.ScheduledTaskQuery{Limit: 20}); !reflect.DeepEqual(st.query, want) {
		t.Errorf("query = %+v, want %+v", st.query, want)
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body: %s", err, w.Body.String())
	}
	if items, ok := resp["items"].([]any); !ok || len(items) != 0 {
		t.Errorf("items = %#v, want an empty array (not null)", resp["items"])
	}
	pg, _ := resp["pagination"].(map[string]any)
	if pg["hasNext"] != false {
		t.Errorf("hasNext = %v, want false", pg["hasNext"])
	}
	if _, present := pg["nextCursor"]; present {
		t.Errorf("nextCursor present on the last page: %v", pg)
	}
}

func TestList_PassesFiltersToTheStore(t *testing.T) {
	st := &fakeStore{}
	eid := uuid.MustParse("5f1c1b0e-6d1a-41f1-8000-000000000001")
	w := call(t, fakeFactory{store: st}, genapi.ListScheduledTasksParams{
		Status:       statuses("WAITING", "FAILED"),
		ModelName:    ptr("orders"),
		ModelVersion: ptr(int32(2)),
		EntityId:     &eid,
		Limit:        ptr(int32(5)),
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	want := spi.ScheduledTaskQuery{
		Statuses:     []spi.ScheduledTaskStatus{spi.ScheduledTaskWaiting, spi.ScheduledTaskFailed},
		ModelName:    "orders",
		ModelVersion: 2,
		EntityID:     eid.String(),
		Limit:        5,
	}
	if !reflect.DeepEqual(st.query, want) {
		t.Errorf("query = %+v, want %+v", st.query, want)
	}
}

func TestList_BoundaryValuesAccepted(t *testing.T) {
	name256 := strings.Repeat("é", 256) // 256 characters, 512 bytes
	for name, tc := range map[string]struct {
		params genapi.ListScheduledTasksParams
		want   spi.ScheduledTaskQuery
	}{
		"limit 1":                  {genapi.ListScheduledTasksParams{Limit: ptr(int32(1))}, spi.ScheduledTaskQuery{Limit: 1}},
		"limit 1000":               {genapi.ListScheduledTasksParams{Limit: ptr(int32(1000))}, spi.ScheduledTaskQuery{Limit: 1000}},
		"modelName 256 characters": {genapi.ListScheduledTasksParams{ModelName: &name256}, spi.ScheduledTaskQuery{ModelName: name256, Limit: 20}},
		"modelVersion 1": {genapi.ListScheduledTasksParams{ModelName: ptr("m"), ModelVersion: ptr(int32(1))},
			spi.ScheduledTaskQuery{ModelName: "m", ModelVersion: 1, Limit: 20}},
		"every status": {genapi.ListScheduledTasksParams{Status: statuses("RUNNING", "WAITING", "FAILED")},
			spi.ScheduledTaskQuery{Statuses: []spi.ScheduledTaskStatus{spi.ScheduledTaskRunning, spi.ScheduledTaskWaiting, spi.ScheduledTaskFailed}, Limit: 20}},
	} {
		t.Run(name, func(t *testing.T) {
			st := &fakeStore{}
			if w := call(t, fakeFactory{store: st}, tc.params); w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
			}
			if !reflect.DeepEqual(st.query, tc.want) {
				t.Errorf("query = %+v, want %+v", st.query, tc.want)
			}
		})
	}
}

// Every §8 BAD_REQUEST case that reaches the handler. The ones the generated
// binder answers (an integer or uuid that is not one) are pinned in
// internal/api and internal/e2e.
func TestList_InvalidParameters_400(t *testing.T) {
	long := strings.Repeat("m", 257)
	for name, params := range map[string]genapi.ListScheduledTasksParams{
		"unknown status":                 {Status: statuses("PENDING")},
		"lower-case status":              {Status: statuses("waiting")},
		"empty status":                   {Status: statuses("")},
		"one valid, one unknown status":  {Status: statuses("WAITING", "DONE")},
		"modelVersion without modelName": {ModelVersion: ptr(int32(1))},
		"modelVersion zero":              {ModelName: ptr("m"), ModelVersion: ptr(int32(0))},
		"modelVersion negative":          {ModelName: ptr("m"), ModelVersion: ptr(int32(-3))},
		"modelName empty":                {ModelName: ptr("")},
		"modelName 257 characters":       {ModelName: &long},
		"modelName invalid UTF-8":        {ModelName: ptr("m\xff")},
		"modelName with NUL":             {ModelName: ptr("m\x00n")},
		"limit zero":                     {Limit: ptr(int32(0))},
		"limit 1001":                     {Limit: ptr(int32(1001))},
		"limit negative":                 {Limit: ptr(int32(-1))},
		"cursor not base64url":           {Cursor: ptr("!!!not-a-cursor!!!")},
		"cursor over 256 characters":     {Cursor: ptr(strings.Repeat("A", 257))},
	} {
		t.Run(name, func(t *testing.T) {
			st := &fakeStore{}
			w := call(t, fakeFactory{store: st}, params)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
			}
			commontest.ExpectErrorCode(t, w.Result(), common.ErrCodeBadRequest)
			if st.calls != 0 {
				t.Errorf("the store was queried for an invalid request")
			}
			if params.Cursor != nil && strings.Contains(w.Body.String(), *params.Cursor) {
				t.Errorf("the 400 echoes the cursor: %s", w.Body.String())
			}
		})
	}
}

func TestList_NextCursorLeadsToTheNextPage(t *testing.T) {
	next := spi.ScheduledTaskCursor{ScheduledTime: 1700000000123, ID: "0123456789abcdef0123456789abcdef"}
	w := call(t, fakeFactory{store: &fakeStore{page: spi.ScheduledTaskPage{Next: &next}}}, genapi.ListScheduledTasksParams{})
	var resp struct {
		Pagination struct {
			HasNext    bool    `json:"hasNext"`
			NextCursor *string `json:"nextCursor"`
		} `json:"pagination"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body: %s", err, w.Body.String())
	}
	if !resp.Pagination.HasNext || resp.Pagination.NextCursor == nil {
		t.Fatalf("pagination = %+v, want hasNext with a nextCursor", resp.Pagination)
	}

	st := &fakeStore{}
	if w := call(t, fakeFactory{store: st}, genapi.ListScheduledTasksParams{Cursor: resp.Pagination.NextCursor}); w.Code != http.StatusOK {
		t.Fatalf("second page: status = %d; body: %s", w.Code, w.Body.String())
	}
	if st.query.After == nil || *st.query.After != next {
		t.Errorf("second page After = %v, want %+v", st.query.After, next)
	}
}

func TestList_ItemFields(t *testing.T) {
	timeout := int64(60000)
	lastAt := int64(1700000100000)
	failedAt := int64(1700000200000)
	armTok, claimTok, owner := uuid.New(), uuid.New(), uuid.New()
	eW, eR, eF := uuid.New(), uuid.New(), uuid.New()
	base := func(id string, e uuid.UUID, s spi.ScheduledTaskStatus) spi.ScheduledTask {
		return spi.ScheduledTask{
			ID: id, TenantID: tenantA, Type: spi.ScheduledTaskFireTransition,
			ScheduledTime: 1700000000123, EntityID: e.String(), ModelName: "orders", ModelVersion: 3,
			Transition: "AutoClose", SourceState: "OPEN", ArmedAt: 1699999990000,
			Status: s, ArmToken: armTok, NextAttemptTime: 1700000000123,
		}
	}
	waiting := base("t-waiting", eW, spi.ScheduledTaskWaiting)
	waiting.TimeoutMs = &timeout
	waiting.ArmedBy = spi.Principal{ID: "alice", Kind: spi.PrincipalUser}
	running := base("t-running", eR, spi.ScheduledTaskRunning)
	running.Claim = &spi.TaskClaim{Token: claimTok, Owner: owner}
	running.UnsafeMarked = true
	running.PartialCommit = true
	failed := base("t-failed", eF, spi.ScheduledTaskFailed)
	failed.Attempts, failed.LostOwners = 2, 1
	failed.LastAttemptTime = &lastAt
	failed.LastError = "PROCESSOR_ERROR: boom"
	failed.FailureReason = spi.FailureUnsafeWorkNotCompleted
	failed.FailedTime = &failedAt

	st := &fakeStore{page: spi.ScheduledTaskPage{Items: []spi.ScheduledTask{waiting, running, failed}}}
	w := call(t, fakeFactory{store: st}, genapi.ListScheduledTasksParams{})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, secret := range []string{armTok.String(), claimTok.String(), owner.String(), string(tenantA)} {
		if strings.Contains(body, secret) {
			t.Errorf("response carries %q, which is never returned; body: %s", secret, body)
		}
	}

	var resp struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Items) != 3 {
		t.Fatalf("items = %d, want 3", len(resp.Items))
	}
	fields := func(taskID string, e uuid.UUID, status string) map[string]any {
		return map[string]any{
			"taskId": taskID, "entityId": e.String(), "modelName": "orders", "modelVersion": float64(3),
			"sourceState": "OPEN", "transition": "AutoClose", "status": status,
			"scheduledTime": "2023-11-14T22:13:20.123Z", "armedTime": "2023-11-14T22:13:10Z",
		}
	}
	wantW := fields("t-waiting", eW, "WAITING")
	wantW["attempts"], wantW["lostOwners"] = float64(0), float64(0)
	wantW["expiresTime"] = "2023-11-14T22:14:20.123Z"
	wantW["nextAttemptTime"] = "2023-11-14T22:13:20.123Z"
	wantW["armedBy"] = map[string]any{"id": "alice", "kind": "user"}

	wantR := fields("t-running", eR, "RUNNING")
	wantR["attempts"], wantR["lostOwners"] = float64(0), float64(0)

	wantF := fields("t-failed", eF, "FAILED")
	wantF["attempts"], wantF["lostOwners"] = float64(2), float64(1)
	wantF["lastAttemptTime"] = "2023-11-14T22:15:00Z"
	wantF["lastError"] = "PROCESSOR_ERROR: boom"
	wantF["failureReason"] = "UNSAFE_WORK_NOT_COMPLETED"
	wantF["failedTime"] = "2023-11-14T22:16:40Z"

	for i, want := range []map[string]any{wantW, wantR, wantF} {
		if !reflect.DeepEqual(resp.Items[i], want) {
			t.Errorf("item %d =\n  %v\nwant\n  %v", i, resp.Items[i], want)
		}
	}
}

// outageErr carries the storage plugin's transient-unavailability marker and,
// in its text, the kind of connection detail a driver error carries.
type outageErr struct{}

func (outageErr) Error() string            { return "acquire timed out: postgres://u:p@db/cyoda" }
func (outageErr) StorageUnavailable() bool { return true }

func TestList_StorageUnavailable_503(t *testing.T) {
	st := &fakeStore{err: fmt.Errorf("failed to query scheduled tasks: %w", outageErr{})}
	w := call(t, fakeFactory{store: st}, genapi.ListScheduledTasksParams{})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body: %s", w.Code, w.Body.String())
	}
	commontest.ExpectErrorCode(t, w.Result(), common.ErrCodeStorageUnavailable)
	var pd struct {
		Properties map[string]any `json:"properties"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &pd)
	if r, _ := pd.Properties["retryable"].(bool); !r {
		t.Errorf("503 is not advertised as retryable; body: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "postgres://") {
		t.Errorf("response leaked storage internals: %s", w.Body.String())
	}
}

func TestList_Failure_500WithTicket(t *testing.T) {
	for name, f := range map[string]fakeFactory{
		"query fails":             {store: &fakeStore{err: errors.New("scan: postgres://u:p@db/cyoda")}},
		"factory fails":           {err: errors.New("open: postgres://u:p@db/cyoda")},
		"stored entity id broken": {store: &fakeStore{page: spi.ScheduledTaskPage{Items: []spi.ScheduledTask{{ID: "t1", EntityID: "not-a-uuid", Status: spi.ScheduledTaskWaiting}}}}},
	} {
		t.Run(name, func(t *testing.T) {
			w := call(t, f, genapi.ListScheduledTasksParams{})
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500; body: %s", w.Code, w.Body.String())
			}
			commontest.ExpectErrorCode(t, w.Result(), common.ErrCodeServerError)
			var pd struct {
				Ticket string `json:"ticket"`
			}
			_ = json.Unmarshal(w.Body.Bytes(), &pd)
			if pd.Ticket == "" {
				t.Errorf("500 carries no ticket: %s", w.Body.String())
			}
			for _, leak := range []string{"postgres://", "not-a-uuid"} {
				if strings.Contains(w.Body.String(), leak) {
					t.Errorf("response leaked %q: %s", leak, w.Body.String())
				}
			}
		})
	}
}
