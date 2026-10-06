package entity_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/consistency"
	"github.com/cyoda-platform/cyoda-go/internal/domain/entity"
)

// neverReadStore embeds a nil EntityStore: any read through it panics, which
// is the assertion that a refused request never reached the backend.
type neverReadStore struct{ spi.EntityStore }

func groupedStatsFence(t *testing.T) (*entity.GroupedStatsService, context.Context, time.Time) {
	t.Helper()
	f := newFenceFixture(t, "fence-grouped", false)
	return entity.NewGroupedStatsService(100, consistency.New(f.tm)), f.ctx, f.c
}

func TestGroupedStats_FenceRefusesLaterInstant(t *testing.T) {
	svc, ctx, c := groupedStatsFence(t)
	later := c.Add(time.Millisecond)
	req := &entity.ValidatedGroupedStatsRequest{
		GroupBy:     []entity.GroupExprValidated{{IsState: true}},
		PointInTime: &later,
	}
	_, err := svc.QueryGroupedStats(ctx, neverReadStore{}, spi.ModelRef{}, nil, req)
	wantAppErr(t, err, http.StatusBadRequest, common.ErrCodePointInTimeAfterConsistencyTime)

	at := c
	req.PointInTime = &at
	buckets, err := svc.QueryGroupedStats(ctx, &fakeIterable{}, spi.ModelRef{}, nil, req)
	if err != nil {
		t.Fatalf("QueryGroupedStats at C: %v", err)
	}
	if len(buckets) != 0 {
		t.Errorf("buckets = %+v, want none", buckets)
	}
}

func TestGroupedStats_PathErrorBeatsFence(t *testing.T) {
	svc, ctx, c := groupedStatsFence(t)
	later := c.Add(time.Millisecond)
	req := &entity.ValidatedGroupedStatsRequest{
		GroupBy:     []entity.GroupExprValidated{{IsState: true}},
		Condition:   []byte(`{"type":"simple","jsonPath":"not a path","operatorType":"EQUALS","value":"x"}`),
		PointInTime: &later,
	}
	_, err := svc.QueryGroupedStats(ctx, neverReadStore{}, spi.ModelRef{}, nil, req)
	wantAppErr(t, err, http.StatusBadRequest, common.ErrCodeInvalidFieldPath)
}
