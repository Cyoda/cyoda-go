package grpc

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// A compute member's reason and failure text reach the state-machine audit
// trail, which records no event holding U+0000. One such text would otherwise
// drop every event that carries it — a criterion's reason is copied into the
// FINISH event's stop reason, a function condition's into TRANSITION_MAKE's
// functions — so a member could make a committed state change leave no record.
// An answer whose text holds U+0000 is therefore unreadable: Terminal, with
// this node's fixed message and none of the member's text.
func TestDispatch_MemberTextHoldingNULIsUnreadable(t *testing.T) {
	no := false
	const want = "the compute member's response could not be read: a reason or error text held U+0000"
	for name, tc := range map[string]struct {
		resp     *ProcessingResponse
		dispatch func(d *ProcessorDispatcher) error
	}{
		"criterion reason": {
			resp: &ProcessingResponse{Success: true, Matches: &no, Reason: "too\x00small"},
			dispatch: func(d *ProcessorDispatcher) error {
				_, _, err := dispatchCriteria(d, testContext(), testEntity(), json.RawMessage(answerCriterion), "transition", "wf1", "t1", "", "tx-1")
				return err
			},
		},
		"criterion failure text": {
			resp: &ProcessingResponse{Success: false, Error: "bad\x00input"},
			dispatch: func(d *ProcessorDispatcher) error {
				_, _, err := dispatchCriteria(d, testContext(), testEntity(), json.RawMessage(answerCriterion), "transition", "wf1", "t1", "", "tx-1")
				return err
			},
		},
		"processor failure text": {
			resp: &ProcessingResponse{Success: false, Error: "card\x00declined"},
			dispatch: func(d *ProcessorDispatcher) error {
				_, err := dispatchProcessor(d, testContext(), testEntity(), testProcessor("python", 5000), "wf1", "t1", "tx-1")
				return err
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			dispatcher, registry, memberID, sentCh := setupTestDispatcher(t)
			replyOnce(t, registry, memberID, sentCh, tc.resp)

			err := tc.dispatch(dispatcher)
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
		})
	}
}
