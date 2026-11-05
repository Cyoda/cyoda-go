package search_test

import (
	"context"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/internal/platformclock"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// settledJob writes a job straight into the async search store and settles it
// with the given terminal status and recorded message, so a status read is
// tested against a known record rather than against an executor's timing.
func settledJob(t *testing.T, ctx context.Context, store spi.AsyncSearchStore, jobID, status, errMsg string) {
	t.Helper()
	tid := spi.GetUserContext(ctx).Tenant.ID
	if err := store.CreateJob(ctx, &spi.SearchJob{
		ID:         jobID,
		TenantID:   tid,
		Status:     "RUNNING",
		ModelRef:   spi.ModelRef{EntityName: "person", ModelVersion: "1"},
		CreateTime: time.Now(),
	}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if status == "RUNNING" {
		return
	}
	if err := store.UpdateJobStatus(ctx, jobID, 1, status, 0, errMsg, time.Now(), 1); err != nil {
		t.Fatalf("UpdateJobStatus: %v", err)
	}
}

// TestAsyncStatus_FailureReason pins that a status read serves the reason a
// failed job recorded, and serves no reason for any other status — on both the
// HTTP-facing status (SearchJobStatus) and the transport summary
// (SnapshotStatus) the gRPC door renders.
func TestAsyncStatus_FailureReason(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	store, _ := factory.AsyncSearchStore(context.Background())
	svc := search.NewSearchService(factory, common.NewTestUUIDGenerator(), store, newTestConsistency(t, factory), platformclock.Real())
	ctx := tenantCtx("tenant-failure-reason")

	const reason = "INVALID_FIELD_PATH: condition or sort references an invalid field path"
	cases := []struct {
		name, jobID, status, recorded, want string
	}{
		{"failed serves its reason", "11111111-1111-1111-1111-111111111111", "FAILED", reason, reason},
		{"successful serves none", "22222222-2222-2222-2222-222222222222", "SUCCESSFUL", "", ""},
		// Only a FAILED job has a reason. A message on any other record is
		// not a failure reason and is not served as one.
		{"cancelled serves none even with a recorded message", "33333333-3333-3333-3333-333333333333", "CANCELLED", "stray", ""},
		{"running serves none", "44444444-4444-4444-4444-444444444444", "RUNNING", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			settledJob(t, ctx, store, tc.jobID, tc.status, tc.recorded)

			st, err := svc.GetAsyncStatus(ctx, tc.jobID)
			if err != nil {
				t.Fatalf("GetAsyncStatus: %v", err)
			}
			if st.Status != tc.status {
				t.Fatalf("status = %q, want %q", st.Status, tc.status)
			}
			if st.FailureReason != tc.want {
				t.Errorf("SearchJobStatus.FailureReason = %q, want %q", st.FailureReason, tc.want)
			}

			snap, err := svc.GetAsyncSearchStatus(ctx, tc.jobID)
			if err != nil {
				t.Fatalf("GetAsyncSearchStatus: %v", err)
			}
			if snap.FailureReason != tc.want {
				t.Errorf("SnapshotStatus.FailureReason = %q, want %q", snap.FailureReason, tc.want)
			}
		})
	}
}
