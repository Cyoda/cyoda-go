package memory_test

import (
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// newCopyTestJob seeds one RUNNING job whose Condition and SearchOpts are
// non-empty, and returns the store, its context, and the caller's own copy of
// the job it submitted.
func newCopyTestJob(t *testing.T, tenant string) (spi.AsyncSearchStore, *spi.SearchJob, func() *spi.SearchJob) {
	t.Helper()
	f := memory.NewStoreFactory()
	t.Cleanup(func() { _ = f.Close() })

	ctx := txIndexCtx(tenant)
	store, err := f.AsyncSearchStore(ctx)
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	submitted := &spi.SearchJob{
		ID:         "job-copy",
		Status:     "RUNNING",
		ModelRef:   spi.ModelRef{EntityName: "item", ModelVersion: "1"},
		CreateTime: time.Now(),
		Condition:  []byte(`{"op":"EQUALS"}`),
		SearchOpts: []byte(`{"limit":10}`),
	}
	if err := store.CreateJob(ctx, submitted); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	get := func() *spi.SearchJob {
		t.Helper()
		got, err := store.GetJob(ctx, "job-copy")
		if err != nil {
			t.Fatalf("GetJob: %v", err)
		}
		return got
	}
	return store, submitted, get
}

// TestSearchJob_ReturnedValuesAreDeepCopies: the SQL backends rebuild every
// SearchJob from freshly-scanned rows, so a caller can never reach store state
// through a returned value. The memory backend handed out shallow struct
// copies, which share the Condition/SearchOpts backing arrays and the
// HeartbeatTime pointer with the live entry — a caller mutating a returned job
// would silently corrupt the store.
func TestSearchJob_ReturnedValuesAreDeepCopies(t *testing.T) {
	store, _, get := newCopyTestJob(t, "tenant-copy-claim")

	claimed, err := store.ClaimStale(txIndexCtx("tenant-copy-claim"), -time.Hour, 10)
	if err != nil {
		t.Fatalf("ClaimStale: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("ClaimStale: want 1 job, got %d", len(claimed))
	}
	job := claimed[0]
	if job.HeartbeatTime == nil {
		t.Fatal("ClaimStale: want HeartbeatTime stamped")
	}

	job.Condition[0] = 'X'
	job.SearchOpts[0] = 'X'
	*job.HeartbeatTime = time.Unix(0, 0)

	stored := get()
	if string(stored.Condition) != `{"op":"EQUALS"}` {
		t.Errorf("mutating a ClaimStale result corrupted the stored Condition: %s", stored.Condition)
	}
	if string(stored.SearchOpts) != `{"limit":10}` {
		t.Errorf("mutating a ClaimStale result corrupted the stored SearchOpts: %s", stored.SearchOpts)
	}
	if stored.HeartbeatTime != nil && stored.HeartbeatTime.Equal(time.Unix(0, 0)) {
		t.Error("mutating a ClaimStale result corrupted the stored HeartbeatTime")
	}
}

// TestSearchJob_GetJobIsADeepCopy covers the sibling read path: GetJob had the
// same shallow copy.
func TestSearchJob_GetJobIsADeepCopy(t *testing.T) {
	_, _, get := newCopyTestJob(t, "tenant-copy-get")

	first := get()
	first.Condition[0] = 'X'
	first.SearchOpts[0] = 'X'

	second := get()
	if string(second.Condition) != `{"op":"EQUALS"}` {
		t.Errorf("mutating a GetJob result corrupted the stored Condition: %s", second.Condition)
	}
	if string(second.SearchOpts) != `{"limit":10}` {
		t.Errorf("mutating a GetJob result corrupted the stored SearchOpts: %s", second.SearchOpts)
	}
}

// TestSearchJob_CreateJobDoesNotAliasCaller covers the write path: the store
// kept the caller's own slices, so the submitter retained a live handle on
// persisted state.
func TestSearchJob_CreateJobDoesNotAliasCaller(t *testing.T) {
	_, submitted, get := newCopyTestJob(t, "tenant-copy-create")

	submitted.Condition[0] = 'X'
	submitted.SearchOpts[0] = 'X'

	stored := get()
	if string(stored.Condition) != `{"op":"EQUALS"}` {
		t.Errorf("mutating the submitted job corrupted the stored Condition: %s", stored.Condition)
	}
	if string(stored.SearchOpts) != `{"limit":10}` {
		t.Errorf("mutating the submitted job corrupted the stored SearchOpts: %s", stored.SearchOpts)
	}
}
