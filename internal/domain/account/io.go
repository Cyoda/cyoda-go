package account

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// boundedJSONDecode wraps http.MaxBytesReader + json.Decoder.Decode.
// Returns non-nil error on oversize body or JSON parse failure. Callers
// translate to 400 BAD_REQUEST via common.WriteError.
//
// All 4 POST /oauth/keys/* adapters use this helper with max = 1<<20 (1 MiB).
func boundedJSONDecode(w http.ResponseWriter, r *http.Request, max int64, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, max)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("decode body: %w", err)
	}
	return nil
}

// BoundedJSONDecodeForTesting exposes boundedJSONDecode for external tests.
func BoundedJSONDecodeForTesting(w http.ResponseWriter, r *http.Request, max int64, dst any) error {
	return boundedJSONDecode(w, r, max, dst)
}
