package entity

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// TestRejectUnstorablePayload pins the boundary rule for U+0000. PostgreSQL
// text/jsonb cannot represent a NUL, so a payload carrying one is a client
// input error the boundary must reject — not a storage failure that surfaces
// as a 500 with a support ticket. Rejecting it here also keeps memory and
// sqlite, which would happily accept it, on the same contract as postgres.
func TestRejectUnstorablePayload(t *testing.T) {
	const nul = "\x00"

	cases := []struct {
		name     string
		payload  any
		wantPath string // "" means the payload must be accepted
	}{
		{"plain object", map[string]any{"name": "ok", "amount": "1"}, ""},
		{"empty object", map[string]any{}, ""},
		{"nil", nil, ""},
		{"other control chars are storable", map[string]any{"name": "a\tb\nc"}, ""},
		{"escaped-looking text is not a NUL", map[string]any{"name": "a\\u0000b"}, ""},

		{"nul in a top-level string", map[string]any{"name": "a" + nul + "b"}, "name"},
		{"nul in a nested string", map[string]any{
			"outer": map[string]any{"inner": nul},
		}, "outer.inner"},
		{"nul in an array element", map[string]any{
			"tags": []any{"ok", "bad" + nul},
		}, "tags[1]"},
		{"nul in a nested array of objects", map[string]any{
			"items": []any{
				map[string]any{"sku": "ok"},
				map[string]any{"sku": nul + "x"},
			},
		}, "items[1].sku"},
		{"nul in an object key", map[string]any{"bad" + nul + "key": "v"}, "bad" + nul + "key"},
		{"bare string payload", "a" + nul, "(root)"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := rejectUnstorablePayload(tc.payload)

			if tc.wantPath == "" {
				if err != nil {
					t.Fatalf("payload rejected but should be accepted: %v", err)
				}
				return
			}

			if err == nil {
				t.Fatalf("payload accepted but should be rejected (expected path %q)", tc.wantPath)
			}
			appErr, ok := err.(*common.AppError)
			if !ok {
				t.Fatalf("error is %T, want *common.AppError", err)
			}
			if appErr.Status != http.StatusBadRequest {
				t.Errorf("status=%d, want 400", appErr.Status)
			}
			if appErr.Code != common.ErrCodeBadRequest {
				t.Errorf("code=%q, want %q", appErr.Code, common.ErrCodeBadRequest)
			}
			// The path is rendered with %q so a control byte in a key is never
			// echoed raw into the response body (output sanitization).
			if want := strconv.Quote(tc.wantPath); !strings.Contains(appErr.Message, want) {
				t.Errorf("message %q does not name the offending path %s", appErr.Message, want)
			}
		})
	}
}
