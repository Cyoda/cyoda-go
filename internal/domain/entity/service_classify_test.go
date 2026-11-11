package entity

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	wfengine "github.com/cyoda-platform/cyoda-go/internal/domain/workflow"

	"github.com/cyoda-platform/cyoda-go/internal/txcoord"
)

// TestClassifyWorkflowError_InfraErrorMapsTo5xx is the regression test for
// security review finding Sec-#1: a CBD segment-boundary infrastructure
// failure (Begin/Commit/Save plugin error) wrapped with
// ErrCommitBeforeDispatchInfra must map to a sanitized 5xx with ticket
// UUID — NOT a 4xx WORKFLOW_FAILED whose body contains verbatim engine
// text like "commit-before-dispatch: commit TX_pre: <pgx-error>".
func TestClassifyWorkflowError_InfraErrorMapsTo5xx(t *testing.T) {
	innerSecret := "internal: connection refused on host=db-master.internal"
	infraInner := errors.Join(wfengine.ErrCommitBeforeDispatchInfra, errors.New(innerSecret))
	// Simulate the production wrapping shape:
	//   fmt.Errorf("commit-before-dispatch: commit TX_pre: %w", errors.Join(sentinel, pgxErr))
	prod := fmt.Errorf("commit-before-dispatch: commit TX_pre: %w", infraInner)

	if !errors.Is(prod, wfengine.ErrCommitBeforeDispatchInfra) {
		t.Fatalf("test setup bug: errors.Is should detect ErrCommitBeforeDispatchInfra in wrapped error")
	}

	appErr := classifyWorkflowError(prod)
	if appErr.Status != http.StatusInternalServerError {
		t.Errorf("infra error: expected 500, got %d", appErr.Status)
	}
	if appErr.Level != common.LevelInternal {
		t.Errorf("infra error: expected LevelInternal, got %v", appErr.Level)
	}
	if appErr.Code != common.ErrCodeServerError {
		t.Errorf("infra error: expected code %q, got %q", common.ErrCodeServerError, appErr.Code)
	}
	// Message is the user-facing surface; it must NOT contain the verbatim
	// engine wrapping text or the inner pgx detail.
	if strings.Contains(appErr.Message, "commit-before-dispatch") {
		t.Errorf("infra error: Message leaks engine internals: %q", appErr.Message)
	}
	if strings.Contains(appErr.Message, "host=db-master") {
		t.Errorf("infra error: Message leaks inner connection detail: %q", appErr.Message)
	}
}

// TestClassifyWorkflowError_PlainTextStays4xx verifies that an engine error
// whose text happens to contain "commit-before-dispatch" but does NOT wrap
// the sentinel still maps to 400 WORKFLOW_FAILED — the classification is
// driven by errors.Is, not string matching.
func TestClassifyWorkflowError_PlainTextStays4xx(t *testing.T) {
	plain := errors.New("commit-before-dispatch: some non-infra failure")
	appErr := classifyWorkflowError(plain)
	if appErr.Status != http.StatusBadRequest {
		t.Errorf("plain text: expected 400, got %d", appErr.Status)
	}
	if appErr.Code != common.ErrCodeWorkflowFailed {
		t.Errorf("plain text: expected code %q, got %q",
			common.ErrCodeWorkflowFailed, appErr.Code)
	}
}

// TestClassifyWorkflowError_ProcessorFailureStays4xx verifies that a
// processor-attributable failure maps to 400 WORKFLOW_FAILED — i.e. the
// Sec-#1 fix does not over-classify legitimate client-domain failures.
func TestClassifyWorkflowError_ProcessorFailureStays4xx(t *testing.T) {
	procErr := fmt.Errorf("processor %s failed: %w", "validate",
		errors.New("validation rejected: amount must be positive"))
	appErr := classifyWorkflowError(procErr)
	if appErr.Status != http.StatusBadRequest {
		t.Errorf("processor failure: expected 400, got %d", appErr.Status)
	}
	if appErr.Code != common.ErrCodeWorkflowFailed {
		t.Errorf("processor failure: expected code %q, got %q",
			common.ErrCodeWorkflowFailed, appErr.Code)
	}
}

// TestClassifyWorkflowError_TransitionNotFoundStill400 guards the existing
// TRANSITION_NOT_FOUND mapping from accidental drift.
func TestClassifyWorkflowError_TransitionNotFoundStill400(t *testing.T) {
	err := fmt.Errorf("transition %q not found in state %q: %w", "x", "S", wfengine.ErrTransitionNotFound)
	appErr := classifyWorkflowError(err)
	if appErr.Status != http.StatusBadRequest {
		t.Errorf("transition-not-found: expected 400, got %d", appErr.Status)
	}
	if appErr.Code != common.ErrCodeTransitionNotFound {
		t.Errorf("transition-not-found: expected code %q, got %q",
			common.ErrCodeTransitionNotFound, appErr.Code)
	}
}

// TestClassifyWorkflowError_ConflictBypassesViaInternal verifies that an
// infra error wrapping spi.ErrConflict still routes correctly through
// common.Internal — which detects the conflict and returns a retryable 409.
// This is a defense-in-depth check: the engine should never wrap a CAS
// conflict in ErrCommitBeforeDispatchInfra (CAS errors bubble unwrapped),
// but if the contract is ever broken, the conflict-detection in
// common.Internal still wins and clients see 409, not 500.
func TestClassifyWorkflowError_ConflictBypassesViaInternal(t *testing.T) {
	withInfraAndConflict := errors.Join(wfengine.ErrCommitBeforeDispatchInfra, spi.ErrConflict)
	appErr := classifyWorkflowError(withInfraAndConflict)
	if appErr.Status != http.StatusConflict {
		t.Errorf("infra+conflict: expected 409, got %d", appErr.Status)
	}
}

// TestClassifyWorkflowError_UniqueViolation409 verifies that a workflow error
// wrapping spi.ErrUniqueViolation maps to 409 UNIQUE_VIOLATION, not 400
// WORKFLOW_FAILED. The response message must not contain the raw error text.
func TestClassifyWorkflowError_UniqueViolation409(t *testing.T) {
	innerText := "unique-key-violation: fields=[name,tenant] clashed on txID=abc123"
	wrapped := fmt.Errorf("processor save failed: %w", fmt.Errorf("%s: %w", innerText, spi.ErrUniqueViolation))
	appErr := classifyWorkflowError(wrapped)
	if appErr.Status != http.StatusConflict {
		t.Errorf("unique violation: expected 409, got %d", appErr.Status)
	}
	if appErr.Code != common.ErrCodeUniqueViolation {
		t.Errorf("unique violation: expected code %q, got %q", common.ErrCodeUniqueViolation, appErr.Code)
	}
	if strings.Contains(appErr.Message, innerText) {
		t.Errorf("unique violation: Message leaks raw error text: %q", appErr.Message)
	}
}

// TestClassifyWorkflowError_PartialUniqueKey422 verifies that a workflow error
// wrapping spi.ErrPartialUniqueKey maps to 422 INVALID_UNIQUE_KEY, not 400
// WORKFLOW_FAILED. The response message must not contain the raw error text.
func TestClassifyWorkflowError_PartialUniqueKey422(t *testing.T) {
	innerText := "partial-key: field 'price' null, cannot compute claim"
	wrapped := fmt.Errorf("processor save failed: %w", fmt.Errorf("%s: %w", innerText, spi.ErrPartialUniqueKey))
	appErr := classifyWorkflowError(wrapped)
	if appErr.Status != http.StatusUnprocessableEntity {
		t.Errorf("partial key: expected 422, got %d", appErr.Status)
	}
	if appErr.Code != common.ErrCodeInvalidUniqueKey {
		t.Errorf("partial key: expected code %q, got %q", common.ErrCodeInvalidUniqueKey, appErr.Code)
	}
	if strings.Contains(appErr.Message, innerText) {
		t.Errorf("partial key: Message leaks raw error text: %q", appErr.Message)
	}
}

// TestClassifyWorkflowError_AuthContextUnavailableMapsTo5xx guards a
// server-side condition (missed constructor / missed cross-node context
// forwarding leaves a principal's Kind unset) from being classified as a
// client-attributable 400. An unset/nil/unrecognized principal Kind can
// never originate from client-supplied input — the client does not control
// dispatch-path UserContext construction — so it must map to a sanitized
// 5xx with a ticket, not 400 WORKFLOW_FAILED echoing the raw principal id.
func TestClassifyWorkflowError_AuthContextUnavailableMapsTo5xx(t *testing.T) {
	principalID := "user-super-secret-internal-id-123"
	// Mirror the production wrapping shape: dispatch.go wraps AttachAuthContext's
	// error with "failed to attach auth context to %s cloud event: %w", and
	// AttachAuthContext itself joins the sentinel with the detailed message.
	inner := errors.Join(contract.ErrAuthContextUnavailable,
		fmt.Errorf("attach auth context: principal kind unset for principal %q", principalID))
	prod := fmt.Errorf("failed to attach auth context to %s cloud event: %w", "processor", inner)

	if !errors.Is(prod, contract.ErrAuthContextUnavailable) {
		t.Fatalf("test setup bug: errors.Is should detect ErrAuthContextUnavailable in wrapped error")
	}

	appErr := classifyWorkflowError(prod)
	if appErr.Status != http.StatusInternalServerError {
		t.Errorf("auth-context-unavailable: expected 500, got %d", appErr.Status)
	}
	if appErr.Level != common.LevelInternal {
		t.Errorf("auth-context-unavailable: expected LevelInternal, got %v", appErr.Level)
	}
	if appErr.Code != common.ErrCodeServerError {
		t.Errorf("auth-context-unavailable: expected code %q, got %q", common.ErrCodeServerError, appErr.Code)
	}
	if strings.Contains(appErr.Message, principalID) {
		t.Errorf("auth-context-unavailable: Message leaks principal id: %q", appErr.Message)
	}

	// Client-visible assertion: the actual HTTP response body must be a
	// generic 500 with a ticket UUID — never the raw principal id or the
	// internal wrapping text.
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/entity/transition", nil)
	common.WriteError(rr, req, appErr)

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("client response: expected HTTP 500, got %d", rr.Code)
	}
	body := rr.Body.String()
	if strings.Contains(body, principalID) {
		t.Errorf("client response leaks principal id: %s", body)
	}
	if !strings.Contains(body, "ticket") {
		t.Errorf("client response missing ticket correlation field: %s", body)
	}
}

// TestClassifyWorkflowError_CriterionTypingInfraMapsTo5xx is the companion
// for the criterion-typing sentinel: the model store a type-directed
// criterion needs being unavailable is a server-side condition, so it must
// be sanitized rather than echoed into a 400 WORKFLOW_FAILED body carrying
// raw store text.
func TestClassifyWorkflowError_CriterionTypingInfraMapsTo5xx(t *testing.T) {
	const innerSecret = "pgx: connection refused on host=db-master.internal"
	prod := fmt.Errorf("%w: model order/1.0: %w",
		wfengine.ErrCriterionTypingInfra, errors.New(innerSecret))

	appErr := classifyWorkflowError(prod)
	if appErr.Status < 500 {
		t.Errorf("status = %d, want 5xx (a model-store outage is not client-attributable)", appErr.Status)
	}
	if strings.Contains(appErr.Message, innerSecret) {
		t.Errorf("client-facing message leaks store detail: %q", appErr.Message)
	}
	if !errors.Is(appErr, prod) {
		t.Error("the original error must stay wrapped for server-side logging")
	}
}

// TestClassifyWorkflowError_ScheduledTaskInfraMapsTo5xx — every save of an
// entity whose workflow carries a schedule re-arms it, so the scheduled-task
// store is on the ordinary write path, not a corner of it. A store failure
// there is never attributable to the caller's input; unclassified it reaches
// the catch-all, which puts the driver's own text into a 400 WORKFLOW_FAILED
// body. This asserts the whole chain: classification, the message, and the
// bytes a client actually receives.
func TestClassifyWorkflowError_ScheduledTaskInfraMapsTo5xx(t *testing.T) {
	const innerSecret = "ERROR: canceling statement due to statement timeout (SQLSTATE 57014)"
	// Mirror the production wrapping shape: arm.go joins the sentinel with the
	// store cause, and engine.go wraps that again on the way out.
	inner := errors.Join(wfengine.ErrScheduledTaskInfra, errors.New(innerSecret))
	prod := fmt.Errorf("failed to reconcile scheduled tasks: %w", inner)

	appErr := classifyWorkflowError(prod)
	if appErr.Status != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 (a scheduled-task store outage is not client-attributable)", appErr.Status)
	}
	if appErr.Level != common.LevelInternal {
		t.Errorf("level = %v, want LevelInternal so the response carries a ticket", appErr.Level)
	}
	if appErr.Code != common.ErrCodeServerError {
		t.Errorf("code = %q, want %q", appErr.Code, common.ErrCodeServerError)
	}
	if !errors.Is(appErr, prod) {
		t.Error("the original error must stay wrapped for server-side logging")
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/entity/JSON/e-1/next", nil)
	common.WriteError(rr, req, appErr)

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("client response: expected HTTP 500, got %d", rr.Code)
	}
	body := rr.Body.String()
	for _, leak := range []string{"SQLSTATE", "57014", "statement timeout", "canceling statement"} {
		if strings.Contains(body, leak) {
			t.Errorf("client response leaks internal detail (%q): %s", leak, body)
		}
	}
	if !strings.Contains(body, "ticket") {
		t.Errorf("client response missing ticket correlation field: %s", body)
	}
}

// TestClassifyWorkflowError_SavepointInfra — a savepoint the plugin could not
// create, undo or release says the transaction is unusable, never that the
// ASYNC_NEW_TX processor around it misbehaved. Unclassified it reaches the
// catch-all, which puts the driver's own text — SQLSTATE, host — into a 400
// WORKFLOW_FAILED body. The two mappings that ARE the caller's business survive:
// a serialization abort stays a retryable 409, a transient outage a retryable 503.
func TestClassifyWorkflowError_SavepointInfra(t *testing.T) {
	driver := errors.New(`ERROR: current transaction is aborted (SQLSTATE 25P02) host=db-1`)
	tests := []struct {
		name      string
		err       error
		status    int
		code      string
		retryable bool
	}{
		{"ticketed 5xx, no driver text", fmt.Errorf("processor p failed: %w", errors.Join(txcoord.ErrScopeInfra, driver)), http.StatusInternalServerError, common.ErrCodeServerError, false},
		{"a conflict stays 409", fmt.Errorf("processor p failed: %w", errors.Join(txcoord.ErrScopeInfra, spi.ErrConflict)), http.StatusConflict, common.ErrCodeConflict, true},
		{"an unavailable store stays 503", fmt.Errorf("processor p failed: %w", errors.Join(txcoord.ErrScopeInfra, &markedStorageOutage{detail: "acquire: host=db-1: context deadline exceeded"})), http.StatusServiceUnavailable, common.ErrCodeStorageUnavailable, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyWorkflowError(tc.err)
			if got.Status != tc.status || got.Code != tc.code || got.Retryable != tc.retryable {
				t.Fatalf("got %d %s retryable=%v; want %d %s retryable=%v", got.Status, got.Code, got.Retryable, tc.status, tc.code, tc.retryable)
			}
			if strings.Contains(got.Message, "SQLSTATE") || strings.Contains(got.Message, "db-1") {
				t.Fatalf("driver text reached the client: %q", got.Message)
			}
		})
	}
}

// TestClassifyWorkflowError_DeadlineExceeded_PreservesChainFor408 is the
// regression test for the review finding: a CBD/cascade segment whose
// pre-commit ctx check fails (see workflow.flushAndCommitSegment, which
// deliberately does NOT wrap with ErrCommitBeforeDispatchInfra so the error
// "reaches the handler-seam classifier as a plain deadline chain") was
// falling through classifyWorkflowError's catch-all —
// common.Operational(...err.Error()) sets no cause, severing the
// DeadlineExceeded chain before ClassifyRequestTimeout ever saw it, so an
// expired client-supplied transactionTimeoutMillis on a CBD/cascade flow
// surfaced as 400 WORKFLOW_FAILED instead of 408 TRANSACTION_TIMEOUT.
//
// This pins classifyWorkflowError's own output (Internal/500, cause
// preserved) AND the full handler-seam chain: feeding that AppError through
// common.ClassifyRequestTimeout on the same expired, feature-marked ctx
// yields 408 — proving ours-first classification actually reaches this path
// end to end.
func TestClassifyWorkflowError_DeadlineExceeded_PreservesChainFor408(t *testing.T) {
	ctx, cancel := common.WithRequestTimeout(context.Background(), 1)
	defer cancel()
	<-ctx.Done()

	// Mirrors flushAndCommitSegment's pre-commit-check wrapping shape exactly
	// (internal/domain/workflow/engine_processors.go): unwrapped, no infra
	// sentinel, so it reaches classifyWorkflowError's tail.
	prod := fmt.Errorf("commit-before-dispatch: context expired before segment commit: %w", ctx.Err())

	if errors.Is(prod, wfengine.ErrCommitBeforeDispatchInfra) {
		t.Fatalf("test setup bug: this error must NOT carry the infra sentinel (that path already worked before this fix)")
	}

	appErr := classifyWorkflowError(prod)
	if appErr.Level != common.LevelInternal {
		t.Fatalf("classifyWorkflowError: expected LevelInternal (never a 400 leaking err.Error()), got %v (status=%d)", appErr.Level, appErr.Status)
	}
	if appErr.Status != http.StatusInternalServerError {
		t.Errorf("classifyWorkflowError: expected 500, got %d", appErr.Status)
	}
	if !errors.Is(appErr, context.DeadlineExceeded) {
		t.Fatal("classifyWorkflowError: cause chain must still contain context.DeadlineExceeded — common.Internal preserves it, common.Operational would have severed it")
	}

	// The handler seam: ours-first classification must still find 408 here.
	classified := common.ClassifyRequestTimeout(ctx, appErr, common.ErrCodeTransactionTimeout)
	if classified == nil {
		t.Fatal("handler-seam ClassifyRequestTimeout did not classify a CBD/cascade timeout as 408 — the chain was severed somewhere")
	}
	if classified.Status != http.StatusRequestTimeout || classified.Code != common.ErrCodeTransactionTimeout || !classified.Retryable {
		t.Errorf("got %+v, want 408 TRANSACTION_TIMEOUT retryable", classified)
	}
}

// TestClassifyWorkflowError_Canceled_MapsTo5xxNotDomainLeak covers the
// non-ours-deadline half of the same branch: an unrelated cancellation
// (e.g. a caller disconnect propagating through the engine) must not become
// a 400 WORKFLOW_FAILED carrying err.Error() either — it stays a ticketed
// 500. common.ClassifyRequestTimeout never turns context.Canceled into 408
// regardless (chainHasDeadlineExceeded only matches DeadlineExceeded), so
// this case simply falls through to the general classifier's own 5xx.
func TestClassifyWorkflowError_Canceled_MapsTo5xxNotDomainLeak(t *testing.T) {
	prod := fmt.Errorf("cascade aborted: %w", context.Canceled)

	appErr := classifyWorkflowError(prod)
	if appErr.Level != common.LevelInternal {
		t.Fatalf("expected LevelInternal, got %v (status=%d, message=%q)", appErr.Level, appErr.Status, appErr.Message)
	}
	if appErr.Status != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", appErr.Status)
	}
	if !errors.Is(appErr, context.Canceled) {
		t.Error("cause chain must still contain context.Canceled for server-side diagnosis")
	}
}

// TestClassifyWorkflowError_NoMatchingMemberMapsTo503 pins the mapping the
// transitions read doors depend on. GET /entity/{id}/transitions evaluates
// workflow selection criteria, so a FUNCTION selection criterion whose tag
// no compute member serves surfaces there exactly as it does on a write:
// a retryable 503 NO_COMPUTE_MEMBER_FOR_TAG, never an opaque 500.
//
// Both transitions handlers classify through classifyWorkflowError for this
// reason; the bare sentinel below is what the non-cluster dispatcher
// returns (the cluster dispatcher pre-classifies it into the same AppError,
// which passes through unchanged).
func TestClassifyWorkflowError_NoMatchingMemberMapsTo503(t *testing.T) {
	prod := fmt.Errorf("failed to evaluate workflow criterion for %q: %w",
		"kind-a-wf", contract.ErrNoMatchingMember)

	appErr := classifyWorkflowError(prod)
	if appErr.Status != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", appErr.Status)
	}
	if appErr.Code != common.ErrCodeNoComputeMemberForTag {
		t.Errorf("code = %q, want %s", appErr.Code, common.ErrCodeNoComputeMemberForTag)
	}
	if !appErr.Retryable {
		t.Error("a missing compute member is a transient infra condition; want retryable")
	}
}

// TestClassifyEngineError_PostSegmentConflict pins what classifyEngineError
// claims for 412 and what it leaves to classifyWorkflowError when the
// post-segment marker is on the chain. The marker is chained onto the store's
// error, which is a conflict, a unique-key refusal or an aborted transaction.
func TestClassifyEngineError_PostSegmentConflict(t *testing.T) {
	const entityID = "9f7f11e2-d8ad-11f1-b5e4-ae468cd3ed18"
	postSegment := func(cause error) error {
		return fmt.Errorf("item 0: processor p failed: %w", fmt.Errorf("%w: %w", wfengine.ErrPostSegmentConflict, cause))
	}
	cases := []struct {
		name      string
		err       error
		status    int
		code      string
		retryable bool
	}{
		{"conflict is 412", postSegment(spi.ErrConflict), http.StatusPreconditionFailed, common.ErrCodeEntityModified, false},
		{"an aborted transaction stays 409", postSegment(fmt.Errorf("%w: %w", spi.ErrConflict, spi.ErrTxAborted)), http.StatusConflict, common.ErrCodeConflict, true},
		{"a unique-key refusal stays 409", postSegment(spi.ErrUniqueViolation), http.StatusConflict, common.ErrCodeUniqueViolation, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyEngineError(tc.err, entityID)
			if got.Status != tc.status || got.Code != tc.code || got.Retryable != tc.retryable {
				t.Fatalf("got %d %s retryable=%t, want %d %s retryable=%t", got.Status, got.Code, got.Retryable, tc.status, tc.code, tc.retryable)
			}
			if tc.status != http.StatusPreconditionFailed {
				return
			}
			if got.Props["entityId"] != entityID {
				t.Fatalf("properties.entityId = %v, want %s", got.Props["entityId"], entityID)
			}
			if strings.Contains(got.Message, "processor") || strings.Contains(got.Message, "segment") {
				t.Fatalf("the engine's text reached the message: %q", got.Message)
			}
		})
	}
}

// TestPerIDDeleteError_SanitizesStorageDetail — DeleteResult.IDToError is
// serialised straight into a 200 body (handler.go's deleteResult map), so
// whatever goes in it is on the wire. A storage failure's own text carries
// driver wording, SQL wrap-context and a SQLSTATE, none of which a client may
// see; the detail belongs in the log under a ticket the caller can quote.
func TestDeleteError_SanitizesStorageDetail(t *testing.T) {
	buf := captureEntitySlog(t)

	raw := errors.New("failed to mark entity deleted: ERROR: canceling statement due to statement timeout (SQLSTATE 57014)")
	msg := reportIDError(context.Background(), "e-1", deleteError("e-1", raw))

	for _, leak := range []string{"SQLSTATE", "57014", "canceling statement", "mark entity deleted"} {
		if strings.Contains(msg, leak) {
			t.Errorf("per-id error leaks internal detail (%q): %s", leak, msg)
		}
	}
	if !strings.Contains(msg, common.ErrCodeServerError) {
		t.Errorf("per-id error carries no error code: %s", msg)
	}
	if !strings.Contains(msg, "ticket") {
		t.Errorf("per-id error carries no ticket, so the logged detail cannot be correlated: %s", msg)
	}
	if logged := buf.String(); !strings.Contains(logged, "57014") {
		t.Errorf("the detail was sanitized out of the response but never logged, so it is lost: %s", logged)
	}
}

// TestPerIDDeleteError_MarkedStorageOutageKeepsItsCode — a per-item failure that
// carries the storage layer's transient-unavailability marker is classified, not
// unexplained. Flattening it to SERVER_ERROR + ticket tells the caller the item
// is hopeless when in fact a retry in a moment is exactly the right move — and
// it is the answer the same failure gets on every other door. The cause still
// stays off the wire: common.StorageUnavailable holds it in WithCause, so the
// message is client-safe by construction.
func TestDeleteError_MarkedStorageOutageKeepsItsCode(t *testing.T) {
	buf := captureEntitySlog(t)

	const cause = "acquire: host=db.internal user=cyoda: context deadline exceeded"
	raw := fmt.Errorf("failed to mark entity deleted: %w", &markedStorageOutage{detail: cause})

	msg := reportIDError(context.Background(), "e-1", deleteError("e-1", raw))

	if !strings.Contains(msg, common.ErrCodeStorageUnavailable) {
		t.Errorf("a marked storage outage lost its domain code: %s", msg)
	}
	if strings.Contains(msg, "ticket") {
		t.Errorf("a classified error minted a ticket it does not need: %s", msg)
	}
	for _, leak := range []string{"db.internal", "acquire", "deadline"} {
		if strings.Contains(msg, leak) {
			t.Errorf("per-id error leaks the storage cause (%q): %s", leak, msg)
		}
	}

	// The classified branch returns before this function's ticketed
	// slog.Error, and the caller does not log either — so without a
	// breadcrumb here, WHY storage was unavailable is recorded nowhere. The
	// usual rescue is that the same outage also fails scope.Commit(), which
	// logs; that is not guaranteed. Same message and field name as
	// common.WriteError's operational branch, so the two read alike in an
	// aggregator.
	logged := buf.String()
	if !strings.Contains(logged, cause) {
		t.Errorf("the storage cause was kept out of the response but never logged, so it is lost: %s", logged)
	}
	if !strings.Contains(logged, "operational error") {
		t.Errorf("the breadcrumb does not use the shared operational-error message: %s", logged)
	}
	if !strings.Contains(logged, "e-1") {
		t.Errorf("the breadcrumb does not name the entity it belongs to: %s", logged)
	}
}

// markedStorageOutage carries the storage layer's transient-unavailability
// marker on a raw error, as a plugin does.
type markedStorageOutage struct{ detail string }

func (e *markedStorageOutage) Error() string          { return e.detail }
func (*markedStorageOutage) StorageUnavailable() bool { return true }

// TestPerIDDeleteError_KeepsDomainDetail — a not-found is the caller's own
// business and carries no infrastructure detail, so it stays legible rather
// than being flattened into a ticket.
func TestDeleteError_KeepsDomainDetail(t *testing.T) {
	msg := reportIDError(context.Background(), "e-1", deleteError("e-1", fmt.Errorf("ENTITY_NOT_FOUND: entity e-1 not found: %w", spi.ErrNotFound)))
	if !strings.Contains(msg, common.ErrCodeEntityNotFound) {
		t.Errorf("a not-found lost its domain code: %s", msg)
	}
	if strings.Contains(msg, "ticket") {
		t.Errorf("a domain error minted a ticket it does not need: %s", msg)
	}
}

// captureEntitySlog redirects the default logger into a buffer for the duration
// of the test, so a "the detail was logged, not returned" claim is checkable.
func captureEntitySlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// TestEngineConflictIsTransactionConflict: a conflict the engine marks as its
// own statement's failure is a transaction conflict, never the caller's
// If-Match precondition; an unmarked one is the precondition.
func TestEngineConflictIsTransactionConflict(t *testing.T) {
	marked := []error{
		spi.ErrTxAborted,
		wfengine.ErrScheduledTaskInfra,
		wfengine.ErrProcessorOutputInfra,
		txcoord.ErrScopeInfra,
		wfengine.ErrCommitBeforeDispatchInfra,
		wfengine.ErrCriterionTypingInfra,
	}
	for _, sentinel := range marked {
		err := fmt.Errorf("processor p failed: %w", errors.Join(sentinel, spi.ErrConflict))
		if !engineConflictIsTransactionConflict(err) {
			t.Errorf("%v joined with a conflict was read as the caller's precondition", sentinel)
		}
	}
	if engineConflictIsTransactionConflict(fmt.Errorf("If-Match precondition failed: %w", spi.ErrConflict)) {
		t.Error("an unmarked conflict — the If-Match check — was read as a transaction conflict")
	}
}

// TestClassifyWorkflowError_TxAbortedIs409: a bare spi.ErrTxAborted reaching
// the classifier is a retryable 409 CONFLICT with its cause, never the
// catch-all 400 WORKFLOW_FAILED.
func TestClassifyWorkflowError_TxAbortedIs409(t *testing.T) {
	err := fmt.Errorf("processor p failed: %w", spi.ErrTxAborted)
	got := classifyWorkflowError(err)
	if got.Status != http.StatusConflict || got.Code != common.ErrCodeConflict || !got.Retryable {
		t.Fatalf("classified as %d %s retryable=%v, want a retryable 409 %s", got.Status, got.Code, got.Retryable, common.ErrCodeConflict)
	}
	if !errors.Is(got, spi.ErrTxAborted) {
		t.Fatal("the cause is not attached")
	}
}

// TestClassifyWorkflowError_EpochMismatchIsARetryable409 — a store error that
// reports a lost shard (spi.ErrEpochMismatch) inside a cascade is not a
// workflow failure: it must not fall through to 400 WORKFLOW_FAILED carrying
// the store's text.
func TestClassifyWorkflowError_EpochMismatchIsARetryable409(t *testing.T) {
	appErr := classifyWorkflowError(fmt.Errorf("tx read record aborted: shard revoked: %w", spi.ErrEpochMismatch))
	if appErr.Status != http.StatusConflict || appErr.Code != common.ErrCodeEpochMismatch || !appErr.Retryable {
		t.Fatalf("got %d/%s/retryable=%v, want 409/%s/true", appErr.Status, appErr.Code, appErr.Retryable, common.ErrCodeEpochMismatch)
	}
	if strings.Contains(appErr.Message, "shard revoked") {
		t.Fatalf("message leaks the store's wording: %q", appErr.Message)
	}
}

// TestPerIDDeleteError_EpochMismatchKeepsItsCode — a delete item that failed on
// a lost shard reports the retryable code, not a ticket.
func TestDeleteError_EpochMismatchKeepsItsCode(t *testing.T) {
	msg := reportIDError(context.Background(), "e1", deleteError("e1", fmt.Errorf("transactional delete aborted: shard revoked: %w", spi.ErrEpochMismatch)))
	if !strings.HasPrefix(msg, common.ErrCodeEpochMismatch+":") {
		t.Fatalf("message = %q, want the %s code", msg, common.ErrCodeEpochMismatch)
	}
}

// An engine refusal answered with an operational error keeps, in the error's
// log-only Detail, the engine's own text — the processor, the item — and the
// entity it was for; the deleted service log line was its only record. The
// passed-through error itself is not changed (it may be shared).
func TestClassifyEngineError_KeepsTheEngineTextAsDetail(t *testing.T) {
	inner := common.Operational(http.StatusServiceUnavailable, common.ErrCodeCalloutFailed, "CALLOUT_FAILED: the compute member failed").AsRetryable()
	got := classifyEngineError(fmt.Errorf("item 3: processor enrich failed: %w", inner), "ent-42")
	if got.Code != common.ErrCodeCalloutFailed || got.Message != inner.Message {
		t.Fatalf("classified = %s %q, want the passed-through error", got.Code, got.Message)
	}
	if !strings.Contains(got.Detail, "processor enrich failed") || !strings.Contains(got.Detail, "item 3") || !strings.Contains(got.Detail, "ent-42") {
		t.Errorf("Detail = %q, want the engine text and the entity id", got.Detail)
	}
	if inner.Detail != "" {
		t.Errorf("the passed-through error was changed: Detail = %q", inner.Detail)
	}

	conflict := classifyEngineError(fmt.Errorf("unique key: %w", spi.ErrUniqueViolation), "ent-43")
	if conflict.Code != common.ErrCodeUniqueViolation || !strings.Contains(conflict.Detail, "unique key") {
		t.Errorf("unique violation = %s, Detail %q; want UNIQUE_VIOLATION carrying the store text", conflict.Code, conflict.Detail)
	}
}

// A not-found id in a delete is answered in the 200 body's per-id map and
// logged once, at INFO, as the door's funnel logs a 404.
func TestDeleteError_NotFoundIsLoggedOnce(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	msg := reportIDError(context.Background(), "e-9", deleteError("e-9", fmt.Errorf("get: %w", spi.ErrNotFound)))
	if !strings.HasPrefix(msg, common.ErrCodeEntityNotFound+": ") {
		t.Fatalf("message = %q, want ENTITY_NOT_FOUND", msg)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], "level=INFO") || !strings.Contains(lines[0], "e-9") {
		t.Errorf("log = %q, want one INFO line naming the id", lines)
	}
}

// A passed-through operational error that already carries a Detail — the
// store's text behind a conflict — keeps it next to the engine's text.
func TestClassifyEngineError_KeepsAPassedThroughDetail(t *testing.T) {
	inner := common.Internal("failed to load workflows", fmt.Errorf("pg: could not serialize access: %w", spi.ErrConflict))
	got := classifyEngineError(fmt.Errorf("load: %w", inner), "ent-44")
	if !strings.Contains(got.Detail, "could not serialize access") || !strings.Contains(got.Detail, "ent-44") {
		t.Errorf("Detail = %q, want the store text and the entity", got.Detail)
	}
}

// An operational error with a cause of its own, found deep inside the
// engine's wrap, keeps the engine's text in Detail: its cause is not the
// engine's error and does not carry that text.
func TestClassifyEngineError_DeepCausedErrorKeepsTheEngineText(t *testing.T) {
	inner := common.Operational(http.StatusServiceUnavailable, common.ErrCodeCalloutFailed, "the compute member failed").WithCause(errors.New("store text"))
	got := classifyEngineError(fmt.Errorf("processor enrich failed: %w", inner), "ent-45")
	if !strings.Contains(got.Detail, "processor enrich failed") || !strings.Contains(got.Detail, "ent-45") {
		t.Errorf("Detail = %q, want the engine text and the entity", got.Detail)
	}
}
