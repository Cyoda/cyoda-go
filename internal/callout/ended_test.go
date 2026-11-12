package callout

import (
	"context"
	"errors"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/txcoord"
)

// TestEnded_CoordinationFailureIsNotTheCallerLeaving: a callout stopped
// because the backend that coordinates the transaction could not be asked
// fails with that cause, never as the caller's departure (which is logged
// quietly and without a ticket).
func TestEnded_CoordinationFailureIsNotTheCallerLeaving(t *testing.T) {
	cause := errors.Join(txcoord.ErrScopeInfra, errors.New("the coordinator's store is unreachable"))
	cctx, cancel := context.WithCancelCause(context.Background())
	cancel(cause)

	err := ended(cctx, cctx.Err())
	if errors.Is(err, common.ErrClientGone) {
		t.Fatalf("ended = %v; a coordination failure must not read as the caller leaving", err)
	}
	if !errors.Is(err, txcoord.ErrScopeInfra) {
		t.Fatalf("ended = %v; want the coordination failure", err)
	}
}
