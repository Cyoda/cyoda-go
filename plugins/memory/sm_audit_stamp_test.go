package memory_test

import (
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// TestSMAuditStamp_StampsTheTransactionsEventsAndNothingElse pins what the
// commit-phase audit stamp must touch: every event labelled with the committing
// transaction, across EVERY entity that transaction wrote — and nothing else.
//
// It is a correctness guard rather than a red-first test. The stamp already
// behaved this way when it scanned the whole tenant; the guard exists because
// the scan is being replaced by a transaction-id index, and an index is exactly
// the kind of change that can silently narrow what gets stamped — missing an
// entity, or stamping a neighbour's events. The multi-entity and
// foreign-label arms are the two ways that regression would show.
func TestSMAuditStamp_StampsTheTransactionsEventsAndNothingElse(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	tm := factory.NewTransactionManager(newTestUUIDGenerator())
	ctx := ctxWithTenant("tenant-audit-stamp")

	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	audit, err := factory.StateMachineAuditStore(ctx)
	if err != nil {
		t.Fatalf("StateMachineAuditStore: %v", err)
	}
	ref := spi.ModelRef{EntityName: "audit-stamp", ModelVersion: "1"}

	// A clock no running process would produce, so a value that survives
	// unstamped is unmistakable rather than merely close to the real one.
	sentinel := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	// TWO entities in one transaction: the stamp must reach both. A
	// transaction-id index that resolves to only the first entity — or to
	// only the entity the last event named — passes a single-entity test.
	for _, id := range []string{"e-1", "e-2"} {
		if _, err := store.Save(txCtx, &spi.Entity{
			Meta: spi.EntityMeta{ID: id, ModelRef: ref}, Data: []byte(`{"n":1}`),
		}); err != nil {
			t.Fatalf("Save %s: %v", id, err)
		}
		if err := audit.Record(txCtx, id, spi.StateMachineEvent{
			EventType: spi.SMEventStarted, EntityID: id, TimeUUID: "u-" + id,
			TransactionID: txID, Details: "in tx", Timestamp: sentinel,
		}); err != nil {
			t.Fatalf("Record %s: %v", id, err)
		}
	}

	// Two events that must NOT move: one belonging to no transaction, one
	// labelled with a transaction that never commits here.
	if err := audit.Record(ctx, "e-1", spi.StateMachineEvent{
		EventType: spi.SMEventFinished, EntityID: "e-1", TimeUUID: "u-none",
		Details: "no transaction", Timestamp: sentinel,
	}); err != nil {
		t.Fatalf("Record unlabelled: %v", err)
	}
	if err := audit.Record(ctx, "e-1", spi.StateMachineEvent{
		EventType: spi.SMEventFinished, EntityID: "e-1", TimeUUID: "u-foreign",
		TransactionID: "tx-someone-else", Details: "foreign label", Timestamp: sentinel,
	}); err != nil {
		t.Fatalf("Record foreign: %v", err)
	}

	if err := tm.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	submit, err := tm.GetSubmitTime(ctx, txID)
	if err != nil {
		t.Fatalf("GetSubmitTime: %v", err)
	}
	if submit.Equal(sentinel) {
		t.Fatal("the commit instant equals the sentinel; the assertions below could not discriminate")
	}

	stamped := 0
	for _, id := range []string{"e-1", "e-2"} {
		events, err := audit.GetEvents(ctx, id)
		if err != nil {
			t.Fatalf("GetEvents %s: %v", id, err)
		}
		for _, ev := range events {
			switch ev.TransactionID {
			case txID:
				if !ev.Timestamp.Equal(submit) {
					t.Errorf("event %s on %s carries %s, want the commit instant %s",
						ev.TimeUUID, id, ev.Timestamp, submit)
				}
				stamped++
			default:
				if !ev.Timestamp.Equal(sentinel) {
					t.Errorf("event %s on %s was restamped to %s, but it does not belong to the committing "+
						"transaction — its recorded time %s must survive", ev.TimeUUID, id, ev.Timestamp, sentinel)
				}
			}
		}
	}
	if stamped != 2 {
		t.Errorf("expected both of the transaction's events to be stamped, got %d — "+
			"a stamp that reaches only one entity of a multi-entity transaction is the regression this guards",
			stamped)
	}
}
