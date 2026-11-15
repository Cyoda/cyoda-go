package model_test

import (
	"context"
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model"
	"github.com/cyoda-platform/cyoda-go/internal/platformclock"
)

func expectApp(t *testing.T, err error, status int, code string) *common.AppError {
	t.Helper()
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *common.AppError, got %T: %v", err, err)
	}
	if appErr.Status != status || appErr.Code != code {
		t.Fatalf("expected %d %s, got %d %s: %s", status, code, appErr.Status, appErr.Code, appErr.Message)
	}
	return appErr
}

func expectProps(t *testing.T, appErr *common.AppError, want map[string]any) {
	t.Helper()
	if len(appErr.Props) != len(want) {
		t.Errorf("props = %v, want %v", appErr.Props, want)
	}
	for k, v := range want {
		if appErr.Props[k] != v {
			t.Errorf("props[%s] = %v (%T), want %v (%T)", k, appErr.Props[k], appErr.Props[k], v, v)
		}
	}
}

var atomicRef = spi.ModelRef{EntityName: "Dataset", ModelVersion: "1"}

func TestUnlockModel_StoreReportsEntities_409ModelHasEntities(t *testing.T) {
	ms := &refreshingModelStore{
		refreshDescriptor: &spi.ModelDescriptor{Ref: atomicRef, State: spi.ModelLocked},
		unlockErr:         &spi.ModelHasEntitiesError{Ref: atomicRef, Count: 3},
	}
	h := model.New(&fakeStoreFactory{modelStore: ms}, platformclock.Real())
	_, err := h.UnlockModel(context.Background(), "Dataset", "1")
	appErr := expectApp(t, err, 409, common.ErrCodeModelHasEntities)
	expectProps(t, appErr, map[string]any{"entityName": "Dataset", "entityVersion": int32(1), "entityCount": int64(3)})
}

func TestDeleteModel_StoreReportsLocked_409ModelAlreadyLocked(t *testing.T) {
	ms := &refreshingModelStore{
		refreshDescriptor: &spi.ModelDescriptor{Ref: atomicRef, State: spi.ModelUnlocked},
		deleteErr:         spi.ErrModelLocked,
	}
	h := model.New(&fakeStoreFactory{modelStore: ms}, platformclock.Real())
	err := h.DeleteModel(context.Background(), "Dataset", "1")
	appErr := expectApp(t, err, 409, common.ErrCodeModelAlreadyLocked)
	expectProps(t, appErr, map[string]any{"entityName": "Dataset", "entityVersion": int32(1), "expectedState": "UNLOCKED", "actualState": "LOCKED"})
}

func TestSetUniqueKeys_StoreReportsLocked_409ModelAlreadyLocked(t *testing.T) {
	ms := &refreshingModelStore{
		refreshDescriptor: &spi.ModelDescriptor{Ref: atomicRef, State: spi.ModelUnlocked, Schema: mustBuildSchema(t)},
		saveErr:           spi.ErrModelLocked,
	}
	h := model.New(&capableStoreFactory{fakeStoreFactory: fakeStoreFactory{modelStore: ms}, supports: true}, platformclock.Real())
	_, err := h.SetUniqueKeys(context.Background(), "Dataset", "1", []spi.UniqueKey{{ID: "uk1", Fields: []string{"$.name"}}})
	appErr := expectApp(t, err, 409, common.ErrCodeModelAlreadyLocked)
	expectProps(t, appErr, map[string]any{"entityName": "Dataset", "entityVersion": int32(1), "expectedState": "UNLOCKED", "actualState": "LOCKED"})
}

func TestImportModel_StoreReportsLocked_409ModelAlreadyLocked(t *testing.T) {
	ms := &refreshingModelStore{saveErr: spi.ErrModelLocked}
	h := model.New(&fakeStoreFactory{modelStore: ms}, platformclock.Real())
	_, err := h.ImportModel(context.Background(), model.ImportModelInput{
		EntityName: "Dataset", ModelVersion: "1", Format: "JSON", Converter: "SAMPLE_DATA", Data: []byte(`{"a":1}`),
	})
	appErr := expectApp(t, err, 409, common.ErrCodeModelAlreadyLocked)
	expectProps(t, appErr, map[string]any{"entityName": "Dataset", "entityVersion": int32(1)})
}
