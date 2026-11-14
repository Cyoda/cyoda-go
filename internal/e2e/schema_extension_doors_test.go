package e2e_test

// schema_extension_doors_test.go — a write that brings a field its model does
// not have extends the model (changeLevel permitting) in a short transaction
// of its own, outside the write's entity transaction. Concurrent extenders of
// one model therefore never conflict, on any door, and the field is in the
// model by the time the write answers 2xx.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// extWrite is one HTTP write of a concurrent burst.
type extWrite struct {
	method, path, body string
}

// fireWritesConcurrently sends every write at once, each exactly once, and
// returns the answers in the order of writes.
func fireWritesConcurrently(t *testing.T, writes []extWrite) []httpResult {
	t.Helper()
	ctx := e2eCtx(t)
	results := make([]httpResult, len(writes))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, w := range writes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i] = resultOf(doAuthOnceRaw(ctx, w.method, w.path, w.body))
		}()
	}
	close(start)
	wg.Wait()
	return results
}

// requireAllWritesSucceeded fails the test for every answer other than a 200
// without a failed collection item.
func requireAllWritesSucceeded(t *testing.T, results []httpResult) {
	t.Helper()
	for i, res := range results {
		switch {
		case res.err != nil:
			t.Errorf("writer %d: transport error: %v", i, res.err)
		case res.status != http.StatusOK:
			t.Errorf("writer %d: status %d, want 200: %s", i, res.status, res.body)
		case collectionItemFailed(res.body):
			t.Errorf("writer %d: 200 with a failed collection item: %s", i, res.body)
		}
	}
}

// collectionItemFailed reports whether a collection answer lists a failed item.
func collectionItemFailed(body string) bool {
	var chunks []struct {
		Failed []any `json:"failed"`
	}
	if json.Unmarshal([]byte(body), &chunks) != nil {
		return false
	}
	for _, c := range chunks {
		if len(c.Failed) > 0 {
			return true
		}
	}
	return false
}

// requireFieldsInExport fails the test for each field path (such as
// "$.parent.child") the model's SIMPLE_VIEW export does not declare. The
// export lists a path's children under the path itself: a leaf child as
// ".name", an object or array child as "#.name".
func requireFieldsInExport(t *testing.T, model string, paths ...string) {
	t.Helper()
	export := exportModelE2E(t, model, 1)
	buckets, _ := export["model"].(map[string]any)
	for _, p := range paths {
		i := strings.LastIndex(p, ".")
		if i <= 0 {
			t.Fatalf("field path %q names no field under a parent", p)
		}
		bucket, _ := buckets[p[:i]].(map[string]any)
		_, leaf := bucket["."+p[i+1:]]
		_, branch := bucket["#."+p[i+1:]]
		if !leaf && !branch {
			t.Errorf("model %s lacks %s after its write answered 2xx: %v", model, p, export)
		}
	}
}

// openThenGoWorkflowJSON is a NONE -> OPEN (automated) -> DONE (manual "go")
// workflow whose manual transition runs one SYNC processor.
func openThenGoWorkflowJSON(wfName, procName string) string {
	return fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": %q, "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "open", "next": "OPEN", "manual": false}]},
				"OPEN": {"transitions": [{"name": "go", "next": "DONE", "manual": true,
					"processors": [{"type": "externalized", "name": %q, "executionMode": "SYNC",
						"config": {"attachEntity": true, "calculationNodesTags": ""}}]}]},
				"DONE": {}
			}
		}]
	}`, wfName, procName)
}

// TestSchemaExtension_ConcurrentNestedUnderNewParent — a burst of concurrent
// PUTs on one model, each adding a nested field of its own under a parent
// object the model does not have. Every write of the burst brings the parent
// too, so the writers race the parent's extension; the interleave is
// probabilistic, and the test asserts consistency, not an order: every write
// answers 2xx and the model gains the parent and every child.
func TestSchemaExtension_ConcurrentNestedUnderNewParent(t *testing.T) {
	const N = 8
	model := uniq("se-nested")
	importModelE2E(t, model, 1)
	lockModelE2E(t, model, 1)
	setChangeLevelE2E(t, model, 1, "STRUCTURAL")

	writes := make([]extWrite, N)
	fields := []string{"$.newParent"}
	for i := range writes {
		id := createEntityE2E(t, model, 1, fmt.Sprintf(`{"name":"w%d","amount":%d,"status":"new"}`, i, i))
		writes[i] = extWrite{http.MethodPut, "/api/entity/JSON/" + id,
			fmt.Sprintf(`{"name":"w%d","amount":%d,"status":"new","newParent":{"child_%d":"v"}}`, i, i, i)}
		fields = append(fields, fmt.Sprintf("$.newParent.child_%d", i))
	}

	requireAllWritesSucceeded(t, fireWritesConcurrently(t, writes))
	requireFieldsInExport(t, model, fields...)
}

// TestSchemaExtension_CriterionOnFieldAddedInSameRequest — a create whose
// processor adds p, followed in the same request by an automated transition
// whose criterion tests p: the criterion sees the field the request itself
// added, so the entity ends in the criterion's target state.
func TestSchemaExtension_CriterionOnFieldAddedInSameRequest(t *testing.T) {
	model := uniq("se-crit")
	const proc = "se-crit-adds-p"
	registerRewriter(t, proc, `{"name":"x","amount":1,"status":"new","p":"yes"}`)
	defer procSvc.Reset()

	importModelE2E(t, model, 1)
	lockModelE2E(t, model, 1)
	setChangeLevelE2E(t, model, 1, "STRUCTURAL")
	status, body := importWorkflowE2E(t, model, 1, fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "se-crit-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false,
					"processors": [{"type": "externalized", "name": %q, "executionMode": "SYNC",
						"config": {"attachEntity": true, "calculationNodesTags": ""}}]}]},
				"CREATED": {"transitions": [{"name": "check", "next": "MATCHED", "manual": false,
					"criterion": {"type": "simple", "jsonPath": "$.p", "operatorType": "EQUALS", "value": "yes"}}]},
				"MATCHED": {}
			}
		}]
	}`, proc))
	if status != http.StatusOK {
		t.Fatalf("workflow import: %d %s", status, body)
	}

	id := createEntityE2E(t, model, 1, `{"name":"x","amount":1,"status":"new"}`)
	if st := getEntityState(t, id); st != "MATCHED" {
		t.Errorf("entity state = %q, want MATCHED: the criterion on the field the processor added in the same request must match", st)
	}
}

// TestGRPC_ConcurrentExtension_SuccessEnvelope is the gRPC twin of the
// concurrent update door: N concurrent EntityUpdateRequests, each adding a
// field of its own to one STRUCTURAL model, all answer success, and the model
// gains every field.
func TestGRPC_ConcurrentExtension_SuccessEnvelope(t *testing.T) {
	const N = 8
	h := newCalloutHarness(t, nil)
	model := uniq("se-grpc-conc")
	h.SetupModelWithWorkflow(t, model, secondaryWorkflow)
	resp := h.DoAuth(t, http.MethodPost, fmt.Sprintf("/api/model/%s/1/changeLevel/STRUCTURAL", model), "", "")
	if body := h.readBody(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("set changeLevel: %d %s", resp.StatusCode, body)
	}
	ids := make([]string, N)
	for i := range ids {
		id, status, body := h.CreateEntity(t, model, 1, fmt.Sprintf(`{"name":"w%d","amount":%d,"status":"new"}`, i, i))
		if status != http.StatusOK {
			t.Fatalf("create %d: %d %s", i, status, body)
		}
		ids[i] = id
	}

	type outcome struct {
		env txEnvelope
		err error
	}
	outcomes := make([]outcome, N)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			env, err := h.updateEntityGRPC(id, fmt.Sprintf(`{"name":"w%d","amount":%d,"status":"new","grpc_%d":"v"}`, i, i, i))
			outcomes[i] = outcome{env, err}
		}()
	}
	close(start)
	wg.Wait()

	for i, o := range outcomes {
		if o.err != nil {
			t.Errorf("writer %d: transport error: %v", i, o.err)
			continue
		}
		if !o.env.Success || o.env.Error != nil {
			t.Errorf("writer %d: envelope success=%v error=%+v; want success", i, o.env.Success, o.env.Error)
		}
	}

	resp = h.DoAuth(t, http.MethodGet, fmt.Sprintf("/api/model/export/SIMPLE_VIEW/%s/1", model), "", "")
	export := h.readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("model export: %d %s", resp.StatusCode, export)
	}
	for i := range N {
		if f := fmt.Sprintf("grpc_%d", i); !strings.Contains(export, f) {
			t.Errorf("model lacks %q after its write succeeded: %s", f, export)
		}
	}
}
