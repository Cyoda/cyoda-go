package dispatch

import (
	"encoding/json"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// DispatchProcessorRequest is the cross-node payload for processor execution.
type DispatchProcessorRequest struct {
	Entity         json.RawMessage         `json:"entity"`
	EntityMeta     spi.EntityMeta          `json:"entityMeta"`
	Processor      spi.ProcessorDefinition `json:"processor"`
	WorkflowName   string                  `json:"workflowName"`
	TransitionName string                  `json:"transitionName"`
	TxID           string                  `json:"txID"`
	TenantID       string                  `json:"tenantID"`
	Tags           string                  `json:"tags"`
	UserID         string                  `json:"userID"`
	Roles          []string                `json:"roles"`
	TxToken        string                  `json:"txToken,omitempty"`
}

// DispatchProcessorResponse is the cross-node result for processor execution.
type DispatchProcessorResponse struct {
	EntityData json.RawMessage `json:"entityData,omitempty"`
	Success    bool            `json:"success"`
	Error      string          `json:"error,omitempty"`
	Warnings   []string        `json:"warnings,omitempty"`
}

// DispatchCriteriaRequest is the cross-node payload for criteria evaluation.
type DispatchCriteriaRequest struct {
	Entity         json.RawMessage `json:"entity"`
	EntityMeta     spi.EntityMeta  `json:"entityMeta"`
	Criterion      json.RawMessage `json:"criterion"`
	Target         string          `json:"target"`
	WorkflowName   string          `json:"workflowName"`
	TransitionName string          `json:"transitionName"`
	ProcessorName  string          `json:"processorName"`
	TxID           string          `json:"txID"`
	TenantID       string          `json:"tenantID"`
	Tags           string          `json:"tags"`
	UserID         string          `json:"userID"`
	Roles          []string        `json:"roles"`
	TxToken        string          `json:"txToken,omitempty"`
}

// DispatchCriteriaResponse is the cross-node result for criteria evaluation.
type DispatchCriteriaResponse struct {
	Matches  bool     `json:"matches"`
	Success  bool     `json:"success"`
	Error    string   `json:"error,omitempty"`
	Reason   string   `json:"reason,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}
