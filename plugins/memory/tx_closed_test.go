package memory_test

import (
	"context"
	"errors"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// Every read and bulk-write entry point refuses a committed transaction's
// context with ErrTxAlreadyCommitted — the same guard set sqlite carries.
// Without it a caller holding a stale transaction context silently reads the
// merged view of a transaction that no longer exists.
func TestTx_ClosedTransaction_RefusesEveryEntryPoint(t *testing.T) {
	f, tm := newTxManager(t)
	ctx := tenantCtx(spi.TenantID("tenant-closed"))
	ref := spi.ModelRef{EntityName: "m-closed", ModelVersion: "1"}
	store, err := f.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	if _, err := store.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: "e-closed", TenantID: "tenant-closed", ModelRef: ref, State: "open"},
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

	iterable, ok := any(store).(spi.Iterable)
	if !ok {
		t.Fatalf("EntityStore does not implement spi.Iterable")
	}

	cases := []struct {
		name string
		call func(context.Context) error
	}{
		{"Get", func(c context.Context) error {
			_, err := store.Get(c, "e-closed")
			return err
		}},
		{"GetAsAt", func(c context.Context) error {
			_, err := store.GetAsAt(c, "e-closed", time.Now())
			return err
		}},
		{"Exists", func(c context.Context) error {
			_, err := store.Exists(c, "e-closed")
			return err
		}},
		{"GetAll", func(c context.Context) error {
			_, err := store.GetAll(c, ref)
			return err
		}},
		{"GetPage", func(c context.Context) error {
			_, err := store.GetPage(c, ref, 10, 0, nil)
			return err
		}},
		{"Iterate", func(c context.Context) error {
			it, err := iterable.Iterate(c, ref, spi.Filter{}, spi.IterateOptions{})
			if err != nil {
				return err
			}
			for it.Next() {
			}
			_ = it.Close()
			return it.Err()
		}},
		{"Count", func(c context.Context) error {
			_, err := store.Count(c, ref)
			return err
		}},
		{"CountByState", func(c context.Context) error {
			_, err := store.CountByState(c, ref, nil)
			return err
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
}
