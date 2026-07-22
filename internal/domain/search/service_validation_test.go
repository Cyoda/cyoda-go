package search_test

import (
	"errors"
	"net/http"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// Fix B — gRPC search bypasses all condition-type validation.
//
// ValidateConditionValueTypes previously had a single call site: the HTTP
// handler. The gRPC search handlers (internal/grpc/search.go) parsed the
// condition and passed it straight to SearchService with no validation, so
// a gRPC client submitting a type-unsound condition (e.g. CONTAINS against
// a temporal meta field, an unknown meta field, or a non-offset temporal
// operand) got wrong-but-available results instead of a 400-equivalent
// rejection — a correctness-over-availability violation.
//
// These tests exercise the transport-agnostic SearchService entry points
// (SubmitAsyncSearch / DirectSearch — the ones the gRPC handlers call
// directly) to prove condition-type validation now runs at the service
// boundary, covering every transport uniformly.

// registerModelWithSchema saves a LOCKED model descriptor with the given
// data fields and returns its ModelRef, so EnsureModelRegistered succeeds
// and validateConditionTypes has a real schema to build a *schema.ModelNode
// from (a nil/empty schema would make ValidateConditionValueTypes a no-op).
func registerModelWithSchema(t *testing.T, factory *memory.StoreFactory, entityName string, fields ...string) spi.ModelRef {
	t.Helper()
	ref := spi.ModelRef{EntityName: entityName, ModelVersion: "1"}
	ctx := tenantCtx("tenant-1")
	ms, err := factory.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	desc := buildSearchDescriptor(t, ref, fields...)
	if err := ms.Save(ctx, desc); err != nil {
		t.Fatalf("Save model: %v", err)
	}
	return ref
}

func assertAppErrorCode(t *testing.T, err error, wantCode string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *common.AppError, got %T: %v", err, err)
	}
	if appErr.Code != wantCode {
		t.Errorf("appErr.Code = %q, want %q (message: %s)", appErr.Code, wantCode, appErr.Message)
	}
	if appErr.Status != http.StatusBadRequest {
		t.Errorf("appErr.Status = %d, want %d", appErr.Status, http.StatusBadRequest)
	}
}

// TestDirectSearch_LifecycleContainsOnTemporalField_RejectsAtServiceBoundary
// verifies that DirectSearch (the gRPC-facing alias for Search) rejects a
// CONTAINS operator against the temporal meta field creationDate with
// CONDITION_TYPE_MISMATCH — CONTAINS has no meaningful temporal semantics.
func TestDirectSearch_LifecycleContainsOnTemporalField_RejectsAtServiceBoundary(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	ref := registerModelWithSchema(t, factory, "grpc-validate-a", "name")

	uuids := common.NewTestUUIDGenerator()
	searchStore, _ := factory.AsyncSearchStore(tenantCtx("tenant-1"))
	svc := search.NewSearchService(factory, uuids, searchStore)

	cond := &predicate.LifecycleCondition{
		Field:        "creationDate",
		OperatorType: "CONTAINS",
		Value:        "2021",
	}

	_, err := svc.DirectSearch(tenantCtx("tenant-1"), ref, cond, search.SearchOptions{})
	assertAppErrorCode(t, err, common.ErrCodeConditionTypeMismatch)
}

// TestDirectSearch_UnknownMetaField_RejectsAtServiceBoundary verifies that
// DirectSearch rejects a LifecycleCondition naming a meta field the
// vocabulary does not recognize with INVALID_FIELD_PATH.
func TestDirectSearch_UnknownMetaField_RejectsAtServiceBoundary(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	ref := registerModelWithSchema(t, factory, "grpc-validate-b", "name")

	uuids := common.NewTestUUIDGenerator()
	searchStore, _ := factory.AsyncSearchStore(tenantCtx("tenant-1"))
	svc := search.NewSearchService(factory, uuids, searchStore)

	cond := &predicate.LifecycleCondition{
		Field:        "notARealMetaField",
		OperatorType: "EQUALS",
		Value:        "x",
	}

	_, err := svc.DirectSearch(tenantCtx("tenant-1"), ref, cond, search.SearchOptions{})
	assertAppErrorCode(t, err, common.ErrCodeInvalidFieldPath)
}

// TestSubmitAsyncSearch_NonOffsetTemporalOperand_RejectsAtServiceBoundary
// verifies that SubmitAsyncSearch (the gRPC-facing alias for SubmitAsync)
// rejects a creationDate comparison whose operand is not an offset-bearing
// RFC3339 timestamp with CONDITION_TYPE_MISMATCH, and that no job is
// created before the rejection.
func TestSubmitAsyncSearch_NonOffsetTemporalOperand_RejectsAtServiceBoundary(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	ref := registerModelWithSchema(t, factory, "grpc-validate-c", "name")

	uuids := common.NewTestUUIDGenerator()
	searchStore, _ := factory.AsyncSearchStore(tenantCtx("tenant-1"))
	svc := search.NewSearchService(factory, uuids, searchStore)

	cond := &predicate.LifecycleCondition{
		Field:        "creationDate",
		OperatorType: "GREATER_THAN",
		// No UTC offset — spi.ParseTemporalMillis requires an offset-bearing
		// RFC3339 timestamp, so this must be rejected as a type mismatch.
		Value: "2021-01-01T00:00:00",
	}

	jobID, err := svc.SubmitAsyncSearch(tenantCtx("tenant-1"), ref, cond, search.SearchOptions{})
	assertAppErrorCode(t, err, common.ErrCodeConditionTypeMismatch)
	if jobID != "" {
		t.Errorf("jobID = %q, want empty (no job created before rejection)", jobID)
	}
}

// C1 — malformed BETWEEN operand arity bypassed gRPC entirely.
//
// search.ValidateCondition (operators.go) — which now also enforces BETWEEN
// arity (exactly two operands) — previously had a single call site: the HTTP
// handler. Like Fix B's condition-type validation gap, the gRPC search
// handlers (internal/grpc/search.go) call DirectSearch/SubmitAsyncSearch
// directly and never passed through the HTTP handler, so a gRPC client could
// submit a scalar-operand BETWEEN condition — the exact C1 repro — and reach
// the storage plugin with no rejection at all.
//
// These tests exercise DirectSearch/SubmitAsyncSearch directly (the
// gRPC-facing entry points) to prove ValidateCondition's arity check now runs
// at the service boundary, covering every transport uniformly.

// TestDirectSearch_ScalarBetweenOperand_RejectsAtServiceBoundary verifies
// that DirectSearch rejects a lifecycle BETWEEN condition whose value is a
// bare scalar (not a 2-element array) with a 400 BAD_REQUEST AppError.
func TestDirectSearch_ScalarBetweenOperand_RejectsAtServiceBoundary(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	ref := registerModelWithSchema(t, factory, "grpc-validate-d", "name")

	uuids := common.NewTestUUIDGenerator()
	searchStore, _ := factory.AsyncSearchStore(tenantCtx("tenant-1"))
	svc := search.NewSearchService(factory, uuids, searchStore)

	// The exact C1 repro condition: a scalar RFC3339 operand against a
	// temporal lifecycle field's BETWEEN operator.
	cond := &predicate.LifecycleCondition{
		Field:        "creationDate",
		OperatorType: "BETWEEN",
		Value:        "2021-01-01T00:00:00Z",
	}

	_, err := svc.DirectSearch(tenantCtx("tenant-1"), ref, cond, search.SearchOptions{})
	assertAppErrorCode(t, err, common.ErrCodeBadRequest)
}

// TestSubmitAsyncSearch_ScalarBetweenOperand_RejectsAtServiceBoundary
// verifies that SubmitAsyncSearch rejects the same malformed BETWEEN
// condition with a 400 BAD_REQUEST AppError, and that no job is created
// before the rejection.
func TestSubmitAsyncSearch_ScalarBetweenOperand_RejectsAtServiceBoundary(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	ref := registerModelWithSchema(t, factory, "grpc-validate-e", "age")

	uuids := common.NewTestUUIDGenerator()
	searchStore, _ := factory.AsyncSearchStore(tenantCtx("tenant-1"))
	svc := search.NewSearchService(factory, uuids, searchStore)

	cond := &predicate.SimpleCondition{
		JsonPath:     "$.age",
		OperatorType: "BETWEEN",
		Value:        float64(18), // scalar, not the required 2-element array
	}

	jobID, err := svc.SubmitAsyncSearch(tenantCtx("tenant-1"), ref, cond, search.SearchOptions{})
	assertAppErrorCode(t, err, common.ErrCodeBadRequest)
	if jobID != "" {
		t.Errorf("jobID = %q, want empty (no job created before rejection)", jobID)
	}
}
