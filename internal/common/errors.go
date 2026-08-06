package common

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// ErrNotFound is a sentinel error for entity/resource not-found conditions.

// ErrEpochMismatch is a sentinel error returned when a node attempts to write
// to a shard it no longer owns (or never owned). Mapped to a retryable HTTP
// error so clients re-route to the new owner.

// ErrConflict is a sentinel error for MVCC conflicts (entity modified concurrently).

// ErrorLevel classifies errors into three tiers for response handling.
type ErrorLevel int

const (
	LevelOperational ErrorLevel = iota // 4xx client errors
	LevelInternal                      // 500 unexpected errors
	LevelFatal                         // unrecoverable, marks system unhealthy
)

// AppError represents a classified application error with client-safe and
// internal details separated for security.
type AppError struct {
	Level     ErrorLevel
	Status    int
	Code      string
	Message   string         // client-safe, always shown
	Detail    string         // internal detail, only in verbose mode / logs
	Err       error          // wrapped original error
	Props     map[string]any // optional structured properties for ProblemDetail
	Retryable bool
}

func (e *AppError) Error() string { return e.Message }

func (e *AppError) Unwrap() error { return e.Err }

// AsRetryable flips the retryable bit on a 4xx error and returns the
// receiver for fluent chaining. Use for the rare case a 4xx is
// retry-eligible — typically SI+FCW transaction aborts (PostgreSQL
// 40001 / 40P01 under REPEATABLE READ, or application-layer FCW
// validation failures on memory/sqlite/cassandra) or optimistic-lock
// failures triggered by concurrent writers, where naive retry without
// a state change can succeed.
//
// Permanent business-logic conflicts (locked-state mismatches, ETag
// mismatches, cardinality precondition failures) MUST NOT be flagged
// retryable — calling those retryable causes pointless backoff and
// 5x request amplification on the parity client side.
//
// AsRetryable mutates the receiver. The intended call shape is
// Operational(...).AsRetryable() on a freshly-constructed *AppError
// — do NOT call on an aliased or shared *AppError, since the flip
// is observable from every other reference to the same instance.
//
// Issue #140 — separates the (status, code, retryable) axes that
// were previously bundled into specialized helpers (Conflict /
// RetryableConflict, removed). Retryable is now opt-in on top of
// any (status, code) pair via Operational(...).AsRetryable().
func (e *AppError) AsRetryable() *AppError {
	e.Retryable = true
	return e
}

// WithCause attaches a wrapped cause to a freshly-constructed *AppError and
// returns the receiver for fluent chaining. The cause is exposed via Unwrap,
// so errors.Is(returned, cause) holds — use this when translating a storage
// SPI sentinel into a client-facing AppError while keeping the sentinel
// inspectable by callers. Like AsRetryable, this mutates the receiver; call
// only on a just-constructed Operational(...), never on a shared instance.
func (e *AppError) WithCause(err error) *AppError {
	e.Err = err
	return e
}

// Operational creates a client error (4xx). No internal detail is captured.
//
// Default is non-retryable; for the rare retry-eligible 4xx (e.g. an
// SI+FCW transaction abort), chain .AsRetryable().
func Operational(status int, code string, message string) *AppError {
	return &AppError{
		Level:   LevelOperational,
		Status:  status,
		Code:    code,
		Message: fmt.Sprintf("%s: %s", code, message),
	}
}

// StorageUnavailable returns a retryable 503 AppError when err carries the
// storage layer's transient-unavailability marker, and nil when it does not.
//
// Matched with errors.As on an interface rather than a concrete type: the error
// is already wrapped several times over by the time a classifier sees it, and
// the marker is a plugin-side type this module must not import. A storage plugin
// opts in by returning an error whose chain satisfies the interface — no SPI
// change, so no coordinated cross-repo release.
//
// The cause rides along via WithCause so the log can say WHY storage was
// unavailable. It stays out of the client-facing message deliberately: unlike a
// domain 4xx, this detail is infrastructure — a pgx connection error carries
// host, user and database (pgx redacts the password), which is operator
// information, not caller information.
func StorageUnavailable(err error) *AppError {
	var su interface{ StorageUnavailable() bool }
	if err != nil && errors.As(err, &su) && su.StorageUnavailable() {
		return Operational(
			http.StatusServiceUnavailable,
			ErrCodeStorageUnavailable,
			"storage is temporarily unavailable — retry",
		).AsRetryable().WithCause(err)
	}
	return nil
}

// Internal creates a 500 error with internal detail from the wrapped error.
//
// If the wrapped error is (or wraps) spi.ErrConflict, the result is routed to
// a retryable 409 instead — a serialization abort (40001/40P01) that fully
// rolled back is retryable, not a server error. This keeps every call site
// honest without forcing each to reason about pgx error codes.
//
// A transient storage outage is routed likewise, to a retryable 503. It is
// checked first because it is the one condition here whose cause must not reach
// the response body, and because a storage plugin's own marker is more specific
// than any sentinel a wrapper below it might also carry.
func Internal(message string, err error) *AppError {
	if appErr := StorageUnavailable(err); appErr != nil {
		return appErr
	}
	if err != nil && errors.Is(err, spi.ErrUniqueViolation) {
		return Operational(http.StatusConflict, ErrCodeUniqueViolation, "a composite unique key constraint was violated")
	}
	if err != nil && errors.Is(err, spi.ErrPartialUniqueKey) {
		return Operational(http.StatusUnprocessableEntity, ErrCodeInvalidUniqueKey, "one or more unique key fields are null or invalid")
	}
	if err != nil && errors.Is(err, spi.ErrConflict) {
		return Operational(http.StatusConflict, ErrCodeConflict, "transaction conflict — retry").AsRetryable()
	}
	detail := ""
	if err != nil {
		detail = err.Error()
	}
	return &AppError{
		Level:   LevelInternal,
		Status:  http.StatusInternalServerError,
		Code:    ErrCodeServerError,
		Message: fmt.Sprintf("%s: %s", ErrCodeServerError, message),
		Detail:  detail,
		Err:     err,
	}
}

// InternalWithCode creates a 500 error with internal detail, like Internal,
// but stamps a specific client-facing error code instead of the generic
// ErrCodeServerError. Use when the 500 is precise enough for the caller to
// distinguish it from an opaque infra failure (e.g. a compute node returned
// a result the engine could not interpret) — the code needs a matching
// errors/<CODE>.md topic (TestErrCode_Parity enforces the pairing). Unlike
// Internal, this does not remap known sentinel errors (unique-violation,
// partial-key, tx-conflict) to their dedicated 4xx codes; callers with a
// custom code are not passing those sentinels.
func InternalWithCode(code, message string, err error) *AppError {
	detail := ""
	if err != nil {
		detail = err.Error()
	}
	return &AppError{
		Level:   LevelInternal,
		Status:  http.StatusInternalServerError,
		Code:    code,
		Message: fmt.Sprintf("%s: %s", code, message),
		Detail:  detail,
		Err:     err,
	}
}

// Fatal creates a 500 error indicating an unrecoverable failure.
func Fatal(message string, err error) *AppError {
	detail := ""
	if err != nil {
		detail = err.Error()
	}
	return &AppError{
		Level:   LevelFatal,
		Status:  http.StatusInternalServerError,
		Code:    ErrCodeServerError,
		Message: fmt.Sprintf("%s: %s", ErrCodeServerError, message),
		Detail:  detail,
		Err:     err,
	}
}

// errorResponseMode controls whether internal error details are included in
// HTTP responses. Safe for concurrent access via atomic.Value.
var errorResponseMode atomic.Value

func init() {
	errorResponseMode.Store("sanitized")
}

// SetErrorResponseMode configures the error response mode.
// Use "verbose" to include internal details in responses (development only).
// Any other value defaults to sanitized mode.
func SetErrorResponseMode(mode string) {
	errorResponseMode.Store(mode)
}

// getErrorResponseMode returns the current error response mode.
func getErrorResponseMode() string {
	return errorResponseMode.Load().(string)
}

// ProblemDetail represents an RFC 9457 Problem Details response.
type ProblemDetail struct {
	Type     string         `json:"type"`
	Title    string         `json:"title"`
	Status   int            `json:"status"`
	Detail   string         `json:"detail,omitempty"`
	Instance string         `json:"instance"`
	Ticket   string         `json:"ticket,omitempty"`
	Props    map[string]any `json:"properties,omitempty"`
}

// WriteError writes an AppError as an RFC 9457 Problem Details JSON response.
// For INTERNAL and FATAL errors, a ticket UUID is generated for correlation.
//
// SECURITY NOTE: The Detail field (from err.Error()) may contain connection
// strings or secrets when real persistence is added. Review logging of Detail
// before connecting to external datastores.
func WriteError(w http.ResponseWriter, r *http.Request, appErr *AppError) {
	path := r.URL.Path

	pd := ProblemDetail{
		Type:     "about:blank",
		Title:    http.StatusText(appErr.Status),
		Status:   appErr.Status,
		Instance: path,
	}

	switch appErr.Level {
	case LevelOperational:
		attrs := []any{
			"status", appErr.Status,
			"message", appErr.Message,
			"path", path,
		}
		// Most operational errors put full domain detail in Message, which the
		// client is entitled to see. The exception is one whose cause is
		// infrastructure rather than domain — a storage failure carrying
		// connection detail such as host, user and database (the password is
		// redacted by the driver) — where the cause is attached via WithCause and
		// kept out of the response. The log is then its only breadcrumb.
		if appErr.Err != nil {
			attrs = append(attrs, "cause", appErr.Err.Error())
		}
		slog.Info("operational error", attrs...)
		pd.Detail = appErr.Message

	case LevelInternal:
		ticket := uuid.New().String()
		// SECURITY NOTE: appErr.Detail may contain secrets (connection strings,
		// credentials) once real persistence is added. Review before deploying
		// with external datastores.
		slog.Error("internal error",
			"ticket", ticket,
			"message", appErr.Message,
			"detail", appErr.Detail,
			"path", path,
		)
		pd.Ticket = ticket
		if getErrorResponseMode() == "verbose" {
			pd.Detail = appErr.Detail
		} else {
			pd.Detail = fmt.Sprintf("SERVER_ERROR: internal error [ticket: %s]", ticket)
		}

	case LevelFatal:
		ticket := uuid.New().String()
		// SECURITY NOTE: appErr.Detail may contain secrets (connection strings,
		// credentials) once real persistence is added. Review before deploying
		// with external datastores.
		slog.Error("FATAL error",
			"ticket", ticket,
			"message", appErr.Message,
			"detail", appErr.Detail,
			"path", path,
		)
		pd.Ticket = ticket
		if getErrorResponseMode() == "verbose" {
			pd.Detail = appErr.Detail
		} else {
			pd.Detail = fmt.Sprintf("SERVER_ERROR: internal error [ticket: %s]", ticket)
		}
	}

	if appErr.Props != nil {
		pd.Props = appErr.Props
	} else {
		pd.Props = make(map[string]any)
	}
	pd.Props["errorCode"] = appErr.Code

	if appErr.Retryable {
		pd.Props["retryable"] = true
	}

	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(appErr.Status)
	if err := json.NewEncoder(w).Encode(pd); err != nil {
		slog.Debug("failed to encode response", "error", err)
	}
}

// SanitizeErrorMessage returns a client-safe error message.
// For AppError: returns Message (which is always client-safe).
// For raw errors: in sanitized mode returns a generic message;
// in verbose mode returns err.Error().
func SanitizeErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	var appErr *AppError
	if errors.As(err, &appErr) {
		// AppError.Message is already client-safe by design.
		return appErr.Message
	}
	// Raw error — could contain internal state.
	if getErrorResponseMode() == "verbose" {
		return err.Error()
	}
	return "SERVER_ERROR: internal server error"
}

// WriteJSON writes a JSON success response with the given status code.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Debug("failed to encode response", "error", err)
	}
}
