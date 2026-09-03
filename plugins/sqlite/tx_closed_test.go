package sqlite_test

import (
	"context"
	"errors"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// Every read entry point refuses a committed transaction's context with
// ErrTxAlreadyCommitted — the twin of the memory plugin's guard set, and of
// the write guards in tx_closed_write_test.go. Without it a caller holding a
// stale transaction context silently reads the merged view of a transaction
// that no longer exists: the snapshot is frozen at a time that has passed,
// and the buffer it merges was already flushed.
func TestTx_ClosedTransaction_RefusesEveryRead(t *testing.T) {
	f, tm := newAttrFactory(t)
	ctx := attrCtx("tenant-closed-read", "u1", spi.PrincipalUser)
	ref := spi.ModelRef{EntityName: "m-closed-read", ModelVersion: "1"}
	store, err := f.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	if _, err := store.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: "e-closed-read", TenantID: "tenant-closed-read", ModelRef: ref, State: "open"},
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
		{"Get", func(c context.Context) error {
			_, err := store.Get(c, "e-closed-read")
			return err
		}},
		{"GetAsAt", func(c context.Context) error {
			_, err := store.GetAsAt(c, "e-closed-read", time.Now())
			return err
		}},
		{"Exists", func(c context.Context) error {
			_, err := store.Exists(c, "e-closed-read")
			return err
		}},
		{"Search", func(c context.Context) error {
			_, err := store.Search(c, spi.Filter{}, spi.SearchOptions{
				ModelName: ref.EntityName, ModelVersion: ref.ModelVersion, Limit: 10,
			})
			return err
		}},
		{"GetPage", func(c context.Context) error {
			_, err := store.GetPage(c, ref, 10, 0, nil)
			return err
		}},
		{"Iterate", func(c context.Context) error {
			it, err := store.Iterate(c, ref, spi.Filter{}, spi.IterateOptions{})
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
