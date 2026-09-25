package scheduledtask

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// cursorBody is the decoded form of a page cursor: the (scheduledTime, id)
// sort key of the last task on the previous page. Opaque to callers.
type cursorBody struct {
	V int    `json:"v"`
	T int64  `json:"t"`
	I string `json:"i"`
}

// errBadCursor is returned for any cursor that does not decode to a valid
// position. The handler answers 400 BAD_REQUEST without echoing the cursor.
var errBadCursor = errors.New("invalid cursor")

// maxCursorLen bounds the cursor string, checked before any decoding. The
// longest real cursor (most negative time, 32-character task id) is under
// 100 characters.
const maxCursorLen = 256

// encodeCursor renders c as an opaque cursor: a position in the
// (scheduledTime, taskId) order, not an offset into one query's results.
func encodeCursor(c spi.ScheduledTaskCursor) string {
	raw, _ := json.Marshal(cursorBody{V: 1, T: c.ScheduledTime, I: c.ID}) // fixed struct of ints and a string; cannot fail
	return base64.RawURLEncoding.EncodeToString(raw)
}

// decodeCursor parses a cursor back into the position it encodes. A string is
// accepted only if it is exactly what encodeCursor produces for that position,
// so every other spelling — whitespace, key order, trailing data, a leading
// zero, padding — is rejected by one check rather than a list of shapes.
func decodeCursor(s string) (spi.ScheduledTaskCursor, error) {
	if s == "" || len(s) > maxCursorLen {
		return spi.ScheduledTaskCursor{}, errBadCursor
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return spi.ScheduledTaskCursor{}, errBadCursor
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var b cursorBody
	if err := dec.Decode(&b); err != nil || b.V != 1 || b.I == "" {
		return spi.ScheduledTaskCursor{}, errBadCursor
	}
	c := spi.ScheduledTaskCursor{ScheduledTime: b.T, ID: b.I}
	if encodeCursor(c) != s {
		return spi.ScheduledTaskCursor{}, errBadCursor
	}
	return c, nil
}
