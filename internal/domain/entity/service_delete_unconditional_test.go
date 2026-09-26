package entity

import (
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// The whole-model DeleteAll fast path is taken only when the request needs
// nothing per entity. A pointInTime or verbose=true on an unconditional
// delete goes through the enumerate-then-delete path so the instant is
// honored and the attempted ids are listed.

func TestDeleteEntitiesConditional_Unconditional_PointInTime_SparesLaterCreates(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	ctx := newDeleteBatchedCtx(t, factory)
	h := buildDeleteBatchedHandler(t, factory, mustTxMgr(t, factory))

	before := seedPersons(t, h, ctx, 3)
	// Memory stamps versions from the process clock, so time.Now() is the
	// server's clock here; the sleeps separate the instants.
	time.Sleep(2 * time.Millisecond)
	pit := time.Now()
	time.Sleep(2 * time.Millisecond)
	after := seedPersons(t, h, ctx, 2)

	res, err := h.DeleteEntitiesConditional(ctx, "Person", 1, nil, &pit, true, 0)
	if err != nil {
		t.Fatalf("DeleteEntitiesConditional: %v", err)
	}
	if res.MatchedCount != 3 || res.RemovedCount != 3 {
		t.Fatalf("matched/removed = %d/%d, want 3/3", res.MatchedCount, res.RemovedCount)
	}
	if len(res.IDToError) != 0 {
		t.Errorf("idToError = %v, want empty", res.IDToError)
	}
	assertSameIDSet(t, res.IDs, before)

	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	for _, id := range before {
		if _, gErr := store.Get(ctx, id); !errors.Is(gErr, spi.ErrNotFound) {
			t.Errorf("entity %s existed at the instant and must be gone; Get err = %v", id, gErr)
		}
	}
	for _, id := range after {
		if _, gErr := store.Get(ctx, id); gErr != nil {
			t.Errorf("entity %s was created after the instant and must survive; Get err = %v", id, gErr)
		}
	}
}

func TestDeleteEntitiesConditional_Unconditional_PointInTime_AlreadyGoneIDInIDToError(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	ctx := newDeleteBatchedCtx(t, factory)
	h := buildDeleteBatchedHandler(t, factory, mustTxMgr(t, factory))

	ids := seedPersons(t, h, ctx, 2)
	time.Sleep(2 * time.Millisecond)
	pit := time.Now()
	time.Sleep(2 * time.Millisecond)
	if _, err := h.DeleteEntity(ctx, ids[1]); err != nil {
		t.Fatalf("DeleteEntity: %v", err)
	}

	res, err := h.DeleteEntitiesConditional(ctx, "Person", 1, nil, &pit, true, 0)
	if err != nil {
		t.Fatalf("DeleteEntitiesConditional: %v", err)
	}
	if res.MatchedCount != 2 || res.RemovedCount != 1 {
		t.Fatalf("matched/removed = %d/%d, want 2/1", res.MatchedCount, res.RemovedCount)
	}
	if _, ok := res.IDToError[ids[1]]; !ok {
		t.Errorf("idToError = %v, want an entry for the already-gone id %s", res.IDToError, ids[1])
	}
	assertSameIDSet(t, res.IDs, ids) // attempted ids, including the failed one
}

func TestDeleteEntitiesConditional_Unconditional_Verbose_ListsAttemptedIDs(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	ctx := newDeleteBatchedCtx(t, factory)
	h := buildDeleteBatchedHandler(t, factory, mustTxMgr(t, factory))
	ids := seedPersons(t, h, ctx, 3)

	res, err := h.DeleteEntitiesConditional(ctx, "Person", 1, nil, nil, true, 0)
	if err != nil {
		t.Fatalf("DeleteEntitiesConditional: %v", err)
	}
	if res.MatchedCount != 3 || res.RemovedCount != 3 {
		t.Fatalf("matched/removed = %d/%d, want 3/3", res.MatchedCount, res.RemovedCount)
	}
	assertSameIDSet(t, res.IDs, ids)
}

func TestDeleteEntitiesConditional_Unconditional_Plain_ReturnsEmptyIDs(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	ctx := newDeleteBatchedCtx(t, factory)
	h := buildDeleteBatchedHandler(t, factory, mustTxMgr(t, factory))
	seedPersons(t, h, ctx, 2)

	res, err := h.DeleteEntitiesConditional(ctx, "Person", 1, nil, nil, false, 0)
	if err != nil {
		t.Fatalf("DeleteEntitiesConditional: %v", err)
	}
	if res.MatchedCount != 2 || res.RemovedCount != 2 {
		t.Fatalf("matched/removed = %d/%d, want 2/2", res.MatchedCount, res.RemovedCount)
	}
	if res.IDs == nil || len(res.IDs) != 0 {
		t.Errorf("IDs = %#v, want a non-nil empty slice when verbose is false", res.IDs)
	}
}

// A joined participant's buffered (uncommitted) entities never existed in
// committed state at any instant, so a pointInTime selection cannot see
// them; the plain fast path (no instant) still deletes them. Both are the
// conditional path's existing semantics, now reachable on the unconditional
// form.
func TestDeleteEntitiesConditional_Joined_PointInTime_SparesBufferedEntities(t *testing.T) {
	h, factory, txMgr, base := newTxJoinTestHandler(t)

	ownerTxID, ownerCtx, err := txMgr.Begin(base)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	joinedCtx, err := txMgr.Join(base, ownerTxID)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	created, err := h.CreateEntity(joinedCtx, sampleWidgetInput())
	if err != nil {
		t.Fatalf("create in joined tx: %v", err)
	}
	bufferedID := created.EntityIDs[0]

	time.Sleep(2 * time.Millisecond)
	pit := time.Now()

	res, err := h.DeleteEntitiesConditional(joinedCtx, "Widget", 1, nil, &pit, true, 0)
	if err != nil {
		t.Fatalf("DeleteEntitiesConditional (joined, pointInTime): %v", err)
	}
	if res.MatchedCount != 0 || len(res.IDs) != 0 {
		t.Fatalf("matched = %d, ids = %v; a buffered entity is not committed state at any instant", res.MatchedCount, res.IDs)
	}
	if err := txMgr.Commit(ownerCtx, ownerTxID); err != nil {
		t.Fatalf("owner commit: %v", err)
	}
	store, err := factory.EntityStore(base)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	if _, err := store.Get(base, bufferedID); err != nil {
		t.Errorf("buffered entity must survive a pointInTime delete and commit; Get err = %v", err)
	}
}

func TestDeleteEntitiesConditional_Joined_NoInstant_DeletesBufferedEntities(t *testing.T) {
	h, factory, txMgr, base := newTxJoinTestHandler(t)

	ownerTxID, ownerCtx, err := txMgr.Begin(base)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	joinedCtx, err := txMgr.Join(base, ownerTxID)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	created, err := h.CreateEntity(joinedCtx, sampleWidgetInput())
	if err != nil {
		t.Fatalf("create in joined tx: %v", err)
	}
	bufferedID := created.EntityIDs[0]

	if _, err := h.DeleteEntitiesConditional(joinedCtx, "Widget", 1, nil, nil, false, 0); err != nil {
		t.Fatalf("DeleteEntitiesConditional (joined, fast path): %v", err)
	}
	if err := txMgr.Commit(ownerCtx, ownerTxID); err != nil {
		t.Fatalf("owner commit: %v", err)
	}
	store, err := factory.EntityStore(base)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	if _, err := store.Get(base, bufferedID); !errors.Is(err, spi.ErrNotFound) {
		t.Errorf("fast path deletes same-tx buffered entities; Get err = %v, want ErrNotFound", err)
	}
}

func assertSameIDSet(t *testing.T, got, want []string) {
	t.Helper()
	g := append([]string(nil), got...)
	w := append([]string(nil), want...)
	sort.Strings(g)
	sort.Strings(w)
	if len(g) != len(w) {
		t.Fatalf("ids = %v (len %d), want %v (len %d)", g, len(g), w, len(w))
	}
	for i := range g {
		if g[i] != w[i] {
			t.Fatalf("ids = %v, want %v", g, w)
		}
	}
}
