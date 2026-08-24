package e2e_test

// grouped_stats_invalid_path_test.go — running-backend (postgres) e2e
// coverage for the grouped-stats path grammar enforced at the API boundary.
//
// A groupBy path or aggregation field that falls outside the storage-SPI
// dotted-identifier grammar used to be caught only by the storage plugin, and
// only on the pushdown path. Whenever pushdown was declined — a residual
// filter, a point-in-time query, sqlite declining stdev — the service fell
// through to the in-process streaming tally, which resolves the path with
// gjson: the lookup missed, every entity landed in a single null bucket, and
// the caller got a 200 with plausible-looking-but-wrong groups. That is the
// wrong-but-available answer .claude/rules/correctness-over-availability.md
// forbids. Validation now runs at the boundary, so both execution paths
// reject identically and the plugin check is a backstop.

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/common/commontest"
)

// TestGroupedStats_MalformedGroupByPath_Returns400 covers the pushdown-eligible
// shape (no condition, no point-in-time): a groupBy path carrying a quote and a
// semicolon must be rejected with 400 INVALID_GROUP_BY_PATH.
func TestGroupedStats_MalformedGroupByPath_Returns400(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}

	const model = "e2e-grouped-stats-badpath"
	setupStatsModel(t, model)
	createEntityE2E(t, model, 1, `{"variantId":"v1","price":10.0}`)

	reqBody := `{"groupBy": ["$.variantId';x"]}`
	path := fmt.Sprintf("/api/entity/stats/%s/1/query", model)
	resp := doAuth(t, http.MethodPost, path, reqBody)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", resp.StatusCode, readBody(t, resp))
	}
	commontest.ExpectErrorCode(t, resp, "INVALID_GROUP_BY_PATH")
}

// TestGroupedStats_MalformedGroupByPath_PointInTime_Returns400 is the
// regression test proper. A point-in-time request makes postgres decline
// pushdown (spi.ErrAggregationNotPushdownable), so the service takes the
// streaming-tally branch — the branch that never validated the path. Pre-fix
// this returned 200 with a single bucket keyed on null; it must now be a 400.
func TestGroupedStats_MalformedGroupByPath_PointInTime_Returns400(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}

	const model = "e2e-grouped-stats-badpath-pit"
	setupStatsModel(t, model)
	createEntityE2E(t, model, 1, `{"variantId":"v1","price":10.0}`)
	createEntityE2E(t, model, 1, `{"variantId":"v2","price":20.0}`)

	pit := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
	reqBody := fmt.Sprintf(`{"groupBy": ["$.variantId';x"], "pointInTime": %q}`, pit)
	path := fmt.Sprintf("/api/entity/stats/%s/1/query", model)
	resp := doAuth(t, http.MethodPost, path, reqBody)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", resp.StatusCode, readBody(t, resp))
	}
	commontest.ExpectErrorCode(t, resp, "INVALID_GROUP_BY_PATH")
}

// TestGroupedStats_MalformedAggregationField_Returns400 covers the sibling
// code: the same grammar applies to an aggregation `field`.
func TestGroupedStats_MalformedAggregationField_Returns400(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}

	const model = "e2e-grouped-stats-badaggfield"
	setupStatsModel(t, model)
	createEntityE2E(t, model, 1, `{"variantId":"v1","price":10.0}`)

	reqBody := `{"groupBy": ["$.variantId"], "aggregations": [{"op":"sum","field":"$.price OR 1=1"}]}`
	path := fmt.Sprintf("/api/entity/stats/%s/1/query", model)
	resp := doAuth(t, http.MethodPost, path, reqBody)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", resp.StatusCode, readBody(t, resp))
	}
	commontest.ExpectErrorCode(t, resp, "INVALID_AGGREGATION_FIELD")
}

// TestGroupedStats_ValidPathForms_Still200 is the positive control over real
// HTTP: the JSON Path form must still be accepted and must still produce the
// `$.variantId` group key — on both the pushdown branch and (with pointInTime)
// the streaming branch — and so must the reserved `state` token, which names
// the lifecycle state rather than a data path and is exempt from the leader
// rule. A tightening that breaks valid callers is worse than the bug it fixes.
func TestGroupedStats_ValidPathForms_Still200(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}

	const model = "e2e-grouped-stats-validpath"
	setupStatsModel(t, model)
	createEntityE2E(t, model, 1, `{"variantId":"v1","price":10.0}`)
	createEntityE2E(t, model, 1, `{"variantId":"v2","price":20.0}`)

	pit := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
	cases := []struct {
		name string
		body string
	}{
		{"dotted leader, pushdown", `{"groupBy": ["$.variantId"]}`},
		{"reserved state token", `{"groupBy": ["state"]}`},
		{"dotted leader, streaming", fmt.Sprintf(`{"groupBy": ["$.variantId"], "pointInTime": %q}`, pit)},
		{"aggregation over dotted field",
			`{"groupBy": ["$.variantId"], "aggregations": [{"op":"sum","field":"$.price"}]}`},
	}
	path := fmt.Sprintf("/api/entity/stats/%s/1/query", model)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := doAuth(t, http.MethodPost, path, tc.body)
			body := readBody(t, resp)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
			}
			buckets := decodeBuckets(t, body)
			if tc.name == "reserved state token" {
				if findBucket(buckets, "state", "CREATED") == nil {
					t.Fatalf("expected a state=CREATED bucket, got %s", body)
				}
				return
			}
			// Every non-state case groups by variantId and must key on the
			// `$.variantId` form the request sent, with real (non-null) values.
			if len(buckets) != 2 {
				t.Fatalf("expected 2 variantId buckets, got %d: %s", len(buckets), body)
			}
			for _, want := range []string{"v1", "v2"} {
				if findBucket(buckets, "$.variantId", want) == nil {
					t.Fatalf("missing bucket $.variantId=%s: %s", want, body)
				}
			}
		})
	}
}

// TestGroupedStats_NonJSONPathForms_Returns400 is the running-backend proof
// that the leader rule is enforced on the groupBy / aggregation-field surface.
//
// A bare "variantId" is not a JSON Path, and it used to be silently read as
// "$.variantId": the request succeeded and the response echoed a group-key
// path spelling the client never sent. Bracket-quoted access used to be folded
// into dotted form here, while the condition surface rejects it — so one
// request could be accepted in `groupBy` and 400'd in `condition` for the same
// spelling of the same field.
//
// Both the pushdown and the streaming (pointInTime) branch are covered: the
// whole reason validation moved to the boundary is that the two branches
// otherwise disagree.
func TestGroupedStats_NonJSONPathForms_Returns400(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}

	const model = "e2e-grouped-stats-nonjsonpath"
	setupStatsModel(t, model)
	createEntityE2E(t, model, 1, `{"variantId":"v1","price":10.0}`)
	createEntityE2E(t, model, 1, `{"variantId":"v2","price":20.0}`)

	pit := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
	cases := []struct {
		name string
		body string
		code string
	}{
		{"bare identifier, pushdown", `{"groupBy": ["variantId"]}`, "INVALID_GROUP_BY_PATH"},
		{"bare identifier, streaming",
			fmt.Sprintf(`{"groupBy": ["variantId"], "pointInTime": %q}`, pit), "INVALID_GROUP_BY_PATH"},
		{"bracket quoted after leader", `{"groupBy": ["$.['variantId']"]}`, "INVALID_GROUP_BY_PATH"},
		{"bracket chain", `{"groupBy": ["$['variantId']"]}`, "INVALID_GROUP_BY_PATH"},
		{"array projection", `{"groupBy": ["$.variantId[*]"]}`, "INVALID_GROUP_BY_PATH"},
		{"bare aggregation field",
			`{"groupBy": ["$.variantId"], "aggregations": [{"op":"sum","field":"price"}]}`,
			"INVALID_AGGREGATION_FIELD"},
		{"bracket-quoted aggregation field",
			`{"groupBy": ["$.variantId"], "aggregations": [{"op":"sum","field":"$.['price']"}]}`,
			"INVALID_AGGREGATION_FIELD"},
		// "state" is a groupBy token, not a path, and there is no defined
		// aggregate over a lifecycle state — as an aggregation field it is just
		// an identifier missing its leader.
		{"state as aggregation field",
			`{"groupBy": ["$.variantId"], "aggregations": [{"op":"sum","field":"state"}]}`,
			"INVALID_AGGREGATION_FIELD"},
	}
	path := fmt.Sprintf("/api/entity/stats/%s/1/query", model)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := doAuth(t, http.MethodPost, path, tc.body)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", resp.StatusCode, readBody(t, resp))
			}
			commontest.ExpectErrorCode(t, resp, tc.code)
		})
	}
}

// TestGroupedStats_NonJSONPathCondition_Returns400 covers the OTHER path
// surface the same request carries. A grouped-stats `condition` is an ordinary
// predicate tree, so its jsonPath obeys the same grammar — and must be refused
// with the same code /search uses, not a grouped-stats-specific one.
func TestGroupedStats_NonJSONPathCondition_Returns400(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}

	const model = "e2e-grouped-stats-condpath"
	setupStatsModel(t, model)
	createEntityE2E(t, model, 1, `{"variantId":"v1","price":10.0}`)

	cases := []struct {
		name string
		path string
	}{
		{"bare identifier", "variantId"},
		{"bracket quoted", "$['variantId']"},
		{"trailing dot", "$.variantId."},
	}
	path := fmt.Sprintf("/api/entity/stats/%s/1/query", model)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(
				`{"groupBy": ["$.variantId"], "condition": {"type":"simple","jsonPath":%q,"operatorType":"EQUALS","value":"v1"}}`,
				tc.path)
			resp := doAuth(t, http.MethodPost, path, body)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", resp.StatusCode, readBody(t, resp))
			}
			commontest.ExpectErrorCode(t, resp, "INVALID_FIELD_PATH")
		})
	}
}
