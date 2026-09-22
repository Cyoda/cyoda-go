package workflow

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

const testMaxResponseTimeout = 60 * time.Second

func limitsProcessorWF(ms int64) spi.WorkflowDefinition {
	wf := retryPolicyFixture("")
	wf.States["S1"].Transitions[0].Processors[0].Config.ResponseTimeoutMs = ms
	return wf
}

func limitsFunctionWF(ms int64) spi.WorkflowDefinition {
	wf := scheduleFunctionFixture("")
	wf.States["S1"].Transitions[0].Schedule.Function.ResponseTimeoutMs = ms
	return wf
}

func limitsCriterion(ms int64) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(
		`{"type":"function","function":{"name":"min-amount","config":{"calculationNodesTags":"pricing","responseTimeoutMs":%d}}}`, ms))
}

func TestValidateCalloutLimits(t *testing.T) {
	cases := []struct {
		name    string
		wf      func(ms int64) spi.WorkflowDefinition
		wantLoc []string
	}{
		{"processor", limitsProcessorWF, []string{`workflow "wf-rp"`, `state "S1"`, `transition "t"`, `processor "p"`}},
		{"schedule function", limitsFunctionWF, []string{`workflow "wf-fn"`, `state "S1"`, `transition "t"`, "schedule.function"}},
		{"transition criterion", func(ms int64) spi.WorkflowDefinition {
			return wfWithTransitionCriterion(limitsCriterion(ms))
		}, []string{`workflow "wf-regex"`, `state "S1"`, `transition "go"`, `criterion function "min-amount"`}},
		{"workflow criterion", func(ms int64) spi.WorkflowDefinition {
			return wfWithWorkflowCriterion(limitsCriterion(ms))
		}, []string{`workflow "wf-regex"`, `criterion function "min-amount"`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, ok := range []int64{0, 1, 60000} {
				if err := validateCalloutLimits([]spi.WorkflowDefinition{tc.wf(ok)}, testMaxResponseTimeout); err != nil {
					t.Errorf("responseTimeoutMs=%d rejected: %v", ok, err)
				}
			}

			err := validateCalloutLimits([]spi.WorkflowDefinition{tc.wf(60001)}, testMaxResponseTimeout)
			if err == nil {
				t.Fatal("responseTimeoutMs=60001 accepted over a 60000 bound")
			}
			for _, want := range append(tc.wantLoc, "responseTimeoutMs 60001", "60000", "CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS") {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error must contain %q; got: %v", want, err)
				}
			}

			err = validateCalloutLimits([]spi.WorkflowDefinition{tc.wf(-1)}, testMaxResponseTimeout)
			if err == nil {
				t.Fatal("responseTimeoutMs=-1 accepted")
			}
			for _, want := range append(tc.wantLoc, "must not be negative") {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error must contain %q; got: %v", want, err)
				}
			}
		})
	}
}

// A value that would overflow time.Duration when multiplied out must still be
// compared correctly: the comparison is made in milliseconds.
func TestValidateCalloutLimits_HugeValue(t *testing.T) {
	err := validateCalloutLimits([]spi.WorkflowDefinition{limitsProcessorWF(1 << 62)}, testMaxResponseTimeout)
	if err == nil {
		t.Fatal("responseTimeoutMs=2^62 accepted")
	}
}

func TestValidateCalloutLimits_FollowsTheConfiguredBound(t *testing.T) {
	wf := limitsProcessorWF(6000)
	if err := validateCalloutLimits([]spi.WorkflowDefinition{wf}, 5*time.Second); err == nil {
		t.Fatal("6000 accepted under a 5000 bound")
	}
	if err := validateCalloutLimits([]spi.WorkflowDefinition{wf}, 6*time.Second); err != nil {
		t.Fatalf("6000 rejected under a 6000 bound: %v", err)
	}
}
