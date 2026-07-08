package externalapi

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/e2e/externalapi/driver"
	"github.com/cyoda-platform/cyoda-go/e2e/externalapi/errorcontract"
	"github.com/cyoda-platform/cyoda-go/e2e/parity"
)

func init() {
	parity.Register(
		parity.NamedTest{Name: "ExternalAPI_07_01_GetEntityAtPointInTime", Fn: RunExternalAPI_07_01_GetEntityAtPointInTime},
		parity.NamedTest{Name: "ExternalAPI_07_02_GetEntityByTransactionID", Fn: RunExternalAPI_07_02_GetEntityByTransactionID},
		parity.NamedTest{Name: "ExternalAPI_07_03_ChangeHistoryFull", Fn: RunExternalAPI_07_03_ChangeHistoryFull},
		parity.NamedTest{Name: "ExternalAPI_07_04_ChangeHistoryAtPointInTime", Fn: RunExternalAPI_07_04_ChangeHistoryAtPointInTime},
		parity.NamedTest{Name: "ExternalAPI_07_05_ChangeHistoryNonExistent", Fn: RunExternalAPI_07_05_ChangeHistoryNonExistent},
	)
}

// RunExternalAPI_07_01_GetEntityAtPointInTime — dictionary 07/01.
// GET entity at three different points in time returns three states.
func RunExternalAPI_07_01_GetEntityAtPointInTime(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	d := driver.NewInProcess(t, fixture)
	if err := d.CreateModelFromSample("pit1", 1, `{"k":1}`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := d.LockModel("pit1", 1); err != nil {
		t.Fatalf("lock: %v", err)
	}
	id, err := d.CreateEntity("pit1", 1, `{"k":1}`)
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	t1 := time.Now().UTC()
	time.Sleep(100 * time.Millisecond)

	if err := d.UpdateEntityData(id, `{"k":2}`); err != nil {
		t.Fatalf("update@t2: %v", err)
	}
	t2 := time.Now().UTC()
	time.Sleep(100 * time.Millisecond)

	if err := d.UpdateEntityData(id, `{"k":3}`); err != nil {
		t.Fatalf("update@t3: %v", err)
	}

	gotT1, err := d.GetEntityAt(id, t1)
	if err != nil {
		t.Fatalf("GetEntityAt(t1): %v", err)
	}
	if gotT1.Data["k"] != float64(1) {
		t.Errorf("at t1: got k=%v, want 1", gotT1.Data["k"])
	}
	gotT2, err := d.GetEntityAt(id, t2)
	if err != nil {
		t.Fatalf("GetEntityAt(t2): %v", err)
	}
	if gotT2.Data["k"] != float64(2) {
		t.Errorf("at t2: got k=%v, want 2", gotT2.Data["k"])
	}
	gotNow, err := d.GetEntity(id)
	if err != nil {
		t.Fatalf("GetEntity(now): %v", err)
	}
	if gotNow.Data["k"] != float64(3) {
		t.Errorf("at now: got k=%v, want 3", gotNow.Data["k"])
	}
}

// RunExternalAPI_07_02_GetEntityByTransactionID — dictionary 07/02.
// Dictionary expects GET /entity/{id}?transactionId=<tx> to return the
// entity envelope as it stood at that transaction.
// equiv_or_better: cyoda-go's GetOneEntity routes the transactionId
// param into the service, which scans the entity's version history
// and returns the matching version's envelope (or ENTITY_NOT_FOUND@404
// on a miss — exercised by 12/neg/05).
func RunExternalAPI_07_02_GetEntityByTransactionID(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	d := driver.NewInProcess(t, fixture)
	if err := d.CreateModelFromSample("pit2", 1, `{"k":1}`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := d.LockModel("pit2", 1); err != nil {
		t.Fatalf("lock: %v", err)
	}
	id, err := d.CreateEntity("pit2", 1, `{"k":1}`)
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	// Capture the current entity's transactionId, then update the entity
	// twice. A transactionId-scoped GET should return the captured-tx
	// snapshot regardless of subsequent updates.
	current, err := d.GetEntity(id)
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}
	createTxID := current.Meta.TransactionID
	if createTxID == "" {
		t.Fatalf("create entity meta missing transactionId; cannot exercise tx-scoped GET")
	}
	if err := d.UpdateEntityData(id, `{"k":2}`); err != nil {
		t.Fatalf("update@tx2: %v", err)
	}
	if err := d.UpdateEntityData(id, `{"k":3}`); err != nil {
		t.Fatalf("update@tx3: %v", err)
	}

	got, err := d.GetEntityByTransactionID(id, createTxID)
	if err != nil {
		t.Fatalf("GetEntityByTransactionID: %v", err)
	}
	if v, _ := got.Data["k"].(float64); v != float64(1) {
		t.Errorf("data.k: got %v, want 1 (the createTxID snapshot, before subsequent updates)", got.Data["k"])
	}
	if got.Meta.TransactionID != createTxID {
		t.Errorf("meta.transactionId: got %q want %q", got.Meta.TransactionID, createTxID)
	}
}

// RunExternalAPI_07_03_ChangeHistoryFull — dictionary 07/03.
// Full change history lists CREATE + N UPDATEs.
func RunExternalAPI_07_03_ChangeHistoryFull(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	d := driver.NewInProcess(t, fixture)
	if err := d.CreateModelFromSample("pit3", 1, `{"k":1}`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := d.LockModel("pit3", 1); err != nil {
		t.Fatalf("lock: %v", err)
	}
	id, err := d.CreateEntity("pit3", 1, `{"k":1}`)
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	if err := d.UpdateEntityData(id, `{"k":2}`); err != nil {
		t.Fatalf("update1: %v", err)
	}
	if err := d.UpdateEntityData(id, `{"k":3}`); err != nil {
		t.Fatalf("update2: %v", err)
	}
	changes, err := d.GetEntityChanges(id)
	if err != nil {
		t.Fatalf("GetEntityChanges: %v", err)
	}
	if len(changes) < 3 {
		t.Errorf("changes: got %d, want >= 3 (1 CREATE + 2 UPDATE)", len(changes))
	}
	// The API returns changes newest-first; the oldest entry (CREATE) is last.
	last := changes[len(changes)-1]
	if last.ChangeType != "CREATE" {
		t.Errorf("changes[last].changeType: got %q, want CREATE", last.ChangeType)
	}
}

// RunExternalAPI_07_04_ChangeHistoryAtPointInTime — dictionary 07/04.
// Asserts that GET /entity/{id}/changes?pointInTime=<t> truncates the
// returned change history to entries at or before the supplied timestamp.
//
// Sequence: CREATE @ k=1 → UPDATE @ k=2 → cutoff → UPDATE @ k=3.
// Full history is 3 entries; truncated history is 2 (CREATE + first UPDATE).
func RunExternalAPI_07_04_ChangeHistoryAtPointInTime(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	d := driver.NewInProcess(t, fixture)
	if err := d.CreateModelFromSample("pit4", 1, `{"k":1}`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := d.LockModel("pit4", 1); err != nil {
		t.Fatalf("lock: %v", err)
	}
	id, err := d.CreateEntity("pit4", 1, `{"k":1}`)
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	if err := d.UpdateEntityData(id, `{"k":2}`); err != nil {
		t.Fatalf("update@k=2: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	cutoff := time.Now().UTC()
	time.Sleep(50 * time.Millisecond)
	if err := d.UpdateEntityData(id, `{"k":3}`); err != nil {
		t.Fatalf("update@k=3: %v", err)
	}

	// Full history — 3 entries.
	full, err := d.GetEntityChanges(id)
	if err != nil {
		t.Fatalf("GetEntityChanges (full): %v", err)
	}
	if len(full) != 3 {
		t.Errorf("full history: got %d entries, want 3", len(full))
	}

	// Truncated history at cutoff — 2 entries (CREATE + first UPDATE).
	truncated, err := d.GetEntityChangesAt(id, cutoff)
	if err != nil {
		t.Fatalf("GetEntityChangesAt: %v", err)
	}
	if len(truncated) != 2 {
		t.Fatalf("truncated history at cutoff: got %d entries, want 2", len(truncated))
	}
	for i, entry := range truncated {
		if entry.TimeOfChange.After(cutoff) {
			t.Errorf("entry %d: timeOfChange %s is after cutoff %s", i, entry.TimeOfChange, cutoff)
		}
	}
}

// RunExternalAPI_07_05_ChangeHistoryNonExistent — dictionary 07/05 (NEGATIVE).
// Dictionary expects HTTP 404 + EntityNotFoundException.
// equiv_or_better: cyoda-go emits ENTITY_NOT_FOUND @404 — matches exactly.
func RunExternalAPI_07_05_ChangeHistoryNonExistent(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	d := driver.NewInProcess(t, fixture)
	bogus := uuid.New()
	status, body, err := d.GetEntityChangesRaw(bogus)
	if err != nil {
		t.Fatalf("GetEntityChangesRaw: %v", err)
	}
	errorcontract.Match(t, status, body, errorcontract.ExpectedError{
		HTTPStatus: http.StatusNotFound,
		ErrorCode:  "ENTITY_NOT_FOUND",
	})
}
