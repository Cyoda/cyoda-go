package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// CompareAndSave's expectedTxID means what it says: the entity's current
// transaction ID as this call sees it — the transaction's own uncommitted
// write if it has one, else the committed row's. It must be non-empty. The
// empty string is not a value any live entity's transaction ID can be trusted
// to differ from — a write taken outside a transaction stores the caller's
// supplied ID verbatim, empty included — so accepting it as "expect no
// entity" made a guard whose job is to fail closed fail open instead, and
// silently overwrite an entity that was there. It is now rejected as a caller
// error, and CompareAndSave neither creates an entity nor resurrects a
// deleted one: Save does that.
type casOutcome int

const (
	casSuccess casOutcome = iota
	casConflict
	casRejected // contract violation: a plain error, not spi.ErrConflict
)

func TestCompareAndSave_ExpectedIDIsLiteral(t *testing.T) {
	const seedTxID = "tx-seed"
	cases := []struct {
		name         string
		seed         bool   // an entity is committed under seedTxID first
		tombstone    bool   // the seeded entity is then deleted, committed, before the CAS
		expectedTxID string // what the caller claims is current
		want         casOutcome
	}{
		// The entity is there and the caller names its transaction ID.
		{"NonEmptyExpectedMatchingExistingEntity", true, false, seedTxID, casSuccess},
		// No entity: the current ID is "", so a non-empty expected ID
		// names a version that does not exist. CompareAndSave does not
		// create.
		{"NonEmptyExpectedAgainstMissingEntity", false, false, "tx-ghost", casConflict},
		// A committed tombstone offers no ID to match: the pre-delete
		// transaction ID no longer names the current version, and there is
		// no expected ID that would resurrect the entity.
		{"NonEmptyExpectedAgainstTombstone", true, true, seedTxID, casConflict},
		// The empty expected ID is a caller error, whatever is or is not
		// stored — it is rejected before any read or write.
		{"EmptyExpectedAgainstMissingEntity", false, false, "", casRejected},
		{"EmptyExpectedAgainstExistingEntity", true, false, "", casRejected},
		{"EmptyExpectedAgainstTombstone", true, true, "", casRejected},
	}

	factory, tm := setupEntityTestWithTM(t)
	ctx := ctxWithTenant("tenant-lit")
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	ref := spi.ModelRef{EntityName: "m-lit", ModelVersion: "1"}

	for _, tc := range cases {
		// One entity ID per subtest: the fixture's schema is shared, so a
		// seeded row must not be visible to the cases that expect none.
		t.Run(tc.name+"/NoTransaction", func(t *testing.T) {
			id := "e-lit-" + tc.name + "-notx"
			if tc.seed {
				seedLiteral(t, store, ctx, ref, id, seedTxID)
			}
			if tc.tombstone {
				if err := store.Delete(ctx, id); err != nil {
					t.Fatalf("seed Delete: %v", err)
				}
			}
			_, err := store.CompareAndSave(ctx, literalEntity(ref, id, "tx-writer"), tc.expectedTxID)
			assertCASOutcome(t, err, tc.want)
			// A rejected call must not have written — the seed is still
			// the seed byte for byte, and where there was no entity there
			// is still none. The tombstone legs matter as much as the
			// existing-entity one: a rejection that resurrected would be
			// exactly the fail-open this rule exists to close.
			if tc.want == casRejected {
				assertLiteralUnchanged(t, store, ctx, id, tc.seed && !tc.tombstone)
			}
		})

		t.Run(tc.name+"/InTransaction", func(t *testing.T) {
			id := "e-lit-" + tc.name + "-tx"
			if tc.seed {
				seedLiteral(t, store, ctx, ref, id, seedTxID)
			}
			if tc.tombstone {
				if err := store.Delete(ctx, id); err != nil {
					t.Fatalf("seed Delete: %v", err)
				}
			}
			txID, txCtx := beginGuarded(t, tm, ctx)
			_, err = store.CompareAndSave(txCtx, literalEntity(ref, id, "tx-writer"), tc.expectedTxID)
			assertCASOutcome(t, err, tc.want)
			if tc.want == casRejected {
				// Unchanged in the transaction's own view — a store that
				// wrote and then errored fails here — and still unchanged
				// once that transaction commits.
				assertLiteralUnchanged(t, store, txCtx, id, tc.seed && !tc.tombstone)
				if err := tm.Commit(txCtx, txID); err != nil {
					t.Fatalf("Commit: %v", err)
				}
				assertLiteralUnchanged(t, store, ctx, id, tc.seed && !tc.tombstone)
			}
		})
	}
}

// A delete staged in the caller's own transaction leaves the transaction
// seeing no entity. There is no expected ID that re-creates it: a non-empty
// one names a version the delete superseded and the empty one is a caller
// error, here as anywhere else.
func TestCompareAndSave_EmptyExpectedAfterSameTxDelete_Rejected(t *testing.T) {
	factory, tm := setupEntityTestWithTM(t)
	ctx := ctxWithTenant("tenant-lit")
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	ref := spi.ModelRef{EntityName: "m-lit", ModelVersion: "1"}
	const id = "e-lit-same-tx-delete"
	seedLiteral(t, store, ctx, ref, id, "tx-seed")

	txID, txCtx := beginGuarded(t, tm, ctx)
	if err := store.Delete(txCtx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, err = store.CompareAndSave(txCtx, literalEntity(ref, id, "tx-writer"), "")
	assertCASOutcome(t, err, casRejected)
	if err := tm.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if _, err := store.Get(ctx, id); !errors.Is(err, spi.ErrNotFound) {
		t.Fatalf("after commit Get: err = %v, want ErrNotFound (the delete must stand)", err)
	}
}

// The empty-ID guard is the FIRST thing CompareAndSave does, ahead of the
// rolled-back and already-committed checks, so a call that is both on a dead
// transaction and empty-ID reports the argument error. That ordering is
// deliberate and the same on all three backends: an empty expectedTxID is
// never a valid call whatever the transaction is doing, and answering it with
// a transaction-state sentinel would send the caller looking in the wrong
// place — and would let a handler retry-on-conflict a call that can never
// succeed. Nothing else pins the precedence, so a later reorder of the guards
// would flip it unnoticed; this is that pin.
func TestCompareAndSave_EmptyExpectedOnDeadTransaction_ReportsArgumentError(t *testing.T) {
	factory, tm := setupEntityTestWithTM(t)
	ctx := ctxWithTenant("tenant-lit")
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	ref := spi.ModelRef{EntityName: "m-lit", ModelVersion: "1"}

	for _, kill := range []string{"Committed", "RolledBack"} {
		t.Run(kill, func(t *testing.T) {
			id := "e-lit-dead-tx-" + kill
			seedLiteral(t, store, ctx, ref, id, "tx-seed")

			txID, txCtx := beginGuarded(t, tm, ctx)
			if kill == "Committed" {
				err = tm.Commit(txCtx, txID)
			} else {
				err = tm.Rollback(txCtx, txID)
			}
			if err != nil {
				t.Fatalf("%s: %v", kill, err)
			}

			_, err = store.CompareAndSave(txCtx, literalEntity(ref, id, "tx-writer"), "")
			assertArgumentRejection(t, err)
			assertLiteralUnchanged(t, store, ctx, id, true)
		})
	}
}

// casRejectionMessage is the text every backend's empty-expectedTxID guard
// returns. The rejection deliberately carries no sentinel — it is a contract
// violation, not a condition a caller retries — so the message is the only
// positive identification available, and matching it is what makes an
// assertion CONFIRM the argument error rather than merely rule out the
// alternatives someone happened to think of. Exactly one error is legitimate
// here, so this is a match, not a disjunction.
const casRejectionMessage = "expectedTxID must not be empty"

// assertArgumentRejection reports that err IS the empty-expectedTxID argument
// rejection: it carries casRejectionMessage, and it wraps none of the
// sentinels a caller would act on differently. The sentinel half matters most
// on a dead transaction, where the guard's position ahead of the rolled-back
// and already-committed checks is what decides the answer — but an
// exclusion-only check would pass on any unrelated fifth error, which is
// precisely the reorder these assertions exist to catch, so the message match
// leads.
func assertArgumentRejection(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("CompareAndSave with an empty expected transaction ID: err = nil, want the argument rejection")
	}
	if !strings.Contains(err.Error(), casRejectionMessage) {
		t.Fatalf("CompareAndSave with an empty expected transaction ID: err = %v, want an error containing %q", err, casRejectionMessage)
	}
	for _, sentinel := range []error{
		spi.ErrConflict, spi.ErrTxAlreadyCommitted, spi.ErrTxRolledBack, spi.ErrTxNotFound,
	} {
		if errors.Is(err, sentinel) {
			t.Fatalf("CompareAndSave with an empty expected transaction ID: err = %v, is the argument rejection but must not also wrap %v", err, sentinel)
		}
	}
}

const seedLiteralData = `{"n":0}`

func literalEntity(ref spi.ModelRef, id, txID string) *spi.Entity {
	return &spi.Entity{
		Meta: spi.EntityMeta{
			ID: id, TenantID: "tenant-lit", ModelRef: ref,
			State: "open", TransactionID: txID, ChangeUser: "user-1",
		},
		Data: []byte(`{"n":1}`),
	}
}

func seedLiteral(t *testing.T, store spi.EntityStore, ctx context.Context, ref spi.ModelRef, id, txID string) {
	t.Helper()
	seed := literalEntity(ref, id, txID)
	seed.Data = []byte(seedLiteralData)
	if _, err := store.Save(ctx, seed); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
}

// assertLiteralUnchanged reports that a rejected CompareAndSave left the store
// exactly as it found it: the seed byte for byte when one was committed and
// not deleted, and no entity at all otherwise — never written, or a committed
// tombstone the rejection must not have lifted.
func assertLiteralUnchanged(t *testing.T, store spi.EntityStore, ctx context.Context, id string, present bool) {
	t.Helper()
	got, err := store.Get(ctx, id)
	if !present {
		if !errors.Is(err, spi.ErrNotFound) {
			t.Fatalf("Get after a rejected CompareAndSave: err = %v, want ErrNotFound (nothing may have been created)", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("Get after a rejected CompareAndSave: %v", err)
	}
	if string(got.Data) != seedLiteralData {
		t.Fatalf("Data after a rejected CompareAndSave = %s, want %s", got.Data, seedLiteralData)
	}
}

func assertCASOutcome(t *testing.T, err error, want casOutcome) {
	t.Helper()
	switch want {
	case casSuccess:
		if err != nil {
			t.Fatalf("CompareAndSave: err = %v, want success", err)
		}
	case casConflict:
		if !errors.Is(err, spi.ErrConflict) {
			t.Fatalf("CompareAndSave: err = %v, want ErrConflict", err)
		}
	case casRejected:
		assertArgumentRejection(t, err)
	}
}
