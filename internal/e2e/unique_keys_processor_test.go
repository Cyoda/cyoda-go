package e2e_test

// unique_keys_processor_test.go — running-backend (postgres) e2e coverage for
// composite unique-key enforcement on the POST-MERGE document, i.e. after a
// workflow processor has mutated the key field.
//
// The composite-unique-key feature enforces keys store-side, computing claims
// from the LIVE entity.Data. The pre-engine check (Handler.withUniqueKeys)
// runs ComputeClaims on the INPUT only — a fast partial-key reject. The real
// uniqueness/over-bound enforcement happens when the cascade's final Save
// computes claims from the processor-mutated data, and the resulting
// spi.ErrUniqueViolation / spi.ErrPartialUniqueKey is routed by
// classifyWorkflowError → 409 UNIQUE_VIOLATION / 422 INVALID_UNIQUE_KEY.
//
// Every existing unique-key e2e uses a no-processor auto-transition, so the
// processor-mutated path (the entire design rationale) was untested on a
// running backend. These tests close that gap:
//
//   Processor rewrites key field, create     → 409 UNIQUE_VIOLATION
//   Processor rewrites key field, If-Match    → 409 UNIQUE_VIOLATION (not 400 WORKFLOW_FAILED)
//   Processor sets over-bound key value        → 422 INVALID_UNIQUE_KEY (not 500/WORKFLOW_FAILED)

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// ukProcWorkflowOneStep builds a NONE -init(processor)-> CREATED workflow whose
// single automated transition runs the named SYNC processor.
func ukProcWorkflowOneStep(model, procName string) string {
	return fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "%s-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE":    {"transitions": [{"name": "init", "next": "CREATED", "manual": false,
					"processors": [{"type": "calculator", "name": "%s", "executionMode": "SYNC",
						"config": {"attachEntity": true, "calculationNodesTags": ""}}]
				}]},
				"CREATED": {}
			}
		}]
	}`, model, procName)
}

// TestUniqueKeys_ProcessorRewrite_409 proves create-path enforcement runs on
// the processor's OUTPUT, not the client input. A processor overwrites the key
// field "name" to a constant ("COLLIDE") on every create. Two entities are
// created with DIFFERENT input names — a pre-processor check would admit both —
// yet the second collides on the post-merge value → 409 UNIQUE_VIOLATION.
func TestUniqueKeys_ProcessorRewrite_409(t *testing.T) {
	const model = "e2e-uk-proc-create"
	const procName = "uk-collide-name"

	procSvc.RegisterProcessor(procName, func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition) (*spi.Entity, error) {
		var data map[string]any
		if err := json.Unmarshal(entity.Data, &data); err != nil {
			return nil, err
		}
		data["name"] = "COLLIDE" // constant — forces every entity to the same key value
		updated, err := json.Marshal(data)
		if err != nil {
			return nil, err
		}
		return &spi.Entity{Meta: entity.Meta, Data: updated}, nil
	})
	defer procSvc.Reset()

	importModelWithSample(t, model, 1, ukSampleData)
	keysJSON := `{"uniqueKeys":[{"id":"name-key","fields":["$.name"]}]}`
	if status, body := setUniqueKeysE2E(t, model, 1, keysJSON); status != http.StatusOK {
		t.Fatalf("setUniqueKeys: expected 200, got %d: %s", status, body)
	}
	lockModelE2E(t, model, 1)
	if status, body := importWorkflowE2E(t, model, 1, ukProcWorkflowOneStep(model, procName)); status != http.StatusOK {
		t.Fatalf("importWorkflow: expected 200, got %d: %s", status, body)
	}

	// First create (input name="a-unique") → processor rewrites to "COLLIDE" → 200.
	if status, body := createEntityRaw(t, model, 1, `{"name":"a-unique","amount":1,"status":"draft"}`); status != http.StatusOK {
		t.Fatalf("first create: expected 200, got %d: %s", status, body)
	}

	// Second create with a DISTINCT input name → processor rewrites to the same
	// "COLLIDE" → 409 UNIQUE_VIOLATION. The differing inputs prove enforcement
	// is on the post-merge value (an input-time check would admit "b-different").
	status, body := createEntityRaw(t, model, 1, `{"name":"b-different","amount":2,"status":"draft"}`)
	if status != http.StatusConflict {
		t.Fatalf("processor-collided create: expected 409, got %d: %s", status, body)
	}
	assertErrorCode(t, body, "UNIQUE_VIOLATION")
}

// TestUniqueKeys_ProcessorRewrite_IfMatchUpdate_409 proves the UPDATE / If-Match
// path surfaces a processor-induced collision as 409 UNIQUE_VIOLATION inside the
// cascade — NOT as 400 WORKFLOW_FAILED. Entity A holds key value "collide-target".
// Entity B holds "b-orig". A conditional (If-Match) PUT on B fires a processor
// that rewrites B's name to "collide-target", colliding with A → 409.
func TestUniqueKeys_ProcessorRewrite_IfMatchUpdate_409(t *testing.T) {
	const model = "e2e-uk-proc-ifmatch"
	const procName = "uk-rewrite-to-target"

	procSvc.RegisterProcessor(procName, func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition) (*spi.Entity, error) {
		var data map[string]any
		if err := json.Unmarshal(entity.Data, &data); err != nil {
			return nil, err
		}
		data["name"] = "collide-target" // rewrite B's key onto A's claimed value
		updated, err := json.Marshal(data)
		if err != nil {
			return nil, err
		}
		return &spi.Entity{Meta: entity.Meta, Data: updated}, nil
	})
	defer procSvc.Reset()

	importModelWithSample(t, model, 1, ukSampleData)
	keysJSON := `{"uniqueKeys":[{"id":"name-key","fields":["$.name"]}]}`
	if status, body := setUniqueKeysE2E(t, model, 1, keysJSON); status != http.StatusOK {
		t.Fatalf("setUniqueKeys: expected 200, got %d: %s", status, body)
	}
	lockModelE2E(t, model, 1)

	// Workflow: NONE -init-> PENDING -approve(processor)-> APPROVED. The init
	// transition has NO processor, so a created entity keeps its input name and
	// claims it. The manual "approve" transition (driven by the If-Match PUT)
	// runs the rewrite processor.
	wf := fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "%s-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE":     {"transitions": [{"name": "init", "next": "PENDING", "manual": false}]},
				"PENDING":  {"transitions": [{"name": "approve", "next": "APPROVED", "manual": true,
					"processors": [{"type": "calculator", "name": "%s", "executionMode": "SYNC",
						"config": {"attachEntity": true, "calculationNodesTags": ""}}]
				}]},
				"APPROVED": {}
			}
		}]
	}`, model, procName)
	if status, body := importWorkflowE2E(t, model, 1, wf); status != http.StatusOK {
		t.Fatalf("importWorkflow: expected 200, got %d: %s", status, body)
	}

	// Entity A claims "collide-target".
	if status, body := createEntityRaw(t, model, 1, `{"name":"collide-target","amount":1,"status":"draft"}`); status != http.StatusOK {
		t.Fatalf("create A: expected 200, got %d: %s", status, body)
	}

	// Entity B claims "b-orig".
	status, body := createEntityRaw(t, model, 1, `{"name":"b-orig","amount":2,"status":"draft"}`)
	if status != http.StatusOK {
		t.Fatalf("create B: expected 200, got %d: %s", status, body)
	}
	bID := extractEntityID(t, body)

	// Fresh If-Match token for B (its current transactionId after the init cascade).
	ifMatch := getEntityTxID(t, bID)

	// If-Match PUT firing the "approve" transition → processor rewrites B's name
	// to "collide-target" → collides with A. Must be 409 UNIQUE_VIOLATION,
	// surfaced through classifyWorkflowError inside the cascade — NOT 400.
	path := fmt.Sprintf("/api/entity/JSON/%s/approve", bID)
	req := authRequest(t, http.MethodPut, path, strings.NewReader(`{"name":"b-orig","amount":2,"status":"approved"}`))
	req.Header.Set("If-Match", ifMatch)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("If-Match PUT approve failed: %v", err)
	}
	respBody := readBody(t, resp)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("If-Match update colliding via processor: expected 409, got %d: %s", resp.StatusCode, respBody)
	}
	assertErrorCode(t, respBody, "UNIQUE_VIOLATION")
}

// TestUniqueKeys_ProcessorOverBoundValue_422 proves an over-bound key value
// produced by a processor surfaces as 422 INVALID_UNIQUE_KEY via
// classifyWorkflowError (the spi.ErrPartialUniqueKey umbrella) — NOT a
// 500/WORKFLOW_FAILED. The processor overwrites "amount" with the numeric
// literal 1e6145 (exponent 6145 > ComputeClaims' maxNumExp=6144).
//
// The model is seeded with an UNBOUND_INTEGER "amount" (a 50-digit literal,
// > 2^128) so schema validation accepts the large-exponent value — mirroring
// the input-path test TestUniqueKeys_OverBoundNumeric. The processor preserves
// the literal via json.RawMessage (1e6145 overflows float64 to +Inf, which is
// not JSON-encodable, so it must NOT round-trip through a float64).
func TestUniqueKeys_ProcessorOverBoundValue_422(t *testing.T) {
	const model = "e2e-uk-proc-overbound"
	const procName = "uk-set-overbound"

	procSvc.RegisterProcessor(procName, func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition) (*spi.Entity, error) {
		// Decode with UseNumber so the seed amount stays exact; overwrite amount
		// with the over-bound literal preserved verbatim via json.RawMessage.
		dec := json.NewDecoder(bytes.NewReader(entity.Data))
		dec.UseNumber()
		var data map[string]any
		if err := dec.Decode(&data); err != nil {
			return nil, err
		}
		data["amount"] = json.RawMessage(`1e6145`)
		updated, err := json.Marshal(data)
		if err != nil {
			return nil, err
		}
		return &spi.Entity{Meta: entity.Meta, Data: updated}, nil
	})
	defer procSvc.Reset()

	// Seed UNBOUND_INTEGER amount (50 digits > 2^128) so schema validation does
	// not reject the processor's large-exponent value before the store-side
	// unique-key check runs.
	const unboundSample = `{"name":"Test","amount":12345678901234567890123456789012345678901234567890,"status":"draft"}`
	importModelWithSample(t, model, 1, unboundSample)
	keysJSON := `{"uniqueKeys":[{"id":"amount-key","fields":["$.amount"]}]}`
	if status, body := setUniqueKeysE2E(t, model, 1, keysJSON); status != http.StatusOK {
		t.Fatalf("setUniqueKeys: expected 200, got %d: %s", status, body)
	}
	lockModelE2E(t, model, 1)
	if status, body := importWorkflowE2E(t, model, 1, ukProcWorkflowOneStep(model, procName)); status != http.StatusOK {
		t.Fatalf("importWorkflow: expected 200, got %d: %s", status, body)
	}

	// Input amount is an in-bounds large integer (passes the pre-engine input
	// check). The processor then sets amount=1e6145; the store-side ComputeClaims
	// rejects it (over-bound exponent → ErrPartialUniqueKey) → 422.
	status, body := createEntityRaw(t, model, 1, `{"name":"Test","amount":777,"status":"draft"}`)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("processor over-bound key: expected 422, got %d: %s", status, body)
	}
	assertErrorCode(t, body, "INVALID_UNIQUE_KEY")
}

// TestUniqueKeys_ProcessorDeletesAndReclaims_SameTx proves the same-transaction
// delete+reclaim works through the REAL reachable path. A SYNC processor runs
// INSIDE the engine transaction (its ctx carries the tx-token, so a store call
// joins that tx); it deletes entity A — freeing A's unique-key value — and
// rewrites entity B's key onto that just-freed value, all committed atomically.
// Must succeed (200): A's claim is released before B's claim is inserted, in one
// tx. This is the production-reachable counterpart of
// TestUniqueKeys_ProcessorRewrite_IfMatchUpdate_409, which is identical EXCEPT it
// does not delete A and therefore 409s — so the delete is provably what frees the
// value. Store-level equivalents live in plugins/{memory,sqlite,postgres}.
func TestUniqueKeys_ProcessorDeletesAndReclaims_SameTx(t *testing.T) {
	const model = "e2e-uk-proc-delete-reclaim"
	const procName = "uk-delete-a-reclaim-b"

	var aID string // A's id; set after A is created, read by the processor closure.

	procSvc.RegisterProcessor(procName, func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition) (*spi.Entity, error) {
		// ctx carries the engine transaction, so this Delete joins the SAME tx as
		// B's pending update — releasing A's claim before B's claim is inserted.
		store, err := testApp.StoreFactory().EntityStore(ctx)
		if err != nil {
			return nil, err
		}
		if err := store.Delete(ctx, aID); err != nil {
			return nil, err
		}
		var data map[string]any
		if err := json.Unmarshal(entity.Data, &data); err != nil {
			return nil, err
		}
		data["name"] = "shared" // B claims A's just-freed value
		updated, err := json.Marshal(data)
		if err != nil {
			return nil, err
		}
		return &spi.Entity{Meta: entity.Meta, Data: updated}, nil
	})
	defer procSvc.Reset()

	importModelWithSample(t, model, 1, ukSampleData)
	keysJSON := `{"uniqueKeys":[{"id":"name-key","fields":["$.name"]}]}`
	if status, body := setUniqueKeysE2E(t, model, 1, keysJSON); status != http.StatusOK {
		t.Fatalf("setUniqueKeys: expected 200, got %d: %s", status, body)
	}
	lockModelE2E(t, model, 1)

	// NONE -init(no proc)-> PENDING -reclaim(SYNC proc)-> DONE
	wf := fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "%s-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE":    {"transitions": [{"name": "init", "next": "PENDING", "manual": false}]},
				"PENDING": {"transitions": [{"name": "reclaim", "next": "DONE", "manual": true,
					"processors": [{"type": "calculator", "name": "%s", "executionMode": "SYNC",
						"config": {"attachEntity": true, "calculationNodesTags": ""}}]
				}]},
				"DONE":    {}
			}
		}]
	}`, model, procName)
	if status, body := importWorkflowE2E(t, model, 1, wf); status != http.StatusOK {
		t.Fatalf("importWorkflow: expected 200, got %d: %s", status, body)
	}

	// Entity A claims "shared".
	{
		status, body := createEntityRaw(t, model, 1, `{"name":"shared","amount":1,"status":"draft"}`)
		if status != http.StatusOK {
			t.Fatalf("create A: expected 200, got %d: %s", status, body)
		}
		aID = extractEntityID(t, body)
	}

	// Entity B claims "b-orig".
	status, body := createEntityRaw(t, model, 1, `{"name":"b-orig","amount":2,"status":"draft"}`)
	if status != http.StatusOK {
		t.Fatalf("create B: expected 200, got %d: %s", status, body)
	}
	bID := extractEntityID(t, body)

	// If-Match PUT firing "reclaim": the SYNC processor deletes A (freeing
	// "shared") and rewrites B's name to "shared", in ONE tx → must be 200, NOT 409.
	ifMatch := getEntityTxID(t, bID)
	path := fmt.Sprintf("/api/entity/JSON/%s/reclaim", bID)
	req := authRequest(t, http.MethodPut, path, strings.NewReader(`{"name":"b-orig","amount":2,"status":"draft"}`))
	req.Header.Set("If-Match", ifMatch)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("If-Match PUT reclaim failed: %v", err)
	}
	respBody := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("same-tx delete+reclaim: expected 200, got %d: %s", resp.StatusCode, respBody)
	}

	// A is gone (deleted in the same tx).
	getA := doAuth(t, http.MethodGet, fmt.Sprintf("/api/entity/%s", aID), "")
	if getA.StatusCode != http.StatusNotFound {
		t.Errorf("A after same-tx delete: expected 404, got %d: %s", getA.StatusCode, readBody(t, getA))
	}

	// B now holds "shared": a fresh create of "shared" collides → 409.
	if st, body := createEntityRaw(t, model, 1, `{"name":"shared","amount":9,"status":"draft"}`); st != http.StatusConflict {
		t.Errorf("reclaimed value must be enforced for B: expected 409, got %d: %s", st, body)
	} else {
		assertErrorCode(t, body, "UNIQUE_VIOLATION")
	}
}
