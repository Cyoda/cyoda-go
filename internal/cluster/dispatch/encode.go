package dispatch

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// EncodePeerBody is how every node-to-node body is encoded, in both directions
// and on both routes: JSON with HTML escaping OFF.
//
// json.Marshal escapes "<", ">" and "&" wherever they appear — six bytes each.
// None of that is needed between two of this cluster's own nodes: the body is
// sealed, not embedded in a web page. Left on, it multiplies a hand-over
// carrying HTML or XML text by up to six, which the envelope ceiling then
// refuses (the ceiling is derived without it: see MaxEnvelopeSize). The entity's
// own bytes are unaffected either way, being base64 on the wire; what this saves
// is every other field a tenant's text can reach — a criterion, a function's
// result, a compute member's message.
//
// U+2028 and U+2029 inside a JSON string stay escaped whatever this setting
// says: encoding/json escapes those two unconditionally. They cost three bytes
// each and cannot reach the entity's payload, so nothing here depends on them.
//
// Exported because the scheduler's peer RPC encodes through it too: one form on
// the wire, both routes.
func EncodePeerBody(v any) ([]byte, error) {
	return encodePeerBody(v)
}

func encodePeerBody(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("failed to encode a peer body: %w", err)
	}
	return buf.Bytes(), nil
}
