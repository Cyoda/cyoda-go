package common

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// DecodeBoundedJSON wraps http.MaxBytesReader + json.Decoder.Decode so a
// handler never accepts an unbounded request body. Returns a non-nil error
// on an oversize body or a JSON parse failure; callers translate that to
// 400 BAD_REQUEST via WriteError.
func DecodeBoundedJSON(w http.ResponseWriter, r *http.Request, max int64, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, max)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		return fmt.Errorf("decode body: %w", err)
	}
	return nil
}
