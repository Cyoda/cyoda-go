package memory_test

import (
	"context"
	"testing"
	"time"
	"unsafe"

	"github.com/stretchr/testify/require"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

func ctTenantCtx(tenant string) context.Context {
	return spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID: "u", Tenant: spi.Tenant{ID: spi.TenantID(tenant), Name: tenant},
	})
}

// C comes from the store's clock, not the process clock.
func TestConsistencyTime_ReadsTheStoreClock(t *testing.T) {
	ahead := time.Now().Add(time.Hour)
	f := memory.NewStoreFactory(memory.WithClock(memory.NewTestClockAt(ahead)))
	ctx := ctTenantCtx("t1")
	tm, err := f.TransactionManager(ctx)
	require.NoError(t, err)
	c, err := tm.ConsistencyTime(ctx)
	require.NoError(t, err)
	require.False(t, c.Before(ahead), "C %v must be at the store clock %v, not the process clock", c, ahead)
}

func TestConsistencyTime_RequiresTenant(t *testing.T) {
	f := memory.NewStoreFactory()
	tm, err := f.TransactionManager(ctTenantCtx("t1"))
	require.NoError(t, err)
	_, err = tm.ConsistencyTime(context.Background())
	require.Error(t, err)
}

// A commit after C is stamped strictly after C even under a frozen clock.
func TestConsistencyTime_ReservesTheFloor(t *testing.T) {
	clock := memory.NewTestClockAt(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	f := memory.NewStoreFactory(memory.WithClock(clock))
	ctx := ctTenantCtx("t1")
	tm, err := f.TransactionManager(ctx)
	require.NoError(t, err)
	c, err := tm.ConsistencyTime(ctx)
	require.NoError(t, err)
	txID, txCtx, err := tm.Begin(ctx)
	require.NoError(t, err)
	es, err := f.EntityStore(txCtx)
	require.NoError(t, err)
	_, err = es.Save(txCtx, &spi.Entity{Meta: spi.EntityMeta{ID: "e1", ModelRef: spi.ModelRef{EntityName: "m", ModelVersion: "1"}}, Data: []byte(`{}`)})
	require.NoError(t, err)
	require.NoError(t, tm.Commit(txCtx, txID))
	submit, err := tm.GetSubmitTime(ctx, txID)
	require.NoError(t, err)
	require.True(t, submit.After(c))
}

// steppingClock returns wall times that step back while keeping Go's
// monotonic reading moving forward, as time.Now does after an NTP step.
type steppingClock struct{ t time.Time }

// timeRepr mirrors the leading fields of time.Time (wall, ext, loc). When the
// time carries a monotonic reading, ext holds it. Test-only.
type timeRepr struct {
	wall uint64
	ext  int64
	loc  *time.Location
}

func (s *steppingClock) Now() time.Time { return s.t }

func TestConsistencyTime_FloorSurvivesWallClockStepBack(t *testing.T) {
	first := time.Now() // carries a monotonic reading
	sc := &steppingClock{t: first}
	f := memory.NewStoreFactory(memory.WithClock(sc))
	ctx := ctTenantCtx("t1")
	tm, err := f.TransactionManager(ctx)
	require.NoError(t, err)
	c1, err := tm.ConsistencyTime(ctx)
	require.NoError(t, err)

	// Step the wall clock back 10 ms while its monotonic reading moves on.
	// time.Time.Add shifts wall and monotonic together, and they only part
	// when the OS steps the wall clock, so the stepped reading is built by
	// shifting the wall value and keeping the unshifted monotonic reading.
	time.Sleep(2 * time.Millisecond)
	now := time.Now()
	sc.t = now.Add(-10 * time.Millisecond)
	(*timeRepr)(unsafe.Pointer(&sc.t)).ext = (*timeRepr)(unsafe.Pointer(&now)).ext
	require.True(t, sc.t.Round(0).Before(first.Round(0)), "precondition: stepped wall time must be earlier")
	require.True(t, sc.t.After(first), "precondition: stepped monotonic reading must be later")

	c2, err := tm.ConsistencyTime(ctx)
	require.NoError(t, err)
	require.False(t, c2.Round(0).Before(c1.Round(0)), "wall-time C went backwards: %v then %v", c1, c2)
}

// A read at C that starts after C returned sees a commit parked inside its stamp.
func TestConsistencyTime_ReadAtCSeesCommitParkedInItsStamp(t *testing.T) {
	gc := newGatedClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	f := memory.NewStoreFactory(memory.WithClock(gc))
	ctx := ctTenantCtx("t1")
	tm, err := f.TransactionManager(ctx)
	require.NoError(t, err)
	txID, txCtx, err := tm.Begin(ctx)
	require.NoError(t, err)
	es, err := f.EntityStore(txCtx)
	require.NoError(t, err)
	_, err = es.Save(txCtx, &spi.Entity{Meta: spi.EntityMeta{ID: "e1", ModelRef: spi.ModelRef{EntityName: "m", ModelVersion: "1"}}, Data: []byte(`{}`)})
	require.NoError(t, err)
	entered, unblock := gc.arm()
	commitErr := make(chan error, 1)
	go func() { commitErr <- tm.Commit(txCtx, txID) }()
	<-entered
	type res struct {
		c   time.Time
		err error
	}
	cCh := make(chan res, 1)
	go func() { c, err := tm.ConsistencyTime(ctx); cCh <- res{c, err} }()
	close(unblock)
	r := <-cCh
	require.NoError(t, r.err)
	require.NoError(t, <-commitErr)
	esr, err := f.EntityStore(ctx)
	require.NoError(t, err)
	got, err := esr.GetAsAt(ctx, "e1", r.c)
	require.NoError(t, err)
	require.Equal(t, "e1", got.Meta.ID)
}
