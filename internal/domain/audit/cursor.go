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

// maxCursorLen bounds the length of a cursor string accepted by
// decodeCursor, checked before any base64/JSON decoding is attempted. The
// largest canonical cursor — a StateMachine key with a 9-digit-nanosecond
// RFC3339Nano timestamp and a 36-char UUID — is well under 200 chars; 256
// leaves headroom without relying solely on net/http's 1 MiB header limit
// to bound an oversized value.
const maxCursorLen = 256

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
//
// One spelling per position: a string is only accepted if it is exactly
// what encodeCursor produces for the key it decodes to
// (encodeCursor(k) == s). That single check — rather than an enumerated
// list of malformed shapes — is what rejects trailing data after the JSON
// object, whitespace, duplicate or non-canonically-cased keys, a non-UTC
// time offset, and a non-canonical UUID spelling (braces, urn:, no
// hyphens): each of those decodes to a valid key but re-encodes to a
// different string than the one the caller sent.
func decodeCursor(s string) (eventKey, error) {
	if len(s) > maxCursorLen {
		return eventKey{}, errBadCursor
	}
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
	var k eventKey
	switch b.K {
	case "EntityChange":
		if b.N == nil || *b.N < 1 || b.E != nil {
			return eventKey{}, errBadCursor
		}
		k = eventKey{at: at, kind: b.K, version: *b.N}
	case "StateMachine":
		if b.E == nil || b.N != nil {
			return eventKey{}, errBadCursor
		}
		id, err := uuid.Parse(*b.E)
		if err != nil {
			return eventKey{}, errBadCursor
		}
		// The nil UUID is not just a non-canonical spelling to catch via
		// the round-trip check below — a store never assigns it
		// (stateMachineItem rejects it on the write side too), so it is
		// rejected outright rather than relying on a spelling that
		// happens to also round-trip.
		if id == uuid.Nil {
			return eventKey{}, errBadCursor
		}
		k = eventKey{at: at, kind: b.K, eventID: id}
	default:
		return eventKey{}, errBadCursor
	}
	if encodeCursor(k) != s {
		return eventKey{}, errBadCursor
	}
	return k, nil
}
