package parity

import (
	"encoding/json"
	"net/http"
	"sort"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// Cross-backend parity scenarios for chronological temporal search filters
// (issue #423): creationDate/lastUpdateTime compared via epoch-ms flooring
// (spi.ParseTemporalMillis / cyoda_epoch_millis), not lexical string
// equality. These prove memory/sqlite/postgres (+ commercial) agree on the
// same chronological ordering and the same millisecond-floor semantics.
// Companion to internal/e2e/search_temporal_test.go (running-postgres-only
// error-code table) — this file is the backend-agnostic slice.

// lifecycleTemporalCond builds a {"type":"lifecycle",...} condition JSON
// string. value may be a string (scalar operand) or []string (BETWEEN's
// [lo, hi]).
func lifecycleTemporalCond(t *testing.T, field, operatorType string, value any) string {
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

// assertResultIDSet fails the test unless results contains exactly the given
// entity IDs (order-independent — GT/LT/GE/LE/NE/BETWEEN don't guarantee a
// particular result ordering).
func assertResultIDSet(t *testing.T, label string, results []client.EntityResult, want []string) {
	t.Helper()
	got := make([]string, len(results))
	for i, r := range results {
		got[i] = r.Meta.ID
	}
	sort.Strings(got)
	wantSorted := append([]string(nil), want...)
	sort.Strings(wantSorted)
	if len(got) != len(wantSorted) {
		t.Errorf("[%s] want %d results %v, got %d %v", label, len(wantSorted), wantSorted, len(got), got)
		return
	}
	for i := range got {
		if got[i] != wantSorted[i] {
			t.Errorf("[%s] want result set %v, got %v", label, wantSorted, got)
			return
		}
	}
}

// RunSearchTemporalCreationDate seeds three entities A, B, C with 50ms gaps
// (guaranteeing distinct, chronologically ordered creationDate values),
// reads back their actual creationDate via a @creationDate:asc sorted
// search, and then asserts the full GT/GE/LT/LE/EQ/NE/BETWEEN chronological
// matrix against a lifecycle condition on creationDate.
//
// The final case in the matrix is the key cross-backend guard: an EQUALS
// operand truncated to millisecond precision must still match B even though
// it differs textually from the stored full-precision value. This proves
// every backend floors to milliseconds consistently — postgres via
// cyoda_epoch_millis, sqlite via integer-division-by-1000, memory via
// time.Time.UnixMilli — rather than comparing lexically or at native
// (possibly sub-ms) precision.
func RunSearchTemporalCreationDate(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-search-temporal-cd"
	const modelVersion = 1
	setupSearchModel(t, c, modelName, modelVersion)

	aID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"A","amount":1,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntity A: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	bID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"B","amount":2,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntity B: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	cID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"C","amount":3,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntity C: %v", err)
	}

	// Capture chronological creationDates via a match-all @creationDate:asc
	// sorted search (mirrors the setupTemporalEntities pattern in
	// internal/e2e/search_temporal_test.go).
	sorted, err := c.SyncSearchSorted(t, modelName, modelVersion, sortMatchAll, []string{"@creationDate:asc"})
	if err != nil {
		t.Fatalf("SyncSearchSorted: %v", err)
	}
	if len(sorted) != 3 {
		t.Fatalf("want 3 results, got %d", len(sorted))
	}
	ids := [3]string{aID.String(), bID.String(), cID.String()}
	for i, want := range ids {
		if sorted[i].Meta.ID != want {
			t.Fatalf("result[%d] id=%q, want %q (entities not chronologically distinct?)", i, sorted[i].Meta.ID, want)
		}
	}
	times := [3]time.Time{sorted[0].Meta.CreationDate, sorted[1].Meta.CreationDate, sorted[2].Meta.CreationDate}
	op := func(tm time.Time) string { return tm.UTC().Format(time.RFC3339Nano) }
	// opMillis truncates to millisecond precision (same offset-bearing
	// RFC3339 form spi.ParseTemporalMillis requires) — the mixed-precision
	// operand for the EQUALS-floor guard.
	opMillis := func(tm time.Time) string {
		return tm.UTC().Truncate(time.Millisecond).Format("2006-01-02T15:04:05.000Z07:00")
	}

	cases := []struct {
		label string
		op    string
		value any
		want  []string
	}{
		{"creationDate GT tA", "GREATER_THAN", op(times[0]), []string{ids[1], ids[2]}},
		{"creationDate GE tB", "GREATER_OR_EQUAL", op(times[1]), []string{ids[1], ids[2]}},
		{"creationDate LT tC", "LESS_THAN", op(times[2]), []string{ids[0], ids[1]}},
		{"creationDate LE tB", "LESS_OR_EQUAL", op(times[1]), []string{ids[0], ids[1]}},
		{"creationDate EQ tB", "EQUALS", op(times[1]), []string{ids[1]}},
		{"creationDate NE tB", "NOT_EQUAL", op(times[1]), []string{ids[0], ids[2]}},
		{"creationDate BETWEEN tA-tB", "BETWEEN", []string{op(times[0]), op(times[1])}, []string{ids[0], ids[1]}},
		// Mixed-precision EQ: ms-truncated form of tB must still match B —
		// the cross-backend flooring-consistency guard.
		{"creationDate EQ tB (ms-truncated)", "EQUALS", opMillis(times[1]), []string{ids[1]}},
	}

	for _, tc := range cases {
		cond := lifecycleTemporalCond(t, "creationDate", tc.op, tc.value)
		results, err := c.SyncSearch(t, modelName, modelVersion, cond)
		if err != nil {
			t.Fatalf("[%s] SyncSearch: %v", tc.label, err)
		}
		assertResultIDSet(t, tc.label, results, tc.want)
	}
}

// RunSearchTemporalLastUpdateTime seeds three entities A, B, C with 50ms
// gaps and asserts a chronological lastUpdateTime GREATER_THAN filter
// returns the same set as the equivalent creationDate filter would —
// lastUpdateTime == creationDate immediately after create (no updates have
// happened yet), so this exercises the same chronological compare path
// (not merely the same field) as RunSearchTemporalCreationDate's GT case.
func RunSearchTemporalLastUpdateTime(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-search-temporal-lut"
	const modelVersion = 1
	setupSearchModel(t, c, modelName, modelVersion)

	aID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"A","amount":1,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntity A: %v", err)
	}
	aEntity, err := c.GetEntity(t, aID)
	if err != nil {
		t.Fatalf("GetEntity A: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	bID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"B","amount":2,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntity B: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	cID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"C","amount":3,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntity C: %v", err)
	}

	cond := lifecycleTemporalCond(t, "lastUpdateTime", "GREATER_THAN", aEntity.Meta.LastUpdateTime.UTC().Format(time.RFC3339Nano))
	results, err := c.SyncSearch(t, modelName, modelVersion, cond)
	if err != nil {
		t.Fatalf("SyncSearch: %v", err)
	}
	assertResultIDSet(t, "lastUpdateTime GT tA", results, []string{bID.String(), cID.String()})
}

// RunSearchUnknownMetaField400 asserts a lifecycle condition referencing a
// meta field the vocabulary does not recognize is rejected with HTTP 400
// and errorCode INVALID_FIELD_PATH — on every backend, before any query
// work is attempted.
func RunSearchUnknownMetaField400(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-search-temporal-400"
	const modelVersion = 1
	setupSearchModel(t, c, modelName, modelVersion)

	if _, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"A","amount":1,"status":"new"}`); err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	cond := lifecycleTemporalCond(t, "bogus", "EQUALS", "x")
	status, body, err := c.SyncSearchRaw(t, modelName, modelVersion, cond)
	if err != nil {
		t.Fatalf("SyncSearchRaw: %v", err)
	}
	if status != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d; body=%s", status, body)
	}
	if !containsErrorCode(body, "INVALID_FIELD_PATH") {
		t.Errorf("expected errorCode INVALID_FIELD_PATH, body=%s", body)
	}
}

// RunSearchBetweenArity400 asserts a BETWEEN lifecycle condition with a
// malformed operand (a scalar, not the required 2-element [lo, hi] array) is
// rejected with HTTP 400 and errorCode BAD_REQUEST on every backend — before
// the fix, this diverged catastrophically: postgres panicked indexing an
// empty spi.Filter.Values (500), sqlite's BETWEEN fallback emitted a
// match-all "1=1" (200, wrong result set), and only memory's spi.MatchFilter
// happened to exclude correctly. Companion to RunSearchUnknownMetaField400 —
// both prove the SearchService validation boundary (ValidateCondition) runs
// identically across backends before any store is touched.
func RunSearchBetweenArity400(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-search-between-arity-400"
	const modelVersion = 1
	setupSearchModel(t, c, modelName, modelVersion)

	if _, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"A","amount":1,"status":"new"}`); err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	cond := lifecycleTemporalCond(t, "creationDate", "BETWEEN", "2021-01-01T00:00:00Z")
	status, body, err := c.SyncSearchRaw(t, modelName, modelVersion, cond)
	if err != nil {
		t.Fatalf("SyncSearchRaw: %v", err)
	}
	if status != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d; body=%s", status, body)
	}
	if !containsErrorCode(body, "BAD_REQUEST") {
		t.Errorf("expected errorCode BAD_REQUEST, body=%s", body)
	}
}

// RunSearchStringMetaVocabulary verifies the canonical string-shaped meta
// fields (id, transactionId, state, transitionForLatestSave) resolve
// identically across backends when filtered with a lifecycle condition —
// the meta-key mapping reconciliation companion to the temporal (date-typed)
// fields above. These fields compare as their stored text form regardless
// of operator (see condition_type_validate.go:validateLifecycleType).
func RunSearchStringMetaVocabulary(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-search-temporal-meta-vocab"
	const modelVersion = 1
	setupSearchModel(t, c, modelName, modelVersion)

	aID, txID, err := c.CreateEntityWithTxID(t, modelName, modelVersion, `{"name":"A","amount":1,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntityWithTxID A: %v", err)
	}
	bID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"B","amount":2,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntity B: %v", err)
	}

	// id EQUALS <known entity id> resolves exactly that entity.
	idResults, err := c.SyncSearch(t, modelName, modelVersion, lifecycleTemporalCond(t, "id", "EQUALS", aID.String()))
	if err != nil {
		t.Fatalf("SyncSearch (id): %v", err)
	}
	assertResultIDSet(t, "id EQUALS", idResults, []string{aID.String()})

	// transactionId EQUALS <known txId> resolves exactly the entity created
	// in that transaction. Checked before the approve transition below,
	// which stamps A with a new transactionId (transactionId tracks the
	// latest save, not the creating transaction).
	txResults, err := c.SyncSearch(t, modelName, modelVersion, lifecycleTemporalCond(t, "transactionId", "EQUALS", txID))
	if err != nil {
		t.Fatalf("SyncSearch (transactionId): %v", err)
	}
	assertResultIDSet(t, "transactionId EQUALS", txResults, []string{aID.String()})

	// Promote A via the manual "approve" transition — this is what makes
	// TransitionForLatestSave carry an explicit, caller-supplied name
	// ("approve") rather than the "loopback" sentinel every entity gets
	// on create (create runs the workflow engine without a client-supplied
	// transition; see entity/service.go CreateEntity).
	if err := c.UpdateEntity(t, aID, "approve", `{"name":"A","amount":1,"status":"approved"}`); err != nil {
		t.Fatalf("UpdateEntity (approve A): %v", err)
	}

	// state EQUALS APPROVED resolves exactly A.
	stateResults, err := c.SyncSearch(t, modelName, modelVersion, lifecycleTemporalCond(t, "state", "EQUALS", "APPROVED"))
	if err != nil {
		t.Fatalf("SyncSearch (state): %v", err)
	}
	assertResultIDSet(t, "state EQUALS APPROVED", stateResults, []string{aID.String()})

	// transitionForLatestSave EQUALS "approve" resolves exactly A (the
	// explicit transition name it was saved with); B still carries the
	// create-time "loopback" sentinel.
	transitionResults, err := c.SyncSearch(t, modelName, modelVersion, lifecycleTemporalCond(t, "transitionForLatestSave", "EQUALS", "approve"))
	if err != nil {
		t.Fatalf("SyncSearch (transitionForLatestSave=approve): %v", err)
	}
	assertResultIDSet(t, "transitionForLatestSave EQUALS approve", transitionResults, []string{aID.String()})

	loopbackResults, err := c.SyncSearch(t, modelName, modelVersion, lifecycleTemporalCond(t, "transitionForLatestSave", "EQUALS", "loopback"))
	if err != nil {
		t.Fatalf("SyncSearch (transitionForLatestSave=loopback): %v", err)
	}
	assertResultIDSet(t, "transitionForLatestSave EQUALS loopback", loopbackResults, []string{bID.String()})
}
