package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/common/commontest"
)

// An array clause applies its operatorType at every position whose entry in
// `values` is not null. Entities: A scores [90,40,75], B scores [10,95,60].

const scoresWorkflow = `{
	"importMode": "REPLACE",
	"workflows": [{
		"version": "1.1", "name": "scores-wf", "initialState": "NONE", "active": true,
		"states": {"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]}, "CREATED": {}}
	}]
}`

func setupScoresModel(t *testing.T, model string) (idA, idB string) {
	t.Helper()
	setupModelSampleWithWorkflow(t, model, `{"name":"S","scores":[1,2,3]}`, scoresWorkflow)
	idA = createEntityE2E(t, model, 1, `{"name":"A","scores":[90,40,75]}`)
	idB = createEntityE2E(t, model, 1, `{"name":"B","scores":[10,95,60]}`)
	return idA, idB
}

// arrayClauseRefusals is every 4xx row of the spec's error table that a
// condition body can trigger, with the code each surface answers.
var arrayClauseRefusals = []struct {
	name, cond, code string
}{
	{"missing operator", `{"type":"array","jsonPath":"$.scores[*]","values":[50]}`, "INVALID_CONDITION"},
	{"old spec shape", `{"type":"array","jsonPath":"$.scores[*]","operatorType":"EQUALS","value":["90"]}`, "INVALID_CONDITION"},
	{"unknown operator", `{"type":"array","jsonPath":"$.scores[*]","operatorType":"FROBNICATE","values":[50]}`, "INVALID_CONDITION"},
	{"all null", `{"type":"array","jsonPath":"$.scores[*]","operatorType":"EQUALS","values":[null,null]}`, "INVALID_CONDITION"},
	{"empty values", `{"type":"array","jsonPath":"$.scores[*]","operatorType":"EQUALS","values":[]}`, "INVALID_CONDITION"},
	{"object entry", `{"type":"array","jsonPath":"$.scores[*]","operatorType":"EQUALS","values":[{"a":1}]}`, "INVALID_CONDITION"},
	{"BETWEEN not a pair", `{"type":"array","jsonPath":"$.scores[*]","operatorType":"BETWEEN","values":[5]}`, "INVALID_CONDITION"},
	{"values not an array", `{"type":"array","jsonPath":"$.scores[*]","operatorType":"EQUALS","values":"x"}`, "BAD_REQUEST"},
	{"bare path", `{"type":"array","jsonPath":"$.scores","operatorType":"EQUALS","values":[90]}`, "INVALID_FIELD_PATH"},
	{"text operator on numbers", `{"type":"array","jsonPath":"$.scores[*]","operatorType":"CONTAINS","values":["9"]}`, "CONDITION_TYPE_MISMATCH"},
}

// wantCodeOnParseFailureSurface returns the code a surface answers for tc. A
// body whose `values` is not an array fails to parse; search answers
// BAD_REQUEST, while conditional delete (entity/service.go,
// DeleteEntitiesConditional) and grouped stats (entity/grouped_stats_service.go,
// queryGroupedStatsInner) wrap the parse failure in ErrInvalidCondition and
// answer INVALID_CONDITION.
func wantCodeOnParseFailureSurface(code, name string) string {
	if name == "values not an array" {
		return "INVALID_CONDITION"
	}
	return code
}

func TestSearch_Sync_ArrayClauseOperator(t *testing.T) {
	const model = "e2e-array-op-sync"
	setupScoresModel(t, model)
	for _, tc := range []struct {
		name, cond, want string // want: the one entity name expected
	}{
		{"GREATER_THAN at 0 and 2", `{"type":"array","jsonPath":"$.scores[*]","operatorType":"GREATER_THAN","values":[50,null,70]}`, "A"},
		{"BETWEEN at 1", `{"type":"array","jsonPath":"$.scores[*]","operatorType":"BETWEEN_INCLUSIVE","values":[null,[30,45]]}`, "A"},
		{"Cloud alias operation", `{"type":"array","jsonPath":"$.scores[*]","operation":"LESS_THAN","values":[50]}`, "B"},
	} {
		status, results := directSearch(t, model, 1, tc.cond)
		if status != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d", tc.name, status)
		}
		if len(results) != 1 || extractDataString(t, results[0], "name") != tc.want {
			t.Errorf("%s: want only %s, got %v", tc.name, tc.want, results)
		}
	}
}

func TestSearch_Sync_ArrayClauseRefusals(t *testing.T) {
	const model = "e2e-array-op-sync-refuse"
	setupScoresModel(t, model)
	for _, tc := range arrayClauseRefusals {
		resp := doAuth(t, http.MethodPost, fmt.Sprintf("/api/search/direct/%s/1", model), tc.cond)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: expected 400, got %d; body: %s", tc.name, resp.StatusCode, readBody(t, resp))
		}
		commontest.ExpectErrorCode(t, resp, tc.code)
		resp.Body.Close()
	}
}

func TestSearch_Async_ArrayClauseOperator(t *testing.T) {
	const model = "e2e-array-op-async"
	idA, _ := setupScoresModel(t, model)
	jobID := submitAsyncSearch(t, model, 1, `{"type":"array","jsonPath":"$.scores[*]","operatorType":"GREATER_THAN","values":[50,null,70]}`)
	if s := waitForAsyncSearch(t, jobID, 10*time.Second); s != "SUCCESSFUL" {
		t.Fatalf("expected SUCCESSFUL, got %q", s)
	}
	resp := doAuth(t, http.MethodGet, fmt.Sprintf("/api/search/async/%s?pageSize=10&pageNumber=0", jobID), "")
	body := readBody(t, resp)
	var page struct {
		Content []struct {
			Meta struct {
				ID string `json:"id"`
			} `json:"meta"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatalf("decode: %v; body %s", err, body)
	}
	if len(page.Content) != 1 || page.Content[0].Meta.ID != idA {
		t.Errorf("want only %s, got %s", idA, body)
	}
}

func TestSearch_Async_ArrayClauseRefusals_NoJobIssued(t *testing.T) {
	const model = "e2e-array-op-async-refuse"
	setupScoresModel(t, model)
	for _, tc := range arrayClauseRefusals {
		resp := doAuth(t, http.MethodPost, fmt.Sprintf("/api/search/async/%s/1", model), tc.cond)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: expected 400, got %d; body: %s", tc.name, resp.StatusCode, readBody(t, resp))
		}
		commontest.ExpectErrorCode(t, resp, tc.code)
		resp.Body.Close()
	}
	if n := queryDB(t, "test-tenant", "SELECT count(*) FROM search_jobs WHERE model_name = $1", model); n != 0 {
		t.Errorf("refused conditions issued %d job(s)", n)
	}
}

func TestDeleteConditional_ArrayClauseOperator(t *testing.T) {
	const model = "e2e-array-op-delete"
	idA, idB := setupScoresModel(t, model)
	resp := doAuth(t, http.MethodDelete, fmt.Sprintf("/api/entity/%s/1", model),
		`{"type":"array","jsonPath":"$.scores[*]","operatorType":"GREATER_THAN","values":[null,90]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, readBody(t, resp))
	}
	resp.Body.Close()
	if r := doAuth(t, http.MethodGet, "/api/entity/"+idB, ""); r.StatusCode != http.StatusNotFound {
		t.Errorf("B (scores[1]=95) must be deleted, got %d", r.StatusCode)
	}
	if r := doAuth(t, http.MethodGet, "/api/entity/"+idA, ""); r.StatusCode != http.StatusOK {
		t.Errorf("A (scores[1]=40) must survive, got %d", r.StatusCode)
	}
}

func TestDeleteConditional_ArrayClauseRefusals_NothingDeleted(t *testing.T) {
	const model = "e2e-array-op-delete-refuse"
	idA, idB := setupScoresModel(t, model)
	for _, tc := range arrayClauseRefusals {
		resp := doAuth(t, http.MethodDelete, fmt.Sprintf("/api/entity/%s/1", model), tc.cond)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: expected 400, got %d; body: %s", tc.name, resp.StatusCode, readBody(t, resp))
		}
		commontest.ExpectErrorCode(t, resp, wantCodeOnParseFailureSurface(tc.code, tc.name))
		resp.Body.Close()
	}
	for _, id := range []string{idA, idB} {
		if r := doAuth(t, http.MethodGet, "/api/entity/"+id, ""); r.StatusCode != http.StatusOK {
			t.Errorf("entity %s must survive refused deletes, got %d", id, r.StatusCode)
		}
	}
}

func TestGroupedStats_ArrayClauseCondition(t *testing.T) {
	const model = "e2e-array-op-stats"
	setupScoresModel(t, model)
	path := fmt.Sprintf("/api/entity/stats/%s/1/query", model)
	resp := doAuth(t, http.MethodPost, path, `{"groupBy":["$.name"],"condition":`+
		`{"type":"array","jsonPath":"$.scores[*]","operatorType":"GREATER_THAN","values":[50,null,70]}}`)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	buckets := decodeBuckets(t, body)
	if len(buckets) != 1 || findBucket(buckets, "$.name", "A") == nil {
		t.Errorf("want one bucket for A, got %s", body)
	}
	for _, tc := range arrayClauseRefusals {
		r := doAuth(t, http.MethodPost, path, `{"groupBy":["$.name"],"condition":`+tc.cond+`}`)
		if r.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: expected 400, got %d; body: %s", tc.name, r.StatusCode, readBody(t, r))
		}
		commontest.ExpectErrorCode(t, r, wantCodeOnParseFailureSurface(tc.code, tc.name))
		r.Body.Close()
	}
}
