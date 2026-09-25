package entity

import (
	"errors"
	"testing"

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
