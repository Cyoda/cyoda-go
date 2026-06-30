package entity

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	wfengine "github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
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
