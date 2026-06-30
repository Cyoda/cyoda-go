package entity

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

// recordingModelStore counts ExtendSchema and Save calls. Get returns the
// stored descriptor. Everything else is no-op or stub as the test needs.
type recordingModelStore struct {
	mu          sync.Mutex
	extendCalls int
	saveCalls   int
	lastDelta   spi.SchemaDelta
	descriptor  *spi.ModelDescriptor
}

func (s *recordingModelStore) Save(_ context.Context, d *spi.ModelDescriptor) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saveCalls++
	s.descriptor = d
	return nil
}
func (s *recordingModelStore) Get(_ context.Context, _ spi.ModelRef) (*spi.ModelDescriptor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.descriptor, nil
}
func (s *recordingModelStore) GetAll(context.Context) ([]spi.ModelRef, error)       { return nil, nil }
func (s *recordingModelStore) Delete(context.Context, spi.ModelRef) error           { return nil }
func (s *recordingModelStore) Lock(context.Context, spi.ModelRef) error             { return nil }
func (s *recordingModelStore) Unlock(context.Context, spi.ModelRef) error           { return nil }
func (s *recordingModelStore) IsLocked(context.Context, spi.ModelRef) (bool, error) { return true, nil }
func (s *recordingModelStore) SetChangeLevel(context.Context, spi.ModelRef, spi.ChangeLevel) error {
	return nil
}
func (s *recordingModelStore) ExtendSchema(_ context.Context, _ spi.ModelRef, delta spi.SchemaDelta) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.extendCalls++
	s.lastDelta = delta
	return nil
}

// Compile-time check that recordingModelStore satisfies the SPI contract.
var _ spi.ModelStore = (*recordingModelStore)(nil)

// descriptorWithChangeLevel builds a LOCKED descriptor with the given
// ChangeLevel and schema bytes derived from the given ModelNode.
func descriptorWithChangeLevel(t *testing.T, node *schema.ModelNode, cl spi.ChangeLevel) *spi.ModelDescriptor {
	t.Helper()
	raw, err := schema.Marshal(node)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return &spi.ModelDescriptor{
		Ref:         spi.ModelRef{EntityName: "Book", ModelVersion: "1"},
		State:       spi.ModelLocked,
		ChangeLevel: cl,
		Schema:      raw,
		UpdateDate:  time.Now().UTC(),
	}
}

func TestValidateOrExtend_NoChangeLevel_ValidatesOnly(t *testing.T) {
	h := &Handler{}
	node := schema.NewObjectNode()
	node.SetChild("name", schema.NewLeafNode(schema.String))
	desc := descriptorWithChangeLevel(t, node, spi.ChangeLevel(""))
	ms := &recordingModelStore{descriptor: desc}

	var data any
	if err := json.Unmarshal([]byte(`{"name":"alice"}`), &data); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if err := h.validateOrExtend(context.Background(), ms, desc, data); err != nil {
		t.Fatalf("validateOrExtend: %v", err)
	}
	if ms.extendCalls != 0 || ms.saveCalls != 0 {
		t.Errorf("strict-validate path must not call ExtendSchema or Save; got extend=%d save=%d",
			ms.extendCalls, ms.saveCalls)
	}
}

func TestValidateOrExtend_NoChangeLevel_UnknownField_Fails(t *testing.T) {
	h := &Handler{}
	node := schema.NewObjectNode()
	node.SetChild("name", schema.NewLeafNode(schema.String))
	desc := descriptorWithChangeLevel(t, node, spi.ChangeLevel(""))
	ms := &recordingModelStore{descriptor: desc}

	var data any
	if err := json.Unmarshal([]byte(`{"name":"a","email":"b@c.d"}`), &data); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	err := h.validateOrExtend(context.Background(), ms, desc, data)
	if err == nil || !strings.Contains(err.Error(), "validation failed") {
		t.Errorf("expected validation failure, got %v", err)
	}
}

func TestValidateOrExtend_ChangeLevel_NoDelta_NoExtendCall(t *testing.T) {
	h := &Handler{}
	node := schema.NewObjectNode()
	node.SetChild("name", schema.NewLeafNode(schema.String))
	desc := descriptorWithChangeLevel(t, node, spi.ChangeLevelStructural)
	ms := &recordingModelStore{descriptor: desc}

	// Data uses only fields already in the schema — Diff should be nil.
	var data any
	if err := json.Unmarshal([]byte(`{"name":"alice"}`), &data); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if err := h.validateOrExtend(context.Background(), ms, desc, data); err != nil {
		t.Fatalf("validateOrExtend: %v", err)
	}
	if ms.extendCalls != 0 {
		t.Errorf("expected 0 ExtendSchema calls on no-op diff, got %d", ms.extendCalls)
	}
	if ms.saveCalls != 0 {
		t.Errorf("expected 0 Save calls; got %d", ms.saveCalls)
	}
}

func TestValidateOrExtend_ChangeLevel_NewField_CallsExtendSchema(t *testing.T) {
	h := &Handler{}
	node := schema.NewObjectNode()
	node.SetChild("name", schema.NewLeafNode(schema.String))
	desc := descriptorWithChangeLevel(t, node, spi.ChangeLevelStructural)
	ms := &recordingModelStore{descriptor: desc}

	var data any
	if err := json.Unmarshal([]byte(`{"name":"alice","email":"a@b.c"}`), &data); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if err := h.validateOrExtend(context.Background(), ms, desc, data); err != nil {
		t.Fatalf("validateOrExtend: %v", err)
	}
	if ms.extendCalls != 1 {
		t.Errorf("expected exactly 1 ExtendSchema call, got %d", ms.extendCalls)
	}
	if ms.saveCalls != 0 {
		t.Errorf("expected 0 Save calls (hot-row regression), got %d", ms.saveCalls)
	}
	if !bytes.Contains(ms.lastDelta, []byte("add_property")) {
		t.Errorf("expected delta to contain add_property op; got %s", ms.lastDelta)
	}
}

// descriptorWithChangeLevelAndKeys builds a LOCKED descriptor with the given
// ChangeLevel, schema, and unique keys.
func descriptorWithChangeLevelAndKeys(t *testing.T, node *schema.ModelNode, cl spi.ChangeLevel, keys []spi.UniqueKey) *spi.ModelDescriptor {
	t.Helper()
	raw, err := schema.Marshal(node)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return &spi.ModelDescriptor{
		Ref:         spi.ModelRef{EntityName: "Score", ModelVersion: "1"},
		State:       spi.ModelLocked,
		ChangeLevel: cl,
		Schema:      raw,
		UniqueKeys:  keys,
		UpdateDate:  time.Now().UTC(),
	}
}

// TestValidateOrExtend_WideningKeyedNullLeaf_Returns422 verifies that when a
// model has a null-only scalar leaf under a unique key, and an entity write
// would widen that field to an object (a TYPE-level change permitted by
// Structural ChangeLevel), validateOrExtend rejects the write with 422
// INVALID_UNIQUE_KEY_DEFINITION rather than silently widening the schema.
func TestValidateOrExtend_WideningKeyedNullLeaf_Returns422(t *testing.T) {
	h := &Handler{}

	// score is initially a null-only leaf (nullable marker).
	// A unique key on $.score is valid at declaration time (it is a scalar leaf).
	node := schema.NewObjectNode()
	node.SetChild("score", schema.NewLeafNode(schema.Null))
	desc := descriptorWithChangeLevelAndKeys(t, node, spi.ChangeLevelStructural, []spi.UniqueKey{
		{ID: "uk-score", Fields: []string{"$.score"}},
	})
	ms := &recordingModelStore{descriptor: desc}

	// Incoming entity widens $.score from leaf[NULL] to object.
	// Use a Go map directly so that we avoid float64 vs json.Number parsing
	// issues — importer.Walk handles map[string]any natively.
	data := map[string]any{
		"score": map[string]any{"sub": "val"},
	}
	err := h.validateOrExtend(context.Background(), ms, desc, data)
	if err == nil {
		t.Fatal("expected error for key-field widening, got nil")
	}
	appErr := classifyValidateOrExtendErr(err)
	if appErr.Status != http.StatusUnprocessableEntity {
		t.Errorf("status: got %d want %d", appErr.Status, http.StatusUnprocessableEntity)
	}
	if appErr.Code != common.ErrCodeInvalidUniqueKeyDefinition {
		t.Errorf("code: got %q want %q", appErr.Code, common.ErrCodeInvalidUniqueKeyDefinition)
	}
	if ms.extendCalls != 0 {
		t.Errorf("ExtendSchema must not be called when write is rejected; got %d calls", ms.extendCalls)
	}
}

// TestValidateOrExtend_AddNewField_NotTouchingKeyedField_Succeeds verifies that
// a normal additive extension (new field added, keyed field untouched) is
// accepted and ExtendSchema is called exactly once.
func TestValidateOrExtend_AddNewField_NotTouchingKeyedField_Succeeds(t *testing.T) {
	h := &Handler{}

	node := schema.NewObjectNode()
	node.SetChild("score", schema.NewLeafNode(schema.Null))
	desc := descriptorWithChangeLevelAndKeys(t, node, spi.ChangeLevelStructural, []spi.UniqueKey{
		{ID: "uk-score", Fields: []string{"$.score"}},
	})
	ms := &recordingModelStore{descriptor: desc}

	// Incoming entity keeps score as null (scalar leaf unchanged) and adds a
	// brand-new string field — a pure structural extension that does not touch
	// the keyed field.
	data := map[string]any{
		"score":    nil,
		"category": "sports",
	}
	if err := h.validateOrExtend(context.Background(), ms, desc, data); err != nil {
		t.Fatalf("validateOrExtend unexpected error: %v", err)
	}
	if ms.extendCalls != 1 {
		t.Errorf("expected 1 ExtendSchema call for the new field; got %d", ms.extendCalls)
	}
}
