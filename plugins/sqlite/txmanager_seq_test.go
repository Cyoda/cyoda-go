package sqlite_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/sqlite"
)

func seqEntity(id string) *spi.Entity {
	return &spi.Entity{
		Meta: spi.EntityMeta{ID: id, ModelRef: spi.ModelRef{EntityName: "SeqModel", ModelVersion: "1"}},
		Data: []byte(`{"n":1}`),
	}
}

func newFrozenTM(t *testing.T) (*sqlite.StoreFactory, spi.TransactionManager, context.Context) {
	t.Helper()
	clock := sqlite.NewTestClock() // never advanced
	f, err := sqlite.NewStoreFactoryForTest(context.Background(), filepath.Join(t.TempDir(), "seq.db"), sqlite.WithClock(clock))
	if err != nil {
		t.Fatalf("NewStoreFactoryForTest: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	ctx := testCtx("tenant-A")
	tm, err := f.TransactionManager(ctx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	return f, tm, ctx
}

func beginSave(t *testing.T, f *sqlite.StoreFactory, tm spi.TransactionManager, ctx context.Context, id string) string {
	t.Helper()
	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	es, err := f.EntityStore(txCtx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	if _, err := es.Save(txCtx, seqEntity(id)); err != nil {
		t.Fatalf("Save: %v", err)
	}
	return txID
}

// Under a frozen clock a commit before Begin carries the same instant as the
// snapshot. It precedes the transaction and must not conflict with it.
func TestFCW_ACommitBeforeBeginDoesNotConflictUnderAFrozenClock(t *testing.T) {
	f, tm, ctx := newFrozenTM(t)
	holdID, _, err := tm.Begin(ctx) // keeps the committed log from being pruned
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	t.Cleanup(func() { _ = tm.Rollback(ctx, holdID) })

	if err := tm.Commit(ctx, beginSave(t, f, tm, ctx, "e1")); err != nil {
		t.Fatalf("first Commit: %v", err)
	}
	if err := tm.Commit(ctx, beginSave(t, f, tm, ctx, "e1")); err != nil {
		t.Fatalf("a transaction begun after the first commit conflicted with it: %v", err)
	}
}

// Two overlapping transactions on one entity still conflict.
func TestFCW_OverlappingWritersStillConflictUnderAFrozenClock(t *testing.T) {
	f, tm, ctx := newFrozenTM(t)
	a := beginSave(t, f, tm, ctx, "e1")
	b := beginSave(t, f, tm, ctx, "e1")
	if err := tm.Commit(ctx, a); err != nil {
		t.Fatalf("first Commit: %v", err)
	}
	if err := tm.Commit(ctx, b); !errors.Is(err, spi.ErrConflict) {
		t.Fatalf("second Commit = %v, want ErrConflict", err)
	}
}

// A transaction open across many later commits still conflicts with the one
// that wrote its entity: pruning drops only entries at or below the oldest
// open snapshot, never one an open transaction can conflict with.
func TestFCW_ALongOpenTransactionConflictsAfterManyLaterCommits(t *testing.T) {
	cases := []struct {
		name string
		// touch puts e1 in the long transaction's read or write set.
		touch func(t *testing.T, f *sqlite.StoreFactory, txCtx context.Context)
	}{
		{"WriteSet", func(t *testing.T, f *sqlite.StoreFactory, txCtx context.Context) {
			es, err := f.EntityStore(txCtx)
			if err != nil {
				t.Fatalf("EntityStore: %v", err)
			}
			if _, err := es.Save(txCtx, seqEntity("e1")); err != nil {
				t.Fatalf("Save: %v", err)
			}
		}},
		{"ReadSet", func(t *testing.T, f *sqlite.StoreFactory, txCtx context.Context) {
			es, err := f.EntityStore(txCtx)
			if err != nil {
				t.Fatalf("EntityStore: %v", err)
			}
			if _, err := es.Get(txCtx, "e1"); err != nil {
				t.Fatalf("Get: %v", err)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, tm, ctx := newFrozenTM(t)
			if err := tm.Commit(ctx, beginSave(t, f, tm, ctx, "e1")); err != nil {
				t.Fatalf("seed Commit: %v", err)
			}
			longID, longCtx, err := tm.Begin(ctx)
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			c.touch(t, f, longCtx)
			if err := tm.Commit(ctx, beginSave(t, f, tm, ctx, "e1")); err != nil {
				t.Fatalf("Commit of e1: %v", err)
			}
			for i := 0; i < 5; i++ {
				if err := tm.Commit(ctx, beginSave(t, f, tm, ctx, "e2")); err != nil {
					t.Fatalf("Commit %d of e2: %v", i, err)
				}
			}
			if err := tm.Commit(ctx, longID); !errors.Is(err, spi.ErrConflict) {
				t.Fatalf("long Commit = %v, want ErrConflict: e1 was committed after it began", err)
			}
		})
	}
}

// Pruning never drops an entry a still-open transaction conflicts with,
// however many commits land after it. The clock is frozen, so only the
// sequence numbers order the commits. This is the entity-level test memory
// runs under the same name (TestCommittedLogPruningKeepsAnOpenTransactionsConflict);
// unlike TestFCW_ALongOpenTransactionConflictsAfterManyLaterCommits above, it
// drives the transaction manager directly — tx.WriteSet/ReadSet/Buffer — not
// through EntityStore.
func TestCommittedLogPruningKeepsAnOpenTransactionsConflict(t *testing.T) {
	for _, mode := range []string{"write", "read"} {
		t.Run(mode, func(t *testing.T) {
			_, tm, ctx := newFrozenTM(t)

			write := func(txCtx context.Context, id string) {
				tx := spi.GetTransaction(txCtx)
				tx.WriteSet[id] = true
				tx.Buffer[id] = &spi.Entity{Meta: spi.EntityMeta{ID: id, TenantID: "tenant-A", ChangeType: "CREATED"}, Data: []byte(`{}`)}
			}
			commitOne := func(id string) {
				t.Helper()
				txID, txCtx, err := tm.Begin(ctx)
				if err != nil {
					t.Fatalf("Begin: %v", err)
				}
				write(txCtx, id)
				if err := tm.Commit(ctx, txID); err != nil {
					t.Fatalf("Commit of %s: %v", id, err)
				}
			}

			longID, longCtx, err := tm.Begin(ctx)
			if err != nil {
				t.Fatalf("Begin long: %v", err)
			}
			if mode == "write" {
				write(longCtx, "e1")
			} else {
				spi.GetTransaction(longCtx).ReadSet["e1"] = true
			}
			commitOne("e1")
			for i := 0; i < 5; i++ {
				commitOne("e2")
			}
			if err := tm.Commit(ctx, longID); !errors.Is(err, spi.ErrConflict) {
				t.Fatalf("long Commit = %v, want ErrConflict: e1 was committed after it began", err)
			}
		})
	}
}
