package postgres

import (
	"strings"
	"testing"
)

// TestEntityDoc_JSONNullDataIsRejectedNotPanic pins the handling of an entity
// whose Data is the JSON literal `null`.
//
// This is reachable from the processor return path: a compute node's response
// carries `data` as a json.RawMessage, so a processor returning {"data":null}
// yields Data = []byte("null"). That is non-empty, so it passes the
// len(entity.Data) == 0 branch above, and json.Unmarshal succeeds — into a NIL
// map. Assigning _meta into a nil map panics.
//
// The panic is only recovered by the HTTP middleware three packages up, and it
// unwinds past the explicit (non-deferred) h.rollbackOwned call on the save
// error path in internal/domain/entity/service.go, so the transaction is
// neither committed nor rolled back and its pooled connection is never
// returned. Repeated, that exhausts the pool and the node stops serving.
//
// marshalEntityDoc must therefore return an error, letting the caller's normal
// error path run.
func TestEntityDoc_JSONNullDataIsRejectedNotPanic(t *testing.T) {
	ent := testEntity()
	ent.Data = []byte("null")

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("marshalEntityDoc panicked on Data=null (%v); it must return an error so the caller can roll back", r)
		}
	}()

	_, err := marshalEntityDoc(ent, testTime, testTime, testTime, false)
	if err == nil {
		t.Fatal("marshalEntityDoc accepted Data=null; want an error naming the problem")
	}
	if !strings.Contains(err.Error(), "object") {
		t.Errorf("error %q does not explain that entity data must be a JSON object", err)
	}
}

// TestEntityDoc_NonObjectDataIsRejected covers the sibling shapes. A top-level
// array, string or number is not a valid entity document either; those already
// fail to unmarshal into a map, so this only guards against a regression that
// starts accepting them.
func TestEntityDoc_NonObjectDataIsRejected(t *testing.T) {
	for _, data := range []string{`[1,2]`, `"hello"`, `42`, `true`} {
		ent := testEntity()
		ent.Data = []byte(data)

		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("marshalEntityDoc panicked on Data=%s: %v", data, r)
				}
			}()
			if _, err := marshalEntityDoc(ent, testTime, testTime, testTime, false); err == nil {
				t.Errorf("marshalEntityDoc accepted non-object Data=%s; want an error", data)
			}
		}()
	}
}
