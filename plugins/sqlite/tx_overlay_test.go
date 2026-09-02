package sqlite_test

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func seedN(t *testing.T, store spi.EntityStore, ctx context.Context, ref spi.ModelRef, n int) []string {
	t.Helper()
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("e%02d", i)
		ids[i] = id
		if _, err := store.Save(ctx, &spi.Entity{
			Meta: spi.EntityMeta{ID: id, TenantID: "tenant-ovl", ModelRef: ref, State: []string{"open", "closed"}[i%2]},
			Data: []byte(fmt.Sprintf(`{"i":%d}`, i)),
		}); err != nil {
			t.Fatalf("seed Save(%s): %v", id, err)
		}
	}
	return ids
}

func drainIDs(t *testing.T, it spi.Iterator) []string {
	t.Helper()
	var ids []string
	for it.Next() {
		ids = append(ids, it.Entity().Meta.ID)
	}
	if err := it.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := it.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	return ids
}

func iterable(t *testing.T, store spi.EntityStore) spi.Iterable {
	t.Helper()
	it, ok := store.(spi.Iterable)
	if !ok {
		t.Fatal("store is not spi.Iterable")
	}
	return it
}

// The in-tx iterator yields the committed snapshot merged with the buffer,
// minus staged deletes, in entity-ID order — the same set getAllTx produced,
// now as one cursor.
func TestTxIterate_MergedViewInIDOrder(t *testing.T) {
	f, tm := newAttrFactory(t)
	ctx := attrCtx("tenant-ovl", "u1", spi.PrincipalUser)
	ref := spi.ModelRef{EntityName: "m-ovl", ModelVersion: "1"}
	store, _ := f.EntityStore(ctx)
	seedN(t, store, ctx, ref, 6) // e00..e05

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tm.Rollback(txCtx, txID) }()
	if err := store.Delete(txCtx, "e01"); err != nil { // staged delete
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Save(txCtx, &spi.Entity{ // buffered add, sorts first
		Meta: spi.EntityMeta{ID: "a00", TenantID: "tenant-ovl", ModelRef: ref, State: "open"},
		Data: []byte(`{"i":100}`),
	}); err != nil {
		t.Fatalf("Save add: %v", err)
	}
	if _, err := store.Save(txCtx, &spi.Entity{ // buffered update shadows e03
		Meta: spi.EntityMeta{ID: "e03", TenantID: "tenant-ovl", ModelRef: ref, State: "open"},
		Data: []byte(`{"i":303}`),
	}); err != nil {
		t.Fatalf("Save update: %v", err)
	}

	it, err := iterable(t, store).Iterate(txCtx, ref, spi.Filter{}, spi.IterateOptions{})
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	var got []string
	var e03Data string
	for it.Next() {
		e := it.Entity()
		got = append(got, e.Meta.ID)
		if e.Meta.ID == "e03" {
			e03Data = string(e.Data)
		}
	}
	_ = it.Close()
	if err := it.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	want := []string{"a00", "e00", "e02", "e03", "e04", "e05"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
	if e03Data != `{"i":303}` {
		t.Fatalf("e03 came from the committed row (%s); the buffered version must win", e03Data)
	}
}

// The filter is pushed into the committed query and applied to the buffer:
// a buffered entity that does not match must not be yielded, and a committed
// row shadowed by a non-matching buffered write must not be yielded either.
func TestTxIterate_FilterAppliesToBothStreams(t *testing.T) {
	f, tm := newAttrFactory(t)
	ctx := attrCtx("tenant-ovl", "u1", spi.PrincipalUser)
	ref := spi.ModelRef{EntityName: "m-ovl-f", ModelVersion: "1"}
	store, _ := f.EntityStore(ctx)
	seedN(t, store, ctx, ref, 4) // e00 open, e01 closed, e02 open, e03 closed

	txID, txCtx, _ := tm.Begin(ctx)
	defer func() { _ = tm.Rollback(txCtx, txID) }()
	// e02 was open; the buffered write closes it → must drop out.
	_, _ = store.Save(txCtx, &spi.Entity{Meta: spi.EntityMeta{ID: "e02", TenantID: "tenant-ovl", ModelRef: ref, State: "closed"}, Data: []byte(`{}`)})
	// new open entity in the buffer → must appear.
	_, _ = store.Save(txCtx, &spi.Entity{Meta: spi.EntityMeta{ID: "z00", TenantID: "tenant-ovl", ModelRef: ref, State: "open"}, Data: []byte(`{}`)})

	filter := spi.Filter{Op: spi.FilterEq, Path: "state", Source: spi.SourceMeta, Value: "open", Declared: []spi.DataType{spi.String}}
	it, err := iterable(t, store).Iterate(txCtx, ref, filter, spi.IterateOptions{})
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	got := drainIDs(t, it)
	want := []string{"e00", "z00"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
}

// TrackingRead records only yielded (post-residual) committed ids — never the
// rows the filter excluded, and never buffered own-writes.
func TestTxIterate_TrackingReadRecordsYieldsOnly(t *testing.T) {
	f, tm := newAttrFactory(t)
	ctx := attrCtx("tenant-ovl", "u1", spi.PrincipalUser)
	ref := spi.ModelRef{EntityName: "m-ovl-tr", ModelVersion: "1"}
	store, _ := f.EntityStore(ctx)
	seedN(t, store, ctx, ref, 4)

	txID, txCtx, _ := tm.Begin(ctx)
	defer func() { _ = tm.Rollback(txCtx, txID) }()
	_, _ = store.Save(txCtx, &spi.Entity{Meta: spi.EntityMeta{ID: "z00", TenantID: "tenant-ovl", ModelRef: ref, State: "open"}, Data: []byte(`{}`)})

	filter := spi.Filter{Op: spi.FilterEq, Path: "state", Source: spi.SourceMeta, Value: "open", Declared: []spi.DataType{spi.String}}
	it, err := iterable(t, store).Iterate(txCtx, ref, filter, spi.IterateOptions{TrackingRead: true})
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	_ = drainIDs(t, it)

	tx := spi.GetTransaction(txCtx)
	var recorded []string
	for id := range tx.ReadSet {
		recorded = append(recorded, id)
	}
	sort.Strings(recorded)
	want := []string{"e00", "e02"}
	if fmt.Sprint(recorded) != fmt.Sprint(want) {
		t.Fatalf("ReadSet = %v, want %v (yielded committed ids only)", recorded, want)
	}
}

// A same-tx Delete while the iterator is open must not deadlock (the cursor
// reads from readDB, never the single writer connection).
func TestTxIterate_SameTxDeleteWhileOpen_NoDeadlock(t *testing.T) {
	f, tm := newAttrFactory(t)
	ctx := attrCtx("tenant-ovl", "u1", spi.PrincipalUser)
	ref := spi.ModelRef{EntityName: "m-ovl-dl", ModelVersion: "1"}
	store, _ := f.EntityStore(ctx)
	seedN(t, store, ctx, ref, 5)

	txID, txCtx, _ := tm.Begin(ctx)
	defer func() { _ = tm.Rollback(txCtx, txID) }()
	it, err := iterable(t, store).Iterate(txCtx, ref, spi.Filter{}, spi.IterateOptions{})
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	if !it.Next() {
		t.Fatalf("first Next: false, err=%v", it.Err())
	}
	done := make(chan error, 1)
	go func() { done <- store.Delete(txCtx, "e04") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Delete while iterator open: %v", err)
		}
	case <-timeoutCh(t):
		t.Fatal("Delete deadlocked behind the open iterator")
	}
	_ = drainIDs(t, it)
}

// Commit while an iterator is open ends the iteration with
// ErrTxAlreadyCommitted rather than recording into a closed transaction.
func TestTxIterate_CommitWhileOpen_EndsWithAlreadyCommitted(t *testing.T) {
	f, tm := newAttrFactory(t)
	ctx := attrCtx("tenant-ovl", "u1", spi.PrincipalUser)
	ref := spi.ModelRef{EntityName: "m-ovl-cm", ModelVersion: "1"}
	store, _ := f.EntityStore(ctx)
	seedN(t, store, ctx, ref, 3)

	txID, txCtx, _ := tm.Begin(ctx)
	it, err := iterable(t, store).Iterate(txCtx, ref, spi.Filter{}, spi.IterateOptions{TrackingRead: true})
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	if !it.Next() {
		t.Fatalf("first Next: false, err=%v", it.Err())
	}
	if err := tm.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	for it.Next() {
	}
	_ = it.Close()
	if err := it.Err(); !errors.Is(err, spi.ErrTxAlreadyCommitted) {
		t.Fatalf("Err after commit = %v, want ErrTxAlreadyCommitted", err)
	}
}

func timeoutCh(t *testing.T) <-chan time.Time {
	t.Helper()
	return time.After(5 * time.Second)
}
