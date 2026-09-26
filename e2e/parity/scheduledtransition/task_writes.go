package scheduledtransition

import (
	"fmt"
	"net/url"
	"testing"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/e2e/parity"
	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// task_writes.go — cross-backend parity for the entity-write and
// workflow-import side of the scheduled-task store (design doc §13,
// "Entity writes and workflow import"): a delete of any shape removes the
// deleted entities' tasks (and no one else's), a workflow import that drops
// a schedule removes the model's tasks, and a later import that restores
// the schedule does not itself arm anything — only a subsequent write in
// the state does.

func init() {
	parity.Register(
		parity.NamedTest{Name: "ScheduledTask_DeleteEntityRemovesItsTasks", Fn: RunScheduledTask_DeleteEntityRemovesItsTasks},
		parity.NamedTest{Name: "ScheduledTask_ConditionalDeleteRemovesDeletedEntitiesTasks", Fn: RunScheduledTask_ConditionalDeleteRemovesDeletedEntitiesTasks},
		parity.NamedTest{Name: "ScheduledTask_BatchedDeleteRemovesTasks", Fn: RunScheduledTask_BatchedDeleteRemovesTasks},
		parity.NamedTest{Name: "ScheduledTask_DeleteAllRemovesModelTasks", Fn: RunScheduledTask_DeleteAllRemovesModelTasks},
		parity.NamedTest{Name: "ScheduledTask_ImportDropRemovesTasksAndRestoreReArms", Fn: RunScheduledTask_ImportDropRemovesTasksAndRestoreReArms},
	)
}

// hourSchedule arms AutoClose one hour after an entity enters OPEN; no
// scenario runs that long, so no scheduler ever fires one of these tasks.
const hourSchedule = `{
	"importMode": "REPLACE",
	"workflows": [{
		"version": "1.1", "name": "task-writes-wf", "initialState": "OPEN", "active": true,
		"states": {
			"OPEN": {"transitions": [{"name": "AutoClose", "next": "CLOSED", "manual": false, "schedule": {"delayMs": 3600000}}]},
			"CLOSED": {}
		}
	}]
}`

const hourScheduleDropped = `{
	"importMode": "REPLACE",
	"workflows": [{
		"version": "1.1", "name": "task-writes-wf", "initialState": "OPEN", "active": true,
		"states": {
			"OPEN": {"transitions": [{"name": "AutoClose", "next": "CLOSED", "manual": true}]},
			"CLOSED": {}
		}
	}]
}`

const kOver1 = `{"type":"simple","jsonPath":"$.k","operatorType":"GREATER_THAN","value":1}`

// listTasks reads GET /api/scheduled-tasks with query, through Q-5's client.
func listTasks(t *testing.T, c *client.Client, query url.Values) []client.ScheduledTask {
	t.Helper()
	query.Set("limit", "1000")
	page, err := c.ListScheduledTasks(t, query)
	if err != nil {
		t.Fatalf("ListScheduledTasks: %v", err)
	}
	return page.Items
}

func entityTasks(t *testing.T, c *client.Client, id uuid.UUID) []client.ScheduledTask {
	t.Helper()
	return listTasks(t, c, url.Values{"entityId": {id.String()}})
}

func modelTaskCount(t *testing.T, c *client.Client, model string) int {
	t.Helper()
	return len(listTasks(t, c, url.Values{"modelName": {model}, "modelVersion": {"1"}}))
}

// seedK creates one entity per k value and checks each has one task.
func seedK(t *testing.T, c *client.Client, model string, ks ...int) []uuid.UUID {
	t.Helper()
	ids := make([]uuid.UUID, 0, len(ks))
	for _, k := range ks {
		id, err := c.CreateEntity(t, model, 1, fmt.Sprintf(`{"k":%d}`, k))
		if err != nil {
			t.Fatalf("CreateEntity: %v", err)
		}
		if n := len(entityTasks(t, c, id)); n != 1 {
			t.Fatalf("tasks of a new entity = %d, want 1", n)
		}
		ids = append(ids, id)
	}
	return ids
}

// RunScheduledTask_DeleteEntityRemovesItsTasks verifies that deleting one
// entity removes its scheduled tasks and leaves another entity's task
// alone (design §13, "deleting one entity removes its tasks").
func RunScheduledTask_DeleteEntityRemovesItsTasks(t *testing.T, fixture parity.BackendFixture) {
	c := client.NewClient(fixture.BaseURL(), fixture.NewTenant(t).Token)
	const model = "task-writes-delete-one"
	setupModelWithWorkflow(t, c, model, 1, `{"k":1}`, hourSchedule)
	ids := seedK(t, c, model, 1, 2)

	if err := c.DeleteEntity(t, ids[0]); err != nil {
		t.Fatalf("DeleteEntity: %v", err)
	}
	if n := len(entityTasks(t, c, ids[0])); n != 0 {
		t.Errorf("tasks of the deleted entity = %d, want 0", n)
	}
	if n := len(entityTasks(t, c, ids[1])); n != 1 {
		t.Errorf("tasks of the other entity = %d, want 1", n)
	}
}

// RunScheduledTask_ConditionalDeleteRemovesDeletedEntitiesTasks verifies
// that a single-transaction conditional delete removes the tasks of every
// entity it deletes and leaves the surviving entity's task alone (design
// §13, "conditional delete removes tasks: single-tx").
func RunScheduledTask_ConditionalDeleteRemovesDeletedEntitiesTasks(t *testing.T, fixture parity.BackendFixture) {
	c := client.NewClient(fixture.BaseURL(), fixture.NewTenant(t).Token)
	const model = "task-writes-cond"
	setupModelWithWorkflow(t, c, model, 1, `{"k":1}`, hourSchedule)
	ids := seedK(t, c, model, 1, 2, 3)

	res, err := c.DeleteEntitiesConditional(t, model, 1, kOver1, 0)
	if err != nil {
		t.Fatalf("DeleteEntitiesConditional: %v", err)
	}
	if res.RemovedCount != 2 {
		t.Fatalf("removed = %d, want 2", res.RemovedCount)
	}
	for i, want := range []int{1, 0, 0} {
		if n := len(entityTasks(t, c, ids[i])); n != want {
			t.Errorf("tasks of entity k=%d = %d, want %d", i+1, n, want)
		}
	}
}

// RunScheduledTask_BatchedDeleteRemovesTasks verifies that a batched
// conditional delete (transactionSize > 0) removes each deleted entity's
// tasks per batch and leaves the surviving entity's task alone (design
// §13, "conditional delete removes tasks: ... batched").
func RunScheduledTask_BatchedDeleteRemovesTasks(t *testing.T, fixture parity.BackendFixture) {
	c := client.NewClient(fixture.BaseURL(), fixture.NewTenant(t).Token)
	const model = "task-writes-batched"
	setupModelWithWorkflow(t, c, model, 1, `{"k":1}`, hourSchedule)
	ids := seedK(t, c, model, 1, 2, 3)

	res, err := c.DeleteEntitiesConditional(t, model, 1, kOver1, 1)
	if err != nil {
		t.Fatalf("DeleteEntitiesConditional: %v", err)
	}
	if res.RemovedCount != 2 || len(res.IDToError) != 0 {
		t.Fatalf("removed=%d idToError=%v, want 2 and none", res.RemovedCount, res.IDToError)
	}
	for i, want := range []int{1, 0, 0} {
		if n := len(entityTasks(t, c, ids[i])); n != want {
			t.Errorf("tasks of entity k=%d = %d, want %d", i+1, n, want)
		}
	}
}

// RunScheduledTask_DeleteAllRemovesModelTasks verifies that delete-all
// (the unconditional fast path) removes every task of the deleted model
// and leaves another model's tasks alone (design §13, "delete-all removes
// the model's tasks").
func RunScheduledTask_DeleteAllRemovesModelTasks(t *testing.T, fixture parity.BackendFixture) {
	c := client.NewClient(fixture.BaseURL(), fixture.NewTenant(t).Token)
	const model = "task-writes-delete-all"
	const other = "task-writes-delete-all-other"
	setupModelWithWorkflow(t, c, model, 1, `{"k":1}`, hourSchedule)
	setupModelWithWorkflow(t, c, other, 1, `{"k":1}`, hourSchedule)
	seedK(t, c, model, 1, 2)
	seedK(t, c, other, 1)

	if err := c.DeleteEntitiesByModel(t, model, 1); err != nil {
		t.Fatalf("DeleteEntitiesByModel: %v", err)
	}
	if n := modelTaskCount(t, c, model); n != 0 {
		t.Errorf("tasks of %s = %d, want 0", model, n)
	}
	if n := modelTaskCount(t, c, other); n != 1 {
		t.Errorf("tasks of %s = %d, want 1: another model's tasks stay", other, n)
	}
}

// RunScheduledTask_ImportDropRemovesTasksAndRestoreReArms verifies that a
// workflow import which drops a transition's schedule removes the model's
// tasks for it (design §13, "a workflow import that drops schedules
// removes the model's tasks"), and that restoring the schedule by a later
// import does not itself arm a task — only a subsequent write in the state
// does (design §7 "Arm").
func RunScheduledTask_ImportDropRemovesTasksAndRestoreReArms(t *testing.T, fixture parity.BackendFixture) {
	c := client.NewClient(fixture.BaseURL(), fixture.NewTenant(t).Token)
	const model = "task-writes-import"
	setupModelWithWorkflow(t, c, model, 1, `{"k":1}`, hourSchedule)
	ids := seedK(t, c, model, 1)

	if err := c.ImportWorkflow(t, model, 1, hourScheduleDropped); err != nil {
		t.Fatalf("ImportWorkflow (drop): %v", err)
	}
	if n := len(entityTasks(t, c, ids[0])); n != 0 {
		t.Fatalf("tasks after the drop = %d, want 0", n)
	}

	if err := c.ImportWorkflow(t, model, 1, hourSchedule); err != nil {
		t.Fatalf("ImportWorkflow (restore): %v", err)
	}
	if n := len(entityTasks(t, c, ids[0])); n != 0 {
		t.Fatalf("tasks after the restore = %d, want 0: an import never arms", n)
	}
	if err := c.UpdateEntityData(t, ids[0], `{"k":1}`); err != nil {
		t.Fatalf("UpdateEntityData: %v", err)
	}
	got := entityTasks(t, c, ids[0])
	if len(got) != 1 || got[0].Transition != "AutoClose" || got[0].Status != "WAITING" || got[0].Attempts != 0 {
		t.Errorf("tasks after a write in the state = %+v, want one WAITING AutoClose task, attempts 0", got)
	}
}
