package entity_test

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/consistency"
	"github.com/cyoda-platform/cyoda-go/internal/domain/entity"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// fenceTM wraps a real transaction manager: ConsistencyTime answers the
// instant (or error) the test chose, and Begin is counted so a test can show
// that no transaction was opened.
type fenceTM struct {
	spi.TransactionManager
	at     time.Time
	err    error
	begins atomic.Int64
}

func (f *fenceTM) ConsistencyTime(context.Context) (time.Time, error) { return f.at, f.err }

func (f *fenceTM) Begin(ctx context.Context) (string, context.Context, error) {
	f.begins.Add(1)
	return f.TransactionManager.Begin(ctx)
}

type fenceFixture struct {
	ctx     context.Context
	h       *entity.Handler
	factory *memory.StoreFactory
	tm      *fenceTM
	ref     spi.ModelRef
	c       time.Time
}

// newFenceFixture builds a handler whose consistency time is a fixed instant
// C. When withModel is set, the model is registered and locked.
func newFenceFixture(t *testing.T, tenant spi.TenantID, withModel bool) *fenceFixture {
	t.Helper()
	factory := memory.NewStoreFactory()
	ctx := spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID: "fence-user",
		Tenant: spi.Tenant{ID: tenant, Name: string(tenant)},
		Roles:  []string{"USER"},
	})
	realTM, err := factory.TransactionManager(ctx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	c := time.Now().UTC().Add(time.Hour).Truncate(time.Millisecond)
	tm := &fenceTM{TransactionManager: realTM, at: c}
	ref := spi.ModelRef{EntityName: "fence-model", ModelVersion: "1"}
	if withModel {
		ms, err := factory.ModelStore(ctx)
		if err != nil {
			t.Fatalf("ModelStore: %v", err)
		}
		if err := ms.Save(ctx, &spi.ModelDescriptor{Ref: ref, State: spi.ModelLocked}); err != nil {
			t.Fatalf("ModelStore.Save: %v", err)
		}
	}
	h := entity.New(factory, tm, common.NewDefaultUUIDGenerator(), nil, txgate.New(), consistency.New(tm))
	return &fenceFixture{ctx: ctx, h: h, factory: factory, tm: tm, ref: ref, c: c}
}

func (f *fenceFixture) save(t *testing.T, id, state string) {
	t.Helper()
	es, err := f.factory.EntityStore(f.ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	if _, err := es.Save(f.ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: id, TenantID: spi.GetUserContext(f.ctx).Tenant.ID, ModelRef: f.ref, State: state},
		Data: []byte(`{}`),
	}); err != nil {
		t.Fatalf("Save %s: %v", id, err)
	}
}

func (f *fenceFixture) after() *time.Time { t := f.c.Add(time.Millisecond); return &t }
func (f *fenceFixture) at() *time.Time    { t := f.c; return &t }

func wantAppErr(t *testing.T, err error, status int, code string) {
	t.Helper()
	var ae *common.AppError
	if !errors.As(err, &ae) {
		t.Fatalf("want AppError %d %s, got %v", status, code, err)
	}
	if ae.Status != status || ae.Code != code {
		t.Fatalf("got %d %s (%s), want %d %s", ae.Status, ae.Code, ae.Message, status, code)
	}
}

func wantRefused(t *testing.T, err error) {
	t.Helper()
	wantAppErr(t, err, http.StatusBadRequest, common.ErrCodePointInTimeAfterConsistencyTime)
}

func TestFence_GetEntity(t *testing.T) {
	f := newFenceFixture(t, "fence-get", true)
	f.save(t, "e1", "NEW")

	_, err := f.h.GetEntity(f.ctx, entity.GetOneEntityInput{EntityID: "e1", PointInTime: f.after()})
	wantRefused(t, err)

	env, err := f.h.GetEntity(f.ctx, entity.GetOneEntityInput{EntityID: "e1", PointInTime: f.at()})
	if err != nil {
		t.Fatalf("GetEntity at C: %v", err)
	}
	if env.Meta["id"] != "e1" {
		t.Errorf("id = %v", env.Meta["id"])
	}
}

func TestFence_GetEntity_MissingEntityIsRefusedFirst(t *testing.T) {
	f := newFenceFixture(t, "fence-get-missing", true)
	_, err := f.h.GetEntity(f.ctx, entity.GetOneEntityInput{EntityID: "no-such-id", PointInTime: f.after()})
	wantRefused(t, err)

	_, err = f.h.GetEntity(f.ctx, entity.GetOneEntityInput{EntityID: "no-such-id", PointInTime: f.at()})
	wantAppErr(t, err, http.StatusNotFound, common.ErrCodeEntityNotFound)
}

func TestFence_ListEntities_PageSizeZero(t *testing.T) {
	f := newFenceFixture(t, "fence-list", true)
	f.save(t, "e1", "NEW")

	_, err := f.h.ListEntities(f.ctx, f.ref.EntityName, f.ref.ModelVersion, entity.PaginationParams{PageSize: 0}, f.after())
	wantRefused(t, err)
	_, err = f.h.ListEntities(f.ctx, f.ref.EntityName, f.ref.ModelVersion, entity.PaginationParams{PageSize: 10}, f.after())
	wantRefused(t, err)

	envs, err := f.h.ListEntities(f.ctx, f.ref.EntityName, f.ref.ModelVersion, entity.PaginationParams{PageSize: 10}, f.at())
	if err != nil {
		t.Fatalf("ListEntities at C: %v", err)
	}
	if len(envs) != 1 {
		t.Errorf("got %d envelopes, want 1", len(envs))
	}
}

func TestFence_GetChangesMetadata(t *testing.T) {
	f := newFenceFixture(t, "fence-changes", true)
	f.save(t, "e1", "NEW")

	_, err := f.h.GetChangesMetadata(f.ctx, "e1", f.after())
	wantRefused(t, err)
	_, err = f.h.GetChangesMetadata(f.ctx, "no-such-id", f.after())
	wantRefused(t, err)

	entries, err := f.h.GetChangesMetadata(f.ctx, "e1", f.at())
	if err != nil {
		t.Fatalf("GetChangesMetadata at C: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("got %d entries, want 1", len(entries))
	}
}

func TestFence_Delete_ModelNotFoundBeforeFence(t *testing.T) {
	f := newFenceFixture(t, "fence-del-404", false)
	for _, batch := range []int{0, 10} {
		_, err := f.h.DeleteEntitiesConditional(f.ctx, "unknown-model", 1, nil, f.after(), false, batch)
		wantAppErr(t, err, http.StatusNotFound, common.ErrCodeModelNotFound)
	}
}

func TestFence_Delete_BothPaths(t *testing.T) {
	f := newFenceFixture(t, "fence-del", true)
	f.save(t, "e1", "NEW")
	for _, batch := range []int{0, 10} {
		_, err := f.h.DeleteEntitiesConditional(f.ctx, f.ref.EntityName, 1, nil, f.after(), false, batch)
		wantRefused(t, err)
	}
	if n := f.tm.begins.Load(); n != 0 {
		t.Errorf("a transaction was begun %d times before the fence refused", n)
	}
	es, _ := f.factory.EntityStore(f.ctx)
	if n, err := es.Count(f.ctx, f.ref, nil); err != nil || n != 1 {
		t.Errorf("count = %d, %v; want 1", n, err)
	}
}

func TestFence_Stats_AllVariants(t *testing.T) {
	f := newFenceFixture(t, "fence-stats", true)
	name, ver := f.ref.EntityName, f.ref.ModelVersion

	_, err := f.h.GetStatistics(f.ctx, f.after())
	wantRefused(t, err)
	_, err = f.h.GetStatisticsByState(f.ctx, nil, f.after())
	wantRefused(t, err)
	_, err = f.h.GetStatisticsForModel(f.ctx, name, ver, f.after())
	wantRefused(t, err)
	_, err = f.h.GetStatisticsByStateForModel(f.ctx, name, ver, nil, f.after())
	wantRefused(t, err)

	// The fence runs even when the tenant has no models.
	empty := newFenceFixture(t, "fence-stats-empty", false)
	_, err = empty.h.GetStatistics(empty.ctx, empty.after())
	wantRefused(t, err)
	_, err = empty.h.GetStatisticsByState(empty.ctx, nil, empty.after())
	wantRefused(t, err)

	// An unknown model keeps its 404 ahead of the fence in the ForModel variants.
	_, err = empty.h.GetStatisticsForModel(empty.ctx, "nope", "1", empty.after())
	wantAppErr(t, err, http.StatusNotFound, common.ErrCodeModelNotFound)
}

func TestFence_Stats_CountsAsAtT(t *testing.T) {
	f := newFenceFixture(t, "fence-stats-asat", true)
	name, ver := f.ref.EntityName, f.ref.ModelVersion
	f.save(t, "e1", "NEW")
	time.Sleep(5 * time.Millisecond)
	mid := time.Now().UTC()
	time.Sleep(5 * time.Millisecond)
	f.save(t, "e2", "NEW")

	stat, err := f.h.GetStatisticsForModel(f.ctx, name, ver, &mid)
	if err != nil {
		t.Fatalf("GetStatisticsForModel: %v", err)
	}
	if stat.Count != 1 {
		t.Errorf("count at mid = %d, want 1", stat.Count)
	}
	stats, err := f.h.GetStatistics(f.ctx, &mid)
	if err != nil || len(stats) != 1 || stats[0].Count != 1 {
		t.Errorf("GetStatistics at mid = %v, %v; want one model with count 1", stats, err)
	}
	byState, err := f.h.GetStatisticsByStateForModel(f.ctx, name, ver, nil, &mid)
	if err != nil || len(byState) != 1 || byState[0].Count != 1 {
		t.Errorf("GetStatisticsByStateForModel at mid = %v, %v; want NEW=1", byState, err)
	}
	all, err := f.h.GetStatisticsByState(f.ctx, nil, &mid)
	if err != nil || len(all) != 1 || all[0].Count != 1 {
		t.Errorf("GetStatisticsByState at mid = %v, %v; want NEW=1", all, err)
	}
	now, err := f.h.GetStatisticsForModel(f.ctx, name, ver, nil)
	if err != nil || now.Count != 2 {
		t.Errorf("count now = %v, %v; want 2", now, err)
	}
}

func TestFence_PropagatesUnavailable(t *testing.T) {
	f := newFenceFixture(t, "fence-unavail", true)
	f.save(t, "e1", "NEW")
	f.tm.err = spi.ErrConsistencyTimeUnavailable
	p := f.after()
	name, ver := f.ref.EntityName, f.ref.ModelVersion
	calls := map[string]func() error{
		"GetEntity": func() error {
			_, err := f.h.GetEntity(f.ctx, entity.GetOneEntityInput{EntityID: "e1", PointInTime: p})
			return err
		},
		"ListEntities": func() error {
			_, err := f.h.ListEntities(f.ctx, name, ver, entity.PaginationParams{PageSize: 1}, p)
			return err
		},
		"GetChangesMetadata": func() error { _, err := f.h.GetChangesMetadata(f.ctx, "e1", p); return err },
		"Delete": func() error {
			_, err := f.h.DeleteEntitiesConditional(f.ctx, name, 1, nil, p, false, 0)
			return err
		},
		"DeleteBatched": func() error {
			_, err := f.h.DeleteEntitiesConditional(f.ctx, name, 1, nil, p, false, 10)
			return err
		},
		"GetStatistics": func() error { _, err := f.h.GetStatistics(f.ctx, p); return err },
		"GetStatisticsByState": func() error {
			_, err := f.h.GetStatisticsByState(f.ctx, nil, p)
			return err
		},
		"GetStatisticsForModel": func() error { _, err := f.h.GetStatisticsForModel(f.ctx, name, ver, p); return err },
		"GetStatisticsByStateForModel": func() error {
			_, err := f.h.GetStatisticsByStateForModel(f.ctx, name, ver, nil, p)
			return err
		},
	}
	for label, call := range calls {
		t.Run(label, func(t *testing.T) {
			wantAppErr(t, call(), http.StatusServiceUnavailable, common.ErrCodeConsistencyTimeUnavailable)
		})
	}
}

func TestFence_GetChangesMetadata_ZeroInstantIsAnInstant(t *testing.T) {
	f := newFenceFixture(t, "fence-changes-zero", true)
	f.save(t, "e1", "NEW")

	var zero time.Time
	entries, err := f.h.GetChangesMetadata(f.ctx, "e1", &zero)
	if err != nil {
		t.Fatalf("GetChangesMetadata at the zero instant: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("history up to the zero instant has %d entries, want 0", len(entries))
	}
}

func TestFence_Stats_UnknownModelByStateForModel404BeforeFence(t *testing.T) {
	f := newFenceFixture(t, "fence-stats-bystate-404", false)
	_, err := f.h.GetStatisticsByStateForModel(f.ctx, "nope", "1", nil, f.after())
	wantAppErr(t, err, http.StatusNotFound, common.ErrCodeModelNotFound)
}

func TestFence_Stats_StatesFilterWithPointInTime(t *testing.T) {
	f := newFenceFixture(t, "fence-stats-filter", true)
	name, ver := f.ref.EntityName, f.ref.ModelVersion
	f.save(t, "e1", "NEW")
	f.save(t, "e2", "APPROVED")
	time.Sleep(5 * time.Millisecond)
	mid := time.Now().UTC()
	time.Sleep(5 * time.Millisecond)
	f.save(t, "e3", "NEW")
	filter := []string{"NEW"}

	got, err := f.h.GetStatisticsByStateForModel(f.ctx, name, ver, &filter, &mid)
	if err != nil || len(got) != 1 || got[0].State != "NEW" || got[0].Count != 1 {
		t.Errorf("ByStateForModel(NEW) at mid = %v, %v; want NEW=1", got, err)
	}
	all, err := f.h.GetStatisticsByState(f.ctx, &filter, &mid)
	if err != nil || len(all) != 1 || all[0].State != "NEW" || all[0].Count != 1 {
		t.Errorf("ByState(NEW) at mid = %v, %v; want NEW=1", all, err)
	}
	_, err = f.h.GetStatisticsByStateForModel(f.ctx, name, ver, &filter, f.after())
	wantRefused(t, err)
}
