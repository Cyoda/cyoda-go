package e2e_test

// consistency_fence_test.go — every operation that answers "as at an instant"
// refuses an instant later than the consistency time (400
// POINT_IN_TIME_AFTER_CONSISTENCY_TIME, carrying properties.consistencyTime)
// and serves the consistency time itself, through the full HTTP stack on a
// running backend.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// laterThan returns an instant an hour after c, in RFC 3339. The consistency
// time follows the clock on an idle system, so an instant only a millisecond
// ahead of one reading is already behind the next; an hour stays later.
func laterThan(t *testing.T, c string) string {
	t.Helper()
	ts, err := time.Parse(time.RFC3339Nano, c)
	if err != nil {
		t.Fatalf("consistency time %q: %v", c, err)
	}
	return ts.Add(time.Hour).UTC().Format(time.RFC3339Nano)
}

// waitConsistentAt blocks until the consistency time has reached ts, so that an
// instant at or before ts is servable.
func waitConsistentAt(t *testing.T, ts time.Time) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		c := consistencyTimeOf(t)
		if got, err := time.Parse(time.RFC3339Nano, c); err == nil && !got.Before(ts) {
			return c
		}
		if time.Now().After(deadline) {
			t.Fatalf("consistency time did not reach %s within 30s (last %s)", ts.Format(time.RFC3339Nano), c)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// expectRefusedAfterConsistencyTime asserts a 400 carrying the fence code and
// properties.consistencyTime.
func expectRefusedAfterConsistencyTime(t *testing.T, status int, body string) {
	t.Helper()
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", status, body)
	}
	var problem struct {
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal([]byte(body), &problem); err != nil {
		t.Fatalf("decode problem: %v: %s", err, body)
	}
	if code, _ := problem.Properties["errorCode"].(string); code != "POINT_IN_TIME_AFTER_CONSISTENCY_TIME" {
		t.Fatalf("errorCode = %q, want POINT_IN_TIME_AFTER_CONSISTENCY_TIME: %s", code, body)
	}
	if ct, _ := problem.Properties["consistencyTime"].(string); ct == "" {
		t.Fatalf("properties.consistencyTime missing: %s", body)
	}
}

// fenceCase is one operation under the fence: it issues the request at the
// given instant. The refused case sends laterThan(C); the served case sends C.
type fenceCase struct {
	name   string
	method string
	path   func(pit string) string
	body   func(pit string) string
}

func TestConsistencyFence(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	const model = "e2e-fence"
	setupStatsModel(t, model)
	id := createEntityE2E(t, model, 1, `{"variantId":"v1","price":1.0}`)
	waitConsistentAt(t, latestChangeTimeE2E(t, id))

	all := `{"type":"lifecycle","field":"state","operatorType":"EQUALS","value":"CREATED"}`
	pitQuery := func(base string) func(string) string {
		return func(pit string) string { return base + "?pointInTime=" + url.QueryEscape(pit) }
	}
	noBody := func(string) string { return "" }
	condBody := func(string) string { return all }
	ordered := []fenceCase{
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
		// DeleteEntities mutates, so it runs last: it deletes the entity the
		// other cases read.
		{"DeleteEntities", http.MethodDelete, pitQuery("/api/entity/" + model + "/1"), noBody},
	}

	for _, c := range ordered {
		t.Run(c.name+"/AfterConsistencyTime_400", func(t *testing.T) {
			later := laterThan(t, consistencyTimeOf(t))
			resp := doAuth(t, c.method, c.path(later), c.body(later))
			expectRefusedAfterConsistencyTime(t, resp.StatusCode, readBody(t, resp))
		})
		t.Run(c.name+"/AtConsistencyTime_200", func(t *testing.T) {
			at := consistencyTimeOf(t)
			resp := doAuth(t, c.method, c.path(at), c.body(at))
			if body := readBody(t, resp); resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200 at the consistency time %s: %s", resp.StatusCode, at, body)
			}
		})
	}

	t.Run("GetOneEntity_MissingEntity_FenceFirst", func(t *testing.T) {
		// The fence is checked before the store is consulted: an unknown id
		// read at a later instant is refused by the fence, not 404, so the
		// answer does not depend on whether the entity exists.
		later := laterThan(t, consistencyTimeOf(t))
		resp := doAuth(t, http.MethodGet, "/api/entity/00000000-0000-4000-8000-000000000001?pointInTime="+url.QueryEscape(later), "")
		expectRefusedAfterConsistencyTime(t, resp.StatusCode, readBody(t, resp))
	})

	t.Run("Transitions_TransactionID_200", func(t *testing.T) {
		// A transactionId names a committed transaction; no instant is
		// involved, so there is nothing to fence.
		id2, tx2 := createEntityE2EWithTxID(t, model, 1, `{"variantId":"v2","price":2.0}`)
		resp := doAuth(t, http.MethodGet, "/api/entity/"+id2+"/transitions?transactionId="+tx2, "")
		if body := readBody(t, resp); resp.StatusCode != http.StatusOK {
			t.Fatalf("transitions by transactionId: %d: %s", resp.StatusCode, body)
		}
	})
}

// TestConsistencyFence_StatsCountsAtAnInstant: counts at an instant between two
// saves reflect only the earlier save, on both per-model statistics reads and
// the grouped read.
func TestConsistencyFence_StatsCountsAtAnInstant(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	const model = "e2e-fence-counts"
	setupStatsModel(t, model)
	a := createEntityE2E(t, model, 1, `{"variantId":"v1","price":1.0}`)
	tA := latestChangeTimeE2E(t, a)
	time.Sleep(20 * time.Millisecond)
	b := createEntityE2E(t, model, 1, `{"variantId":"v2","price":2.0}`)
	tB := latestChangeTimeE2E(t, b)
	mid := midpointBetweenE2E(t, tA, tB)
	waitConsistentAt(t, tB)

	resp := doAuth(t, http.MethodGet, "/api/entity/stats/"+model+"/1?pointInTime="+mid, "")
	body := readBody(t, resp)
	var stats struct {
		Count int `json:"count"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &stats) != nil || stats.Count != 1 {
		t.Errorf("StatsForModel at the midpoint: %d %s, want count 1", resp.StatusCode, body)
	}

	resp = doAuth(t, http.MethodGet, "/api/entity/stats/states/"+model+"/1?pointInTime="+mid, "")
	body = readBody(t, resp)
	var byState []struct {
		State string `json:"state"`
		Count int    `json:"count"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &byState) != nil || len(byState) != 1 || byState[0].Count != 1 {
		t.Errorf("StatsByStateForModel at the midpoint: %d %s, want one state with count 1", resp.StatusCode, body)
	}

	resp = doAuth(t, http.MethodPost, "/api/entity/stats/"+model+"/1/query", fmt.Sprintf(`{"groupBy":["state"],"pointInTime":%q}`, mid))
	body = readBody(t, resp)
	buckets := decodeBuckets(t, body)
	if resp.StatusCode != http.StatusOK || len(buckets) != 1 || buckets[0]["count"] != float64(1) {
		t.Errorf("GroupedStats at the midpoint: %d %s, want one bucket with count 1", resp.StatusCode, body)
	}
}

// TestConsistencyFence_JoinedTransaction: a read joined to a transaction is
// fenced like any other (a later instant is refused), and at the consistency
// time it serves the committed revision.
func TestConsistencyFence_JoinedTransaction(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	h := newCallbackHarness(t)
	const secondary = "fence-joined-secondary"
	const primary = "fence-joined-primary"
	h.SetupModelWithWorkflow(t, secondary, secondaryWorkflow)
	committedID, status, body := h.CreateEntity(t, secondary, 1, `{"name":"committed","amount":1,"status":"new"}`)
	if status != http.StatusOK {
		t.Fatalf("seed entity: %d %s", status, body)
	}

	type outcome struct {
		refused, served callbackResult
		err             error
	}
	done := make(chan outcome, 1)
	h.RegisterProc("fence-joined-read", func(rc *reqCtx) (map[string]any, error) {
		var o outcome
		defer func() { done <- o }()
		ct, err := h.callback(http.MethodGet, "/api/entity/consistency-time", "", "")
		if err != nil || ct.StatusCode != http.StatusOK {
			o.err = fmt.Errorf("consistency time: %v %d %s", err, ct.StatusCode, ct.Body)
			return nil, o.err
		}
		var dto struct {
			ConsistencyTime string `json:"consistencyTime"`
		}
		if err := json.Unmarshal([]byte(ct.Body), &dto); err != nil {
			o.err = err
			return nil, err
		}
		ts, err := time.Parse(time.RFC3339Nano, dto.ConsistencyTime)
		if err != nil {
			o.err = err
			return nil, err
		}
		later := ts.Add(time.Hour).UTC().Format(time.RFC3339Nano)
		o.refused, o.err = h.callback(http.MethodGet, "/api/entity/"+committedID+"?pointInTime="+url.QueryEscape(later), "", rc.token)
		if o.err != nil {
			return nil, o.err
		}
		o.served, o.err = h.callback(http.MethodGet, "/api/entity/"+committedID+"?pointInTime="+url.QueryEscape(dto.ConsistencyTime), "", rc.token)
		return nil, o.err
	})
	h.setupModelSampleWithWorkflow(t, primary, workflowSampleWith(`"x": ""`), intxSearchPrimaryWF("fence-joined-wf", "fence-joined-read"))
	if _, status, body := h.CreateEntity(t, primary, 1, `{"name":"p","amount":1,"status":"new"}`); status != http.StatusOK {
		t.Fatalf("primary create: %d %s", status, body)
	}

	var o outcome
	select {
	case o = <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("processor did not run")
	}
	if o.err != nil {
		t.Fatalf("joined reads: %v", o.err)
	}
	if o.refused.StatusCode != http.StatusBadRequest || !strings.Contains(o.refused.Body, "POINT_IN_TIME_AFTER_CONSISTENCY_TIME") {
		t.Errorf("joined read at a later instant: %d %s, want 400 POINT_IN_TIME_AFTER_CONSISTENCY_TIME", o.refused.StatusCode, o.refused.Body)
	}
	if o.served.StatusCode != http.StatusOK || !strings.Contains(o.served.Body, "committed") {
		t.Errorf("joined read at the consistency time: %d %s, want 200 with the committed revision", o.served.StatusCode, o.served.Body)
	}
}
