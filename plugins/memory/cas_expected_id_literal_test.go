package memory_test

import (
	"context"
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// CompareAndSave's expectedTxID means what it says: the entity's current
// transaction ID as this call sees it — the buffered own-write's if the
// transaction has one, else the committed row's, and "" when there is no
// entity at all. Anything else is a conflict.
//
// The store used to skip the comparison entirely when there was no row, so a
// caller naming a transaction ID that could not possibly be current got a
// created entity instead of ErrConflict, and a caller saying "expect no
// entity" silently clobbered whatever was there.
func TestCompareAndSave_ExpectedIDIsLiteral(t *testing.T) {
	const seedTxID = "tx-seed"
	cases := []struct {
		name         string
		seed         bool   // an entity is committed under seedTxID first
		expectedTxID string // what the caller claims is current
		wantConflict bool
	}{
		// No entity: the current ID is "", so a non-empty expected ID
		// names a version that does not exist.
		{"NonEmptyExpectedAgainstMissingEntity", false, "tx-ghost", true},
		// No entity and "expect no entity" — the create case.
		{"EmptyExpectedAgainstMissingEntity", false, "", false},
		// An entity IS there, so "expect no entity" is wrong.
		{"EmptyExpectedAgainstExistingEntity", true, "", true},
	}

	for _, tc := range cases {
		t.Run(tc.name+"/NoTransaction", func(t *testing.T) {
			factory := memory.NewStoreFactory()
			defer factory.Close()
			ctx := ctxWithTenant("tenant-lit")
			store, err := factory.EntityStore(ctx)
			if err != nil {
				t.Fatalf("EntityStore: %v", err)
			}
			ref := spi.ModelRef{EntityName: "m-lit", ModelVersion: "1"}
			if tc.seed {
				seedLiteral(t, store, ctx, ref, seedTxID)
			}
			_, err = store.CompareAndSave(ctx, literalEntity(ref, "tx-writer"), tc.expectedTxID)
			assertConflict(t, err, tc.wantConflict)
		})

		t.Run(tc.name+"/InTransaction", func(t *testing.T) {
			factory := memory.NewStoreFactory()
			defer factory.Close()
			uuids := newTestUUIDGenerator()
			tm := factory.NewTransactionManager(uuids)
			ctx := ctxWithTenant("tenant-lit")
			store, err := factory.EntityStore(ctx)
			if err != nil {
				t.Fatalf("EntityStore: %v", err)
			}
			ref := spi.ModelRef{EntityName: "m-lit", ModelVersion: "1"}
			if tc.seed {
				seedLiteral(t, store, ctx, ref, seedTxID)
			}
			_, txCtx, err := tm.Begin(ctx)
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			_, err = store.CompareAndSave(txCtx, literalEntity(ref, "tx-writer"), tc.expectedTxID)
			assertConflict(t, err, tc.wantConflict)
		})
	}
}

func literalEntity(ref spi.ModelRef, txID string) *spi.Entity {
	return &spi.Entity{
		Meta: spi.EntityMeta{
			ID: "e-lit", TenantID: "tenant-lit", ModelRef: ref,
			State: "open", TransactionID: txID,
		},
		Data: []byte(`{"n":1}`),
	}
}

func seedLiteral(t *testing.T, store spi.EntityStore, ctx context.Context, ref spi.ModelRef, txID string) {
	t.Helper()
	if _, err := store.Save(ctx, literalEntity(ref, txID)); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
}

func assertConflict(t *testing.T, err error, want bool) {
	t.Helper()
	switch {
	case want && !errors.Is(err, spi.ErrConflict):
		t.Fatalf("CompareAndSave: err = %v, want ErrConflict", err)
	case !want && err != nil:
		t.Fatalf("CompareAndSave: err = %v, want success", err)
	}
}
