package entity_test

import (
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/entity"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
)

func mustPanic(t *testing.T, name string, f func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatalf("%s with a nil consistency service did not panic", name)
		}
	}()
	f()
}

func TestNew_NilConsistencyPanics(t *testing.T) {
	mustPanic(t, "entity.New", func() {
		entity.New(nil, nil, common.NewDefaultUUIDGenerator(), nil, txgate.New(), nil)
	})
}

func TestNewGroupedStatsService_NilConsistencyPanics(t *testing.T) {
	mustPanic(t, "entity.NewGroupedStatsService", func() { entity.NewGroupedStatsService(10, nil) })
}

func TestNewGroupedStatsHandler_NilConsistencyPanics(t *testing.T) {
	mustPanic(t, "entity.NewGroupedStatsHandler", func() { entity.NewGroupedStatsHandler(nil, 10, nil) })
}
