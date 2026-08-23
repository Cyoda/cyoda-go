package grpc

// search_queue_full_test.go pins the gRPC door's half of the E1.4 error
// mapping: handleSnapshotSearchRequest remaps a bare search.ErrQueueFull to
// search.QueueFullError() before handing off to snapshotSearchError, so the
// async-search worker pool's backpressure reaches the CloudEvent envelope as
// a retryable SEARCH_QUEUE_FULL rather than falling through buildErrorFields'
// raw-error branch to an unclassified, non-retryable SERVER_ERROR.
//
// This task (E1) adds the pool and the mapping but does not yet wire
// SubmitAsync's execution through it — that lands in a later engine task —
// so there is no code path that makes a real SearchService return
// ErrQueueFull from SubmitAsyncSearch today. This test pins the classifier
// the handler already delegates to (buildErrorFields, shared with every
// other envelope on this door) against the exact value the handler now
// produces, matching TestBuildErrorFields_RawStorageUnavailableMarker's
// pattern for the sibling STORAGE_UNAVAILABLE case.

import (
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
)

func TestBuildErrorFields_SearchQueueFull(t *testing.T) {
	code, message, retryable := buildErrorFields(search.QueueFullError())

	if code != "CLIENT_ERROR" {
		t.Errorf("code = %q, want CLIENT_ERROR — a retryable 503 is an operational classification, not a server fault", code)
	}
	if !strings.HasPrefix(message, common.ErrCodeSearchQueueFull+":") {
		t.Errorf("message = %q, want the %s domain code", message, common.ErrCodeSearchQueueFull)
	}
	if retryable == nil || !*retryable {
		t.Errorf("retryable = %v, want true — the queue drains as in-flight jobs complete", retryable)
	}
}
