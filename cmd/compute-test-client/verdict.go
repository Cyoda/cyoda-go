package main

import "errors"

// verdictError fails a callout and carries this compute node's own verdict on
// whether the failure is worth retrying. A catalog entry that returns any
// other error fails the callout with no verdict.
type verdictError struct {
	msg       string
	retryable bool
}

func (e *verdictError) Error() string { return e.msg }

// verdictOf returns the verdict err carries, or nil when it carries none.
func verdictOf(err error) *bool {
	var ve *verdictError
	if errors.As(err, &ve) {
		v := ve.retryable
		return &v
	}
	return nil
}

// errorNode builds the "error" object of a failed calculation response.
func errorNode(code, msg string, retryable *bool) map[string]any {
	node := map[string]any{"code": code, "message": msg}
	if retryable != nil {
		node["retryable"] = *retryable
	}
	return node
}
