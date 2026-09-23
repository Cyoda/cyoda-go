package audit_test

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/common/commontest"
	"github.com/cyoda-platform/cyoda-go/internal/domain/audit"
)

// TestAudit_EntityChangeCarriesVersion pins that EntityChange audit events
// carry the store's version number on the wire, newest first.
func TestAudit_EntityChangeCarriesVersion(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "AuditVer", 1, `{"name":"Alice"}`)
	id := createEntityAndGetID(t, srv.URL, "AuditVer", 1, `{"name":"Bob"}`)
	updateEntity(t, srv.URL, id, `{"name":"Carol"}`)
	events, _ := getAuditEvents(t, srv.URL, id, "eventType=EntityChange")
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
	if events[0]["version"] != float64(2) || events[1]["version"] != float64(1) {
		t.Fatalf("versions = %v, %v; want 2, 1", events[0]["version"], events[1]["version"])
	}
}

// TestAudit_StateMachineEventCarriesEventID pins that StateMachine audit
// events carry a store-assigned v1 UUID eventId, stable across repeated
// reads of the same event.
func TestAudit_StateMachineEventCarriesEventID(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "AuditSMID", 1, `{"name":"Alice","age":30}`)

	wfBody := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1",
			"name": "audit-sm-id-flow",
			"initialState": "INITIAL",
			"active": true,
			"states": {
				"INITIAL": {
					"transitions": [{
						"name": "auto-validate",
						"next": "STABLE",
						"manual": false
					}]
				},
				"STABLE": {}
			}
		}]
	}`
	importWorkflow(t, srv.URL, "AuditSMID", 1, wfBody)

	id := createEntityAndGetID(t, srv.URL, "AuditSMID", 1, `{"name":"Bob","age":25}`)

	first, _ := getAuditEvents(t, srv.URL, id, "eventType=StateMachine")
	second, _ := getAuditEvents(t, srv.URL, id, "eventType=StateMachine")
	if len(first) == 0 {
		t.Fatal("expected at least one StateMachine event")
	}
	if len(first) != len(second) {
		t.Fatalf("event count changed between reads: %d vs %d", len(first), len(second))
	}
	seen := map[string]bool{}
	for i, ev := range first {
		s, _ := ev["eventId"].(string)
		id, err := uuid.Parse(s)
		if err != nil {
			t.Fatalf("event %d eventId %q is not a UUID", i, s)
		}
		if id.Version() != 1 {
			t.Errorf("event %d eventId %q is version %d, want a store-assigned v1 UUID", i, s, id.Version())
		}
		if seen[s] {
			t.Fatalf("eventId %q repeated", s)
		}
		seen[s] = true
		if second[i]["eventId"] != s {
			t.Fatalf("event %d eventId changed between reads", i)
		}
	}
}

// TestGetStateMachineFinishedEvent_CarriesEventID pins that the finished-event
// endpoint's eventId matches the STATE_MACHINE_FINISH event's eventId as seen
// through the general search for the same transaction.
func TestGetStateMachineFinishedEvent_CarriesEventID(t *testing.T) {
	srv := newTestServerNoContextPath(t)
	importAndLockModel(t, srv.URL, "SMFinishID", 1, `{"name":"Alice","age":30}`)

	wfBody := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1",
			"name": "finish-id-flow",
			"initialState": "INITIAL",
			"active": true,
			"states": {
				"INITIAL": {
					"transitions": [{
						"name": "auto-finish",
						"next": "FINAL",
						"manual": false
					}]
				},
				"FINAL": {}
			}
		}]
	}`
	importWorkflow(t, srv.URL, "SMFinishID", 1, wfBody)

	entityID := createEntityAndGetID(t, srv.URL, "SMFinishID", 1, `{"name":"Bob","age":25}`)
	smTxID := getSmTransactionID(t, srv.URL, entityID)

	events, _ := getAuditEvents(t, srv.URL, entityID, "eventType=StateMachine", "transactionId="+smTxID)
	var wantEventID string
	for _, ev := range events {
		if ev["eventType"] == "STATE_MACHINE_FINISH" {
			wantEventID, _ = ev["eventId"].(string)
		}
	}
	if wantEventID == "" {
		t.Fatal("expected a STATE_MACHINE_FINISH event with a non-empty eventId in the search results")
	}

	url := srv.URL + "/audit/entity/" + entityID + "/workflow/" + smTxID + "/finished"
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if result["eventId"] != wantEventID {
		t.Errorf("finished event eventId = %v, want %v", result["eventId"], wantEventID)
	}
}

// TestAudit_TotalOrder pins that the merged audit event list follows one
// deterministic order: newest instant first, EntityChange before
// StateMachine at the same instant, then version (EntityChange) or the
// eventId's time field and bytes (StateMachine), each descending.
// compareKeys itself is unexported, so this re-implements the rule from the
// emitted wire fields (utcTime, auditEventType, version, eventId) and
// asserts it holds for every adjacent pair, and that two reads return
// identical order. An entity created under an auto-transition workflow
// commits its CREATED EntityChange and every StateMachine event of that
// transaction with the same commit timestamp on the memory backend, giving
// an instant shared by 1 EntityChange and >=2 StateMachine events — the
// exact case the tie-break rules exist for. The test asserts that tie
// exists so it is not vacuous.
func TestAudit_TotalOrder(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "AuditOrder", 1, `{"name":"Alice","age":30}`)

	wfBody := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1",
			"name": "audit-order-flow",
			"initialState": "INITIAL",
			"active": true,
			"states": {
				"INITIAL": {
					"transitions": [{
						"name": "auto-validate",
						"next": "STABLE",
						"manual": false
					}]
				},
				"STABLE": {}
			}
		}]
	}`
	importWorkflow(t, srv.URL, "AuditOrder", 1, wfBody)

	entityID := createEntityAndGetID(t, srv.URL, "AuditOrder", 1, `{"name":"Bob","age":25}`)

	first, _ := getAuditEvents(t, srv.URL, entityID)
	second, _ := getAuditEvents(t, srv.URL, entityID)

	if len(first) < 3 {
		t.Fatalf("expected at least 3 audit events (1 EntityChange + >=2 StateMachine), got %d", len(first))
	}
	if len(first) != len(second) {
		t.Fatalf("event count changed between reads: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i]["auditEventType"] != second[i]["auditEventType"] ||
			first[i]["utcTime"] != second[i]["utcTime"] ||
			first[i]["eventId"] != second[i]["eventId"] ||
			first[i]["version"] != second[i]["version"] {
			t.Fatalf("item %d differs between reads: %v vs %v", i, first[i], second[i])
		}
	}

	type orderFields struct {
		at      time.Time
		kind    string
		version int64
		eventID uuid.UUID
	}
	fieldsOf := func(ev map[string]any) orderFields {
		t.Helper()
		utcStr, _ := ev["utcTime"].(string)
		at, err := time.Parse(time.RFC3339Nano, utcStr)
		if err != nil {
			t.Fatalf("event utcTime %q does not parse: %v", utcStr, err)
		}
		kind, _ := ev["auditEventType"].(string)
		f := orderFields{at: at, kind: kind}
		if v, ok := ev["version"]; ok {
			f.version = int64(v.(float64))
		}
		if s, ok := ev["eventId"].(string); ok && s != "" {
			id, err := uuid.Parse(s)
			if err != nil {
				t.Fatalf("event eventId %q does not parse: %v", s, err)
			}
			f.eventID = id
		}
		return f
	}

	// compare re-implements compareKeys from the emitted wire fields:
	// negative when a sorts before b (a is newer / first), 0 only for
	// identical fields.
	compare := func(a, b orderFields) int {
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

	prev := fieldsOf(first[0])
	for i := 1; i < len(first); i++ {
		cur := fieldsOf(first[i])
		if c := compare(prev, cur); c >= 0 {
			t.Fatalf("adjacent pair %d/%d not strictly ordered: compare = %d; prev=%v cur=%v", i-1, i, c, first[i-1], first[i])
		}
		prev = cur
	}

	tieFound := false
	byInstant := map[string]struct{ entityChange, stateMachine int }{}
	for _, ev := range first {
		utcStr, _ := ev["utcTime"].(string)
		kind, _ := ev["auditEventType"].(string)
		counts := byInstant[utcStr]
		if kind == "EntityChange" {
			counts.entityChange++
		} else {
			counts.stateMachine++
		}
		byInstant[utcStr] = counts
	}
	for _, counts := range byInstant {
		if counts.entityChange >= 1 && counts.stateMachine >= 2 {
			tieFound = true
			break
		}
	}
	if !tieFound {
		t.Fatal("expected an instant shared by an EntityChange event and >=2 StateMachine events; without that tie this test does not exercise the tie-break rules")
	}
}

// --- stubs for TestSearch_SMEventWithoutID_Returns500 ---

var fieldsTestFixedTime = time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)

// stubFieldsEntityStore answers GetVersionMetadata with a single, valid
// EntityChange version.
type stubFieldsEntityStore struct {
	spi.EntityStore
}

func (stubFieldsEntityStore) GetVersionMetadata(context.Context, string, spi.VersionMetadataOptions) ([]spi.EntityVersionMeta, error) {
	return []spi.EntityVersionMeta{
		{Version: 1, ChangeType: "CREATED", Timestamp: fieldsTestFixedTime},
	}, nil
}

// stubFieldsSMAuditStore answers GetEvents with a single event whose
// TimeUUID is empty — a store fault, never emitted with a blank identity.
type stubFieldsSMAuditStore struct {
	spi.StateMachineAuditStore
}

func (stubFieldsSMAuditStore) GetEvents(context.Context, string) ([]spi.StateMachineEvent, error) {
	return []spi.StateMachineEvent{
		{
			EventType: spi.SMEventStarted,
			EntityID:  "some-entity-id",
			TimeUUID:  "",
			Details:   "sensitive internal detail that must not leak",
			Timestamp: fieldsTestFixedTime,
		},
	}, nil
}

// stubFieldsFactory hands back the fixed stub stores above. Every other
// factory accessor is unimplemented — the handler under test reaches none of
// them.
type stubFieldsFactory struct {
	spi.StoreFactory
}

func (stubFieldsFactory) EntityStore(context.Context) (spi.EntityStore, error) {
	return stubFieldsEntityStore{}, nil
}

func (stubFieldsFactory) StateMachineAuditStore(context.Context) (spi.StateMachineAuditStore, error) {
	return stubFieldsSMAuditStore{}, nil
}

// TestSearch_SMEventWithoutID_Returns500 pins that a StateMachine event with
// no store-assigned id fails the request rather than being emitted with a
// blank eventId: an unavailable-by-corruption dependency fails the
// operation instead of downgrading it (correctness over availability).
func TestSearch_SMEventWithoutID_Returns500(t *testing.T) {
	entityID := uuid.New()
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/audit/entity/"+entityID.String(), nil).
		WithContext(pushdownTestCtx())

	audit.New(stubFieldsFactory{}).SearchEntityAuditEvents(w, r, entityID, genapi.SearchEntityAuditEventsParams{})

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body: %s", w.Code, w.Body.String())
	}
	commontest.ExpectErrorCode(t, w.Result(), common.ErrCodeServerError)

	var pd struct {
		Ticket string `json:"ticket"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &pd); err != nil {
		t.Fatalf("decode problem detail: %v; body: %s", err, w.Body.String())
	}
	if pd.Ticket == "" {
		t.Errorf("expected a non-empty ticket on the 500 response; body: %s", w.Body.String())
	}
	// stateMachineItem's error carries the entity id and the parse failure
	// text ("state machine event of entity <id> has no valid id: invalid
	// UUID length: 0") — that is what common.Internal puts in AppError.Detail
	// and, in sanitized mode, never reaches the response. These are the
	// internals that could actually leak; Details itself is never part of
	// the error.
	body := w.Body.String()
	if strings.Contains(body, "some-entity-id") {
		t.Errorf("response leaked the entity id: %s", body)
	}
	if strings.Contains(body, "invalid UUID") {
		t.Errorf("response leaked parse-error internals: %s", body)
	}
}

// stubFieldsSMAuditStoreFinished answers GetEventsByTransaction with a
// single STATE_MACHINE_FINISH event whose TimeUUID is empty — the same
// store-fault case as TestSearch_SMEventWithoutID_Returns500, but through
// GetStateMachineFinishedEvent's own store call rather than GetEvents.
type stubFieldsSMAuditStoreFinished struct {
	spi.StateMachineAuditStore
}

func (stubFieldsSMAuditStoreFinished) GetEventsByTransaction(context.Context, string, string) ([]spi.StateMachineEvent, error) {
	return []spi.StateMachineEvent{
		{
			EventType: spi.SMEventFinished,
			EntityID:  "some-entity-id",
			TimeUUID:  "",
			Details:   "sensitive internal detail that must not leak",
			Timestamp: fieldsTestFixedTime,
		},
	}, nil
}

// TestGetStateMachineFinishedEvent_SMEventWithoutID_Returns500 is the
// finished-endpoint counterpart of TestSearch_SMEventWithoutID_Returns500:
// a STATE_MACHINE_FINISH event with no store-assigned id fails the request
// rather than being answered with a blank eventId.
func TestGetStateMachineFinishedEvent_SMEventWithoutID_Returns500(t *testing.T) {
	w := callFinishedEvent(t, stubFieldsSMAuditStoreFinished{})

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body: %s", w.Code, w.Body.String())
	}
	commontest.ExpectErrorCode(t, w.Result(), common.ErrCodeServerError)

	var pd struct {
		Ticket string `json:"ticket"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &pd); err != nil {
		t.Fatalf("decode problem detail: %v; body: %s", err, w.Body.String())
	}
	if pd.Ticket == "" {
		t.Errorf("expected a non-empty ticket on the 500 response; body: %s", w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, "some-entity-id") {
		t.Errorf("response leaked the entity id: %s", body)
	}
	if strings.Contains(body, "invalid UUID") {
		t.Errorf("response leaked parse-error internals: %s", body)
	}
}
