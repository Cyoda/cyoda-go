package parity

import (
	"net/http"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// Behaviours a further compute client can be started with (see
// cmd/compute-test-client/behaviour.go). Each applies to every calculation
// request the client receives.
const (
	ComputeBehaviourCatalog       = ""               // serve the catalog, like the fixture's own client
	ComputeBehaviourStall         = "stall"          // take the work, never answer
	ComputeBehaviourFail          = "fail"           // answer success=false, no verdict
	ComputeBehaviourFailRetryable = "fail-retryable" // answer success=false, retryable=true
	ComputeBehaviourLateCallback  = "late-callback"  // never answer; call back with the pass on Release
	ComputeBehaviourDrop          = "drop"           // close the stream on receiving work
)

// ComputeClientSpec describes a further compute client for one scenario.
type ComputeClientSpec struct {
	// TenantID is the tenant the client joins under. Required. Use a FRESH
	// tenant (fixture.NewTenant) unless the scenario is about the shared one:
	// a callout with empty calculationNodesTags matches every compute client
	// of its tenant, so a misbehaving client under the shared compute tenant
	// would be handed other scenarios' work.
	TenantID string
	// Tags are the client's join tags. Required. Keep them short.
	Tags []string
	// Behaviour is one of the ComputeBehaviour* constants.
	Behaviour string
}

// ReceivedCallout is one calculation request as a compute client received it.
// The pass is never exposed — only whether one came with the work.
type ReceivedCallout struct {
	Seq         int                  `json:"seq"`  // 1-based arrival order at this client
	Kind        string               `json:"kind"` // processor | criterion | function
	Name        string               `json:"name"`
	RequestID   string               `json:"requestId"`
	EventID     string               `json:"eventId"` // the CloudEvent id
	EntityID    string               `json:"entityId"`
	PassPresent bool                 `json:"passPresent"`
	Callback    *LateCallbackOutcome `json:"callback,omitempty"`
}

// LateCallbackOutcome is what the two doors answered when a late-callback
// client presented, after Release, the pass it had been given.
type LateCallbackOutcome struct {
	HTTPStatus       int    `json:"httpStatus"`
	HTTPErrorCode    string `json:"httpErrorCode,omitempty"`
	GRPCAttempted    bool   `json:"grpcAttempted"`
	GRPCSuccess      bool   `json:"grpcSuccess"`
	GRPCErrorCode    string `json:"grpcErrorCode,omitempty"`
	GRPCErrorMessage string `json:"grpcErrorMessage,omitempty"`
	Error            string `json:"error,omitempty"` // the callback could not be made at all
}

// ComputeClient is a handle on a further compute client.
type ComputeClient interface {
	// MemberID is the id the server gave the client when it joined.
	MemberID() string
	// Received returns every calculation request the client has received, in
	// arrival order. It still works after a "drop" client closed its stream.
	Received(t *testing.T) []ReceivedCallout
	// Release tells a late-callback client to make its callbacks now, waits
	// for them, and returns the record, which then carries their outcomes.
	Release(t *testing.T) []ReceivedCallout
	// Stop ends the client's process and returns once it is reaped. The server
	// notices the closed stream a moment later, so a scenario asserting that
	// the client is gone polls for it. Calling Stop again is harmless.
	Stop()
}

// ComputeClientFixture is an OPTIONAL capability: a fixture that implements it
// can start further compute clients for one scenario. It is not part of
// BackendFixture, so an out-of-tree backend that has not wired it keeps
// compiling against the shared registry; scenarios that need it skip there.
type ComputeClientFixture interface {
	// StartComputeClient starts a compute client per spec and returns once it
	// has joined the server. Implementations MUST call t.Helper() and t.Fatal
	// on failure. In-tree fixtures delegate to
	// fixtureutil.StartComputeClientForFixture.
	StartComputeClient(t *testing.T, spec ComputeClientSpec) ComputeClient
}

func requireComputeClients(t *testing.T, fixture BackendFixture) ComputeClientFixture {
	t.Helper()
	cf, ok := fixture.(ComputeClientFixture)
	if !ok {
		t.Skip("fixture cannot start further compute clients; scenario pending on this backend")
	}
	return cf
}

// StartComputeClientOrSkip starts a further compute client for this scenario,
// or skips the scenario when the fixture lacks the capability. The client is
// stopped when the scenario ends.
func StartComputeClientOrSkip(t *testing.T, fixture BackendFixture, spec ComputeClientSpec) ComputeClient {
	t.Helper()
	cc := requireComputeClients(t, fixture).StartComputeClient(t, spec)
	t.Cleanup(cc.Stop)
	return cc
}

// AwaitReceived polls until cc has received at least n calculation requests.
func AwaitReceived(t *testing.T, cc ComputeClient, n int, within time.Duration) []ReceivedCallout {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		got := cc.Received(t)
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("compute client %s received %d requests within %s; want at least %d", cc.MemberID(), len(got), within, n)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// ComputeClientWorkflow builds a NONE -> ACTIVE workflow whose automated
// transition carries one SYNC processor routed to tag. extraConfig is merged
// into the processor's config.
func ComputeClientWorkflow(wfName, procName, tag, contextValue string, extraConfig map[string]any) string {
	cfg := map[string]any{"calculationNodesTags": tag}
	for k, v := range extraConfig {
		cfg[k] = v
	}
	return cbWorkflowDoc(wfName, map[string]any{
		"NONE": map[string]any{"transitions": []any{map[string]any{
			"name": "init", "next": "ACTIVE", "manual": false,
			"processors": []any{cbProc(procName, "SYNC", contextValue, cfg)},
		}}},
		"ACTIVE": map[string]any{},
	})
}

const computeClientEntity = `{"name":"Test","amount":10,"status":"new"}`

// RunComputeClientJoinServeLeave proves the capability end to end: before the
// client starts its tag has no compute node; once started it is given the
// work; once stopped the tag has no compute node again.
func RunComputeClientJoinServeLeave(t *testing.T, fixture BackendFixture) {
	requireComputeClients(t, fixture)
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const model, tag = "h-cc-join-leave", "h-cc-jl"
	cbSetupModel(t, c, model, cbSampleNoWriteback, ComputeClientWorkflow("h-cc-jl-wf", "noop", tag, "", nil))

	noComputeNode := func() bool {
		status, body, err := c.CreateEntityRaw(t, model, 1, computeClientEntity)
		if err != nil {
			t.Fatalf("CreateEntityRaw: %v", err)
		}
		return status == http.StatusServiceUnavailable && containsErrorCode(body, "NO_COMPUTE_MEMBER_FOR_TAG")
	}
	if !noComputeNode() {
		t.Fatal("before any client started: want 503 NO_COMPUTE_MEMBER_FOR_TAG")
	}

	cc := StartComputeClientOrSkip(t, fixture, ComputeClientSpec{TenantID: tenant.ID, Tags: []string{tag}})
	if cc.MemberID() == "" {
		t.Fatal("the started client reports no member id")
	}
	if _, err := c.CreateEntity(t, model, 1, computeClientEntity); err != nil {
		t.Fatalf("create with the client attached: %v", err)
	}
	got := cc.Received(t)
	if len(got) != 1 {
		t.Fatalf("client received %d requests; want 1: %+v", len(got), got)
	}
	if r := got[0]; r.Seq != 1 || r.Kind != "processor" || r.Name != "noop" || r.RequestID == "" || r.EventID == "" || !r.PassPresent {
		t.Errorf("record = %+v", r)
	}

	cc.Stop()
	// The server learns of the closed stream a moment after the process is
	// gone; until then a create may be routed to the dying member.
	deadline := time.Now().Add(10 * time.Second)
	for !noComputeNode() {
		if time.Now().After(deadline) {
			t.Fatal("10s after Stop the server still routes to the stopped client")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// RunComputeClientBehaviours proves each scripted behaviour reaches the server
// as intended. Every case runs under a tenant of its own.
func RunComputeClientBehaviours(t *testing.T, fixture BackendFixture) {
	requireComputeClients(t, fixture)

	cases := []struct {
		behaviour  string
		timeoutMs  int
		wantStatus int
		wantCode   string
	}{
		{ComputeBehaviourStall, 300, http.StatusServiceUnavailable, "DISPATCH_TIMEOUT"},
		{ComputeBehaviourFail, 0, http.StatusBadRequest, "WORKFLOW_FAILED"},
		{ComputeBehaviourFailRetryable, 0, http.StatusBadRequest, "WORKFLOW_FAILED"},
		{ComputeBehaviourDrop, 0, http.StatusServiceUnavailable, "COMPUTE_MEMBER_DISCONNECTED"},
		{ComputeBehaviourLateCallback, 300, http.StatusServiceUnavailable, "DISPATCH_TIMEOUT"},
	}
	for _, tc := range cases {
		t.Run(tc.behaviour, func(t *testing.T) {
			tenant := fixture.NewTenant(t)
			c := client.NewClient(fixture.BaseURL(), tenant.Token)
			model, secondary, tag := "h-cc-beh-"+tc.behaviour, "h-cc-beh-sec-"+tc.behaviour, "h-cc-"+tc.behaviour

			cbSetupModel(t, c, secondary, cbSampleSecondary, cbSecondaryWorkflow)
			var extra map[string]any
			if tc.timeoutMs > 0 {
				extra = map[string]any{"responseTimeoutMs": tc.timeoutMs}
			}
			cbSetupModel(t, c, model, cbSampleNoWriteback,
				ComputeClientWorkflow("h-cc-beh-wf", "noop", tag, cbContext(secondary, "h-cc-late"), extra))

			cc := StartComputeClientOrSkip(t, fixture, ComputeClientSpec{
				TenantID: tenant.ID, Tags: []string{tag}, Behaviour: tc.behaviour,
			})

			status, body, err := c.CreateEntityRaw(t, model, 1, computeClientEntity)
			if err != nil {
				t.Fatalf("CreateEntityRaw: %v", err)
			}
			if status != tc.wantStatus || !containsErrorCode(body, tc.wantCode) {
				t.Fatalf("got %d %s; want %d %s", status, body, tc.wantStatus, tc.wantCode)
			}
			// The record outlives the stream: a "drop" client still answers.
			if got := AwaitReceived(t, cc, 1, 5*time.Second); !got[0].PassPresent {
				t.Errorf("record = %+v; want a pass to have come with the work", got[0])
			}

			if tc.behaviour != ComputeBehaviourLateCallback {
				return
			}
			got := cc.Release(t)
			cb := got[0].Callback
			if cb == nil {
				t.Fatal("Release recorded no callback outcome")
			}
			if cb.Error != "" {
				t.Fatalf("the late callback could not be made: %s", cb.Error)
			}
			// The callout has ended and its transaction is rolled back. Both
			// doors evaluate the pass and refuse; which code they give is the
			// fencing scenarios' subject, not this one's.
			if cb.HTTPStatus == 0 || cb.HTTPStatus == http.StatusOK {
				t.Errorf("HTTP door answered %d to a pass whose callout has ended", cb.HTTPStatus)
			}
			if !cb.GRPCAttempted || cb.GRPCSuccess {
				t.Errorf("gRPC door: attempted=%t success=%t; want attempted and refused", cb.GRPCAttempted, cb.GRPCSuccess)
			}
		})
	}
}
