package grpc

// search_storage_outage_test.go — a storage outage must answer the same on both
// doors.
//
// The async-search service methods (GetAsyncStatus / GetAsyncResults /
// CancelAsync / DirectSearch) return raw errors that carry the storage layer's
// StorageUnavailable marker rather than a pre-classified AppError. The HTTP door
// runs them through common.Internal, which recognises the marker and answers a
// retryable 503 STORAGE_UNAVAILABLE. If this door only inspects *common.AppError,
// the identical error falls through to the raw-error path and the same outage,
// on the same service method, becomes a non-retryable SERVER_ERROR — telling one
// client to back off and retry and the other that its request is hopeless.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// storageOutageError stands in for what a storage plugin returns when its pool
// could not serve the statement — carrying the marker and the operator-only
// detail (host, user, database) that must never reach a caller.
type storageOutageError struct{ detail string }

func (e *storageOutageError) Error() string          { return e.detail }
func (*storageOutageError) StorageUnavailable() bool { return true }

const storageOutageDetail = "GetJob: acquire: host=db.internal user=cyoda database=cyoda: context deadline exceeded"

// outageSearchStore fails every job lookup with the marked error. Embedding the
// real store keeps the rest of the interface honest.
type outageSearchStore struct {
	spi.AsyncSearchStore
}

func (s *outageSearchStore) GetJob(_ context.Context, jobID string) (*spi.SearchJob, error) {
	return nil, fmt.Errorf("failed to look up search job %s: %w", jobID, &storageOutageError{detail: storageOutageDetail})
}

// newOutageSearchEnv wires both doors to one SearchService whose store is out.
func newOutageSearchEnv(t *testing.T) (*CloudEventsServiceImpl, *search.Handler, context.Context) {
	t.Helper()
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })

	base, err := factory.AsyncSearchStore(context.Background())
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	svcSearch := search.NewSearchService(factory, common.NewDefaultUUIDGenerator(), &outageSearchStore{AsyncSearchStore: base})

	ctx := spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID: "test-user",
		Kind:   spi.PrincipalUser,
		Tenant: spi.Tenant{ID: "test-tenant", Name: "Test Tenant"},
		Roles:  []string{"ADMIN"},
	})
	return &CloudEventsServiceImpl{searchService: svcSearch}, search.NewHandler(svcSearch), ctx
}

// TestSearchStorageOutage_BothDoorsAgree drives one job-status lookup through
// gRPC and through HTTP against the same failing store.
func TestSearchStorageOutage_BothDoorsAgree(t *testing.T) {
	svc, httpHandler, ctx := newOutageSearchEnv(t)
	jobID := uuid.New()

	// --- gRPC door -------------------------------------------------------
	ce, err := svc.EntitySearch(ctx, makeCE(SnapshotGetStatusRequest, map[string]any{
		"id":         "outage-status",
		"snapshotId": jobID.String(),
	}))
	if err != nil {
		t.Fatalf("unexpected transport-level error (failures belong in the envelope): %v", err)
	}
	var env events.EntitySnapshotSearchResponseJson
	validateResponse(t, ce, &env)
	if env.Success {
		t.Fatal("expected success=false for a storage outage")
	}
	if env.Error == nil {
		t.Fatal("expected an error block")
	}
	if env.Error.Code != "CLIENT_ERROR" {
		t.Errorf("gRPC Error.Code = %q, want CLIENT_ERROR — a retryable 503 is an operational classification, not a server fault", env.Error.Code)
	}
	if !strings.HasPrefix(env.Error.Message, common.ErrCodeStorageUnavailable+":") {
		t.Errorf("gRPC Error.Message = %q, want the %s domain code", env.Error.Message, common.ErrCodeStorageUnavailable)
	}
	if env.Error.Retryable == nil || !*env.Error.Retryable {
		t.Errorf("gRPC Error.Retryable = %v, want true — the outage clears on its own", env.Error.Retryable)
	}
	// Gate 3: the connection detail is the operator's, never the caller's.
	if strings.Contains(env.Error.Message, "db.internal") || strings.Contains(env.Error.Message, storageOutageDetail) {
		t.Errorf("gRPC envelope leaks the storage cause: %q", env.Error.Message)
	}

	// --- HTTP door -------------------------------------------------------
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/search/snapshot/"+jobID.String()+"/status", nil).WithContext(ctx)
	httpHandler.GetAsyncSearchStatus(rec, req, jobID)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("HTTP status = %d, want 503; body: %s", rec.Code, rec.Body.String())
	}
	var pd common.ProblemDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &pd); err != nil {
		t.Fatalf("decode ProblemDetail: %v; body: %s", err, rec.Body.String())
	}
	if code, _ := pd.Props["errorCode"].(string); code != common.ErrCodeStorageUnavailable {
		t.Errorf("HTTP errorCode = %q, want %s", code, common.ErrCodeStorageUnavailable)
	}
	if retryable, _ := pd.Props["retryable"].(bool); !retryable {
		t.Errorf("HTTP retryable = %v, want true", pd.Props["retryable"])
	}
	if strings.Contains(rec.Body.String(), "db.internal") {
		t.Errorf("HTTP body leaks the storage cause: %s", rec.Body.String())
	}
}

// TestBuildErrorFields_RawStorageUnavailableMarker pins the classifier itself:
// the marker must be recognised on a raw error, not only on an error already
// wrapped as an AppError. Every one of this door's sixteen envelopes goes
// through this one function, so the check belongs here and nowhere else.
func TestBuildErrorFields_RawStorageUnavailableMarker(t *testing.T) {
	raw := fmt.Errorf("failed to get result IDs: %w", &storageOutageError{detail: storageOutageDetail})

	code, message, retryable := buildErrorFields(raw)
	if code != "CLIENT_ERROR" {
		t.Errorf("code = %q, want CLIENT_ERROR", code)
	}
	if !strings.HasPrefix(message, common.ErrCodeStorageUnavailable+":") {
		t.Errorf("message = %q, want the %s domain code", message, common.ErrCodeStorageUnavailable)
	}
	if retryable == nil || !*retryable {
		t.Errorf("retryable = %v, want true", retryable)
	}
	if strings.Contains(message, storageOutageDetail) {
		t.Errorf("message leaks the storage cause: %q", message)
	}

	// And the HTTP door's classifier, on the identical error, must agree.
	httpErr := common.Internal("job lookup failed", raw)
	if httpErr.Status != http.StatusServiceUnavailable || httpErr.Code != common.ErrCodeStorageUnavailable || !httpErr.Retryable {
		t.Errorf("HTTP classification = %d/%s/retryable=%v, want 503/%s/true",
			httpErr.Status, httpErr.Code, httpErr.Retryable, common.ErrCodeStorageUnavailable)
	}
	if !strings.HasPrefix(message, httpErr.Code+":") {
		t.Errorf("the two doors disagree on the domain code: gRPC %q vs HTTP %q", message, httpErr.Code)
	}

	// An unmarked raw error keeps the ticketed SERVER_ERROR path.
	plainCode, _, plainRetryable := buildErrorFields(errors.New("something unclassified"))
	if plainCode != "SERVER_ERROR" || plainRetryable != nil {
		t.Errorf("unmarked raw error = %q/retryable=%v, want SERVER_ERROR/nil", plainCode, plainRetryable)
	}
}
