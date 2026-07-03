package openapivalidator

import "encoding/json"

// ErrorTriple identifies one observed (operationId, status, errorCode)
// combination. ErrorCode is the value at properties.errorCode of a
// ProblemDetail body; empty for success responses or bodies without it.
type ErrorTriple struct {
	Operation string
	Status    int
	ErrorCode string
}

// extractErrorCode reads properties.errorCode from a ProblemDetail JSON body.
// Returns "" when the body is not JSON, has no properties object, or carries no
// errorCode string. Mirrors internal/common.ProblemDetail, where the code is
// nested under "properties" (see internal/common/errors.go WriteError).
func extractErrorCode(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var pd struct {
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(body, &pd); err != nil {
		return ""
	}
	if code, ok := pd.Properties["errorCode"].(string); ok {
		return code
	}
	return ""
}

func (c *collector) recordErrorCode(operationID string, status int, code string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.errorTriples[ErrorTriple{Operation: operationID, Status: status, ErrorCode: code}] = struct{}{}
}

func (c *collector) observedErrorTriples() []ErrorTriple {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]ErrorTriple, 0, len(c.errorTriples))
	for tr := range c.errorTriples {
		out = append(out, tr)
	}
	return out
}

// ObservedErrorTriples returns the (operationId, status, errorCode) triples
// observed across the run. Read at suite end by the error-code matrix test.
func ObservedErrorTriples() []ErrorTriple { return defaultCollector.observedErrorTriples() }
