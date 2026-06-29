package memory

import (
	"context"
	"fmt"
	"iter"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// entityVersion is one version of an entity in the per-entity history.
//
// Invariant: once an entityVersion is appended to the per-entity []entityVersion
// slice and the write lock is released, its fields are NEVER mutated. Iterators
// and snapshots may hold *entityVersion or *spi.Entity (via the .entity field)
// pointers and read them lock-free after releasing the read lock. This invariant
// is load-bearing for the snapshot-then-iterate pattern in Iterable / GroupedAggregator.
//
// If you add a code path that mutates a published entityVersion, fix the
// invariant doc here AND audit the memory plugin's Iterable/GroupedAggregator
// implementations.
type entityVersion struct {
	entity        *spi.Entity
	transactionID string
	submitTime    time.Time // set at save time (or at transaction commit time later)
	deleted       bool
	changeType    string
	user          string
}

type EntityStore struct {
	tenant  spi.TenantID
	factory *StoreFactory
}

func copyEntity(e *spi.Entity) *spi.Entity {
	cp := &spi.Entity{Meta: e.Meta, Data: make([]byte, len(e.Data))}
	copy(cp.Data, e.Data)
	return cp
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

// getAllSnapshotUnlocked returns all entities matching modelRef that were visible
// at snapshotTime. Caller must hold at least s.factory.entityMu.RLock().
func (s *EntityStore) getAllSnapshotUnlocked(modelRef spi.ModelRef, snapshotTime time.Time) []*spi.Entity {
	var result []*spi.Entity
	for _, versions := range s.factory.entityData[s.tenant] {
		if len(versions) == 0 {
			continue
		}
		var found *spi.Entity
		for _, v := range versions {
			if !v.submitTime.After(snapshotTime) {
				if v.deleted {
					found = nil
				} else {
					found = v.entity
				}
			} else {
				break
			}
		}
		if found != nil && found.Meta.ModelRef == modelRef {
			result = append(result, copyEntity(found))
		}
	}
	return result
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
		// Transaction mode: write to buffer, not main store.
		cp := copyEntity(entity)
		tx.Buffer[entity.Meta.ID] = cp
		tx.WriteSet[entity.Meta.ID] = true
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

func (s *EntityStore) CompareAndSave(ctx context.Context, entity *spi.Entity, expectedTxID string) (int64, error) {
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
		// Check CAS against main store (committed data), not buffer.
		// Hold entityMu.RLock through both version check AND buffer write
		// to prevent TOCTOU. Wrap in an IIFE so the unlock runs via defer
		// (per .claude/rules/go-mutex-discipline.md).
		var conflict bool
		func() {
			s.factory.entityMu.RLock()
			defer s.factory.entityMu.RUnlock()
			versions := s.factory.entityData[s.tenant][entity.Meta.ID]
			if len(versions) > 0 {
				for i := len(versions) - 1; i >= 0; i-- {
					if !versions[i].deleted && versions[i].entity != nil {
						if versions[i].entity.Meta.TransactionID != expectedTxID {
							conflict = true
							return
						}
						break
					}
				}
			}
			// Write to buffer under the same lock hold.
			cp := copyEntity(entity)
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

	// Non-transaction: existing behavior.
	s.factory.entityMu.Lock()
	defer s.factory.entityMu.Unlock()

	tid := s.tenant
	eid := entity.Meta.ID

	if versions, ok := s.factory.entityData[tid][eid]; ok && len(versions) > 0 {
		for i := len(versions) - 1; i >= 0; i-- {
			if !versions[i].deleted && versions[i].entity != nil {
				if versions[i].entity.Meta.TransactionID != expectedTxID {
					return 0, spi.ErrConflict
				}
				break
			}
		}
	}

	return s.saveUnlocked(ctx, entity)
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

	now := s.factory.clock.Now()

	creationDate := entity.Meta.CreationDate
	if len(versions) > 0 {
		creationDate = versions[0].entity.Meta.CreationDate
	} else if creationDate.IsZero() {
		creationDate = now
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
			ChangeType:              entity.Meta.ChangeType,
			ChangeUser:              entity.Meta.ChangeUser,
			TransitionForLatestSave: entity.Meta.TransitionForLatestSave,
		},
		Data: make([]byte, len(entity.Data)),
	}
	copy(saved.Data, entity.Data)

	// invariant: appended versions are immutable post-publish; see entityVersion godoc.
	s.factory.entityData[tid][eid] = append(versions, entityVersion{
		entity:        saved,
		transactionID: entity.Meta.TransactionID,
		submitTime:    now,
		changeType:    entity.Meta.ChangeType,
		user:          entity.Meta.ChangeUser,
	})

	// Apply unique-key claims: release old (handles update-moves-key) then insert new.
	s.factory.releaseClaims(eid)
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

func (s *EntityStore) GetAll(ctx context.Context, modelRef spi.ModelRef) ([]*spi.Entity, error) {
	tx := spi.GetTransaction(ctx)
	if tx != nil {
		// Hold tx.OpMu.RLock for the duration of the tx-state reads and
		// the ReadSet writes so Commit/Rollback (which take tx.OpMu.Lock)
		// cannot race with our iteration of tx.Buffer / tx.Deletes.
		// Lock order: tx.OpMu before factory.entityMu.
		tx.OpMu.RLock()
		defer tx.OpMu.RUnlock()
		if tx.RolledBack {
			return nil, fmt.Errorf("GetAll: %w (txID=%s)", spi.ErrTxRolledBack, tx.ID)
		}
		// Combine: snapshot of main store + buffer - deletes. Wrap the
		// entityMu hold in an IIFE so the unlock runs via defer (per
		// .claude/rules/go-mutex-discipline.md — bare Unlock is not the
		// right answer).
		var mainEntities []*spi.Entity
		func() {
			s.factory.entityMu.RLock()
			defer s.factory.entityMu.RUnlock()
			mainEntities = s.getAllSnapshotUnlocked(modelRef, tx.SnapshotTime)
		}()

		result := make(map[string]*spi.Entity)
		for _, e := range mainEntities {
			if !tx.Deletes[e.Meta.ID] {
				result[e.Meta.ID] = e
				tx.ReadSet[e.Meta.ID] = true
			}
		}
		// Overlay buffer.
		for id, e := range tx.Buffer {
			if e.Meta.ModelRef == modelRef {
				result[id] = copyEntity(e)
				tx.ReadSet[id] = true
			}
		}

		entities := make([]*spi.Entity, 0, len(result))
		for _, e := range result {
			entities = append(entities, e)
		}
		return entities, nil
	}

	// Non-transaction: existing behavior.
	s.factory.entityMu.RLock()
	defer s.factory.entityMu.RUnlock()

	result := make([]*spi.Entity, 0)
	for _, versions := range s.factory.entityData[s.tenant] {
		if len(versions) == 0 {
			continue
		}
		latest := versions[len(versions)-1]
		if latest.deleted {
			continue
		}
		if latest.entity.Meta.ModelRef == modelRef {
			result = append(result, copyEntity(latest.entity))
		}
	}
	return result, nil
}

func (s *EntityStore) GetAllAsAt(ctx context.Context, modelRef spi.ModelRef, asAt time.Time) ([]*spi.Entity, error) {
	s.factory.entityMu.RLock()
	defer s.factory.entityMu.RUnlock()

	// Historical query: always reads committed data.
	result := make([]*spi.Entity, 0)
	for _, versions := range s.factory.entityData[s.tenant] {
		if len(versions) == 0 {
			continue
		}

		var found *spi.Entity
		var wasDeleted bool
		for _, v := range versions {
			if !v.submitTime.After(asAt) {
				if v.deleted {
					found = nil
					wasDeleted = true
				} else {
					found = v.entity
					wasDeleted = false
				}
			} else {
				break
			}
		}
		_ = wasDeleted
		if found != nil && found.Meta.ModelRef == modelRef {
			result = append(result, copyEntity(found))
		}
	}
	return result, nil
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
		// Check existence: buffer first, then committed store. Wrap the
		// entityMu hold in an IIFE so the unlock runs via defer.
		if _, inBuffer := tx.Buffer[entityID]; !inBuffer {
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
		}
		tx.Deletes[entityID] = true
		delete(tx.Buffer, entityID) // remove from buffer if present
		tx.WriteSet[entityID] = true
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
	uc := spi.GetUserContext(ctx)
	userName := ""
	if uc != nil {
		userName = uc.UserID
	}
	s.factory.entityData[s.tenant][entityID] = append(versions, entityVersion{
		entity:        nil,
		transactionID: "",
		submitTime:    s.factory.clock.Now(),
		deleted:       true,
		changeType:    "DELETED",
		user:          userName,
	})
	// Release unique-key claims so the freed values can be claimed immediately.
	s.factory.releaseClaims(entityID)
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
		// Get all entities for the model (snapshot), mark each as deleted
		// in tx. Wrap the entityMu hold in an IIFE so the unlock runs via
		// defer.
		var mainEntities []*spi.Entity
		func() {
			s.factory.entityMu.RLock()
			defer s.factory.entityMu.RUnlock()
			mainEntities = s.getAllSnapshotUnlocked(modelRef, tx.SnapshotTime)
		}()

		for _, e := range mainEntities {
			tx.Deletes[e.Meta.ID] = true
			delete(tx.Buffer, e.Meta.ID)
			tx.WriteSet[e.Meta.ID] = true
		}
		// Also delete any buffered entities for this model.
		// First pass: collect IDs to delete (avoid iterate-during-mutation).
		toDelete := make([]string, 0)
		for id, e := range tx.Buffer {
			if e.Meta.ModelRef == modelRef {
				toDelete = append(toDelete, id)
			}
		}
		// Second pass: delete.
		for _, id := range toDelete {
			delete(tx.Buffer, id)
			tx.Deletes[id] = true
			tx.WriteSet[id] = true
		}
		return nil
	}

	// Non-transaction: existing behavior.
	s.factory.entityMu.Lock()
	defer s.factory.entityMu.Unlock()

	now := s.factory.clock.Now()
	uc := spi.GetUserContext(ctx)
	userName := ""
	if uc != nil {
		userName = uc.UserID
	}
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
				entity:        nil,
				transactionID: "",
				submitTime:    now,
				deleted:       true,
				changeType:    "DELETED",
				user:          userName,
			})
			// Release unique-key claims so freed values can be claimed immediately.
			s.factory.releaseClaims(eid)
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

func (s *EntityStore) Count(ctx context.Context, modelRef spi.ModelRef) (int64, error) {
	tx := spi.GetTransaction(ctx)
	if tx != nil {
		// Use the same logic as GetAll to get the merged view, then count.
		all, err := s.GetAll(ctx, modelRef)
		if err != nil {
			return 0, err
		}
		return int64(len(all)), nil
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
		// In-tx: use GetAll's merged-view logic (matches existing Count's in-tx fallback).
		all, err := s.GetAll(ctx, modelRef)
		if err != nil {
			return nil, err
		}
		result := make(map[string]int64)
		for _, e := range all {
			st := e.Meta.State
			if filter != nil {
				if _, ok := filter[st]; !ok {
					continue
				}
			}
			result[st]++
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

func (s *EntityStore) GetVersionHistory(ctx context.Context, entityID string) ([]spi.EntityVersion, error) {
	s.factory.entityMu.RLock()
	defer s.factory.entityMu.RUnlock()

	// Historical query: always reads committed data.
	versions, ok := s.factory.entityData[s.tenant][entityID]
	if !ok || len(versions) == 0 {
		return nil, fmt.Errorf("entity %s: %w", entityID, spi.ErrNotFound)
	}

	// NOTE: optimization opportunity — both current consumers (audit handler and
	// entity changes metadata) only use metadata, never Entity.Data. When performance
	// matters, consider a metadata-only variant that skips copying Data bytes.
	result := make([]spi.EntityVersion, 0, len(versions))
	for _, v := range versions {
		ev := spi.EntityVersion{
			ChangeType: v.changeType,
			User:       v.user,
			Timestamp:  v.submitTime,
			Deleted:    v.deleted,
		}
		if v.entity != nil {
			ev.Entity = copyEntity(v.entity)
			ev.Version = v.entity.Meta.Version
		}
		result = append(result, ev)
	}
	return result, nil
}
