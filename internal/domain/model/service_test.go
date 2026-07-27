package model_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

// refreshingModelStore is a ModelStore fake that:
//   - returns getDescriptor from Get (the "stale" view)
//   - returns refreshDescriptor from RefreshAndGet (the "fresh" view)
//
// Save, Get, and RefreshAndGet calls are counted for assertions. Save
// also captures the last saved descriptor so tests can verify the
// import produced a new UNLOCKED descriptor.
type refreshingModelStore struct {
	mu sync.Mutex

	// getDescriptor is what Get returns. Typically a LOCKED stale value.
	getDescriptor *spi.ModelDescriptor
	getErr        error
	getCount      int

	// refreshDescriptor is what RefreshAndGet returns. nil models the
	// authoritative post-delete state where no model exists upstream.
	refreshDescriptor *spi.ModelDescriptor
	refreshErr        error
	refreshCount      int

	// saved is the last descriptor passed to Save, if any.
	saved     *spi.ModelDescriptor
	saveCount int
}

func (s *refreshingModelStore) Get(_ context.Context, _ spi.ModelRef) (*spi.ModelDescriptor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.getCount++
	return s.getDescriptor, s.getErr
}

func (s *refreshingModelStore) RefreshAndGet(_ context.Context, _ spi.ModelRef) (*spi.ModelDescriptor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshCount++
	return s.refreshDescriptor, s.refreshErr
}

func (s *refreshingModelStore) Save(_ context.Context, d *spi.ModelDescriptor) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saveCount++
	s.saved = d
	return nil
}

func (s *refreshingModelStore) GetAll(context.Context) ([]spi.ModelRef, error) { return nil, nil }
func (s *refreshingModelStore) Delete(context.Context, spi.ModelRef) error     { return nil }
func (s *refreshingModelStore) Lock(context.Context, spi.ModelRef) error       { return nil }
func (s *refreshingModelStore) Unlock(context.Context, spi.ModelRef) error     { return nil }
func (s *refreshingModelStore) IsLocked(context.Context, spi.ModelRef) (bool, error) {
	return true, nil
}
func (s *refreshingModelStore) SetChangeLevel(context.Context, spi.ModelRef, spi.ChangeLevel) error {
	return nil
}
func (s *refreshingModelStore) ExtendSchema(context.Context, spi.ModelRef, spi.SchemaDelta) error {
	return nil
}

// Compile-time check that refreshingModelStore satisfies the SPI contract.
var _ spi.ModelStore = (*refreshingModelStore)(nil)

func (s *refreshingModelStore) GetCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getCount
}

func (s *refreshingModelStore) RefreshCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refreshCount
}

func (s *refreshingModelStore) Saved() *spi.ModelDescriptor {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saved
}

// fakeStoreFactory satisfies spi.StoreFactory with the given ModelStore.
// All other stores return an error — ImportModel only touches ModelStore.
type fakeStoreFactory struct {
	modelStore spi.ModelStore
}

func (f *fakeStoreFactory) ModelStore(_ context.Context) (spi.ModelStore, error) {
	return f.modelStore, nil
}

var errUnused = errors.New("store not used by this test")

func (f *fakeStoreFactory) EntityStore(_ context.Context) (spi.EntityStore, error) {
	return nil, errUnused
}
func (f *fakeStoreFactory) KeyValueStore(_ context.Context) (spi.KeyValueStore, error) {
	return nil, errUnused
}
func (f *fakeStoreFactory) MessageStore(_ context.Context) (spi.MessageStore, error) {
	return nil, errUnused
}
func (f *fakeStoreFactory) WorkflowStore(_ context.Context) (spi.WorkflowStore, error) {
	return nil, errUnused
}
func (f *fakeStoreFactory) StateMachineAuditStore(_ context.Context) (spi.StateMachineAuditStore, error) {
	return nil, errUnused
}
func (f *fakeStoreFactory) AsyncSearchStore(_ context.Context) (spi.AsyncSearchStore, error) {
	return nil, errUnused
}
func (f *fakeStoreFactory) ScheduledTaskStore(_ context.Context) (spi.ScheduledTaskStore, error) {
	return nil, errUnused
}
func (f *fakeStoreFactory) TransactionManager(_ context.Context) (spi.TransactionManager, error) {
	return nil, errUnused
}
func (f *fakeStoreFactory) Close() error { return nil }

// TestImportModel_StaleCacheAfterRemoteDelete_ProceedsAfterRefresh verifies
// that when the cached ModelStore.Get returns a stale LOCKED descriptor
// (e.g. because a peer's delete was broadcast on gossip but hasn't landed
// on this node yet), ImportModel consults RefreshAndGet to bypass the
// cache. The fresh authoritative state is "no model exists", so the import
// must proceed (Save) rather than reject with 409.
func TestImportModel_StaleCacheAfterRemoteDelete_ProceedsAfterRefresh(t *testing.T) {
	staleRef := spi.ModelRef{EntityName: "Dataset", ModelVersion: "1"}
	stale := &spi.ModelDescriptor{
		Ref:   staleRef,
		State: spi.ModelLocked,
		// No Schema — merging path not exercised; fresh path is nil.
	}
	ms := &refreshingModelStore{
		getDescriptor:     stale,
		refreshDescriptor: nil, // peer's delete propagated authoritatively
	}

	h := model.New(&fakeStoreFactory{modelStore: ms})

	// A trivial JSON sample — the importer only needs parseable sample data.
	result, err := h.ImportModel(context.Background(), model.ImportModelInput{
		EntityName:   "Dataset",
		ModelVersion: "1",
		Format:       "JSON",
		Converter:    "SAMPLE_DATA",
		Data:         []byte(`{"field":"value"}`),
	})
	if err != nil {
		t.Fatalf("ImportModel: expected success after cache refresh, got %v", err)
	}
	if result == nil || result.ModelID == "" {
		t.Fatalf("expected non-empty ModelID in result, got %+v", result)
	}
	if ms.RefreshCount() == 0 {
		t.Errorf("expected RefreshAndGet to be called at least once, got 0")
	}
	if ms.Saved() == nil {
		t.Fatal("expected ModelStore.Save to be called with new descriptor")
	}
	if ms.Saved().State != spi.ModelUnlocked {
		t.Errorf("expected saved descriptor State=UNLOCKED, got %s", ms.Saved().State)
	}
}

// TestLockModel_StaleCacheAfterRemoteDelete_404Not409 verifies that
// LockModel's existence pre-check goes through RefreshAndGet, not the
// cached Get. When a peer has deleted the model but this node still
// holds a stale LOCKED descriptor in its per-request cache, LockModel
// must observe the authoritative "gone" state and return 404
// (model-not-found), not 409 (already-locked).
//
// This test documents the pattern applied to LockModel, UnlockModel,
// DeleteModel, and SetChangeLevel — all four sites have the same shape
// and share the getModelFresh helper.
func TestLockModel_StaleCacheAfterRemoteDelete_404Not409(t *testing.T) {
	staleRef := spi.ModelRef{EntityName: "Dataset", ModelVersion: "1"}
	stale := &spi.ModelDescriptor{
		Ref:   staleRef,
		State: spi.ModelLocked,
	}
	ms := &refreshingModelStore{
		getDescriptor:     stale,
		refreshDescriptor: nil, // peer's delete propagated authoritatively
	}

	h := model.New(&fakeStoreFactory{modelStore: ms})

	_, err := h.LockModel(context.Background(), "Dataset", "1")
	if err == nil {
		t.Fatalf("LockModel: expected model-not-found error after refresh, got nil")
	}
	if ms.RefreshCount() == 0 {
		t.Errorf("expected RefreshAndGet to be called at least once, got 0")
	}
	// The error must be a 404 (MODEL_NOT_FOUND), not a 409 (already-locked
	// conflict). The exact error type comes from modelNotFound() in
	// handler.go — we match on its code/status rather than the message.
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *common.AppError, got %T: %v", err, err)
	}
	if appErr.Status != 404 {
		t.Errorf("expected HTTP 404 (model-not-found), got %d: %s", appErr.Status, appErr.Message)
	}
}

// TestExportModel_ClassifiesModelStoreErrors verifies that ExportModel
// distinguishes spi.ErrNotFound (a legitimate 404) from other infrastructure
// errors returned by ModelStore.Get (which must be 5xx). Blanket-mapping every
// Get error to 404 MODEL_NOT_FOUND hides real failures — a schema fold or a
// transient pgx connection blip would look indistinguishable from a genuine
// missing model.
func TestExportModel_ClassifiesModelStoreErrors(t *testing.T) {
	ref := spi.ModelRef{EntityName: "Dataset", ModelVersion: "1"}

	t.Run("ErrNotFound maps to 404", func(t *testing.T) {
		ms := &refreshingModelStore{getErr: spi.ErrNotFound}
		h := model.New(&fakeStoreFactory{modelStore: ms})

		_, err := h.ExportModel(context.Background(), "Dataset", "1", "JSON_SCHEMA")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		var appErr *common.AppError
		if !errors.As(err, &appErr) {
			t.Fatalf("expected *common.AppError, got %T: %v", err, err)
		}
		if appErr.Status != 404 {
			t.Errorf("expected 404 for ErrNotFound, got %d: %s", appErr.Status, appErr.Message)
		}
	})

	t.Run("arbitrary error maps to 5xx", func(t *testing.T) {
		synthetic := errors.New("synthetic fold failure")
		ms := &refreshingModelStore{getErr: synthetic}
		h := model.New(&fakeStoreFactory{modelStore: ms})

		_, err := h.ExportModel(context.Background(), "Dataset", "1", "JSON_SCHEMA")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		var appErr *common.AppError
		if !errors.As(err, &appErr) {
			t.Fatalf("expected *common.AppError, got %T: %v", err, err)
		}
		if appErr.Status == 404 {
			t.Errorf("non-ErrNotFound infra error must not be 404 MODEL_NOT_FOUND; got %d: %s", appErr.Status, appErr.Message)
		}
		if appErr.Status < 500 || appErr.Status >= 600 {
			t.Errorf("expected 5xx for non-ErrNotFound error, got %d: %s", appErr.Status, appErr.Message)
		}
		// The original error must be preserved in the chain for logging /
		// correlation via the ticket UUID.
		if !errors.Is(err, synthetic) {
			t.Errorf("expected wrapped error to satisfy errors.Is(synthetic), got %v", err)
		}
	})

	_ = ref // silence unused in case future expansion needs it
}

// TestImportModel_OnLockedModel_ReturnsModelAlreadyLocked verifies that a
// re-import targeting a LOCKED model surfaces the dictionary-aligned
// `MODEL_ALREADY_LOCKED` code rather than the generic `CONFLICT`. The state
// precondition (expected UNLOCKED, actual LOCKED) is identical to the relock
// branch, so it shares the code. See #128.
func TestImportModel_OnLockedModel_ReturnsModelAlreadyLocked(t *testing.T) {
	ref := spi.ModelRef{EntityName: "Dataset", ModelVersion: "1"}
	locked := &spi.ModelDescriptor{Ref: ref, State: spi.ModelLocked}
	ms := &refreshingModelStore{
		getDescriptor:     locked,
		refreshDescriptor: locked,
	}

	h := model.New(&fakeStoreFactory{modelStore: ms})

	_, err := h.ImportModel(context.Background(), model.ImportModelInput{
		EntityName:   "Dataset",
		ModelVersion: "1",
		Format:       "JSON",
		Converter:    "SAMPLE_DATA",
		Data:         []byte(`{"field":"value"}`),
	})
	if err == nil {
		t.Fatal("ImportModel on locked model: expected error, got nil")
	}
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *common.AppError, got %T: %v", err, err)
	}
	if appErr.Status != 409 {
		t.Errorf("expected HTTP 409, got %d: %s", appErr.Status, appErr.Message)
	}
	if appErr.Code != common.ErrCodeModelAlreadyLocked {
		t.Errorf("expected error code %q, got %q (message: %s)",
			common.ErrCodeModelAlreadyLocked, appErr.Code, appErr.Message)
	}
}

// TestUnlockModel_AlreadyUnlocked_ReturnsModelAlreadyUnlocked verifies the
// symmetric counterpart to the relock fix: unlocking a model that is already
// UNLOCKED rejects with a specific `MODEL_ALREADY_UNLOCKED` code rather than
// the generic `CONFLICT`. Distinct from `MODEL_NOT_LOCKED`, which is reserved
// for the entity-write-without-lock path on the entity service.
func TestUnlockModel_AlreadyUnlocked_ReturnsModelAlreadyUnlocked(t *testing.T) {
	ref := spi.ModelRef{EntityName: "Dataset", ModelVersion: "1"}
	unlocked := &spi.ModelDescriptor{Ref: ref, State: spi.ModelUnlocked}
	ms := &refreshingModelStore{
		getDescriptor:     unlocked,
		refreshDescriptor: unlocked,
	}

	h := model.New(&fakeStoreFactory{modelStore: ms})

	_, err := h.UnlockModel(context.Background(), "Dataset", "1")
	if err == nil {
		t.Fatal("UnlockModel on unlocked model: expected error, got nil")
	}
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *common.AppError, got %T: %v", err, err)
	}
	if appErr.Status != 409 {
		t.Errorf("expected HTTP 409, got %d: %s", appErr.Status, appErr.Message)
	}
	if appErr.Code != common.ErrCodeModelAlreadyUnlocked {
		t.Errorf("expected error code %q, got %q (message: %s)",
			common.ErrCodeModelAlreadyUnlocked, appErr.Code, appErr.Message)
	}
}

// TestSetChangeLevel_Invalid_400 verifies that SetChangeLevel returns a 400
// INVALID_CHANGE_LEVEL error when given an off-enum value. The model must exist
// (non-nil refreshDescriptor) so execution reaches the changeLevel validation
// step rather than returning 404.
func TestSetChangeLevel_Invalid_400(t *testing.T) {
	ref := spi.ModelRef{EntityName: "Dataset", ModelVersion: "1"}
	desc := &spi.ModelDescriptor{Ref: ref, State: spi.ModelLocked}
	ms := &refreshingModelStore{
		getDescriptor:     desc,
		refreshDescriptor: desc,
	}

	h := model.New(&fakeStoreFactory{modelStore: ms})

	err := h.SetChangeLevel(context.Background(), "Dataset", "1", "BOGUS")
	if err == nil {
		t.Fatal("SetChangeLevel with invalid level: expected error, got nil")
	}
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *common.AppError, got %T: %v", err, err)
	}
	if appErr.Status != 400 {
		t.Errorf("expected HTTP 400, got %d: %s", appErr.Status, appErr.Message)
	}
	if appErr.Code != common.ErrCodeInvalidChangeLevel {
		t.Errorf("expected error code %q, got %q (message: %s)",
			common.ErrCodeInvalidChangeLevel, appErr.Code, appErr.Message)
	}
}

// capableStoreFactory wraps fakeStoreFactory and additionally implements
// spi.CompositeUniqueKeyCapable. The supports field controls whether the
// factory advertises composite-unique-key support.
type capableStoreFactory struct {
	fakeStoreFactory
	supports bool
}

func (f *capableStoreFactory) SupportsCompositeUniqueKeys() bool { return f.supports }

// mustBuildSchema builds and marshals a schema with two scalar leaf fields:
//
//	$.name (string) and $.age (integer)
//
// It is used by SetUniqueKeys tests that need a stored schema to validate keys against.
func mustBuildSchema(t *testing.T) []byte {
	t.Helper()
	root := schema.NewObjectNode()
	root.SetChild("name", schema.NewLeafNode(schema.String))
	root.SetChild("age", schema.NewLeafNode(schema.Integer))
	b, err := schema.Marshal(root)
	if err != nil {
		t.Fatalf("mustBuildSchema: marshal failed: %v", err)
	}
	return b
}

// TestSetUniqueKeys_UnsupportedBackend_Returns422 verifies that a factory
// that does not implement spi.CompositeUniqueKeyCapable causes SetUniqueKeys
// to return a 422 COMPOSITE_KEY_UNSUPPORTED error immediately without
// touching the model store.
func TestSetUniqueKeys_UnsupportedBackend_Returns422(t *testing.T) {
	ms := &refreshingModelStore{}
	// fakeStoreFactory does NOT implement CompositeUniqueKeyCapable.
	h := model.New(&fakeStoreFactory{modelStore: ms})

	_, err := h.SetUniqueKeys(context.Background(), "Dataset", "1", []spi.UniqueKey{
		{ID: "uk1", Fields: []string{"$.name"}},
	})
	if err == nil {
		t.Fatal("expected error for unsupported backend, got nil")
	}
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *common.AppError, got %T: %v", err, err)
	}
	if appErr.Status != 422 {
		t.Errorf("expected HTTP 422, got %d: %s", appErr.Status, appErr.Message)
	}
	if appErr.Code != common.ErrCodeCompositeKeyUnsupported {
		t.Errorf("expected code %q, got %q", common.ErrCodeCompositeKeyUnsupported, appErr.Code)
	}
}

// TestSetUniqueKeys_ModelNotFound_Returns404 verifies that SetUniqueKeys
// returns 404 MODEL_NOT_FOUND when the model store returns nil from
// RefreshAndGet (the authoritative "no model" state).
func TestSetUniqueKeys_ModelNotFound_Returns404(t *testing.T) {
	ms := &refreshingModelStore{refreshDescriptor: nil}
	h := model.New(&capableStoreFactory{
		fakeStoreFactory: fakeStoreFactory{modelStore: ms},
		supports:         true,
	})

	_, err := h.SetUniqueKeys(context.Background(), "Dataset", "1", []spi.UniqueKey{
		{ID: "uk1", Fields: []string{"$.name"}},
	})
	if err == nil {
		t.Fatal("expected error for not-found model, got nil")
	}
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *common.AppError, got %T: %v", err, err)
	}
	if appErr.Status != 404 {
		t.Errorf("expected HTTP 404, got %d: %s", appErr.Status, appErr.Message)
	}
	if appErr.Code != common.ErrCodeModelNotFound {
		t.Errorf("expected code %q, got %q", common.ErrCodeModelNotFound, appErr.Code)
	}
}

// TestSetUniqueKeys_LockedModel_Returns409 verifies that SetUniqueKeys
// returns 409 MODEL_ALREADY_LOCKED when the model is in LOCKED state,
// because unique-key definitions are only editable while the model is UNLOCKED.
func TestSetUniqueKeys_LockedModel_Returns409(t *testing.T) {
	ref := spi.ModelRef{EntityName: "Dataset", ModelVersion: "1"}
	locked := &spi.ModelDescriptor{
		Ref:    ref,
		State:  spi.ModelLocked,
		Schema: mustBuildSchema(t),
	}
	ms := &refreshingModelStore{refreshDescriptor: locked}
	h := model.New(&capableStoreFactory{
		fakeStoreFactory: fakeStoreFactory{modelStore: ms},
		supports:         true,
	})

	_, err := h.SetUniqueKeys(context.Background(), "Dataset", "1", []spi.UniqueKey{
		{ID: "uk1", Fields: []string{"$.name"}},
	})
	if err == nil {
		t.Fatal("expected error for locked model, got nil")
	}
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *common.AppError, got %T: %v", err, err)
	}
	if appErr.Status != 409 {
		t.Errorf("expected HTTP 409, got %d: %s", appErr.Status, appErr.Message)
	}
	if appErr.Code != common.ErrCodeModelAlreadyLocked {
		t.Errorf("expected code %q, got %q", common.ErrCodeModelAlreadyLocked, appErr.Code)
	}
}

// TestSetUniqueKeys_UnknownField_Returns422_INVALID_UNIQUE_KEY_DEFINITION
// verifies that referencing a field that does not exist as a scalar leaf in
// the schema returns 422 INVALID_UNIQUE_KEY_DEFINITION.
func TestSetUniqueKeys_UnknownField_Returns422_INVALID_UNIQUE_KEY_DEFINITION(t *testing.T) {
	ref := spi.ModelRef{EntityName: "Dataset", ModelVersion: "1"}
	unlocked := &spi.ModelDescriptor{
		Ref:    ref,
		State:  spi.ModelUnlocked,
		Schema: mustBuildSchema(t),
	}
	ms := &refreshingModelStore{refreshDescriptor: unlocked}
	h := model.New(&capableStoreFactory{
		fakeStoreFactory: fakeStoreFactory{modelStore: ms},
		supports:         true,
	})

	_, err := h.SetUniqueKeys(context.Background(), "Dataset", "1", []spi.UniqueKey{
		{ID: "uk1", Fields: []string{"$.nonexistent"}},
	})
	if err == nil {
		t.Fatal("expected error for unknown field, got nil")
	}
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *common.AppError, got %T: %v", err, err)
	}
	if appErr.Status != 422 {
		t.Errorf("expected HTTP 422, got %d: %s", appErr.Status, appErr.Message)
	}
	if appErr.Code != common.ErrCodeInvalidUniqueKeyDefinition {
		t.Errorf("expected code %q, got %q", common.ErrCodeInvalidUniqueKeyDefinition, appErr.Code)
	}
}

// TestSetUniqueKeys_HappyPath_PersistsKeys verifies that SetUniqueKeys
// saves the updated descriptor with the new UniqueKeys and returns a
// ModelTransitionResult with the correct ModelID and State.
func TestSetUniqueKeys_HappyPath_PersistsKeys(t *testing.T) {
	ref := spi.ModelRef{EntityName: "Dataset", ModelVersion: "1"}
	unlocked := &spi.ModelDescriptor{
		Ref:    ref,
		State:  spi.ModelUnlocked,
		Schema: mustBuildSchema(t),
	}
	ms := &refreshingModelStore{refreshDescriptor: unlocked}
	h := model.New(&capableStoreFactory{
		fakeStoreFactory: fakeStoreFactory{modelStore: ms},
		supports:         true,
	})

	keys := []spi.UniqueKey{
		{ID: "uk1", Fields: []string{"$.name", "$.age"}},
	}
	result, err := h.SetUniqueKeys(context.Background(), "Dataset", "1", keys)
	if err != nil {
		t.Fatalf("SetUniqueKeys: unexpected error: %v", err)
	}
	if result == nil || result.ModelID == "" {
		t.Fatal("expected non-empty ModelID in result")
	}
	if result.State != "UNLOCKED" {
		t.Errorf("expected State=UNLOCKED, got %q", result.State)
	}

	saved := ms.Saved()
	if saved == nil {
		t.Fatal("expected ModelStore.Save to be called")
	}
	if len(saved.UniqueKeys) != 1 {
		t.Fatalf("expected 1 UniqueKey persisted, got %d", len(saved.UniqueKeys))
	}
	if saved.UniqueKeys[0].ID != "uk1" {
		t.Errorf("expected key ID %q, got %q", "uk1", saved.UniqueKeys[0].ID)
	}
}

// TestImportModel_PreservesUniqueKeys_WhenReimporting verifies that when
// an existing model already has UniqueKeys set, a re-import carries those
// keys forward (analogous to how ChangeLevel is preserved).
func TestImportModel_PreservesUniqueKeys_WhenReimporting(t *testing.T) {
	ref := spi.ModelRef{EntityName: "Dataset", ModelVersion: "1"}
	existingSchema := mustBuildSchema(t)
	existing := &spi.ModelDescriptor{
		Ref:    ref,
		State:  spi.ModelUnlocked,
		Schema: existingSchema,
		UniqueKeys: []spi.UniqueKey{
			{ID: "uk1", Fields: []string{"$.name"}},
		},
	}
	ms := &refreshingModelStore{
		getDescriptor:     existing,
		refreshDescriptor: existing,
	}

	h := model.New(&fakeStoreFactory{modelStore: ms})

	// Re-import with a JSON sample that contains the same fields (no removal).
	result, err := h.ImportModel(context.Background(), model.ImportModelInput{
		EntityName:   "Dataset",
		ModelVersion: "1",
		Format:       "JSON",
		Converter:    "SAMPLE_DATA",
		Data:         []byte(`{"name":"Alice","age":30}`),
	})
	if err != nil {
		t.Fatalf("ImportModel: unexpected error on re-import: %v", err)
	}
	if result == nil || result.ModelID == "" {
		t.Fatal("expected non-empty ModelID in result")
	}

	saved := ms.Saved()
	if saved == nil {
		t.Fatal("expected ModelStore.Save to be called")
	}
	if len(saved.UniqueKeys) != 1 {
		t.Fatalf("expected 1 UniqueKey preserved, got %d", len(saved.UniqueKeys))
	}
	if saved.UniqueKeys[0].ID != "uk1" {
		t.Errorf("expected key ID %q, got %q", "uk1", saved.UniqueKeys[0].ID)
	}
}

// TestImportModel_KeyReferencesAbsentField_Rejected is a defensive guard test.
// schema.Merge is strictly additive (every existing field is unconditionally
// preserved), so a normal SetUniqueKeys + re-import sequence cannot produce a
// descriptor whose key references a field absent from the schema. This test
// exercises the re-validate branch to protect against out-of-band descriptor
// corruption or future merge-semantics changes — it is not an API-reachable
// "dropped field" scenario under current semantics.
func TestImportModel_KeyReferencesAbsentField_Rejected(t *testing.T) {
	ref := spi.ModelRef{EntityName: "Dataset", ModelVersion: "1"}

	// Build a schema with $.name field only.
	nameOnly := schema.NewObjectNode()
	nameOnly.SetChild("name", schema.NewLeafNode(schema.String))
	nameOnlyBytes, err := schema.Marshal(nameOnly)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	// The existing model has a unique key on $.name and $.score, where $.score
	// will be absent from the incoming sample data.
	existing := &spi.ModelDescriptor{
		Ref:    ref,
		State:  spi.ModelUnlocked,
		Schema: nameOnlyBytes,
		UniqueKeys: []spi.UniqueKey{
			{ID: "uk1", Fields: []string{"$.name", "$.score"}},
		},
	}
	ms := &refreshingModelStore{
		getDescriptor:     existing,
		refreshDescriptor: existing,
	}

	h := model.New(&fakeStoreFactory{modelStore: ms})

	// The descriptor is pre-seeded with $.score in the key but NOT in the
	// schema, simulating out-of-band corruption (additive merge cannot drop a
	// field). Re-import must detect and reject this corrupted state.
	_, err = h.ImportModel(context.Background(), model.ImportModelInput{
		EntityName:   "Dataset",
		ModelVersion: "1",
		Format:       "JSON",
		Converter:    "SAMPLE_DATA",
		Data:         []byte(`{"name":"Alice"}`),
	})
	if err == nil {
		t.Fatal("expected error when re-import drops a key field, got nil")
	}
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *common.AppError, got %T: %v", err, err)
	}
	if appErr.Status != 422 {
		t.Errorf("expected HTTP 422, got %d: %s", appErr.Status, appErr.Message)
	}
	if appErr.Code != common.ErrCodeInvalidUniqueKeyDefinition {
		t.Errorf("expected code %q, got %q", common.ErrCodeInvalidUniqueKeyDefinition, appErr.Code)
	}
}

// TestLockModel_AlreadyLocked_ReturnsSpecificCode verifies that a relock
// attempt returns the dictionary-aligned `MODEL_ALREADY_LOCKED` code rather
// than the generic `CONFLICT`. cyoda-cloud's dictionary asserts the specific
// failure mode (cf. EntityModelFacadeIT.kt's class-name regex), and the
// generic code discards information the dictionary preserves. See #128.
func TestLockModel_AlreadyLocked_ReturnsSpecificCode(t *testing.T) {
	ref := spi.ModelRef{EntityName: "Dataset", ModelVersion: "1"}
	locked := &spi.ModelDescriptor{Ref: ref, State: spi.ModelLocked}
	ms := &refreshingModelStore{
		getDescriptor:     locked,
		refreshDescriptor: locked,
	}

	h := model.New(&fakeStoreFactory{modelStore: ms})

	_, err := h.LockModel(context.Background(), "Dataset", "1")
	if err == nil {
		t.Fatal("LockModel on locked model: expected error, got nil")
	}
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *common.AppError, got %T: %v", err, err)
	}
	if appErr.Status != 409 {
		t.Errorf("expected HTTP 409, got %d: %s", appErr.Status, appErr.Message)
	}
	if appErr.Code != common.ErrCodeModelAlreadyLocked {
		t.Errorf("expected error code %q, got %q (message: %s)",
			common.ErrCodeModelAlreadyLocked, appErr.Code, appErr.Message)
	}
}

// TestImportModel_InvalidUniqueKeyDefinition_422 verifies the defensive guard
// in ImportModel that re-validates carried unique keys against the merged
// schema on re-import.
//
// The guard targets out-of-band descriptor corruption (e.g. a key referencing
// a field that was never in the schema) rather than a scenario reachable via
// normal API use — schema.Merge is additive and cannot drop an existing field.
// We simulate corruption by seeding the store with an UNLOCKED descriptor
// whose UniqueKeys reference a phantom field not present in any schema, then
// re-importing sample data that does not contain that field.
func TestImportModel_InvalidUniqueKeyDefinition_422(t *testing.T) {
	// Seed the store with an existing UNLOCKED descriptor whose UniqueKeys
	// reference "PHANTOM_FIELD" — a field that never existed in any schema.
	// Schema is intentionally nil so the service takes the newNode branch
	// (finalNode = newNode from the import data), which also won't contain
	// PHANTOM_FIELD.
	corrupted := &spi.ModelDescriptor{
		Ref:   spi.ModelRef{EntityName: "Dataset", ModelVersion: "1"},
		State: spi.ModelUnlocked,
		UniqueKeys: []spi.UniqueKey{
			{ID: "k1", Fields: []string{"PHANTOM_FIELD"}},
		},
		// No Schema — forces finalNode = newNode path; PHANTOM_FIELD absent.
	}
	ms := &refreshingModelStore{
		refreshDescriptor: corrupted,
	}
	h := model.New(&fakeStoreFactory{modelStore: ms})

	_, err := h.ImportModel(context.Background(), model.ImportModelInput{
		EntityName:   "Dataset",
		ModelVersion: "1",
		Format:       "JSON",
		Converter:    "SAMPLE_DATA",
		Data:         []byte(`{"name":"x"}`),
	})
	if err == nil {
		t.Fatal("ImportModel: expected 422 error for corrupted unique key definition, got nil")
	}
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *common.AppError, got %T: %v", err, err)
	}
	if appErr.Status != 422 {
		t.Errorf("expected HTTP 422, got %d: %s", appErr.Status, appErr.Message)
	}
	if appErr.Code != common.ErrCodeInvalidUniqueKeyDefinition {
		t.Errorf("expected error code %q, got %q (message: %s)",
			common.ErrCodeInvalidUniqueKeyDefinition, appErr.Code, appErr.Message)
	}
}

// TestExportModel_UnsupportedConverter_400 verifies that ExportModel rejects
// a converter value that is not JSON_SCHEMA or SIMPLE_VIEW with a 400
// BAD_REQUEST operational error. The OpenAPI enum gate (route-matcher rejects
// out-of-enum values at the HTTP layer) makes this path unreachable in
// production, but the domain layer still guards it defensively.
func TestExportModel_UnsupportedConverter_400(t *testing.T) {
	ref := spi.ModelRef{EntityName: "Dataset", ModelVersion: "1"}
	desc := &spi.ModelDescriptor{
		Ref:    ref,
		State:  spi.ModelLocked,
		Schema: mustBuildSchema(t),
	}
	ms := &refreshingModelStore{getDescriptor: desc}
	h := model.New(&fakeStoreFactory{modelStore: ms})

	_, err := h.ExportModel(context.Background(), "Dataset", "1", "SAMPLE_DATA")
	if err == nil {
		t.Fatal("ExportModel with unsupported converter: expected error, got nil")
	}
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *common.AppError, got %T: %v", err, err)
	}
	if appErr.Status != 400 {
		t.Errorf("expected HTTP 400, got %d: %s", appErr.Status, appErr.Message)
	}
	if appErr.Code != common.ErrCodeBadRequest {
		t.Errorf("expected code %q, got %q (message: %s)",
			common.ErrCodeBadRequest, appErr.Code, appErr.Message)
	}
}
