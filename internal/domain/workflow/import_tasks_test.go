package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda-platform/cyoda-go/internal/testing/taskconflict"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

const (
	importTenant spi.TenantID = "import-tasks-tenant"
	otherTenant  spi.TenantID = "import-tasks-other-tenant"
)

// dropRemindImport keeps AutoClose scheduled and makes Remind manual with no
// schedule.
const dropRemindImport = `{"importMode":"REPLACE","workflows":[{
	"version":"1.1","name":"import-sched","initialState":"OPEN","active":true,
	"states":{
		"OPEN":{"transitions":[
			{"name":"AutoClose","next":"CLOSED","manual":false,"schedule":{"delayMs":3600000}},
			{"name":"Remind","next":"REMINDED","manual":true}]},
		"CLOSED":{},
		"REMINDED":{}}}]}`

// conflictOnCommitTxMgr refuses the first n commits the way
// first-committer-wins does: the transaction is rolled back and the commit
// answers spi.ErrConflict. It counts every commit.
type conflictOnCommitTxMgr struct {
	spi.TransactionManager
	mu      sync.Mutex
	refuse  int
	commits int
}

func (m *conflictOnCommitTxMgr) Commit(ctx context.Context, txID string) error {
	refuse := func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.commits++
		if m.refuse == 0 {
			return false
		}
		m.refuse--
		return true
	}()
	if refuse {
		if err := m.TransactionManager.Rollback(ctx, txID); err != nil {
			return fmt.Errorf("rollback of refused commit: %w", err)
		}
		return fmt.Errorf("test: task row written after this transaction began: %w", spi.ErrConflict)
	}
	return m.TransactionManager.Commit(ctx, txID)
}

func (m *conflictOnCommitTxMgr) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.commits
}

type importTaskEnv struct {
	h       *Handler
	ctx     context.Context
	real    *memory.StoreFactory
	factory *taskconflict.Factory
	txMgr   spi.TransactionManager // the memory manager, never wrapped
	commits *conflictOnCommitTxMgr // what the engine commits through
	plan    *taskconflict.Plan
	advance func(deltaMs int64)
}

func userCtx(tenant spi.TenantID) context.Context {
	return spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID: "import-user", UserName: "Import", Tenant: spi.Tenant{ID: tenant, Name: string(tenant)}, Roles: []string{"ROLE_ADMIN"},
	})
}

func newImportTaskEnv(t *testing.T) *importTaskEnv {
	t.Helper()
	real := memory.NewStoreFactory()
	t.Cleanup(func() { real.Close() })
	txMgr, err := real.TransactionManager(context.Background())
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	ctx := userCtx(importTenant)
	node := schema.NewObjectNode()
	node.SetChild("k", schema.NewLeafNode(schema.Integer))
	raw, err := schema.Marshal(node)
	if err != nil {
		t.Fatalf("schema.Marshal: %v", err)
	}
	for _, tenantCtx := range []context.Context{ctx, userCtx(otherTenant)} {
		ms, err := real.ModelStore(tenantCtx)
		if err != nil {
			t.Fatalf("ModelStore: %v", err)
		}
		for _, name := range []string{"sched-import", "sched-other"} {
			if err := ms.Save(tenantCtx, &spi.ModelDescriptor{Ref: spi.ModelRef{EntityName: name, ModelVersion: "1"}, State: spi.ModelLocked, Schema: raw}); err != nil {
				t.Fatalf("ModelStore.Save: %v", err)
			}
		}
	}
	plan := taskconflict.NewPlan()
	factory := &taskconflict.Factory{StoreFactory: real, Plan: plan}
	commits := &conflictOnCommitTxMgr{TransactionManager: txMgr}
	clock, advance := steppableClock(time.Now().UnixMilli())
	engine := NewEngine(factory, common.NewDefaultUUIDGenerator(), commits, WithScheduledClock(clock))
	return &importTaskEnv{
		h: New(factory, engine, 60*time.Second), ctx: ctx, real: real, factory: factory,
		txMgr: txMgr, commits: commits, plan: plan, advance: advance,
	}
}

// armIn arms one OPEN task per transition for entityID of model, in tenant.
func (e *importTaskEnv) armIn(t *testing.T, tenant spi.TenantID, model, entityID string, transitions ...string) {
	t.Helper()
	txID, txCtx, err := e.txMgr.Begin(userCtx(tenant))
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	sts, err := e.real.ScheduledTaskStore(txCtx)
	if err != nil {
		t.Fatalf("ScheduledTaskStore: %v", err)
	}
	arm := make([]spi.ScheduledTask, 0, len(transitions))
	for _, tr := range transitions {
		arm = append(arm, spi.ScheduledTask{
			ID: taskID(tenant, entityID, "OPEN", tr), TenantID: tenant, Type: spi.ScheduledTaskFireTransition,
			ScheduledTime: time.Now().Add(time.Hour).UnixMilli(), EntityID: entityID,
			ModelName: model, ModelVersion: 1, Transition: tr, SourceState: "OPEN",
		})
	}
	if _, err := sts.ReconcileForEntity(txCtx, spi.ReconcileRequest{
		TenantID: tenant, EntityID: entityID, CurrentState: "OPEN", Arm: arm,
	}); err != nil {
		t.Fatalf("ReconcileForEntity: %v", err)
	}
	if err := e.txMgr.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

func (e *importTaskEnv) arm(t *testing.T, model, entityID string, transitions ...string) {
	t.Helper()
	e.armIn(t, importTenant, model, entityID, transitions...)
}

// transitionsIn lists the committed transitions of model's tasks in tenant,
// sorted.
func (e *importTaskEnv) transitionsIn(t *testing.T, tenant spi.TenantID, model string) []string {
	t.Helper()
	ctx := userCtx(tenant)
	sts, err := e.real.ScheduledTaskStore(ctx)
	if err != nil {
		t.Fatalf("ScheduledTaskStore: %v", err)
	}
	page, err := sts.Query(ctx, tenant, spi.ScheduledTaskQuery{ModelName: model, ModelVersion: 1, Limit: 1000})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	out := []string{}
	for _, it := range page.Items {
		out = append(out, it.Transition)
	}
	sort.Strings(out)
	return out
}

func (e *importTaskEnv) transitions(t *testing.T, model string) []string {
	t.Helper()
	return e.transitionsIn(t, importTenant, model)
}

func (e *importTaskEnv) importWorkflowsCtx(t *testing.T, ctx context.Context, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/model/sched-import/1/workflow/import", strings.NewReader(body)).WithContext(ctx)
	rec := httptest.NewRecorder()
	e.h.ImportEntityModelWorkflow(rec, req, "sched-import", 1)
	return rec
}

func (e *importTaskEnv) importWorkflows(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	return e.importWorkflowsCtx(t, e.ctx, body)
}

func (e *importTaskEnv) seed(t *testing.T) {
	t.Helper()
	e.arm(t, "sched-import", "e-1", "AutoClose", "Remind")
	e.arm(t, "sched-other", "e-2", "Remind")
}

func requireRetryableConflict(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusConflict {
		t.Fatalf("import: %d %s, want 409", rec.Code, rec.Body)
	}
	var pd struct {
		Properties struct {
			ErrorCode string `json:"errorCode"`
			Retryable bool   `json:"retryable"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &pd); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if pd.Properties.ErrorCode != common.ErrCodeConflict || !pd.Properties.Retryable {
		t.Errorf("errorCode=%q retryable=%v, want CONFLICT retryable", pd.Properties.ErrorCode, pd.Properties.Retryable)
	}
}

func (e *importTaskEnv) requireSaved(t *testing.T) {
	t.Helper()
	wfStore, err := e.real.WorkflowStore(e.ctx)
	if err != nil {
		t.Fatalf("WorkflowStore: %v", err)
	}
	saved, err := wfStore.Get(e.ctx, spi.ModelRef{EntityName: "sched-import", ModelVersion: "1"})
	if err != nil || len(saved) != 1 || saved[0].Name != "import-sched" {
		t.Fatalf("saved workflows = %+v (err %v), want the imported one: the save comes first", saved, err)
	}
}

func TestImport_RemovesTasksOfTransitionsNoLongerScheduled(t *testing.T) {
	e := newImportTaskEnv(t)
	e.seed(t)

	if rec := e.importWorkflows(t, dropRemindImport); rec.Code != http.StatusOK {
		t.Fatalf("import: %d %s", rec.Code, rec.Body)
	}
	if got := e.transitions(t, "sched-import"); strings.Join(got, ",") != "AutoClose" {
		t.Errorf("sched-import tasks = %v, want [AutoClose]", got)
	}
	if got := e.transitions(t, "sched-other"); strings.Join(got, ",") != "Remind" {
		t.Errorf("sched-other tasks = %v, want [Remind]: another model's tasks stay", got)
	}
}

// An import in one tenant never touches another tenant's tasks, even of a
// model with the same name and version.
func TestImport_OtherTenantsTasksUntouched(t *testing.T) {
	e := newImportTaskEnv(t)
	e.seed(t)
	e.armIn(t, otherTenant, "sched-import", "e-1", "AutoClose", "Remind")

	if rec := e.importWorkflows(t, dropRemindImport); rec.Code != http.StatusOK {
		t.Fatalf("import: %d %s", rec.Code, rec.Body)
	}
	if got := e.transitions(t, "sched-import"); strings.Join(got, ",") != "AutoClose" {
		t.Errorf("importing tenant's tasks = %v, want [AutoClose]", got)
	}
	if got := e.transitionsIn(t, otherTenant, "sched-import"); strings.Join(got, ",") != "AutoClose,Remind" {
		t.Errorf("other tenant's tasks = %v, want both: an import is scoped to its tenant", got)
	}
}

// The engine falls back to the default workflow when no stored workflow
// matches an entity, and arms its scheduled transitions. The import keeps
// their tasks, as the arm side counts them.
func TestImport_KeepsTasksTheDefaultWorkflowArms(t *testing.T) {
	e := newImportTaskEnv(t)
	e.h.engine.defaultWorkflows = []spi.WorkflowDefinition{{
		Version: "1.1", Name: "default-sched", InitialState: "OPEN", Active: true,
		States: map[string]spi.StateDefinition{
			"OPEN":   {Transitions: []spi.TransitionDefinition{{Name: "DefaultTick", Next: "CLOSED", Schedule: &spi.TransitionSchedule{DelayMs: 1000}}}},
			"CLOSED": {},
		},
	}}
	e.arm(t, "sched-import", "e-1", "AutoClose", "Remind", "DefaultTick")

	if rec := e.importWorkflows(t, dropRemindImport); rec.Code != http.StatusOK {
		t.Fatalf("import: %d %s", rec.Code, rec.Body)
	}
	if got := e.transitions(t, "sched-import"); strings.Join(got, ",") != "AutoClose,DefaultTick" {
		t.Errorf("tasks = %v, want [AutoClose DefaultTick]", got)
	}
}

func TestImport_TaskConflict_RetriedThenSucceeds(t *testing.T) {
	e := newImportTaskEnv(t)
	e.seed(t)
	e.plan.Refuse(taskconflict.DeleteForModel, 2)

	if rec := e.importWorkflows(t, dropRemindImport); rec.Code != http.StatusOK {
		t.Fatalf("import: %d %s", rec.Code, rec.Body)
	}
	if got := e.plan.Calls(taskconflict.DeleteForModel); got != 3 {
		t.Errorf("DeleteForModel calls = %d, want 3", got)
	}
	if got := e.transitions(t, "sched-import"); strings.Join(got, ",") != "AutoClose" {
		t.Errorf("tasks = %v, want [AutoClose]", got)
	}
}

func TestImport_CommitConflict_RetriedThenSucceeds(t *testing.T) {
	e := newImportTaskEnv(t)
	e.seed(t)
	e.commits.refuse = 1

	if rec := e.importWorkflows(t, dropRemindImport); rec.Code != http.StatusOK {
		t.Fatalf("import: %d %s", rec.Code, rec.Body)
	}
	if got := e.plan.Calls(taskconflict.DeleteForModel); got != 2 {
		t.Errorf("DeleteForModel calls = %d, want 2: the refused commit runs the removal again", got)
	}
	if got := e.commits.count(); got != 2 {
		t.Errorf("commits = %d, want 2", got)
	}
	if got := e.transitions(t, "sched-import"); strings.Join(got, ",") != "AutoClose" {
		t.Errorf("tasks = %v, want [AutoClose]", got)
	}
}

func TestImport_CommitConflictPersists_409_WorkflowsAlreadySaved(t *testing.T) {
	e := newImportTaskEnv(t)
	e.seed(t)
	e.commits.refuse = 100

	requireRetryableConflict(t, e.importWorkflows(t, dropRemindImport))
	if got, want := e.commits.count(), 1+common.TaskConflictRetries; got != want {
		t.Errorf("commits = %d, want %d", got, want)
	}
	e.requireSaved(t)
	if got := e.transitions(t, "sched-import"); strings.Join(got, ",") != "AutoClose,Remind" {
		t.Errorf("tasks = %v, want both: every removal rolled back", got)
	}
}

func TestImport_TaskConflictPersists_409_WorkflowsAlreadySaved_ReimportSucceeds(t *testing.T) {
	e := newImportTaskEnv(t)
	e.seed(t)
	e.plan.Refuse(taskconflict.DeleteForModel, 100)

	requireRetryableConflict(t, e.importWorkflows(t, dropRemindImport))
	if got, want := e.plan.Calls(taskconflict.DeleteForModel), 1+common.TaskConflictRetries; got != want {
		t.Errorf("DeleteForModel calls = %d, want %d", got, want)
	}
	e.requireSaved(t)
	if got := e.transitions(t, "sched-import"); strings.Join(got, ",") != "AutoClose,Remind" {
		t.Errorf("tasks = %v, want both: every removal rolled back", got)
	}

	e.plan.Reset()
	if rec := e.importWorkflows(t, dropRemindImport); rec.Code != http.StatusOK {
		t.Fatalf("re-import: %d %s", rec.Code, rec.Body)
	}
	if got := e.transitions(t, "sched-import"); strings.Join(got, ",") != "AutoClose" {
		t.Errorf("tasks after re-import = %v, want [AutoClose]", got)
	}
}

// A task an import that answered 409 left behind stays until the import is
// retried. If it falls due first, the fire door cancels it with an audit
// event: the saved workflows no longer schedule its transition.
func TestImport_LeftoverAfter409_CancelledByTheFireDoorWithAuditEvent(t *testing.T) {
	e := newImportTaskEnv(t)
	entity := makeEntity("e-1", spi.ModelRef{EntityName: "sched-import", ModelVersion: "1"}, map[string]any{"k": 1})
	entity.Meta.State = "OPEN"
	entity.Meta.TenantID = importTenant
	entity.Meta.TransactionID = "seed-tx-1"
	es, err := e.real.EntityStore(e.ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	if _, err := es.Save(e.ctx, entity); err != nil {
		t.Fatalf("Save entity: %v", err)
	}
	e.arm(t, "sched-import", "e-1", "Remind")
	e.plan.Refuse(taskconflict.DeleteForModel, 100)
	requireRetryableConflict(t, e.importWorkflows(t, dropRemindImport))
	e.plan.Reset()

	e.advance(2 * time.Hour.Milliseconds())
	r := fireDue(t, e.h.engine, e.ctx, taskID(importTenant, "e-1", "OPEN", "Remind"))
	if r.Err != nil {
		t.Fatalf("FireScheduledTransition: %v", r.Err)
	}
	if r.Outcome != OutcomeCancelled {
		t.Fatalf("outcome = %v, want Cancelled", r.Outcome)
	}
	if n := countAuditEvents(t, e.real, e.ctx, "e-1", spi.SMEventScheduledTransitionCancelled); n != 1 {
		t.Errorf("SCHEDULED_TRANSITION_CANCEL events = %d, want 1", n)
	}
	if got := e.transitions(t, "sched-import"); len(got) != 0 {
		t.Errorf("tasks = %v, want none", got)
	}
}

func TestImport_TaskStoreFailure_500NotRetried(t *testing.T) {
	e := newImportTaskEnv(t)
	e.seed(t)
	e.plan.Fail(taskconflict.DeleteForModel, errors.New("task store unreachable"))

	if rec := e.importWorkflows(t, dropRemindImport); rec.Code != http.StatusInternalServerError {
		t.Fatalf("import: %d %s, want 500", rec.Code, rec.Body)
	}
	if got := e.plan.Calls(taskconflict.DeleteForModel); got != 1 {
		t.Errorf("DeleteForModel calls = %d, want 1", got)
	}
}

// An import that joined a transaction (a routed callback) removes the tasks
// in that transaction and neither commits nor retries it: the owner's commit
// decides. On a backend whose workflow save joins the transaction too, the
// save and the removal then commit or roll back together.
func TestImport_Joined_RemovesInTheOwnersTransaction(t *testing.T) {
	for _, commit := range []bool{true, false} {
		t.Run(fmt.Sprintf("ownerCommits=%v", commit), func(t *testing.T) {
			e := newImportTaskEnv(t)
			e.seed(t)
			txID, txCtx, err := e.txMgr.Begin(e.ctx)
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}

			if rec := e.importWorkflowsCtx(t, txCtx, dropRemindImport); rec.Code != http.StatusOK {
				t.Fatalf("import: %d %s", rec.Code, rec.Body)
			}
			if got := e.commits.count(); got != 0 {
				t.Errorf("commits = %d, want 0: the owner commits", got)
			}
			if got := e.transitions(t, "sched-import"); strings.Join(got, ",") != "AutoClose,Remind" {
				t.Errorf("committed tasks before the owner's commit = %v, want both", got)
			}

			want := "AutoClose,Remind"
			if commit {
				if err := e.txMgr.Commit(txCtx, txID); err != nil {
					t.Fatalf("owner Commit: %v", err)
				}
				want = "AutoClose"
			} else if err := e.txMgr.Rollback(txCtx, txID); err != nil {
				t.Fatalf("owner Rollback: %v", err)
			}
			if got := e.transitions(t, "sched-import"); strings.Join(got, ",") != want {
				t.Errorf("tasks = %v, want [%s]", got, want)
			}
		})
	}
}

func TestImport_Joined_ConflictNotRetried_409(t *testing.T) {
	e := newImportTaskEnv(t)
	e.seed(t)
	txID, txCtx, err := e.txMgr.Begin(e.ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	t.Cleanup(func() { _ = e.txMgr.Rollback(context.Background(), txID) })
	e.plan.Refuse(taskconflict.DeleteForModel, 100)

	requireRetryableConflict(t, e.importWorkflowsCtx(t, txCtx, dropRemindImport))
	if got := e.plan.Calls(taskconflict.DeleteForModel); got != 1 {
		t.Errorf("DeleteForModel calls = %d, want 1: a joined request is not retried", got)
	}
}

func TestScheduledTransitions_KeepRule(t *testing.T) {
	sched := &spi.TransitionSchedule{DelayMs: 1000}
	keep := scheduledTransitions([]spi.WorkflowDefinition{
		{Name: "active", Active: true, States: map[string]spi.StateDefinition{
			"A": {Transitions: []spi.TransitionDefinition{
				{Name: "armed", Schedule: sched},
				{Name: "manual", Manual: true, Schedule: sched},
				{Name: "disabled", Disabled: true, Schedule: sched},
				{Name: "plain"},
			}},
		}},
		{Name: "inactive", Active: false, States: map[string]spi.StateDefinition{
			"B": {Transitions: []spi.TransitionDefinition{{Name: "armed-inactive", Schedule: sched}}},
		}},
	})
	cases := []struct {
		state, transition string
		want              bool
	}{
		{"A", "armed", true},
		{"A", "manual", false},
		{"A", "disabled", false},
		{"A", "plain", false},
		{"B", "armed-inactive", true},
		{"B", "armed", false},
		{"A", "missing", false},
	}
	for _, c := range cases {
		if got := keep(c.state, c.transition); got != c.want {
			t.Errorf("keep(%q, %q) = %v, want %v", c.state, c.transition, got, c.want)
		}
	}
}
