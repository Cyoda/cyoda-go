package entity

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// rejectUnstorablePayload rejects a decoded entity payload that no supported
// backend can persist.
//
// The only such value today is U+0000: PostgreSQL's text and jsonb types
// cannot represent a NUL, so a body whose string carries a \u0000 escape —
// syntactically valid JSON that passes schema validation — used to reach the
// store and fail there, surfacing a client input error as a 500 with a
// support ticket. It is a 400.
//
// Rejecting at the boundary rather than classifying the storage error also
// keeps the backends on one contract: memory and sqlite would otherwise accept
// a payload postgres refuses, which is a divergence, not a tolerance.
func rejectUnstorablePayload(v any) error {
	path, found := findNulString(v, "")
	if !found {
		return nil
	}
	if path == "" {
		path = "(root)"
	}
	return common.Operational(
		http.StatusBadRequest,
		common.ErrCodeBadRequest,
		fmt.Sprintf("payload field %q contains a NUL character (U+0000), which cannot be stored", path),
	)
}

// prefixItemErr restates an operational error against the collection item it
// came from, so a batch rejection says which element was at fault.
func prefixItemErr(err error, i int) error {
	appErr, ok := err.(*common.AppError)
	if !ok {
		return err
	}
	restated := common.Operational(appErr.Status, appErr.Code, fmt.Sprintf("item %d: %s", i, appErr.Message))
	restated.Props = appErr.Props
	return restated
}

// findNulString returns the JSON path of the first object key or string value
// containing U+0000. Keys are visited in sorted order so the reported path is
// deterministic for a given payload.
func findNulString(v any, path string) (string, bool) {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			child := k
			if path != "" {
				child = path + "." + k
			}
			if strings.ContainsRune(k, 0) {
				return child, true
			}
			if p, ok := findNulString(t[k], child); ok {
				return p, true
			}
		}
	case []any:
		for i, elem := range t {
			if p, ok := findNulString(elem, fmt.Sprintf("%s[%d]", path, i)); ok {
				return p, true
			}
		}
	case string:
		if strings.ContainsRune(t, 0) {
			return path, true
		}
	}
	return "", false
}
