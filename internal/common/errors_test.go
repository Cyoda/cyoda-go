package common_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

func TestOperationalError(t *testing.T) {
	err := common.Operational(http.StatusNotFound, "ENTITY_NOT_FOUND", "entity not found")
	if err.Level != common.LevelOperational {
		t.Errorf("expected OPERATIONAL, got %v", err.Level)
	}
	if err.Status != 404 {
		t.Errorf("expected 404, got %d", err.Status)
	}
}

func TestInternalError(t *testing.T) {
	cause := errors.New("db connection failed")
	err := common.Internal("store error", cause)
	if err.Level != common.LevelInternal {
		t.Errorf("expected INTERNAL, got %v", err.Level)
	}
	if err.Status != 500 {
		t.Errorf("expected 500, got %d", err.Status)
	}
	if err.Err != cause {
		t.Error("expected wrapped cause")
	}
	if err.Detail == "" {
		t.Error("expected detail from cause error")
	}
}

func TestInternal_AutoRoutesErrConflict(t *testing.T) {
	// Internal() must detect spi.ErrConflict anywhere in the error chain and
	// return a 409 Conflict with retryable=true — not a 500. The postgres
	// plugin's classifyError wraps 40001/40P01 in spi.ErrConflict; every
	// common.Internal call site in the handler relies on this auto-routing
	// rather than checking errors.Is itself.
	t.Run("direct sentinel", func(t *testing.T) {
		err := common.Internal("save failed", spi.ErrConflict)
		if err.Status != http.StatusConflict {
			t.Errorf("status = %d, want 409", err.Status)
		}
		if !err.Retryable {
			t.Error("expected retryable=true")
		}
		if err.Level != common.LevelOperational {
			t.Errorf("level = %v, want Operational", err.Level)
		}
	})

	t.Run("wrapped sentinel", func(t *testing.T) {
		wrapped := fmt.Errorf("failed to insert entity: %w: some pg detail", spi.ErrConflict)
		err := common.Internal("save failed", wrapped)
		if err.Status != http.StatusConflict {
			t.Errorf("status = %d, want 409", err.Status)
		}
		if !err.Retryable {
			t.Error("expected retryable=true")
		}
	})

	t.Run("non-conflict error stays 500", func(t *testing.T) {
		err := common.Internal("save failed", errors.New("plain db error"))
		if err.Status != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", err.Status)
		}
		if err.Retryable {
			t.Error("plain errors must not be retryable")
		}
	})
}

func TestFatalError(t *testing.T) {
	err := common.Fatal("data corruption", errors.New("bad state"))
	if err.Level != common.LevelFatal {
		t.Errorf("expected FATAL, got %v", err.Level)
	}
}

func TestWriteErrorSanitizedInternal(t *testing.T) {
	common.SetErrorResponseMode("sanitized")
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	appErr := common.Internal("something broke", errors.New("secret details"))

	common.WriteError(w, r, appErr)

	if w.Code != 500 {
		t.Errorf("expected 500, got %d", w.Code)
	}
	var pd map[string]any
	json.NewDecoder(w.Body).Decode(&pd)
	if pd["ticket"] == nil || pd["ticket"] == "" {
		t.Error("expected ticket UUID in sanitized response")
	}
	// detail should include the ticket UUID for client correlation, NOT the internal "secret details"
	detail, _ := pd["detail"].(string)
	ticket, _ := pd["ticket"].(string)
	expectedDetail := "SERVER_ERROR: internal error [ticket: " + ticket + "]"
	if detail != expectedDetail {
		t.Errorf("expected %q, got %q", expectedDetail, detail)
	}
}

func TestWriteErrorVerboseInternal(t *testing.T) {
	common.SetErrorResponseMode("verbose")
	defer common.SetErrorResponseMode("sanitized")
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	appErr := common.Internal("something broke", errors.New("db conn string here"))

	common.WriteError(w, r, appErr)

	var pd map[string]any
	json.NewDecoder(w.Body).Decode(&pd)
	if pd["ticket"] == nil {
		t.Error("expected ticket in verbose mode")
	}
	detail, _ := pd["detail"].(string)
	if detail != "db conn string here" {
		t.Errorf("expected internal detail in verbose mode, got %q", detail)
	}
}

func TestWriteErrorOperationalNoTicket(t *testing.T) {
	common.SetErrorResponseMode("sanitized")
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	appErr := common.Operational(http.StatusBadRequest, "BAD_REQUEST", "invalid input")

	common.WriteError(w, r, appErr)

	if w.Code != 400 {
		t.Errorf("expected 400, got %d", w.Code)
	}
	var pd map[string]any
	json.NewDecoder(w.Body).Decode(&pd)
	if pd["ticket"] != nil {
		t.Error("OPERATIONAL errors should not have ticket")
	}
	detail, _ := pd["detail"].(string)
	if detail != "BAD_REQUEST: invalid input" {
		t.Errorf("expected message as detail, got %q", detail)
	}
}

func TestWriteErrorFatalProducesTicketAndSanitizedDetail(t *testing.T) {
	common.SetErrorResponseMode("sanitized")
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	appErr := common.Fatal("data corruption", errors.New("internal stack trace"))

	common.WriteError(w, r, appErr)

	if w.Code != 500 {
		t.Errorf("expected 500, got %d", w.Code)
	}
	var pd map[string]any
	json.NewDecoder(w.Body).Decode(&pd)
	if pd["ticket"] == nil || pd["ticket"] == "" {
		t.Error("expected ticket UUID in FATAL response")
	}
	// In sanitized mode, detail should include the ticket UUID for correlation, not the internal trace.
	detail, _ := pd["detail"].(string)
	ticket, _ := pd["ticket"].(string)
	expectedDetail := "SERVER_ERROR: internal error [ticket: " + ticket + "]"
	if detail != expectedDetail {
		t.Errorf("expected %q, got %q", expectedDetail, detail)
	}
}

func TestWriteErrorSanitizedDoesNotLeakSecrets(t *testing.T) {
	common.SetErrorResponseMode("sanitized")
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)

	// Simulate an error that contains a "secret" connection string.
	secretErr := errors.New("pq: password authentication failed for user admin@host:5432/mydb?sslmode=disable")
	appErr := common.Internal("database error", secretErr)

	common.WriteError(w, r, appErr)

	body := w.Body.String()
	// The response body must NOT contain the connection string.
	if strings.Contains(body, "admin@host") {
		t.Error("sanitized response leaked connection string")
	}
	if strings.Contains(body, "5432") {
		t.Error("sanitized response leaked port number")
	}
	if strings.Contains(body, "sslmode") {
		t.Error("sanitized response leaked connection parameter")
	}
	// Should contain the ticket-based generic message.
	if !strings.Contains(body, "SERVER_ERROR: internal error [ticket:") {
		t.Error("expected ticket-based generic message in response")
	}
}

// TestSetErrorResponseModeConcurrent verifies that errorResponseMode is safe
// for concurrent read/write access (IM-02).
func TestSetErrorResponseModeConcurrent(t *testing.T) {
	var wg sync.WaitGroup
	// Concurrent writers
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			common.SetErrorResponseMode("verbose")
			common.SetErrorResponseMode("sanitized")
		}()
	}
	// Concurrent readers via SanitizeErrorMessage (which reads the mode)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = common.SanitizeErrorMessage(errors.New("test"))
		}()
	}
	wg.Wait()
	// If we get here without a race detector failure, the test passes.
}

func TestSetErrorResponseModeRoundTrip(t *testing.T) {
	common.SetErrorResponseMode("verbose")
	// In verbose mode, SanitizeErrorMessage should return the raw error.
	msg := common.SanitizeErrorMessage(errors.New("raw detail"))
	if msg != "raw detail" {
		t.Errorf("expected 'raw detail' in verbose mode, got %q", msg)
	}

	common.SetErrorResponseMode("sanitized")
	msg = common.SanitizeErrorMessage(errors.New("raw detail"))
	if msg != "SERVER_ERROR: internal server error" {
		t.Errorf("expected generic message in sanitized mode, got %q", msg)
	}
}

func TestAppErrorImplementsError(t *testing.T) {
	appErr := common.Operational(400, "BAD_REQUEST", "bad request")
	var err error = appErr
	if err.Error() != "BAD_REQUEST: bad request" {
		t.Errorf("expected 'BAD_REQUEST: bad request', got %q", err.Error())
	}
}

func TestErrEpochMismatch_IsSentinel(t *testing.T) {
	wrapped := fmt.Errorf("Begin failed: %w", spi.ErrEpochMismatch)
	if !errors.Is(wrapped, spi.ErrEpochMismatch) {
		t.Fatalf("expected wrapped error to match sentinel, got %v", wrapped)
	}
}

func TestErrCodeEpochMismatch_Defined(t *testing.T) {
	if common.ErrCodeEpochMismatch == "" {
		t.Fatal("ErrCodeEpochMismatch must be a non-empty string")
	}
}

func TestErrCodeSearchJobNotFound(t *testing.T) {
	if common.ErrCodeSearchJobNotFound != "SEARCH_JOB_NOT_FOUND" {
		t.Errorf("got %q", common.ErrCodeSearchJobNotFound)
	}
}

func TestErrCodeSearchJobAlreadyTerminal(t *testing.T) {
	if common.ErrCodeSearchJobAlreadyTerminal != "SEARCH_JOB_ALREADY_TERMINAL" {
		t.Errorf("got %q", common.ErrCodeSearchJobAlreadyTerminal)
	}
}

func TestErrCodeSearchShardTimeout(t *testing.T) {
	if common.ErrCodeSearchShardTimeout != "SEARCH_SHARD_TIMEOUT" {
		t.Errorf("got %q", common.ErrCodeSearchShardTimeout)
	}
}

func TestErrCodeSearchResultLimit(t *testing.T) {
	if common.ErrCodeSearchResultLimit != "SEARCH_RESULT_LIMIT" {
		t.Errorf("got %q", common.ErrCodeSearchResultLimit)
	}
}

// TestOperational_NotRetryableByDefault pins that 4xx errors from the
// primitive Operational constructor are non-retryable by default —
// retryable is opt-in via AsRetryable(). A permanent business-logic
// conflict ("model already locked", ETag mismatch on If-Match) must
// not advertise retryable; the parity HTTP client retries on
// properties.retryable=true and would burn 5 attempts × 30+ms backoff
// per permanent conflict otherwise.
func TestOperational_NotRetryableByDefault(t *testing.T) {
	err := common.Operational(http.StatusConflict, common.ErrCodeModelAlreadyLocked, "model already locked")
	if err.Status != http.StatusConflict {
		t.Errorf("status = %d, want 409", err.Status)
	}
	if err.Retryable {
		t.Error("Operational() must default to retryable=false")
	}
	if err.Level != common.LevelOperational {
		t.Errorf("level = %v, want Operational", err.Level)
	}
}

// TestOperational_AsRetryable_FlipsRetryableBit pins #140's contract:
// the three axes (status, code, retryable) are independent and any
// 4xx with any code can be flagged retryable via the fluent
// AsRetryable() opt-in. A retryable conflict with a specific
// dictionary code (e.g. retryable TX_CONFLICT — distinct from the
// generic CONFLICT) was previously unreachable without bare-field
// mutation; AsRetryable() closes that gap.
func TestOperational_AsRetryable_FlipsRetryableBit(t *testing.T) {
	err := common.Operational(http.StatusConflict, common.ErrCodeConflict, "transaction conflict — retry").AsRetryable()
	if err.Status != http.StatusConflict {
		t.Errorf("status = %d, want 409", err.Status)
	}
	if !err.Retryable {
		t.Error("AsRetryable() must set retryable=true")
	}
	if err.Code != common.ErrCodeConflict {
		t.Errorf("code = %q, want %q", err.Code, common.ErrCodeConflict)
	}
}

// TestOperational_AsRetryable_ReturnsSameInstance pins that the
// fluent helper returns the receiver — chains compose without
// allocating, and the returned pointer equals the input.
func TestOperational_AsRetryable_ReturnsSameInstance(t *testing.T) {
	err := common.Operational(http.StatusConflict, common.ErrCodeConflict, "x")
	if got := err.AsRetryable(); got != err {
		t.Errorf("AsRetryable() returned %p, want receiver %p", got, err)
	}
}

// TestWriteError_PermanentConflict_NoRetryableProperty verifies the wire
// shape the parity client sees: when retryable=false the response body
// must NOT advertise properties.retryable=true. Without that, the
// client would amplify load 5x on every business-logic 409.
func TestWriteError_PermanentConflict_NoRetryableProperty(t *testing.T) {
	common.SetErrorResponseMode("sanitized")
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/test", nil)

	common.WriteError(w, r, common.Operational(http.StatusConflict, common.ErrCodeModelAlreadyLocked, "model already locked"))

	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", w.Code)
	}
	var pd map[string]any
	if err := json.NewDecoder(w.Body).Decode(&pd); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	props, _ := pd["properties"].(map[string]any)
	if props == nil {
		t.Fatal("expected properties map in problem detail")
	}
	if v, ok := props["retryable"]; ok {
		t.Errorf("permanent conflict must omit properties.retryable, got %v", v)
	}
}

// TestWriteError_RetryableConflict_AdvertisesRetryable verifies the
// retry-eligible path still advertises properties.retryable=true so the
// parity client can engage its backoff loop.
func TestWriteError_RetryableConflict_AdvertisesRetryable(t *testing.T) {
	common.SetErrorResponseMode("sanitized")
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/test", nil)

	common.WriteError(w, r, common.Operational(http.StatusConflict, common.ErrCodeConflict, "transaction conflict — retry").AsRetryable())

	var pd map[string]any
	if err := json.NewDecoder(w.Body).Decode(&pd); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	props, _ := pd["properties"].(map[string]any)
	if props == nil {
		t.Fatal("expected properties map in problem detail")
	}
	got, ok := props["retryable"].(bool)
	if !ok || !got {
		t.Errorf("retryable conflict must advertise properties.retryable=true, got %v", props["retryable"])
	}
}
