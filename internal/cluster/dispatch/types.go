package dispatch

import (
	"encoding/json"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// DispatchCalloutRequest is the cross-node payload for a callout dispatched
// to a peer node. Kind discriminates the callout shape and selects which of
// the union fields below are populated:
//   - Kind == "processor": Processor is set.
//   - Kind == "criteria": Criterion, Target, and ProcessorName are set.
//   - Kind == "function": Function is set.
//
// The remaining fields are shared across every callout kind.
type DispatchCalloutRequest struct {
	Kind string `json:"kind"`

	Entity         json.RawMessage `json:"entity"`
	EntityMeta     spi.EntityMeta  `json:"entityMeta"`
	WorkflowName   string          `json:"workflowName"`
	TransitionName string          `json:"transitionName"`
	TxID           string          `json:"txID"`
	TenantID       string          `json:"tenantID"`
	Tags           string          `json:"tags"`
	UserID         string          `json:"userID"`
	// PrincipalKind is the originating principal's explicit kind
	// (spi.PrincipalUser/Service/System). The peer reconstructs a
	// UserContext from this request (handler.go buildContext) and
	// AttachAuthContext fails the dispatch closed if Kind is unset or
	// outside {user,service,system} — see internal/grpc/cloudevent.go.
	PrincipalKind spi.PrincipalKind `json:"principalKind"`
	Roles         []string          `json:"roles"`

	// RequestID is created by the owner and sent on every try, by every pnode.
	RequestID string `json:"requestID"`
	// TriesLeft is the most tries the receiving pnode may make; at least 1.
	TriesLeft int `json:"triesLeft"`
	// AnswerLimitMs is the answer limit as the owner resolved it, so that two
	// pnodes cannot disagree about it.
	AnswerLimitMs int64 `json:"answerLimitMs"`
	// OwnerNodeID is the pnode that holds the transaction; the receiving pnode
	// puts it in every pass it mints, so that callbacks are routed there.
	OwnerNodeID string `json:"ownerNodeID"`
	// Major is the fencing number of this hand-over. The receiving pnode
	// numbers its tries minor = 1, 2, … under it.
	Major uint32 `json:"major"`
	// Outer names the enclosing callouts, copied into every pass minted; empty
	// unless the callout was made from inside a callback.
	Outer []WirePair `json:"outer,omitempty"`
	// RepeatSafe is the owner's decision whether the work may be given to
	// another cnode after a hand-off.
	RepeatSafe bool `json:"repeatSafe"`

	TxToken string `json:"txToken,omitempty"`

	// Processor is set when Kind == "processor".
	Processor *spi.ProcessorDefinition `json:"processor,omitempty"`

	// Criterion, Target, and ProcessorName are set when Kind == "criteria".
	Criterion     json.RawMessage `json:"criterion,omitempty"`
	Target        string          `json:"target,omitempty"`
	ProcessorName string          `json:"processorName,omitempty"`

	// Function is set when Kind == "function".
	Function *spi.ScheduleFunction `json:"function,omitempty"`
}

// DispatchCalloutResponse is the cross-node result for a callout dispatched
// to a peer node — a union mirroring DispatchCalloutRequest's Kind:
//   - Kind == "processor": EntityData is populated.
//   - Kind == "criteria": Matches and Reason are populated.
//   - Kind == "function": Result and ResultKind are populated.
type DispatchCalloutResponse struct {
	// Outcome is "ok", or the kind of the failure: "no_handoff", "no_answer",
	// "member_failed", "terminal". Only an authenticated "no_handoff" tells the
	// owner that nothing was handed to a cnode.
	Outcome string `json:"outcome"`
	// TriesUsed is the number of tries made. A pointer, because absent (read
	// as one try) and zero (nothing was tried) are different answers.
	TriesUsed *int `json:"triesUsed,omitempty"`
	// Attempts is one entry per failed try.
	Attempts []WireAttempt `json:"attempts,omitempty"`
	// MemberError and MemberRetryable are the cnode's own message and verdict
	// ("member_failed" only).
	MemberError     string `json:"memberError,omitempty"`
	MemberRetryable *bool  `json:"memberRetryable,omitempty"`

	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`

	// ErrorCode, ErrorStatus, and ErrorRetryable classify a failed local
	// dispatch (Success == false) using the same taxonomy single-node
	// dispatch uses (*common.AppError's Code/Status/Retryable, or the
	// NO_COMPUTE_MEMBER_FOR_TAG/503/retryable trio for
	// contract.ErrNoMatchingMember). The forwarding node re-mints an
	// *common.AppError from this trio instead of collapsing every peer
	// failure into a generic 400 WORKFLOW_FAILED — see B1 in the
	// scheduled-transition-function final review. Error stays the
	// sanitized human-readable message; ErrorCode == "" means the peer
	// predates this classification (or the failure wasn't classifiable),
	// so the forwarding node falls back to the historical plain error.
	ErrorCode      string `json:"errorCode,omitempty"`
	ErrorStatus    int    `json:"errorStatus,omitempty"`
	ErrorRetryable bool   `json:"errorRetryable,omitempty"`

	// EntityData is populated for a processor callout response.
	EntityData []byte `json:"entityData,omitempty"`

	// Matches and Reason are populated for a criteria callout response.
	Matches *bool  `json:"matches,omitempty"`
	Reason  string `json:"reason,omitempty"`

	// Result and ResultKind are populated for a function callout response.
	Result     json.RawMessage `json:"result,omitempty"`
	ResultKind string          `json:"resultKind,omitempty"`

	Warnings []string `json:"warnings,omitempty"`
	// Errors are the diagnostics the tries added on the answering pnode.
	Errors []string `json:"errors,omitempty"`
}

// OutcomeOK is the outcome of a hand-over that a cnode answered. The other
// outcomes are the words of contract.CalloutFailureKind.
const OutcomeOK = "ok"

// WirePair is one (callout, major, minor) on the wire.
type WirePair struct {
	Callout string `json:"callout"`
	Major   uint32 `json:"major"`
	Minor   uint32 `json:"minor"`
}

// WireAttempt is one failed try on the wire.
type WireAttempt struct {
	MemberID string `json:"memberID"`
	Kind     string `json:"kind"`
	Cause    string `json:"cause"`
}
