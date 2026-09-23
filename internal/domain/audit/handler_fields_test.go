package audit_test

import (
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
		if _, err := uuid.Parse(s); err != nil {
			t.Fatalf("event %d eventId %q is not a UUID", i, s)
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
	if want := "sensitive internal detail that must not leak"; strings.Contains(w.Body.String(), want) {
		t.Errorf("response leaked event internals: %s", w.Body.String())
	}
}
