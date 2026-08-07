package entity_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/entity"
	wfengine "github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// A storage outage on the entity read that precedes a delete, an update or a
// transitions listing is not "the entity does not exist". Answering 404 there is
// a substituted answer: it reads as a completed lookup and stops the caller
// retrying, which is the failure mode
// .claude/rules/correctness-over-availability.md forbids. Every one of these
// paths must keep 404 for a genuine spi.ErrNotFound and surface anything else as
// a retryable 503 with its cause logged, not echoed.

// readOutageDSN stands in for the connection detail a real driver error carries,
// so the Gate-3 leak assertions have something to look for.
const readOutageDSN = "postgres://u:p@db/cyoda"

type readOutageErr struct{}

func (readOutageErr) Error() string            { return "acquire timed out: " + readOutageDSN }
func (readOutageErr) StorageUnavailable() bool { return true }

// storageOutage is the shape a plugin returns when the store could not serve the
// read at all; wrapped, because every real one is by the time a classifier sees it.
func storageOutage() error {
	return fmt.Errorf("entity read failed: %w", readOutageErr{})
}

// readFailEntityStore delegates every operation to a real backend except the two
// entity reads, which fail with a configured error. Only the read is faulted, so
// each test exercises the production classification and nothing else.
type readFailEntityStore struct {
	spi.EntityStore
	err error
}

func (s readFailEntityStore) Get(context.Context, string) (*spi.Entity, error) {
	return nil, s.err
}

func (s readFailEntityStore) GetAsAt(context.Context, string, time.Time) (*spi.Entity, error) {
	return nil, s.err
}

type readFailFactory struct {
	spi.StoreFactory
	err error
}

func (f readFailFactory) EntityStore(ctx context.Context) (spi.EntityStore, error) {
	inner, err := f.StoreFactory.EntityStore(ctx)
	if err != nil {
		return nil, err
	}
	return readFailEntityStore{EntityStore: inner, err: f.err}, nil
}

// newReadOutageHandler wires a Handler onto a real in-memory backend — real
// transaction manager, real begin/rollback — whose entity reads fail with readErr.
func newReadOutageHandler(t *testing.T, readErr error) (*entity.Handler, context.Context) {
	t.Helper()
	ctx := readOutageCtx()
	factory := readFailFactory{StoreFactory: memory.NewStoreFactory(), err: readErr}
	txMgr, err := factory.TransactionManager(ctx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	engine := wfengine.NewEngine(factory, common.NewDefaultUUIDGenerator(), txMgr)
	h := entity.New(factory, txMgr, common.NewDefaultUUIDGenerator(), engine, txgate.New(), nil)
	return h, ctx
}

func readOutageCtx() context.Context {
	return spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID:   "outage-user",
		UserName: "Outage",
		Tenant:   spi.Tenant{ID: "outage-tenant", Name: "Outage"},
		Roles:    []string{"user"},
	})
}

// expectStorageUnavailable asserts the retryable 503 contract on a service-layer
// error: the right code and status, advertised retryable, and no driver detail in
// the client-facing message (Gate 3).
func expectStorageUnavailable(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *common.AppError, got %T: %v", err, err)
	}
	if appErr.Status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; err: %v", appErr.Status, appErr)
	}
	if appErr.Code != common.ErrCodeStorageUnavailable {
		t.Errorf("code = %q, want %q", appErr.Code, common.ErrCodeStorageUnavailable)
	}
	if !appErr.Retryable {
		t.Errorf("503 is not advertised as retryable: %v", appErr)
	}
	if strings.Contains(appErr.Message, readOutageDSN) {
		t.Errorf("client-facing message leaked storage internals: %s", appErr.Message)
	}
}

// expectEntityNotFound asserts the other direction — the 404 these paths have
// always returned for an entity that genuinely is not there.
func expectEntityNotFound(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *common.AppError, got %T: %v", err, err)
	}
	if appErr.Status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; err: %v", appErr.Status, appErr)
	}
	if appErr.Code != common.ErrCodeEntityNotFound {
		t.Errorf("code = %q, want %q", appErr.Code, common.ErrCodeEntityNotFound)
	}
}

// --- DeleteEntity ------------------------------------------------------------

func TestDeleteEntity_StorageOutage_Returns503(t *testing.T) {
	h, ctx := newReadOutageHandler(t, storageOutage())
	_, err := h.DeleteEntity(ctx, uuid.NewString())
	expectStorageUnavailable(t, err)
}

func TestDeleteEntity_MissingEntity_Still404(t *testing.T) {
	h, ctx := newReadOutageHandler(t, spi.ErrNotFound)
	_, err := h.DeleteEntity(ctx, uuid.NewString())
	expectEntityNotFound(t, err)
}

// --- UpdateEntity (updateEntityCore, shared with PatchEntity) -----------------

func TestUpdateEntity_StorageOutage_Returns503(t *testing.T) {
	h, ctx := newReadOutageHandler(t, storageOutage())
	_, err := h.UpdateEntity(ctx, entity.UpdateEntityInput{
		EntityID: uuid.NewString(),
		Format:   "JSON",
		Data:     json.RawMessage(`{"name":"x"}`),
	})
	expectStorageUnavailable(t, err)
}

func TestUpdateEntity_MissingEntity_Still404(t *testing.T) {
	h, ctx := newReadOutageHandler(t, spi.ErrNotFound)
	_, err := h.UpdateEntity(ctx, entity.UpdateEntityInput{
		EntityID: uuid.NewString(),
		Format:   "JSON",
		Data:     json.RawMessage(`{"name":"x"}`),
	})
	expectEntityNotFound(t, err)
}

// --- UpdateEntityCollection --------------------------------------------------

func TestUpdateEntityCollection_StorageOutage_Returns503(t *testing.T) {
	h, ctx := newReadOutageHandler(t, storageOutage())
	_, err := h.UpdateEntityCollection(ctx, []entity.UpdateCollectionItem{
		{EntityID: uuid.NewString(), Payload: json.RawMessage(`{"name":"x"}`)},
	})
	expectStorageUnavailable(t, err)
}

func TestUpdateEntityCollection_MissingEntity_Still404(t *testing.T) {
	h, ctx := newReadOutageHandler(t, spi.ErrNotFound)
	_, err := h.UpdateEntityCollection(ctx, []entity.UpdateCollectionItem{
		{EntityID: uuid.NewString(), Payload: json.RawMessage(`{"name":"x"}`)},
	})
	expectEntityNotFound(t, err)
}

// --- HandleGetTransitions ----------------------------------------------------
//
// This route declares 503 in api/openapi.yaml, and its platform-api alias reads
// the entity through workflow.GetAvailableTransitions, which has classified the
// outage correctly all along. The two must not diverge on the same condition.

func callGetTransitions(t *testing.T, readErr error) *httptest.ResponseRecorder {
	t.Helper()
	h, _ := newReadOutageHandler(t, readErr)
	entityID := uuid.NewString()
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/entity/"+entityID+"/transitions", nil)
	r.SetPathValue("entityId", entityID)
	h.HandleGetTransitions(w, r.WithContext(readOutageCtx()))
	return w
}

func TestHandleGetTransitions_StorageOutage_Returns503(t *testing.T) {
	w := callGetTransitions(t, storageOutage())
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body: %s", w.Code, w.Body.String())
	}
	var pd struct {
		Detail     string         `json:"detail"`
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &pd); err != nil {
		t.Fatalf("decode problem detail: %v; body: %s", err, w.Body.String())
	}
	if !strings.HasPrefix(pd.Detail, common.ErrCodeStorageUnavailable+":") {
		t.Errorf("detail = %q, want a %s prefix", pd.Detail, common.ErrCodeStorageUnavailable)
	}
	if r, _ := pd.Properties["retryable"].(bool); !r {
		t.Errorf("503 is not advertised as retryable; body: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), readOutageDSN) {
		t.Errorf("response leaked storage internals: %s", w.Body.String())
	}
}

func TestHandleGetTransitions_MissingEntity_Still404(t *testing.T) {
	w := callGetTransitions(t, spi.ErrNotFound)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), common.ErrCodeEntityNotFound) {
		t.Errorf("body does not carry %s: %s", common.ErrCodeEntityNotFound, w.Body.String())
	}
}
