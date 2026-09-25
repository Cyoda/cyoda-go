package entity

import (
	"context"
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/testing/taskconflict"
)

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
