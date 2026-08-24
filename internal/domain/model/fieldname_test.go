package model_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model"
)

// TestImportModel_RejectsUnaddressableFieldName pins the explicit-import half
// of the field-name rule. Sample data is the primary way a model's field set is
// established, so a key the wire jsonPath grammar cannot address must be
// refused here — recording it would guarantee a field that can be written and
// never queried.
//
// The response follows the import-time validation precedent
// (400 VALIDATION_FAILED with the offending location in the detail) rather than
// the generic parse-failure BAD_REQUEST, because the body parsed fine: it is
// the content that violates a documented rule.
func TestImportModel_RejectsUnaddressableFieldName(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"non-ascii", `{"café": 5}`, `"café"`},
		{"space", `{"first name": "x"}`, `"first name"`},
		{"dot in key", `{"first.name": 1}`, `"first.name"`},
		{"empty key", `{"": 1}`, `""`},
		{"nested", `{"ok": {"bad name": 1}}`, `"bad name"`},
		{"in array element", `{"arr": [{"bad name": 1}]}`, `"bad name"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := &refreshingModelStore{}
			h := model.New(&fakeStoreFactory{modelStore: store})

			_, err := h.ImportModel(context.Background(), model.ImportModelInput{
				EntityName:   "fieldname",
				ModelVersion: "1",
				Format:       "JSON",
				Converter:    "SAMPLE_DATA",
				Data:         []byte(c.body),
			})
			if err == nil {
				t.Fatalf("ImportModel(%s) must be rejected", c.body)
			}

			var appErr *common.AppError
			if !errors.As(err, &appErr) {
				t.Fatalf("error must be *common.AppError, got %T: %v", err, err)
			}
			if appErr.Status != http.StatusBadRequest {
				t.Errorf("status = %d, want %d", appErr.Status, http.StatusBadRequest)
			}
			if appErr.Code != common.ErrCodeValidationFailed {
				t.Errorf("code = %q, want %q", appErr.Code, common.ErrCodeValidationFailed)
			}
			if !strings.Contains(appErr.Message, c.want) {
				t.Errorf("message must name the offending field %s, got %q", c.want, appErr.Message)
			}
			if appErr.Props["entityName"] != "fieldname" {
				t.Errorf("props must identify the model, got %v", appErr.Props)
			}
			if store.saveCount != 0 {
				t.Errorf("a rejected import must not persist a descriptor (saveCount=%d)", store.saveCount)
			}
		})
	}
}

// TestImportModel_AcceptsAddressableFieldName is the positive control: the
// whole legal segment charset, plus nested containers, still imports.
func TestImportModel_AcceptsAddressableFieldName(t *testing.T) {
	const body = `{"_":1,"-":2,"_meta":3,"first-name":"x","first_name":"y","camelCase":1,` +
		`"UPPER":1,"0abc":1,"x_1-2A":1,"nested":{"b-c":[{"d_1":1}]},"tags":["a"]}`

	store := &refreshingModelStore{}
	h := model.New(&fakeStoreFactory{modelStore: store})

	if _, err := h.ImportModel(context.Background(), model.ImportModelInput{
		EntityName:   "fieldname",
		ModelVersion: "1",
		Format:       "JSON",
		Converter:    "SAMPLE_DATA",
		Data:         []byte(body),
	}); err != nil {
		t.Fatalf("ImportModel must accept addressable field names, got %v", err)
	}
	if store.saveCount != 1 {
		t.Fatalf("expected the model to be saved once, got %d", store.saveCount)
	}
	if store.saved == nil || len(store.saved.Schema) == 0 {
		t.Fatal("expected a non-empty schema to be persisted")
	}
}

// TestImportModel_LegacyModelReimportRejected documents the upgrade path for a
// deployment that already holds a model with an unaddressable field: the model
// keeps working for reads and exports, but re-importing the sample data that
// produced it now fails, naming the key to rename. That is the intended,
// approved consequence — the field set may not be re-established in a shape the
// query surface cannot address.
func TestImportModel_LegacyModelReimportRejected(t *testing.T) {
	store := &refreshingModelStore{
		getDescriptor: &spi.ModelDescriptor{
			Ref:   spi.ModelRef{EntityName: "legacy", ModelVersion: "1"},
			State: spi.ModelUnlocked,
		},
	}
	store.refreshDescriptor = store.getDescriptor
	h := model.New(&fakeStoreFactory{modelStore: store})

	_, err := h.ImportModel(context.Background(), model.ImportModelInput{
		EntityName:   "legacy",
		ModelVersion: "1",
		Format:       "JSON",
		Converter:    "SAMPLE_DATA",
		Data:         []byte(`{"first name": "x"}`),
	})
	var appErr *common.AppError
	if !errors.As(err, &appErr) || appErr.Code != common.ErrCodeValidationFailed {
		t.Fatalf("re-import of a legacy unaddressable field must fail VALIDATION_FAILED, got %v", err)
	}
	if store.saveCount != 0 {
		t.Errorf("a rejected re-import must not overwrite the stored descriptor")
	}
}
