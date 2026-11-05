package grpc

import (
	"context"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/internal/platformclock"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// TestRPC_SnapshotGetStatus_FailureReason pins the gRPC half of the async
// status's failure reason: a SnapshotGetStatusRequest for a FAILED job answers
// a success envelope whose status carries the reason the job recorded, and one
// for a SUCCESSFUL job carries none.
func TestRPC_SnapshotGetStatus_FailureReason(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	store, _ := factory.AsyncSearchStore(context.Background())
	svc := &CloudEventsServiceImpl{searchService: search.NewSearchService(
		factory, common.NewDefaultUUIDGenerator(), store, newTestConsistency(t, factory), platformclock.Real())}
	ctx := grpcTenantCtx()

	const reason = "search failed unexpectedly"
	const failedID = "aaaaaaaa-0000-0000-0000-000000000001"
	const okID = "aaaaaaaa-0000-0000-0000-000000000002"
	for id, terminal := range map[string]struct{ status, msg string }{
		failedID: {"FAILED", reason},
		okID:     {"SUCCESSFUL", ""},
	} {
		if err := store.CreateJob(ctx, &spi.SearchJob{
			ID: id, TenantID: "test-tenant", Status: "RUNNING",
			ModelRef:   spi.ModelRef{EntityName: "person", ModelVersion: "1"},
			CreateTime: time.Now(),
		}); err != nil {
			t.Fatalf("CreateJob: %v", err)
		}
		if err := store.UpdateJobStatus(ctx, id, 1, terminal.status, 0, terminal.msg, time.Now(), 1); err != nil {
			t.Fatalf("UpdateJobStatus: %v", err)
		}
	}

	status := func(t *testing.T, id string) events.SearchSnapshotStatusJson {
		t.Helper()
		resp, err := svc.EntitySearch(ctx, makeCE(SnapshotGetStatusRequest, map[string]any{
			"id": "status-" + id, "snapshotId": id,
		}))
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		var typed events.EntitySnapshotSearchResponseJson
		validateResponse(t, resp, &typed)
		if !typed.Success {
			t.Fatalf("status: success=false; error: %+v", typed.Error)
		}
		return typed.Status
	}

	failed := status(t, failedID)
	if failed.Status != events.SearchSnapshotStatusJsonStatusFAILED {
		t.Fatalf("status = %s, want FAILED", failed.Status)
	}
	if failed.FailureReason == nil || *failed.FailureReason != reason {
		t.Errorf("failureReason = %v, want %q", failed.FailureReason, reason)
	}

	ok := status(t, okID)
	if ok.Status != events.SearchSnapshotStatusJsonStatusSUCCESSFUL {
		t.Fatalf("status = %s, want SUCCESSFUL", ok.Status)
	}
	if ok.FailureReason != nil {
		t.Errorf("SUCCESSFUL status carries failureReason %q; want it absent", *ok.FailureReason)
	}
}
