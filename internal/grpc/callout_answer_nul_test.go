package grpc

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// A compute member's reason and failure text reach the state-machine audit
// trail, which cannot hold U+0000 on PostgreSQL, and a criterion's reason is
// copied into later events too (the FINISH event's stop reason,
// TRANSITION_MAKE's functions). An answer whose text holds U+0000 is therefore
// unreadable: Terminal, with this node's fixed message, none of the member's
// text, and none of its warnings.
func TestDispatch_MemberTextHoldingNULIsUnreadable(t *testing.T) {
	no := false
	const want = "the compute member's response could not be read: a reason or error text held U+0000"
	criterion := func(d *ProcessorDispatcher, ctx context.Context) error {
		_, _, err := dispatchCriteria(d, ctx, testEntity(), json.RawMessage(answerCriterion), "transition", "wf1", "t1", "", "tx-1")
		return err
	}
	processor := func(d *ProcessorDispatcher, ctx context.Context) error {
		_, err := dispatchProcessor(d, ctx, testEntity(), testProcessor("python", 5000), "wf1", "t1", "tx-1")
		return err
	}
	for name, tc := range map[string]struct {
		resp     *ProcessingResponse
		dispatch func(d *ProcessorDispatcher, ctx context.Context) error
	}{
		"criterion reason": {
			resp:     &ProcessingResponse{Success: true, Matches: &no, Reason: "too\x00small", Warnings: []string{"the member warns"}},
			dispatch: criterion,
		},
		"criterion failure text": {
			resp:     &ProcessingResponse{Success: false, Error: "bad\x00input", Warnings: []string{"the member warns"}},
			dispatch: criterion,
		},
		"processor failure text": {
			resp:     &ProcessingResponse{Success: false, Error: "card\x00declined", Warnings: []string{"the member warns"}},
			dispatch: processor,
		},
		"error text on a success answer": {
			resp:     &ProcessingResponse{Success: true, Error: "ignored\x00text", Warnings: []string{"the member warns"}},
			dispatch: processor,
		},
	} {
		t.Run(name, func(t *testing.T) {
			dispatcher, registry, memberID, sentCh := setupTestDispatcher(t)
			replyOnce(t, registry, memberID, sentCh, tc.resp)
			ctx := common.WithDiagnostics(testContext())

			err := tc.dispatch(dispatcher, ctx)
			var failure *contract.CalloutFailure
			if !errors.As(err, &failure) {
				t.Fatalf("error = %v; want a *contract.CalloutFailure", err)
			}
			if failure.Kind != contract.Terminal {
				t.Errorf("kind = %s; want %s", failure.Kind, contract.Terminal)
			}
			if failure.Message != want || failure.MemberText != "" {
				t.Errorf("message = %q, member text = %q; want %q and no member text", failure.Message, failure.MemberText, want)
			}
			if got := common.GetDiagnostics(ctx).GetWarnings(); len(got) != 0 {
				t.Errorf("warnings = %v; want none: nothing is read out of an answer that could not be read", got)
			}
		})
	}
}
