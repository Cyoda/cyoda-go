package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// A COMMIT_BEFORE_DISPATCH processor commits the write's first segment before
// its callout. If another transaction changes the entity during the callout,
// the engine's apply-result compare after it conflicts. Every write route
// answers that as the single update does: 412 ENTITY_MODIFIED naming the
// entity, with a fixed message.
//
// The callout makes the other transaction's write itself — an ordinary update
// of the entity it was dispatched for — so the conflict is deterministic:
// RegisterProcessor runs the closure synchronously inside the dispatch, and
// with startNewTxOnDispatch unset that dispatch runs outside any transaction,
// after the commit.
//
// The segmenting transition is automated, so a create reaches it, and guarded
// by status == "new". The out-of-band update writes another status, so its own
// cascade from the initial state does not take the transition again.
const cbdCreateConflictWorkflow = `{
	"importMode": "REPLACE",
	"workflows": [{
		"version": "1.1", "name": "%s", "initialState": "NONE", "active": true,
		"states": {
			"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false,
				"criterion": {"type":"simple","jsonPath":"$.status","operatorType":"EQUALS","value":"new"},
				"processors": [{"type": "externalized", "name": "%s",
					"executionMode": "COMMIT_BEFORE_DISPATCH",
					"config": {"attachEntity": true, "calculationNodesTags": ""}}]
			}]},
			"CREATED": {}
		}
	}]
}`

// registerCBDSideWriter registers a processor that, on its first call only,
// commits an out-of-band update of the entity it was dispatched for. It
// returns the dispatch count, the entity it wrote, and the update's outcome;
// all three are read only after the request under test returns, which the
// synchronous dispatch cannot overlap.
func registerCBDSideWriter(t *testing.T, name string) (dispatched *atomic.Int32, target *string, sideStatus *int, sideErr *error) {
	t.Helper()
	dispatched = &atomic.Int32{}
	target, sideStatus, sideErr = new(string), new(int), new(error)
	testCtx := e2eCtx(t) // the closure never touches t
	procSvc.RegisterProcessor(name, func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition) (*spi.Entity, error) {
		if dispatched.Add(1) != 1 {
			return entity, nil
		}
		*target = entity.Meta.ID
		// doAuthRaw, not doAuth: this runs on the server's goroutine and must
		// not call t.Fatalf.
		resp, err := doAuthRaw(testCtx, http.MethodPut,
			"/api/entity/JSON/"+entity.Meta.ID,
			`{"name":"X_CONCURRENT","amount":7,"status":"other"}`)
		if err != nil {
			*sideErr = err
			return entity, nil
		}
		defer func() { _ = resp.Body.Close() }()
		*sideStatus = resp.StatusCode
		return entity, nil
	})
	t.Cleanup(procSvc.Reset)
	return dispatched, target, sideStatus, sideErr
}

// requireSideWriteLanded checks the preconditions of a post-segment conflict:
// the callout fired exactly once (so the conflict is on the far side of the
// commit, and the request stopped at it), and the out-of-band write committed
// (so the apply-result compare has something to conflict with).
func requireSideWriteLanded(t *testing.T, dispatched *atomic.Int32, sideStatus int, sideErr error) {
	t.Helper()
	if sideErr != nil {
		t.Fatalf("out-of-band write from the callout failed: %v", sideErr)
	}
	if sideStatus != http.StatusOK {
		t.Fatalf("out-of-band write returned %d, want 200; the apply-result compare would not conflict", sideStatus)
	}
	if c := dispatched.Load(); c != 1 {
		t.Fatalf("CBD dispatch fired %d time(s), want 1", c)
	}
}

// assertPostSegmentEntityModified asserts the problem body of a post-segment
// conflict: ENTITY_MODIFIED naming entityID, not retryable, with the single
// update's fixed message and none of the engine's text.
func assertPostSegmentEntityModified(t *testing.T, body, entityID string) {
	t.Helper()
	assertErrorCode(t, body, "ENTITY_MODIFIED")
	var pd struct {
		Detail     string         `json:"detail"`
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal([]byte(body), &pd); err != nil {
		t.Fatalf("parse problem detail: %v; body: %s", err, body)
	}
	if got := pd.Properties["entityId"]; got != entityID {
		t.Errorf("properties.entityId = %v, want %s; body: %s", got, entityID, body)
	}
	if r, _ := pd.Properties["retryable"].(bool); r {
		t.Errorf("a conflict past a committed segment is not retryable: a retry re-fires the callout; body: %s", body)
	}
	if want := "ENTITY_MODIFIED: entity has been modified since last read"; pd.Detail != want {
		t.Errorf("detail = %q, want %q", pd.Detail, want)
	}
}

// TestCreate_CBDPostSegmentConflict_Is412: a single create.
func TestCreate_CBDPostSegmentConflict_Is412(t *testing.T) {
	const model = "e2e-create-cbd-post"
	dispatched, target, sideStatus, sideErr := registerCBDSideWriter(t, "create-cbd-post-writer")
	setupModelWithWorkflow(t, model, fmt.Sprintf(cbdCreateConflictWorkflow, "create-cbd-post-wf", "create-cbd-post-writer"))

	resp := doAuth(t, http.MethodPost, fmt.Sprintf("/api/entity/JSON/%s/1", model),
		`{"name":"X","amount":1,"status":"new"}`)
	body := readBody(t, resp)

	requireSideWriteLanded(t, dispatched, *sideStatus, *sideErr)
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want 412; body: %s", resp.StatusCode, body)
	}
	assertPostSegmentEntityModified(t, body, *target)
	// The segment before the callout committed, and the out-of-band write
	// over it stands.
	if data := getEntityData(t, *target, ""); data["name"] != "X_CONCURRENT" {
		t.Errorf("entity = %v; want the out-of-band write to stand", data)
	}
}

// TestCreateCollection_CBDPostSegmentConflict_Is412: an item of a collection
// create. The request fails whole: the item after the conflicting one is never
// run, and nothing of it is committed.
func TestCreateCollection_CBDPostSegmentConflict_Is412(t *testing.T) {
	const model = "e2e-create-coll-cbd-post"
	dispatched, target, sideStatus, sideErr := registerCBDSideWriter(t, "create-coll-cbd-post-writer")
	setupModelWithWorkflow(t, model, fmt.Sprintf(cbdCreateConflictWorkflow, "create-coll-cbd-post-wf", "create-coll-cbd-post-writer"))

	item := func(name string) string {
		b, _ := json.Marshal(map[string]any{
			"model":   map[string]any{"name": model, "version": 1},
			"payload": fmt.Sprintf(`{"name":%q,"amount":1,"status":"new"}`, name),
		})
		return string(b)
	}
	resp := doAuth(t, http.MethodPost, "/api/entity/JSON", "["+strings.Join([]string{item("X"), item("Y")}, ",")+"]")
	body := readBody(t, resp)

	requireSideWriteLanded(t, dispatched, *sideStatus, *sideErr)
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want 412; body: %s", resp.StatusCode, body)
	}
	assertPostSegmentEntityModified(t, body, *target)
	// Only the conflicting item's committed segment exists; the later item
	// was never written.
	if n := countEntitiesInModel(t, model, 1); n != 1 {
		t.Errorf("model holds %d entities, want 1 (the conflicting item's committed segment)", n)
	}
}
