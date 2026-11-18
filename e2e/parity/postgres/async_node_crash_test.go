package postgres

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cyoda/cyoda-go/e2e/externalapi/driver"
	"github.com/cyoda/cyoda-go/e2e/parity/client"
)

// crashMatchAll is an empty AND group — it matches every entity of the model,
// so each async search's authoritative result set is the full seeded set.
const crashMatchAll = `{"type":"group","operator":"AND","conditions":[]}`

// TestAsyncNodeCrash_PeerCompletes is the real-crash multi-node proof for the
// async-search orphan-reexecute feature: a cluster node is SIGKILLed while it
// owns in-flight async-search jobs, and a SURVIVING node must reclaim each
// orphaned job (its heartbeat having aged past the stale window) and drive it
// to SUCCESSFUL — exactly once.
//
// It is deliberately NOT registered in the shared multinode scenario registry
// (multinode.Register): a hard-crash + node-down is postgres-first and
// destabilises the shared cluster fixture, so it stands alone with its own
// short-cadence fixture. It type-asserts the postgres fixture's optional
// KillNode/ConnString capabilities rather than widening MultiNodeFixture.
//
// Assertions, in order of authority:
//   - Recovery: every submitted job reaches SUCCESSFUL when polled on a
//     survivor. A job that is genuinely orphaned and never reclaimed would
//     stay RUNNING forever and time out here — that is the bug this feature
//     fixes.
//   - Single author (authoritative): each job's result count equals the seeded
//     matching count. A torn or double-authored write would not equal it.
//   - No re-execution churn: each job's persisted claim epoch is small. A job
//     created (epoch 1) and reclaimed once by a survivor (epoch 2) ends at
//     epoch 2; a higher epoch would mean repeated reclaim/re-execution. This is
//     read straight from Postgres (no production log line is added for the
//     test).
func TestAsyncNodeCrash_PeerCompletes(t *testing.T) {
	// 3 nodes: kill one, two survivors race on SKIP LOCKED to reclaim the
	// orphaned jobs. Short cadences so an orphaned job's heartbeat ages past
	// the stale window and a survivor reclaims it within the test budget.
	// STALE_AFTER must be >= 4x HEARTBEAT_INTERVAL (server startup validation).
	fix, cleanup := MustSetupMultiNodeWithEnv(t, 3, []string{
		"CYODA_SEARCH_JOB_HEARTBEAT_INTERVAL=1s",
		"CYODA_SEARCH_JOB_STALE_AFTER=4s",
		"CYODA_SEARCH_REAP_INTERVAL=1s",
	})
	defer cleanup()

	urls := fix.BaseURLs()
	if len(urls) != 3 {
		t.Fatalf("expected 3 node URLs, got %d", len(urls))
	}
	killer, ok := fix.(interface{ KillNode(int) })
	if !ok {
		t.Fatal("postgres multinode fixture does not expose KillNode")
	}
	dbSource, ok := fix.(interface{ ConnString() string })
	if !ok {
		t.Fatal("postgres multinode fixture does not expose ConnString")
	}

	tenant := fix.NewTenant(t)

	const modelName = "async-crash"
	const modelVersion = 1
	const seededEntities = 20

	// Create + lock the model and seed entities via node 0.
	d := driver.NewRemote(t, urls[0], tenant.Token)
	if err := d.CreateModelFromSample(modelName, modelVersion, `{"n": 0}`); err != nil {
		t.Fatalf("CreateModelFromSample: %v", err)
	}
	if err := d.LockModel(modelName, modelVersion); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	for i := 0; i < seededEntities; i++ {
		if _, err := d.CreateEntity(modelName, modelVersion, fmt.Sprintf(`{"n": %d}`, i)); err != nil {
			t.Fatalf("CreateEntity %d: %v", i, err)
		}
	}

	// Submit a batch of async searches to node 0. The default per-tenant
	// in-flight cap is 8, so a batch of 8 to a single tenant/node is accepted
	// without SEARCH_QUEUE_FULL (completions only free slots).
	const batch = 8
	submitClient := client.NewClient(urls[0], tenant.Token)
	jobIDs := make([]string, 0, batch)
	for i := 0; i < batch; i++ {
		jobID, err := submitClient.SubmitAsyncSearch(t, modelName, modelVersion, crashMatchAll)
		if err != nil {
			t.Fatalf("SubmitAsyncSearch %d: %v", i, err)
		}
		jobIDs = append(jobIDs, jobID)
	}

	// Crash node 0 immediately so its in-flight jobs are orphaned before it can
	// mark them SUCCESSFUL. (Jobs node 0 happened to finish before the kill are
	// still valid — they complete at epoch 1 and satisfy every assertion.)
	killer.KillNode(0)

	// Poll a SURVIVOR (node 1) until each job is SUCCESSFUL. Budget is generous:
	// staleAfter (4s) + several 1s reclaim cadences + scan time.
	survivor := client.NewClient(urls[1], tenant.Token)
	const pollBudget = 60 * time.Second
	const pollInterval = 250 * time.Millisecond

	for _, jobID := range jobIDs {
		deadline := time.Now().Add(pollBudget)
		var lastStatus string
		for {
			status, err := survivor.GetAsyncSearchStatus(t, jobID)
			if err != nil {
				t.Fatalf("GetAsyncSearchStatus(jobID=%s) on survivor: %v", jobID, err)
			}
			lastStatus = status
			if status == "SUCCESSFUL" {
				break
			}
			if status == "FAILED" || status == "CANCELLED" || status == "NOT_FOUND" {
				t.Fatalf("job %s reached terminal non-success status %q on survivor", jobID, status)
			}
			if time.Now().After(deadline) {
				t.Fatalf("job %s did not reach SUCCESSFUL within %s (last status %q) — a survivor did not complete the orphaned job",
					jobID, pollBudget, lastStatus)
			}
			time.Sleep(pollInterval)
		}

		// Authoritative single-author check: the completed result set equals
		// the full seeded set (match-all), with no torn or duplicated write.
		page, err := survivor.GetAsyncSearchResults(t, jobID)
		if err != nil {
			t.Fatalf("GetAsyncSearchResults(jobID=%s) on survivor: %v", jobID, err)
		}
		if len(page.Content) != seededEntities {
			t.Errorf("job %s result count = %d, want %d", jobID, len(page.Content), seededEntities)
		}
		if int(page.Page.TotalElements) != seededEntities {
			t.Errorf("job %s totalElements = %d, want %d", jobID, page.Page.TotalElements, seededEntities)
		}
	}

	// No re-execution churn: read each job's persisted epoch/status/result_count
	// straight from Postgres. RLS is enabled-not-forced and this handle connects
	// as the table owner, so it reads every tenant's rows without setting
	// app.current_tenant; job IDs are UUIDs, so selecting by id alone is exact.
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbSource.ConnString())
	if err != nil {
		t.Fatalf("open pgx pool for epoch assertion: %v", err)
	}
	defer pool.Close()

	for _, jobID := range jobIDs {
		var (
			status      string
			epoch       int64
			resultCount int
		)
		if err := pool.QueryRow(ctx,
			`SELECT status, epoch, result_count FROM search_jobs WHERE id = $1`, jobID,
		).Scan(&status, &epoch, &resultCount); err != nil {
			t.Fatalf("read search_jobs row for job %s: %v", jobID, err)
		}
		if status != "SUCCESSFUL" {
			t.Errorf("job %s persisted status = %q, want SUCCESSFUL", jobID, status)
		}
		// epoch 1 = created; +1 per successful claim. A single crash-and-reclaim
		// ends at 2; a job node 0 finished pre-crash stays at 1. Anything higher
		// is re-execution churn.
		if epoch > 2 {
			t.Errorf("job %s persisted epoch = %d, want <= 2 (higher indicates re-execution churn)", jobID, epoch)
		}
		if resultCount != seededEntities {
			t.Errorf("job %s persisted result_count = %d, want %d", jobID, resultCount, seededEntities)
		}
	}
}
