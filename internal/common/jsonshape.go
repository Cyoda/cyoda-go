package common

import (
	"encoding/json"
	"errors"
	"fmt"
)

// JSONErrorShape renders a decode/encode error for the server log without the
// value it failed on: json.SyntaxError and json.UnmarshalTypeError can quote a
// fragment of the payload — a tenant's entity data, a compute member's response,
// another node's hand-over body — that a log line must never carry. It gives
// the error's Go type and, where present, the byte offset (SyntaxError) or the
// struct field named (UnmarshalTypeError; the Struct and Field names come from
// the caller's own fixed request/response shapes, never from payload content).
//
// It lives here because every boundary that decodes a payload written by
// someone else needs the same spelling: the compute-member legs in
// internal/grpc, and the node-to-node hand-over in internal/cluster/dispatch.
func JSONErrorShape(err error) string {
	var syn *json.SyntaxError
	if errors.As(err, &syn) {
		return fmt.Sprintf("%T at offset %d", syn, syn.Offset)
	}
	var ute *json.UnmarshalTypeError
	if errors.As(err, &ute) {
		return fmt.Sprintf("%T (struct %s field %s)", ute, ute.Struct, ute.Field)
	}
	return fmt.Sprintf("%T", err)
}
