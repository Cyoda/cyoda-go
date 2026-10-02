package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"

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
// itself, and under claims it is one more claimer. Every processor takes
// 200ms; tenant 0's are unsafe, tenant 1's idempotent. Each pnode runs at most
// two tasks, two per tenant.
//
// The pnodes are frozen (SIGSTOP) from just before the first task is due
// until just after the last one is, and resumed together, so every claim loop
// starts against the whole due set at once.
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
//   - tenants take turns within a claim call: a pnode's first claim of two
//     tasks, made while both tenants have many due tasks, holds one task of
//     each, and at least one pnode's first claim is of two tasks.
func TestSchedulerMN_ClaimContention(t *testing.T) {
	t.Parallel()
	const (
		nodes     = 3
		isolated  = 2
		perTenant = 45
		delayMs   = 10000
		// sleepMs is every processor's run time: no run ends sooner than
		// sleepMs after its claim committed.
		sleepMs = 200
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
	}}, "CYODA_SCHEDULER_MAX_RUNS=2", "CYODA_SCHEDULER_MAX_RUNS_PER_TENANT=2")

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
	slow := func(tag string, idempotent bool) map[string]any {
		p := mnProc("slow-configurable", "SYNC", tag, idempotent, 10000)
		p["config"].(map[string]any)["context"] = fmt.Sprintf(`{"sleep_ms":%d}`, sleepMs)
		return p
	}
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
		// work to a client it does not know of.
		for i := 0; i < nodes; i++ {
			ct.clients = append(ct.clients, s.startClient(t, i, ct.tenant, tag, parity.ComputeBehaviourCatalog))
		}
		wf := mnWorkflow("mn-contention-wf", map[string]any{
			"Open": map[string]any{"transitions": []any{
				map[string]any{"name": "ToA", "next": "A", "manual": false, "schedule": map[string]any{"delayMs": delayMs}, "processors": []any{slow(tag, idempotent)}},
				map[string]any{"name": "ToB", "next": "B", "manual": false, "schedule": map[string]any{"delayMs": delayMs}, "processors": []any{slow(tag, idempotent)}},
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

	// Each entity's tasks are due delayMs after its create, so the due
	// instants spread over exactly as long as the creates take, and the
	// freeze below must cover that spread. The creates run concurrently:
	// done one after another, the spread is the sum of every request's
	// latency, which under parallel load (other suites starting containers
	// and building servers) grows past what the freeze can cover.
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
	createdFrom := time.Now()
	for w := 0; w < createWorkers; w++ {
		createWG.Add(1)
		go func() {
			defer createWG.Done()
			for j := range jobs {
				id, err := j.ct.c.CreateEntity(t, model, 1, mnSample)
				if err != nil {
					func() {
						createMu.Lock()
						defer createMu.Unlock()
						createErrs = append(createErrs, fmt.Errorf("CreateEntity %d of tenant %s: %w", j.i, j.ct.tenant.ID, err))
					}()
					continue
				}
				j.ct.ids[j.i] = id // each job owns its slot
			}
		}()
	}
	// Interleave the tenants so that neither one's tasks are all due first.
	for i := 0; i < perTenant; i++ {
		for _, ct := range tenants {
			jobs <- createJob{ct: ct, i: i}
		}
	}
	close(jobs)
	createWG.Wait()
	createdTo := time.Now()
	if len(createErrs) > 0 {
		t.Fatalf("creating the entities: %v", errors.Join(createErrs...))
	}

	// Freeze every pnode only around the due instants: from 500ms before the
	// first task is due until 300ms after the last one is, then resume them
	// together. A frozen pnode neither heartbeats nor ends its idle scheduler
	// transactions, so the freeze stays well inside the watchdog window
	// (13s under the tuned settings) and the scheduler pool's
	// idle_in_transaction_session_timeout (10s).
	const delay = delayMs * time.Millisecond
	stopAt := createdFrom.Add(delay - 500*time.Millisecond)
	resumeAt := createdTo.Add(delay + 300*time.Millisecond)
	t.Logf("creating the entities took %s, so the pnodes are frozen for %s", createdTo.Sub(createdFrom), resumeAt.Sub(stopAt))
	if freeze := resumeAt.Sub(stopAt); freeze > 8*time.Second {
		t.Fatalf("creating the entities took %s, so the pnodes would be frozen for %s; the freeze must stay under 8s", createdTo.Sub(createdFrom), freeze)
	}
	t.Cleanup(func() {
		for i := 0; i < nodes; i++ {
			_ = s.pg.SignalNode(i, syscall.SIGCONT)
		}
	})
	time.Sleep(time.Until(stopAt)) // wait for the freeze instant, not an assertion
	for i := 0; i < nodes; i++ {
		if err := s.pg.SignalNode(i, syscall.SIGSTOP); err != nil {
			t.Fatalf("SIGSTOP pnode %d: %v", i, err)
		}
	}
	time.Sleep(time.Until(resumeAt)) // wait for every due time, not an assertion

	// Watch the table while the tasks run. Only the watcher writes its maps;
	// they are read after wg.Wait().
	//
	// A pnode's first claim is read from the first snapshot that shows any
	// of its claims. The previous complete snapshot showed none, so every
	// claim this one shows committed after that previous snapshot was taken.
	// No run ends sooner than sleepMs after its claim committed, so when this
	// snapshot ended less than sleepMs after the previous one started, every
	// task of the pnode's first claim is still RUNNING in it. The window is
	// measured between the watcher's own snapshots, not from the resume: how
	// soon after the resume a pnode first claims depends on the load the test
	// runs under.
	//
	// The snapshot can also show a second claim: the three pnodes rank the
	// same due tasks, a rival's row locks make a claim take fewer tasks than
	// it has free slots, and the pnode claims the rest on its next scan, 50ms
	// later. The rows one claim call wrote share their xmin, its transaction
	// id, and a later claim's transaction id is higher; no other write
	// reaches a RUNNING row before its processor answers. So the first claim
	// is the rows with the lowest xmin.
	tenantIDs := []string{tenants[0].tenant.ID, tenants[1].tenant.ID}
	owners := map[string]int{}             // claim_owner -> RUNNING rows seen
	firstClaim := map[string][]string{}    // claim_owner -> tenants of its first claim
	firstSeen := map[string]string{}       // claim_owner -> its first snapshot's rows, tenant@xmin
	claims := map[string]map[string]bool{} // tenant/task -> claim tokens seen
	taskEntity := map[string]string{}      // tenant/task -> entity id
	doubles := 0
	stop := make(chan struct{})
	var stopOnce sync.Once
	var wg sync.WaitGroup
	wg.Add(1)
	// The pnodes are frozen and nothing is due before the freeze, so no
	// task of these tenants is RUNNING yet: this instant stands in for a
	// complete snapshot showing no claims.
	prevStart := time.Now()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
			startedAt := time.Now()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			rows, err := s.db.Query(ctx, `SELECT claim_owner::text, claim_token::text, tenant_id, entity_id, id, xmin::text FROM scheduled_tasks
				WHERE tenant_id = ANY($1) AND status = 'RUNNING'`, tenantIDs)
			if err != nil {
				cancel()
				continue
			}
			complete := true
			perEntity := map[string]int{}
			type claimedRow struct {
				tenant string
				xmin   uint64
			}
			perOwner := map[string][]claimedRow{}
			for rows.Next() {
				var o, tok, tn, e, id, xminText string
				if rows.Scan(&o, &tok, &tn, &e, &id, &xminText) != nil {
					complete = false
					continue
				}
				xmin, perr := strconv.ParseUint(xminText, 10, 64)
				if perr != nil {
					complete = false
					continue
				}
				perEntity[tn+"/"+e]++
				if claims[tn+"/"+id] == nil {
					claims[tn+"/"+id] = map[string]bool{}
				}
				claims[tn+"/"+id][tok] = true
				taskEntity[tn+"/"+id] = e
				perOwner[o] = append(perOwner[o], claimedRow{tenant: tn, xmin: xmin})
			}
			rows.Close()
			if rows.Err() != nil {
				complete = false
			}
			cancel()
			endedAt := time.Now()
			for _, n := range perEntity {
				if n > 1 {
					doubles++
				}
			}
			// An incomplete snapshot may miss rows: it can neither show a
			// first claim whole nor stand as the snapshot before one.
			inWindow := complete && endedAt.Sub(prevStart) < sleepMs*time.Millisecond
			for o, rs := range perOwner {
				if _, seen := owners[o]; !seen && inWindow {
					first := rs[0].xmin
					shown := make([]string, 0, len(rs))
					for _, r := range rs {
						first = min(first, r.xmin)
						shown = append(shown, fmt.Sprintf("%s@%d", r.tenant, r.xmin))
					}
					for _, r := range rs {
						if r.xmin == first {
							firstClaim[o] = append(firstClaim[o], r.tenant)
						}
					}
					firstSeen[o] = strings.Join(shown, " ")
				}
				owners[o] += len(rs)
			}
			if complete {
				prevStart = startedAt
			}
		}
	}()
	halt := func() {
		stopOnce.Do(func() { close(stop) })
		wg.Wait()
	}
	t.Cleanup(halt)
	for i := 0; i < nodes; i++ {
		if err := s.pg.SignalNode(i, syscall.SIGCONT); err != nil {
			t.Fatalf("SIGCONT pnode %d: %v", i, err)
		}
	}

	mnAwait(t, 90*time.Second, "every entity settled", func() bool {
		for _, ct := range tenants {
			for _, id := range ct.ids {
				got, err := ct.c.GetEntity(t, id)
				if err != nil || (got.Meta.State != "A" && got.Meta.State != "B") {
					return false
				}
			}
		}
		return true
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
	// A second token is allowed only for a claim whose reply was lost and
	// which was given back before any run: then the processor was still sent
	// once.
	for task, toks := range claims {
		if len(toks) > 1 && !(gaveBack && sent[taskEntity[task]] == 1) {
			t.Errorf("task %s was claimed %d times; each task is claimed once, by one pnode", task, len(toks))
		}
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
	pairs := 0
	for o, tns := range firstClaim {
		if len(tns) < 2 {
			continue
		}
		pairs++
		if tns[0] == tns[1] {
			t.Errorf("pnode %d's first claim took two tasks of one tenant while the other tenant had due tasks: tenants do not take turns (its first snapshot, tenant@xmin: %s)", s.ownerNode(t, o), firstSeen[o])
		}
	}
	if pairs == 0 {
		t.Errorf("no pnode's first claim of two tasks was seen (first claims: %v); the turn check did not run", firstClaim)
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
