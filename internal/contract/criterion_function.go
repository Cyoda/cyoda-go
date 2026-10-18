package contract

import (
	"encoding/json"

	"github.com/cyoda-platform/cyoda-go-spi/strictjson"
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
	// member. Empty when absent; the JSON text `null` when written as null.
	Criterion json.RawMessage `json:"criterion"`
}

// ParseCriterionFunction decodes the `function` member of a function
// condition. present reports whether that member is there and not null. It
// does not check the condition's `type`: whether a condition is a function
// condition is the caller's knowledge (predicate.ParseCondition). The
// condition is a client's typed JSON, so its member names are exact: a member
// the shape does not declare, a case variant and a duplicate are refused.
//
// A refusal is the decoder's, with its location relative to the condition.
func ParseCriterionFunction(condition json.RawMessage) (fn CriterionFunction, present bool, err *strictjson.Error) {
	var envelope struct {
		Type     string             `json:"type"`
		Function *CriterionFunction `json:"function"`
	}
	if decodeErr := strictjson.Decode(condition, &envelope); decodeErr != nil {
		return CriterionFunction{}, false, decodeErr.(*strictjson.Error)
	}
	if envelope.Function == nil {
		return CriterionFunction{}, false, nil
	}
	return *envelope.Function, true, nil
}
