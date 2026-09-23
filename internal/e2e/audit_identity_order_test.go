package e2e_test

import (
	"bytes"
	"cmp"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// audit_identity_order_test.go — E2E (Postgres, full HTTP stack) coverage for
// the audit event identity/order/cursor contract:
//
//   - EntityChange events carry `version`; StateMachine events carry a
//     store-assigned `eventId` (a v1 UUID).
//   - The merged trail orders newest first: utcTime DESC, then EntityChange
//     before StateMachine at a shared instant, then version DESC
//     (EntityChange) or the eventId's time field / bytes DESC (StateMachine).
//   - Pages are opaque position cursors; any cursor the server did not
//     produce is rejected with 400 BAD_REQUEST.
//
// internal/domain/audit/handler_test.go proves the same contract against an
// in-process httptest server over the memory backend; these tests prove it
// through the real HTTP+gRPC stack against Postgres.

// setupAuditIdentitySecondary spins up a callback harness whose primary
// entity's SYNC processor creates a secondary entity and then updates it via
// a loopback PUT — both calls echoing the same tx-token, so both joins land
// in the primary transition's transaction T and commit at T's single commit
// instant. Returns the harness and the secondary's id once the primary
// transition has committed and recorded the secondary id in its own data.
func setupAuditIdentitySecondary(t *testing.T, procName, primaryName, secondaryName string) (*callbackHarness, string) {
	t.Helper()
	h := newCallbackHarness(t)
	h.SetupModelWithWorkflow(t, secondaryName, secondaryWorkflow)

	secondaryIDCh := make(chan string, 1)
	h.RegisterProc(procName, func(rc *reqCtx) (map[string]any, error) {
		created, err := rc.CreateEntity(secondaryName, 1, `{"name":"child","amount":1,"status":"new"}`)
		if err != nil {
			return nil, fmt.Errorf("callback create failed: %w", err)
		}
		if created.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("callback create status=%d body=%s", created.StatusCode, created.Body)
		}
		updated, err := rc.UpdateEntity(created.EntityID, `{"name":"child","amount":2,"status":"updated"}`)
		if err != nil {
			return nil, fmt.Errorf("callback update failed: %w", err)
		}
		if updated.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("callback update status=%d body=%s", updated.StatusCode, updated.Body)
		}
		secondaryIDCh <- created.EntityID
		out := cloneData(rc.entityData)
		out["secondaryId"] = created.EntityID
		return out, nil
	})

	primaryWF := fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": %q, "initialState": "NONE", "active": true,
			"states": {
				"NONE":   {"transitions": [{"name": "init", "next": "ACTIVE", "manual": false,
					"processors": [{"type": "calculator", "name": %q, "executionMode": "SYNC",
						"config": {"attachEntity": true, "calculationNodesTags": ""}}]
				}]},
				"ACTIVE": {}
			}
		}]
	}`, primaryName+"-wf", procName)
	// The processor writes `secondaryId`; the model must declare it.
	h.setupModelSampleWithWorkflow(t, primaryName, workflowSampleWith(`"secondaryId": ""`), primaryWF)

	primaryID, status, body := h.CreateEntity(t, primaryName, 1, `{"name":"parent","amount":100,"status":"new"}`)
	if status != http.StatusOK {
		t.Fatalf("primary create: status=%d body=%s", status, body)
	}
	var secondaryID string
	select {
	case secondaryID = <-secondaryIDCh:
	case <-time.After(10 * time.Second):
		t.Fatal("timeout: callback did not run / did not create+update the secondary entity")
	}
	if got := h.GetEntityData(t, primaryID)["secondaryId"]; got != secondaryID {
		t.Fatalf("primary.secondaryId = %v; want %q", got, secondaryID)
	}
	return h, secondaryID
}

// auditPageResult is the decoded shape of GET /audit/entity/{id}: items plus
// the pagination envelope (hasNext, nextCursor).
type auditPageResult struct {
	Items      []map[string]any `json:"items"`
	Pagination map[string]any   `json:"pagination"`
}

// getAuditPage fetches one page of a callback harness's own stack, with a
// caller-controlled raw query string (so callers can set limit/cursor/
// eventType directly) via h.DoAuth.
func getAuditPage(t *testing.T, h *callbackHarness, entityID, query string) ([]map[string]any, map[string]any) {
	t.Helper()
	path := fmt.Sprintf("/api/audit/entity/%s", entityID)
	if query != "" {
		path += "?" + query
	}
	resp := h.DoAuth(t, http.MethodGet, path, "", "")
	body := h.readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("audit GET %s: expected 200, got %d: %s", entityID, resp.StatusCode, body)
	}
	var out auditPageResult
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("audit GET %s: decode: %v; body=%s", entityID, err, body)
	}
	return out.Items, out.Pagination
}

// auditPage is getAuditPage against the shared package-level stack (doAuth),
// for tests that don't need a callback harness.
func auditPage(t *testing.T, entityID, query string) ([]map[string]any, map[string]any) {
	t.Helper()
	path := fmt.Sprintf("/api/audit/entity/%s", entityID)
	if query != "" {
		path += "?" + query
	}
	resp := doAuth(t, http.MethodGet, path, "")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("audit GET %s: expected 200, got %d: %s", entityID, resp.StatusCode, body)
	}
	var out auditPageResult
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("audit GET %s: decode: %v; body=%s", entityID, err, body)
	}
	return out.Items, out.Pagination
}

// wireKey is the sort key of one wire-encoded audit event, extracted from
// its JSON fields — the E2E-side reimplementation of
// internal/domain/audit's eventKey, built from the response the real server
// sent rather than from any internal type.
type wireKey struct {
	at      time.Time
	kind    string
	version int64
	eventID uuid.UUID
}

// eventWireKey decodes ev's sort key from its emitted fields (auditEventType,
// utcTime, version | eventId), failing the test if a field required by the
// event's own kind is missing or malformed.
func eventWireKey(t *testing.T, ev map[string]any) wireKey {
	t.Helper()
	kind, _ := ev["auditEventType"].(string)
	utc, _ := ev["utcTime"].(string)
	at, err := time.Parse(time.RFC3339Nano, utc)
	if err != nil {
		t.Fatalf("event has unparseable utcTime %q: %v (event=%v)", utc, err, ev)
	}
	k := wireKey{at: at, kind: kind}
	switch kind {
	case "EntityChange":
		v, ok := ev["version"].(float64)
		if !ok {
			t.Fatalf("EntityChange event missing numeric version: %v", ev)
		}
		k.version = int64(v)
	case "StateMachine":
		idStr, _ := ev["eventId"].(string)
		if idStr == "" {
			t.Fatalf("StateMachine event missing eventId: %v", ev)
		}
		id, err := uuid.Parse(idStr)
		if err != nil {
			t.Fatalf("StateMachine event has unparseable eventId %q: %v", idStr, err)
		}
		k.eventID = id
	default:
		t.Fatalf("unexpected auditEventType %q (event=%v)", kind, ev)
	}
	return k
}

// compareWireKeys mirrors internal/domain/audit.compareKeys: newest first —
// utcTime DESC, then EntityChange before StateMachine at a shared instant,
// then version DESC (EntityChange) or the eventId's time field / bytes DESC
// (StateMachine). Negative means a sorts before b.
func compareWireKeys(a, b wireKey) int {
	if c := b.at.Compare(a.at); c != 0 {
		return c
	}
	if a.kind != b.kind {
		return strings.Compare(a.kind, b.kind)
	}
	if a.kind == "EntityChange" {
		return cmp.Compare(b.version, a.version)
	}
	if c := cmp.Compare(b.eventID.Time(), a.eventID.Time()); c != 0 {
		return c
	}
	return bytes.Compare(b.eventID[:], a.eventID[:])
}

// assertStrictOrder asserts that every adjacent pair of events in the given
// (already server-ordered) slice is strictly ordered by compareWireKeys —
// i.e. the wire order matches the documented total order exactly, with no
// ties left unresolved.
func assertStrictOrder(t *testing.T, events []map[string]any) {
	t.Helper()
	for i := 0; i+1 < len(events); i++ {
		ka := eventWireKey(t, events[i])
		kb := eventWireKey(t, events[i+1])
		if c := compareWireKeys(ka, kb); c >= 0 {
			t.Fatalf("events[%d] and events[%d] are not strictly ordered (cmp=%d):\n[%d]=%v\n[%d]=%v",
				i, i+1, c, i, events[i], i+1, events[i+1])
		}
	}
}

// TestAuditE2E_TwoJoinedSavesHaveDistinctVersions proves identity and order
// for two saves joined into the SAME transaction: a secondary entity created
// then updated (loopback) inside the primary transition's transaction T.
// Both EntityChange events share T's transactionId and commit instant, so
// only `version` DESC distinguishes them (2 then 1); every StateMachine
// event of that same transaction sorts after both, each carrying a distinct
// store-assigned eventId.
func TestAuditE2E_TwoJoinedSavesHaveDistinctVersions(t *testing.T) {
	h, secondaryID := setupAuditIdentitySecondary(t, "cb-audit-create-update", "audit-e2e-primary-1", "audit-e2e-secondary-1")

	events, _ := getAuditPage(t, h, secondaryID, "limit=1000")
	if len(events) < 3 {
		t.Fatalf("expected at least 3 audit events (2 EntityChange + >=1 StateMachine), got %d: %v", len(events), events)
	}

	assertStrictOrder(t, events)

	var ecEvents, smEvents []map[string]any
	for _, ev := range events {
		switch ev["auditEventType"] {
		case "EntityChange":
			ecEvents = append(ecEvents, ev)
		case "StateMachine":
			smEvents = append(smEvents, ev)
		default:
			t.Fatalf("unexpected auditEventType %v in %v", ev["auditEventType"], ev)
		}
	}
	if len(ecEvents) != 2 {
		t.Fatalf("expected exactly 2 EntityChange events (create + update), got %d: %v", len(ecEvents), ecEvents)
	}
	if len(smEvents) == 0 {
		t.Fatal("expected at least 1 StateMachine event")
	}

	// Both EntityChange events belong to T: same transactionId, same commit
	// instant (utcTime) — the two saves were joined into one transaction.
	txID, _ := ecEvents[0]["transactionId"].(string)
	if txID == "" {
		t.Fatal("expected non-empty transactionId on the first EntityChange event")
	}
	if got := ecEvents[1]["transactionId"]; got != txID {
		t.Fatalf("EntityChange events do not share a transactionId: %v vs %v", txID, got)
	}
	if ecEvents[0]["utcTime"] != ecEvents[1]["utcTime"] {
		t.Fatalf("EntityChange events do not share a commit instant: %v vs %v", ecEvents[0]["utcTime"], ecEvents[1]["utcTime"])
	}

	// Newest first: UPDATE (version 2) before CREATE (version 1).
	if ecEvents[0]["version"] != float64(2) || ecEvents[1]["version"] != float64(1) {
		t.Fatalf("expected versions [2,1], got [%v,%v]", ecEvents[0]["version"], ecEvents[1]["version"])
	}
	if ecEvents[0]["changeType"] != "UPDATE" || ecEvents[1]["changeType"] != "CREATE" {
		t.Fatalf("expected changeTypes [UPDATE,CREATE], got [%v,%v]", ecEvents[0]["changeType"], ecEvents[1]["changeType"])
	}

	// The two EntityChange events come first; every event after them is
	// StateMachine (of the same transaction T).
	if events[0]["auditEventType"] != "EntityChange" || events[1]["auditEventType"] != "EntityChange" {
		t.Fatalf("expected the two EntityChange events first, got %v then %v", events[0]["auditEventType"], events[1]["auditEventType"])
	}
	for i := 2; i < len(events); i++ {
		if events[i]["auditEventType"] != "StateMachine" {
			t.Fatalf("position %d: expected StateMachine, got %v", i, events[i]["auditEventType"])
		}
		if tx := events[i]["transactionId"]; tx != txID {
			t.Fatalf("position %d: StateMachine transactionId %v != T's transactionId %v", i, tx, txID)
		}
	}

	// Every StateMachine eventId is a distinct UUID.
	seen := map[string]bool{}
	for _, ev := range smEvents {
		id, _ := ev["eventId"].(string)
		if id == "" {
			t.Fatalf("StateMachine event missing eventId: %v", ev)
		}
		if seen[id] {
			t.Fatalf("duplicate StateMachine eventId %s", id)
		}
		seen[id] = true
	}

	// A second read returns the identical eventIds in the identical order —
	// eventId is a stored identity, not recomputed per request.
	events2, _ := getAuditPage(t, h, secondaryID, "limit=1000")
	if len(events2) != len(events) {
		t.Fatalf("second read returned %d events, want %d", len(events2), len(events))
	}
	for i := range events {
		if events2[i]["eventId"] != events[i]["eventId"] {
			t.Fatalf("position %d: eventId changed across reads: first=%v second=%v", i, events[i]["eventId"], events2[i]["eventId"])
		}
	}
}

// TestAuditE2E_WalkOverOneInstantTie walks the same kind of secondary
// (created then updated, joined into one transaction, so its whole trail
// shares one commit instant) one event at a time via limit=1 + nextCursor.
// The concatenation of the walk must equal the unpaged list, in order — no
// page boundary may repeat or skip an event tied on time.
func TestAuditE2E_WalkOverOneInstantTie(t *testing.T) {
	h, secondaryID := setupAuditIdentitySecondary(t, "cb-audit-create-update-walk", "audit-e2e-primary-2", "audit-e2e-secondary-2")

	all, _ := getAuditPage(t, h, secondaryID, "limit=1000")
	if len(all) < 3 {
		t.Fatalf("expected several audit events sharing the commit instant, got %d", len(all))
	}
	assertStrictOrder(t, all)

	var walked []map[string]any
	cursor := ""
	for i := 0; i <= len(all)+1; i++ {
		query := "limit=1"
		if cursor != "" {
			query += "&cursor=" + url.QueryEscape(cursor)
		}
		page, pagination := getAuditPage(t, h, secondaryID, query)
		if len(page) != 1 {
			t.Fatalf("expected 1 event per page, got %d", len(page))
		}
		walked = append(walked, page...)
		hasNext, _ := pagination["hasNext"].(bool)
		if !hasNext {
			break
		}
		cursor, _ = pagination["nextCursor"].(string)
		if cursor == "" {
			t.Fatal("hasNext=true but nextCursor missing")
		}
		if i == len(all)+1 {
			t.Fatal("walk did not terminate within the expected number of pages")
		}
	}

	if len(walked) != len(all) {
		t.Fatalf("walk returned %d events, want %d", len(walked), len(all))
	}
	assertStrictOrder(t, walked)
	for i := range all {
		if walked[i]["auditEventType"] != all[i]["auditEventType"] ||
			fmt.Sprintf("%v", walked[i]["version"]) != fmt.Sprintf("%v", all[i]["version"]) ||
			fmt.Sprintf("%v", walked[i]["eventId"]) != fmt.Sprintf("%v", all[i]["eventId"]) {
			t.Fatalf("position %d mismatch: got %v, want %v", i, walked[i], all[i])
		}
	}
}

// TestAuditE2E_FinishedEventIDMatchesSearch proves that the finished-event
// door and the search door report the SAME identity for a workflow's
// STATE_MACHINE_FINISH event: the `eventId` returned by
// GET /audit/entity/{id}/workflow/{txId}/finished must equal the `eventId`
// of that entity's STATE_MACHINE_FINISH event found via
// GET /audit/entity/{id}?eventType=StateMachine.
func TestAuditE2E_FinishedEventIDMatchesSearch(t *testing.T) {
	const model = "audit-e2e-finished-3"
	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "audit-e2e-finished-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE":    {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {"transitions": [{"name": "finish", "next": "DONE", "manual": false}]},
				"DONE":    {}
			}
		}]
	}`
	setupModelWithWorkflow(t, model, wf)

	entityID, txID := createEntityE2EWithTxID(t, model, 1, `{"name":"Test","amount":10,"status":"new"}`)

	path := fmt.Sprintf("/api/audit/entity/%s/workflow/%s/finished", entityID, txID)
	resp := doAuth(t, http.MethodGet, path, "")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("finished endpoint: expected 200, got %d: %s", resp.StatusCode, body)
	}
	var finished map[string]any
	if err := json.Unmarshal([]byte(body), &finished); err != nil {
		t.Fatalf("decode finished response: %v; body=%s", err, body)
	}
	finishedEventID, _ := finished["eventId"].(string)
	if finishedEventID == "" {
		t.Fatalf("finished response missing eventId: %v", finished)
	}

	events := getSMAuditEvents(t, entityID)
	var searchEventID string
	for _, ev := range events {
		if ev["eventType"] == "STATE_MACHINE_FINISH" {
			searchEventID, _ = ev["eventId"].(string)
			break
		}
	}
	if searchEventID == "" {
		t.Fatalf("no STATE_MACHINE_FINISH event with an eventId found via search: %v", events)
	}
	if searchEventID != finishedEventID {
		t.Fatalf("finished eventId %q != search STATE_MACHINE_FINISH eventId %q", finishedEventID, searchEventID)
	}
}

// TestAuditE2E_NewEventsMidWalk is the E2E counterpart of
// internal/domain/audit's TestCursor_NewEventsMidWalk: a new transaction
// committing mid-walk adds events at the newest end; the walk must neither
// repeat nor skip the events that existed when it began.
func TestAuditE2E_NewEventsMidWalk(t *testing.T) {
	const model = "audit-e2e-midwalk-4"
	importModelSampleE2E(t, model, 1, `{"name":"A"}`)
	lockModelE2E(t, model, 1)

	id := createEntityE2E(t, model, 1, `{"name":"v1"}`)
	for _, n := range []string{"v2", "v3", "v4"} {
		updateEntityE2E(t, id, "UPDATE", `{"name":"`+n+`"}`)
	}
	all, _ := auditPage(t, id, "eventType=EntityChange")
	page1, p1 := auditPage(t, id, "eventType=EntityChange&limit=2")

	// A new transaction commits mid-walk, after page 1 was fetched.
	updateEntityE2E(t, id, "UPDATE", `{"name":"v5"}`)

	nextCursor, _ := p1["nextCursor"].(string)
	if nextCursor == "" {
		t.Fatalf("expected non-empty nextCursor on page 1, got %v", p1["nextCursor"])
	}
	page2, _ := auditPage(t, id, "eventType=EntityChange&limit=10&cursor="+url.QueryEscape(nextCursor))

	walked := append(page1, page2...)
	if len(walked) != len(all) {
		t.Fatalf("walk returned %d events, want the %d that existed at the start", len(walked), len(all))
	}
	for i := range all {
		if walked[i]["version"] != all[i]["version"] {
			t.Fatalf("position %d: version %v, want %v", i, walked[i]["version"], all[i]["version"])
		}
	}
}

// TestAuditE2E_Cursor400 proves every listed malformed-cursor shape is
// rejected with 400 BAD_REQUEST, both against an entity that exists and
// against one that does not — the cursor is validated before any store
// lookup runs, so a nonexistent entity id never turns the answer into 404.
func TestAuditE2E_Cursor400(t *testing.T) {
	const model = "audit-e2e-cursor400-5"
	importModelSampleE2E(t, model, 1, `{"name":"A"}`)
	lockModelE2E(t, model, 1)
	id := createEntityE2E(t, model, 1, `{"name":"v1"}`)

	b64 := func(jsonBody string) string {
		return base64.RawURLEncoding.EncodeToString([]byte(jsonBody))
	}

	cases := []struct {
		name   string
		cursor string
	}{
		{"legacy-offset", "20"},
		{"garbage", "!!!"},
		{"unknown-kind", b64(`{"v":1,"t":"2026-09-23T10:00:00Z","k":"System","n":1}`)},
		{"statemachine-bad-uuid", b64(`{"v":1,"t":"2026-09-23T10:00:00Z","k":"StateMachine","e":"x"}`)},
		{"entitychange-zero-version", b64(`{"v":1,"t":"2026-09-23T10:00:00Z","k":"EntityChange","n":0}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := fmt.Sprintf("/api/audit/entity/%s?cursor=%s", id, url.QueryEscape(tc.cursor))
			resp := doAuth(t, http.MethodGet, path, "")
			assertProblemJSON(t, resp, http.StatusBadRequest, "BAD_REQUEST")
		})
	}

	t.Run("nonexistent-entity", func(t *testing.T) {
		nonexistentID := uuid.New().String()
		path := fmt.Sprintf("/api/audit/entity/%s?cursor=%s", nonexistentID, url.QueryEscape("20"))
		resp := doAuth(t, http.MethodGet, path, "")
		assertProblemJSON(t, resp, http.StatusBadRequest, "BAD_REQUEST")
	})
}
