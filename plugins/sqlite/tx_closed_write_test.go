package sqlite_test

import (
	"context"
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// Every write entry point refuses a committed transaction's context with
// ErrTxAlreadyCommitted, the same guard the read paths already carry. Without
// it a caller holding a stale transaction context silently buffers a write
// into a transaction that has already been flushed and closed — the write is
// then never applied anywhere, a silent data loss (see
// .claude/rules/correctness-over-availability.md).
func TestTx_ClosedTransaction_RefusesEveryWrite(t *testing.T) {
	f, tm := newAttrFactory(t)
	ctx := attrCtx("tenant-closed-write", "u1", spi.PrincipalUser)
	ref := spi.ModelRef{EntityName: "m-closed-write", ModelVersion: "1"}
	store, err := f.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	if _, err := store.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: "e-closed-write", TenantID: "tenant-closed-write", ModelRef: ref, State: "open"},
		Data: []byte(`{"n":1}`),
	}); err != nil {
		t.Fatalf("seed Save: %v", err)
	}

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := tm.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	cases := []struct {
		name string
		call func(context.Context) error
	}{
		{"Save", func(c context.Context) error {
			_, err := store.Save(c, &spi.Entity{
				Meta: spi.EntityMeta{ID: "e-closed-write", TenantID: "tenant-closed-write", ModelRef: ref, State: "open"},
				Data: []byte(`{"n":2}`),
			})
			return err
		}},
		{"CompareAndSave", func(c context.Context) error {
			_, err := store.CompareAndSave(c, &spi.Entity{
				Meta: spi.EntityMeta{ID: "e-closed-write", TenantID: "tenant-closed-write", ModelRef: ref, State: "open"},
				Data: []byte(`{"n":3}`),
			}, txID)
			return err
		}},
		{"Delete", func(c context.Context) error {
			return store.Delete(c, "e-closed-write")
		}},
		{"DeleteAll", func(c context.Context) error {
			return store.DeleteAll(c, ref)
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call(txCtx)
			if !errors.Is(err, spi.ErrTxAlreadyCommitted) {
				t.Fatalf("%s on a committed transaction: err = %v, want ErrTxAlreadyCommitted", tc.name, err)
			}
		})
	}

	// The seeded entity must still read back unchanged: nothing buffered
	// against the closed transaction may have been silently applied.
	got, err := store.Get(ctx, "e-closed-write")
	if err != nil {
		t.Fatalf("Get after closed-tx writes: %v", err)
	}
	if string(got.Data) != `{"n":1}` {
		t.Fatalf("Get after closed-tx writes: Data = %s, want unchanged {\"n\":1}", got.Data)
	}
}
