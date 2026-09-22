package grpc

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

func TestResolveAnswerLimit(t *testing.T) {
	d := newTestDispatcher(t, NewMemberRegistry()) // default 30s, upper bound 60s
	tests := []struct {
		name         string
		storedMs     int64
		want         time.Duration
		wantTerminal bool
	}{
		{"unset uses the default", 0, 30 * time.Second, false},
		{"negative uses the default", -5, 30 * time.Second, false},
		{"a stored value is used as it is", 1500, 1500 * time.Millisecond, false},
		{"exactly the bound is allowed", 60_000, 60 * time.Second, false},
		{"one over the bound is refused, not clamped", 60_001, 0, true},
		{"a value that would overflow a Duration is refused", math.MaxInt64, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, failure := d.ResolveAnswerLimit(tt.storedMs)
			if tt.wantTerminal {
				if failure == nil || failure.Kind != contract.Terminal {
					t.Fatalf("failure = %+v, want Terminal", failure)
				}
				if !strings.Contains(failure.Message, "CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS") {
					t.Errorf("message %q must name the setting", failure.Message)
				}
				var appErr *common.AppError
				if errors.As(failure, &appErr) {
					t.Errorf("must carry no AppError (it is a 400 WORKFLOW_FAILED by the catch-all), got %v", appErr)
				}
				return
			}
			if failure != nil {
				t.Fatalf("unexpected failure: %v", failure)
			}
			if got != tt.want {
				t.Errorf("limit = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDispatch_StoredTimeoutOverTheBound_IsTerminalAndNothingIsSent(t *testing.T) {
	registry := NewMemberRegistry()
	sent := make(chan struct{}, 1)
	m := registry.Register("m-1", testTenantID, []string{"python"}, func(*cepb.CloudEvent) error {
		sent <- struct{}{}
		return nil
	}, nil)
	t.Cleanup(func() { registry.Unregister(m) })
	d := newTestDispatcher(t, registry)

	_, err := dispatchProcessor(d, testContext(), testEntity(), testProcessor("python", 60_001), "wf1", "t1", "tx-1")
	var failure *contract.CalloutFailure
	if !errors.As(err, &failure) || failure.Kind != contract.Terminal {
		t.Fatalf("err = %v, want a Terminal CalloutFailure", err)
	}
	select {
	case <-sent:
		t.Fatal("a callout over the bound must not reach a cnode")
	default:
	}
}
