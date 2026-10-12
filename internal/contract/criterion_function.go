package contract

import (
	"encoding/json"
	"fmt"
)

// CriterionFunctionConfig is the callout configuration of a function
// criterion: {"type":"function","function":{"name":…,"config":{…}}}. The SPI
// stores a criterion as raw JSON, so this is the one typed view of it, shared
// by workflow import (which validates it) and by dispatch (which acts on it).
type CriterionFunctionConfig struct {
	CalculationNodesTags string `json:"calculationNodesTags"`
	// AttachEntity is nil when the author did not set it; the default is true.
	AttachEntity      *bool  `json:"attachEntity"`
	ResponseTimeoutMs int64  `json:"responseTimeoutMs"`
	RetryPolicy       string `json:"retryPolicy"`
	// Context is a pass-through string surfaced verbatim in the request's
	// parameters node.
	Context string `json:"context"`
}

// CriterionFunction is the `function` member of a function condition.
type CriterionFunction struct {
	Name   string                  `json:"name"`
	Config CriterionFunctionConfig `json:"config"`
	// Criterion is the pre-check as written: the `function.criterion`
	// member. Empty when absent.
	Criterion json.RawMessage `json:"criterion"`
}

// ParseCriterionFunction decodes the `function` member of a function
// condition. present reports whether that member is there and not null. It
// does not check the condition's `type`: whether a condition is a function
// condition is the caller's knowledge (predicate.ParseCondition). Members it
// does not name are ignored — the criterion is client-owned JSON.
func ParseCriterionFunction(condition json.RawMessage) (fn CriterionFunction, present bool, err error) {
	var envelope struct {
		Function *CriterionFunction `json:"function"`
	}
	if err := json.Unmarshal(condition, &envelope); err != nil {
		return CriterionFunction{}, false, fmt.Errorf("failed to parse criterion function: %w", err)
	}
	if envelope.Function == nil {
		return CriterionFunction{}, false, nil
	}
	return *envelope.Function, true, nil
}
