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

// maxErrorTextBytes bounds lastError. Every backend stores the same text.
const maxErrorTextBytes = 1024

const (
	cancelledText = "CANCELLED: the run was stopped by the scheduler"
	conflictText  = "CONFLICT: a concurrent write changed the entity or its task"
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
	var appErr *common.AppError
	if errors.As(err, &appErr) && appErr.Level == common.LevelOperational {
		return appErr.Message, uuid.Nil, true
	}
	ticket = uuid.New()
	return internalErrorText(ticket), ticket, false
}

// sanitiseErrorText makes text storable on every backend: NUL and invalid
// UTF-8 become U+FFFD, and the text is cut at a character boundary to at most
// maxErrorTextBytes.
func sanitiseErrorText(s string) string {
	s = strings.ReplaceAll(s, "\x00", "�")
	s = strings.ToValidUTF8(s, "�")
	if len(s) <= maxErrorTextBytes {
		return s
	}
	cut := maxErrorTextBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
