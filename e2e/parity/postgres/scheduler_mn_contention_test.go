package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cyoda-platform/cyoda-go/e2e/parity"
	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
	"github.com/cyoda-platform/cyoda-go/e2e/parity/fixtureutil"
)

// contentionTenant is one tenant of TestSchedulerMN_ClaimContention.
type contentionTenant struct {
	tenant  parity.Tenant
	c       *client.Client
	ids     []uuid.UUID
	clients []parity.ComputeClient
}

// TestSchedulerMN_ClaimContention: two tenants, 45 entities each, every
// entity with two scheduled transitions out of Open due at the same instant,
// on three claiming pnodes. Pnode 2 has no gossip seeds and a cluster secret
// of its own, so no peer can join it either: its cluster view holds only
// itself, and under claims it is one more claimer. Tenant 0's processors are
// unsafe, tenant 1's idempotent. Each pnode runs at most two tasks, two per
// tenant. Every compute client holds the work it is sent until the scenario
// releases it.
//
// No task is claimed before every task is due. As each entity's create
// returns, the scenario locks the entity's two task rows in a transaction of
// its own, and a claim passes over a row another transaction holds
// (plugins/postgres lockClaimsSQL). The scenario ends that transaction once
// the last task is due, so every claim loop starts against the whole due set
// at once. Each entity's rows are locked one round trip after its create,
// well inside the delay before they are due, so how long the creates take in
// all does not matter.
//
// Asserted, as invariants rather than an interleave:
//   - no entity ever has two RUNNING tasks;
//   - each task is claimed once, by one pnode: the watcher never sees a task
//     under two claim tokens (unless a pnode gave back a claim whose reply it
//     lost, and the processor was still sent once), and each entity's
//     processor is sent exactly once — for an idempotent processor no mark
//     stands between a second claim and a second send;
//   - each entity fires exactly once, down one of its two transitions;
//   - the sibling that did not fire is removed with SCHEDULED_TRANSITION_CANCEL
//     when the entity leaves Open; no task is left, none FAILED;
//   - every pnode, the isolated one included, held claims;
//   - tenants take turns within a claim call: every claim of two tasks made
//     before any processor answered, while both tenants have many due tasks,
//     holds one task of each, and at least one claim is of two tasks.
func TestSchedulerMN_ClaimContention(t *testing.T) {
	t.Parallel()
	const (
		nodes     = 3
		isolated  = 2
		perTenant = 45
		delayMs   = 10000
		// maxRuns is each pnode's run slots; perTenantRuns its runs per tenant.
		maxRuns       = 2
		perTenantRuns = 2
		// createWorkers bounds the concurrent creates.
		createWorkers = 16
	)
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		t.Fatalf("isolated pnode's cluster secret: %v", err)
	}
	s := newSchedMN(t, nodes, fixtureutil.LaunchOpts{NodeEnv: func(i int) []string {
		if i == isolated {
			// No seeds: it joins nobody. Its own secret: the others' joins to
			// it fail, so it is never merged into their cluster either. The
			// secret also keys transaction tokens and peer dispatch; this
			// scenario uses neither across pnodes.
			return []string{"CYODA_SEED_NODES=", "CYODA_HMAC_SECRET=" + hex.EncodeToString(secret)}
		}
		return nil
	}}, fmt.Sprintf("CYODA_SCHEDULER_MAX_RUNS=%d", maxRuns), fmt.Sprintf("CYODA_SCHEDULER_MAX_RUNS_PER_TENANT=%d", perTenantRuns),
		mnLongAnswerEnv)

	// The partial view: pnode 2 is a cluster of one, pnodes 0 and 1 a cluster
	// of two. The log lines are written before a pnode serves; their copy into
	// the capture buffer may lag.
	mnAwait(t, 10*time.Second, "pnode 2 starting as a cluster of one", func() bool {
		return strings.Contains(s.pg.NodeLogs(isolated), `msg="no seeds configured, proceeding as cluster of one"`)
	})
	for i := 0; i < nodes; i++ {
		if i == isolated {
			continue
		}
		mnAwait(t, 10*time.Second, "a connected pnode joining a cluster of two", func() bool {
			return strings.Contains(s.pg.NodeLogs(i), `msg="joined cluster"`)
		})
		if logs := s.pg.NodeLogs(i); !strings.Contains(logs, "members=2") || strings.Contains(logs, "members=3") {
			t.Fatalf("pnode %d's cluster view is not the two connected pnodes", i)
		}
	}

	const model = "mn-contention"
	var tenants []*contentionTenant
	for k := 0; k < 2; k++ {
		// Tenant 0's processors are unsafe: each run writes its mark before
		// the send. Tenant 1's are idempotent: no mark fences a second claim
		// before the send, so a task claimed twice is sent twice.
		idempotent := k == 1
		ct := &contentionTenant{tenant: s.pg.NewTenant(t)}
		ct.c = s.client(0, ct.tenant)
		tag := "mc-" + mnShort()
		// A compute client on every pnode: the isolated pnode cannot hand
		// work to a client it does not know of. A held processor answers
		// when the scenario releases it, within mnLongAnswer.
		for i := 0; i < nodes; i++ {
			ct.clients = append(ct.clients, s.startClient(t, i, ct.tenant, tag, parity.ComputeBehaviourHold))
		}
		wf := mnWorkflow("mn-contention-wf", map[string]any{
			"Open": map[string]any{"transitions": []any{
				map[string]any{"name": "ToA", "next": "A", "manual": false, "schedule": map[string]any{"delayMs": delayMs}, "processors": []any{mnProc("noop", "SYNC", tag, idempotent, mnLongAnswer)}},
				map[string]any{"name": "ToB", "next": "B", "manual": false, "schedule": map[string]any{"delayMs": delayMs}, "processors": []any{mnProc("noop", "SYNC", tag, idempotent, mnLongAnswer)}},
			}},
			"A": map[string]any{},
			"B": map[string]any{},
		})
		if err := ct.c.ImportModel(t, model, 1, mnSample); err != nil {
			t.Fatalf("ImportModel: %v", err)
		}
		if err := ct.c.LockModel(t, model, 1); err != nil {
			t.Fatalf("LockModel: %v", err)
		}
		if err := ct.c.ImportWorkflow(t, model, 1, wf); err != nil {
			t.Fatalf("ImportWorkflow: %v", err)
		}
		tenants = append(tenants, ct)
	}
	tenantIDs := []string{tenants[0].tenant.ID, tenants[1].tenant.ID}

	// The gate: the scenario's own transaction, holding every task row from
	// just after its entity's create until every task is due. pgx runs one
	// statement at a time on a transaction, hence gateMu. A failed statement
	// aborts the transaction, so the gate stops at its first error, gateErr.
	gate, err := s.db.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin the gate transaction: %v", err)
	}
	t.Cleanup(func() { _ = gate.Rollback(context.Background()) })
	var gateMu sync.Mutex
	var gateErr error
	lockTasks := func(tenantID string, id uuid.UUID) {
		gateMu.Lock()
		defer gateMu.Unlock()
		if gateErr != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		rows, err := gate.Query(ctx, `SELECT status FROM scheduled_tasks
			WHERE tenant_id = $1 AND entity_id = $2 FOR UPDATE`, tenantID, id.String())
		var statuses []string
		if err == nil {
			statuses, err = pgx.CollectRows(rows, pgx.RowTo[string])
		}
		// A task claimed before its lock stays RUNNING: its processor is held.
		if err == nil && (len(statuses) != 2 || statuses[0] != "WAITING" || statuses[1] != "WAITING") {
			err = fmt.Errorf("its tasks were %v when locked; want two WAITING, locked before they are due", statuses)
		}
		if err != nil {
			gateErr = fmt.Errorf("locking the tasks of entity %s of tenant %s: %w", id, tenantID, err)
		}
	}

	// Each entity's tasks are due delayMs after its create, so the due
	// instants spread over as long as the creates take. The creates run
	// concurrently, interleaving the tenants so that neither one's tasks are
	// all due first.
	type createJob struct {
		ct *contentionTenant
		i  int
	}
	jobs := make(chan createJob)
	var createWG sync.WaitGroup
	var createMu sync.Mutex
	var createErrs []error
	for _, ct := range tenants {
		ct.ids = make([]uuid.UUID, perTenant)
	}
	for w := 0; w < createWorkers; w++ {
		createWG.Add(1)
		go func() {
			defer createWG.Done()
			for j := range jobs {
				id, err := j.ct.c.CreateEntity(t, model, 1, mnSample)
				if err == nil {
					j.ct.ids[j.i] = id // each job owns its slot
					lockTasks(j.ct.tenant.ID, id)
				} else {
					func() {
						createMu.Lock()
						defer createMu.Unlock()
						createErrs = append(createErrs, fmt.Errorf("CreateEntity %d of tenant %s: %w", j.i, j.ct.tenant.ID, err))
					}()
				}
			}
		}()
	}
	createdFrom := time.Now()
	for i := 0; i < perTenant; i++ {
		for _, ct := range tenants {
			jobs <- createJob{ct: ct, i: i}
		}
	}
	close(jobs)
	createWG.Wait()
	createdTo := time.Now()
	if gateErr != nil {
		t.Fatalf("gate: %v", gateErr)
	}
	if len(createErrs) > 0 {
		t.Fatalf("creating the entities: %v", errors.Join(createErrs...))
	}
	t.Logf("creating the entities took %s", createdTo.Sub(createdFrom))
	// Every create had returned by createdTo, so every task is due by then
	// plus delayMs.
	time.Sleep(time.Until(createdTo.Add(delayMs*time.Millisecond + 300*time.Millisecond))) // wait for every due time, not an assertion

	// Watch the table while the tasks run. Only the watcher writes its maps;
	// they are read after wg.Wait().
	owners := map[string]bool{}            // claim owners seen
	claims := map[string]map[string]bool{} // tenant/task -> claim tokens seen
	taskEntity := map[string]string{}      // tenant/task -> entity id
	doubles := 0
	stop := make(chan struct{})
	var stopOnce sync.Once
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
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			rows, err := s.db.Query(ctx, `SELECT claim_owner::text, claim_token::text, tenant_id, entity_id, id FROM scheduled_tasks
				WHERE tenant_id = ANY($1) AND status = 'RUNNING'`, tenantIDs)
			if err != nil {
				cancel()
				continue
			}
			perEntity := map[string]int{}
			for rows.Next() {
				var o, tok, tn, e, id string
				if rows.Scan(&o, &tok, &tn, &e, &id) != nil {
					continue
				}
				owners[o] = true
				perEntity[tn+"/"+e]++
				if claims[tn+"/"+id] == nil {
					claims[tn+"/"+id] = map[string]bool{}
				}
				claims[tn+"/"+id][tok] = true
				taskEntity[tn+"/"+id] = e
			}
			rows.Close()
			cancel()
			for _, n := range perEntity {
				if n > 1 {
					doubles++
				}
			}
		}
	}()
	halt := func() {
		stopOnce.Do(func() { close(stop) })
		wg.Wait()
	}
	t.Cleanup(halt)
	if err := gate.Commit(context.Background()); err != nil {
		t.Fatalf("commit the gate transaction: %v", err)
	}
	openedAt := time.Now()

	// Every processor is held, so no claimed task leaves RUNNING until the
	// scenario releases its processor. Once every pnode holds all its run
	// slots, one snapshot therefore shows, whole, every claim made since the
	// gate opened, each while both tenants had many due tasks. The rows one
	// claim call wrote share their xmin, its transaction id, and no other
	// write reaches a RUNNING row before its processor answers.
	type heldRow struct{ owner, tenant, xmin string }
	running := func() ([]heldRow, map[string]int, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		rows, err := s.db.Query(ctx, `SELECT claim_owner::text, tenant_id, xmin::text FROM scheduled_tasks
			WHERE tenant_id = ANY($1) AND status = 'RUNNING'`, tenantIDs)
		if err != nil {
			return nil, nil, err
		}
		got, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (heldRow, error) {
			var h heldRow
			err := r.Scan(&h.owner, &h.tenant, &h.xmin)
			return h, err
		})
		if err != nil {
			return nil, nil, err
		}
		perOwner := map[string]int{}
		for _, h := range got {
			perOwner[h.owner]++
		}
		return got, perOwner, nil
	}
	// perPnode names each claim owner by its pnode (-1: no pnode's scheduler).
	perPnode := func(perOwner map[string]int) string {
		parts := make([]string, 0, len(perOwner))
		for o, n := range perOwner {
			parts = append(parts, fmt.Sprintf("pnode %d: %d", s.ownerNode(t, o), n))
		}
		slices.Sort(parts)
		return "RUNNING tasks per pnode: " + strings.Join(parts, ", ")
	}
	// A pnode claims only while its heartbeats succeed (§6.1): each pnode's
	// slots are due full Slack after the gate opened, or after the heartbeat
	// that follows its last failed one.
	fill := fixtureutil.HeartbeatGate{ReadyAt: openedAt, Slack: 15 * time.Second}
	for i := 0; i < nodes; i++ {
		inc := s.pg.Incarnation(t, i).String()
		fill.Await(t, s.logsOf([]int{i}), fmt.Sprintf("pnode %d holding all %d of its run slots", i, maxRuns), func() (bool, string) {
			_, perOwner, err := running()
			if err != nil {
				return false, err.Error()
			}
			for o, n := range perOwner {
				if n > maxRuns {
					t.Fatalf("pnode %d holds %d RUNNING tasks; its run cap is %d (%s)", s.ownerNode(t, o), n, maxRuns, perPnode(perOwner))
				}
			}
			return perOwner[inc] == maxRuns, perPnode(perOwner)
		})
	}
	// Nothing ends while the processors are held: every pnode is still full.
	held, perOwner, err := running()
	if err != nil {
		t.Fatalf("read the claims: %v", err)
	}
	if len(perOwner) != nodes {
		t.Fatalf("want exactly the %d pnodes holding claims; %s", nodes, perPnode(perOwner))
	}
	for o, n := range perOwner {
		if n > maxRuns {
			t.Fatalf("pnode %d holds %d RUNNING tasks; its run cap is %d (%s)", s.ownerNode(t, o), n, maxRuns, perPnode(perOwner))
		}
		if n < maxRuns {
			t.Fatalf("a held claim ended before its processor was released; %s", perPnode(perOwner))
		}
	}
	type claimCall struct{ owner, xmin string }
	byClaim := map[claimCall][]string{} // the tenants of each claim call's tasks
	for _, h := range held {
		k := claimCall{owner: h.owner, xmin: h.xmin}
		byClaim[k] = append(byClaim[k], h.tenant)
	}
	pairs := 0
	for k, tns := range byClaim {
		if len(tns) < 2 {
			continue
		}
		pairs++
		if tns[0] == tns[1] {
			t.Errorf("pnode %d's claim with xmin %s took two tasks of one tenant while the other tenant had due tasks: tenants do not take turns (claims: %v)", s.ownerNode(t, k.owner), k.xmin, byClaim)
		}
	}
	if pairs == 0 {
		t.Errorf("no claim of two tasks was made (claims: %v); the turn check did not run", byClaim)
	}

	// Release the held processors until every entity has settled.
	// The remaining claims stop while every pnode's heartbeats fail, as a
	// database stall makes them: the wait counts from the heartbeat after the
	// first pnode's last failed one (fixtureutil.HeartbeatGate).
	settle := fixtureutil.HeartbeatGate{ReadyAt: time.Now(), Slack: 90 * time.Second}
	all := make([]int, nodes)
	for i := range all {
		all[i] = i
	}
	settle.Await(t, s.logsOf(all), "every entity settled", func() (bool, string) {
		for _, ct := range tenants {
			for _, cl := range ct.clients {
				cl.Release(t)
			}
		}
		var active int
		if err := s.db.QueryRow(context.Background(), `SELECT count(*) FROM scheduled_tasks
			WHERE tenant_id = ANY($1) AND status IN ('WAITING', 'RUNNING')`, tenantIDs).Scan(&active); err != nil {
			return false, "read the tasks: " + err.Error()
		}
		if active > 0 {
			return false, fmt.Sprintf("%d tasks waiting or running", active)
		}
		for _, ct := range tenants {
			for _, id := range ct.ids {
				got, err := ct.c.GetEntity(t, id)
				if err != nil {
					return false, "read entity " + id.String() + ": " + err.Error()
				}
				if got.Meta.State != "A" && got.Meta.State != "B" {
					return false, "entity " + id.String() + " in state " + got.Meta.State
				}
			}
		}
		return true, ""
	})
	halt()

	// Requests each entity's processor received, over every client.
	sent := map[string]int{}
	for _, ct := range tenants {
		for _, cl := range ct.clients {
			for _, r := range cl.Received(t) {
				sent[r.EntityID]++
			}
		}
	}
	gaveBack := false
	for i := 0; i < nodes; i++ {
		if strings.Contains(s.pg.NodeLogs(i), `msg="scheduler gave back claims that had no run"`) {
			gaveBack = true
		}
	}

	if doubles != 0 {
		t.Errorf("an entity had two RUNNING tasks %d times", doubles)
	}
	claimedBy := map[int]bool{}
	for o := range owners {
		n := s.ownerNode(t, o)
		if n < 0 {
			t.Errorf("claim owner %q is no pnode's scheduler", o)
		}
		claimedBy[n] = true
	}
	for i := 0; i < nodes; i++ {
		if !claimedBy[i] {
			t.Errorf("pnode %d held no claim; the scenario needs every pnode, the isolated one included, to contend (owners: %v)", i, owners)
		}
	}
	// A second token is allowed only for a claim whose reply was lost and
	// which was given back before any run: then the processor was still sent
	// once.
	for task, toks := range claims {
		if len(toks) > 1 && !(gaveBack && sent[taskEntity[task]] == 1) {
			t.Errorf("task %s was claimed %d times; each task is claimed once, by one pnode", task, len(toks))
		}
	}

	for _, ct := range tenants {
		for _, id := range ct.ids {
			if n := sent[id.String()]; n != 1 {
				t.Errorf("entity %s: processor sent %d times; want 1", id, n)
			}
			if n := mnCountEvents(t, ct.c, id, "SCHEDULED_TRANSITION_FIRE"); n != 1 {
				t.Errorf("entity %s: %d fires; want 1", id, n)
			}
			if n := mnCountEvents(t, ct.c, id, "SCHEDULED_TRANSITION_CANCEL"); n != 1 {
				t.Errorf("entity %s: %d cancels; want 1, the sibling that did not fire", id, n)
			}
		}
	}
	var left, failed int
	if err := s.db.QueryRow(context.Background(),
		`SELECT count(*), count(*) FILTER (WHERE status = 'FAILED') FROM scheduled_tasks WHERE tenant_id = ANY($1)`, tenantIDs).Scan(&left, &failed); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if left != 0 || failed != 0 {
		t.Errorf("%d tasks left (%d FAILED); want none: the fired sibling is removed, the other cancelled", left, failed)
	}
}
