package audit

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

// cursorBody is the decoded form of an audit page cursor: the sort key of
// the last event on the previous page. Opaque to callers.
type cursorBody struct {
	V int     `json:"v"`
	T string  `json:"t"`
	K string  `json:"k"`
	N *int64  `json:"n,omitempty"`
	E *string `json:"e,omitempty"`
}

// errBadCursor is returned for any cursor that does not decode to a valid
// eventKey. The handler maps it to 400 BAD_REQUEST without echoing the
// cursor value back to the caller.
var errBadCursor = errors.New("invalid cursor")

// encodeCursor renders k as an opaque page cursor: a position in the total
// order (compareKeys), not an offset into any one query's result set.
func encodeCursor(k eventKey) string {
	b := cursorBody{V: 1, T: k.at.UTC().Format(time.RFC3339Nano), K: k.kind}
	if k.kind == "EntityChange" {
		n := k.version
		b.N = &n
	} else {
		e := k.eventID.String()
		b.E = &e
	}
	raw, _ := json.Marshal(b) // fixed struct of strings and ints; cannot fail
	return base64.RawURLEncoding.EncodeToString(raw)
}

// decodeCursor parses a page cursor back into the eventKey it encodes.
// Anything that does not round-trip to a valid key — including the old
// integer-offset cursor format, malformed JSON, an unknown field, a
// version/eventID mismatched to the wrong kind, or a version below 1 — is
// rejected as errBadCursor.
func decodeCursor(s string) (eventKey, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return eventKey{}, errBadCursor
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var b cursorBody
	if err := dec.Decode(&b); err != nil || b.V != 1 {
		return eventKey{}, errBadCursor
	}
	at, err := time.Parse(time.RFC3339Nano, b.T)
	if err != nil {
		return eventKey{}, errBadCursor
	}
	switch b.K {
	case "EntityChange":
		if b.N == nil || *b.N < 1 || b.E != nil {
			return eventKey{}, errBadCursor
		}
		return eventKey{at: at, kind: b.K, version: *b.N}, nil
	case "StateMachine":
		if b.E == nil || b.N != nil {
			return eventKey{}, errBadCursor
		}
		id, err := uuid.Parse(*b.E)
		if err != nil {
			return eventKey{}, errBadCursor
		}
		return eventKey{at: at, kind: b.K, eventID: id}, nil
	}
	return eventKey{}, errBadCursor
}
