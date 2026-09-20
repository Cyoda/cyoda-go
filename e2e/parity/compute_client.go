package parity

import "testing"

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
