package e2e_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// importBothScheduled arms AutoClose and Remind one hour after an entity
// enters OPEN.
const importBothScheduled = `{
	"importMode": "REPLACE",
	"workflows": [{
		"version": "1.1", "name": "stw-import-wf", "initialState": "OPEN", "active": true,
		"states": {
			"OPEN": {"transitions": [
				{"name": "AutoClose", "next": "CLOSED", "manual": false, "schedule": {"delayMs": 3600000}},
				{"name": "Remind", "next": "REMINDED", "manual": false, "schedule": {"delayMs": 3600000}}
			]},
			"CLOSED": {},
			"REMINDED": {}
		}
	}]
}`

// importRemindUnscheduled keeps AutoClose scheduled and makes Remind manual.
const importRemindUnscheduled = `{
	"importMode": "REPLACE",
	"workflows": [{
		"version": "1.1", "name": "stw-import-wf", "initialState": "OPEN", "active": true,
		"states": {
			"OPEN": {"transitions": [
				{"name": "AutoClose", "next": "CLOSED", "manual": false, "schedule": {"delayMs": 3600000}},
				{"name": "Remind", "next": "REMINDED", "manual": true}
			]},
			"CLOSED": {},
			"REMINDED": {}
		}
	}]
}`

func importPath(model string) string { return fmt.Sprintf("/api/model/%s/1/workflow/import", model) }

func TestScheduledTaskWrites_Import_DroppingAScheduleRemovesItsTasks(t *testing.T) {
	const model = "e2e-stw-import"
	setupModelWithWorkflow(t, model, importBothScheduled)
	id := createEntityE2E(t, model, 1, schedWritesPayload)
	if n := taskRows(t, "entity_id = $1", id); n != 2 {
		t.Fatalf("task rows before = %d, want 2", n)
	}

	res := resultOf(doAuthOnceRaw(e2eCtx(t), http.MethodPost, importPath(model), importRemindUnscheduled))
	if res.status != http.StatusOK {
		t.Fatalf("import: %d %s", res.status, res.body)
	}
	if n := taskRows(t, "entity_id = $1 AND transition = 'Remind'", id); n != 0 {
		t.Errorf("Remind task rows = %d, want 0", n)
	}
	if n := taskRows(t, "entity_id = $1 AND transition = 'AutoClose'", id); n != 1 {
		t.Errorf("AutoClose task rows = %d, want 1", n)
	}
}

func TestScheduledTaskWrites_Import_RacingOneClaim_SucceedsAfterRetry(t *testing.T) {
	const model = "e2e-stw-import-race"
	setupModelWithWorkflow(t, model, importBothScheduled)
	id := createEntityE2E(t, model, 1, schedWritesPayload)
	hold := holdTaskRows(t, "entity_id = $1 AND transition = 'Remind'", id)

	ctx := e2eCtx(t)
	done := make(chan httpResult, 1)
	go func() {
		done <- resultOf(doAuthOnceRaw(ctx, http.MethodPost, importPath(model), importRemindUnscheduled))
	}()
	hold.awaitBlocked(t)
	hold.claimAndCommit(t)

	if res := <-done; res.status != http.StatusOK {
		t.Fatalf("import racing a claim: %d %s, want 200", res.status, res.body)
	}
	if n := taskRows(t, "entity_id = $1 AND transition = 'Remind'", id); n != 0 {
		t.Errorf("Remind task rows = %d, want 0", n)
	}
}

func TestScheduledTaskWrites_Import_PersistentConflict_409ThenReimportSucceeds(t *testing.T) {
	const model = "e2e-stw-import-persist"
	setupModelWithWorkflow(t, model, importBothScheduled)
	id := createEntityE2E(t, model, 1, schedWritesPayload)
	trig := installConflictTrigger(t, "model_name", model)

	requireConflictProblem(t, resultOf(doAuthOnceRaw(e2eCtx(t), http.MethodPost, importPath(model), importRemindUnscheduled)))
	if got, want := trig.refusals(t), int64(1+common.TaskConflictRetries); got != want {
		t.Errorf("server attempts = %d, want %d", got, want)
	}
	status, exported := exportWorkflowE2E(t, model, 1)
	if status != http.StatusOK {
		t.Fatalf("export: %d", status)
	}
	if raw := fmt.Sprint(exported); !strings.Contains(raw, "Remind") || strings.Count(raw, "delayMs") != 1 {
		t.Errorf("exported workflows = %s, want Remind without a schedule: the save comes before the removal", raw)
	}
	if n := taskRows(t, "entity_id = $1", id); n != 2 {
		t.Errorf("task rows = %d, want 2", n)
	}

	trig.remove(t)
	if res := resultOf(doAuthOnceRaw(e2eCtx(t), http.MethodPost, importPath(model), importRemindUnscheduled)); res.status != http.StatusOK {
		t.Fatalf("re-import: %d %s", res.status, res.body)
	}
	if n := taskRows(t, "entity_id = $1 AND transition = 'Remind'", id); n != 0 {
		t.Errorf("Remind task rows after re-import = %d, want 0", n)
	}
}
