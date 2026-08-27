package search_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// TestAsyncSearchJob_StoreSentinelIsClassified pins that an async job
// classifies a store's cross-backend sentinel exactly as the synchronous path
// does.
//
// The job record is the only report an async caller ever gets. The sync path
// runs every store error through search.ClassifyStoreQueryError, so a
// path a plugin refuses comes back as 400 INVALID_FIELD_PATH; the async
// executor assigned the iterate error straight to the producer error, so
// jobFailureMessage found no AppError and wrote the generic
// "search failed unexpectedly" — the same request, the same cause, reported as
// a server fault on one door and a client error on the other. Reaching this at
// all means the boundary grammar and a plugin disagree, which is a defect on
// its own; what the caller is told about their request must not depend on
// which door they used.
func TestAsyncSearchJob_StoreSentinelIsClassified(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()

	ctx := tenantCtx("tenant-async-classify")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}
	saveModelWithFields(t, ctx, base, ref, map[string]schema.DataType{"name": schema.String})
	saveEntity(t, ctx, base, ref, "e1", []byte(`{"name":"Alice"}`))

	realStore, _ := base.EntityStore(ctx)
	ies := &iterableEntityStore{
		EntityStore: realStore,
		iterateFn: func(context.Context, spi.ModelRef, spi.Filter, spi.IterateOptions) (spi.Iterator, error) {
			return nil, fmt.Errorf("%w: disallowed character", spi.ErrInvalidFilterPath)
		},
	}
	factory := &iterableFactory{StoreFactory: base, entityStore: ies}

	searchStore, _ := base.AsyncSearchStore(context.Background())
	svc := search.NewSearchService(factory, common.NewTestUUIDGenerator(), searchStore)

	jobID, err := svc.SubmitAsync(ctx, ref, &predicate.SimpleCondition{
		JsonPath: "$.name", OperatorType: "EQUALS", Value: "Alice",
	}, search.SearchOptions{})
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
		t.Fatalf("status = %q, want FAILED", status.Status)
	}

	job, err := searchStore.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if !strings.Contains(job.Error, "invalid field path") {
		t.Errorf("job error = %q; a refused path must be reported as the client error it is, the same as the synchronous door reports it", job.Error)
	}
	// Gate 3: the classified message is the client-safe one, not the
	// plugin's own text.
	if strings.Contains(job.Error, "disallowed character") {
		t.Errorf("job error leaks the store's internal detail: %q", job.Error)
	}
}
