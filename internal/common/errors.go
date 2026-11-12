package common

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// ErrorLevel classifies errors into three tiers for response handling.
type ErrorLevel int

const (
	LevelOperational ErrorLevel = iota // client-visible: 4xx, 501, 503
	LevelInternal                      // 500 unexpected errors
	LevelFatal                         // unrecoverable, marks system unhealthy
)

// ErrClientGone marks an error as the request's own client having gone away
// while a layer waited on it — not a server fault. Only a layer that was
// waiting on the client may mark a cause with it: internal/domain/txjoin, for a
// joined request whose context ended while it queued for the transaction's
// lock, having touched nothing; internal/callout, for a callout whose caller's
// context ended during or between tries; internal/platformclock, for a
// clock move whose caller's context ended between steps or during a store call
// it was waiting on, which leaves the clock at the last step it applied and
// confirmed, for a set-back whose caller's context ended during its
// consistency-time read, which restores the previous setting, or for a
// confirmation round whose caller's context ended while the cluster's nodes
// were confirming a clock change; and internal/clusterstatus, for a cluster
// status read whose caller's context ended during a round or while it waited
// for nodes. A door's error funnel files a departed client on this marker and
// on nothing else, because a cancellation somewhere on an error's chain says
// only that something was called off, not that this is what went wrong — see
// isClientGoneCancellation.
//
// An error marked with it is logged at DEBUG and carries no ticket: there is
// nobody left to quote one to.
var ErrClientGone = errors.New("the request's client went away")

// ClientGone marks err — a context error a layer read off the caller's own
// context, or the error of a call that context's end interrupted — as that
// client's departure. Only a layer that was waiting on the client may call it:
// internal/txcoord, for a joined request whose context ended while it queued
// for the transaction's gate; internal/callout, for a callout whose
// caller's context ended during or between tries; internal/platformclock,
// for a clock move whose caller's context ended between steps or during a
// store call, or a clock set-back whose caller's context ended during its
// consistency-time read, or a clock change whose caller's context ended while
// the cluster's nodes were confirming it; and internal/clusterstatus, for a
// cluster status read whose caller's context ended during a round or while it
// waited for nodes. Everything under the marker is left intact, so errors.Is
// still finds the context error and status.FromContextError still reads the
// right gRPC code off it.
func ClientGone(err error) error {
	return fmt.Errorf("%w: %w", ErrClientGone, err)
}

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
	// Ticket, when set, is the correlation UUID the response carries instead of
	// a freshly minted one. Set it when the caller has ALREADY logged the full
	// detail under a ticket of its own — the panic recovery middlewares do —
	// so the client-visible ticket and that log line are the same identifier.
	// Empty is the normal case: the writer mints one.
	Ticket string
	// LogLevel, when set, is the level an operational error's one log line is
	// written at instead of INFO. A site that knows the event is more notable
	// (an unreachable transaction owner) or less (a refusal routine to the
	// protocol) sets it with WithLogLevel, attaches its detail with
	// WithCause, and logs nothing itself: the funnel writes the one line.
	LogLevel *slog.Level
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
// The (status, code, retryable) axes are separate; they were once
// bundled into specialized helpers (Conflict /
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

// WithLogLevel sets the level of an operational error's one log line (see
// LogLevel). Mutates the receiver, like WithCause; call only on a
// just-constructed error.
func (e *AppError) WithLogLevel(level slog.Level) *AppError {
	e.LogLevel = &level
	return e
}

// OperationalLogLevel is the level an operational error is logged at: its
// LogLevel when set, INFO otherwise.
func (e *AppError) OperationalLogLevel() slog.Level {
	if e.LogLevel != nil {
		return *e.LogLevel
	}
	return slog.LevelInfo
}

// withDetail sets a just-constructed error's log-only Detail to err's text.
func withDetail(e *AppError, err error) *AppError {
	e.Detail = err.Error()
	return e
}

// WithTicket pins the correlation UUID the response will carry, for callers
// that have already logged the full detail under it. Without this the writer
// mints its own, and the identifier the client is handed names no log line —
// which is the whole point of a ticket. Mutates the receiver, like AsRetryable
// and WithCause; call only on a just-constructed error.
func (e *AppError) WithTicket(ticket string) *AppError {
	e.Ticket = ticket
	return e
}

// ticketOr returns the pinned ticket, or a fresh one when none was pinned.
func (e *AppError) ticketOr() string {
	if e.Ticket != "" {
		return e.Ticket
	}
	return uuid.New().String()
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

// ModelAdminInJoinedTransaction is the refusal a request that carries a
// transaction token meets on a model or workflow administration operation.
// Model and workflow administration never runs inside a transaction: the
// request is refused before the token is verified and before anything is
// read, joined or written. The HTTP and gRPC doors answer with this one
// error, so the message is written once.
func ModelAdminInJoinedTransaction() *AppError {
	return Operational(http.StatusBadRequest, ErrCodeModelAdminInJoinedTransaction,
		"model and workflow administration cannot run inside a transaction: "+
			"the request carries a transaction token; make it without the token, as an independent request")
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

// EpochMismatch returns a retryable 409 AppError when err reports that the
// store lost ownership of the shard a transaction ran on (spi.ErrEpochMismatch),
// and nil when it does not. The store has rolled the transaction back, so the
// same request can succeed on a fresh attempt. The store's own wording rides
// along as the cause, for the log; it stays out of the client-facing message.
func EpochMismatch(err error) *AppError {
	if err == nil || !errors.Is(err, spi.ErrEpochMismatch) {
		return nil
	}
	return Operational(http.StatusConflict, ErrCodeEpochMismatch,
		"the transaction's storage shard changed owner — retry").AsRetryable().WithCause(err)
}

// TxAbortedConflict answers an error that is a consequence of an earlier
// conflict in its transaction (spi.ErrTxAborted): a retryable 409 CONFLICT with
// err attached as the cause. It returns nil for any other error. A backend
// that aborts the transaction on a conflict returns spi.ErrTxAborted for a
// statement it refuses afterwards; the workflow engine returns it, on every
// backend, for a processor that failed after the transaction lost a write race.
//
// An If-Match compare-and-save that meets such a transaction never evaluated
// the precondition, so it must not be answered as 412 ENTITY_MODIFIED. Every
// compare that maps a conflict to 412 asks this first.
func TxAbortedConflict(err error) *AppError {
	if err == nil || !errors.Is(err, spi.ErrTxAborted) {
		return nil
	}
	return Operational(http.StatusConflict, ErrCodeConflict, "transaction conflict — retry").AsRetryable().WithCause(err)
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
	if appErr := EpochMismatch(err); appErr != nil {
		return appErr
	}
	// The three sentinel answers below keep the store's text as Detail, which
	// is logged and never answered. It is not attached as a cause: a cause
	// would put the sentinel in the answer's unwrap chain, where a caller
	// classifying an unwrapped conflict would read it again.
	if err != nil && errors.Is(err, spi.ErrUniqueViolation) {
		return withDetail(Operational(http.StatusConflict, ErrCodeUniqueViolation, "a composite unique key constraint was violated"), err)
	}
	if err != nil && errors.Is(err, spi.ErrPartialUniqueKey) {
		return withDetail(Operational(http.StatusUnprocessableEntity, ErrCodeInvalidUniqueKey, "one or more unique key fields are null or invalid"), err)
	}
	if err != nil && errors.Is(err, spi.ErrConflict) {
		return withDetail(Operational(http.StatusConflict, ErrCodeConflict, "transaction conflict — retry").AsRetryable(), err)
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

// isClientGoneCancellation reports whether appErr's cause is the request's
// client having gone away while a layer waited on it, rather than a
// genuine server fault. It asks the one question that settles it: did the layer
// that was waiting on the client SAY so, by marking the cause ErrClientGone?
//
// Nothing weaker will do. A cancellation on the chain proves only that
// something, somewhere, was called off: a joined request is deliberately
// detached from its client (internal/domain/txjoin uses context.WithoutCancel),
// so work that outlives its caller can fail carrying an unrelated
// context.Canceled — classifyWorkflowError wraps any such cause as Internal —
// and "the request's context is also done" says nothing about whether that is
// what went wrong. Guessing from the two together files genuine faults as
// departed clients, and a fault filed that way is logged at DEBUG with no
// ticket, below the default level: no trace at all.
func isClientGoneCancellation(appErr *AppError) bool {
	return errors.Is(appErr.Err, ErrClientGone)
}

// WriteError writes an AppError as an RFC 9457 Problem Details JSON response.
// For INTERNAL and FATAL errors, a ticket UUID is generated for correlation —
// except when an INTERNAL error's cause is the client having gone away
// mid-request (see isClientGoneCancellation): nothing was wrong, there is
// nobody to quote a ticket to, and that is logged at DEBUG with no ticket.
// The status written is unchanged either way — an implicit 200 is what
// writing a status at all
// exists to prevent (internal/httpmw/txjoin_mw.go's writeJoinError explains
// why for the joined door, but every HTTP door funnels through here).
//
// Detail goes to the log, and to the response only for an internal or fatal
// error in the verbose error-response mode (development only). A storage
// error's text there carries host, user and database; the driver redacts the
// password.
func WriteError(w http.ResponseWriter, r *http.Request, appErr *AppError) {
	RecordOutcome(r.Context(), appErr)
	ticket := LogAnswer(r.Context(), appErr, "path", r.URL.Path)
	writeProblem(w, r, appErr, ticket)
}

// WriteRecordedFailure writes the failure recorded on ctx's holder as
// WriteError answered it — the same problem under the same ticket — without
// recording or logging it again: its one line is the one WriteError held for
// the join layer to write (LogAnswer). It
// reports false, writing nothing, when no failure is recorded. A door that
// holds a handler's answer uses it to send the failure the handler answered
// after it had written a success status.
func WriteRecordedFailure(ctx context.Context, w http.ResponseWriter, r *http.Request) bool {
	appErr, ticket := recordedAnswer(ctx)
	if appErr == nil {
		return false
	}
	writeProblem(w, r, appErr, ticket)
	return true
}

// writeProblem writes appErr as a problem response under ticket, which
// LogError returned for it.
func writeProblem(w http.ResponseWriter, r *http.Request, appErr *AppError, ticket string) {
	path := r.URL.Path
	pd := ProblemDetail{
		Type:     "about:blank",
		Title:    http.StatusText(appErr.Status),
		Status:   appErr.Status,
		Instance: path,
	}
	pd.Ticket = ticket
	pd.Detail = clientText(appErr, pd.Ticket, getErrorResponseMode() == "verbose")

	// A copy: the error, and its Props map, may be shared.
	pd.Props = make(map[string]any, len(appErr.Props)+2)
	for k, v := range appErr.Props {
		pd.Props[k] = v
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

// LogError writes an error's one log line, by its class, and returns the
// ticket an internal or fatal error is logged under ("" for an operational
// error or a client that went away). It is the funnel every door shares:
// WriteError and the gRPC envelope funnel through LogAnswer, and ReportError
// for an error answered outside a problem response. attrs name where the error
// was answered (an HTTP path, a chunk, a gRPC request).
func LogError(ctx context.Context, appErr *AppError, attrs ...any) (ticket string) {
	ticket = mintTicket(appErr)
	logLine(ctx, appErr, ticket, attrs...)
	return ticket
}

// mintTicket is the ticket appErr's line is logged under: none for an
// operational error or a client that went away, its pinned or a fresh one
// otherwise.
func mintTicket(appErr *AppError) string {
	if appErr.Level == LevelOperational || (appErr.Level == LevelInternal && isClientGoneCancellation(appErr)) {
		return ""
	}
	return appErr.ticketOr()
}

// lineLevel is the level appErr's line is logged at.
func lineLevel(appErr *AppError) slog.Level {
	switch {
	case appErr.Level == LevelOperational:
		return appErr.OperationalLogLevel()
	case appErr.Level == LevelInternal && isClientGoneCancellation(appErr):
		return slog.LevelDebug
	default:
		return slog.LevelError
	}
}

// logLine writes appErr's one line under ticket (mintTicket's).
func logLine(ctx context.Context, appErr *AppError, ticket string, attrs ...any) {
	switch appErr.Level {
	case LevelOperational:
		// Most operational errors put full domain detail in Message, which the
		// client is entitled to see. The exception is one whose cause is
		// infrastructure rather than domain — a storage failure carrying
		// connection detail such as host, user and database (the password is
		// redacted by the driver) — where the cause is attached via WithCause and
		// kept out of the response, or whose engine text is kept as Detail. The
		// log is then its only breadcrumb; neither is ever answered.
		line := append([]any{"status", appErr.Status, "code", appErr.Code, "message", appErr.Message}, attrs...)
		if appErr.Detail != "" {
			line = append(line, "detail", appErr.Detail)
		}
		if appErr.Err != nil {
			line = append(line, "cause", appErr.Err.Error())
		}
		slog.Log(ctx, appErr.OperationalLogLevel(), "operational error", line...)
	case LevelInternal:
		if isClientGoneCancellation(appErr) {
			// Routine: the client disconnected, nothing was wrong on this end,
			// and there is nobody to quote a ticket to. No ticket is minted.
			slog.Debug("client gone before request completed",
				append([]any{"code", appErr.Code, "message", appErr.Message, "detail", appErr.Detail}, attrs...)...)
			return
		}
		// Detail may hold infrastructure detail (a storage error's connection
		// detail such as host, user and database; the driver redacts the
		// password). It goes to the log only, and to the response only in the
		// verbose error-response mode (CYODA_ERROR_RESPONSE_MODE, development
		// only; see `cyoda help config`).
		slog.Error("internal error",
			append([]any{"ticket", ticket, "code", appErr.Code, "message", appErr.Message, "detail", appErr.Detail}, attrs...)...)
	default: // LevelFatal
		slog.Error("FATAL error",
			append([]any{"ticket", ticket, "code", appErr.Code, "message", appErr.Message, "detail", appErr.Detail}, attrs...)...)
	}
}

// clientText is what a client is told about appErr: an operational error's
// message; for an internal or fatal one, the generic text with its ticket (or
// without one for a client that went away), or its Detail when verbose (the
// verbose error-response mode, for a problem response only).
func clientText(appErr *AppError, ticket string, verbose bool) string {
	if appErr.Level == LevelOperational {
		return appErr.Message
	}
	if verbose {
		return appErr.Detail
	}
	if ticket == "" {
		return "SERVER_ERROR: internal error"
	}
	return fmt.Sprintf("SERVER_ERROR: internal error [ticket: %s]", ticket)
}

// ReportError logs an error answered outside a problem response — an error
// element in a 2xx collection body — exactly as WriteError logs it, and
// returns the text the client is given for it: as WriteError gives it in the
// sanitized mode, in every mode. Such a body also reaches gRPC as it is
// (a delete's per-id map), where no answer carries an internal Detail.
func ReportError(ctx context.Context, appErr *AppError, attrs ...any) string {
	return SanitizedText(appErr, LogError(ctx, appErr, attrs...))
}

// SanitizedText is what a client is told about appErr outside a problem
// response, given the ticket LogError returned: the operational message, or
// the generic text with the ticket (without one for a client that went
// away). For the gRPC envelope, which has no verbose mode.
func SanitizedText(appErr *AppError, ticket string) string {
	return clientText(appErr, ticket, false)
}

// WriteJSON writes a JSON success response with the given status code.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Debug("failed to encode response", "error", err)
	}
}
