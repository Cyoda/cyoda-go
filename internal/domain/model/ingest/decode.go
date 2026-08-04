package ingest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// DecodeJSONPreservingNumbers is the precision-preserving counterpart to
// json.Unmarshal: numeric leaves arrive as json.Number rather than float64,
// so callers can choose Int64()/Float64()/string preservation. Mirrors
// importer.ParseJSON's UseNumber() behavior.
//
// Like json.Unmarshal (and unlike a bare Decoder.Decode), data must hold
// exactly one JSON value. Decode alone stops at the end of the first value and
// silently ignores the rest, so a body such as `{"x":1}}}` would parse as
// `{"x":1}` while the original bytes — still malformed — went on to be
// persisted, turning a client input error into a 500 from storage.
func DecodeJSONPreservingNumbers(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return fmt.Errorf("unexpected content after top-level JSON value")
	}
	return nil
}
