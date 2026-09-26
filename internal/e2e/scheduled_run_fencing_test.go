package e2e_test

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/app"
)

// scheduled_run_fencing_test.go — a run that lost its life or its claim
// cannot commit, stamp, mark or record; the per-pnode run limits; and the
// scheduler's own PostgreSQL pool under pressure.

// injectReclaim gives the entity's RUNNING task a new claim, as another
// pnode's lost-owner claim would, and returns it.
func injectReclaim(t *testing.T, s *schedDB, entityID string) (token, owner string) {
	t.Helper()
	token, owner = uuid.NewString(), uuid.NewString()
	tag, err := s.pool.Exec(context.Background(),
		`UPDATE scheduled_tasks SET claim_token = $2, claim_owner = $3, lost_owners = lost_owners + 1
		  WHERE tenant_id = $1 AND entity_id = $4 AND status = 'RUNNING'`,
		harnessTenant, token, owner, entityID)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("inject a reclaim: rows %d, err %v", tag.RowsAffected(), err)
	}
	return token, owner
}

// TestSchedFence_ReArmedLifeRefusesOldRun: while the run's processor is held,
// a client update in the source state re-arms the task (a new life). The old
// run's commit writes the task row, which the client's commit changed after
// the run began: C1 refuses it; the re-read sees a new life: superseded,
// nothing recorded (§5.2). The new life fires; exactly one fire.
func TestSchedFence_ReArmedLifeRefusesOldRun(t *testing.T) {
	h, s := newSchedulerHarness(t, nil)
	model, tag := uniq("sf-rearm"), uniq("sf-rearm-tag")
	release := make(chan struct{})
	rel := closeOnce(release)
	t.Cleanup(rel)
	cn := h.AttachCnode(t, cnodeSpec{name: "p", tags: []string{tag}, script: scriptHoldFirst(release)})
	h.SetupModelWithWorkflow(t, model, fireOpenToDone("sf-rearm-wf", 100, 0, sProc("p", "SYNC", tag, true)))
	id := createOpen(t, h, model, workflowSampleModel)

	awaitDBCondition(t, scheduledFireTimeout, "the run's processor", func() bool { return len(cn.Received()) == 1 })

	resp := h.DoAuth(t, http.MethodPut, "/api/entity/JSON/"+id, `{"name":"Test Order","amount":3,"status":"draft"}`, "")
	if body := h.readBody(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("client update: %d %s", resp.StatusCode, body)
	}
	// Watch the row from here on: the old run must never record an attempt
	// or a failure on it. The watcher reads without t (it is not the test
	// goroutine) and stops before the harness closes the pool.
	var worst atomic.Int32
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
			var recorded bool
			if err := s.pool.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM scheduled_tasks
				WHERE tenant_id = $1 AND entity_id = $2 AND (attempts > 0 OR status = 'FAILED'))`,
				harnessTenant, id).Scan(&recorded); err == nil && recorded {
				worst.Store(1)
			}
		}
	}()
	stopWatch := sync.OnceFunc(func() { close(stop); wg.Wait() })
	t.Cleanup(stopWatch)
	rel()
	awaitCallbackEntityState(t, h, id, "Done", scheduledFireTimeout)
	stopWatch()

	if worst.Load() != 0 {
		t.Error("the superseded run recorded an attempt or a failure on the new life")
	}
	events := schedEvents(t, h, id)
	if n := len(smEventsOfType(events, "SCHEDULED_TRANSITION_FIRE")); n != 1 {
		t.Errorf("%d fires; want exactly 1 (the new life's)", n)
	}
	if n := len(smEventsOfType(events, "SCHEDULED_TRANSITION_ARM")); n != 2 {
		t.Errorf("%d arms; want 2 (the create and the update)", n)
	}
	if amount, _ := h.GetEntityData(t, id)["amount"].(float64); amount != 3 {
		t.Errorf("amount = %v; want the client's 3", amount)
	}
	if n := len(cn.Received()); n != 2 {
		t.Errorf("the processor was sent %d times; want 2 (old run, new life)", n)
	}
}

// TestSchedFence_ReclaimedTaskRefusesOldRun: the RUNNING task is claimed from
// under the run. (a) Its final commit writes the task row: C1 refuses it and
// the re-read sees another claim — superseded, nothing recorded. (b) Its next
// COMMIT_BEFORE_DISPATCH segment's stamp is fenced by the claim token and
// refused, so the segment never commits and its processor is never sent.
func TestSchedFence_ReclaimedTaskRefusesOldRun(t *testing.T) {
	for _, tc := range []struct {
		name  string
		procs func(tagA, tagB string) []map[string]any
	}{
		{"FinalCommit", func(a, _ string) []map[string]any { return []map[string]any{sProc("p1", "SYNC", a, true)} }},
		{"SegmentStamp", func(a, b string) []map[string]any {
			return []map[string]any{sProc("p1", "SYNC", a, true), sProc("p2", "COMMIT_BEFORE_DISPATCH", b, true)}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, s := newSchedulerHarness(t, nil)
			model, tagA, tagB := uniq("sf-reclaim"), uniq("sf-reclaim-a"), uniq("sf-reclaim-b")
			gotWork, release := make(chan struct{}, 1), make(chan struct{})
			rel := closeOnce(release)
			t.Cleanup(rel)
			h.AttachCnode(t, cnodeSpec{name: "a", tags: []string{tagA}, script: holdScript(gotWork, release)})
			b := h.AttachCnode(t, cnodeSpec{name: "b", tags: []string{tagB}})
			h.SetupModelWithWorkflow(t, model, fireOpenToDone("sf-reclaim-wf", 100, 0, tc.procs(tagA, tagB)...))
			id := createOpen(t, h, model, workflowSampleModel)

			<-gotWork
			token, owner := injectReclaim(t, s, id)
			rel()

			// The run ends; the scheduler stays live for a few retry
			// delays (max 4s), in which the old run could try to record.
			awaitSchedulerLiveFor(t, h, 4*time.Second)
			r := mustTask(t, s, id)
			if r.Status != "RUNNING" || r.ClaimToken != token || r.ClaimOwner != owner || r.Attempts != 0 || r.LastError != "" {
				t.Errorf("task = %+v; want the injected claim untouched and nothing recorded by the old run", r)
			}
			requireState(t, h, id, "Open")
			if hasSMEventType(schedEvents(t, h, id), "SCHEDULED_TRANSITION_FIRE", "") {
				t.Error("the reclaimed run committed a fire")
			}
			if n := len(b.Received()); n != 0 {
				t.Errorf("the segment's processor was sent %d times; want 0", n)
			}
		})
	}
}

// TestSchedFence_SupersededOwnerSendsNoUnsafe: the old run is held in its
// idempotent first processor while a client update re-arms the task; the new
// life runs to completion. When the old run resumes, its MarkUnsafe before the
// unsafe second processor is refused (a stale arm token): the unsafe
// processor is sent once, by the new life only (§5.5).
func TestSchedFence_SupersededOwnerSendsNoUnsafe(t *testing.T) {
	h, s := newSchedulerHarness(t, nil)
	model, tagA, tagB := uniq("sf-noun"), uniq("sf-noun-a"), uniq("sf-noun-b")
	release := make(chan struct{})
	rel := closeOnce(release)
	t.Cleanup(rel)
	a := h.AttachCnode(t, cnodeSpec{name: "a", tags: []string{tagA}, script: scriptHoldFirst(release)})
	b := h.AttachCnode(t, cnodeSpec{name: "b", tags: []string{tagB}})
	h.SetupModelWithWorkflow(t, model, fireOpenToDone("sf-noun-wf", 100, 0,
		sProc("p1", "SYNC", tagA, true), sProc("p2", "SYNC", tagB, false)))
	id := createOpen(t, h, model, workflowSampleModel)

	awaitDBCondition(t, scheduledFireTimeout, "the old run's p1", func() bool { return len(a.Received()) == 1 })
	resp := h.DoAuth(t, http.MethodPut, "/api/entity/JSON/"+id, `{"name":"Test Order","amount":4,"status":"draft"}`, "")
	if body := h.readBody(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("client update: %d %s", resp.StatusCode, body)
	}
	awaitCallbackEntityState(t, h, id, "Done", scheduledFireTimeout) // the new life
	rel()                                                            // the old run resumes
	awaitSchedulerLiveFor(t, h, 3*time.Second)

	if n := len(b.Received()); n != 1 {
		t.Errorf("the unsafe processor was sent %d times; want 1 (the new life)", n)
	}
	if n := len(smEventsOfType(schedEvents(t, h, id), "SCHEDULED_TRANSITION_FIRE")); n != 1 {
		t.Errorf("%d fires; want 1", n)
	}
	if _, ok := s.task(t, id, "Fire"); ok {
		t.Error("a task remains after the fire")
	}
}

// TestSchedLimits_MaxRunsAndFreedSlotClaimsAtOnce: MAX_RUNS 2 on a 10s scan
// interval. Three due tasks: two runs start, the third waits; when one run
// ends, the third is claimed at once — long before the next scan (§6.1).
func TestSchedLimits_MaxRunsAndFreedSlotClaimsAtOnce(t *testing.T) {
	h, s := newSchedulerHarness(t, func(cfg *app.Config) {
		cfg.Scheduler.MaxRuns = 2
		cfg.Scheduler.MaxRunsPerTenant = 2
		cfg.Scheduler.ScanInterval = 10 * time.Second
	})
	model, tag := uniq("sl-max"), uniq("sl-max-tag")
	tokens := make(chan struct{}, 3)
	cn := h.AttachCnode(t, cnodeSpec{name: "p", tags: []string{tag}, script: func(ctx context.Context, _ receivedCallout, _ *reqCtx) cnodeReply {
		select {
		case <-tokens:
		case <-ctx.Done():
			return neverAnswer()
		}
		return answerOK()
	}})
	t.Cleanup(func() {
		for i := 0; i < 3; i++ {
			select {
			case tokens <- struct{}{}:
			default:
			}
		}
	})
	h.SetupModelWithWorkflow(t, model, fireOpenToDone("sl-max-wf", 100, 0, sProc("p", "SYNC", tag, true)))
	ids := []string{createOpen(t, h, model, workflowSampleModel), createOpen(t, h, model, workflowSampleModel), createOpen(t, h, model, workflowSampleModel)}

	awaitDBCondition(t, 25*time.Second, "two runs", func() bool { return len(cn.Received()) >= 2 })
	// A claim is one statement, so a scan that broke the limit would show a
	// third RUNNING row by now; the third task, due as long as the others,
	// waits.
	running := s.count(t, `SELECT count(*) FROM scheduled_tasks WHERE tenant_id = $1 AND status = 'RUNNING'`, harnessTenant)
	waiting := s.count(t, `SELECT count(*) FROM scheduled_tasks WHERE tenant_id = $1 AND status = 'WAITING'`, harnessTenant)
	if running != 2 || waiting != 1 || len(cn.Received()) != 2 {
		t.Fatalf("running %d, waiting %d, processors sent %d; want MAX_RUNS 2 running and 1 waiting", running, waiting, len(cn.Received()))
	}
	freedAt := time.Now()
	tokens <- struct{}{}
	awaitDBCondition(t, 3*time.Second, "the third run, claimed when the slot freed", func() bool { return len(cn.Received()) == 3 })
	if since := time.Since(freedAt); since > 3*time.Second {
		t.Errorf("the third run started %s after the slot freed; want at once, not at the next 10s scan", since)
	}
	tokens <- struct{}{}
	tokens <- struct{}{}
	for _, id := range ids {
		awaitCallbackEntityState(t, h, id, "Done", scheduledFireTimeout)
	}
}

// holdMainPool starts n creates of a model whose SYNC processor is held, so
// each keeps one main-pool connection in an open transaction. It returns once
// the cnode has all n, and a func that releases them and waits for the creates.
func holdMainPool(t *testing.T, h *callbackHarness, n int) func() {
	t.Helper()
	model, tag := uniq("pool-hold"), uniq("pool-hold-tag")
	gotWork, release := make(chan struct{}, n), make(chan struct{})
	rel := closeOnce(release)
	t.Cleanup(rel)
	h.AttachCnode(t, cnodeSpec{name: "holder", tags: []string{tag}, script: func(ctx context.Context, _ receivedCallout, _ *reqCtx) cnodeReply {
		gotWork <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return neverAnswer()
		}
		return answerOK()
	}})
	h.SetupModelWithWorkflow(t, model, schedDoc("pool-hold-wf", map[string]any{
		"Open": map[string]any{"transitions": []any{map[string]any{"name": "Go", "next": "Done", "manual": false,
			"processors": []any{sProc("h", "SYNC", tag, true)}}}},
		"Done": map[string]any{},
	}))
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = h.CreateEntityRaw(model, 1, workflowSampleModel) }()
	}
	for i := 0; i < n; i++ {
		select {
		case <-gotWork:
		case <-time.After(15 * time.Second):
			t.Fatalf("only %d of %d holders reached their processor", i, n)
		}
	}
	return func() { rel(); wg.Wait() }
}

// poolProbe sets up a model for requirePoolExhausted while the pool is free.
func poolProbe(t *testing.T, h *callbackHarness) string {
	t.Helper()
	model := uniq("pool-probe")
	h.SetupModelWithWorkflow(t, model, schedDoc("pool-probe-wf", map[string]any{"Open": map[string]any{}}))
	return model
}

// requirePoolExhausted proves the main pool has no free connection: a create
// of the probe model does not succeed within a second.
func requirePoolExhausted(t *testing.T, h *callbackHarness, probeModel string) {
	t.Helper()
	done := make(chan createEntityResult, 1)
	go func() { done <- h.CreateEntityRaw(probeModel, 1, workflowSampleModel) }()
	select {
	case res := <-done:
		if res.status == http.StatusOK {
			t.Fatalf("a create succeeded while the main pool should be exhausted")
		}
	case <-time.After(time.Second):
	}
}

// maxHeartbeat is the newest liveness stamp of any scheduler owner.
func maxHeartbeat(t *testing.T, s *schedDB) time.Time {
	t.Helper()
	var at time.Time
	if err := s.pool.QueryRow(context.Background(), `SELECT max(heartbeat_at) FROM scheduler_owners`).Scan(&at); err != nil {
		t.Fatalf("read liveness: %v", err)
	}
	return at
}

// TestSchedPool_HeartbeatNotStarvedByMainPool: every main-pool connection is
// held by an open entity transaction; the heartbeat, on its own connection
// (C4), keeps advancing the liveness record.
func TestSchedPool_HeartbeatNotStarvedByMainPool(t *testing.T) {
	h, s := newSchedulerHarness(t, func(*app.Config) { t.Setenv("CYODA_POSTGRES_MAX_CONNS", "2") })
	probe := poolProbe(t, h)
	release := holdMainPool(t, h, 2)
	defer release()
	requirePoolExhausted(t, h, probe)

	// One heartbeat a second: two seconds of advance within five.
	before := maxHeartbeat(t, s)
	awaitDBCondition(t, 5*time.Second, "the heartbeat advancing 2s with the main pool exhausted",
		func() bool { return maxHeartbeat(t, s).Sub(before) >= 2*time.Second })
}

// TestSchedPool_AsyncSearchReclaimNotStarved: an async-search job whose owner
// is gone is reclaimed, and then heartbeated, on the scheduler pool while every
// main-pool connection is held (§10.2). The heartbeat keeps the claim live for
// several stale windows, so the job is not reclaimed a second time.
func TestSchedPool_AsyncSearchReclaimNotStarved(t *testing.T) {
	h, s := newSchedulerHarness(t, func(cfg *app.Config) {
		t.Setenv("CYODA_POSTGRES_MAX_CONNS", "2")
		cfg.SearchJobHeartbeatInterval = 250 * time.Millisecond
		cfg.SearchJobStaleAfter = time.Second
	})
	model := uniq("pool-search")
	h.SetupModelWithWorkflow(t, model, schedDoc("pool-search-wf", map[string]any{"Open": map[string]any{}}))
	createOpen(t, h, model, workflowSampleModel)
	probe := poolProbe(t, h)

	// A job as CreateJob writes it (a point in time and search options, both
	// read back on re-execution) whose owner stopped heartbeating an hour ago.
	jobID := uuid.NewString()
	pit := time.Now().Add(time.Minute).UTC()
	opts, err := json.Marshal(struct {
		Limit       int       `json:"limit"`
		PointInTime time.Time `json:"pointInTime"`
	}{Limit: 0, PointInTime: pit})
	if err != nil {
		t.Fatalf("marshal search opts: %v", err)
	}
	if _, err := s.pool.Exec(context.Background(), `
		INSERT INTO search_jobs (id, tenant_id, status, model_name, model_ver, condition, point_in_time, search_opts,
		                         result_count, error, created_at, heartbeat_time, calc_ms, epoch)
		VALUES ($1, $2, 'RUNNING', $3, '1', '{"type":"group","operator":"AND","conditions":[]}'::jsonb, $4, $5,
		        0, '', now() - interval '1 hour', now() - interval '1 hour', 0, 1)`,
		jobID, harnessTenant, model, pit, opts); err != nil {
		t.Fatalf("seed an orphaned job: %v", err)
	}
	release := holdMainPool(t, h, 2)
	defer release()
	requirePoolExhausted(t, h, probe)

	readJob := func() (epoch int64, hb time.Time) {
		if err := s.pool.QueryRow(context.Background(),
			`SELECT epoch, heartbeat_time FROM search_jobs WHERE tenant_id = $1 AND id = $2`, harnessTenant, jobID).Scan(&epoch, &hb); err != nil {
			t.Fatalf("read the job: %v", err)
		}
		return epoch, hb
	}
	awaitDBCondition(t, 10*time.Second, "the reclaim", func() bool { e, _ := readJob(); return e >= 2 })
	claimed, first := readJob()

	// Three stale windows of heartbeats (one every 250ms) within five seconds:
	// a heartbeat that stamps once and then waits on the main pool fails here.
	awaitDBCondition(t, 5*time.Second, "the reclaimed job's heartbeat advancing 3s with the main pool exhausted",
		func() bool { _, hb := readJob(); return hb.Sub(first) >= 3*time.Second })
	if epoch, _ := readJob(); epoch != claimed {
		t.Fatalf("the job was reclaimed again (epoch %d, claimed at %d): its heartbeat did not keep the claim live", epoch, claimed)
	}
}

// TestSchedPool_LockTimeoutOnTaskRowLock: another entity's transaction holds
// the scheduled task's row (its joined callback updated the entity in the
// source state, which re-arms the task) and stays open. The run fails; its
// RecordAttempt on the scheduler pool waits on that row lock and gives up
// after lock_timeout (2s), then retries — no scheduler-pool statement waits
// longer. When the holder commits, the re-arm stands and the old run's
// bookkeeping is refused: the new life has attempts 0.
func TestSchedPool_LockTimeoutOnTaskRowLock(t *testing.T) {
	h, s := newSchedulerHarness(t, nil)
	model, tag := uniq("pool-lock"), uniq("pool-lock-tag")
	gotWork, release := make(chan struct{}, 1), make(chan struct{})
	rel := closeOnce(release)
	t.Cleanup(rel)
	var runCalls atomic.Int32
	h.AttachCnode(t, cnodeSpec{name: "run", tags: []string{tag}, script: func(ctx context.Context, _ receivedCallout, _ *reqCtx) cnodeReply {
		if runCalls.Add(1) > 1 {
			return neverAnswer()
		}
		gotWork <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return neverAnswer()
		}
		return answerFail("run boom")
	}})
	h.SetupModelWithWorkflow(t, model, fireOpenToDone("pool-lock-wf", 100, 0, sProc("p", "SYNC", tag, true)))
	id := createOpen(t, h, model, workflowSampleModel)
	<-gotWork
	before := mustTask(t, s, id)

	// The holder: a create whose processor updates the scheduled entity
	// through its joined callback, then waits.
	hModel, hTag := uniq("pool-lock-holder"), uniq("pool-lock-holder-tag")
	updated, holdRelease := make(chan int, 1), make(chan struct{})
	holdRel := closeOnce(holdRelease)
	t.Cleanup(holdRel)
	h.AttachCnode(t, cnodeSpec{name: "holder", tags: []string{hTag}, script: func(ctx context.Context, _ receivedCallout, rc *reqCtx) cnodeReply {
		res, err := rc.UpdateEntity(id, `{"name":"Test Order","amount":9,"status":"draft"}`)
		st := -1
		if err == nil {
			st = res.StatusCode
		}
		updated <- st
		select {
		case <-holdRelease:
		case <-ctx.Done():
			return neverAnswer()
		}
		return answerOK()
	}})
	h.SetupModelWithWorkflow(t, hModel, schedDoc("pool-lock-holder-wf", map[string]any{
		"Open": map[string]any{"transitions": []any{map[string]any{"name": "Go", "next": "Done", "manual": false,
			"processors": []any{sProc("h", "SYNC", hTag, true)}}}},
		"Done": map[string]any{},
	}))
	holderDone := make(chan createEntityResult, 1)
	go func() { holderDone <- h.CreateEntityRaw(hModel, 1, workflowSampleModel) }()
	if st := <-updated; st != http.StatusOK {
		t.Fatalf("the holder's joined update answered %d; want 200", st)
	}

	rel() // the run fails; its RecordAttempt meets the holder's row lock
	var seen int
	var longest float64
	deadline := time.Now().Add(7 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		var wait float64
		if err := s.pool.QueryRow(context.Background(), `
			SELECT count(*), COALESCE(max(extract(epoch FROM now() - query_start)), 0)::float8
			  FROM pg_stat_activity
			 WHERE datname = current_database() AND wait_event_type = 'Lock'
			   AND query ILIKE '%scheduled_tasks%' AND pid <> pg_backend_pid()`).Scan(&n, &wait); err != nil {
			t.Fatalf("pg_stat_activity: %v", err)
		}
		if n > 0 {
			seen++
		}
		if wait > longest {
			longest = wait
		}
		time.Sleep(100 * time.Millisecond)
	}
	if seen == 0 {
		t.Fatal("no scheduler statement ever waited on the task row; the scenario did not hold the lock")
	}
	if longest > 3 {
		t.Errorf("a statement waited %.1fs on the task-row lock; lock_timeout is 2s", longest)
	}

	holdRel()
	if res := <-holderDone; res.status != http.StatusOK {
		t.Fatalf("holder create: %d %s", res.status, res.body)
	}
	r := s.awaitTask(t, id, "Fire", 10*time.Second, "the new life",
		func(r taskRow, ok bool) bool { return ok && r.ArmToken != before.ArmToken })
	if r.Attempts != 0 || r.LastError != "" {
		t.Errorf("new life = %+v; the old run's refused bookkeeping must not reach it", r)
	}
}
