package dispatch

import (
	"encoding/json"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// DispatchCalloutRequest is the cross-node payload for a callout dispatched
// to a peer node. Kind discriminates the callout shape ("processor" or
// "criteria" today; "function" is added in a later task) and selects which
// of the union fields below are populated:
//   - Kind == "processor": Processor is set.
//   - Kind == "criteria": Criterion, Target, and ProcessorName are set.
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
	Roles          []string        `json:"roles"`
	TxToken        string          `json:"txToken,omitempty"`

	// Processor is set when Kind == "processor".
	Processor *spi.ProcessorDefinition `json:"processor,omitempty"`

	// Criterion, Target, and ProcessorName are set when Kind == "criteria".
	Criterion     json.RawMessage `json:"criterion,omitempty"`
	Target        string          `json:"target,omitempty"`
	ProcessorName string          `json:"processorName,omitempty"`
}

// DispatchCalloutResponse is the cross-node result for a callout dispatched
// to a peer node — a union mirroring DispatchCalloutRequest's Kind:
//   - Kind == "processor": EntityData is populated.
//   - Kind == "criteria": Matches and Reason are populated.
type DispatchCalloutResponse struct {
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`

	// EntityData is populated for a processor callout response.
	EntityData []byte `json:"entityData,omitempty"`

	// Matches and Reason are populated for a criteria callout response.
	Matches *bool  `json:"matches,omitempty"`
	Reason  string `json:"reason,omitempty"`

	Warnings []string `json:"warnings,omitempty"`
}
