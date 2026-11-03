package model_test

import (
	"context"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/domain/model"
	"github.com/cyoda-platform/cyoda-go/internal/testing/fakeclock"
)

func TestModelUpdateDateFromClock(t *testing.T) {
	at := time.Date(2015, 3, 1, 0, 0, 0, 0, time.UTC)
	ms := &refreshingModelStore{}
	h := model.New(&fakeStoreFactory{modelStore: ms}, fakeclock.New(at))

	if _, err := h.ImportModel(context.Background(), model.ImportModelInput{
		EntityName: "Dataset", ModelVersion: "1", Format: "JSON", Converter: "SAMPLE_DATA",
		Data: []byte(`{"name":"Alice"}`),
	}); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	saved := ms.Saved()
	if saved == nil {
		t.Fatal("ModelStore.Save was not called")
	}
	if !saved.UpdateDate.Equal(at) {
		t.Errorf("UpdateDate = %v, want %v", saved.UpdateDate, at)
	}
}
