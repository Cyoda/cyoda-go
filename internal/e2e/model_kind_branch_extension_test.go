package e2e_test

import (
	"net/http"
	"strings"
	"testing"
)

// created reports whether an entity-write status is a success.
func created(status int) bool {
	return status == http.StatusCreated || status == http.StatusOK
}

// A model whose field declares BOTH a scalar and an array — established by a
// sample-data collection import — admits either kind with a changeLevel set.
//
// This is the limitation that made such a model unusable: the extension path
// compared one kind per path, so it refused a write matching a declared branch
// whenever that branch was not the one the node's label happened to name, and
// the message told the client to send the declared kind — which is what the
// client had sent.
func TestModelKindBranch_DeclaredBranchesAcceptedAtEveryLevel(t *testing.T) {
	const model = "e2e-kind-branch-declared"
	importModelSampleE2E(t, model, 1, `[{"poly":"x"},{"poly":["a"]}]`)
	lockModelE2E(t, model, 1)

	// Both branches are named by the export, so both are declared.
	root := rootBucketE2E(t, model, 1)
	for _, key := range []string{".poly", ".poly[*]"} {
		if _, ok := root[key]; !ok {
			t.Fatalf("precondition: export must name %q; bucket: %v", key, root)
		}
	}

	for _, level := range []string{"ARRAY_LENGTH", "ARRAY_ELEMENTS", "TYPE", "STRUCTURAL"} {
		t.Run(level, func(t *testing.T) {
			setChangeLevelE2E(t, model, 1, level)
			for _, payload := range []string{`{"poly":"scalar"}`, `{"poly":["one","two"]}`} {
				status, body := createEntityRawE2E(t, model, 1, payload)
				if !created(status) {
					t.Errorf("%s: status = %d, want 2xx; body: %s", payload, status, body)
				}
			}
		})
	}
}

// A kind OUTSIDE the declared set is still a schema change, so it follows the
// level: refused below STRUCTURAL, accepted at it.
func TestModelKindBranch_AThirdKindFollowsTheLevel(t *testing.T) {
	const model = "e2e-kind-branch-third"
	importModelSampleE2E(t, model, 1, `[{"poly":"x"},{"poly":["a"]}]`)
	lockModelE2E(t, model, 1)

	setChangeLevelE2E(t, model, 1, "TYPE")
	status, body := createEntityRawE2E(t, model, 1, `{"poly":{"k":"v"}}`)
	if status != http.StatusBadRequest {
		t.Fatalf("a third kind at TYPE: status = %d, want 400; body: %s", status, body)
	}
	if !strings.Contains(body, "STRUCTURAL") {
		t.Errorf("the rejection must name the level that resolves it; body: %s", body)
	}

	setChangeLevelE2E(t, model, 1, "STRUCTURAL")
	status, body = createEntityRawE2E(t, model, 1, `{"poly":{"k":"v"}}`)
	if !created(status) {
		t.Fatalf("a third kind at STRUCTURAL: status = %d, want 2xx; body: %s", status, body)
	}

	root := rootBucketE2E(t, model, 1)
	for _, key := range []string{".poly", ".poly[*]", "#.poly"} {
		if _, ok := root[key]; !ok {
			t.Errorf("export must name the %q branch; bucket: %v", key, root)
		}
	}
}

// A field observed only as null, later holding a container. The extension
// accepted this and the delta computation could not express it, so it reached
// the client as a 500.
func TestModelKindBranch_NullOnlyFieldLaterHoldsAContainer(t *testing.T) {
	for _, c := range []struct {
		name    string
		payload string
		want    string
	}{
		{"object", `{"n":{"k":"v"}}`, "#.n"},
		{"array", `{"n":["a"]}`, ".n[*]"},
	} {
		t.Run(c.name, func(t *testing.T) {
			model := "e2e-kind-branch-null-" + c.name
			importModelSampleE2E(t, model, 1, `{"n":null}`)
			lockModelE2E(t, model, 1)
			setChangeLevelE2E(t, model, 1, "TYPE")

			status, body := createEntityRawE2E(t, model, 1, c.payload)
			if !created(status) {
				t.Fatalf("status = %d, want 2xx; body: %s", status, body)
			}
			if _, ok := rootBucketE2E(t, model, 1)[c.want]; !ok {
				t.Errorf("export must name the %q branch; bucket: %v", c.want, rootBucketE2E(t, model, 1))
			}
		})
	}
}

// A field first written as an empty array, later holding object elements. The
// commonest of the three widenings that reached the client as a 500.
func TestModelKindBranch_EmptyArrayLaterHoldsObjectElements(t *testing.T) {
	const model = "e2e-kind-branch-emptyarray"
	importModelSampleE2E(t, model, 1, `{"items":[]}`)
	lockModelE2E(t, model, 1)
	setChangeLevelE2E(t, model, 1, "ARRAY_ELEMENTS")

	status, body := createEntityRawE2E(t, model, 1, `{"items":[{"a":1}]}`)
	if !created(status) {
		t.Fatalf("status = %d, want 2xx; body: %s", status, body)
	}

	root := rootBucketE2E(t, model, 1)
	if _, ok := root["#.items"]; !ok {
		t.Errorf("export must name the object elements; bucket: %v", root)
	}
}

// Writing null to a field declared as a scalar proposes no schema change, so
// no level gates it. It used to be refused below TYPE as a "type change",
// although the delta it produced was empty.
func TestModelKindBranch_NullAgainstAScalarNeedsNoLevel(t *testing.T) {
	const model = "e2e-kind-branch-nullwrite"
	importModelSampleE2E(t, model, 1, `{"s":"x"}`)
	lockModelE2E(t, model, 1)
	setChangeLevelE2E(t, model, 1, "ARRAY_LENGTH")

	status, body := createEntityRawE2E(t, model, 1, `{"s":null}`)
	if !created(status) {
		t.Errorf("status = %d, want 2xx; body: %s", status, body)
	}
}

// The strict door is untouched: with no changeLevel the model is fixed, and a
// kind outside the declared set is a plain validation failure whatever else
// changed.
func TestModelKindBranch_StrictDoorStillRejects(t *testing.T) {
	const model = "e2e-kind-branch-strict"
	importModelSampleE2E(t, model, 1, `[{"poly":"x"},{"poly":["a"]}]`)
	lockModelE2E(t, model, 1)

	status, body := createEntityRawE2E(t, model, 1, `{"poly":{"k":"v"}}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", status, body)
	}
	if !strings.Contains(body, "VALIDATION_FAILED") {
		t.Errorf("body must carry VALIDATION_FAILED; body: %s", body)
	}
	// The message names every kind the field does declare, so the caller is
	// told what it accepts rather than only what it refused.
	if !strings.Contains(body, "got object") {
		t.Errorf("body must name the kind that was sent; body: %s", body)
	}
}

// A unique key can only be enforced over a path that holds a scalar and
// nothing else, so a STRUCTURAL write that would give a KEYED path a container
// branch is refused — and refused before anything is written.
//
// The ordering is what makes this matter: the schema extension is committed
// before the write's own transaction is opened. Letting the widening through
// would leave the model permanently declaring a kind that every later write of
// that kind is then refused for, with nothing left to roll it back.
func TestModelKindBranch_KeyedPathCannotGainAContainer(t *testing.T) {
	const model = "e2e-kind-branch-keyed"
	importModelSampleE2E(t, model, 1, `{"sku":"x"}`)

	if status, body := setUniqueKeysE2E(t, model, 1,
		`{"uniqueKeys":[{"id":"sku-key","fields":["$.sku"]}]}`); status != http.StatusOK {
		t.Fatalf("setUniqueKeys: status = %d; body: %s", status, body)
	}
	lockModelE2E(t, model, 1)
	setChangeLevelE2E(t, model, 1, "STRUCTURAL")

	before := rootBucketE2E(t, model, 1)

	status, body := createEntityRawE2E(t, model, 1, `{"sku":{"sub":"val"}}`)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body: %s", status, body)
	}
	if !strings.Contains(body, "INVALID_UNIQUE_KEY_DEFINITION") {
		t.Errorf("body must carry INVALID_UNIQUE_KEY_DEFINITION; body: %s", body)
	}

	// Nothing was recorded: the model still declares exactly what it did.
	after := rootBucketE2E(t, model, 1)
	if len(before) != len(after) {
		t.Fatalf("the rejected write changed the model\n  before: %v\n  after:  %v", before, after)
	}
	for k, v := range before {
		if after[k] != v {
			t.Errorf("the rejected write changed %q: %v -> %v", k, v, after[k])
		}
	}

	// And the declared kind still writes.
	if status, body := createEntityRawE2E(t, model, 1, `{"sku":"plain"}`); !created(status) {
		t.Errorf("the declared kind must still write: status = %d; body: %s", status, body)
	}
}

// A keyed path must not be polymorphic, and the sample-data import is a door
// that can make one: a second import while the model is UNLOCKED unions a new
// kind onto the path. Before the branch set the import was accepted, and the
// model was left carrying a key over a path admitting a value no claim can be
// computed from.
func TestModelKindBranch_ImportCannotMakeAKeyedPathPolymorphic(t *testing.T) {
	const model = "e2e-kind-branch-import-keyed"
	importModelSampleE2E(t, model, 1, `{"sku":"x"}`)

	if status, body := setUniqueKeysE2E(t, model, 1,
		`{"uniqueKeys":[{"id":"sku-key","fields":["$.sku"]}]}`); status != http.StatusOK {
		t.Fatalf("setUniqueKeys: status = %d; body: %s", status, body)
	}

	// Still UNLOCKED, so the import would ordinarily merge and be accepted.
	status, body := importModelRawE2E(t, model, 1, `{"sku":{"a":1}}`)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body: %s", status, body)
	}
	if !strings.Contains(body, "INVALID_UNIQUE_KEY_DEFINITION") {
		t.Errorf("body must carry INVALID_UNIQUE_KEY_DEFINITION; body: %s", body)
	}

	root := rootBucketE2E(t, model, 1)
	if _, ok := root["#.sku"]; ok {
		t.Errorf("the rejected import must not add the object branch; bucket: %v", root)
	}
	if got := root[".sku"]; got != "STRING" {
		t.Errorf(".sku = %v, want STRING (unchanged)", got)
	}
}
