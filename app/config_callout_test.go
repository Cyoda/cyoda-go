package app

import (
	"os"
	"strings"
	"testing"
	"time"
)

// unsetEnv clears keys for the duration of the test, restoring them after.
func unsetEnv(t *testing.T, keys ...string) {
	t.Helper()
	for _, k := range keys {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

// validCalloutConfig is the Callout block DefaultConfig yields under an empty
// environment — the baseline each validation case perturbs by one field.
func validCalloutConfig() CalloutConfig {
	return CalloutConfig{
		FixedNumRetries:        3,
		ResponseTimeout:        30 * time.Second,
		ResponseTimeoutMax:     60 * time.Second,
		HandoverAllowance:      30 * time.Second,
		PassAllowance:          30 * time.Second,
		JoinedResponseMaxBytes: 10485760,
	}
}

func TestDefaultConfig_CalloutTriesAndAnswerLimit(t *testing.T) {
	unsetEnv(t, "CYODA_RETRY_FIXED_NUM_RETRIES",
		"CYODA_CALLOUT_RESPONSE_TIMEOUT_MS", "CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS")

	got := DefaultConfig().Callout
	if got.FixedNumRetries != 3 {
		t.Errorf("default FixedNumRetries = %d, want 3", got.FixedNumRetries)
	}
	if got.ResponseTimeout != 30*time.Second {
		t.Errorf("default ResponseTimeout = %s, want 30s", got.ResponseTimeout)
	}
	if got.ResponseTimeoutMax != 60*time.Second {
		t.Errorf("default ResponseTimeoutMax = %s, want 1m0s", got.ResponseTimeoutMax)
	}

	t.Setenv("CYODA_RETRY_FIXED_NUM_RETRIES", "0")
	t.Setenv("CYODA_CALLOUT_RESPONSE_TIMEOUT_MS", "1500")
	t.Setenv("CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS", "45000")
	got = DefaultConfig().Callout
	if got.FixedNumRetries != 0 {
		t.Errorf("FixedNumRetries override = %d, want 0 (0 is a value, not 'unset')", got.FixedNumRetries)
	}
	if got.ResponseTimeout != 1500*time.Millisecond {
		t.Errorf("ResponseTimeout override = %s, want 1.5s", got.ResponseTimeout)
	}
	if got.ResponseTimeoutMax != 45*time.Second {
		t.Errorf("ResponseTimeoutMax override = %s, want 45s", got.ResponseTimeoutMax)
	}
}

func TestValidateCallout_TriesAndAnswerLimit(t *testing.T) {
	if err := ValidateCallout(validCalloutConfig()); err != nil {
		t.Fatalf("defaults rejected: %v", err)
	}
	atBound := validCalloutConfig()
	atBound.ResponseTimeout = atBound.ResponseTimeoutMax
	if err := ValidateCallout(atBound); err != nil {
		t.Fatalf("default answer limit equal to the upper bound rejected: %v", err)
	}
	zeroRetries := validCalloutConfig()
	zeroRetries.FixedNumRetries = 0
	if err := ValidateCallout(zeroRetries); err != nil {
		t.Fatalf("0 retries rejected: %v", err)
	}

	cases := []struct {
		name     string
		mutate   func(*CalloutConfig)
		wantName string
	}{
		{"negative retries", func(c *CalloutConfig) { c.FixedNumRetries = -1 }, "CYODA_RETRY_FIXED_NUM_RETRIES"},
		{"zero answer limit", func(c *CalloutConfig) { c.ResponseTimeout = 0 }, "CYODA_CALLOUT_RESPONSE_TIMEOUT_MS"},
		{"negative answer limit", func(c *CalloutConfig) { c.ResponseTimeout = -time.Second }, "CYODA_CALLOUT_RESPONSE_TIMEOUT_MS"},
		{"zero upper bound", func(c *CalloutConfig) { c.ResponseTimeoutMax = 0 }, "CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS"},
		{"answer limit above the upper bound", func(c *CalloutConfig) {
			c.ResponseTimeout = c.ResponseTimeoutMax + time.Millisecond
		}, "CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validCalloutConfig()
			tc.mutate(&c)
			err := ValidateCallout(c)
			if err == nil {
				t.Fatalf("ValidateCallout(%+v) = nil, want an error", c)
			}
			if !strings.Contains(err.Error(), tc.wantName) {
				t.Errorf("error must name %s; got: %v", tc.wantName, err)
			}
		})
	}
}

func TestDefaultConfig_CalloutAllowances(t *testing.T) {
	unsetEnv(t, "CYODA_CALLOUT_HANDOVER_ALLOWANCE", "CYODA_CALLOUT_PASS_ALLOWANCE")
	got := DefaultConfig().Callout
	if got.HandoverAllowance != 30*time.Second {
		t.Errorf("default HandoverAllowance = %s, want 30s", got.HandoverAllowance)
	}
	if got.PassAllowance != 30*time.Second {
		t.Errorf("default PassAllowance = %s, want 30s", got.PassAllowance)
	}
	t.Setenv("CYODA_CALLOUT_HANDOVER_ALLOWANCE", "10s")
	t.Setenv("CYODA_CALLOUT_PASS_ALLOWANCE", "45s")
	got = DefaultConfig().Callout
	if got.HandoverAllowance != 10*time.Second || got.PassAllowance != 45*time.Second {
		t.Errorf("overrides not bound: %+v", got)
	}
}

func TestDefaultConfig_CalloutJoinedResponseMaxBytes(t *testing.T) {
	unsetEnv(t, "CYODA_CALLOUT_JOINED_RESPONSE_MAX_BYTES")
	if got := DefaultConfig().Callout.JoinedResponseMaxBytes; got != 10485760 {
		t.Errorf("default JoinedResponseMaxBytes = %d, want 10485760 (10 MiB)", got)
	}
	t.Setenv("CYODA_CALLOUT_JOINED_RESPONSE_MAX_BYTES", "2097152")
	if got := DefaultConfig().Callout.JoinedResponseMaxBytes; got != 2097152 {
		t.Errorf("JoinedResponseMaxBytes override = %d, want 2097152", got)
	}
}

func TestValidateCallout_JoinedResponseMaxBytes(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*CalloutConfig)
	}{
		{"zero answer ceiling", func(c *CalloutConfig) { c.JoinedResponseMaxBytes = 0 }},
		{"negative answer ceiling", func(c *CalloutConfig) { c.JoinedResponseMaxBytes = -1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validCalloutConfig()
			tc.mutate(&c)
			err := ValidateCallout(c)
			if err == nil {
				t.Fatalf("ValidateCallout(%+v) = nil, want an error", c)
			}
			if !strings.Contains(err.Error(), "CYODA_CALLOUT_JOINED_RESPONSE_MAX_BYTES") {
				t.Errorf("error must name CYODA_CALLOUT_JOINED_RESPONSE_MAX_BYTES; got: %v", err)
			}
		})
	}
}

func TestValidateCallout_Allowances(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*CalloutConfig)
		wantName string
	}{
		{"zero hand-over allowance", func(c *CalloutConfig) { c.HandoverAllowance = 0 }, "CYODA_CALLOUT_HANDOVER_ALLOWANCE"},
		{"negative hand-over allowance", func(c *CalloutConfig) { c.HandoverAllowance = -time.Second }, "CYODA_CALLOUT_HANDOVER_ALLOWANCE"},
		{"zero pass allowance", func(c *CalloutConfig) { c.PassAllowance = 0 }, "CYODA_CALLOUT_PASS_ALLOWANCE"},
		{"negative pass allowance", func(c *CalloutConfig) { c.PassAllowance = -time.Second }, "CYODA_CALLOUT_PASS_ALLOWANCE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validCalloutConfig()
			tc.mutate(&c)
			err := ValidateCallout(c)
			if err == nil {
				t.Fatalf("ValidateCallout(%+v) = nil, want an error", c)
			}
			if !strings.Contains(err.Error(), tc.wantName) {
				t.Errorf("error must name %s; got: %v", tc.wantName, err)
			}
		})
	}
}
