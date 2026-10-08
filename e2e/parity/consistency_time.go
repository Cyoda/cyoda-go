package parity

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// Consistency-time scenarios. Every operation that answers "as at an instant"
// refuses an instant later than the consistency time and serves the
// consistency time itself, on every backend. Instants between two saves come
// from server stamps, never the process clock.

const (
	ctModel   = "parity-consistency-time"
	ctVersion = 1
)

const ctMatchAll = `{"type":"group","operator":"AND","conditions":[]}`

func ctSetup(t *testing.T, fixture BackendFixture) *client.Client {
	t.Helper()
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)
	setupSortModel(t, c, ctModel, ctVersion)
	return c
}

func ctCreate(t *testing.T, c *client.Client, name string) uuid.UUID {
	t.Helper()
	id, err := c.CreateEntity(t, ctModel, ctVersion, fmt.Sprintf(`{"name":%q,"amount":1,"status":"new"}`, name))
	if err != nil {
		t.Fatalf("CreateEntity %s: %v", name, err)
	}
	return id
}

// ctWaitConsistentAt polls the endpoint until the consistency time has reached
// ts (a server stamp) and returns that reading verbatim.
func ctWaitConsistentAt(t *testing.T, c *client.Client, ts time.Time) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		s, err := c.GetConsistencyTime(t)
		if err != nil {
			t.Fatalf("GetConsistencyTime: %v", err)
		}
		if got, perr := time.Parse(time.RFC3339Nano, s); perr == nil && !got.Before(ts) {
			return s
		}
		if time.Now().After(deadline) {
			t.Fatalf("consistency time did not reach %s within 30s (last %s)", ts.Format(time.RFC3339Nano), s)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// ctLater returns an instant an hour after c: the consistency time follows the
// clock on an idle system, so a millisecond-later instant is overtaken.
func ctLater(t *testing.T, c string) string {
	t.Helper()
	ts, err := time.Parse(time.RFC3339Nano, c)
	if err != nil {
		t.Fatalf("consistency time %q: %v", c, err)
	}
	return ts.Add(time.Hour).UTC().Format(time.RFC3339Nano)
}

// ctFenceCase is one HTTP fenced operation; path and body take the instant.
type ctFenceCase struct {
	name, method string
	path, body   func(pit string) string
}

func ctFenceCases(id uuid.UUID) []ctFenceCase {
	pitQuery := func(base string) func(string) string {
		return func(pit string) string { return base + "?pointInTime=" + url.QueryEscape(pit) }
	}
	none := func(string) string { return "" }
	matchAll := func(string) string { return ctMatchAll }
	model := fmt.Sprintf("%s/%d", ctModel, ctVersion)
	return []ctFenceCase{
		{"GetOneEntity", http.MethodGet, pitQuery("/api/entity/" + id.String()), none},
		{"GetAllEntities", http.MethodGet, pitQuery("/api/entity/" + model), none},
		{"SearchDirect", http.MethodPost, pitQuery("/api/search/direct/" + model), matchAll},
		{"SearchAsyncSubmit", http.MethodPost, pitQuery("/api/search/async/" + model), matchAll},
		{"Stats", http.MethodGet, pitQuery("/api/entity/stats"), none},
		{"StatsForModel", http.MethodGet, pitQuery("/api/entity/stats/" + model), none},
		{"StatsByState", http.MethodGet, pitQuery("/api/entity/stats/states"), none},
		{"StatsByStateForModel", http.MethodGet, pitQuery("/api/entity/stats/states/" + model), none},
		{"GroupedStats", http.MethodPost, func(string) string { return "/api/entity/stats/" + model + "/query" },
			func(pit string) string { return fmt.Sprintf(`{"groupBy":["state"],"pointInTime":%q}`, pit) }},
		{"ChangesMetadata", http.MethodGet, pitQuery("/api/entity/" + id.String() + "/changes"), none},
		{"Transitions_PointInTime", http.MethodGet, pitQuery("/api/entity/" + id.String() + "/transitions"), none},
		// Mutates, so it runs last: it deletes the entity the others read.
		{"DeleteEntities", http.MethodDelete, pitQuery("/api/entity/" + model), none},
	}
}

// RunConsistencyTimeEndpoint: GET /entity/consistency-time answers 200 with an
// RFC 3339 instant that never goes backwards across successive calls.
func RunConsistencyTimeEndpoint(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	var prev time.Time
	for i := 0; i < 5; i++ {
		s, err := c.GetConsistencyTime(t)
		if err != nil {
			t.Fatalf("call %d: GetConsistencyTime: %v", i, err)
		}
		ts, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			t.Fatalf("call %d: %q is not RFC 3339: %v", i, s, err)
		}
		if ts.Before(prev) {
			t.Fatalf("call %d: consistency time went backwards: %s after %s", i, ts.Format(time.RFC3339Nano), prev.Format(time.RFC3339Nano))
		}
		prev = ts
		time.Sleep(5 * time.Millisecond)
	}
}

// RunConsistencyTimeFenceRefuses: every HTTP fenced operation asked about an
// instant later than the consistency time answers 400
// POINT_IN_TIME_AFTER_CONSISTENCY_TIME carrying properties.consistencyTime.
func RunConsistencyTimeFenceRefuses(t *testing.T, fixture BackendFixture) {
	c := ctSetup(t, fixture)
	id := ctCreate(t, c, "fence")
	ctWaitConsistentAt(t, c, LatestChangeTime(t, c, id))

	for _, fc := range ctFenceCases(id) {
		t.Run(fc.name, func(t *testing.T) {
			ct, err := c.GetConsistencyTime(t)
			if err != nil {
				t.Fatalf("GetConsistencyTime: %v", err)
			}
			later := ctLater(t, ct)
			status, body, err := c.DoRaw(t, fc.method, fc.path(later), fc.body(later))
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", status, body)
			}
			var problem struct {
				Properties map[string]any `json:"properties"`
			}
			if err := json.Unmarshal(body, &problem); err != nil {
				t.Fatalf("decode problem: %v: %s", err, body)
			}
			if code, _ := problem.Properties["errorCode"].(string); code != "POINT_IN_TIME_AFTER_CONSISTENCY_TIME" {
				t.Errorf("errorCode = %q, want POINT_IN_TIME_AFTER_CONSISTENCY_TIME: %s", code, body)
			}
			if got, _ := problem.Properties["consistencyTime"].(string); got == "" {
				t.Errorf("properties.consistencyTime missing: %s", body)
			}
		})
	}
}

// RunConsistencyTimeReadAtC: every HTTP fenced operation asked about the
// consistency time itself, passed back verbatim, is served; get-by-id shows the
// entity saved before it.
func RunConsistencyTimeReadAtC(t *testing.T, fixture BackendFixture) {
	c := ctSetup(t, fixture)
	id := ctCreate(t, c, "served")
	// One read, no retry: the consistency time of a confirmed save covers it.
	saved := LatestChangeTime(t, c, id)
	first, err := c.GetConsistencyTime(t)
	if err != nil {
		t.Fatalf("GetConsistencyTime: %v", err)
	}
	if got, perr := time.Parse(time.RFC3339Nano, first); perr != nil || got.Before(saved) {
		t.Fatalf("consistency time %s is behind the confirmed save at %s", first, saved.Format(time.RFC3339Nano))
	}

	for _, fc := range ctFenceCases(id) {
		t.Run(fc.name, func(t *testing.T) {
			at, err := c.GetConsistencyTime(t)
			if err != nil {
				t.Fatalf("GetConsistencyTime: %v", err)
			}
			status, body, err := c.DoRaw(t, fc.method, fc.path(at), fc.body(at))
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			if status < 200 || status > 299 {
				t.Fatalf("status = %d, want 2xx at the consistency time %s: %s", status, at, body)
			}
			if fc.name == "GetOneEntity" {
				var ent client.EntityResult
				if err := json.Unmarshal(body, &ent); err != nil {
					t.Fatalf("decode entity: %v: %s", err, body)
				}
				if ent.Meta.ID != id.String() {
					t.Errorf("entity at the consistency time %s: id = %s, want the entity saved before it (%s)", at, ent.Meta.ID, id)
				}
			}
		})
	}
}

// RunConsistencyTimeAsyncDefault: an async search submitted without a
// pointInTime reads at the consistency time, which includes every save already
// confirmed to the caller.
func RunConsistencyTimeAsyncDefault(t *testing.T, fixture BackendFixture) {
	c := ctSetup(t, fixture)
	want := map[string]bool{}
	for i := 0; i < 3; i++ {
		id := ctCreate(t, c, fmt.Sprintf("async-%d", i))
		want[id.String()] = true
		page, err := c.AwaitAsyncSearchResults(t, ctModel, ctVersion, ctMatchAll, 30*time.Second)
		if err != nil {
			t.Fatalf("save %d: async search: %v", i, err)
		}
		got := map[string]bool{}
		for _, e := range page.Content {
			got[e.Meta.ID] = true
		}
		for w := range want {
			if !got[w] {
				t.Errorf("save %d: async search without pointInTime omits confirmed entity %s (got %d results)", i, w, len(got))
			}
		}
	}
}

// RunConsistencyTimeStatsAsAt: all four statistics reads and the grouped read
// count as at an instant between two saves, picking the model's entry out of
// the tenant-wide lists.
func RunConsistencyTimeStatsAsAt(t *testing.T, fixture BackendFixture) {
	c := ctSetup(t, fixture)
	a := ctCreate(t, c, "a")
	tA := LatestChangeTime(t, c, a)
	// Save until a save's server stamp is strictly after tA. A backend with a
	// coarse stamp clock may stamp a quick follow-up save with tA itself; such
	// a save is at or before the midpoint and counts with the first.
	atOrBefore := 1
	var tB time.Time
	deadline := time.Now().Add(2 * time.Second)
	for i := 0; ; i++ {
		b := ctCreate(t, c, fmt.Sprintf("b%d", i))
		tB = LatestChangeTime(t, c, b)
		if tB.After(tA) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no save was stamped after %s within 2s (last %s)", tA.Format(time.RFC3339Nano), tB.Format(time.RFC3339Nano))
		}
		atOrBefore++
	}
	midT := MidpointBetween(t, tA, tB).UTC()
	mid := midT.Format(time.RFC3339Nano)
	ctWaitConsistentAt(t, c, tB)

	get := func(path string, out any) {
		t.Helper()
		status, body, err := c.DoRaw(t, http.MethodGet, path+"?pointInTime="+url.QueryEscape(mid), "")
		if err != nil || status != http.StatusOK {
			t.Fatalf("GET %s: %d %v %s", path, status, err, body)
		}
		if err := json.Unmarshal(body, out); err != nil {
			t.Fatalf("decode %s: %v: %s", path, err, body)
		}
	}
	model := fmt.Sprintf("%s/%d", ctModel, ctVersion)

	var perModel struct {
		Count int `json:"count"`
	}
	get("/api/entity/stats/"+model, &perModel)
	if perModel.Count != atOrBefore {
		t.Errorf("StatsForModel at the midpoint: count = %d, want %d", perModel.Count, atOrBefore)
	}

	var perModelStates []struct {
		Count int `json:"count"`
	}
	get("/api/entity/stats/states/"+model, &perModelStates)
	total := 0
	for _, e := range perModelStates {
		total += e.Count
	}
	if total != atOrBefore {
		t.Errorf("StatsByStateForModel at the midpoint: total = %d, want %d (%v)", total, atOrBefore, perModelStates)
	}

	var all []struct {
		ModelName string `json:"modelName"`
		Count     int    `json:"count"`
	}
	get("/api/entity/stats", &all)
	found := false
	for _, e := range all {
		if e.ModelName == ctModel {
			found = true
			if e.Count != atOrBefore {
				t.Errorf("Stats at the midpoint: count = %d, want %d", e.Count, atOrBefore)
			}
		}
	}
	if !found {
		t.Errorf("Stats at the midpoint: no entry for %s", ctModel)
	}

	var allStates []struct {
		ModelName string `json:"modelName"`
		Count     int    `json:"count"`
	}
	get("/api/entity/stats/states", &allStates)
	total, found = 0, false
	for _, e := range allStates {
		if e.ModelName == ctModel {
			found = true
			total += e.Count
		}
	}
	if !found || total != atOrBefore {
		t.Errorf("StatsByState at the midpoint: found=%v total=%d, want an entry totalling %d", found, total, atOrBefore)
	}

	buckets, err := c.QueryGroupedStats(t, ctModel, ctVersion, client.GroupedStatsRequest{GroupBy: []string{"state"}, PointInTime: &midT})
	if err != nil {
		t.Fatalf("QueryGroupedStats: %v", err)
	}
	if len(buckets) != 1 || buckets[0].Count != int64(atOrBefore) {
		t.Errorf("GroupedStats at the midpoint: %+v, want one bucket with count %d", buckets, atOrBefore)
	}
}
