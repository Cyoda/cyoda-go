package parity

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// workflow_callout_config.go pins what workflow import accepts and refuses in
// the configuration of a callout — a processor, a function criterion, a
// schedule function — and that the fields it accepts survive storage.

// calloutConfigWorkflow builds a workflow with one manual transition carrying
// an optional function criterion and an optional processor, and one scheduled
// transition when schedule is non-empty. Arguments are raw JSON or "".
func calloutConfigWorkflow(wfName, criterion, processorConfig, schedule string) string {
	manual := `"name": "go", "next": "DONE", "manual": true`
	if criterion != "" {
		manual += `, "criterion": ` + criterion
	}
	if processorConfig != "" {
		manual += fmt.Sprintf(`, "processors": [{"type":"externalized","name":"p","executionMode":"SYNC","config": %s}]`, processorConfig)
	}
	transitions := "{" + manual + "}"
	if schedule != "" {
		transitions += fmt.Sprintf(`, {"name": "later", "next": "DONE", "schedule": %s}`, schedule)
	}
	return fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.5", "name": %q, "initialState": "OPEN", "active": true,
			"states": {"OPEN": {"transitions": [%s]}, "DONE": {}}
		}]
	}`, wfName, transitions)
}

func calloutConfigModel(t *testing.T, c *client.Client, modelName string) {
	t.Helper()
	if err := c.ImportModel(t, modelName, 1, `{"name":"Test","amount":0}`); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	if err := c.LockModel(t, modelName, 1); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
}

func criterionWithConfig(config string) string {
	return fmt.Sprintf(`{"type":"function","function":{"name":"min-amount","config":%s}}`, config)
}

func scheduleWithFunction(extra string) string {
	return fmt.Sprintf(`{"function":{"name":"computeFire","resultKind":"Schedule","calculationNodesTags":"scheduler"%s}}`, extra)
}

// RunWorkflowImportCalloutRetryPolicyValidated: retryPolicy is NONE, FIXED or
// absent on all three callout kinds; anything else is 400 VALIDATION_FAILED.
func RunWorkflowImportCalloutRetryPolicyValidated(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)
	const modelName = "parity-callout-retry-policy"
	calloutConfigModel(t, c, modelName)

	for _, policy := range []string{"NONE", "FIXED"} {
		body := calloutConfigWorkflow("retry-ok-wf",
			criterionWithConfig(fmt.Sprintf(`{"calculationNodesTags":"pricing","retryPolicy":%q}`, policy)),
			fmt.Sprintf(`{"calculationNodesTags":"workers","retryPolicy":%q}`, policy),
			scheduleWithFunction(fmt.Sprintf(`,"retryPolicy":%q`, policy)))
		status, resp, err := c.ImportWorkflowRaw(t, modelName, 1, body)
		if err != nil {
			t.Fatalf("[%s] ImportWorkflowRaw: %v", policy, err)
		}
		if status != http.StatusOK {
			t.Fatalf("retryPolicy %q on all three callouts: expected 200, got %d; body=%s", policy, status, resp)
		}
	}

	for _, tc := range []struct{ label, body string }{
		{"processor", calloutConfigWorkflow("retry-bad-wf", "", `{"calculationNodesTags":"workers","retryPolicy":"LINEAR"}`, "")},
		{"criterion", calloutConfigWorkflow("retry-bad-wf", criterionWithConfig(`{"calculationNodesTags":"pricing","retryPolicy":"LINEAR"}`), "", "")},
		{"function", calloutConfigWorkflow("retry-bad-wf", "", "", scheduleWithFunction(`,"retryPolicy":"LINEAR"`))},
	} {
		status, resp, err := c.ImportWorkflowRaw(t, modelName, 1, tc.body)
		if err != nil {
			t.Fatalf("[%s] ImportWorkflowRaw: %v", tc.label, err)
		}
		if status != http.StatusBadRequest {
			t.Fatalf("[%s] unknown retryPolicy: expected 400, got %d; body=%s", tc.label, status, resp)
		}
		if !containsErrorCode(resp, "VALIDATION_FAILED") {
			t.Errorf("[%s] expected errorCode VALIDATION_FAILED, body=%s", tc.label, resp)
		}
		if !bytes.Contains(resp, []byte("retry-bad-wf")) {
			t.Errorf("[%s] detail must name the workflow, body=%s", tc.label, resp)
		}
	}
}

// RunWorkflowImportResponseTimeoutBounded: responseTimeoutMs is accepted up to
// the server's upper bound (the fixture runs with the default, 60000) and
// refused when negative or above it, on all three callout kinds.
func RunWorkflowImportResponseTimeoutBounded(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)
	const modelName = "parity-callout-timeout-bound"
	calloutConfigModel(t, c, modelName)

	build := map[string]func(ms int) string{
		"processor": func(ms int) string {
			return calloutConfigWorkflow("bound-wf", "", fmt.Sprintf(`{"calculationNodesTags":"workers","responseTimeoutMs":%d}`, ms), "")
		},
		"criterion": func(ms int) string {
			return calloutConfigWorkflow("bound-wf", criterionWithConfig(fmt.Sprintf(`{"calculationNodesTags":"pricing","responseTimeoutMs":%d}`, ms)), "", "")
		},
		"function": func(ms int) string {
			return calloutConfigWorkflow("bound-wf", "", "", scheduleWithFunction(fmt.Sprintf(`,"responseTimeoutMs":%d`, ms)))
		},
	}
	for _, kind := range []string{"processor", "criterion", "function"} {
		status, resp, err := c.ImportWorkflowRaw(t, modelName, 1, build[kind](60000))
		if err != nil {
			t.Fatalf("[%s] ImportWorkflowRaw: %v", kind, err)
		}
		if status != http.StatusOK {
			t.Fatalf("[%s] responseTimeoutMs at the bound: expected 200, got %d; body=%s", kind, status, resp)
		}
		for _, bad := range []int{60001, -1} {
			status, resp, err := c.ImportWorkflowRaw(t, modelName, 1, build[kind](bad))
			if err != nil {
				t.Fatalf("[%s %d] ImportWorkflowRaw: %v", kind, bad, err)
			}
			if status != http.StatusBadRequest {
				t.Fatalf("[%s] responseTimeoutMs %d: expected 400, got %d; body=%s", kind, bad, status, resp)
			}
			if !containsErrorCode(resp, "VALIDATION_FAILED") {
				t.Errorf("[%s] responseTimeoutMs %d: expected errorCode VALIDATION_FAILED, body=%s", kind, bad, resp)
			}
		}
	}
}

// RunWorkflowCalloutFieldsRoundTrip: idempotent on a processor and retryPolicy
// on a schedule function are stored and exported by every backend, stamped
// with the current schema version.
func RunWorkflowCalloutFieldsRoundTrip(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)
	const modelName = "parity-callout-fields-roundtrip"
	calloutConfigModel(t, c, modelName)

	body := calloutConfigWorkflow("callout-fields-wf", "",
		`{"calculationNodesTags":"workers","idempotent":true}`,
		scheduleWithFunction(`,"retryPolicy":"NONE"`))
	if err := c.ImportWorkflow(t, modelName, 1, body); err != nil {
		t.Fatalf("ImportWorkflow: %v", err)
	}
	raw, err := c.ExportWorkflow(t, modelName, 1)
	if err != nil {
		t.Fatalf("ExportWorkflow: %v", err)
	}
	for _, want := range []string{`"idempotent":true`, `"retryPolicy":"NONE"`, `"version":"1.5"`} {
		if !bytes.Contains(compactJSON(t, raw), []byte(want)) {
			t.Errorf("export must contain %s; got: %s", want, raw)
		}
	}
}

func compactJSON(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		t.Fatalf("compact export: %v", err)
	}
	return buf.Bytes()
}
