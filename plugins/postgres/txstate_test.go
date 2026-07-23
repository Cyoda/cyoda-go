package postgres

import (
	"errors"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// TestNewTxState_ZeroValue verifies that a freshly constructed txState has
// the expected tenantID and empty/nil collections.
func TestNewTxState_ZeroValue(t *testing.T) {
	tid := spi.TenantID("tenant-1")
	s := newTxState(tid)

	if s.tenantID != tid {
		t.Errorf("tenantID = %q, want %q", s.tenantID, tid)
	}
	if s.readSet == nil {
		t.Error("readSet is nil, want empty map")
	}
	if len(s.readSet) != 0 {
		t.Errorf("readSet len = %d, want 0", len(s.readSet))
	}
	if s.writeSet == nil {
		t.Error("writeSet is nil, want empty map")
	}
	if len(s.writeSet) != 0 {
		t.Errorf("writeSet len = %d, want 0", len(s.writeSet))
	}
	if s.savepoints != nil {
		t.Errorf("savepoints = %v, want nil", s.savepoints)
	}
}

// TestRecordRead_FirstReadWins verifies that the first read version is
// captured and a subsequent read of the same entity is ignored.
func TestRecordRead_FirstReadWins(t *testing.T) {
	s := newTxState("t1")
	s.RecordRead("e1", 5)
	s.RecordRead("e1", 7) // should be ignored
	if got := s.readSet["e1"]; got != 5 {
		t.Errorf("readSet[e1] = %d, want 5", got)
	}
	if len(s.readSet) != 1 {
		t.Errorf("readSet len = %d, want 1", len(s.readSet))
	}
}

// TestRecordRead_SkipIfWritten verifies that RecordRead is a no-op when the
// entity is already in writeSet.
func TestRecordRead_SkipIfWritten(t *testing.T) {
	s := newTxState("t1")
	s.writeSet["e1"] = 3 // pre-populate directly
	s.RecordRead("e1", 7)
	if _, ok := s.readSet["e1"]; ok {
		t.Error("readSet should not contain e1 after writeSet pre-populate")
	}
	if s.writeSet["e1"] != 3 {
		t.Errorf("writeSet[e1] = %d, want 3", s.writeSet["e1"])
	}
}

// TestRecordRead_MultipleEntities verifies that distinct entities are
// recorded independently.
func TestRecordRead_MultipleEntities(t *testing.T) {
	s := newTxState("t1")
	s.RecordRead("e1", 1)
	s.RecordRead("e2", 2)
	s.RecordRead("e3", 3)
	if got := s.readSet["e1"]; got != 1 {
		t.Errorf("readSet[e1] = %d, want 1", got)
	}
	if got := s.readSet["e2"]; got != 2 {
		t.Errorf("readSet[e2] = %d, want 2", got)
	}
	if got := s.readSet["e3"]; got != 3 {
		t.Errorf("readSet[e3] = %d, want 3", got)
	}
	if len(s.readSet) != 3 {
		t.Errorf("readSet len = %d, want 3", len(s.readSet))
	}
}

// TestRecordWrite_FirstWriteWins verifies that the first write version is
// kept and subsequent writes of the same entity are ignored.
func TestRecordWrite_FirstWriteWins(t *testing.T) {
	s := newTxState("t1")
	s.RecordWrite("e1", 5)
	s.RecordWrite("e1", 7) // should be ignored
	if got := s.writeSet["e1"]; got != 5 {
		t.Errorf("writeSet[e1] = %d, want 5", got)
	}
	if len(s.writeSet) != 1 {
		t.Errorf("writeSet len = %d, want 1", len(s.writeSet))
	}
}

// TestRecordWrite_PromotesFromReadSet verifies that an entity already in
// readSet is promoted to writeSet using the readSet's captured version
// and removed from readSet.
func TestRecordWrite_PromotesFromReadSet(t *testing.T) {
	s := newTxState("t1")
	s.RecordRead("e1", 5)
	s.RecordWrite("e1", 5)
	if _, ok := s.readSet["e1"]; ok {
		t.Error("e1 should have been removed from readSet after promotion")
	}
	if got := s.writeSet["e1"]; got != 5 {
		t.Errorf("writeSet[e1] = %d, want 5", got)
	}
}

// TestRecordWrite_FreshInsertZero verifies that a fresh insert (version 0)
// is recorded in writeSet with value 0.
func TestRecordWrite_FreshInsertZero(t *testing.T) {
	s := newTxState("t1")
	s.RecordWrite("e1", 0)
	if got, ok := s.writeSet["e1"]; !ok || got != 0 {
		t.Errorf("writeSet[e1] = %d (ok=%v), want 0 and present", got, ok)
	}
}

// TestValidateReadSet_AllMatch verifies that no error is returned when all
// readSet entities match the current snapshot.
func TestValidateReadSet_AllMatch(t *testing.T) {
	s := newTxState("t1")
	s.RecordRead("e1", 5)
	s.RecordRead("e2", 10)
	current := map[string]int64{"e1": 5, "e2": 10, "e3": 99}
	if err := s.ValidateReadSet(current); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestValidateReadSet_VersionMismatch verifies that a version mismatch
// returns an error containing the entity ID.
func TestValidateReadSet_VersionMismatch(t *testing.T) {
	s := newTxState("t1")
	s.RecordRead("e1", 5)
	current := map[string]int64{"e1": 6}
	err := s.ValidateReadSet(current)
	if err == nil {
		t.Fatal("expected error for version mismatch, got nil")
	}
	if !strings.Contains(err.Error(), "e1") {
		t.Errorf("error does not mention entity ID: %v", err)
	}
}

// TestValidateReadSet_MissingEntity verifies that a deleted entity (absent
// from current snapshot) returns an error.
func TestValidateReadSet_MissingEntity(t *testing.T) {
	s := newTxState("t1")
	s.RecordRead("e1", 5)
	current := map[string]int64{}
	err := s.ValidateReadSet(current)
	if err == nil {
		t.Fatal("expected error for missing entity, got nil")
	}
	if !strings.Contains(err.Error(), "e1") {
		t.Errorf("error does not mention entity ID: %v", err)
	}
}

// TestPushSavepoint_DeepCopiesSets verifies that PushSavepoint stores
// independent copies of readSet and writeSet (mutations after push don't
// affect the snapshot).
func TestPushSavepoint_DeepCopiesSets(t *testing.T) {
	s := newTxState("t1")
	s.RecordRead("e1", 5)
	s.RecordWrite("e2", 10)
	s.PushSavepoint("sp1", nil, nil)

	// Mutate state after the push.
	s.RecordRead("e3", 99)
	s.RecordWrite("e4", 77)

	if len(s.savepoints) != 1 {
		t.Fatalf("savepoints len = %d, want 1", len(s.savepoints))
	}
	snap := s.savepoints[0]
	if _, ok := snap.readSet["e3"]; ok {
		t.Error("snapshot readSet should not contain e3 added after push")
	}
	if _, ok := snap.writeSet["e4"]; ok {
		t.Error("snapshot writeSet should not contain e4 added after push")
	}
	// Original entries should be in snapshot.
	if snap.readSet["e1"] != 5 {
		t.Errorf("snapshot readSet[e1] = %d, want 5", snap.readSet["e1"])
	}
	if snap.writeSet["e2"] != 10 {
		t.Errorf("snapshot writeSet[e2] = %d, want 10", snap.writeSet["e2"])
	}
}

// TestRestoreSavepoint_RestoresSets verifies that RestoreSavepoint reverts
// readSet and writeSet to the snapshot and that the savepoint itself is
// preserved (postgres ROLLBACK TO SAVEPOINT semantics).
func TestRestoreSavepoint_RestoresSets(t *testing.T) {
	s := newTxState("t1")
	s.RecordRead("e1", 5)
	s.PushSavepoint("sp1", nil, nil)

	// Do more work after the savepoint.
	s.RecordRead("e2", 20)
	s.RecordWrite("e3", 30)

	_, _, err := s.RestoreSavepoint("sp1")
	if err != nil {
		t.Fatalf("RestoreSavepoint: %v", err)
	}

	// Sets should be back to snapshot state.
	if len(s.readSet) != 1 || s.readSet["e1"] != 5 {
		t.Errorf("readSet after restore = %v, want {e1:5}", s.readSet)
	}
	if len(s.writeSet) != 0 {
		t.Errorf("writeSet after restore = %v, want empty", s.writeSet)
	}
	// Savepoint itself is preserved.
	if len(s.savepoints) != 1 || s.savepoints[0].id != "sp1" {
		t.Errorf("savepoints after restore = %v, want [sp1]", s.savepoints)
	}
}

// TestRestoreSavepoint_TrimsLaterSavepoints verifies that restoring sp1
// trims sp2 (which was pushed after sp1) but keeps sp1.
func TestRestoreSavepoint_TrimsLaterSavepoints(t *testing.T) {
	s := newTxState("t1")
	s.PushSavepoint("sp1", nil, nil)
	s.RecordRead("e1", 1)
	s.PushSavepoint("sp2", nil, nil)

	_, _, err := s.RestoreSavepoint("sp1")
	if err != nil {
		t.Fatalf("RestoreSavepoint: %v", err)
	}

	if len(s.savepoints) != 1 {
		t.Errorf("savepoints len = %d, want 1", len(s.savepoints))
	}
	if s.savepoints[0].id != "sp1" {
		t.Errorf("savepoints[0].id = %q, want sp1", s.savepoints[0].id)
	}
}

// TestReleaseSavepoint_DropsEntryKeepsWork verifies that ReleaseSavepoint
// removes the savepoint entry but leaves the current readSet/writeSet intact.
func TestReleaseSavepoint_DropsEntryKeepsWork(t *testing.T) {
	s := newTxState("t1")
	s.PushSavepoint("sp1", nil, nil)
	s.RecordRead("e1", 5)
	s.RecordWrite("e2", 10)

	if err := s.ReleaseSavepoint("sp1"); err != nil {
		t.Fatalf("ReleaseSavepoint: %v", err)
	}

	if len(s.savepoints) != 0 {
		t.Errorf("savepoints len = %d, want 0", len(s.savepoints))
	}
	// Work done after the push is preserved.
	if s.readSet["e1"] != 5 {
		t.Errorf("readSet[e1] = %d, want 5", s.readSet["e1"])
	}
	if s.writeSet["e2"] != 10 {
		t.Errorf("writeSet[e2] = %d, want 10", s.writeSet["e2"])
	}
}

// TestRestoreSavepoint_Unknown verifies that restoring an unknown savepoint
// returns spi.ErrSavepointNotFound.
func TestRestoreSavepoint_Unknown(t *testing.T) {
	s := newTxState("t1")
	_, _, err := s.RestoreSavepoint("nonexistent")
	if err == nil {
		t.Fatal("expected error for unknown savepoint, got nil")
	}
	if !errors.Is(err, spi.ErrSavepointNotFound) {
		t.Fatalf("expected ErrSavepointNotFound, got: %v", err)
	}
}

// TestReleaseSavepoint_Unknown verifies that releasing an unknown savepoint
// returns spi.ErrSavepointNotFound.
func TestReleaseSavepoint_Unknown(t *testing.T) {
	s := newTxState("t1")
	err := s.ReleaseSavepoint("bogus")
	if err == nil {
		t.Fatal("expected error for unknown savepoint, got nil")
	}
	if !errors.Is(err, spi.ErrSavepointNotFound) {
		t.Fatalf("expected ErrSavepointNotFound, got: %v", err)
	}
}
