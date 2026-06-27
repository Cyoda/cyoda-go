package entity

import (
	"encoding/json"
	"fmt"
)

// mergeMergePatch applies an RFC 7386 JSON Merge Patch (the already-parsed
// patch) onto the stored entity data, returning the merged value. Both sides
// are number-preserving (json.Number) so large integers survive the merge.
func mergeMergePatch(existing json.RawMessage, patch any) (any, error) {
	var target any
	if len(existing) > 0 {
		if err := decodeJSONPreservingNumbers(existing, &target); err != nil {
			return nil, fmt.Errorf("failed to decode stored entity data: %w", err)
		}
	}
	return applyMergePatch(target, patch), nil
}

// applyMergePatch is the RFC 7386 section 2 algorithm. When patch is not a
// JSON object it replaces the target wholesale; otherwise each key is merged
// recursively, and an explicit null deletes the key.
//
// Mutates targetObj in place; safe because mergeMergePatch always passes a
// freshly-decoded, decode-local map that is never aliased back into the store.
func applyMergePatch(target, patch any) any {
	patchObj, ok := patch.(map[string]any)
	if !ok {
		return patch
	}
	targetObj, ok := target.(map[string]any)
	if !ok {
		targetObj = map[string]any{}
	}
	for k, v := range patchObj {
		if v == nil {
			delete(targetObj, k)
		} else {
			targetObj[k] = applyMergePatch(targetObj[k], v)
		}
	}
	return targetObj
}
