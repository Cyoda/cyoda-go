package e2e_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// schedQueryWorkflow parks every entity in OPEN with one scheduled transition
// an hour away. Its task stays WAITING for the whole test: the shared stack
// runs no scheduler, and a scheduler of a per-test harness claims only due
// tasks and RUNNING tasks of a lost owner.
func schedQueryWorkflow(withTimeout bool) string {
	timeout := ""
	if withTimeout {
		timeout = `, "timeoutMs": 60000`
	}
	return `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "sched-query-wf", "initialState": "OPEN", "active": true,
			"states": {
				"OPEN": {"transitions": [{"name": "AutoClose", "next": "CLOSED", "manual": false,
					"schedule": {"delayMs": 3600000` + timeout + `}}]},
				"CLOSED": {}
			}
		}]
	}`
}

// schedTenant is a fresh tenant with its own M2M client. A fresh tenant has no
// tasks of other tests, so an unfiltered list is deterministic.
type schedTenant struct {
	id, clientID, secret string
}

func newSchedTenant(t *testing.T) schedTenant {
	t.Helper()
	id := "schedq-" + randSuffix(t)
	cid, secret := createM2MClient(t, id, "user-"+id, true)
	return schedTenant{id: id, clientID: cid, secret: secret}
}

// do issues one request as this tenant. path has no /api prefix.
func (s schedTenant) do(t *testing.T, method, path, body string) (int, string) {
	t.Helper()
	var b []byte
	if body != "" {
		b = []byte(body)
	}
	resp := adminRequestAs(t, s.id, s.clientID, s.secret, method, path, b)
	return resp.StatusCode, readBody(t, resp)
}

func (s schedTenant) setupModel(t *testing.T, name string, version int, withTimeout bool) {
	t.Helper()
	for _, step := range []struct{ method, path, body string }{
		{http.MethodPost, fmt.Sprintf("/model/import/JSON/SAMPLE_DATA/%s/%d", name, version), `{"k":1}`},
		{http.MethodPut, fmt.Sprintf("/model/%s/%d/lock", name, version), ""},
		{http.MethodPost, fmt.Sprintf("/model/%s/%d/workflow/import", name, version), schedQueryWorkflow(withTimeout)},
	} {
		if status, body := s.do(t, step.method, step.path, step.body); status/100 != 2 {
			t.Fatalf("%s %s: %d %s", step.method, step.path, status, body)
		}
	}
}

func (s schedTenant) createEntity(t *testing.T, name string, version int) string {
	t.Helper()
	status, body := s.do(t, http.MethodPost, fmt.Sprintf("/entity/JSON/%s/%d", name, version), `{"k":1}`)
	if status != http.StatusOK {
		t.Fatalf("create entity %s/%d: %d %s", name, version, status, body)
	}
	var res []struct {
		EntityIDs []string `json:"entityIds"`
	}
	if err := json.Unmarshal([]byte(body), &res); err != nil || len(res) == 0 || len(res[0].EntityIDs) != 1 {
		t.Fatalf("create entity: unexpected body %s (%v)", body, err)
	}
	return res[0].EntityIDs[0]
}

func (s schedTenant) list(t *testing.T, query url.Values) (int, string) {
	t.Helper()
	path := "/scheduled-tasks"
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	return s.do(t, http.MethodGet, path, "")
}

type schedTaskItem struct {
	TaskID          string     `json:"taskId"`
	EntityID        string     `json:"entityId"`
	ModelName       string     `json:"modelName"`
	ModelVersion    int        `json:"modelVersion"`
	SourceState     string     `json:"sourceState"`
	Transition      string     `json:"transition"`
	Status          string     `json:"status"`
	ScheduledTime   time.Time  `json:"scheduledTime"`
	ArmedTime       time.Time  `json:"armedTime"`
	ExpiresTime     *time.Time `json:"expiresTime"`
	Attempts        int        `json:"attempts"`
	LostOwners      int        `json:"lostOwners"`
	NextAttemptTime *time.Time `json:"nextAttemptTime"`
	LastAttemptTime *time.Time `json:"lastAttemptTime"`
	LastError       *string    `json:"lastError"`
	FailureReason   *string    `json:"failureReason"`
	FailedTime      *time.Time `json:"failedTime"`
	ArmedBy         *struct {
		ID   string `json:"id"`
		Kind string `json:"kind"`
	} `json:"armedBy"`
}

type schedTaskPage struct {
	Items      []schedTaskItem `json:"items"`
	Pagination struct {
		HasNext    bool    `json:"hasNext"`
		NextCursor *string `json:"nextCursor"`
	} `json:"pagination"`
}

// scheduledTaskKeys are the fields ScheduledTaskDto declares. Tokens, claim
// owners, node ids and the tenant are not among them and must never appear.
var scheduledTaskKeys = map[string]bool{
	"taskId": true, "entityId": true, "modelName": true, "modelVersion": true, "sourceState": true,
	"transition": true, "status": true, "scheduledTime": true, "armedTime": true, "expiresTime": true,
	"attempts": true, "lostOwners": true, "nextAttemptTime": true, "lastAttemptTime": true,
	"lastError": true, "failureReason": true, "failedTime": true, "armedBy": true,
}

func (s schedTenant) listPage(t *testing.T, query url.Values) schedTaskPage {
	t.Helper()
	status, body := s.list(t, query)
	if status != http.StatusOK {
		t.Fatalf("GET /scheduled-tasks?%s: %d %s", query.Encode(), status, body)
	}
	var raw struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatalf("decode: %v; body: %s", err, body)
	}
	for _, it := range raw.Items {
		for k := range it {
			if !scheduledTaskKeys[k] {
				t.Errorf("item carries %q, which ScheduledTaskDto does not declare; body: %s", k, body)
			}
		}
	}
	var page schedTaskPage
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatalf("decode: %v; body: %s", err, body)
	}
	if page.Items == nil {
		t.Fatalf("items is null, want an array; body: %s", body)
	}
	return page
}

// listAll follows nextCursor to the end and returns every item and each
// page's size.
func (s schedTenant) listAll(t *testing.T, query url.Values, limit int) ([]schedTaskItem, []int) {
	t.Helper()
	q := url.Values{}
	for k, v := range query {
		q[k] = v
	}
	q.Set("limit", strconv.Itoa(limit))
	var items []schedTaskItem
	var sizes []int
	for i := 0; ; i++ {
		if i > 100 {
			t.Fatal("paging did not end after 100 pages")
		}
		page := s.listPage(t, q)
		items = append(items, page.Items...)
		sizes = append(sizes, len(page.Items))
		if !page.Pagination.HasNext {
			if page.Pagination.NextCursor != nil {
				t.Errorf("the last page carries a nextCursor")
			}
			return items, sizes
		}
		if page.Pagination.NextCursor == nil {
			t.Fatal("hasNext without a nextCursor")
		}
		q.Set("cursor", *page.Pagination.NextCursor)
	}
}

func assertTaskEntities(t *testing.T, label string, items []schedTaskItem, want ...string) {
	t.Helper()
	got := make([]string, 0, len(items))
	for _, it := range items {
		got = append(got, it.EntityID)
	}
	sort.Strings(got)
	w := slices.Clone(want)
	sort.Strings(w)
	if !slices.Equal(got, w) {
		t.Errorf("%s: entities %v, want %v", label, got, w)
	}
}

func assertTaskOrder(t *testing.T, items []schedTaskItem) {
	t.Helper()
	for i := 1; i < len(items); i++ {
		a, b := items[i-1], items[i]
		if a.ScheduledTime.After(b.ScheduledTime) || (a.ScheduledTime.Equal(b.ScheduledTime) && a.TaskID >= b.TaskID) {
			t.Errorf("items %d and %d are out of (scheduledTime, taskId) order: %s/%s then %s/%s", i-1, i,
				a.ScheduledTime.Format(time.RFC3339Nano), a.TaskID, b.ScheduledTime.Format(time.RFC3339Nano), b.TaskID)
		}
	}
}

// markFailed turns an armed task into a FAILED one in PostgreSQL. Reading a
// FAILED task back needs no run, and the shared stack runs no scheduler.
func markFailed(t *testing.T, tenant, entityID string, lastAttemptAt, failedAt time.Time) {
	t.Helper()
	tag, err := dbPool.Exec(e2eCtx(t), `UPDATE scheduled_tasks
		SET status = 'FAILED', failure_reason = 'UNSAFE_WORK_NOT_COMPLETED',
		    last_error = 'PROCESSOR_ERROR: seeded failure', attempts = 2, lost_owners = 1,
		    last_attempt_time = $3, failed_time = $4, claim_token = NULL, claim_owner = NULL
		WHERE tenant_id = $1 AND entity_id = $2`,
		tenant, entityID, lastAttemptAt.UnixMilli(), failedAt.UnixMilli())
	if err != nil {
		t.Fatalf("seed FAILED task: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("seed FAILED task: %d rows, want 1", tag.RowsAffected())
	}
}

func TestScheduledTasks_Paging_200(t *testing.T) {
	ten := newSchedTenant(t)
	const model = "sched-page"
	ten.setupModel(t, model, 1, false)
	var want []string
	for i := 0; i < 5; i++ {
		want = append(want, ten.createEntity(t, model, 1))
	}

	first := ten.listPage(t, nil)
	if len(first.Items) != 5 || first.Pagination.HasNext {
		t.Fatalf("default page: %d items, hasNext %v; want 5 items and no next page", len(first.Items), first.Pagination.HasNext)
	}

	items, sizes := ten.listAll(t, nil, 2)
	if !slices.Equal(sizes, []int{2, 2, 1}) {
		t.Errorf("page sizes = %v, want [2 2 1]", sizes)
	}
	assertTaskEntities(t, "paged walk", items, want...)
	assertTaskOrder(t, items)
	for _, it := range items {
		if it.Status != "WAITING" || it.ModelName != model || it.ModelVersion != 1 || it.SourceState != "OPEN" ||
			it.Transition != "AutoClose" || it.Attempts != 0 || it.LostOwners != 0 {
			t.Errorf("unexpected item %+v", it)
		}
		if !it.ScheduledTime.Equal(it.ArmedTime.Add(time.Hour)) {
			t.Errorf("scheduledTime %s is not armedTime %s + 1h", it.ScheduledTime, it.ArmedTime)
		}
		if it.NextAttemptTime == nil || !it.NextAttemptTime.Equal(it.ScheduledTime) {
			t.Errorf("a new WAITING task's nextAttemptTime must equal scheduledTime: %+v", it)
		}
		if it.ExpiresTime != nil || it.FailureReason != nil || it.FailedTime != nil || it.LastError != nil || it.LastAttemptTime != nil {
			t.Errorf("fields present that the contract leaves absent here: %+v", it)
		}
		if it.ArmedBy == nil || it.ArmedBy.ID == "" || it.ArmedBy.Kind == "" {
			t.Errorf("armedBy missing for a task armed by a client write: %+v", it)
		}
	}
}

func TestScheduledTasks_Filters_200(t *testing.T) {
	ten := newSchedTenant(t)
	const modelA, modelB = "sched-filter-a", "sched-filter-b"
	ten.setupModel(t, modelA, 1, true)
	ten.setupModel(t, modelA, 2, false)
	ten.setupModel(t, modelB, 1, false)
	a1 := []string{ten.createEntity(t, modelA, 1), ten.createEntity(t, modelA, 1)}
	a2 := ten.createEntity(t, modelA, 2)
	b1 := ten.createEntity(t, modelB, 1)
	lastAt := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	failedAt := lastAt.Add(5 * time.Second)
	markFailed(t, ten.id, b1, lastAt, failedAt)

	waiting := []string{a1[0], a1[1], a2}
	all := []string{a1[0], a1[1], a2, b1}
	for _, tc := range []struct {
		label string
		query url.Values
		want  []string
	}{
		{"status WAITING", url.Values{"status": {"WAITING"}}, waiting},
		{"status FAILED", url.Values{"status": {"FAILED"}}, []string{b1}},
		{"status WAITING or FAILED", url.Values{"status": {"WAITING", "FAILED"}}, all},
		{"status RUNNING", url.Values{"status": {"RUNNING"}}, nil},
		{"modelName", url.Values{"modelName": {modelA}}, waiting},
		{"modelName and modelVersion", url.Values{"modelName": {modelA}, "modelVersion": {"2"}}, []string{a2}},
		{"entityId", url.Values{"entityId": {a1[0]}}, []string{a1[0]}},
		{"status and modelName", url.Values{"status": {"WAITING"}, "modelName": {modelB}}, nil},
	} {
		t.Run(tc.label, func(t *testing.T) {
			items, _ := ten.listAll(t, tc.query, 2)
			assertTaskEntities(t, tc.label, items, tc.want...)
			assertTaskOrder(t, items)
		})
	}

	t.Run("expiresTime from timeoutMs", func(t *testing.T) {
		page := ten.listPage(t, url.Values{"entityId": {a1[0]}})
		if len(page.Items) != 1 {
			t.Fatalf("items = %d, want 1", len(page.Items))
		}
		it := page.Items[0]
		if it.ExpiresTime == nil || !it.ExpiresTime.Equal(it.ScheduledTime.Add(time.Minute)) {
			t.Errorf("expiresTime = %v, want scheduledTime %s + 60s", it.ExpiresTime, it.ScheduledTime)
		}
	})

	t.Run("FAILED item", func(t *testing.T) {
		page := ten.listPage(t, url.Values{"entityId": {b1}})
		if len(page.Items) != 1 {
			t.Fatalf("items = %d, want 1", len(page.Items))
		}
		it := page.Items[0]
		if it.Status != "FAILED" || it.Attempts != 2 || it.LostOwners != 1 {
			t.Errorf("status/attempts/lostOwners = %s/%d/%d, want FAILED/2/1", it.Status, it.Attempts, it.LostOwners)
		}
		if it.FailureReason == nil || *it.FailureReason != "UNSAFE_WORK_NOT_COMPLETED" {
			t.Errorf("failureReason = %v, want UNSAFE_WORK_NOT_COMPLETED", it.FailureReason)
		}
		if it.LastError == nil || *it.LastError != "PROCESSOR_ERROR: seeded failure" {
			t.Errorf("lastError = %v", it.LastError)
		}
		if it.FailedTime == nil || !it.FailedTime.Equal(failedAt) {
			t.Errorf("failedTime = %v, want %s", it.FailedTime, failedAt)
		}
		if it.LastAttemptTime == nil || !it.LastAttemptTime.Equal(lastAt) {
			t.Errorf("lastAttemptTime = %v, want %s", it.LastAttemptTime, lastAt)
		}
		if it.NextAttemptTime != nil {
			t.Errorf("a FAILED task carries nextAttemptTime %v", it.NextAttemptTime)
		}
	})
}

func TestScheduledTasks_EmptyList_200(t *testing.T) {
	a, b := newSchedTenant(t), newSchedTenant(t)
	const model = "sched-empty"
	b.setupModel(t, model, 1, false)
	bEntity := b.createEntity(t, model, 1)

	for label, q := range map[string]url.Values{
		"unknown model":           {"modelName": {"sched-never-imported"}},
		"unknown entity":          {"entityId": {"00000000-0000-4000-8000-00000000abcd"}},
		"another tenant's entity": {"entityId": {bEntity}},
		"another tenant's model":  {"modelName": {model}},
	} {
		t.Run(label, func(t *testing.T) {
			status, body := a.list(t, q)
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200; body: %s", status, body)
			}
			if !strings.Contains(body, `"items":[]`) {
				t.Errorf("want an empty items array; body: %s", body)
			}
			page := a.listPage(t, q)
			if len(page.Items) != 0 || page.Pagination.HasNext {
				t.Errorf("want an empty last page; got %d items, hasNext %v", len(page.Items), page.Pagination.HasNext)
			}
		})
	}
}

func TestScheduledTasks_TenantIsolation(t *testing.T) {
	a, b := newSchedTenant(t), newSchedTenant(t)
	const model = "sched-iso"
	a.setupModel(t, model, 1, false)
	b.setupModel(t, model, 1, false)
	aIDs := []string{a.createEntity(t, model, 1), a.createEntity(t, model, 1)}
	bIDs := []string{b.createEntity(t, model, 1), b.createEntity(t, model, 1)}

	aTasks := map[string]bool{}
	aItems, _ := a.listAll(t, nil, 10)
	assertTaskEntities(t, "tenant A", aItems, aIDs...)
	for _, it := range aItems {
		aTasks[it.TaskID] = true
	}

	for label, q := range map[string]url.Values{
		"no filter":                  nil,
		"status":                     {"status": {"WAITING"}},
		"modelName":                  {"modelName": {model}},
		"modelName and modelVersion": {"modelName": {model}, "modelVersion": {"1"}},
	} {
		t.Run(label, func(t *testing.T) {
			items, sizes := b.listAll(t, q, 1)
			assertTaskEntities(t, "tenant B, "+label, items, bIDs...)
			if !slices.Equal(sizes, []int{1, 1}) {
				t.Errorf("page sizes = %v, want [1 1]", sizes)
			}
			for _, it := range items {
				if aTasks[it.TaskID] {
					t.Errorf("tenant B sees tenant A's task %s", it.TaskID)
				}
			}
		})
	}
	for _, id := range aIDs {
		if page := b.listPage(t, url.Values{"entityId": {id}}); len(page.Items) != 0 {
			t.Errorf("tenant B sees tasks of tenant A's entity %s", id)
		}
	}

	// A cursor is a position, not a capability: tenant A's cursor, used by
	// tenant B, returns only tenant B's tasks.
	pageA := a.listPage(t, url.Values{"limit": {"1"}})
	if pageA.Pagination.NextCursor == nil {
		t.Fatal("tenant A's first page of one has no nextCursor")
	}
	pageB := b.listPage(t, url.Values{"cursor": {*pageA.Pagination.NextCursor}})
	for _, it := range pageB.Items {
		if !slices.Contains(bIDs, it.EntityID) {
			t.Errorf("tenant B, with tenant A's cursor, sees entity %s", it.EntityID)
		}
	}
}

// Every 400 BAD_REQUEST case of the error table, through the generated binder
// and the handler. A bad cursor is never echoed.
func TestScheduledTasks_InvalidParameters_400(t *testing.T) {
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	wrongVersion := enc(`{"v":2,"t":1,"i":"x"}`)
	longCursor := strings.Repeat("A", 257)
	for _, tc := range []struct{ name, query, cursor string }{
		{"unknown status", "status=PENDING", ""},
		{"lower-case status", "status=waiting", ""},
		{"empty status", "status=", ""},
		{"one valid, one unknown status", "status=WAITING&status=DONE", ""},
		{"modelVersion without modelName", "modelVersion=1", ""},
		{"modelVersion zero", "modelName=m&modelVersion=0", ""},
		{"modelVersion negative", "modelName=m&modelVersion=-1", ""},
		{"modelVersion not an integer", "modelName=m&modelVersion=abc", ""},
		{"modelVersion decimal", "modelName=m&modelVersion=1.5", ""},
		{"entityId not a UUID", "entityId=not-a-uuid", ""},
		{"modelName empty", "modelName=", ""},
		{"modelName 257 characters", "modelName=" + strings.Repeat("m", 257), ""},
		{"modelName with NUL", "modelName=m%00n", ""},
		{"modelName invalid UTF-8", "modelName=m%FF", ""},
		{"limit zero", "limit=0", ""},
		{"limit 1001", "limit=1001", ""},
		{"limit not an integer", "limit=abc", ""},
		{"limit empty", "limit=", ""},
		{"cursor not base64url", "cursor=%21%21%21", "!!!"},
		{"cursor wrong version", "cursor=" + wrongVersion, wrongVersion},
		{"cursor over 256 characters", "cursor=" + longCursor, longCursor},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := doAuth(t, http.MethodGet, "/api/scheduled-tasks?"+tc.query, "")
			body := readBody(t, resp)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body: %s", resp.StatusCode, body)
			}
			if code := problemErrorCode(body); code != "BAD_REQUEST" {
				t.Errorf("errorCode = %q, want BAD_REQUEST; body: %s", code, body)
			}
			if tc.cursor != "" && strings.Contains(body, tc.cursor) {
				t.Errorf("the 400 echoes the cursor: %s", body)
			}
		})
	}
}

func TestScheduledTasks_Unauthorized_401(t *testing.T) {
	for name, header := range map[string]string{
		"no token":            "",
		"garbage token":       "Bearer not-a-jwt",
		"untrusted signature": "Bearer " + untrustedJWT,
	} {
		t.Run(name, func(t *testing.T) {
			resp := unauthRequest(t, http.MethodGet, "/api/scheduled-tasks", header)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", resp.StatusCode)
			}
			assertUnauthorizedProblem(t, resp)
		})
	}
}
