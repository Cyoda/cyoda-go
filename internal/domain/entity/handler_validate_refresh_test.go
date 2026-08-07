package entity

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

// refreshingStore is a ModelStore fake that implements RefreshAndGet
// with a queue: each Get returns the head of getQueue; each
// RefreshAndGet returns the head of refreshQueue.
type refreshingStore struct {
	mu           sync.Mutex
	getQueue     []*spi.ModelDescriptor
	refreshQueue []*spi.ModelDescriptor
	getCount     int
	refreshCount int
}

func (s *refreshingStore) Get(_ context.Context, _ spi.ModelRef) (*spi.ModelDescriptor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.getCount++
	if len(s.getQueue) == 0 {
		// Fallback to last refresh result if queue drained.
		if len(s.refreshQueue) > 0 {
			return s.refreshQueue[0], nil
		}
		return nil, nil
	}
	d := s.getQueue[0]
	s.getQueue = s.getQueue[1:]
	return d, nil
}

func (s *refreshingStore) RefreshAndGet(_ context.Context, _ spi.ModelRef) (*spi.ModelDescriptor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshCount++
	if len(s.refreshQueue) == 0 {
		return nil, nil
	}
	d := s.refreshQueue[0]
	s.refreshQueue = s.refreshQueue[1:]
	return d, nil
}

// Satisfy the rest of spi.ModelStore with no-ops.
func (s *refreshingStore) Save(context.Context, *spi.ModelDescriptor) error     { return nil }
func (s *refreshingStore) GetAll(context.Context) ([]spi.ModelRef, error)       { return nil, nil }
func (s *refreshingStore) Delete(context.Context, spi.ModelRef) error           { return nil }
func (s *refreshingStore) Lock(context.Context, spi.ModelRef) error             { return nil }
func (s *refreshingStore) Unlock(context.Context, spi.ModelRef) error           { return nil }
func (s *refreshingStore) IsLocked(context.Context, spi.ModelRef) (bool, error) { return true, nil }
func (s *refreshingStore) SetChangeLevel(context.Context, spi.ModelRef, spi.ChangeLevel) error {
	return nil
}
func (s *refreshingStore) ExtendSchema(context.Context, spi.ModelRef, spi.SchemaDelta) error {
	return nil
}

// Compile-time check that refreshingStore satisfies the SPI contract.
var _ spi.ModelStore = (*refreshingStore)(nil)

func (s *refreshingStore) GetCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getCount
}

func (s *refreshingStore) RefreshCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refreshCount
}

// buildDescriptorWithFields constructs a LOCKED descriptor whose Schema
// encodes an object node with the given named string-leaf children.
func buildDescriptorWithFields(t *testing.T, ref spi.ModelRef, fields ...string) *spi.ModelDescriptor {
	t.Helper()
	node := schema.NewObjectNode()
	for _, f := range fields {
		node.SetChild(f, schema.NewLeafNode(schema.String))
	}
	raw, err := schema.Marshal(node)
	if err != nil {
		t.Fatalf("schema.Marshal: %v", err)
	}
	return &spi.ModelDescriptor{
		Ref:    ref,
		State:  spi.ModelLocked,
		Schema: raw,
	}
}

func TestValidateWithRefresh_NoErrors_NoRefresh(t *testing.T) {
	h := &Handler{}
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	desc := buildDescriptorWithFields(t, ref, "a")
	ms := &refreshingStore{getQueue: []*spi.ModelDescriptor{desc}}

	err := h.ValidateWithRefresh(context.Background(), ms, ref, map[string]any{"a": "x"})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if ms.RefreshCount() != 0 {
		t.Errorf("expected 0 refreshes on clean validation, got %d", ms.RefreshCount())
	}
}

func TestValidateWithRefresh_StaleSchema_RefreshesOnce_ThenSucceeds(t *testing.T) {
	h := &Handler{}
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	stale := buildDescriptorWithFields(t, ref, "a")
	fresh := buildDescriptorWithFields(t, ref, "a", "b")
	ms := &refreshingStore{
		getQueue:     []*spi.ModelDescriptor{stale},
		refreshQueue: []*spi.ModelDescriptor{fresh},
	}

	// Data references 'b' — stale rejects, fresh accepts.
	err := h.ValidateWithRefresh(context.Background(), ms, ref, map[string]any{"a": "x", "b": "y"})
	if err != nil {
		t.Fatalf("expected pass after refresh, got %v", err)
	}
	if ms.RefreshCount() != 1 {
		t.Errorf("expected exactly 1 refresh, got %d", ms.RefreshCount())
	}
}

func TestValidateWithRefresh_RefreshStillStale_ReturnsErrors(t *testing.T) {
	h := &Handler{}
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	stale := buildDescriptorWithFields(t, ref, "a")
	stillStale := buildDescriptorWithFields(t, ref, "a")
	ms := &refreshingStore{
		getQueue:     []*spi.ModelDescriptor{stale},
		refreshQueue: []*spi.ModelDescriptor{stillStale},
	}

	err := h.ValidateWithRefresh(context.Background(), ms, ref, map[string]any{"a": "x", "b": "y"})
	if err == nil || !strings.Contains(err.Error(), "validation failed") {
		t.Errorf("expected validation failure after refresh, got %v", err)
	}
	if ms.RefreshCount() != 1 {
		t.Errorf("expected exactly 1 refresh (bounded), got %d", ms.RefreshCount())
	}
}

func TestValidateWithRefresh_TypeMismatch_NoRefresh(t *testing.T) {
	// Non-unknown-element validation failure — must not trigger refresh.
	h := &Handler{}
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	desc := buildDescriptorWithFields(t, ref, "a") // 'a' is String
	ms := &refreshingStore{getQueue: []*spi.ModelDescriptor{desc}}

	// Data for 'a' is the wrong type — bool not string.
	//
	// Updated for A.1: the value-based classifier in validate.go only
	// recognizes json.Number for numerics; raw Go ints leak through the
	// default branch as String, which would coincidentally satisfy the
	// String schema and mask the test's intent. Bool is classified
	// unambiguously via inferDataType and mismatches String reliably.
	err := h.ValidateWithRefresh(context.Background(), ms, ref, map[string]any{"a": true})
	if err == nil {
		t.Fatal("expected type-mismatch error")
	}
	if ms.RefreshCount() != 0 {
		t.Errorf("non-unknown-element failures must not refresh, got %d", ms.RefreshCount())
	}
}

func TestValidateWithRefresh_NoRefreshInterface_ReturnsErrorsDirect(t *testing.T) {
	h := &Handler{}
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	// recordingModelStore (from handler_validate_or_extend_test.go) does
	// NOT implement RefreshAndGet — the wrapper must surface the original
	// errors without attempting refresh.
	desc := buildDescriptorWithFields(t, ref, "a")
	ms := &recordingModelStore{descriptor: desc}

	err := h.ValidateWithRefresh(context.Background(), ms, ref, map[string]any{"a": "x", "b": "y"})
	if err == nil || !strings.Contains(err.Error(), "validation failed") {
		t.Errorf("expected direct validation failure (no refresh available), got %v", err)
	}
}

// failingModelStore is refreshingStore with either of its two reads failing, as
// a saturated pool or a cancelled statement makes them fail.
type failingModelStore struct {
	*refreshingStore
	getErr     error
	refreshErr error
}

func (s *failingModelStore) Get(ctx context.Context, ref spi.ModelRef) (*spi.ModelDescriptor, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	return s.refreshingStore.Get(ctx, ref)
}

func (s *failingModelStore) RefreshAndGet(ctx context.Context, ref spi.ModelRef) (*spi.ModelDescriptor, error) {
	if s.refreshErr != nil {
		return nil, s.refreshErr
	}
	return s.refreshingStore.RefreshAndGet(ctx, ref)
}

// TestValidateWithRefresh_StoreFailureIsClassifiedInternal — ValidateWithRefresh
// reads the model store twice, and neither read can fail for a reason the caller
// caused. Its errors are classified by classifyValidateOrExtendErr, whose
// catch-all is a 400 BAD_REQUEST carrying err.Error() verbatim: unmarked, a
// cancelled statement reaches the caller as a complaint about THEIR payload,
// with the driver's own text and a SQLSTATE in the response body.
//
// This is the guard that lets the helper be wired to a door (it is a
// ready-to-use wrapper with no production call site yet) without re-opening the
// hole in the process.
func TestValidateWithRefresh_StoreFailureIsClassifiedInternal(t *testing.T) {
	const storeText = "ERROR: canceling statement due to statement timeout (SQLSTATE 57014)"
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}

	cases := []struct {
		name  string
		store spi.ModelStore
		data  map[string]any
	}{
		{
			name:  "initial read",
			store: &failingModelStore{refreshingStore: &refreshingStore{}, getErr: errors.New(storeText)},
			data:  map[string]any{"a": "x"},
		},
		{
			name: "refresh-on-stale read",
			store: &failingModelStore{
				refreshingStore: &refreshingStore{
					getQueue: []*spi.ModelDescriptor{buildDescriptorWithFields(t, ref, "a")},
				},
				refreshErr: errors.New(storeText),
			},
			// References 'b', which the stale descriptor does not know — the
			// one signal that drives the second read.
			data: map[string]any{"a": "x", "b": "y"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handler{}
			err := h.ValidateWithRefresh(context.Background(), tc.store, ref, tc.data)
			if err == nil {
				t.Fatal("expected the store failure to surface")
			}

			appErr := classifyValidateOrExtendErr(err)
			if appErr.Status != http.StatusInternalServerError {
				t.Errorf("status = %d, want 500 — a store outage is not the caller's bad request", appErr.Status)
			}
			if appErr.Level != common.LevelInternal {
				t.Errorf("level = %v, want LevelInternal so the response carries a ticket", appErr.Level)
			}
			if strings.Contains(appErr.Message, "57014") || strings.Contains(appErr.Message, "SQLSTATE") {
				t.Errorf("client-facing message leaks store detail: %q", appErr.Message)
			}

			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/entity/JSON/E/1", nil)
			common.WriteError(rr, req, appErr)
			body := rr.Body.String()
			for _, leak := range []string{"SQLSTATE", "57014", "canceling statement"} {
				if strings.Contains(body, leak) {
					t.Errorf("client response leaks internal detail (%q): %s", leak, body)
				}
			}
			if !strings.Contains(body, "ticket") {
				t.Errorf("client response missing ticket correlation field: %s", body)
			}
		})
	}
}
