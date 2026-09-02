package memory_test

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

type contextT = context.Context

func seedStates(t *testing.T, store spi.EntityStore, ctx contextT, ref spi.ModelRef, states ...string) {
	t.Helper()
	for i, st := range states {
		if _, err := store.Save(ctx, &spi.Entity{
			Meta: spi.EntityMeta{ID: fmt.Sprintf("e%02d", i), TenantID: "tenant-it", ModelRef: ref, State: st},
			Data: []byte(`{}`),
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
}

func readSetIDs(tx *spi.TransactionState) []string {
	var ids []string
	for id := range tx.ReadSet {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// stateIs is the meta "state" equality leaf. Declared is mandatory: spi.Prepare
// refuses a leaf whose comparison type it cannot resolve.
func stateIs(state string) spi.Filter {
	return spi.Filter{
		Op:       spi.FilterEq,
		Path:     "state",
		Source:   spi.SourceMeta,
		Value:    state,
		Declared: []spi.DataType{spi.String},
	}
}

// In-tx Iterate with TrackingRead records only the committed ids it yields —
// not the whole merged model at open.
func TestIterate_InTx_TrackingReadRecordsYieldsOnly(t *testing.T) {
	f, tm := newTxManager(t)
	ctx := tenantCtx("tenant-it")
	ref := spi.ModelRef{EntityName: "m-it", ModelVersion: "1"}
	store, _ := f.EntityStore(ctx)
	seedStates(t, store, ctx, ref, "open", "closed", "open", "closed")

	txID, txCtx, _ := tm.Begin(ctx)
	defer func() { _ = tm.Rollback(txCtx, txID) }()
	_, _ = store.Save(txCtx, &spi.Entity{Meta: spi.EntityMeta{ID: "z00", TenantID: "tenant-it", ModelRef: ref, State: "open"}, Data: []byte(`{}`)})

	it, err := store.(spi.Iterable).Iterate(txCtx, ref, stateIs("open"), spi.IterateOptions{TrackingRead: true})
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	tx := spi.GetTransaction(txCtx)
	if got := readSetIDs(tx); len(got) != 0 {
		t.Fatalf("ReadSet populated at open: %v; must record per yield", got)
	}
	var yielded []string
	for it.Next() {
		yielded = append(yielded, it.Entity().Meta.ID)
	}
	_ = it.Close()
	if err := it.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	sort.Strings(yielded)
	if fmt.Sprint(yielded) != fmt.Sprint([]string{"e00", "e02", "z00"}) {
		t.Fatalf("yielded = %v", yielded)
	}
	if got := readSetIDs(tx); fmt.Sprint(got) != fmt.Sprint([]string{"e00", "e02"}) {
		t.Fatalf("ReadSet = %v, want [e00 e02] (yielded committed ids only)", got)
	}
}

// In-tx GroupedAggregate records nothing — the rule every backend shares.
func TestGroupedAggregate_InTx_RecordsNothing(t *testing.T) {
	f, tm := newTxManager(t)
	ctx := tenantCtx("tenant-it")
	ref := spi.ModelRef{EntityName: "m-ga", ModelVersion: "1"}
	store, _ := f.EntityStore(ctx)
	seedStates(t, store, ctx, ref, "open", "closed")

	txID, txCtx, _ := tm.Begin(ctx)
	defer func() { _ = tm.Rollback(txCtx, txID) }()
	ga := store.(spi.GroupedAggregator)
	_, err := ga.GroupedAggregate(txCtx, ref, []spi.GroupExpr{{Kind: spi.GroupExprState}}, spi.Filter{}, spi.GroupedAggregationsOptions{
		MaxBuckets: 10,
	})
	if err != nil {
		t.Fatalf("GroupedAggregate: %v", err)
	}
	if got := readSetIDs(spi.GetTransaction(txCtx)); len(got) != 0 {
		t.Fatalf("GroupedAggregate recorded %v into the read-set; must record nothing", got)
	}
}

// A Rollback between two yields ends the iteration, even for a non-tracking
// iterator: the per-yield transaction check is unconditional, so an open
// iterator stops serving a view of a transaction that has been thrown away.
func TestIterate_InTx_RollbackWhileOpen_EndsWithRolledBack(t *testing.T) {
	f, tm := newTxManager(t)
	ctx := tenantCtx("tenant-it")
	ref := spi.ModelRef{EntityName: "m-it-rb", ModelVersion: "1"}
	store, _ := f.EntityStore(ctx)
	seedStates(t, store, ctx, ref, "open", "open", "open")

	txID, txCtx, _ := tm.Begin(ctx)
	it, err := store.(spi.Iterable).Iterate(txCtx, ref, spi.Filter{}, spi.IterateOptions{})
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	if !it.Next() {
		t.Fatalf("first Next: false, err=%v", it.Err())
	}
	if err := tm.Rollback(txCtx, txID); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	for it.Next() {
	}
	_ = it.Close()
	if err := it.Err(); !errors.Is(err, spi.ErrTxRolledBack) {
		t.Fatalf("Err after rollback = %v, want ErrTxRolledBack", err)
	}
}

// A point-in-time iteration is committed-only and ignores the ambient
// transaction: it yields neither the buffer's own-writes nor the read-set, and
// a Rollback under it does not end the drain — the same routing sqlite applies
// by sending a PIT read away from its transaction iterator.
func TestIterate_InTx_PIT_IgnoresTransaction(t *testing.T) {
	f, tm := newTxManager(t)
	ctx := tenantCtx("tenant-it")
	ref := spi.ModelRef{EntityName: "m-it-pit", ModelVersion: "1"}
	store, _ := f.EntityStore(ctx)
	seedStates(t, store, ctx, ref, "open", "open", "open")

	txID, txCtx, _ := tm.Begin(ctx)
	tx := spi.GetTransaction(txCtx)
	snapshotTime := tx.SnapshotTime
	if _, err := store.Save(txCtx, &spi.Entity{Meta: spi.EntityMeta{ID: "z00", TenantID: "tenant-it", ModelRef: ref, State: "open"}, Data: []byte(`{}`)}); err != nil {
		t.Fatalf("buffered Save: %v", err)
	}

	it, err := store.(spi.Iterable).Iterate(txCtx, ref, spi.Filter{}, spi.IterateOptions{PointInTime: &snapshotTime, TrackingRead: true})
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	var yielded []string
	if !it.Next() {
		t.Fatalf("first Next: false, err=%v", it.Err())
	}
	yielded = append(yielded, it.Entity().Meta.ID)
	if err := tm.Rollback(txCtx, txID); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	for it.Next() {
		yielded = append(yielded, it.Entity().Meta.ID)
	}
	_ = it.Close()
	if err := it.Err(); err != nil {
		t.Fatalf("Err: %v, want nil (a PIT read does not observe the transaction)", err)
	}
	sort.Strings(yielded)
	if fmt.Sprint(yielded) != fmt.Sprint([]string{"e00", "e01", "e02"}) {
		t.Fatalf("yielded = %v, want [e00 e01 e02] (committed only)", yielded)
	}
	if got := readSetIDs(tx); len(got) != 0 {
		t.Fatalf("PIT iteration recorded %v into the read-set; must record nothing", got)
	}
}
