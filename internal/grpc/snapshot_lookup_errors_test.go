package grpc

// A snapshot read, cancel or results page for a snapshot the caller does not
// have, or results of one still running, is the caller's error on gRPC as on
// HTTP (404 SEARCH_JOB_NOT_FOUND, 400 BAD_REQUEST): CLIENT_ERROR with the
// same code, not a ticketed SERVER_ERROR.

import (
	"context"
	"strings"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/internal/platformclock"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

func TestSnapshotLookupErrors_AreClientErrors(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	store, _ := factory.AsyncSearchStore(context.Background())
	svc := &CloudEventsServiceImpl{searchService: search.NewSearchService(
		factory, common.NewDefaultUUIDGenerator(), store, newTestConsistency(t, factory), platformclock.Real())}
	ctx := grpcTenantCtx()

	const unknownID = "aaaaaaaa-0000-0000-0000-00000000dead"
	const runningID = "aaaaaaaa-0000-0000-0000-00000000f00d"
	if err := store.CreateJob(ctx, &spi.SearchJob{
		ID: runningID, TenantID: "test-tenant", Status: "RUNNING",
		ModelRef:   spi.ModelRef{EntityName: "person", ModelVersion: "1"},
		CreateTime: time.Now(),
	}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	wantClientError := func(t *testing.T, code, message, prefix string) {
		t.Helper()
		if code != "CLIENT_ERROR" || !strings.HasPrefix(message, prefix) {
			t.Fatalf("error = %s %q, want CLIENT_ERROR with prefix %q", code, message, prefix)
		}
	}
	unary := func(t *testing.T, ceType, id string) (string, string) {
		t.Helper()
		resp, err := svc.EntitySearch(ctx, makeCE(ceType, map[string]any{"id": "req", "snapshotId": id}))
		if err != nil {
			t.Fatalf("EntitySearch: %v", err)
		}
		var typed events.EntitySnapshotSearchResponseJson
		validateResponse(t, resp, &typed)
		if typed.Success || typed.Error == nil {
			t.Fatalf("answer = success %v, error %+v; want refused", typed.Success, typed.Error)
		}
		return typed.Error.Code, typed.Error.Message
	}
	results := func(t *testing.T, id string) (string, string) {
		t.Helper()
		stream := &mockEntityStream{ctx: ctx}
		if err := svc.EntitySearchCollection(makeCE(SnapshotGetRequest, map[string]any{
			"id": "req", "snapshotId": id, "pageNumber": 0, "pageSize": 10,
		}), stream); err != nil {
			t.Fatalf("EntitySearchCollection: %v", err)
		}
		if len(stream.sent) != 1 {
			t.Fatalf("stream carried %d envelopes, want one refusal", len(stream.sent))
		}
		var typed events.EntityResponseJson
		validateResponse(t, stream.sent[0], &typed)
		if typed.Success || typed.Error == nil {
			t.Fatalf("answer = success %v, error %+v; want refused", typed.Success, typed.Error)
		}
		return typed.Error.Code, typed.Error.Message
	}

	t.Run("status of an unknown snapshot", func(t *testing.T) {
		code, msg := unary(t, SnapshotGetStatusRequest, unknownID)
		wantClientError(t, code, msg, "SEARCH_JOB_NOT_FOUND: ")
	})
	t.Run("cancel of an unknown snapshot", func(t *testing.T) {
		code, msg := unary(t, SnapshotCancelRequest, unknownID)
		wantClientError(t, code, msg, "SEARCH_JOB_NOT_FOUND: ")
	})
	t.Run("results of an unknown snapshot", func(t *testing.T) {
		code, msg := results(t, unknownID)
		wantClientError(t, code, msg, "SEARCH_JOB_NOT_FOUND: ")
	})
	t.Run("results of a running snapshot", func(t *testing.T) {
		code, msg := results(t, runningID)
		wantClientError(t, code, msg, "BAD_REQUEST: ")
	})
}
