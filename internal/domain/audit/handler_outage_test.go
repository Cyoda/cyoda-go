package audit_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/common/commontest"
	"github.com/cyoda-platform/cyoda-go/internal/domain/audit"
)

// smOutageDSN stands in for the connection detail a real driver error carries.
// Synthetic — it gives the Gate-3 leak assertion something to look for.
const smOutageDSN = "postgres://u:p@db/cyoda"

type smOutageErr struct{}

func (smOutageErr) Error() string            { return "acquire timed out: " + smOutageDSN }
func (smOutageErr) StorageUnavailable() bool { return true }

// stubSMAuditStore answers GetEventsByTransaction with either a canned failure
// or the empty slice every backend returns for a transaction with no events.
type stubSMAuditStore struct {
	spi.StateMachineAuditStore
	err error
}

func (s stubSMAuditStore) GetEventsByTransaction(context.Context, string, string) ([]spi.StateMachineEvent, error) {
	if s.err != nil {
		return nil, s.err
	}
	return []spi.StateMachineEvent{}, nil
}

// stubSMFactory hands back the stub store. Every other factory accessor is
// unimplemented — the handler reaches none of them on this path.
type stubSMFactory struct {
	spi.StoreFactory
	store spi.StateMachineAuditStore
}

func (f stubSMFactory) StateMachineAuditStore(context.Context) (spi.StateMachineAuditStore, error) {
	return f.store, nil
}

func callFinishedEvent(t *testing.T, store spi.StateMachineAuditStore) *httptest.ResponseRecorder {
	t.Helper()
	entityID, txID := uuid.New(), uuid.New()
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/audit/entity/%s/workflow/%s/finished", entityID, txID), nil)
	audit.New(stubSMFactory{store: store}).GetStateMachineFinishedEvent(w, r, entityID, txID)
	return w
}

// A failed audit read is not an absent workflow. Answering 404 during an outage
// tells the caller its transaction left no trace — a substituted answer that
// reads as a completed lookup.
func TestGetStateMachineFinishedEvent_StorageOutage_Returns503(t *testing.T) {
	w := callFinishedEvent(t, stubSMAuditStore{err: fmt.Errorf("failed to query events: %w", smOutageErr{})})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body: %s", w.Code, w.Body.String())
	}
	commontest.ExpectErrorCode(t, w.Result(), common.ErrCodeStorageUnavailable)
	var pd struct {
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &pd); err != nil {
		t.Fatalf("decode problem detail: %v; body: %s", err, w.Body.String())
	}
	if r, _ := pd.Properties["retryable"].(bool); !r {
		t.Errorf("503 is not advertised as retryable; body: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), smOutageDSN) {
		t.Errorf("response leaked storage internals: %s", w.Body.String())
	}
}

// The other direction: a transaction that genuinely has no events. Every
// backend reports that as an empty slice, never as an error, so the 404 this
// endpoint has always returned is untouched.
func TestGetStateMachineFinishedEvent_NoEvents_Still404(t *testing.T) {
	w := callFinishedEvent(t, stubSMAuditStore{})
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body: %s", w.Code, w.Body.String())
	}
	commontest.ExpectErrorCode(t, w.Result(), common.ErrCodeEntityNotFound)
}

// --- stubs for the search endpoint's state machine failure paths ---

// stubOutageSMStore answers GetEvents with either a canned failure or the
// empty slice every backend returns for an entity with no state machine
// events.
type stubOutageSMStore struct {
	spi.StateMachineAuditStore
	err error
}

func (s stubOutageSMStore) GetEvents(context.Context, string) ([]spi.StateMachineEvent, error) {
	if s.err != nil {
		return nil, s.err
	}
	return []spi.StateMachineEvent{}, nil
}

// stubOutageFactory pairs a fixed EntityStore (one valid EntityChange
// version, reusing stubFieldsEntityStore from handler_fields_test.go) with a
// StateMachineAuditStore accessor that can fail two different ways: the
// factory call itself (smStoreErr), or the store it hands back (getEventsErr).
// Only one of the two is set per test.
type stubOutageFactory struct {
	spi.StoreFactory
	smStoreErr   error
	getEventsErr error
}

func (stubOutageFactory) EntityStore(context.Context) (spi.EntityStore, error) {
	return stubFieldsEntityStore{}, nil
}

func (f stubOutageFactory) StateMachineAuditStore(context.Context) (spi.StateMachineAuditStore, error) {
	if f.smStoreErr != nil {
		return nil, f.smStoreErr
	}
	return stubOutageSMStore{err: f.getEventsErr}, nil
}

func callSearch(t *testing.T, factory spi.StoreFactory, params genapi.SearchEntityAuditEventsParams) *httptest.ResponseRecorder {
	t.Helper()
	entityID := uuid.New()
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/audit/entity/"+entityID.String(), nil).
		WithContext(pushdownTestCtx())
	audit.New(factory).SearchEntityAuditEvents(w, r, entityID, params)
	return w
}

func expect503(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body: %s", w.Code, w.Body.String())
	}
	commontest.ExpectErrorCode(t, w.Result(), common.ErrCodeStorageUnavailable)
	var pd struct {
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &pd); err != nil {
		t.Fatalf("decode problem detail: %v; body: %s", err, w.Body.String())
	}
	if r, _ := pd.Properties["retryable"].(bool); !r {
		t.Errorf("503 is not advertised as retryable; body: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), smOutageDSN) {
		t.Errorf("response leaked storage internals: %s", w.Body.String())
	}
}

func expect500(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
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
	if strings.Contains(w.Body.String(), "plain sm failure") {
		t.Errorf("response leaked internal error text: %s", w.Body.String())
	}
}

// A failed StateMachineAuditStore lookup is not an entity with no workflow
// events: answering 200 without them would present a partial trail as
// complete. These four tests cover both places that lookup can fail — the
// factory accessor and the store's GetEvents — crossed with both failure
// shapes (a storage outage vs. a plain error).

func TestSearch_SMFactoryOutage_Returns503(t *testing.T) {
	factory := stubOutageFactory{smStoreErr: fmt.Errorf("failed to acquire connection: %w", smOutageErr{})}
	w := callSearch(t, factory, genapi.SearchEntityAuditEventsParams{})
	expect503(t, w)
}

func TestSearch_SMFactoryFailure_Returns500(t *testing.T) {
	factory := stubOutageFactory{smStoreErr: errors.New("plain sm failure")}
	w := callSearch(t, factory, genapi.SearchEntityAuditEventsParams{})
	expect500(t, w)
}

func TestSearch_SMGetEventsOutage_Returns503(t *testing.T) {
	factory := stubOutageFactory{getEventsErr: fmt.Errorf("failed to query events: %w", smOutageErr{})}
	w := callSearch(t, factory, genapi.SearchEntityAuditEventsParams{})
	expect503(t, w)
}

func TestSearch_SMGetEventsFailure_Returns500(t *testing.T) {
	factory := stubOutageFactory{getEventsErr: errors.New("plain sm failure")}
	w := callSearch(t, factory, genapi.SearchEntityAuditEventsParams{})
	expect500(t, w)
}

// TestSearch_EntityChangeOnly_IgnoresSMStore pins the other side: filtering
// to EntityChange only must not reach the state machine store at all, even
// when it is set up to fail — the eventType filter, not the SM store's
// health, decides whether it is consulted.
func TestSearch_EntityChangeOnly_IgnoresSMStore(t *testing.T) {
	factory := stubOutageFactory{smStoreErr: errors.New("must not be reached")}
	params := genapi.SearchEntityAuditEventsParams{
		EventType: &[]genapi.SearchEntityAuditEventsParamsEventType{genapi.EntityChange},
	}
	w := callSearch(t, factory, params)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var body struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v; body: %s", err, w.Body.String())
	}
	if len(body.Items) != 1 {
		t.Fatalf("got %d items, want 1: %v", len(body.Items), body.Items)
	}
}
