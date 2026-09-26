package scheduler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

func calloutFailure(kind contract.CalloutFailureKind, appErr *common.AppError) *contract.CalloutFailure {
	return &contract.CalloutFailure{Kind: kind, Code: appErr.Code, Message: appErr.Message, Err: appErr}
}

func TestRecordedError_AllowList(t *testing.T) {
	timeout := common.Operational(http.StatusServiceUnavailable, common.ErrCodeDispatchTimeout,
		"processor dispatch timed out after 100ms: no response").AsRetryable()
	noMember := common.Operational(http.StatusServiceUnavailable, common.ErrCodeNoComputeMemberForTag,
		"no compute member for tags [billing]").AsRetryable()
	badRequest := common.Operational(http.StatusBadRequest, common.ErrCodeWorkflowFailed, "the processor output is not an object")

	tests := []struct {
		name     string
		err      error
		want     string
		warnOnly bool
	}{
		{"a cancellation of the run", fmt.Errorf("run stopped: %w", context.Canceled), cancelledText, true},
		{"a cancellation inside a callout failure",
			&contract.CalloutFailure{Kind: contract.NoAnswer, Message: "cancelled", Err: context.Canceled}, cancelledText, true},
		{"a conflict", fmt.Errorf("failed to commit: %w", spi.ErrConflict), conflictText, true},
		{"a busy task row", fmt.Errorf("failed to mark the task: %w", spi.ErrTaskBusy),
			"CONFLICT: the task is being written by another transaction", true},
		{"a busy task row inside a callout failure",
			&contract.CalloutFailure{Kind: contract.NoHandOff, Message: "busy", Err: spi.ErrTaskBusy},
			"CONFLICT: the task is being written by another transaction", true},
		{"a compute node's own message", fmt.Errorf("processor charge failed: %w",
			&contract.CalloutFailure{Kind: contract.MemberFailed, Message: "card declined"}), "card declined", true},
		{"a callout timeout", calloutFailure(contract.NoAnswer, timeout),
			"DISPATCH_TIMEOUT: processor dispatch timed out after 100ms: no response", true},
		{"no compute member for the tag", calloutFailure(contract.NoHandOff, noMember),
			"NO_COMPUTE_MEMBER_FOR_TAG: no compute member for tags [billing]", true},
		{"an Operational AppError, wrapped", fmt.Errorf("fire: %w", badRequest),
			"WORKFLOW_FAILED: the processor output is not an object", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text, ticket, warnOnly := recordedError(tt.err)
			if text != tt.want || warnOnly != tt.warnOnly || ticket != uuid.Nil {
				t.Errorf("recordedError = (%q, %s, %v), want (%q, nil ticket, %v)", text, ticket, warnOnly, tt.want, tt.warnOnly)
			}
		})
	}
}

func TestRecordedError_AnythingElseIsATicketOnly(t *testing.T) {
	for name, err := range map[string]error{
		"a store error": fmt.Errorf("failed to read entity: %w",
			errors.New("pq: connection refused host=db.internal user=cyoda")),
		"an Internal AppError": common.Internal("failed to save", errors.New("disk full at /var/lib/pg")),
	} {
		t.Run(name, func(t *testing.T) {
			text, ticket, warnOnly := recordedError(err)
			if warnOnly || ticket == uuid.Nil {
				t.Fatalf("recordedError = (%q, %s, %v), want a ticket and ERROR", text, ticket, warnOnly)
			}
			if want := "internal error [ticket: " + ticket.String() + "]"; text != want {
				t.Errorf("text = %q, want %q", text, want)
			}
		})
	}
}

func TestRecordedError_CalloutFailureInternalCauseGetsATicket(t *testing.T) {
	t.Run("an Internal AppError cause falls through to the ticket row", func(t *testing.T) {
		internal := common.Internal("failed to mint transaction pass", errors.New("disk full at /var/lib/pg"))
		err := &contract.CalloutFailure{Kind: contract.NoAnswer, Code: internal.Code, Message: internal.Message, Err: internal}

		text, ticket, warnOnly := recordedError(err)
		if warnOnly || ticket == uuid.Nil {
			t.Fatalf("recordedError = (%q, %s, %v), want a ticket and ERROR", text, ticket, warnOnly)
		}
		if want := internalErrorText(ticket); text != want {
			t.Errorf("text = %q, want %q", text, want)
		}
	})

	t.Run("a Fatal AppError cause falls through to the ticket row", func(t *testing.T) {
		fatal := common.Fatal("unrecoverable", errors.New("panic recovered: nil pointer"))
		err := &contract.CalloutFailure{Kind: contract.Terminal, Code: fatal.Code, Message: fatal.Message, Err: fatal}

		text, ticket, warnOnly := recordedError(err)
		if warnOnly || ticket == uuid.Nil {
			t.Fatalf("recordedError = (%q, %s, %v), want a ticket and ERROR", text, ticket, warnOnly)
		}
		if want := internalErrorText(ticket); text != want {
			t.Errorf("text = %q, want %q", text, want)
		}
	})

	t.Run("an Operational AppError cause still records its Message", func(t *testing.T) {
		op := common.Operational(http.StatusBadRequest, common.ErrCodeWorkflowFailed, "the processor output is not an object")
		err := &contract.CalloutFailure{Kind: contract.NoHandOff, Code: op.Code, Message: op.Message, Err: op}

		text, ticket, warnOnly := recordedError(err)
		if text != op.Message || !warnOnly || ticket != uuid.Nil {
			t.Errorf("recordedError = (%q, %s, %v), want (%q, nil ticket, true)", text, ticket, warnOnly, op.Message)
		}
	})

	t.Run("a plain code with no cause still records its Message", func(t *testing.T) {
		err := &contract.CalloutFailure{Kind: contract.MemberFailed, Message: "card declined"}

		text, ticket, warnOnly := recordedError(err)
		if text != "card declined" || !warnOnly || ticket != uuid.Nil {
			t.Errorf("recordedError = (%q, %s, %v), want (%q, nil ticket, true)", text, ticket, warnOnly, "card declined")
		}
	})
}

func TestSanitiseErrorText(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"short text is unchanged", "CODE: detail", "CODE: detail"},
		{"NUL is replaced", "a\x00b", "a�b"},
		{"invalid UTF-8 is replaced", "a\xffb", "a�b"},
		{"exactly the limit is kept", strings.Repeat("a", 1024), strings.Repeat("a", 1024)},
		{"a character across the limit is dropped whole", strings.Repeat("a", 1023) + "é", strings.Repeat("a", 1023)},
		{"three-byte characters are cut at a boundary", strings.Repeat("€", 400), strings.Repeat("€", 341)},
		{"NUL and multi-byte text over the limit", strings.Repeat("\x00€", 300),
			strings.Repeat("�€", 170) + "�"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitiseErrorText(tt.in)
			if got != tt.want {
				t.Errorf("sanitiseErrorText: got %d bytes %q, want %d bytes", len(got), got, len(tt.want))
			}
			if len(got) > maxErrorTextBytes || !utf8.ValidString(got) || strings.ContainsRune(got, 0) {
				t.Errorf("result is not storable: %d bytes, valid=%v", len(got), utf8.ValidString(got))
			}
		})
	}
}
