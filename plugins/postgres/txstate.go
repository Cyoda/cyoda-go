package postgres

import (
	"context"
	"fmt"
	"sort"
	"sync"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// txState holds per-transaction bookkeeping for first-committer-wins
// validation. One instance per active tx, indexed by txID on the
// TransactionManager.
//
// Invariants:
//   - An entity ID appears in at most one of readSet/writeSet at any time.
//   - readSet[id] = the version as first observed by a Get within this tx.
//   - writeSet[id] = the pre-write version for an entity we updated or deleted.
//
// writeSet is maintained for the readSet-disjoint invariant (RecordRead skips
// entities in writeSet, keeping readSet correct) and for future use with
// advisory locks / non-entity stores (tracked as #35). writeSet is NOT
// validated at commit time in the current implementation — PostgreSQL's
// native tuple-level DML locks catch write-write conflicts.
//
// See docs/superpowers/specs/2026-04-15-postgres-si-first-committer-wins-design.md
// for the full semantic model.
type txState struct {
	mu         sync.Mutex
	tenantID   spi.TenantID
	readSet    map[string]int64
	writeSet   map[string]int64
	savepoints []savepointEntry
}

type savepointEntry struct {
	id       string
	readSet  map[string]int64
	writeSet map[string]int64
	// deletes/deleteAttribution are a bookkeeping-only snapshot of the SPI
	// TransactionState's Deletes/DeleteAttribution maps at Savepoint time
	// (see TransactionManager.Savepoint/RollbackToSavepoint). Postgres's own
	// persistence never restores from these — PostgreSQL's own SAVEPOINT/
	// ROLLBACK TO SAVEPOINT already governs actual visibility — they exist
	// solely to satisfy the SPI's Deletes/DeleteAttribution contract for
	// callers that inspect TransactionState directly.
	deletes           map[string]bool
	deleteAttribution map[string]spi.WriteAttribution
}

func newTxState(tenantID spi.TenantID) *txState {
	return &txState{
		tenantID: tenantID,
		readSet:  make(map[string]int64),
		writeSet: make(map[string]int64),
	}
}

// RecordRead records a read of the given entity at the given version.
//
// Invariants enforced:
//   - No-op if id ∈ writeSet: we wrote it; our own writes don't need
//     cross-tx read validation.
//   - No-op if id ∈ readSet: first-read-wins — we capture the version we
//     made decisions on, not a later re-read.
func (s *txState) RecordRead(id string, version int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Invariant: writeSet takes precedence — skip if we already wrote this entity.
	if _, inWrite := s.writeSet[id]; inWrite {
		return
	}
	// Invariant: first-read-wins — skip if already in readSet.
	if _, inRead := s.readSet[id]; inRead {
		return
	}
	s.readSet[id] = version
}

// RecordWrite records a write (save/delete) of the given entity with the
// given pre-write version. Pass 0 for a fresh insert.
//
// Invariants enforced:
//   - First-write-wins: if id ∈ writeSet, keep the original pre-write version.
//   - Promotion: if id ∈ readSet, move to writeSet using the readSet's captured
//     version (they agree by construction) and remove from readSet.
func (s *txState) RecordWrite(id string, preWriteVersion int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Invariant: first-write-wins — keep original pre-write version.
	if _, inWrite := s.writeSet[id]; inWrite {
		return
	}
	// Invariant: readSet promotion — if we read it, promote to writeSet using
	// the read's captured version (not the caller's preWriteVersion, which must
	// agree but the readSet version is the authoritative captured value).
	if readVersion, inRead := s.readSet[id]; inRead {
		s.writeSet[id] = readVersion
		delete(s.readSet, id)
		return
	}
	s.writeSet[id] = preWriteVersion
}

// SortedReadIDs returns a sorted slice of entity IDs in readSet only.
// Used by Commit to restrict the FOR SHARE validation query to entities we
// read but did not write in this transaction. Write-write conflicts are
// detected by PostgreSQL's tuple-level locks (SQLSTATE 40001), so writeSet
// entities do not need to be included in the validation query.
func (s *txState) SortedReadIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.readSet))
	for id := range s.readSet {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// ValidateReadSet checks that every entity in readSet still exists in
// the DB at the captured version. Returns an error describing the first
// mismatch; nil if all match.
func (s *txState) ValidateReadSet(current map[string]int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, expected := range s.readSet {
		got, ok := current[id]
		if !ok {
			return fmt.Errorf("read-set validation: entity %s deleted by concurrent committer (expected version %d)", id, expected)
		}
		if got != expected {
			return fmt.Errorf("read-set validation: entity %s version changed: expected %d, current %d", id, expected, got)
		}
	}
	return nil
}

// PushSavepoint stores a deep copy of the current readSet/writeSet (and the
// caller-supplied deletes/deleteAttribution snapshot — see
// TransactionManager.Savepoint) under the given savepoint ID. Subsequent
// RestoreSavepoint(id) restores all of them to this snapshot and trims later
// savepoints (postgres nested savepoint semantics). deletes/deleteAttribution
// may be nil (e.g. no tx was in scope when the snapshot was taken); the
// stored copies are always non-nil, matching readSet/writeSet's behaviour.
func (s *txState) PushSavepoint(id string, deletes map[string]bool, deleteAttribution map[string]spi.WriteAttribution) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := savepointEntry{
		id:                id,
		readSet:           make(map[string]int64, len(s.readSet)),
		writeSet:          make(map[string]int64, len(s.writeSet)),
		deletes:           make(map[string]bool, len(deletes)),
		deleteAttribution: make(map[string]spi.WriteAttribution, len(deleteAttribution)),
	}
	for k, v := range s.readSet {
		snap.readSet[k] = v
	}
	for k, v := range s.writeSet {
		snap.writeSet[k] = v
	}
	for k, v := range deletes {
		snap.deletes[k] = v
	}
	for k, v := range deleteAttribution {
		snap.deleteAttribution[k] = v
	}
	s.savepoints = append(s.savepoints, snap)
}

// HasSavepoint reports whether a savepoint with the given id is currently
// on the snapshot stack. Used by the TM lifecycle methods to pre-validate
// the savepoint exists before issuing the SQL command, so missing
// savepoints surface as spi.ErrSavepointNotFound rather than an opaque
// PostgreSQL SQLSTATE 3B001 error.
func (s *txState) HasSavepoint(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sp := range s.savepoints {
		if sp.id == id {
			return true
		}
	}
	return false
}

// RestoreSavepoint restores readSet/writeSet to the snapshot captured at
// PushSavepoint(id) and trims any savepoints pushed after id. The named
// savepoint itself remains (mirroring postgres ROLLBACK TO SAVEPOINT). The
// snapshotted deletes/deleteAttribution are returned (not applied here —
// they belong to the *spi.TransactionState obtained via ctx, not this
// package-internal struct) so the caller (TransactionManager.
// RollbackToSavepoint) can restore them.
func (s *txState) RestoreSavepoint(id string) (deletes map[string]bool, deleteAttribution map[string]spi.WriteAttribution, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := -1
	for i, sp := range s.savepoints {
		if sp.id == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil, nil, fmt.Errorf("%w (savepointID=%q)", spi.ErrSavepointNotFound, id)
	}
	snap := s.savepoints[idx]
	s.readSet = make(map[string]int64, len(snap.readSet))
	s.writeSet = make(map[string]int64, len(snap.writeSet))
	for k, v := range snap.readSet {
		s.readSet[k] = v
	}
	for k, v := range snap.writeSet {
		s.writeSet[k] = v
	}
	s.savepoints = s.savepoints[:idx+1]

	deletes = make(map[string]bool, len(snap.deletes))
	for k, v := range snap.deletes {
		deletes[k] = v
	}
	deleteAttribution = make(map[string]spi.WriteAttribution, len(snap.deleteAttribution))
	for k, v := range snap.deleteAttribution {
		deleteAttribution[k] = v
	}
	return deletes, deleteAttribution, nil
}

// recordReadIfInTx records a read into the tx's state, if the context
// carries a transaction. No-op for non-tx reads.
func (tm *TransactionManager) recordReadIfInTx(ctx context.Context, entityID string, version int64) {
	txState := spi.GetTransaction(ctx)
	if txState == nil {
		return
	}
	s, ok := tm.lookupTxState(txState.ID)
	if !ok {
		return
	}
	s.RecordRead(entityID, version)
}

// recordWriteIfInTx records a write into the tx's state, if the context
// carries a transaction. No-op for non-tx writes.
func (tm *TransactionManager) recordWriteIfInTx(ctx context.Context, entityID string, preWriteVersion int64) {
	txState := spi.GetTransaction(ctx)
	if txState == nil {
		return
	}
	s, ok := tm.lookupTxState(txState.ID)
	if !ok {
		return
	}
	s.RecordWrite(entityID, preWriteVersion)
}

// ReleaseSavepoint drops the savepoint entry without touching the current
// readSet/writeSet — work done after the push is kept. Mirrors postgres
// RELEASE SAVEPOINT semantics.
func (s *txState) ReleaseSavepoint(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := -1
	for i, sp := range s.savepoints {
		if sp.id == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("%w (savepointID=%q)", spi.ErrSavepointNotFound, id)
	}
	s.savepoints = append(s.savepoints[:idx], s.savepoints[idx+1:]...)
	return nil
}
