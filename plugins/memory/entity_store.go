package memory

import (
	"context"
	"fmt"
	"iter"
	"sort"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// entityVersion is one version of an entity in the per-entity history.
//
// Invariant: once an entityVersion is appended to the per-entity []entityVersion
// slice and the write lock is released, its fields are NEVER mutated. Iterators
// and snapshots may hold *entityVersion or *spi.Entity (via the .entity field)
// pointers and read them lock-free after releasing the read lock. This invariant
// is load-bearing for the snapshot-then-iterate pattern in Iterate / GroupedAggregator.
//
// If you add a code path that mutates a published entityVersion, fix the
// invariant doc here AND audit the memory plugin's Iterate/GroupedAggregator
// implementations.
type entityVersion struct {
	entity *spi.Entity
	// version is this row's version number, populated for EVERY row
	// including DELETED tombstones (entity is nil there, so Meta.Version
	// is unavailable — this field is the single source of truth GetVersionMetadata
	// and GetVersionByTransaction read, fixing a prior zero-Version tombstone
	// bug where a deleted row's version was unrecoverable).
	version       int64
	transactionID string
	submitTime    time.Time // set at save time (or at transaction commit time later)
	deleted       bool
	changeType    string
	user          string
	// changeUserKind and executor carry the follow-on-action attribution
	// (see docs/superpowers/plans/2026-07-23-attribute-followon-actions.md).
	// Populated independently of entity — entity is nil for DELETED
	// versions, but attribution must not be (see GetVersionMetadata).
	changeUserKind spi.PrincipalKind
	executor       spi.Principal
	// modelRef records the entity's model reference for a DELETED tombstone
	// only (entity is nil there, so it carries no ModelRef of its own).
	// Needed because a transaction that creates and deletes the same id
	// before committing never separately flushes the create — the buffered
	// create is evicted from tx.Buffer by Delete, so the tombstone this
	// commit appends is the ONLY committed version, and it is the sole
	// place left to record the model an entity-immutability check (see
	// modelRefOfLocked) can read once that create is gone. txmanager.Commit's
	// delete flush is the ONLY site that populates it, from the transaction's
	// own buffered-then-evicted create (see stageDeletedBufferModel), and only
	// when the entity has no prior committed version at all. Left zero-value
	// on every other tombstone, because modelRefOfLocked never needs it there:
	// a live version elsewhere in the history already carries the model.
	//
	// The non-transactional Delete path (see Delete) deliberately sets nothing
	// here and needs nothing: it refuses to delete an entity whose latest
	// version is absent or already a tombstone, so a live version — the one it
	// just read — always remains ahead of the tombstone it appends, and
	// firstNonTombstone finds the model there.
	modelRef spi.ModelRef
}

// firstNonTombstone returns the first version in versions with a non-nil
// entity — i.e. the entity's earliest still-recoverable snapshot. A
// version's entity is nil only for a DELETED tombstone (see entityVersion's
// doc comment above); versions[0] is NOT guaranteed to be one, because a
// transaction that creates and deletes the same id before committing never
// separately flushes the create (see modelRef's doc comment) — the
// committed history in that case begins with a tombstone. Callers that need
// the entity's original model/creation-date and find no non-nil entity here
// must fall back to a tombstone's own modelRef (see modelRefOfLocked).
func firstNonTombstone(versions []entityVersion) (*spi.Entity, bool) {
	for _, v := range versions {
		if v.entity != nil {
			return v.entity, true
		}
	}
	return nil, false
}

type EntityStore struct {
	tenant  spi.TenantID
	factory *StoreFactory
}

// unstageDelete removes a staged delete for id from BOTH maps the delete
// occupies (Deletes and DeleteAttribution always cover the same key set).
func unstageDelete(tx *spi.TransactionState, id string) {
	delete(tx.Deletes, id)
	delete(tx.DeleteAttribution, id)
}

func copyEntity(e *spi.Entity) *spi.Entity {
	cp := &spi.Entity{Meta: e.Meta, Data: make([]byte, len(e.Data))}
	copy(cp.Data, e.Data)
	return cp
}

// deriveChangeType computes the ChangeType to record for a save the same way
// plugins/sqlite and plugins/postgres do: DERIVED from whether the entity ID
// already has a prior version in the store (row-existence), never trusted
// verbatim from the caller. hasPriorVersion mirrors sqlite/postgres's isNew
// check exactly — it is true whenever entityID has ANY prior version, live
// or soft-deleted (a fresh save's tombstone-recreate case is "UPDATED", not
// "CREATED" — an already-existing entities row, same as sqlite/postgres,
// which never delete the row, only flag it deleted).
//
// A caller-supplied ChangeType is trusted only when hasPriorVersion is true
// and the value is neither "" nor "CREATED" — e.g. an explicit "DELETED"
// stamped by the delete path. This is the one case where the caller, not
// row-existence, is authoritative. Any other case (including a save that
// fetched a stale "CREATED" from an entity read earlier, e.g. a
// scheduled-transition fire re-saving an already-existing entity) is
// overridden — that staleness is exactly the bug this derivation fixes.
func deriveChangeType(callerChangeType string, hasPriorVersion bool) string {
	if !hasPriorVersion {
		return "CREATED"
	}
	if callerChangeType == "" || callerChangeType == "CREATED" {
		return "UPDATED"
	}
	return callerChangeType
}

// getSnapshotVersion walks the version history for entityID and returns the
// latest version whose submitTime <= snapshotTime. Caller must hold at least
// s.factory.entityMu.RLock().
func (s *EntityStore) getSnapshotVersion(entityID string, snapshotTime time.Time) (*spi.Entity, error) {
	versions, ok := s.factory.entityData[s.tenant][entityID]
	if !ok || len(versions) == 0 {
		return nil, fmt.Errorf("entity %s: %w", entityID, spi.ErrNotFound)
	}

	var result *spi.Entity
	var wasDeleted bool
	for _, v := range versions {
		if !v.submitTime.After(snapshotTime) {
			if v.deleted {
				result = nil
				wasDeleted = true
			} else {
				result = v.entity
				wasDeleted = false
			}
		} else {
			break
		}
	}
	if result == nil {
		if wasDeleted {
			return nil, fmt.Errorf("entity %s was deleted at requested time: %w", entityID, spi.ErrNotFound)
		}
		return nil, fmt.Errorf("entity %s: %w", entityID, spi.ErrNotFound)
	}
	return copyEntity(result), nil
}

// modelRefOfLocked returns the model reference id was first saved under, and
// whether id has any known history at all. An entity's model reference never
// changes after creation (see spi.ErrEntityModelMismatch), so this is the
// entity's model for its whole lifetime, including through delete/recreate.
//
// versions[0].entity is NOT always non-nil: a transaction that creates and
// deletes the same id before committing never separately flushes the
// create — only the tombstone txmanager.Commit's delete flush appends is
// committed, at index 0, with a nil entity (see entityVersion's doc
// comment and firstNonTombstone). This function therefore prefers the
// first version with a live entity, and falls back to a tombstone's own
// stamped modelRef when every version is a tombstone (that stamp is what
// makes the model recoverable even though the create itself never
// materialized as a live version — see modelRef's field doc). A tombstone
// with a zero modelRef reports "unknown" (false) rather than a false
// positive/negative on an empty ModelRef.
//
// Caller must hold s.factory.entityMu (read or write).
func (s *EntityStore) modelRefOfLocked(tid spi.TenantID, id string) (spi.ModelRef, bool) {
	versions := s.factory.entityData[tid][id]
	if e, ok := firstNonTombstone(versions); ok {
		return e.Meta.ModelRef, true
	}
	for _, v := range versions {
		if v.deleted && v.modelRef != (spi.ModelRef{}) {
			return v.modelRef, true
		}
	}
	return spi.ModelRef{}, false
}

// modelMismatchErr wraps spi.ErrEntityModelMismatch for entity id.
func modelMismatchErr(id string) error {
	return fmt.Errorf("entity %s: %w", id, spi.ErrEntityModelMismatch)
}

// checkModelImmutable enforces that a same-transaction Save/CompareAndSave
// does not change entity's model reference, comparing against the
// transaction's own earlier buffered write for this id when one exists
// (last-write-wins to a mismatched model is still a mismatch), and against
// committed state otherwise — a same-tx staged delete does not exempt this
// check, since the entity's model is fixed for the entity ID regardless of
// delete/recreate.
func (s *EntityStore) checkModelImmutable(tx *spi.TransactionState, entity *spi.Entity) error {
	id := entity.Meta.ID
	if buffered, ok := tx.Buffer[id]; ok {
		if buffered.Meta.ModelRef != entity.Meta.ModelRef {
			return modelMismatchErr(id)
		}
		return nil
	}
	var mismatch bool
	func() {
		s.factory.entityMu.RLock()
		defer s.factory.entityMu.RUnlock()
		if ref, ok := s.modelRefOfLocked(s.tenant, id); ok && ref != entity.Meta.ModelRef {
			mismatch = true
		}
	}()
	if mismatch {
		return modelMismatchErr(id)
	}
	return nil
}

func (s *EntityStore) SaveAll(ctx context.Context, entities iter.Seq[*spi.Entity]) ([]int64, error) {
	return spi.DefaultSaveAll(s, ctx, entities)
}

func (s *EntityStore) Save(ctx context.Context, entity *spi.Entity) (int64, error) {
	tx := spi.GetTransaction(ctx)
	if tx != nil {
		// Hold tx.OpMu.RLock for the duration of the buffer mutation so
		// that Commit/Rollback (which take tx.OpMu.Lock) cannot race with
		// writes to tx.Buffer / tx.WriteSet. Lock order matches
		// txmanager.Commit: tx.OpMu before factory.entityMu.
		tx.OpMu.RLock()
		defer tx.OpMu.RUnlock()
		if tx.RolledBack {
			return 0, fmt.Errorf("Save: %w (txID=%s)", spi.ErrTxRolledBack, tx.ID)
		}
		if tx.Closed {
			return 0, fmt.Errorf("Save: %w (txID=%s)", spi.ErrTxAlreadyCommitted, tx.ID)
		}
		// An entity's model reference is fixed at creation (see
		// spi.EntityMeta.ModelRef's doc comment); reject a same-tx buffered
		// write that would change it BEFORE it is buffered — a buffered
		// write already looks committed to every subsequent read this
		// transaction takes, so deferring the check to flush time would let
		// the caller believe a rejected write had succeeded.
		if err := s.checkModelImmutable(tx, entity); err != nil {
			return 0, err
		}
		// Transaction mode: write to buffer, not main store. Stage any
		// value this overwrites (see stageSuperseded's godoc) BEFORE
		// overwriting tx.Buffer, so a same-tx double-save of the same
		// entity can still flush both versions at Commit.
		cp := copyEntity(entity)
		cp.Meta.TransactionID = tx.ID
		s.factory.txManager.stageSuperseded(tx.ID, entity.Meta.ID, tx.Buffer[entity.Meta.ID])
		tx.Buffer[entity.Meta.ID] = cp
		tx.WriteSet[entity.Meta.ID] = true
		// If the entity was previously marked for deletion in this tx, unmark it
		// (last-write-wins: Save-after-Delete → present). Keeps tx.Buffer and
		// tx.Deletes/DeleteAttribution mutually exclusive, the invariant
		// txmanager.Commit assumes and that Search / Iterate / commit all rely
		// on to agree. Mirrors plugins/sqlite/entity_store.go Save.
		unstageDelete(tx, entity.Meta.ID)
		// Capture unique keys at buffer time (last-write-wins, matching tx.Buffer
		// semantics). Commit sees ONE ctx but a mixed-model batch may buffer
		// entities with different key contexts, so keys must be stored per-entity.
		s.factory.txManager.recordUniqueKeys(tx.ID, entity.Meta.ID, spi.UniqueKeysFromContext(ctx))
		return 0, nil // actual version assigned at commit
	}

	// Non-transaction mode: direct write (implicit auto-commit).
	s.factory.entityMu.Lock()
	defer s.factory.entityMu.Unlock()
	return s.saveUnlocked(ctx, entity)
}

// CompareAndSave writes entity only if its stored transaction ID is still
// expectedTxID. expectedTxID must not be empty: it is compared literally, and
// the empty string is the transaction ID a missing or deleted entity reports
// — but also the one a write taken outside a transaction stores verbatim when
// the caller supplied none, so an empty expected ID cannot tell "no entity"
// from "an entity written outside a transaction" and would overwrite the
// latter. It is rejected as a caller error, before any read or write.
// CompareAndSave therefore never creates an entity and never resurrects a
// deleted one; Save does that.
func (s *EntityStore) CompareAndSave(ctx context.Context, entity *spi.Entity, expectedTxID string) (int64, error) {
	if expectedTxID == "" {
		return 0, fmt.Errorf("CompareAndSave: expectedTxID must not be empty")
	}
	tx := spi.GetTransaction(ctx)
	if tx != nil {
		// Hold tx.OpMu.RLock for the duration of the CAS+buffer mutation
		// so that Commit/Rollback (which take tx.OpMu.Lock) cannot race
		// with writes to tx.Buffer / tx.WriteSet. Lock order matches
		// txmanager.Commit: tx.OpMu before factory.entityMu.
		tx.OpMu.RLock()
		defer tx.OpMu.RUnlock()
		if tx.RolledBack {
			return 0, fmt.Errorf("CompareAndSave: %w (txID=%s)", spi.ErrTxRolledBack, tx.ID)
		}
		if tx.Closed {
			return 0, fmt.Errorf("CompareAndSave: %w (txID=%s)", spi.ErrTxAlreadyCommitted, tx.ID)
		}
		// A write compares against the transaction's own view, and a same-tx
		// delete means the transaction sees NO entity: the current
		// transaction ID is "". expectedTxID is non-empty here, so it names a
		// version the delete has already superseded and conflicts. Nothing
		// re-creates the entity through this method — Save does that, and it
		// unstages the delete. Same answer postgres gives.
		if tx.Deletes[entity.Meta.ID] {
			return 0, fmt.Errorf("CompareAndSave %s: %w", entity.Meta.ID, spi.ErrConflict)
		}
		// Same enforcement as Save's tx branch — see checkModelImmutable.
		if err := s.checkModelImmutable(tx, entity); err != nil {
			return 0, err
		}
		// A buffered own-write IS the transaction's current version of this
		// entity, so the comparison is against it, not against the committed
		// row it supersedes. An expected ID naming the buffered version
		// matches — that is how a joined callback updates an entity created
		// earlier in the same transaction; only a stale expected ID (the
		// committed version's) conflicts. The buffered entity carries this
		// transaction's ID because every in-tx write is stamped with it at
		// write time — the same value postgres's own uncommitted row holds,
		// which is what its CAS reads off its own connection.
		if buffered, ok := tx.Buffer[entity.Meta.ID]; ok {
			if buffered.Meta.TransactionID != expectedTxID {
				return 0, fmt.Errorf("CompareAndSave %s: %w", entity.Meta.ID, spi.ErrConflict)
			}
			cp := copyEntity(entity)
			cp.Meta.TransactionID = tx.ID
			s.factory.txManager.stageSuperseded(tx.ID, entity.Meta.ID, buffered)
			tx.Buffer[entity.Meta.ID] = cp
			tx.WriteSet[entity.Meta.ID] = true
			s.factory.txManager.recordUniqueKeys(tx.ID, entity.Meta.ID, spi.UniqueKeysFromContext(ctx))
			return 0, nil
		}
		// Check CAS against main store (committed data), not buffer.
		// Hold entityMu.RLock through both version check AND buffer write
		// to prevent TOCTOU. Wrap in an IIFE so the unlock runs via defer
		// (per .claude/rules/go-mutex-discipline.md).
		var conflict bool
		func() {
			s.factory.entityMu.RLock()
			defer s.factory.entityMu.RUnlock()
			if currentTxIDLocked(s.factory.entityData[s.tenant][entity.Meta.ID]) != expectedTxID {
				conflict = true
				return
			}
			// Write to buffer under the same lock hold. Stage any value
			// this overwrites (see stageSuperseded's godoc) first.
			cp := copyEntity(entity)
			cp.Meta.TransactionID = tx.ID
			s.factory.txManager.stageSuperseded(tx.ID, entity.Meta.ID, tx.Buffer[entity.Meta.ID])
			tx.Buffer[entity.Meta.ID] = cp
			tx.WriteSet[entity.Meta.ID] = true
		}()
		if conflict {
			return 0, spi.ErrConflict
		}
		// Capture unique keys at buffer time (outside the IIFE; entityMu released).
		s.factory.txManager.recordUniqueKeys(tx.ID, entity.Meta.ID, spi.UniqueKeysFromContext(ctx))
		return 0, nil
	}

	// Non-transaction: the same literal comparison, against committed data.
	s.factory.entityMu.Lock()
	defer s.factory.entityMu.Unlock()

	if currentTxIDLocked(s.factory.entityData[s.tenant][entity.Meta.ID]) != expectedTxID {
		return 0, spi.ErrConflict
	}

	return s.saveUnlocked(ctx, entity)
}

// currentTxIDLocked returns the transaction ID a compare-and-save compares
// against: the latest committed version's, or "" when there is no entity —
// never written, or deleted. A deleted entity is no entity, exactly as Get
// reports it, so its tombstone does not expose the superseded version's ID.
// "" is also what an entity written outside a transaction with no supplied ID
// stores; the two are indistinguishable here, which is why CompareAndSave
// rejects an empty expectedTxID rather than letting it match either.
// The caller must hold factory.entityMu (read or write).
func currentTxIDLocked(versions []entityVersion) string {
	if len(versions) == 0 {
		return ""
	}
	latest := versions[len(versions)-1]
	if latest.deleted || latest.entity == nil {
		return ""
	}
	return latest.entity.Meta.TransactionID
}

// saveUnlocked performs the save logic without acquiring the lock. The caller
// must hold s.factory.entityMu (write lock). ctx is used to read unique keys
// for composite unique-key claim enforcement.
func (s *EntityStore) saveUnlocked(ctx context.Context, entity *spi.Entity) (int64, error) {
	tid := s.tenant
	eid := entity.Meta.ID

	// Compute and validate unique-key claims before writing entity data.
	// All steps run inside the caller's entityMu.Lock() so the check-and-insert
	// is atomic with respect to other concurrent non-tx saves.
	keys := spi.UniqueKeysFromContext(ctx)
	newClaims, err := spi.ComputeClaims(keys, entity.Data)
	if err != nil {
		return 0, err // ErrPartialUniqueKey family
	}
	model := entity.Meta.ModelRef.EntityName
	version := entity.Meta.ModelRef.ModelVersion
	for _, c := range newClaims {
		k := claimKey{tenant: string(tid), model: model, version: version, keyID: c.KeyID, signature: c.Signature}
		if holder, exists := s.factory.uniqueClaims[k]; exists && holder != eid {
			return 0, spi.ErrUniqueViolation
		}
	}

	if s.factory.entityData[tid] == nil {
		s.factory.entityData[tid] = make(map[string][]entityVersion)
	}

	if ref, ok := s.modelRefOfLocked(tid, eid); ok && ref != entity.Meta.ModelRef {
		return 0, modelMismatchErr(eid)
	}

	versions := s.factory.entityData[tid][eid]
	var nextVersion int64 = 1
	if len(versions) > 0 {
		for i := len(versions) - 1; i >= 0; i-- {
			if !versions[i].deleted {
				nextVersion = versions[i].entity.Meta.Version + 1
				break
			}
		}
	}

	// Stamped under the monotonic floor a commit uses (nextSubmitTime), not
	// the raw clock: the floor can stand ahead of the clock, and Begin floors
	// a new transaction's SnapshotTime to it, so a raw-clock stamp could land
	// at or below a snapshot already open.
	now := s.factory.txManager.nextSubmitTime()
	changeType := deriveChangeType(entity.Meta.ChangeType, len(versions) > 0)

	// versions[0] is not guaranteed to carry a live entity — see
	// firstNonTombstone's doc comment (a same-tx create+delete commits a
	// tombstone at index 0). Recreating an id whose whole history is
	// tombstone(s) has no recoverable original creation date, so it falls
	// through to the "no versions" branch below, same as a brand-new id.
	//
	// A caller-supplied CreationDate is IGNORED, not used as a fallback: the
	// creation date is the store's, not the caller's (see
	// spi.EntityMeta.CreationDate). The engine builds an entity with its own
	// clock BEFORE it opens a transaction, so honouring that value dated a
	// created entity at the moment the write started rather than the moment
	// it was published — a gap as long as the transaction, processor
	// callouts included. Pinned by spitest's Save/CallerCreationDateIgnored.
	creationDate := now
	if e, ok := firstNonTombstone(versions); ok {
		creationDate = e.Meta.CreationDate
	}

	saved := &spi.Entity{
		Meta: spi.EntityMeta{
			ID:                      eid,
			TenantID:                tid,
			ModelRef:                entity.Meta.ModelRef,
			State:                   entity.Meta.State,
			Version:                 nextVersion,
			CreationDate:            creationDate,
			LastModifiedDate:        now,
			TransactionID:           entity.Meta.TransactionID,
			ChangeType:              changeType,
			ChangeUser:              entity.Meta.ChangeUser,
			ChangeUserKind:          entity.Meta.ChangeUserKind,
			ChangeExecutor:          entity.Meta.ChangeExecutor,
			TransitionForLatestSave: entity.Meta.TransitionForLatestSave,
		},
		Data: make([]byte, len(entity.Data)),
	}
	copy(saved.Data, entity.Data)

	// invariant: appended versions are immutable post-publish; see entityVersion godoc.
	s.factory.entityData[tid][eid] = append(versions, entityVersion{
		entity:         saved,
		version:        nextVersion,
		transactionID:  entity.Meta.TransactionID,
		submitTime:     now,
		changeType:     changeType,
		user:           entity.Meta.ChangeUser,
		changeUserKind: entity.Meta.ChangeUserKind,
		executor:       entity.Meta.ChangeExecutor,
	})
	s.factory.recordTxIndex(tid, eid, entity.Meta.TransactionID, nextVersion)

	// Apply unique-key claims: release old (handles update-moves-key) then insert new.
	s.factory.releaseClaims(string(tid), eid)
	s.factory.insertClaims(eid, string(tid), model, version, newClaims)

	return nextVersion, nil
}

func (s *EntityStore) Get(ctx context.Context, entityID string) (*spi.Entity, error) {
	tx := spi.GetTransaction(ctx)
	if tx != nil {
		// Hold tx.OpMu.RLock for the duration of the tx-state reads and
		// the ReadSet write so Commit/Rollback (which take tx.OpMu.Lock)
		// cannot race with us. Lock order matches txmanager.Commit:
		// tx.OpMu before factory.entityMu.
		tx.OpMu.RLock()
		defer tx.OpMu.RUnlock()
		if tx.RolledBack {
			return nil, fmt.Errorf("Get: %w (txID=%s)", spi.ErrTxRolledBack, tx.ID)
		}
		if tx.Closed {
			return nil, fmt.Errorf("Get: %w (txID=%s)", spi.ErrTxAlreadyCommitted, tx.ID)
		}
		// Check if deleted in this transaction.
		if tx.Deletes[entityID] {
			return nil, fmt.Errorf("entity %s: %w", entityID, spi.ErrNotFound)
		}
		// Check buffer first (read-your-own-writes).
		if buffered, ok := tx.Buffer[entityID]; ok {
			tx.ReadSet[entityID] = true
			return copyEntity(buffered), nil
		}
		// Fall back to main store with snapshot read.
		s.factory.entityMu.RLock()
		defer s.factory.entityMu.RUnlock()
		tx.ReadSet[entityID] = true
		return s.getSnapshotVersion(entityID, tx.SnapshotTime)
	}

	// Non-transaction: existing behavior (latest committed).
	s.factory.entityMu.RLock()
	defer s.factory.entityMu.RUnlock()

	versions, ok := s.factory.entityData[s.tenant][entityID]
	if !ok || len(versions) == 0 {
		return nil, fmt.Errorf("entity %s: %w", entityID, spi.ErrNotFound)
	}
	latest := versions[len(versions)-1]
	if latest.deleted {
		return nil, fmt.Errorf("entity %s: %w", entityID, spi.ErrNotFound)
	}
	return copyEntity(latest.entity), nil
}

func (s *EntityStore) GetAsAt(ctx context.Context, entityID string, asAt time.Time) (*spi.Entity, error) {
	// Take tx.OpMu BEFORE factory.entityMu to preserve the lock order
	// established by Save/CompareAndSave and txmanager.Commit. Historical
	// queries always read committed data, but the in-tx tx.RolledBack
	// read and tx.ReadSet write must be serialised against Commit/Rollback
	// (which take tx.OpMu.Lock).
	if tx := spi.GetTransaction(ctx); tx != nil {
		tx.OpMu.RLock()
		defer tx.OpMu.RUnlock()
		if tx.RolledBack {
			return nil, fmt.Errorf("GetAsAt: %w (txID=%s)", spi.ErrTxRolledBack, tx.ID)
		}
		if tx.Closed {
			return nil, fmt.Errorf("GetAsAt: %w (txID=%s)", spi.ErrTxAlreadyCommitted, tx.ID)
		}
		tx.ReadSet[entityID] = true
	}

	s.factory.entityMu.RLock()
	defer s.factory.entityMu.RUnlock()

	versions, ok := s.factory.entityData[s.tenant][entityID]
	if !ok || len(versions) == 0 {
		return nil, fmt.Errorf("entity %s: %w", entityID, spi.ErrNotFound)
	}

	var result *spi.Entity
	var wasDeleted bool
	for _, v := range versions {
		if !v.submitTime.After(asAt) {
			if v.deleted {
				result = nil
				wasDeleted = true
			} else {
				result = v.entity
				wasDeleted = false
			}
		} else {
			break
		}
	}
	if result == nil {
		if wasDeleted {
			return nil, fmt.Errorf("entity %s was deleted at requested time: %w", entityID, spi.ErrNotFound)
		}
		return nil, fmt.Errorf("no version of entity %s exists at %v: %w", entityID, asAt, spi.ErrNotFound)
	}
	return copyEntity(result), nil
}

func (s *EntityStore) Delete(ctx context.Context, entityID string) error {
	tx := spi.GetTransaction(ctx)
	if tx != nil {
		// Hold tx.OpMu.RLock for the duration of the existence check and
		// the tx.Deletes / tx.Buffer / tx.WriteSet mutations so Commit/
		// Rollback (which take tx.OpMu.Lock) cannot race with us. Lock
		// order: tx.OpMu before factory.entityMu.
		tx.OpMu.RLock()
		defer tx.OpMu.RUnlock()
		if tx.RolledBack {
			return fmt.Errorf("Delete: %w (txID=%s)", spi.ErrTxRolledBack, tx.ID)
		}
		if tx.Closed {
			return fmt.Errorf("Delete: %w (txID=%s)", spi.ErrTxAlreadyCommitted, tx.ID)
		}
		// Check existence: buffer first, then committed store. Wrap the
		// entityMu hold in an IIFE so the unlock runs via defer.
		if buffered, inBuffer := tx.Buffer[entityID]; !inBuffer {
			var versions []entityVersion
			func() {
				s.factory.entityMu.RLock()
				defer s.factory.entityMu.RUnlock()
				versions = s.factory.entityData[s.tenant][entityID]
			}()
			if len(versions) == 0 {
				return fmt.Errorf("entity %s: %w", entityID, spi.ErrNotFound)
			}
			latest := versions[len(versions)-1]
			if latest.deleted {
				return fmt.Errorf("entity %s: %w", entityID, spi.ErrNotFound)
			}
		} else {
			// entityID is a same-tx buffered create/update with no prior
			// committed version. Commit's delete flush evicts it from
			// tx.Buffer below without ever separately flushing it, so the
			// tombstone that flush appends is the ONLY committed version —
			// stage its model now so that flush can stamp it (see
			// deletedBufferModels's field doc); otherwise a later
			// Save/CompareAndSave against this id could never tell what
			// model it was created under.
			s.factory.txManager.stageDeletedBufferModel(tx.ID, entityID, buffered.Meta.ModelRef)
		}
		tx.Deletes[entityID] = true
		delete(tx.Buffer, entityID) // remove from buffer if present
		tx.WriteSet[entityID] = true
		// Capture attribution at STAGE time (this caller), not at commit
		// time (the eventual committer, possibly a different actor) — see
		// txmanager.Commit's flush, which prefers this entry over
		// re-deriving attribution from the commit ctx.
		a, e := spi.AttributionFor(ctx)
		tx.DeleteAttribution[entityID] = spi.WriteAttribution{Attributed: a, Executor: e}
		return nil
	}

	// Non-transaction: existing behavior.
	s.factory.entityMu.Lock()
	defer s.factory.entityMu.Unlock()

	versions, ok := s.factory.entityData[s.tenant][entityID]
	if !ok || len(versions) == 0 {
		return fmt.Errorf("entity %s: %w", entityID, spi.ErrNotFound)
	}
	latest := versions[len(versions)-1]
	if latest.deleted {
		return fmt.Errorf("entity %s: %w", entityID, spi.ErrNotFound)
	}
	attributed, executor := spi.AttributionFor(ctx)
	// latest.deleted was already checked false above, so latest.entity is
	// guaranteed non-nil here.
	// Stamped under the monotonic floor — see saveUnlocked.
	deletedAt := s.factory.txManager.nextSubmitTime()
	s.factory.entityData[s.tenant][entityID] = append(versions, entityVersion{
		entity:         nil,
		version:        latest.entity.Meta.Version + 1,
		transactionID:  "",
		submitTime:     deletedAt,
		deleted:        true,
		changeType:     "DELETED",
		user:           attributed.ID,
		changeUserKind: attributed.Kind,
		executor:       executor,
	})
	// Release unique-key claims so the freed values can be claimed immediately.
	s.factory.releaseClaims(string(s.tenant), entityID)
	return nil
}

func (s *EntityStore) DeleteAll(ctx context.Context, modelRef spi.ModelRef) error {
	tx := spi.GetTransaction(ctx)
	if tx != nil {
		// Hold tx.OpMu.RLock for the duration of the snapshot read and
		// the iteration/mutation of tx.Buffer / tx.Deletes / tx.WriteSet
		// so Commit/Rollback (which take tx.OpMu.Lock) cannot race with
		// us. Lock order: tx.OpMu before factory.entityMu.
		tx.OpMu.RLock()
		defer tx.OpMu.RUnlock()
		if tx.RolledBack {
			return fmt.Errorf("DeleteAll: %w (txID=%s)", spi.ErrTxRolledBack, tx.ID)
		}
		if tx.Closed {
			return fmt.Errorf("DeleteAll: %w (txID=%s)", spi.ErrTxAlreadyCommitted, tx.ID)
		}
		// Get all entities for the model (snapshot), mark each as deleted
		// in tx. Wrap the entityMu hold in an IIFE so the unlock runs via
		// defer. We only need each entity's id, so the pointer snapshot
		// (no payload deep-copy) is enough.
		var mainEntities []*spi.Entity
		var snapErr error
		func() {
			s.factory.entityMu.RLock()
			defer s.factory.entityMu.RUnlock()
			mainEntities, snapErr = s.getAllSnapshotPointersUnlocked(ctx, modelRef, tx.SnapshotTime)
		}()
		if snapErr != nil {
			return fmt.Errorf("DeleteAll: %w", snapErr)
		}

		// Attribution captured once for this call (this caller, this ctx)
		// and applied to every entity ID this DeleteAll stages — same
		// stage-time-not-commit-time posture as single-entity Delete.
		a, e := spi.AttributionFor(ctx)
		attribution := spi.WriteAttribution{Attributed: a, Executor: e}

		for _, ent := range mainEntities {
			tx.Deletes[ent.Meta.ID] = true
			delete(tx.Buffer, ent.Meta.ID)
			tx.WriteSet[ent.Meta.ID] = true
			tx.DeleteAttribution[ent.Meta.ID] = attribution
		}
		// Also delete any buffered entities for this model.
		// First pass: collect IDs to delete (avoid iterate-during-mutation).
		toDelete := make([]string, 0)
		for id, ent := range tx.Buffer {
			if ent.Meta.ModelRef == modelRef {
				toDelete = append(toDelete, id)
			}
		}
		// Second pass: delete. Same same-tx create-then-delete gap as
		// single-entity Delete — stage each evicted buffered entity's model
		// before evicting it, so a same-tx-created-then-DeleteAll'd id whose
		// create never separately flushed still leaves its model recoverable
		// on the tombstone Commit's delete flush appends (see
		// deletedBufferModels's field doc).
		for _, id := range toDelete {
			s.factory.txManager.stageDeletedBufferModel(tx.ID, id, tx.Buffer[id].Meta.ModelRef)
			delete(tx.Buffer, id)
			tx.Deletes[id] = true
			tx.WriteSet[id] = true
			tx.DeleteAttribution[id] = attribution
		}
		return nil
	}

	// Non-transaction: existing behavior.
	s.factory.entityMu.Lock()
	defer s.factory.entityMu.Unlock()

	// Stamped under the monotonic floor — see saveUnlocked. One stamp for the
	// whole sweep: a non-transactional DeleteAll is a single write.
	now := s.factory.txManager.nextSubmitTime()
	attributed, executor := spi.AttributionFor(ctx)
	for eid, versions := range s.factory.entityData[s.tenant] {
		if len(versions) == 0 {
			continue
		}
		latest := versions[len(versions)-1]
		if latest.deleted {
			continue
		}
		if latest.entity.Meta.ModelRef == modelRef {
			s.factory.entityData[s.tenant][eid] = append(versions, entityVersion{
				entity:         nil,
				version:        latest.entity.Meta.Version + 1,
				transactionID:  "",
				submitTime:     now,
				deleted:        true,
				changeType:     "DELETED",
				user:           attributed.ID,
				changeUserKind: attributed.Kind,
				executor:       executor,
			})
			// Release unique-key claims so freed values can be claimed immediately.
			s.factory.releaseClaims(string(s.tenant), eid)
		}
	}
	return nil
}

func (s *EntityStore) Exists(ctx context.Context, entityID string) (bool, error) {
	tx := spi.GetTransaction(ctx)
	if tx != nil {
		// Hold tx.OpMu.RLock for the duration of the tx-state reads so
		// Commit/Rollback (which take tx.OpMu.Lock) cannot race with us.
		// Lock order: tx.OpMu before factory.entityMu.
		tx.OpMu.RLock()
		defer tx.OpMu.RUnlock()
		if tx.RolledBack {
			return false, fmt.Errorf("Exists: %w (txID=%s)", spi.ErrTxRolledBack, tx.ID)
		}
		if tx.Closed {
			return false, fmt.Errorf("Exists: %w (txID=%s)", spi.ErrTxAlreadyCommitted, tx.ID)
		}
		// Check deletes first.
		if tx.Deletes[entityID] {
			return false, nil
		}
		// Check buffer.
		if _, ok := tx.Buffer[entityID]; ok {
			return true, nil
		}
		// Fall back to snapshot.
		s.factory.entityMu.RLock()
		defer s.factory.entityMu.RUnlock()
		_, err := s.getSnapshotVersion(entityID, tx.SnapshotTime)
		return err == nil, nil
	}

	// Non-transaction: existing behavior.
	s.factory.entityMu.RLock()
	defer s.factory.entityMu.RUnlock()
	versions, ok := s.factory.entityData[s.tenant][entityID]
	if !ok || len(versions) == 0 {
		return false, nil
	}
	return !versions[len(versions)-1].deleted, nil
}

// countTx tallies the transaction's view of modelRef without copying: the
// committed pointer snapshot at tx.SnapshotTime minus staged deletes, plus
// buffered own-writes of the model. Caller holds tx.OpMu.RLock.
func (s *EntityStore) countTx(ctx context.Context, tx *spi.TransactionState, modelRef spi.ModelRef, tally func(state string)) error {
	var committed []*spi.Entity
	var snapErr error
	func() {
		s.factory.entityMu.RLock()
		defer s.factory.entityMu.RUnlock()
		committed, snapErr = s.getAllSnapshotPointersUnlocked(ctx, modelRef, tx.SnapshotTime)
	}()
	if snapErr != nil {
		return snapErr
	}
	for i, e := range committed {
		if i&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if tx.Deletes[e.Meta.ID] {
			continue
		}
		// Only a buffered write of the SAME model supersedes this committed
		// row — that is how GetPage, Search/Iterate and buildSnapshot overlay
		// the buffer. A buffered write of another model leaves the committed row
		// standing in this model's view, so it must still be tallied.
		if b, buffered := tx.Buffer[e.Meta.ID]; buffered && b.Meta.ModelRef == modelRef {
			continue // the buffered version is tallied below
		}
		tally(e.Meta.State)
	}
	for id, e := range tx.Buffer {
		if e.Meta.ModelRef != modelRef || tx.Deletes[id] {
			continue
		}
		tally(e.Meta.State)
	}
	return nil
}

func (s *EntityStore) Count(ctx context.Context, modelRef spi.ModelRef) (int64, error) {
	tx := spi.GetTransaction(ctx)
	if tx != nil {
		tx.OpMu.RLock()
		defer tx.OpMu.RUnlock()
		if tx.RolledBack {
			return 0, fmt.Errorf("Count: %w (txID=%s)", spi.ErrTxRolledBack, tx.ID)
		}
		if tx.Closed {
			return 0, fmt.Errorf("Count: %w (txID=%s)", spi.ErrTxAlreadyCommitted, tx.ID)
		}
		var n int64
		if err := s.countTx(ctx, tx, modelRef, func(string) { n++ }); err != nil {
			return 0, fmt.Errorf("Count: %w", err)
		}
		return n, nil
	}

	// Non-transaction: existing behavior.
	s.factory.entityMu.RLock()
	defer s.factory.entityMu.RUnlock()

	var count int64
	for _, versions := range s.factory.entityData[s.tenant] {
		if len(versions) == 0 {
			continue
		}
		latest := versions[len(versions)-1]
		if latest.deleted {
			continue
		}
		if latest.entity.Meta.ModelRef == modelRef {
			count++
		}
	}
	return count, nil
}

// CountByState returns counts of non-deleted entities grouped by state for the
// given model. See SPI godoc on EntityStore.CountByState for filter semantics.
func (s *EntityStore) CountByState(ctx context.Context, modelRef spi.ModelRef, states []string) (map[string]int64, error) {
	if states != nil && len(states) == 0 {
		return map[string]int64{}, nil
	}

	var filter map[string]struct{}
	if states != nil {
		filter = make(map[string]struct{}, len(states))
		for _, st := range states {
			filter[st] = struct{}{}
		}
	}

	tx := spi.GetTransaction(ctx)
	if tx != nil {
		tx.OpMu.RLock()
		defer tx.OpMu.RUnlock()
		if tx.RolledBack {
			return nil, fmt.Errorf("CountByState: %w (txID=%s)", spi.ErrTxRolledBack, tx.ID)
		}
		if tx.Closed {
			return nil, fmt.Errorf("CountByState: %w (txID=%s)", spi.ErrTxAlreadyCommitted, tx.ID)
		}
		result := make(map[string]int64)
		err := s.countTx(ctx, tx, modelRef, func(st string) {
			if filter != nil {
				if _, ok := filter[st]; !ok {
					return
				}
			}
			result[st]++
		})
		if err != nil {
			return nil, fmt.Errorf("CountByState: %w", err)
		}
		return result, nil
	}

	// Non-transaction: iterate latest versions directly.
	s.factory.entityMu.RLock()
	defer s.factory.entityMu.RUnlock()

	result := make(map[string]int64)
	for _, versions := range s.factory.entityData[s.tenant] {
		if len(versions) == 0 {
			continue
		}
		latest := versions[len(versions)-1]
		if latest.deleted {
			continue
		}
		if latest.entity.Meta.ModelRef != modelRef {
			continue
		}
		st := latest.entity.Meta.State
		if filter != nil {
			if _, ok := filter[st]; !ok {
				continue
			}
		}
		result[st]++
	}
	return result, nil
}

// currentStatePointersUnlocked returns the latest non-deleted *spi.Entity
// pointer (uncopied — see copyEntity call sites for where copies are made)
// per entity matching modelRef. Caller must hold at least
// s.factory.entityMu.RLock().
func (s *EntityStore) currentStatePointersUnlocked(ctx context.Context, modelRef spi.ModelRef) ([]*spi.Entity, error) {
	result := make([]*spi.Entity, 0)
	i := 0
	for _, versions := range s.factory.entityData[s.tenant] {
		if i&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		i++
		if len(versions) == 0 {
			continue
		}
		latest := versions[len(versions)-1]
		if latest.deleted {
			continue
		}
		if latest.entity.Meta.ModelRef == modelRef {
			result = append(result, latest.entity)
		}
	}
	return result, nil
}

// pageSlice sorts rows byte-wise by entity ID — memory's documented
// canonical order (see spi.OrderSpec's doc comment on Source=SourceMeta,
// Path="id") — and returns a copied [offset:offset+limit) window. rows are
// NOT copied before this call; only the entities that end up on the
// returned page are copied, per GetPage's efficiency contract.
func pageSlice(rows []*spi.Entity, limit, offset int) []*spi.Entity {
	sort.Slice(rows, func(i, j int) bool { return rows[i].Meta.ID < rows[j].Meta.ID })
	if offset >= len(rows) {
		return []*spi.Entity{}
	}
	end := offset + limit
	if end > len(rows) {
		end = len(rows)
	}
	page := rows[offset:end]
	out := make([]*spi.Entity, len(page))
	for i, e := range page {
		out[i] = copyEntity(e)
	}
	return out
}

// GetPage returns a page of modelRef's entities in memory's canonical
// byte-wise entity-ID order. See the spi.EntityStore.GetPage doc comment
// for the full contract: limit>=1 && offset>=0 is required; asAt==nil
// reads the live (in-tx overlay when a transaction is ambient) view and,
// in-tx, unconditionally records every returned entity in the
// transaction's read-set (unlike Search/Iterate's opt-in TrackingRead);
// asAt!=nil ignores any ambient transaction and reads committed-only
// state as of that instant.
func (s *EntityStore) GetPage(ctx context.Context, modelRef spi.ModelRef, limit, offset int, asAt *time.Time) ([]*spi.Entity, error) {
	if limit < 1 {
		return nil, fmt.Errorf("GetPage: limit must be >= 1")
	}
	if offset < 0 {
		return nil, fmt.Errorf("GetPage: offset must be >= 0")
	}

	if asAt != nil {
		// Committed-only PIT snapshot — ignores any ambient transaction's
		// overlay, mirroring Search/Iterate's PointInTime branch.
		s.factory.entityMu.RLock()
		defer s.factory.entityMu.RUnlock()
		rows, err := s.getAllSnapshotPointersUnlocked(ctx, modelRef, *asAt)
		if err != nil {
			return nil, fmt.Errorf("GetPage: %w", err)
		}
		return pageSlice(rows, limit, offset), nil
	}

	tx := spi.GetTransaction(ctx)
	if tx == nil {
		s.factory.entityMu.RLock()
		defer s.factory.entityMu.RUnlock()
		rows, err := s.currentStatePointersUnlocked(ctx, modelRef)
		if err != nil {
			return nil, fmt.Errorf("GetPage: %w", err)
		}
		return pageSlice(rows, limit, offset), nil
	}

	// In-tx: merged committed ∪ write-set view. Hold tx.OpMu.RLock for the
	// duration so Commit/Rollback (tx.OpMu.Lock) cannot race with our reads
	// of tx.Buffer/tx.Deletes and our write to tx.ReadSet. Lock order:
	// tx.OpMu before factory.entityMu (matches Save/Search).
	tx.OpMu.RLock()
	defer tx.OpMu.RUnlock()
	if tx.RolledBack {
		return nil, fmt.Errorf("GetPage: %w (txID=%s)", spi.ErrTxRolledBack, tx.ID)
	}
	if tx.Closed {
		return nil, fmt.Errorf("GetPage: %w (txID=%s)", spi.ErrTxAlreadyCommitted, tx.ID)
	}

	var mainEntities []*spi.Entity
	var snapErr error
	func() {
		s.factory.entityMu.RLock()
		defer s.factory.entityMu.RUnlock()
		mainEntities, snapErr = s.getAllSnapshotPointersUnlocked(ctx, modelRef, tx.SnapshotTime)
	}()
	if snapErr != nil {
		return nil, fmt.Errorf("GetPage: %w", snapErr)
	}

	merged := make(map[string]*spi.Entity, len(mainEntities))
	for _, e := range mainEntities {
		if !tx.Deletes[e.Meta.ID] {
			merged[e.Meta.ID] = e
		}
	}
	for id, e := range tx.Buffer {
		if e.Meta.ModelRef == modelRef {
			merged[id] = e
		}
	}
	rows := make([]*spi.Entity, 0, len(merged))
	for _, e := range merged {
		rows = append(rows, e)
	}
	page := pageSlice(rows, limit, offset)
	// Unconditional: every entity on the returned page enters the
	// transaction's read-set — no TrackingRead knob, per GetPage's SPI doc
	// comment (unlike Search/Iterate's opt-in TrackingRead).
	for _, e := range page {
		tx.ReadSet[e.Meta.ID] = true
	}
	return page, nil
}

// GetVersionByTransaction returns the earliest (lowest-Version) version of
// entityID written by transaction txID. DELETED tombstones never match
// (they carry no entity payload — see the SPI doc comment) and an empty
// txID never matches, even a stored-empty one from a non-transactional
// write.
func (s *EntityStore) GetVersionByTransaction(ctx context.Context, entityID, txID string) (*spi.EntityVersion, error) {
	if txID == "" {
		return nil, fmt.Errorf("entity %s: %w", entityID, spi.ErrNotFound)
	}

	s.factory.entityMu.RLock()
	defer s.factory.entityMu.RUnlock()

	version, ok := s.factory.txIndex[s.tenant][entityID][txID]
	if !ok {
		return nil, fmt.Errorf("entity %s: %w", entityID, spi.ErrNotFound)
	}
	for _, v := range s.factory.entityData[s.tenant][entityID] {
		if v.deleted || v.entity == nil {
			continue
		}
		if v.entity.Meta.Version != version {
			continue
		}
		return &spi.EntityVersion{
			Entity:         copyEntity(v.entity),
			ChangeType:     v.changeType,
			User:           v.user,
			Timestamp:      v.submitTime,
			Version:        v.entity.Meta.Version,
			AttributedKind: v.changeUserKind,
			Executor:       v.executor,
		}, nil
	}
	// The index points at a version that is no longer present as a live
	// row (should not happen — txIndex only ever records non-deleted
	// saves). Treat defensively as not found rather than panicking.
	return nil, fmt.Errorf("entity %s: %w", entityID, spi.ErrNotFound)
}

// GetVersionMetadata returns entityID's version metadata — no entity
// payload — newest first, ties broken by Version DESC. opts.From/
// opts.Until bound the window inclusively (nil side unbounded); opts.Limit
// caps the row count (0 means all). Deleted is true only on the DELETED
// tombstone row, and Version is populated on every returned row, including
// the tombstone (entityVersion.version is the source of truth for this —
// see its doc comment).
func (s *EntityStore) GetVersionMetadata(ctx context.Context, entityID string, opts spi.VersionMetadataOptions) ([]spi.EntityVersionMeta, error) {
	s.factory.entityMu.RLock()
	defer s.factory.entityMu.RUnlock()

	versions, ok := s.factory.entityData[s.tenant][entityID]
	if !ok || len(versions) == 0 {
		return nil, fmt.Errorf("entity %s: %w", entityID, spi.ErrNotFound)
	}

	result := make([]spi.EntityVersionMeta, 0, len(versions))
	for _, v := range versions {
		if opts.From != nil && v.submitTime.Before(*opts.From) {
			continue
		}
		if opts.Until != nil && v.submitTime.After(*opts.Until) {
			continue
		}
		result = append(result, spi.EntityVersionMeta{
			Version:        v.version,
			ChangeType:     v.changeType,
			Timestamp:      v.submitTime,
			User:           v.user,
			AttributedKind: v.changeUserKind,
			Executor:       v.executor,
			TransactionID:  v.transactionID,
			Deleted:        v.changeType == "DELETED",
		})
	}

	// Newest first, ties broken by Version DESC.
	sort.Slice(result, func(i, j int) bool {
		if !result[i].Timestamp.Equal(result[j].Timestamp) {
			return result[i].Timestamp.After(result[j].Timestamp)
		}
		return result[i].Version > result[j].Version
	})

	if opts.Limit > 0 && len(result) > opts.Limit {
		result = result[:opts.Limit]
	}
	return result, nil
}
