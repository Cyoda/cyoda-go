package entity

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/testing/taskconflict"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
)

// armForeignModelTask arms one task of another model in the same tenant, so
// a test can see that a model-wide removal stays inside its model.
func armForeignModelTask(t *testing.T, e *taskEnv) {
	t.Helper()
	txID, txCtx, err := e.txMgr.Begin(e.ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	sts, err := e.real.ScheduledTaskStore(txCtx)
	if err != nil {
		t.Fatalf("ScheduledTaskStore: %v", err)
	}
	if _, err := sts.ReconcileForEntity(txCtx, spi.ReconcileRequest{
		TenantID: taskTenant, EntityID: "foreign-entity", CurrentState: "OPEN",
		Arm: []spi.ScheduledTask{{
			ID: "foreign-task", TenantID: taskTenant, Type: spi.ScheduledTaskFireTransition,
			ScheduledTime: 9_999_999_999_999, EntityID: "foreign-entity",
			ModelName: "Pet", ModelVersion: 1, Transition: "AutoClose", SourceState: "OPEN",
		}},
	}); err != nil {
		t.Fatalf("ReconcileForEntity: %v", err)
	}
	if err := e.txMgr.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

func TestDeleteAllEntities_RemovesTheModelsTasks(t *testing.T) {
	e := newTaskEnv(t)
	seedPersons(t, e.h, e.ctx, 3)
	armForeignModelTask(t, e)
	if n := e.modelTasks(t, "Person"); n != 3 {
		t.Fatalf("Person tasks before = %d, want 3", n)
	}

	if _, err := e.h.DeleteAllEntities(e.ctx, "Person", "1"); err != nil {
		t.Fatalf("DeleteAllEntities: %v", err)
	}
	if n := e.modelTasks(t, "Person"); n != 0 {
		t.Errorf("Person tasks = %d, want 0", n)
	}
	if n := e.modelTasks(t, "Pet"); n != 1 {
		t.Errorf("Pet tasks = %d, want 1: another model's tasks stay", n)
	}
}

func TestDeleteEntitiesConditional_FastPath_RemovesTheModelsTasks(t *testing.T) {
	e := newTaskEnv(t)
	seedPersons(t, e.h, e.ctx, 2)

	if _, err := e.h.DeleteEntitiesConditional(e.ctx, "Person", "1", nil, nil, false, 0); err != nil {
		t.Fatalf("DeleteEntitiesConditional: %v", err)
	}
	if got := e.plan.Calls(taskconflict.DeleteForModel); got != 1 {
		t.Errorf("DeleteForModel calls = %d, want 1 (the fast path)", got)
	}
	if n := e.modelTasks(t, "Person"); n != 0 {
		t.Errorf("Person tasks = %d, want 0", n)
	}
}

func TestDeleteAllEntities_TaskConflict_RetriedThenSucceeds(t *testing.T) {
	e := newTaskEnv(t)
	seedPersons(t, e.h, e.ctx, 2)
	e.plan.Refuse(taskconflict.DeleteForModel, 2)

	res, err := e.h.DeleteAllEntities(e.ctx, "Person", "1")
	if err != nil {
		t.Fatalf("DeleteAllEntities: %v", err)
	}
	if res.TotalCount != 2 {
		t.Errorf("TotalCount = %d, want 2 (each attempt counts afresh)", res.TotalCount)
	}
	if got := e.plan.Calls(taskconflict.DeleteForModel); got != 3 {
		t.Errorf("DeleteForModel calls = %d, want 3", got)
	}
	if n := e.modelTasks(t, "Person"); n != 0 {
		t.Errorf("Person tasks = %d, want 0", n)
	}
}

func TestDeleteAllEntities_TaskConflictPersists_Retryable409(t *testing.T) {
	e := newTaskEnv(t)
	ids := seedPersons(t, e.h, e.ctx, 2)
	e.plan.Refuse(taskconflict.DeleteForModel, 100)

	_, err := e.h.DeleteAllEntities(e.ctx, "Person", "1")
	requireConflict409(t, err)
	if got, want := e.plan.Calls(taskconflict.DeleteForModel), 1+common.TaskConflictRetries; got != want {
		t.Errorf("DeleteForModel calls = %d, want %d", got, want)
	}
	if !e.exists(t, ids[0]) || !e.exists(t, ids[1]) {
		t.Error("entities removed although every attempt rolled back")
	}
}

func TestDeleteAllEntities_Joined_NotRetried(t *testing.T) {
	e := newTaskEnv(t)
	seedPersons(t, e.h, e.ctx, 1)
	e.plan.Refuse(taskconflict.DeleteForModel, 100)
	txID, joinedCtx, err := e.txMgr.Begin(e.ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	t.Cleanup(func() { _ = e.txMgr.Rollback(e.ctx, txID) })

	_, err = e.h.DeleteAllEntities(joinedCtx, "Person", "1")
	requireConflict409(t, err)
	if got := e.plan.Calls(taskconflict.DeleteForModel); got != 1 {
		t.Errorf("DeleteForModel calls = %d, want 1", got)
	}
}

func TestDeleteEntity_RemovesTheEntitysTasks(t *testing.T) {
	e := newTaskEnv(t)
	ids := seedPersons(t, e.h, e.ctx, 2)
	if n := e.tasksOf(t, ids[0]); n != 1 {
		t.Fatalf("tasks of %s before the delete = %d, want 1", ids[0], n)
	}

	if _, err := e.h.DeleteEntity(e.ctx, ids[0]); err != nil {
		t.Fatalf("DeleteEntity: %v", err)
	}

	if n := e.tasksOf(t, ids[0]); n != 0 {
		t.Errorf("tasks of the deleted entity = %d, want 0", n)
	}
	if n := e.tasksOf(t, ids[1]); n != 1 {
		t.Errorf("tasks of the other entity = %d, want 1", n)
	}
}

func TestDeleteEntity_TaskConflict_RetriedThenSucceeds(t *testing.T) {
	e := newTaskEnv(t)
	ids := seedPersons(t, e.h, e.ctx, 1)
	e.plan.Refuse(taskconflict.DeleteForEntities, 2)

	if _, err := e.h.DeleteEntity(e.ctx, ids[0]); err != nil {
		t.Fatalf("DeleteEntity: %v", err)
	}
	if got := e.plan.Calls(taskconflict.DeleteForEntities); got != 3 {
		t.Errorf("DeleteForEntities calls = %d, want 3 (two refusals, then success)", got)
	}
	if e.exists(t, ids[0]) {
		t.Error("entity still exists after a successful delete")
	}
	if n := e.tasksOf(t, ids[0]); n != 0 {
		t.Errorf("tasks = %d, want 0", n)
	}
}

func TestDeleteEntity_TaskConflictPersists_Retryable409(t *testing.T) {
	e := newTaskEnv(t)
	ids := seedPersons(t, e.h, e.ctx, 1)
	e.plan.Refuse(taskconflict.DeleteForEntities, 100)

	_, err := e.h.DeleteEntity(e.ctx, ids[0])
	requireConflict409(t, err)
	if got, want := e.plan.Calls(taskconflict.DeleteForEntities), 1+common.TaskConflictRetries; got != want {
		t.Errorf("DeleteForEntities calls = %d, want %d", got, want)
	}
	if !e.exists(t, ids[0]) {
		t.Error("entity removed although every attempt rolled back")
	}
	if n := e.tasksOf(t, ids[0]); n != 1 {
		t.Errorf("tasks = %d, want 1", n)
	}
}

func TestDeleteEntity_Joined_NotRetried(t *testing.T) {
	e := newTaskEnv(t)
	ids := seedPersons(t, e.h, e.ctx, 1)
	e.plan.Refuse(taskconflict.DeleteForEntities, 100)

	txID, joinedCtx, err := e.txMgr.Begin(e.ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	t.Cleanup(func() { _ = e.txMgr.Rollback(e.ctx, txID) })

	_, err = e.h.DeleteEntity(joinedCtx, ids[0])
	requireConflict409(t, err)
	if got := e.plan.Calls(taskconflict.DeleteForEntities); got != 1 {
		t.Errorf("DeleteForEntities calls = %d, want 1: a joined delete is not retried", got)
	}
}

func TestDeleteEntity_TaskStoreFailure_Is500AndNotRetried(t *testing.T) {
	e := newTaskEnv(t)
	ids := seedPersons(t, e.h, e.ctx, 1)
	e.plan.Fail(taskconflict.DeleteForEntities, errors.New("task store unreachable"))

	_, err := e.h.DeleteEntity(e.ctx, ids[0])
	var appErr *common.AppError
	if !errors.As(err, &appErr) || appErr.Status != 500 {
		t.Fatalf("err = %v, want a 500 AppError", err)
	}
	if got := e.plan.Calls(taskconflict.DeleteForEntities); got != 1 {
		t.Errorf("DeleteForEntities calls = %d, want 1", got)
	}
	if !e.exists(t, ids[0]) {
		t.Error("entity removed although the task removal failed")
	}
}

// racingDeleteStore commits a concurrent save of the entity in its own
// transaction just before it delegates a Delete. The delete's transaction
// began before that commit, so memory refuses the delete at commit with
// spi.ErrConflict: the way memory and SQLite report a lost race.
type racingDeleteStore struct {
	spi.EntityStore
	t      *testing.T
	txMgr  spi.TransactionManager
	base   context.Context
	races  int // how many Deletes still race
	delete int // how many Deletes ran
}

func (s *racingDeleteStore) Delete(ctx context.Context, entityID string) error {
	s.delete++
	if s.races > 0 {
		s.races--
		txID, raceCtx, err := s.txMgr.Begin(s.base)
		if err != nil {
			s.t.Fatalf("race Begin: %v", err)
		}
		ent, err := s.EntityStore.Get(raceCtx, entityID)
		if err != nil {
			s.t.Fatalf("race Get: %v", err)
		}
		if _, err := s.EntityStore.Save(raceCtx, ent); err != nil {
			s.t.Fatalf("race Save: %v", err)
		}
		if err := s.txMgr.Commit(raceCtx, txID); err != nil {
			s.t.Fatalf("race Commit: %v", err)
		}
	}
	return s.EntityStore.Delete(ctx, entityID)
}

func newRacingDeleteStore(t *testing.T, e *taskEnv, races int) *racingDeleteStore {
	t.Helper()
	inner, err := e.real.EntityStore(e.ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	return &racingDeleteStore{EntityStore: inner, t: t, txMgr: e.txMgr, base: e.ctx, races: races}
}

func TestDeleteEntity_CommitConflict_RetriedThenSucceeds(t *testing.T) {
	e := newTaskEnv(t)
	ids := seedPersons(t, e.h, e.ctx, 1)
	racing := newRacingDeleteStore(t, e, 1)
	e.withEntityStore(t, racing)

	if _, err := e.h.DeleteEntity(e.ctx, ids[0]); err != nil {
		t.Fatalf("DeleteEntity: %v", err)
	}
	if racing.delete != 2 {
		t.Errorf("attempts = %d, want 2 (one refused at commit, then success)", racing.delete)
	}
	if e.exists(t, ids[0]) {
		t.Error("entity still exists after a successful delete")
	}
	if n := e.tasksOf(t, ids[0]); n != 0 {
		t.Errorf("tasks = %d, want 0", n)
	}
}

func TestDeleteEntity_CommitConflictPersists_Retryable409WithCause(t *testing.T) {
	e := newTaskEnv(t)
	ids := seedPersons(t, e.h, e.ctx, 1)
	racing := newRacingDeleteStore(t, e, 100)
	e.withEntityStore(t, racing)

	_, err := e.h.DeleteEntity(e.ctx, ids[0])
	requireConflict409(t, err)
	if !errors.Is(err, spi.ErrConflict) {
		t.Errorf("err = %v: the 409 does not carry spi.ErrConflict as its cause", err)
	}
	if got, want := racing.delete, 1+common.TaskConflictRetries; got != want {
		t.Errorf("attempts = %d, want %d", got, want)
	}
	if !e.exists(t, ids[0]) {
		t.Error("entity removed although every attempt was refused at commit")
	}
	if n := e.tasksOf(t, ids[0]); n != 1 {
		t.Errorf("tasks = %d, want 1", n)
	}
}

// racingDeleteAllStore commits a concurrent save of one of the model's
// entities in its own transaction just before it delegates a DeleteAll. The
// delete's transaction began before that commit, so memory refuses the
// delete-all at commit with spi.ErrConflict — the way memory and SQLite
// report a lost race.
type racingDeleteAllStore struct {
	spi.EntityStore
	t         *testing.T
	txMgr     spi.TransactionManager
	base      context.Context
	raceID    string // id of an entity of the model being deleted
	races     int    // how many DeleteAlls still race
	deleteAll int    // how many DeleteAlls ran
}

func (s *racingDeleteAllStore) DeleteAll(ctx context.Context, ref spi.ModelRef) error {
	s.deleteAll++
	if s.races > 0 {
		s.races--
		txID, raceCtx, err := s.txMgr.Begin(s.base)
		if err != nil {
			s.t.Fatalf("race Begin: %v", err)
		}
		ent, err := s.EntityStore.Get(raceCtx, s.raceID)
		if err != nil {
			s.t.Fatalf("race Get: %v", err)
		}
		if _, err := s.EntityStore.Save(raceCtx, ent); err != nil {
			s.t.Fatalf("race Save: %v", err)
		}
		if err := s.txMgr.Commit(raceCtx, txID); err != nil {
			s.t.Fatalf("race Commit: %v", err)
		}
	}
	return s.EntityStore.DeleteAll(ctx, ref)
}

func newRacingDeleteAllStore(t *testing.T, e *taskEnv, raceID string, races int) *racingDeleteAllStore {
	t.Helper()
	inner, err := e.real.EntityStore(e.ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	return &racingDeleteAllStore{EntityStore: inner, t: t, txMgr: e.txMgr, base: e.ctx, raceID: raceID, races: races}
}

func TestDeleteAllEntities_CommitConflict_RetriedThenSucceeds(t *testing.T) {
	e := newTaskEnv(t)
	ids := seedPersons(t, e.h, e.ctx, 2)
	racing := newRacingDeleteAllStore(t, e, ids[0], 1)
	e.withEntityStore(t, racing)

	if _, err := e.h.DeleteAllEntities(e.ctx, "Person", "1"); err != nil {
		t.Fatalf("DeleteAllEntities: %v", err)
	}
	if racing.deleteAll != 2 {
		t.Errorf("attempts = %d, want 2 (one refused at commit, then success)", racing.deleteAll)
	}
	if e.exists(t, ids[0]) || e.exists(t, ids[1]) {
		t.Error("an entity still exists after a successful delete-all")
	}
	if n := e.modelTasks(t, "Person"); n != 0 {
		t.Errorf("Person tasks = %d, want 0", n)
	}
}

func TestDeleteAllEntities_CommitConflictPersists_Retryable409WithCause(t *testing.T) {
	e := newTaskEnv(t)
	ids := seedPersons(t, e.h, e.ctx, 2)
	racing := newRacingDeleteAllStore(t, e, ids[0], 100)
	e.withEntityStore(t, racing)

	_, err := e.h.DeleteAllEntities(e.ctx, "Person", "1")
	requireConflict409(t, err)
	if !errors.Is(err, spi.ErrConflict) {
		t.Errorf("err = %v: the 409 does not carry spi.ErrConflict as its cause", err)
	}
	if got, want := racing.deleteAll, 1+common.TaskConflictRetries; got != want {
		t.Errorf("attempts = %d, want %d", got, want)
	}
	if !e.exists(t, ids[0]) || !e.exists(t, ids[1]) {
		t.Error("an entity was removed although every attempt was refused at commit")
	}
}

var ageAtLeastOne = []byte(`{"type":"simple","jsonPath":"$.age","operatorType":"GREATER_OR_EQUAL","value":1}`)

// deleteRefusingStore wraps an EntityStore. Delete refuses the next
// conflicts calls with a first-committer-wins conflict, and always fails
// for failID with a plain error.
type deleteRefusingStore struct {
	spi.EntityStore
	mu        sync.Mutex
	conflicts int
	failID    string
}

func (s *deleteRefusingStore) takeConflict() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conflicts == 0 {
		return false
	}
	s.conflicts--
	return true
}

func (s *deleteRefusingStore) Delete(ctx context.Context, id string) error {
	if id == s.failID {
		return errors.New("entity row unreadable")
	}
	if s.takeConflict() {
		return fmt.Errorf("entity row changed: %w", spi.ErrConflict)
	}
	return s.EntityStore.Delete(ctx, id)
}

func (e *taskEnv) refusingStore(t *testing.T, conflicts int, failID string) {
	t.Helper()
	real, err := e.real.EntityStore(e.ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	e.withEntityStore(t, &deleteRefusingStore{EntityStore: real, conflicts: conflicts, failID: failID})
}

func TestDeleteEntitiesConditional_SingleTx_RemovesOnlyTheDeletedEntitiesTasks(t *testing.T) {
	e := newTaskEnv(t)
	ids := seedPersons(t, e.h, e.ctx, 3) // ages 0, 1, 2

	res, err := e.h.DeleteEntitiesConditional(e.ctx, "Person", "1", ageAtLeastOne, nil, false, 0)
	if err != nil {
		t.Fatalf("DeleteEntitiesConditional: %v", err)
	}
	if res.RemovedCount != 2 {
		t.Fatalf("RemovedCount = %d, want 2", res.RemovedCount)
	}
	for i, want := range []int{1, 0, 0} {
		if n := e.tasksOf(t, ids[i]); n != want {
			t.Errorf("tasks of entity %d = %d, want %d", i, n, want)
		}
	}
}

func TestDeleteEntitiesConditional_SingleTx_FailedIDKeepsItsTasks(t *testing.T) {
	e := newTaskEnv(t)
	ids := seedPersons(t, e.h, e.ctx, 3)
	e.refusingStore(t, 0, ids[2])

	res, err := e.h.DeleteEntitiesConditional(e.ctx, "Person", "1", ageAtLeastOne, nil, false, 0)
	if err != nil {
		t.Fatalf("DeleteEntitiesConditional: %v", err)
	}
	if _, failed := res.IDToError[ids[2]]; !failed {
		t.Fatalf("IDToError = %v, want an entry for %s", res.IDToError, ids[2])
	}
	if n := e.tasksOf(t, ids[1]); n != 0 {
		t.Errorf("tasks of the deleted entity = %d, want 0", n)
	}
	if n := e.tasksOf(t, ids[2]); n != 1 {
		t.Errorf("tasks of the entity whose delete failed = %d, want 1", n)
	}
}

func TestDeleteEntitiesConditional_SingleTx_TaskConflict_RetriedWithAFreshResult(t *testing.T) {
	e := newTaskEnv(t)
	seedPersons(t, e.h, e.ctx, 3)
	e.plan.Refuse(taskconflict.DeleteForEntities, 1)

	res, err := e.h.DeleteEntitiesConditional(e.ctx, "Person", "1", ageAtLeastOne, nil, true, 0)
	if err != nil {
		t.Fatalf("DeleteEntitiesConditional: %v", err)
	}
	if res.MatchedCount != 2 || res.RemovedCount != 2 || len(res.IDs) != 2 {
		t.Errorf("Matched=%d Removed=%d IDs=%v, want 2, 2 and two ids: a retry starts a new result",
			res.MatchedCount, res.RemovedCount, res.IDs)
	}
	if got := e.plan.Calls(taskconflict.DeleteForEntities); got != 2 {
		t.Errorf("DeleteForEntities calls = %d, want 2", got)
	}
}

func TestDeleteEntitiesConditional_SingleTx_TaskConflictPersists_Retryable409(t *testing.T) {
	e := newTaskEnv(t)
	ids := seedPersons(t, e.h, e.ctx, 2)
	e.plan.Refuse(taskconflict.DeleteForEntities, 100)

	_, err := e.h.DeleteEntitiesConditional(e.ctx, "Person", "1", ageAtLeastOne, nil, false, 0)
	requireConflict409(t, err)
	if got, want := e.plan.Calls(taskconflict.DeleteForEntities), 1+common.TaskConflictRetries; got != want {
		t.Errorf("DeleteForEntities calls = %d, want %d", got, want)
	}
	if !e.exists(t, ids[1]) {
		t.Error("entity removed although every attempt rolled back")
	}
}

func TestDeleteEntitiesConditional_SingleTx_Joined_NotRetried(t *testing.T) {
	e := newTaskEnv(t)
	seedPersons(t, e.h, e.ctx, 2)
	e.plan.Refuse(taskconflict.DeleteForEntities, 100)
	txID, joinedCtx, err := e.txMgr.Begin(e.ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	t.Cleanup(func() { _ = e.txMgr.Rollback(e.ctx, txID) })

	_, err = e.h.DeleteEntitiesConditional(joinedCtx, "Person", "1", ageAtLeastOne, nil, false, 0)
	requireConflict409(t, err)
	if got := e.plan.Calls(taskconflict.DeleteForEntities); got != 1 {
		t.Errorf("DeleteForEntities calls = %d, want 1", got)
	}
}

// An entity row changed after the snapshot fails the attempt, as on
// PostgreSQL, where the 40001 aborts the transaction. It is not one id's
// outcome, and the retry deletes every matched id.
func TestDeleteEntitiesConditional_SingleTx_EntityRowConflict_Retried(t *testing.T) {
	e := newTaskEnv(t)
	seedPersons(t, e.h, e.ctx, 3)
	e.refusingStore(t, 1, "")

	res, err := e.h.DeleteEntitiesConditional(e.ctx, "Person", "1", ageAtLeastOne, nil, false, 0)
	if err != nil {
		t.Fatalf("DeleteEntitiesConditional: %v", err)
	}
	if res.RemovedCount != 2 || len(res.IDToError) != 0 {
		t.Errorf("Removed=%d IDToError=%v, want 2 and none", res.RemovedCount, res.IDToError)
	}
}

// --- Fix round 1 ---

// racingDeleteStore's race only ever touches the id passed to Delete, so
// seedPersons(t, e.h, e.ctx, 2) (ages 0, 1) with ageAtLeastOne matches exactly
// ids[1] — one Delete call per attempt, mirroring TestDeleteEntity_CommitConflict*'s
// single-id shape.
func TestDeleteEntitiesConditional_SingleTx_CommitConflict_RetriedThenSucceeds(t *testing.T) {
	e := newTaskEnv(t)
	seedPersons(t, e.h, e.ctx, 2)
	racing := newRacingDeleteStore(t, e, 1)
	e.withEntityStore(t, racing)

	res, err := e.h.DeleteEntitiesConditional(e.ctx, "Person", "1", ageAtLeastOne, nil, false, 0)
	if err != nil {
		t.Fatalf("DeleteEntitiesConditional: %v", err)
	}
	if res.RemovedCount != 1 {
		t.Errorf("RemovedCount = %d, want 1", res.RemovedCount)
	}
	if racing.delete != 2 {
		t.Errorf("attempts = %d, want 2 (one refused at commit, then success)", racing.delete)
	}
}

func TestDeleteEntitiesConditional_SingleTx_CommitConflictPersists_Retryable409WithCause(t *testing.T) {
	e := newTaskEnv(t)
	ids := seedPersons(t, e.h, e.ctx, 2)
	racing := newRacingDeleteStore(t, e, 100)
	e.withEntityStore(t, racing)

	_, err := e.h.DeleteEntitiesConditional(e.ctx, "Person", "1", ageAtLeastOne, nil, false, 0)
	requireConflict409(t, err)
	if !errors.Is(err, spi.ErrConflict) {
		t.Errorf("err = %v: the 409 does not carry spi.ErrConflict as its cause", err)
	}
	if got, want := racing.delete, 1+common.TaskConflictRetries; got != want {
		t.Errorf("attempts = %d, want %d", got, want)
	}
	if !e.exists(t, ids[1]) {
		t.Error("entity removed although every attempt was refused at commit")
	}
}

// heldJoinedCtx begins a fresh transaction and takes its gate the way
// txjoin.Joiner.RunVerified does before it ever calls a routed callback's
// handler (internal/domain/txjoin/txjoin.go: AcquireCtx, then WithHeld) —
// so the ctx a test hands the handler here is held exactly as a real joined
// callback's is, not merely tx-bearing. A once-function whose internal
// "owned" wrongly read true against such a ctx would try to acquire the same
// non-reentrant gate a second time and deadlock; a joined ctx built from a
// bare txMgr.Begin/Join (as the existing *_Joined_NotRetried tests use)
// never holds the gate, so it cannot catch that.
func (e *taskEnv) heldJoinedCtx(t *testing.T) (ctx context.Context, txID string, release func()) {
	t.Helper()
	txID, joinedCtx, err := e.txMgr.Begin(e.ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	rel, err := e.h.gate.AcquireCtx(joinedCtx, txID, 0)
	if err != nil {
		t.Fatalf("AcquireCtx: %v", err)
	}
	held, _ := txgate.WithHeld(joinedCtx, e.h.gate, txID, &rel)
	return held, txID, func() { rel() }
}

// runWithTimeout runs fn on its own goroutine and fails the test if fn has
// not returned within timeout, rather than hanging the run — the bounded-
// timeout pattern TestJoinedFlows_ErrorPath_DoNotDeadlock (service_rollback_test.go)
// already uses for the same class of gate deadlock.
func runWithTimeout(t *testing.T, timeout time.Duration, fn func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		t.Fatal("operation did not return within the timeout: a joined call must not try to re-acquire the gate it already holds")
		return nil // unreachable; t.Fatal stops this goroutine
	}
}

func TestDeleteEntity_Joined_HeldGateNotReacquired(t *testing.T) {
	e := newTaskEnv(t)
	ids := seedPersons(t, e.h, e.ctx, 1)
	heldCtx, txID, release := e.heldJoinedCtx(t)
	t.Cleanup(func() { _ = e.txMgr.Rollback(e.ctx, txID) })
	defer release()

	err := runWithTimeout(t, 5*time.Second, func() error {
		_, err := e.h.DeleteEntity(heldCtx, ids[0])
		return err
	})
	if err != nil {
		t.Fatalf("DeleteEntity under an already-held gate: %v", err)
	}
	if got := e.plan.Calls(taskconflict.DeleteForEntities); got != 1 {
		t.Errorf("DeleteForEntities calls = %d, want 1: a joined delete is not retried", got)
	}
}

func TestDeleteAllEntities_Joined_HeldGateNotReacquired(t *testing.T) {
	e := newTaskEnv(t)
	seedPersons(t, e.h, e.ctx, 1)
	heldCtx, txID, release := e.heldJoinedCtx(t)
	t.Cleanup(func() { _ = e.txMgr.Rollback(e.ctx, txID) })
	defer release()

	err := runWithTimeout(t, 5*time.Second, func() error {
		_, err := e.h.DeleteAllEntities(heldCtx, "Person", "1")
		return err
	})
	if err != nil {
		t.Fatalf("DeleteAllEntities under an already-held gate: %v", err)
	}
	if got := e.plan.Calls(taskconflict.DeleteForModel); got != 1 {
		t.Errorf("DeleteForModel calls = %d, want 1: a joined delete-all is not retried", got)
	}
}

func TestDeleteEntitiesConditional_SingleTx_Joined_HeldGateNotReacquired(t *testing.T) {
	e := newTaskEnv(t)
	seedPersons(t, e.h, e.ctx, 2)
	heldCtx, txID, release := e.heldJoinedCtx(t)
	t.Cleanup(func() { _ = e.txMgr.Rollback(e.ctx, txID) })
	defer release()

	err := runWithTimeout(t, 5*time.Second, func() error {
		_, err := e.h.DeleteEntitiesConditional(heldCtx, "Person", "1", ageAtLeastOne, nil, false, 0)
		return err
	})
	if err != nil {
		t.Fatalf("DeleteEntitiesConditional under an already-held gate: %v", err)
	}
	if got := e.plan.Calls(taskconflict.DeleteForEntities); got != 1 {
		t.Errorf("DeleteForEntities calls = %d, want 1: a joined conditional delete is not retried", got)
	}
}
