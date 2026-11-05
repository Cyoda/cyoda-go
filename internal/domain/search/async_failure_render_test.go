package search_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/internal/platformclock"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// scanOutageError is a storage error carrying the transient-unavailability
// marker, as a plugin returns it when it cannot get a connection.
type scanOutageError struct{}

func (scanOutageError) Error() string            { return "acquire: pool exhausted (db.internal:5432)" }
func (scanOutageError) StorageUnavailable() bool { return true }

// runFailingAsync runs one async search whose scan is shaped by iterate and
// returns the job's final status and everything logged meanwhile.
func runFailingAsync(t *testing.T, iterate func(real spi.EntityStore) func(context.Context, spi.ModelRef, spi.Filter, spi.IterateOptions) (spi.Iterator, error)) (search.SearchJobStatus, string) {
	t.Helper()
	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	base := memory.NewStoreFactory()
	t.Cleanup(func() { base.Close() })
	ctx := tenantCtx("tenant-render")
	ref := spi.ModelRef{EntityName: "renderitem", ModelVersion: "1"}
	saveModelWithFields(t, ctx, base, ref, map[string]schema.DataType{"name": schema.String})
	saveEntity(t, ctx, base, ref, "e1", []byte(`{"name":"Alice"}`))
	realStore, err := base.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	factory := &iterableFactory{StoreFactory: base, entityStore: &iterableEntityStore{EntityStore: realStore, iterateFn: iterate(realStore)}}
	searchStore, _ := base.AsyncSearchStore(context.Background())
	svc := search.NewSearchService(factory, common.NewTestUUIDGenerator(), searchStore, newTestConsistency(t, factory), platformclock.Real())

	jobID, err := svc.SubmitAsync(ctx, ref, &predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "EQUALS", Value: "Alice"}, search.SearchOptions{Limit: 10})
	if err != nil {
		t.Fatalf("SubmitAsync: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var st search.SearchJobStatus
	for time.Now().Before(deadline) {
		if st, err = svc.GetAsyncStatus(ctx, jobID); err != nil {
			t.Fatalf("GetAsyncStatus: %v", err)
		}
		if st.Status == "FAILED" || st.Status == "SUCCESSFUL" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if st.Status != "FAILED" {
		t.Fatalf("status = %q, want FAILED", st.Status)
	}
	return st, logBuf.String()
}

// A storage outage during the scan records what the direct door answers for
// it — STORAGE_UNAVAILABLE — and its cause is logged, not lost.
func TestAsyncSearch_ScanStorageOutageRecordsStorageUnavailable(t *testing.T) {
	st, logs := runFailingAsync(t, func(spi.EntityStore) func(context.Context, spi.ModelRef, spi.Filter, spi.IterateOptions) (spi.Iterator, error) {
		return func(context.Context, spi.ModelRef, spi.Filter, spi.IterateOptions) (spi.Iterator, error) {
			return nil, scanOutageError{}
		}
	})
	if !strings.HasPrefix(st.FailureReason, common.ErrCodeStorageUnavailable+":") {
		t.Fatalf("reason = %q, want the STORAGE_UNAVAILABLE code", st.FailureReason)
	}
	if strings.Contains(st.FailureReason, "db.internal") {
		t.Fatalf("reason leaks the cause: %q", st.FailureReason)
	}
	if !strings.Contains(logs, "pool exhausted") {
		t.Fatalf("the outage's cause is not logged:\n%s", logs)
	}
}

// A Close() error fails the job once: one log line for it, and the ticketed
// reason a server-side failure gets.
func TestAsyncSearch_IteratorCloseErrorLoggedOnceAndTicketed(t *testing.T) {
	st, logs := runFailingAsync(t, func(real spi.EntityStore) func(context.Context, spi.ModelRef, spi.Filter, spi.IterateOptions) (spi.Iterator, error) {
		return func(ctx context.Context, m spi.ModelRef, f spi.Filter, o spi.IterateOptions) (spi.Iterator, error) {
			it, err := real.Iterate(ctx, m, f, o)
			if err != nil {
				return nil, err
			}
			return &closeErrIterator{Iterator: it}, nil
		}
	})
	if !strings.HasPrefix(st.FailureReason, "SERVER_ERROR: internal error [ticket: ") {
		t.Fatalf("reason = %q, want the ticketed generic text", st.FailureReason)
	}
	if n := strings.Count(logs, "injected close error"); n != 1 {
		t.Fatalf("the close error was logged %d times, want once:\n%s", n, logs)
	}
}
