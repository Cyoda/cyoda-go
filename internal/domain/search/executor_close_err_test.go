package search_test

// TestAsyncSearchJob_IteratorCloseErrorFailsJob pins the final-review finding
// that the async executor's iterator drain treated Close()'s error as
// advisory (log-and-continue) while trusting Err() as authoritative. For a
// database/sql-backed iterator (e.g. plugins/sqlite's sqliteIter), Close()
// returns rows.Close()'s error and database/sql does NOT fold that into
// Rows.Err() — so Err() can stay nil while Close() reports the scan was cut
// short. Before the fix, that combination let a job land SUCCESSFUL with a
// truncated result set, indistinguishable from a complete one. closeErrIterator
// below reproduces exactly that shape: Next()/Entity() behave normally (the
// scan "completes" from the consumer's point of view), Err() returns nil, and
// only Close() reports failure.

import (
	"context"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// closeErrIterator wraps a real spi.Iterator, letting Next()/Entity()/Err()
// behave exactly as the wrapped iterator does (so Err() reports nil once the
// underlying scan finishes cleanly) but forcing Close() to return a non-nil
// error — the "driver error surfaced only at Close" shape database/sql
// iterators exhibit and Rows.Err() never sees.
type closeErrIterator struct {
	spi.Iterator
}

func (c *closeErrIterator) Close() error {
	_ = c.Iterator.Close()
	return errCloseInjected
}

var errCloseInjected = &closeInjectedError{}

type closeInjectedError struct{}

func (*closeInjectedError) Error() string { return "injected close error: driver error mid-scan" }

func TestAsyncSearchJob_IteratorCloseErrorFailsJob(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()

	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "closeerritem", ModelVersion: "1"}
	saveModelWithFields(t, ctx, base, ref, map[string]schema.DataType{"name": schema.String})
	saveEntity(t, ctx, base, ref, "e1", []byte(`{"name":"Alice"}`))

	realStore, err := base.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	iterableReal, ok := realStore.(spi.Iterable)
	if !ok {
		t.Fatal("precondition: memory EntityStore must implement spi.Iterable")
	}
	ies := &iterableEntityStore{
		EntityStore: realStore,
		iterateFn: func(ctx context.Context, model spi.ModelRef, filter spi.Filter, opts spi.IterateOptions) (spi.Iterator, error) {
			it, iterErr := iterableReal.Iterate(ctx, model, filter, opts)
			if iterErr != nil {
				return nil, iterErr
			}
			return &closeErrIterator{Iterator: it}, nil
		},
	}
	factory := &iterableFactory{StoreFactory: base, entityStore: ies}

	uuids := common.NewTestUUIDGenerator()
	searchStore, _ := base.AsyncSearchStore(context.Background())
	svc := search.NewSearchService(factory, uuids, searchStore)

	cond := &predicate.SimpleCondition{
		JsonPath:     "$.name",
		OperatorType: "EQUALS",
		Value:        "Alice",
	}
	jobID, err := svc.SubmitAsync(ctx, ref, cond, search.SearchOptions{})
	if err != nil {
		t.Fatalf("SubmitAsync: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	var status search.SearchJobStatus
	for time.Now().Before(deadline) {
		status, err = svc.GetAsyncStatus(ctx, jobID)
		if err != nil {
			t.Fatalf("GetAsyncStatus: %v", err)
		}
		if status.Status == "FAILED" || status.Status == "SUCCESSFUL" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if status.Status != "FAILED" {
		t.Fatalf("job status = %q, want FAILED — a Close() error (even with Err()==nil) must fail the job, not report a truncated result set as SUCCESSFUL", status.Status)
	}
}
