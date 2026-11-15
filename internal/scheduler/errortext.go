package scheduler

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

const (
	cancelledText = "CANCELLED: the run was stopped by the scheduler"
	conflictText  = "CONFLICT: a concurrent write changed the entity or its task"
	taskBusyText  = "CONFLICT: the task is being written by another transaction"
)

// internalErrorText is what a tenant user sees for a failure whose detail is
// internal. The full error is logged at ERROR under the same ticket.
func internalErrorText(ticket uuid.UUID) string {
	return "internal error [ticket: " + ticket.String() + "]"
}

// recordedError maps a run's failure to lastError, which tenant users can read.
// err must be non-nil. The first matching row applies. Only the last row
// mints a ticket; the caller logs the full error at ERROR under it. The other
// rows are client-safe text and are logged at WARN.
func recordedError(err error) (text string, ticket uuid.UUID, warnOnly bool) {
	if errors.Is(err, context.Canceled) {
		return cancelledText, uuid.Nil, true
	}
	if errors.Is(err, spi.ErrConflict) {
		return conflictText, uuid.Nil, true
	}
	if errors.Is(err, spi.ErrTaskBusy) {
		return taskBusyText, uuid.Nil, true
	}
	var failure *contract.CalloutFailure
	if errors.As(err, &failure) {
		var cause *common.AppError
		internalCause := errors.As(failure.Err, &cause) && cause.Level != common.LevelOperational
		if !internalCause {
			return failure.Message, uuid.Nil, true
		}
		// The cause is a LevelInternal or LevelFatal AppError: this pnode's
		// own failure, not a client-safe verdict from a compute node. Fall
		// through to the ticket row below, which mints a ticket and lets the
		// caller log the full cause at ERROR under it.
	}
	if text, ok := refusedAtCommit(err); ok {
		return text, uuid.Nil, true
	}
	var appErr *common.AppError
	if errors.As(err, &appErr) && appErr.Level == common.LevelOperational {
		return appErr.Message, uuid.Nil, true
	}
	ticket = uuid.New()
	return internalErrorText(ticket), ticket, false
}

// refusedAtCommit reports whether err is a commit refused on the run's final
// entities — a duplicate or partial unique key, or a model no longer locked —
// and returns the text the doors answer for it. That is the answer a client
// write gets, not a fault of this pnode. The text keeps the store's detail
// out; the outcome's log line carries it as the cause. An error that
// common.Internal maps to anything else, a storage outage carrying one of the
// sentinels among them, is not such a refusal and keeps the ticket.
func refusedAtCommit(err error) (string, bool) {
	if !errors.Is(err, spi.ErrUniqueViolation) && !errors.Is(err, spi.ErrPartialUniqueKey) &&
		!errors.Is(err, spi.ErrModelNotLocked) {
		return "", false
	}
	switch appErr := common.Internal("", err); appErr.Code {
	case common.ErrCodeUniqueViolation, common.ErrCodeInvalidUniqueKey, common.ErrCodeModelNotLocked:
		return appErr.Message, true
	}
	return "", false
}

// sanitiseErrorText makes text storable on every backend: NUL and invalid
// UTF-8 become U+FFFD, and the text is cut at a character boundary to at most
// spi.MaxTaskErrorBytes.
func sanitiseErrorText(s string) string {
	s = strings.ReplaceAll(s, "\x00", "�")
	s = strings.ToValidUTF8(s, "�")
	if len(s) <= spi.MaxTaskErrorBytes {
		return s
	}
	cut := spi.MaxTaskErrorBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
