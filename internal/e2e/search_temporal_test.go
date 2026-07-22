package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/common/commontest"
)

// ---------------------------------------------------------------------------
// Temporal search filters (issue #423): creationDate / lastUpdateTime
// chronological compare via LifecycleCondition, on a running Postgres
// backend through the full HTTP stack.
// ---------------------------------------------------------------------------

// lifecycleCond builds a {"type":"lifecycle",...} condition JSON string.
// value may be a string (scalar operand) or []string (BETWEEN's [lo, hi]).
func lifecycleCond(t *testing.T, field, operatorType string, value any) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"type":         "lifecycle",
		"field":        field,
		"operatorType": operatorType,
		"value":        value,
	})
	if err != nil {
		t.Fatalf("marshal lifecycle condition: %v", err)
	}
	return string(b)
}

// idSet returns the sorted set of entity IDs (via meta.id) from search results.
func idSet(t *testing.T, results []map[string]any) []string {
	t.Helper()
	ids := make([]string, len(results))
	for i, r := range results {
		ids[i] = resultMetaID(t, r)
	}
	sort.Strings(ids)
	return ids
}

// assertIDSet fails the test unless results contains exactly the given IDs
// (order-independent — GT/LT/GE/LE/NE/BETWEEN don't guarantee ordering).
func assertIDSet(t *testing.T, results []map[string]any, want []string) {
	t.Helper()
	got := idSet(t, results)
	wantSorted := append([]string(nil), want...)
	sort.Strings(wantSorted)
	if len(got) != len(wantSorted) {
		t.Fatalf("expected %d results %v, got %d %v", len(wantSorted), wantSorted, len(got), got)
	}
	for i := range got {
		if got[i] != wantSorted[i] {
			t.Fatalf("expected result set %v, got %v", wantSorted, got)
		}
	}
}

// truncateToMillis re-serializes an RFC3339(Nano) instant string truncated to
// millisecond precision, preserving the offset. Used to prove EQUALS compares
// via epoch-ms flooring (spi.ParseTemporalMillis / cyoda_epoch_millis), not
// lexical string equality against the full-precision stored representation.
func truncateToMillis(t *testing.T, rfc3339 string) string {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339Nano, rfc3339)
	if err != nil {
		t.Fatalf("parse timestamp %q: %v", rfc3339, err)
	}
	return parsed.Truncate(time.Millisecond).Format("2006-01-02T15:04:05.000Z07:00")
}

// setupTemporalEntities creates three entities A, B, C on model with a 50ms
// gap between each (guaranteeing distinct, chronologically ordered
// creationDate/lastUpdateTime), then reads back their meta.creationDate via
// a @creationDate:asc sorted search. Returns entity IDs and creationDate
// strings in chronological order (A, B, C).
func setupTemporalEntities(t *testing.T, model string) (ids [3]string, times [3]string) {
	t.Helper()
	setupSearchModel(t, model)

	idA := createEntityE2E(t, model, 1, `{"name":"A","amount":1,"status":"new"}`)
	time.Sleep(50 * time.Millisecond)
	idB := createEntityE2E(t, model, 1, `{"name":"B","amount":2,"status":"new"}`)
	time.Sleep(50 * time.Millisecond)
	idC := createEntityE2E(t, model, 1, `{"name":"C","amount":3,"status":"new"}`)

	status, results := directSearchSorted(t, model, 1, matchAllCond, []string{"@creationDate:asc"})
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}

	wantIDs := []string{idA, idB, idC}
	for i, wantID := range wantIDs {
		gotID := resultMetaID(t, results[i])
		if gotID != wantID {
			t.Fatalf("result[%d] id = %q, want %q (creation order) — entities not chronologically distinct?", i, gotID, wantID)
		}
	}

	for i := range results {
		meta, ok := results[i]["meta"].(map[string]any)
		if !ok {
			t.Fatalf("result[%d] has no meta object: %v", i, results[i])
		}
		cd, ok := meta["creationDate"].(string)
		if !ok || cd == "" {
			t.Fatalf("result[%d] meta.creationDate missing or not a string: %v", i, meta)
		}
		times[i] = cd
	}

	return [3]string{idA, idB, idC}, times
}

// --- Chronological correctness: creationDate ---

func TestSearchTemporal_CreationDate_GreaterThan(t *testing.T) {
	const model = "e2e-search-temporal-cd-gt"
	ids, times := setupTemporalEntities(t, model)

	cond := lifecycleCond(t, "creationDate", "GREATER_THAN", times[0])
	status, results := directSearch(t, model, 1, cond)
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	assertIDSet(t, results, []string{ids[1], ids[2]})
}

func TestSearchTemporal_CreationDate_GreaterOrEqual(t *testing.T) {
	const model = "e2e-search-temporal-cd-ge"
	ids, times := setupTemporalEntities(t, model)

	cond := lifecycleCond(t, "creationDate", "GREATER_OR_EQUAL", times[1])
	status, results := directSearch(t, model, 1, cond)
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	assertIDSet(t, results, []string{ids[1], ids[2]})
}

func TestSearchTemporal_CreationDate_LessThan(t *testing.T) {
	const model = "e2e-search-temporal-cd-lt"
	ids, times := setupTemporalEntities(t, model)

	cond := lifecycleCond(t, "creationDate", "LESS_THAN", times[2])
	status, results := directSearch(t, model, 1, cond)
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	assertIDSet(t, results, []string{ids[0], ids[1]})
}

func TestSearchTemporal_CreationDate_LessOrEqual(t *testing.T) {
	const model = "e2e-search-temporal-cd-le"
	ids, times := setupTemporalEntities(t, model)

	cond := lifecycleCond(t, "creationDate", "LESS_OR_EQUAL", times[1])
	status, results := directSearch(t, model, 1, cond)
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	assertIDSet(t, results, []string{ids[0], ids[1]})
}

func TestSearchTemporal_CreationDate_Equals(t *testing.T) {
	const model = "e2e-search-temporal-cd-eq"
	ids, times := setupTemporalEntities(t, model)

	cond := lifecycleCond(t, "creationDate", "EQUALS", times[1])
	status, results := directSearch(t, model, 1, cond)
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	assertIDSet(t, results, []string{ids[1]})
}

// TestSearchTemporal_CreationDate_Equals_MixedPrecision proves EQUALS compares
// via epoch-ms flooring (cyoda_epoch_millis / spi.ParseTemporalMillis), not
// lexical string equality: a millisecond-truncated form of B's exact instant
// must still match B even though the operand string differs textually from
// the stored full-precision value.
func TestSearchTemporal_CreationDate_Equals_MixedPrecision(t *testing.T) {
	const model = "e2e-search-temporal-cd-eq-mixed"
	ids, times := setupTemporalEntities(t, model)

	truncated := truncateToMillis(t, times[1])
	cond := lifecycleCond(t, "creationDate", "EQUALS", truncated)
	status, results := directSearch(t, model, 1, cond)
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	assertIDSet(t, results, []string{ids[1]})
}

func TestSearchTemporal_CreationDate_NotEqual(t *testing.T) {
	const model = "e2e-search-temporal-cd-ne"
	ids, times := setupTemporalEntities(t, model)

	cond := lifecycleCond(t, "creationDate", "NOT_EQUAL", times[1])
	status, results := directSearch(t, model, 1, cond)
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	assertIDSet(t, results, []string{ids[0], ids[2]})
}

func TestSearchTemporal_CreationDate_Between(t *testing.T) {
	const model = "e2e-search-temporal-cd-between"
	ids, times := setupTemporalEntities(t, model)

	cond := lifecycleCond(t, "creationDate", "BETWEEN", []string{times[0], times[1]})
	status, results := directSearch(t, model, 1, cond)
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	assertIDSet(t, results, []string{ids[0], ids[1]})
}

// --- lastUpdateTime ---

// TestSearchTemporal_LastUpdateTime_GreaterThan verifies lastUpdateTime is
// wired through the same chronological (not lexical) comparison path.
// lastUpdateTime == creationDate immediately after create (no updates have
// happened), so filtering on lastUpdateTime > A's creationDate must return
// exactly {B, C} — the same set as the creationDate GT case.
func TestSearchTemporal_LastUpdateTime_GreaterThan(t *testing.T) {
	const model = "e2e-search-temporal-lut-gt"
	ids, times := setupTemporalEntities(t, model)

	cond := lifecycleCond(t, "lastUpdateTime", "GREATER_THAN", times[0])
	status, results := directSearch(t, model, 1, cond)
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	assertIDSet(t, results, []string{ids[1], ids[2]})
}

// --- Accepted 200 (explicit status check on top of the matrix above) ---

func TestSearchTemporal_Accepted200(t *testing.T) {
	const model = "e2e-search-temporal-accepted"
	ids, _ := setupTemporalEntities(t, model)

	cond := lifecycleCond(t, "creationDate", "GREATER_THAN", "2000-01-01T00:00:00Z")
	status, results := directSearch(t, model, 1, cond)
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	assertIDSet(t, results, []string{ids[0], ids[1], ids[2]})
}

// --- 400 error table ---

func TestSearchTemporal_400_StringOpOnTemporalField(t *testing.T) {
	const model = "e2e-search-temporal-400-string-op"
	setupSearchModel(t, model)
	createEntityE2E(t, model, 1, `{"name":"A","amount":1,"status":"new"}`)

	cond := lifecycleCond(t, "creationDate", "CONTAINS", "2021")
	resp := doAuth(t, http.MethodPost, fmt.Sprintf("/api/search/direct/%s/%d", model, 1), cond)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		body := readBody(t, resp)
		t.Fatalf("expected 400, got %d; body: %s", resp.StatusCode, body)
	}
	commontest.ExpectErrorCode(t, resp, "CONDITION_TYPE_MISMATCH")
}

func TestSearchTemporal_400_BadOperand(t *testing.T) {
	const model = "e2e-search-temporal-400-bad-operand"
	setupSearchModel(t, model)
	createEntityE2E(t, model, 1, `{"name":"A","amount":1,"status":"new"}`)

	cond := lifecycleCond(t, "creationDate", "GREATER_THAN", "not-a-date")
	resp := doAuth(t, http.MethodPost, fmt.Sprintf("/api/search/direct/%s/%d", model, 1), cond)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		body := readBody(t, resp)
		t.Fatalf("expected 400, got %d; body: %s", resp.StatusCode, body)
	}
	commontest.ExpectErrorCode(t, resp, "CONDITION_TYPE_MISMATCH")
}

// TestSearchTemporal_400_OffsetLessOperand verifies an RFC3339 timestamp
// missing its mandatory UTC offset is rejected — spi.ParseTemporalMillis
// requires a full offset-bearing instant (see temporal.go doc comment).
func TestSearchTemporal_400_OffsetLessOperand(t *testing.T) {
	const model = "e2e-search-temporal-400-no-offset"
	setupSearchModel(t, model)
	createEntityE2E(t, model, 1, `{"name":"A","amount":1,"status":"new"}`)

	cond := lifecycleCond(t, "creationDate", "GREATER_THAN", "2021-01-01T00:00:00")
	resp := doAuth(t, http.MethodPost, fmt.Sprintf("/api/search/direct/%s/%d", model, 1), cond)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		body := readBody(t, resp)
		t.Fatalf("expected 400, got %d; body: %s", resp.StatusCode, body)
	}
	commontest.ExpectErrorCode(t, resp, "CONDITION_TYPE_MISMATCH")
}

func TestSearchTemporal_400_UnknownMetaField(t *testing.T) {
	const model = "e2e-search-temporal-400-unknown-field"
	setupSearchModel(t, model)
	createEntityE2E(t, model, 1, `{"name":"A","amount":1,"status":"new"}`)

	cond := lifecycleCond(t, "bogusField", "EQUALS", "x")
	resp := doAuth(t, http.MethodPost, fmt.Sprintf("/api/search/direct/%s/%d", model, 1), cond)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		body := readBody(t, resp)
		t.Fatalf("expected 400, got %d; body: %s", resp.StatusCode, body)
	}
	commontest.ExpectErrorCode(t, resp, "INVALID_FIELD_PATH")
}
