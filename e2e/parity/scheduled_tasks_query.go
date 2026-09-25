package parity

import (
	"net/url"
	"slices"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// scheduledQueryWorkflow parks each entity in OPEN with one scheduled
// transition an hour away, so its task stays WAITING while the scenario runs.
const scheduledQueryWorkflow = `{
	"importMode": "REPLACE",
	"workflows": [{
		"version": "1.1", "name": "sched-query-wf", "initialState": "OPEN", "active": true,
		"states": {
			"OPEN": {"transitions": [{"name": "AutoClose", "next": "CLOSED", "manual": false,
				"schedule": {"delayMs": 3600000}}]},
			"CLOSED": {}
		}
	}]
}`

func setupScheduledQueryModel(t *testing.T, c *client.Client, name string, version int) {
	t.Helper()
	if err := c.ImportModel(t, name, version, `{"k":1}`); err != nil {
		t.Fatalf("ImportModel %s/%d: %v", name, version, err)
	}
	if err := c.LockModel(t, name, version); err != nil {
		t.Fatalf("LockModel %s/%d: %v", name, version, err)
	}
	if err := c.ImportWorkflow(t, name, version, scheduledQueryWorkflow); err != nil {
		t.Fatalf("ImportWorkflow %s/%d: %v", name, version, err)
	}
}

func createScheduledQueryEntities(t *testing.T, c *client.Client, name string, version, n int) []string {
	t.Helper()
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		id, err := c.CreateEntity(t, name, version, `{"k":1}`)
		if err != nil {
			t.Fatalf("CreateEntity %s/%d: %v", name, version, err)
		}
		ids = append(ids, id.String())
	}
	return ids
}

func listScheduledTasksPage(t *testing.T, c *client.Client, query url.Values) client.ScheduledTaskPage {
	t.Helper()
	page, err := c.ListScheduledTasks(t, query)
	if err != nil {
		t.Fatalf("ListScheduledTasks %s: %v", query.Encode(), err)
	}
	if page.Items == nil {
		t.Fatalf("ListScheduledTasks %s: items is null, want an array", query.Encode())
	}
	return page
}

// walkScheduledTasks follows nextCursor to the end and returns every item and
// each page's size.
func walkScheduledTasks(t *testing.T, c *client.Client, query url.Values, limit int) ([]client.ScheduledTask, []int) {
	t.Helper()
	q := url.Values{}
	for k, v := range query {
		q[k] = v
	}
	q.Set("limit", strconv.Itoa(limit))
	var items []client.ScheduledTask
	var sizes []int
	for i := 0; ; i++ {
		if i > 100 {
			t.Fatal("paging did not end after 100 pages")
		}
		page := listScheduledTasksPage(t, c, q)
		items = append(items, page.Items...)
		sizes = append(sizes, len(page.Items))
		if !page.Pagination.HasNext {
			if page.Pagination.NextCursor != "" {
				t.Errorf("the last page carries a nextCursor")
			}
			return items, sizes
		}
		if page.Pagination.NextCursor == "" {
			t.Fatal("hasNext without a nextCursor")
		}
		q.Set("cursor", page.Pagination.NextCursor)
	}
}

func assertScheduledTaskEntities(t *testing.T, label string, items []client.ScheduledTask, want ...string) {
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

func assertScheduledTaskOrder(t *testing.T, items []client.ScheduledTask) {
	t.Helper()
	for i := 1; i < len(items); i++ {
		a, b := items[i-1], items[i]
		if a.ScheduledTime.After(b.ScheduledTime) || (a.ScheduledTime.Equal(b.ScheduledTime) && a.TaskID >= b.TaskID) {
			t.Errorf("items %d and %d are out of (scheduledTime, taskId) order", i-1, i)
		}
	}
}

// RunScheduledTasksQueryPaging: an unfiltered walk returns every task of the
// tenant once, in (scheduledTime, taskId) order, with exact page sizes.
func RunScheduledTasksQueryPaging(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)
	const model = "sched-query-paging"
	setupScheduledQueryModel(t, c, model, 1)
	want := createScheduledQueryEntities(t, c, model, 1, 5)

	first := listScheduledTasksPage(t, c, nil)
	if len(first.Items) != 5 || first.Pagination.HasNext {
		t.Fatalf("default page: %d items, hasNext %v; want 5 and no next page", len(first.Items), first.Pagination.HasNext)
	}
	items, sizes := walkScheduledTasks(t, c, nil, 2)
	if !slices.Equal(sizes, []int{2, 2, 1}) {
		t.Errorf("page sizes = %v, want [2 2 1]", sizes)
	}
	assertScheduledTaskEntities(t, "paged walk", items, want...)
	assertScheduledTaskOrder(t, items)
	for _, it := range items {
		if it.Status != "WAITING" || it.NextAttemptTime == nil || it.FailureReason != "" ||
			!it.ScheduledTime.Equal(it.ArmedTime.Add(time.Hour)) {
			t.Errorf("unexpected item %+v", it)
		}
	}
}

// RunScheduledTasksQueryFilters: each filter, alone and combined.
func RunScheduledTasksQueryFilters(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)
	const modelA, modelB = "sched-query-filter-a", "sched-query-filter-b"
	setupScheduledQueryModel(t, c, modelA, 1)
	setupScheduledQueryModel(t, c, modelA, 2)
	setupScheduledQueryModel(t, c, modelB, 1)
	a1 := createScheduledQueryEntities(t, c, modelA, 1, 2)
	a2 := createScheduledQueryEntities(t, c, modelA, 2, 1)
	b1 := createScheduledQueryEntities(t, c, modelB, 1, 1)
	all := slices.Concat(a1, a2, b1)
	modelAAll := slices.Concat(a1, a2)

	for _, tc := range []struct {
		label string
		query url.Values
		want  []string
	}{
		{"status WAITING", url.Values{"status": {"WAITING"}}, all},
		{"status RUNNING", url.Values{"status": {"RUNNING"}}, nil},
		{"status FAILED", url.Values{"status": {"FAILED"}}, nil},
		{"status WAITING or RUNNING", url.Values{"status": {"WAITING", "RUNNING"}}, all},
		{"modelName", url.Values{"modelName": {modelA}}, modelAAll},
		{"modelName and modelVersion", url.Values{"modelName": {modelA}, "modelVersion": {"2"}}, a2},
		{"entityId", url.Values{"entityId": {a1[0]}}, a1[:1]},
		{"entityId and a model it is not in", url.Values{"entityId": {a1[0]}, "modelName": {modelB}}, nil},
	} {
		t.Run(tc.label, func(t *testing.T) {
			items, _ := walkScheduledTasks(t, c, tc.query, 2)
			assertScheduledTaskEntities(t, tc.label, items, tc.want...)
			assertScheduledTaskOrder(t, items)
		})
	}
}

// RunScheduledTasksQueryTenantIsolation: two tenants with the same model name;
// under every filter each sees only its own tasks, and a cursor minted for one
// tenant reveals nothing of it to the other.
func RunScheduledTasksQueryTenantIsolation(t *testing.T, fixture BackendFixture) {
	tenantA, tenantB := fixture.NewTenant(t), fixture.NewTenant(t)
	cA := client.NewClient(fixture.BaseURL(), tenantA.Token)
	cB := client.NewClient(fixture.BaseURL(), tenantB.Token)
	const model = "sched-query-iso"
	setupScheduledQueryModel(t, cA, model, 1)
	setupScheduledQueryModel(t, cB, model, 1)
	aIDs := createScheduledQueryEntities(t, cA, model, 1, 2)
	bIDs := createScheduledQueryEntities(t, cB, model, 1, 2)

	aItems, _ := walkScheduledTasks(t, cA, nil, 10)
	assertScheduledTaskEntities(t, "tenant A", aItems, aIDs...)
	aTasks := map[string]bool{}
	for _, it := range aItems {
		aTasks[it.TaskID] = true
	}

	for label, q := range map[string]url.Values{
		"no filter":                  nil,
		"status":                     {"status": {"WAITING"}},
		"modelName":                  {"modelName": {model}},
		"modelName and modelVersion": {"modelName": {model}, "modelVersion": {"1"}},
	} {
		items, sizes := walkScheduledTasks(t, cB, q, 1)
		assertScheduledTaskEntities(t, "tenant B, "+label, items, bIDs...)
		if !slices.Equal(sizes, []int{1, 1}) {
			t.Errorf("tenant B, %s: page sizes = %v, want [1 1]", label, sizes)
		}
		for _, it := range items {
			if aTasks[it.TaskID] {
				t.Errorf("tenant B, %s: sees tenant A's task %s", label, it.TaskID)
			}
		}
	}
	for _, id := range aIDs {
		if page := listScheduledTasksPage(t, cB, url.Values{"entityId": {id}}); len(page.Items) != 0 {
			t.Errorf("tenant B sees tasks of tenant A's entity %s", id)
		}
	}

	pageA := listScheduledTasksPage(t, cA, url.Values{"limit": {"1"}})
	if pageA.Pagination.NextCursor == "" {
		t.Fatal("tenant A's first page of one has no nextCursor")
	}
	for _, it := range listScheduledTasksPage(t, cB, url.Values{"cursor": {pageA.Pagination.NextCursor}}).Items {
		if !slices.Contains(bIDs, it.EntityID) {
			t.Errorf("tenant B, with tenant A's cursor, sees entity %s", it.EntityID)
		}
	}
}
