package sqlite_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/sqlite"
)

// TestSMAudit_DataNumbersKeepEveryDigit pins that a number in an event's Data
// reads back with every digit it was recorded with, including one a float64
// cannot hold: numbers in Data read back as json.Number.
func TestSMAudit_DataNumbersKeepEveryDigit(t *testing.T) {
	factory, err := sqlite.NewStoreFactoryForTest(context.Background(), filepath.Join(t.TempDir(), "audit-numbers.db"))
	if err != nil {
		t.Fatalf("NewStoreFactoryForTest: %v", err)
	}
	defer factory.Close()

	ctx := testCtx("tenant-numbers")
	as, err := factory.StateMachineAuditStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	const big = "12345678901234567890"
	if err := as.Record(ctx, "e-1", spi.StateMachineEvent{
		EventType: spi.SMEventWorkflowFound, EntityID: "e-1",
		Data: map[string]any{"big": json.Number(big), "small": 42},
	}); err != nil {
		t.Fatal(err)
	}
	evs, err := as.GetEvents(ctx, "e-1")
	if err != nil || len(evs) != 1 {
		t.Fatalf("GetEvents = %v, %v", evs, err)
	}
	for key, want := range map[string]string{"big": big, "small": "42"} {
		if got, ok := evs[0].Data[key].(json.Number); !ok || got.String() != want {
			t.Errorf("Data[%q] = %v (%T); want json.Number %s", key, evs[0].Data[key], evs[0].Data[key], want)
		}
	}
}
