package events_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/api/grpc/events"
)

// A required member whose schema states its type cannot be null, so a null
// there is an invalid instance, as an absent member is. go-jsonschema checks
// only that the key is present: a null passed and decoded as the zero value, so
// `"uniqueKeys": null` read as "no keys" and removed every key of the model.
// scripts/generate-events.sh rewrites those required checks; this test is what
// says the rewrite is still there, on a required array, string and object.
func TestRequiredMember_ExplicitNullIsRefused(t *testing.T) {
	const mdl = `"model":{"name":"m","version":1}`
	const keys = `"uniqueKeys":[]`
	for name, payload := range map[string]string{
		"array":  `{"id":"e1",` + mdl + `,"uniqueKeys":null}`,
		"string": `{"id":null,` + mdl + `,` + keys + `}`,
		"object": `{"id":"e1","model":null,` + keys + `}`,
	} {
		var req events.EntityModelSetUniqueKeysRequestJson
		err := json.Unmarshal([]byte(payload), &req)
		if err == nil {
			t.Errorf("%s: a null required member decoded: %+v", name, req)
			continue
		}
		if !strings.Contains(err.Error(), "must not be null") {
			t.Errorf("%s: error %q does not say the member must not be null", name, err)
		}
		if strings.Contains(err.Error(), "Json") {
			t.Errorf("%s: error %q names a Go type", name, err)
		}
	}
}

// An empty array is a value, not an absence: it still decodes, and is what a
// client sends to remove every key.
func TestRequiredMember_EmptyArrayIsAValue(t *testing.T) {
	var req events.EntityModelSetUniqueKeysRequestJson
	if err := json.Unmarshal([]byte(`{"id":"e1","model":{"name":"m","version":1},"uniqueKeys":[]}`), &req); err != nil {
		t.Fatalf("an empty uniqueKeys array must decode: %v", err)
	}
	if req.UniqueKeys == nil || len(req.UniqueKeys) != 0 {
		t.Errorf("uniqueKeys = %#v; want an empty, non-nil slice", req.UniqueKeys)
	}
}

// A required member whose schema states no type holds any JSON value, null
// included: the entity data, the patch and the model-import sample are the
// client's data, and the domain refuses a null there with its own code
// (VALIDATION_FAILED, as on HTTP). The decode leaves those to it.
func TestRequiredMember_AnyValueMemberMayBeNull(t *testing.T) {
	for name, decode := range map[string]func() error{
		"create data": func() error {
			var v events.EntityCreatePayloadJson
			return json.Unmarshal([]byte(`{"model":{"name":"m","version":1},"data":null}`), &v)
		},
		"update data": func() error {
			var v events.EntityUpdatePayloadJson
			return json.Unmarshal([]byte(`{"entityId":"e","data":null}`), &v)
		},
		"patch": func() error {
			var v events.EntityPatchPayloadJson
			return json.Unmarshal([]byte(`{"entityId":"e","ifMatch":"*","patch":null}`), &v)
		},
		"import payload": func() error {
			var v events.EntityModelImportRequestJson
			return json.Unmarshal([]byte(`{"id":"e","model":{"name":"m","version":1},"dataFormat":"JSON","converter":"SAMPLE_DATA","payload":null}`), &v)
		},
	} {
		if err := decode(); err != nil {
			t.Errorf("%s: a null the schema admits was refused at decode: %v", name, err)
		}
	}
}
