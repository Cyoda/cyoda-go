package parity

import (
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// sortSimpleWorkflowJSON is a minimal workflow for sort scenarios that do not
// need lifecycle state changes — just the automatic NONE→CREATED init transition.
// Scenarios that need the approve transition use searchWorkflowJSON from search.go.
const sortSimpleWorkflowJSON = `{
	"importMode": "REPLACE",
	"workflows": [{
		"version": "1.1", "name": "sort-wf", "initialState": "NONE", "active": true,
		"states": {
			"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
			"CREATED": {}
		}
	}]
}`

// sortMatchAll matches every entity (reused across sort parity scenarios).
const sortMatchAll = `{"type":"group","operator":"AND","conditions":[]}`

// setupSortModel imports the default sample document, locks the model, and
// attaches the simple sort workflow (NONE→CREATED, no manual transitions).
func setupSortModel(t *testing.T, c *client.Client, modelName string, modelVersion int) {
	t.Helper()
	setupModelWithWorkflow(t, c, modelName, modelVersion, sortSimpleWorkflowJSON)
}

// setupSortModelWithSample is like setupSortModel but accepts a custom sample
// document, which determines the model schema (field types, nested paths).
// Use this for scenarios that need non-default fields (boolean, score, etc.).
func setupSortModelWithSample(t *testing.T, c *client.Client, modelName string, modelVersion int, sampleDoc string) {
	t.Helper()
	if err := c.ImportModel(t, modelName, modelVersion, sampleDoc); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	if err := c.LockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	if err := c.ImportWorkflow(t, modelName, modelVersion, sortSimpleWorkflowJSON); err != nil {
		t.Fatalf("ImportWorkflow: %v", err)
	}
}

// assertSortedIDs verifies that the search results carry exactly the given IDs
// in the specified order.  label is used in error messages to identify the
// call site (e.g. "asc", "desc").
func assertSortedIDs(t *testing.T, label string, results []client.EntityResult, wantIDs []string) {
	t.Helper()
	if len(results) != len(wantIDs) {
		t.Errorf("[%s] want %d results, got %d", label, len(wantIDs), len(results))
		return
	}
	for i, want := range wantIDs {
		if got := results[i].Meta.ID; got != want {
			t.Errorf("[%s] result[%d] id=%q, want %q", label, i, got, want)
		}
	}
}

// RunSearchSortDataText seeds three entities with distinct string names
// (Charlie, Alice, Bob) and verifies that sort=name:asc returns them in
// ascending lexicographic order and sort=name:desc returns them in descending
// order.  This is the baseline cross-backend text-sort contract.
func RunSearchSortDataText(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-sort-text"
	const modelVersion = 1
	setupSortModel(t, c, modelName, modelVersion)

	charlieID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Charlie","amount":30,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntity Charlie: %v", err)
	}
	aliceID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Alice","amount":10,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntity Alice: %v", err)
	}
	bobID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Bob","amount":20,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntity Bob: %v", err)
	}

	asc, err := c.SyncSearchSorted(t, modelName, modelVersion, sortMatchAll, []string{"name:asc"})
	if err != nil {
		t.Fatalf("SyncSearchSorted asc: %v", err)
	}
	assertSortedIDs(t, "name:asc", asc, []string{
		aliceID.String(), bobID.String(), charlieID.String(),
	})

	desc, err := c.SyncSearchSorted(t, modelName, modelVersion, sortMatchAll, []string{"name:desc"})
	if err != nil {
		t.Fatalf("SyncSearchSorted desc: %v", err)
	}
	assertSortedIDs(t, "name:desc", desc, []string{
		charlieID.String(), bobID.String(), aliceID.String(),
	})
}

// RunSearchSortDataNumeric seeds entities with amounts 9, 100, 10 and asserts
// that sort=amount:asc returns them in numeric order (9→10→100), not in the
// lexicographic order that string comparison would produce (10→100→9).
// This scenario is the explicit guard for the lexical-vs-numeric divergence
// across backends.
func RunSearchSortDataNumeric(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-sort-numeric"
	const modelVersion = 1
	setupSortModel(t, c, modelName, modelVersion)

	// Inserted in non-sorted order to prove sorting is applied.
	id9, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"nine","amount":9,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntity 9: %v", err)
	}
	id100, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"hundred","amount":100,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntity 100: %v", err)
	}
	id10, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"ten","amount":10,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntity 10: %v", err)
	}

	// Numeric ascending: 9 < 10 < 100.
	// Lexicographic ascending would give: "10" < "100" < "9" — wrong.
	results, err := c.SyncSearchSorted(t, modelName, modelVersion, sortMatchAll, []string{"amount:asc"})
	if err != nil {
		t.Fatalf("SyncSearchSorted: %v", err)
	}
	assertSortedIDs(t, "amount:asc", results, []string{
		id9.String(), id10.String(), id100.String(),
	})
}

// RunSearchSortDataBool seeds one entity with active=false and one with
// active=true.  Asserts that sort=active:asc places false before true (the
// canonical false<true ordering) and sort=active:desc reverses it.
// This guards divergence between SQLite's 0/1 storage, Postgres's ::boolean
// cast, and the Go cmpBool comparator in the in-memory path.
func RunSearchSortDataBool(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-sort-bool"
	const modelVersion = 1
	// Schema must include a boolean field so the engine infers OrderBool.
	setupSortModelWithSample(t, c, modelName, modelVersion,
		`{"name":"Test","amount":10,"status":"new","active":true}`)

	falseID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"inactive","amount":1,"status":"new","active":false}`)
	if err != nil {
		t.Fatalf("CreateEntity false: %v", err)
	}
	trueID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"active","amount":2,"status":"new","active":true}`)
	if err != nil {
		t.Fatalf("CreateEntity true: %v", err)
	}

	// Ascending: false < true.
	asc, err := c.SyncSearchSorted(t, modelName, modelVersion, sortMatchAll, []string{"active:asc"})
	if err != nil {
		t.Fatalf("SyncSearchSorted asc: %v", err)
	}
	assertSortedIDs(t, "active:asc", asc, []string{falseID.String(), trueID.String()})

	// Descending: true > false.
	desc, err := c.SyncSearchSorted(t, modelName, modelVersion, sortMatchAll, []string{"active:desc"})
	if err != nil {
		t.Fatalf("SyncSearchSorted desc: %v", err)
	}
	assertSortedIDs(t, "active:desc", desc, []string{trueID.String(), falseID.String()})
}

// RunSearchSortMetaCreationDate seeds four entities (A, B, C, D) and asserts
// that sort=@creationDate:asc returns them in the canonical millisecond-floor
// order with entity_id as the tiebreaker.
//
// Sub-millisecond guard design: B and C are created with no sleep between them
// so they are likely to share the same millisecond-floored creation timestamp.
// A and D are separated by ≥1 ms sleeps to guarantee distinct millisecond
// buckets.  The expected order is computed AFTER creation by reading each
// entity's actual creationDate from the server, truncating to milliseconds, and
// applying the canonical (ms, entity_id-asc) sort.  A backend that does NOT
// floor timestamps to milliseconds would use sub-ms precision and might order B
// and C differently from the entity_id tiebreaker — causing a test failure.
func RunSearchSortMetaCreationDate(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-sort-date"
	const modelVersion = 1
	setupSortModel(t, c, modelName, modelVersion)

	aID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"alpha","amount":1,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntity A: %v", err)
	}
	// Ensure A is in a strictly earlier millisecond bucket than B and C.
	time.Sleep(2 * time.Millisecond)

	bID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"beta","amount":2,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntity B: %v", err)
	}
	// No sleep: B and C are created as close together as possible so they
	// have a high chance of sharing the same millisecond (sub-ms pair).
	cID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"gamma","amount":3,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntity C: %v", err)
	}

	// Ensure D is in a strictly later millisecond bucket than B and C.
	time.Sleep(2 * time.Millisecond)
	dID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"delta","amount":4,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntity D: %v", err)
	}

	// Read back actual creation timestamps from the server, then compute the
	// expected sort order using the canonical (ms-floor, entity_id-asc) rule.
	type eCreation struct {
		id string
		ms int64 // UnixMilli — the canonical sort key
	}

	ids := []uuid.UUID{aID, bID, cID, dID}
	entries := make([]eCreation, 0, len(ids))
	for _, id := range ids {
		ent, err := c.GetEntity(t, id)
		if err != nil {
			t.Fatalf("GetEntity(%s): %v", id, err)
		}
		entries = append(entries, eCreation{
			id: ent.Meta.ID,
			ms: ent.Meta.CreationDate.Truncate(time.Millisecond).UnixMilli(),
		})
	}

	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].ms != entries[j].ms {
			return entries[i].ms < entries[j].ms
		}
		return entries[i].id < entries[j].id // entity_id ascending tiebreaker
	})

	wantIDs := make([]string, len(entries))
	for i, e := range entries {
		wantIDs[i] = e.id
	}

	results, err := c.SyncSearchSorted(t, modelName, modelVersion, sortMatchAll, []string{"@creationDate:asc"})
	if err != nil {
		t.Fatalf("SyncSearchSorted: %v", err)
	}
	assertSortedIDs(t, "@creationDate:asc", results, wantIDs)

	// Diagnostic: log whether B and C landed in the same millisecond (the
	// sub-ms pair case is the intended guard path).
	bEntry := entries[0] // after sort, positions vary
	cEntry := entries[0]
	for _, e := range entries {
		if e.id == bID.String() {
			bEntry = e
		}
		if e.id == cID.String() {
			cEntry = e
		}
	}
	if bEntry.ms == cEntry.ms {
		t.Logf("sub-ms pair guard active: B and C share ms=%d, resolved by entity_id (%s, %s)",
			bEntry.ms, bEntry.id, cEntry.id)
	}
}

// RunSearchSortMetaState seeds two entities in CREATED state, promotes one to
// APPROVED via the manual approve transition, and asserts that sort=@state:asc
// places the APPROVED entity before the CREATED one (lexicographic order).
func RunSearchSortMetaState(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-sort-state"
	const modelVersion = 1
	// searchWorkflowJSON includes the manual "approve" transition (CREATED→APPROVED).
	setupSearchModel(t, c, modelName, modelVersion)

	aliceID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Alice","amount":100,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntity Alice: %v", err)
	}
	bobID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Bob","amount":50,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntity Bob: %v", err)
	}

	// Promote Alice to APPROVED via the manual transition.
	if err := c.UpdateEntity(t, aliceID, "approve", `{"name":"Alice","amount":100,"status":"approved"}`); err != nil {
		t.Fatalf("UpdateEntity (approve Alice): %v", err)
	}

	// APPROVED < CREATED lexicographically → Alice first.
	results, err := c.SyncSearchSorted(t, modelName, modelVersion, sortMatchAll, []string{"@state:asc"})
	if err != nil {
		t.Fatalf("SyncSearchSorted: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("want 2 results, got %d", len(results))
	}
	if results[0].Meta.ID != aliceID.String() {
		t.Errorf("result[0] id=%q, want aliceID=%q (APPROVED before CREATED)", results[0].Meta.ID, aliceID.String())
	}
	if results[0].Meta.State != "APPROVED" {
		t.Errorf("result[0] state=%q, want APPROVED", results[0].Meta.State)
	}
	if results[1].Meta.ID != bobID.String() {
		t.Errorf("result[1] id=%q, want bobID=%q (CREATED)", results[1].Meta.ID, bobID.String())
	}
	if results[1].Meta.State != "CREATED" {
		t.Errorf("result[1] state=%q, want CREATED", results[1].Meta.State)
	}
}

// RunSearchSortMultiKeyTiebreaker creates two entities with an identical
// primary sort-key value (amount=50) and asserts they are resolved by the
// entity_id ascending tiebreaker — the canonical rule that SQL and in-memory
// backends must both apply when rows are equal under all explicit keys.
func RunSearchSortMultiKeyTiebreaker(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-sort-tiebreak"
	const modelVersion = 1
	setupSortModel(t, c, modelName, modelVersion)

	e1ID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"entity-one","amount":50,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntity E1: %v", err)
	}
	e2ID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"entity-two","amount":50,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntity E2: %v", err)
	}

	results, err := c.SyncSearchSorted(t, modelName, modelVersion, sortMatchAll, []string{"amount:asc"})
	if err != nil {
		t.Fatalf("SyncSearchSorted: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("want 2 results, got %d", len(results))
	}

	// Entity_id ascending tiebreaker: lexicographically smaller UUID first.
	wantFirst, wantSecond := e1ID.String(), e2ID.String()
	if wantFirst > wantSecond {
		wantFirst, wantSecond = wantSecond, wantFirst
	}
	if results[0].Meta.ID != wantFirst {
		t.Errorf("result[0] id=%q, want %q (entity_id tiebreaker, lexicographically first)", results[0].Meta.ID, wantFirst)
	}
	if results[1].Meta.ID != wantSecond {
		t.Errorf("result[1] id=%q, want %q (entity_id tiebreaker, lexicographically second)", results[1].Meta.ID, wantSecond)
	}
}

// RunSearchSortNullsLast seeds three entities: A (score=5), B (no score field),
// and C (score=10).  Asserts that for both ascending and descending sort on
// score, the entity with a missing score field sorts LAST regardless of
// direction — the canonical nulls-last behaviour.
func RunSearchSortNullsLast(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-sort-nulls"
	const modelVersion = 1
	// Schema must include score so it is a known, typed sortable field.
	setupSortModelWithSample(t, c, modelName, modelVersion,
		`{"name":"Test","amount":10,"status":"new","score":1}`)

	aID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"a","amount":1,"status":"new","score":5}`)
	if err != nil {
		t.Fatalf("CreateEntity A (score=5): %v", err)
	}
	bID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"b","amount":2,"status":"new"}`) // no score
	if err != nil {
		t.Fatalf("CreateEntity B (no score): %v", err)
	}
	cID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"c","amount":3,"status":"new","score":10}`)
	if err != nil {
		t.Fatalf("CreateEntity C (score=10): %v", err)
	}

	// Ascending: 5 < 10 < null-last → A, C, B.
	asc, err := c.SyncSearchSorted(t, modelName, modelVersion, sortMatchAll, []string{"score:asc"})
	if err != nil {
		t.Fatalf("SyncSearchSorted asc: %v", err)
	}
	assertSortedIDs(t, "score:asc", asc, []string{aID.String(), cID.String(), bID.String()})

	// Descending: 10 > 5 > null-last → C, A, B.
	desc, err := c.SyncSearchSorted(t, modelName, modelVersion, sortMatchAll, []string{"score:desc"})
	if err != nil {
		t.Fatalf("SyncSearchSorted desc: %v", err)
	}
	assertSortedIDs(t, "score:desc", desc, []string{cID.String(), aID.String(), bID.String()})
}

// RunSearchSortPointInTime creates two entities (A="Zulu", B="Alpha"), records
// a snapshot timestamp, updates A's name to "Bravo", then verifies that a
// sorted search at the snapshot sees the old names in sort order while the
// current sorted search sees the updated name.  Guards sort + PIT interaction
// across all backends.
func RunSearchSortPointInTime(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-sort-pit"
	const modelVersion = 1
	setupSortModel(t, c, modelName, modelVersion)

	aID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Zulu","amount":1,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntity A: %v", err)
	}
	bID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Alpha","amount":2,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntity B: %v", err)
	}

	// Capture t1 at B's creation so both entities exist in the snapshot.
	t1 := pitbLatestChangeTime(t, c, bID)

	// Space ≥1 ms so the update lands in a strictly later millisecond.
	time.Sleep(2 * time.Millisecond)

	// Update A's name; at t1 it was still "Zulu".
	if err := c.UpdateEntityData(t, aID, `{"name":"Bravo","amount":1,"status":"new"}`); err != nil {
		t.Fatalf("UpdateEntityData A: %v", err)
	}

	// At snapshot t1: sort by name:asc → [Alpha(B), Zulu(A)].
	atT1, err := c.SyncSearchSortedAt(t, modelName, modelVersion, sortMatchAll, []string{"name:asc"}, t1)
	if err != nil {
		t.Fatalf("SyncSearchSortedAt(t1): %v", err)
	}
	assertSortedIDs(t, "@t1 name:asc", atT1, []string{bID.String(), aID.String()})
	if len(atT1) == 2 {
		if got := atT1[1].Data["name"]; got != "Zulu" {
			t.Errorf("@t1 result[1] name=%q, want Zulu (snapshot must predate update)", got)
		}
	}

	// Current view: sort by name:asc → [Alpha(B), Bravo(A)].
	nowResults, err := c.SyncSearchSorted(t, modelName, modelVersion, sortMatchAll, []string{"name:asc"})
	if err != nil {
		t.Fatalf("SyncSearchSorted(now): %v", err)
	}
	assertSortedIDs(t, "now name:asc", nowResults, []string{bID.String(), aID.String()})
	if len(nowResults) == 2 {
		if got := nowResults[1].Data["name"]; got != "Bravo" {
			t.Errorf("now result[1] name=%q, want Bravo (update must be visible)", got)
		}
	}
}

// RunSearchSortDataFieldNamedMeta seeds entities with a top-level data field
// literally named "meta" (e.g. {"meta":{"label":{"x":"gamma"}}}) and sorts by
// the data path "meta.label.x:asc" (no @ prefix).  Asserts the sort resolves
// correctly as a DATA field — the @ prefix is the only discriminator between
// system meta and a data field named "meta".
func RunSearchSortDataFieldNamedMeta(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-sort-metafield"
	const modelVersion = 1
	// Schema must include the nested data field "meta.label.x".
	setupSortModelWithSample(t, c, modelName, modelVersion,
		`{"name":"Test","amount":10,"status":"new","meta":{"label":{"x":"default"}}}`)

	// Create in reverse sort order to prove sorting is applied.
	e1ID, err := c.CreateEntity(t, modelName, modelVersion,
		`{"name":"e1","amount":1,"status":"new","meta":{"label":{"x":"gamma"}}}`)
	if err != nil {
		t.Fatalf("CreateEntity E1: %v", err)
	}
	e2ID, err := c.CreateEntity(t, modelName, modelVersion,
		`{"name":"e2","amount":2,"status":"new","meta":{"label":{"x":"alpha"}}}`)
	if err != nil {
		t.Fatalf("CreateEntity E2: %v", err)
	}
	e3ID, err := c.CreateEntity(t, modelName, modelVersion,
		`{"name":"e3","amount":3,"status":"new","meta":{"label":{"x":"beta"}}}`)
	if err != nil {
		t.Fatalf("CreateEntity E3: %v", err)
	}

	// Sort by data path meta.label.x ascending: alpha < beta < gamma → E2, E3, E1.
	results, err := c.SyncSearchSorted(t, modelName, modelVersion, sortMatchAll, []string{"meta.label.x:asc"})
	if err != nil {
		t.Fatalf("SyncSearchSorted: %v", err)
	}
	assertSortedIDs(t, "meta.label.x:asc", results, []string{
		e2ID.String(), e3ID.String(), e1ID.String(),
	})
}

// RunSearchNoSortDefaultOrder creates three entities and asserts that a search
// with NO sort parameter returns them in entity_id ascending order on every
// backend.  This is the MAJOR-1 regression guard: the memory backend's GetAll
// returns Go map-iteration order (non-deterministic) unless sortEntities is
// called unconditionally with the entity_id tiebreaker even when no specs are
// supplied.  SQL backends default to ORDER BY entity_id naturally, so the
// guard specifically pins that memory matches SQL without an explicit sort param.
func RunSearchNoSortDefaultOrder(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-sort-nosort"
	const modelVersion = 1
	setupSortModel(t, c, modelName, modelVersion)

	// Insert in non-UUID-lexicographic order to maximise the chance of
	// catching a non-deterministic backend.
	id1, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"C","amount":3,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntity 1: %v", err)
	}
	id2, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"A","amount":1,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntity 2: %v", err)
	}
	id3, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"B","amount":2,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntity 3: %v", err)
	}

	// No sort keys — must return entity_id ascending on every backend.
	results, err := c.SyncSearch(t, modelName, modelVersion, sortMatchAll)
	if err != nil {
		t.Fatalf("SyncSearch: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("want 3 results, got %d", len(results))
	}

	// Compute expected order: lexicographic sort on the UUID strings.
	wantIDs := []string{id1.String(), id2.String(), id3.String()}
	sort.Strings(wantIDs)

	assertSortedIDs(t, "no-sort default", results, wantIDs)
}
