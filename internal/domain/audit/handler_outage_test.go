package audit_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
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
