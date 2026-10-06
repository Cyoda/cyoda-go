package e2e_test

// consistency_unavailable_test.go — every operation that is fenced by the
// consistency time, the async submit that defaults its instant from it, and
// GET /entity/consistency-time answer 503 CONSISTENCY_TIME_UNAVAILABLE
// (retryable) while a save of the tenant is held in its commit phase and the
// store's wait budget runs out. The stack has a database of its own so the
// held marker concerns no other test, and a 1 s statement timeout so each cell
// costs about a second.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/jackc/pgx/v5"

	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/e2e/openapivalidator"
)

// farFuture is later than any consistency time the node can have cached, so
// the fence has to ask the store.
const farFuture = "2099-01-01T00:00:00Z"

// holdMarker takes the tenant's in-flight commit marker on its own connection
// and keeps it until release: cyoda_stamp, then a statement that keeps running
// so the 5 s idle-in-transaction limit cyoda_stamp sets does not end the
// transaction. It returns once the tenant's marker (the granted advisory lock
// cyoda_stamp takes, keyed by the tenant's row in consistency_tenant_keys) is
// visible in pg_locks.
func holdMarker(t *testing.T, s *schedDB, tenant string) (release func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	conn, err := pgx.Connect(ctx, s.url)
	if err != nil {
		cancel()
		t.Fatalf("holder connect: %v", err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		cancel()
		t.Fatalf("holder begin: %v", err)
	}
	key := tenantMarkerKey(t, s, tenant)
	var stamp any
	if err := tx.QueryRow(ctx, `SELECT cyoda_stamp($1)`, key).Scan(&stamp); err != nil {
		cancel()
		t.Fatalf("holder cyoda_stamp: %v", err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = tx.Exec(ctx, `SELECT pg_sleep(300)`)
	}()
	var once sync.Once
	release = func() {
		once.Do(func() {
			cancel() // ends the sleep
			wg.Wait()
			_ = conn.Close(context.Background()) // ends the transaction, dropping the marker
		})
	}
	t.Cleanup(release)

	awaitDBCondition(t, 10*time.Second, "the holder's marker in pg_locks", func() bool {
		return s.count(t, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory'
			AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
			AND classid = $1::oid AND objsubid = 2 AND objid <> 0
			AND mode = 'ExclusiveLock' AND granted`, key) > 0
	})
	return release
}

// tenantMarkerKey returns the tenant's marker key, allocating it as the
// postgres plugin does when the tenant has none.
func tenantMarkerKey(t *testing.T, s *schedDB, tenant string) int32 {
	t.Helper()
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO consistency_tenant_keys (tenant_id) VALUES ($1) ON CONFLICT (tenant_id) DO NOTHING`,
		tenant); err != nil {
		t.Fatalf("allocate tenant marker key: %v", err)
	}
	var key int32
	if err := s.pool.QueryRow(ctx,
		`SELECT tenant_key FROM consistency_tenant_keys WHERE tenant_id = $1`, tenant).Scan(&key); err != nil {
		t.Fatalf("read tenant marker key: %v", err)
	}
	return key
}

// expectUnavailable asserts a 503 CONSISTENCY_TIME_UNAVAILABLE that advertises
// itself retryable.
func expectUnavailable(t *testing.T, status int, body string) {
	t.Helper()
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", status, body)
	}
	var problem struct {
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal([]byte(body), &problem); err != nil {
		t.Fatalf("decode problem: %v: %s", err, body)
	}
	if code, _ := problem.Properties["errorCode"].(string); code != "CONSISTENCY_TIME_UNAVAILABLE" {
		t.Fatalf("errorCode = %q, want CONSISTENCY_TIME_UNAVAILABLE: %s", code, body)
	}
	if retryable, _ := problem.Properties["retryable"].(bool); !retryable {
		t.Fatalf("503 not advertised retryable: %s", body)
	}
}

func TestConsistencyUnavailable(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	const model = "e2e-unavail"
	s := newSchedDB(t)

	// Setup runs on a stack without the 1 s statement timeout, so a loaded
	// host cannot fail the model import.
	setup := newStackOn(t, s, nil)
	for _, step := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/model/import/JSON/SAMPLE_DATA/" + model + "/1", statsSampleDoc},
		{http.MethodPut, "/api/model/" + model + "/1/lock", ""},
		{http.MethodPost, "/api/model/" + model + "/1/workflow/import", statsWorkflowJSON},
		{http.MethodPost, "/api/entity/JSON/" + model + "/1", `{"variantId":"v1","price":1.0}`},
	} {
		resp := setup.DoAuth(t, step.method, step.path, step.body, "")
		if body := setup.readBody(t, resp); resp.StatusCode != http.StatusOK {
			t.Fatalf("setup %s %s: %d %s", step.method, step.path, resp.StatusCode, body)
		}
	}
	resp := setup.DoAuth(t, http.MethodGet, "/api/entity/"+model+"/1", "", "")
	var listed []struct {
		Meta struct {
			ID string `json:"id"`
		} `json:"meta"`
	}
	if body := setup.readBody(t, resp); resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &listed) != nil || len(listed) != 1 {
		t.Fatalf("list entities: %d %s", resp.StatusCode, body)
	}
	id := listed[0].Meta.ID

	// The stack under test: 1 s statement timeout, so the store's wait budget
	// is 1 s. Its handler sits behind the conformance validator.
	t.Setenv("CYODA_POSTGRES_STATEMENT_TIMEOUT", "1s")
	h := newStackOn(t, s, nil)
	swagger, err := genapi.GetSwagger()
	if err != nil {
		t.Fatalf("get swagger: %v", err)
	}
	swagger.Servers = openapi3.Servers{{URL: "/api"}}
	validator, err := openapivalidator.NewValidator(swagger)
	if err != nil {
		t.Fatalf("build validator: %v", err)
	}
	srv := httptest.NewServer(openapivalidator.NewMiddleware(validator)(h.app.Handler()))
	t.Cleanup(srv.Close)
	token := h.token(t)
	do := func(method, path, body string) (int, string) {
		t.Helper()
		resp := doAuthAgainst(t, srv.URL, token, method, path, body)
		return resp.StatusCode, readBody(t, resp)
	}

	jobsBefore := s.count(t, `SELECT count(*) FROM search_jobs WHERE tenant_id = $1`, harnessTenant)
	release := holdMarker(t, s, harnessTenant)

	all := `{"type":"lifecycle","field":"state","operatorType":"EQUALS","value":"CREATED"}`
	pitQuery := func(base string) func(string) string {
		return func(pit string) string { return base + "?pointInTime=" + url.QueryEscape(pit) }
	}
	noBody := func(string) string { return "" }
	condBody := func(string) string { return all }
	cases := []fenceCase{
		{"GetOneEntity", http.MethodGet, pitQuery("/api/entity/" + id), noBody},
		{"GetAllEntities", http.MethodGet, pitQuery("/api/entity/" + model + "/1"), noBody},
		{"SearchDirect", http.MethodPost, pitQuery("/api/search/direct/" + model + "/1"), condBody},
		{"SearchAsyncSubmit", http.MethodPost, pitQuery("/api/search/async/" + model + "/1"), condBody},
		{"Stats", http.MethodGet, pitQuery("/api/entity/stats"), noBody},
		{"StatsForModel", http.MethodGet, pitQuery("/api/entity/stats/" + model + "/1"), noBody},
		{"StatsByState", http.MethodGet, pitQuery("/api/entity/stats/states"), noBody},
		{"StatsByStateForModel", http.MethodGet, pitQuery("/api/entity/stats/states/" + model + "/1"), noBody},
		{"GroupedStats", http.MethodPost, func(string) string { return "/api/entity/stats/" + model + "/1/query" },
			func(pit string) string { return fmt.Sprintf(`{"groupBy":["state"],"pointInTime":%q}`, pit) }},
		{"ChangesMetadata", http.MethodGet, pitQuery("/api/entity/" + id + "/changes"), noBody},
		{"Transitions_PointInTime", http.MethodGet, pitQuery("/api/entity/" + id + "/transitions"), noBody},
		{"DeleteEntities", http.MethodDelete, pitQuery("/api/entity/" + model + "/1"), noBody},
	}
	for _, c := range cases {
		t.Run(c.name+"_503", func(t *testing.T) {
			status, body := do(c.method, c.path(farFuture), c.body(farFuture))
			expectUnavailable(t, status, body)
		})
	}

	t.Run("ConsistencyTimeEndpoint_503", func(t *testing.T) {
		status, body := do(http.MethodGet, "/api/entity/consistency-time", "")
		expectUnavailable(t, status, body)
	})

	t.Run("AsyncSubmitWithoutPointInTime_503", func(t *testing.T) {
		// The default instant comes from the consistency time, which cannot be
		// certified: the submit fails and no job is created.
		status, body := do(http.MethodPost, "/api/search/async/"+model+"/1", all)
		expectUnavailable(t, status, body)
	})

	t.Run("NoJobCreated", func(t *testing.T) {
		if got := s.count(t, `SELECT count(*) FROM search_jobs WHERE tenant_id = $1`, harnessTenant); got != jobsBefore {
			t.Fatalf("search_jobs rows = %d, want %d: a refused submit created a job", got, jobsBefore)
		}
	})

	// With the marker gone the same requests are no longer unavailable: the far
	// instant is refused by the fence (400), the endpoint serves, and the delete
	// that was refused earlier left the entity in place.
	release()
	t.Run("MarkerReleased_NotUnavailable", func(t *testing.T) {
		status, body := do(http.MethodGet, "/api/entity/"+id+"?pointInTime="+url.QueryEscape(farFuture), "")
		expectRefusedAfterConsistencyTime(t, status, body)
		if status, body := do(http.MethodGet, "/api/entity/consistency-time", ""); status != http.StatusOK {
			t.Fatalf("consistency-time after release: %d %s", status, body)
		}
		if status, body := do(http.MethodGet, "/api/entity/"+id, ""); status != http.StatusOK {
			t.Fatalf("entity after refused delete: %d %s", status, body)
		}
	})
}
