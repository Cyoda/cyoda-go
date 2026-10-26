package entity

// applyMergePatch is the RFC 7386 section 2 algorithm. When patch is not a
// JSON object it replaces the target wholesale; otherwise each key is merged
// recursively, and an explicit null deletes the key.
//
// Mutates targetObj in place; safe because updateEntityCore always passes the
// stored data freshly decoded, a local value never aliased back into the store.
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
