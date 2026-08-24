package ingest

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

// descWithChangeLevel builds a descriptor whose schema is an empty object and
// whose ChangeLevel permits structural growth — the configuration under which
// an entity write is allowed to ESTABLISH new fields, and therefore the one
// where an unaddressable field name must be refused.
func descWithChangeLevel(t *testing.T) *spi.ModelDescriptor {
	t.Helper()
	b, err := schema.Marshal(schema.NewObjectNode())
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return &spi.ModelDescriptor{
		Ref:         spi.ModelRef{EntityName: "fieldname", ModelVersion: "1"},
		Schema:      b,
		ChangeLevel: spi.ChangeLevelStructural,
	}
}

// TestValidateOrExtend_RejectsUnaddressableFieldName pins the auto-evolution
// half of the rule. An entity write is the OTHER way a model's field set grows,
// so it must refuse the same names the explicit import refuses — and it must do
// so with a diagnosable operational error, not the generic catch-all, since the
// caller's only remedy is to rename a key it can see named in the response.
func TestValidateOrExtend_RejectsUnaddressableFieldName(t *testing.T) {
	err := ValidateOrExtend(context.Background(), &recordingModelStore{}, descWithChangeLevel(t),
		map[string]any{"first name": "x"})
	if err == nil {
		t.Fatal("ValidateOrExtend must reject an unaddressable field name")
	}

	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("error must be a pre-classified *common.AppError, got %T: %v", err, err)
	}
	if appErr.Status != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", appErr.Status, http.StatusBadRequest)
	}
	if appErr.Code != common.ErrCodeValidationFailed {
		t.Errorf("code = %q, want %q", appErr.Code, common.ErrCodeValidationFailed)
	}
	if !strings.Contains(appErr.Message, `"first name"`) {
		t.Errorf("message must name the offending field, got %q", appErr.Message)
	}
	// A change-level violation is the other reason a write is refused here and
	// has a completely different remedy; the two must not read alike.
	if strings.Contains(appErr.Message, "change level") {
		t.Errorf("field-name rejection must not wear the change-level prefix: %q", appErr.Message)
	}
}

// recordingModelStore is a ModelStore that only implements what
// ValidateOrExtend touches: the ExtendSchema append. Every other method is a
// no-op so the fake stays a fake.
type recordingModelStore struct{ deltas []spi.SchemaDelta }

func (s *recordingModelStore) Save(context.Context, *spi.ModelDescriptor) error { return nil }
func (s *recordingModelStore) Get(context.Context, spi.ModelRef) (*spi.ModelDescriptor, error) {
	return nil, nil
}
func (s *recordingModelStore) GetAll(context.Context) ([]spi.ModelRef, error) { return nil, nil }
func (s *recordingModelStore) Delete(context.Context, spi.ModelRef) error     { return nil }
func (s *recordingModelStore) Lock(context.Context, spi.ModelRef) error       { return nil }
func (s *recordingModelStore) Unlock(context.Context, spi.ModelRef) error     { return nil }
func (s *recordingModelStore) IsLocked(context.Context, spi.ModelRef) (bool, error) {
	return false, nil
}
func (s *recordingModelStore) SetChangeLevel(context.Context, spi.ModelRef, spi.ChangeLevel) error {
	return nil
}
func (s *recordingModelStore) ExtendSchema(_ context.Context, _ spi.ModelRef, d spi.SchemaDelta) error {
	s.deltas = append(s.deltas, d)
	return nil
}

// TestValidateOrExtend_AcceptsAddressableFieldName is the positive control on
// the same door: an addressable new field still extends the schema, and the
// delta actually reaches the store.
func TestValidateOrExtend_AcceptsAddressableFieldName(t *testing.T) {
	store := &recordingModelStore{}
	if err := ValidateOrExtend(context.Background(), store, descWithChangeLevel(t),
		map[string]any{"first_name": "x", "last-name": "y", "_meta9": "z"}); err != nil {
		t.Fatalf("addressable field names must extend the schema, got %v", err)
	}
	if len(store.deltas) != 1 {
		t.Fatalf("expected one schema delta, got %d", len(store.deltas))
	}
}

// TestValidateOrExtend_LegacyFieldWithoutChangeLevel documents the upgrade
// path: a model stored before the rule may already carry an unaddressable
// field. With no ChangeLevel the write does not walk the payload at all — it
// validates against the stored schema — so such an entity remains writable
// (and, as before, unqueryable). The rule bounds the field set going forward;
// it does not retroactively lock existing tenants out of their data.
func TestValidateOrExtend_LegacyFieldWithoutChangeLevel(t *testing.T) {
	legacy := schema.NewObjectNode()
	legacy.SetChild("first name", schema.NewLeafNode(schema.String))
	b, err := schema.Marshal(legacy)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	desc := &spi.ModelDescriptor{
		Ref:    spi.ModelRef{EntityName: "legacy", ModelVersion: "1"},
		Schema: b,
	}
	if err := ValidateOrExtend(context.Background(), &recordingModelStore{}, desc,
		map[string]any{"first name": "x"}); err != nil {
		t.Fatalf("a legacy field must stay writable without a ChangeLevel, got %v", err)
	}
}
