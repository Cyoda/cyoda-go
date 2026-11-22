package parity

// unique_keys_commit_time.go — unique keys are checked at commit, on the
// transaction's final entities, with the keys of the stored model. The same
// on every backend:
//   - two entities swap key values in one transaction → commits;
//   - a chain of moves (A takes B's value, B takes C's) → commits;
//   - a value claimed first and freed later in the transaction → commits;
//   - one value claimed by two entities of one transaction → 409
//     UNIQUE_VIOLATION, nothing committed;
//   - a callback write of another entity that duplicates a value, fixed by a
//     later processor's callback → commits.
//
// The moves are made by the compute node's cb-move-status processor: it writes
// each entity named on the anchor's data.moves through joined updates, so all
// of them are in the anchor's transaction. Races are isolated e2e; every
// scenario here is sequential.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cyoda/cyoda-go/e2e/parity/client"
)

// ukctSample declares every field the scenarios write: the key field status,
// the moves cb-move-status reads and the targetId cb-write-target reads.
const ukctSample = `{"name":"","status":"","targetId":"","moves":[{"id":"","status":""}]}`

// ukctKeys is the unique key every scenario model carries.
const ukctKeys = `{"uniqueKeys":[{"id":"status-key","fields":["$.status"]}]}`

// ukctSetup imports model from ukctSample, declares the status key on it,
// locks it and imports a NONE -init-> DONE workflow whose automated transition
// runs cb-move-status. extraStates adds states to that workflow; DONE may be
// replaced by it.
func ukctSetup(t *testing.T, c *client.Client, model string, extraStates map[string]any) {
	t.Helper()
	if err := c.ImportModel(t, model, 1, ukctSample); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	if status, raw := ukCapabilityGateOrSkip(t, c, model, 1, ukctKeys); status != http.StatusOK {
		t.Fatalf("SetUniqueKeys on unlocked model: expected 200, got %d: %s", status, raw)
	}
	if err := c.LockModel(t, model, 1); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	states := map[string]any{
		"NONE": map[string]any{"transitions": []any{map[string]any{
			"name": "init", "next": "DONE", "manual": false,
			"processors": []any{cbProc("cb-move-status", "SYNC", "", nil)},
		}}},
		"DONE": map[string]any{},
	}
	for k, v := range extraStates {
		states[k] = v
	}
	if err := c.ImportWorkflow(t, model, 1, cbWorkflowDoc(model+"-wf", states)); err != nil {
		t.Fatalf("ImportWorkflow: %v", err)
	}
}

// ukctCreate creates an entity of model with name and, when status is not
// empty, that key value.
func ukctCreate(t *testing.T, c *client.Client, model, name, status string) uuid.UUID {
	t.Helper()
	doc := map[string]any{"name": name}
	if status != "" {
		doc["status"] = status
	}
	b, _ := json.Marshal(doc)
	id, err := c.CreateEntity(t, model, 1, string(b))
	if err != nil {
		t.Fatalf("create %s with status %q: %v", name, status, err)
	}
	return id
}

// ukctMove is one entry of an anchor's data.moves.
type ukctMove struct {
	ID     uuid.UUID `json:"id"`
	Status string    `json:"status"`
}

// ukctDriver creates an anchor (with status, when not empty) whose
// cb-move-status processor makes moves in the anchor's transaction, and
// returns the create's answer.
func ukctDriver(t *testing.T, c *client.Client, model, status string, moves ...ukctMove) (int, []byte) {
	t.Helper()
	doc := map[string]any{"name": "driver", "moves": moves}
	if status != "" {
		doc["status"] = status
	}
	b, _ := json.Marshal(doc)
	code, raw, err := c.CreateEntityRaw(t, model, 1, string(b))
	if err != nil {
		t.Fatalf("driver create: %v", err)
	}
	return code, raw
}

// ukctRequireStatus fails unless entity id holds key value want.
func ukctRequireStatus(t *testing.T, c *client.Client, id uuid.UUID, want string) {
	t.Helper()
	e, err := c.GetEntity(t, id)
	if err != nil {
		t.Fatalf("GetEntity %s: %v", id, err)
	}
	if got, _ := e.Data["status"].(string); got != want {
		t.Errorf("entity %s has status %q; want %q", id, got, want)
	}
}

// ukctRequireClaimed fails unless a new entity with key value status is
// refused 409 UNIQUE_VIOLATION: some committed entity holds the value.
func ukctRequireClaimed(t *testing.T, c *client.Client, model, status string) {
	t.Helper()
	code, raw, err := c.CreateEntityRaw(t, model, 1, `{"name":"probe","status":"`+status+`"}`)
	if err != nil {
		t.Fatalf("probe create: %v", err)
	}
	if code != http.StatusConflict {
		t.Fatalf("create with held value %q: got %d; want 409 UNIQUE_VIOLATION: %s", status, code, raw)
	}
	ukAssertErrCode(t, raw, "UNIQUE_VIOLATION")
}

// ukctRequireFree fails unless a new entity with key value status is
// created: no committed entity holds the value.
func ukctRequireFree(t *testing.T, c *client.Client, model, status string) {
	t.Helper()
	code, raw, err := c.CreateEntityRaw(t, model, 1, `{"name":"probe","status":"`+status+`"}`)
	if err != nil {
		t.Fatalf("probe create: %v", err)
	}
	if code != http.StatusOK {
		t.Fatalf("create with free value %q: got %d; want 200: %s", status, code, raw)
	}
}

// RunUniqueSwapInOneTx: A holds x and B holds y; one transaction moves A to y
// and B to x. The final entities are unique, so it commits, and the claims
// follow the final entities.
func RunUniqueSwapInOneTx(t *testing.T, fixture BackendFixture) {
	tenant := fixture.ComputeTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)
	const model = "ukct-swap"
	ukctSetup(t, c, model, nil)

	a := ukctCreate(t, c, model, "a", "x")
	b := ukctCreate(t, c, model, "b", "y")

	if code, raw := ukctDriver(t, c, model, "", ukctMove{a, "y"}, ukctMove{b, "x"}); code != http.StatusOK {
		t.Fatalf("swap: got %d; want 200: %s", code, raw)
	}
	ukctRequireStatus(t, c, a, "y")
	ukctRequireStatus(t, c, b, "x")
	ukctRequireClaimed(t, c, model, "x")
	ukctRequireClaimed(t, c, model, "y")
}

// RunUniqueChainOfMoves: A, B and C hold a, b and c; one transaction moves A
// to b, B to c and C to d, each move claiming a value before its holder
// frees it. It commits; a is free afterwards and d is held.
func RunUniqueChainOfMoves(t *testing.T, fixture BackendFixture) {
	tenant := fixture.ComputeTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)
	const model = "ukct-chain"
	ukctSetup(t, c, model, nil)

	a := ukctCreate(t, c, model, "a", "a")
	b := ukctCreate(t, c, model, "b", "b")
	cc := ukctCreate(t, c, model, "c", "c")

	if code, raw := ukctDriver(t, c, model, "", ukctMove{a, "b"}, ukctMove{b, "c"}, ukctMove{cc, "d"}); code != http.StatusOK {
		t.Fatalf("chain of moves: got %d; want 200: %s", code, raw)
	}
	ukctRequireStatus(t, c, a, "b")
	ukctRequireStatus(t, c, b, "c")
	ukctRequireStatus(t, c, cc, "d")
	ukctRequireClaimed(t, c, model, "d")
	ukctRequireFree(t, c, model, "a")
}

// RunUniqueFreeAfterClaim: A holds x; a create claims x and its processor
// then moves A to w in the same transaction. It commits; the new entity
// holds x.
func RunUniqueFreeAfterClaim(t *testing.T, fixture BackendFixture) {
	tenant := fixture.ComputeTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)
	const model = "ukct-free-after-claim"
	ukctSetup(t, c, model, nil)

	a := ukctCreate(t, c, model, "a", "x")

	code, raw := ukctDriver(t, c, model, "x", ukctMove{a, "w"})
	if code != http.StatusOK {
		t.Fatalf("claim then free: got %d; want 200: %s", code, raw)
	}
	ukctRequireStatus(t, c, a, "w")
	ukctRequireClaimed(t, c, model, "x")
}

// RunUniqueSameValueTwiceInTx: one transaction moves A and B to the same
// value z. The final entities duplicate z: 409 UNIQUE_VIOLATION, and nothing
// the transaction wrote is committed.
func RunUniqueSameValueTwiceInTx(t *testing.T, fixture BackendFixture) {
	tenant := fixture.ComputeTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)
	const model = "ukct-same-value-twice"
	ukctSetup(t, c, model, nil)

	a := ukctCreate(t, c, model, "a", "a")
	b := ukctCreate(t, c, model, "b", "b")

	code, raw := ukctDriver(t, c, model, "", ukctMove{a, "z"}, ukctMove{b, "z"})
	if code != http.StatusConflict {
		t.Fatalf("same value twice: got %d; want 409 UNIQUE_VIOLATION: %s", code, raw)
	}
	ukAssertErrCode(t, raw, "UNIQUE_VIOLATION")
	ukctRequireStatus(t, c, a, "a")
	ukctRequireStatus(t, c, b, "b")
	list, err := c.ListEntitiesByModel(t, model, 1)
	if err != nil {
		t.Fatalf("ListEntitiesByModel: %v", err)
	}
	if len(list) != 2 {
		t.Errorf("model has %d entities; want 2 (the refused driver is not stored)", len(list))
	}
	ukctRequireFree(t, c, model, "z")
}

// RunUniqueIntermediateDuplicateFixed: A holds x and B holds b. A manual
// transition of D runs two processors whose joined callbacks write B, D's
// targetId: the first sets B's status to x (a duplicate of A's value), the
// second to fixed. Only the final entities are checked, so the transition
// commits.
func RunUniqueIntermediateDuplicateFixed(t *testing.T, fixture BackendFixture) {
	tenant := fixture.ComputeTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)
	const model = "ukct-intermediate-dup"
	ukctSetup(t, c, model, map[string]any{
		"DONE": map[string]any{"transitions": []any{map[string]any{
			"name": "fix", "next": "FIXED", "manual": true,
			"processors": []any{
				cbProc("cb-write-target", "SYNC", `{"marker":"x"}`, nil),
				cbProc("cb-write-target", "SYNC", `{"marker":"fixed"}`, nil),
			},
		}}},
		"FIXED": map[string]any{},
	})

	a := ukctCreate(t, c, model, "a", "x")
	b := ukctCreate(t, c, model, "b", "b")
	d := ukctCreate(t, c, model, "d", "d")

	body := fmt.Sprintf(`{"name":"d","status":"d","targetId":%q}`, b)
	if code, raw, err := c.UpdateEntityRaw(t, d, "fix", body); err != nil || code != http.StatusOK {
		t.Fatalf("transition with an intermediate duplicate: got %d err=%v; want 200: %s", code, err, raw)
	}
	e, err := c.GetEntity(t, d)
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}
	if e.Meta.State != "FIXED" {
		t.Fatalf("D is %s; want FIXED", e.Meta.State)
	}
	ukctRequireStatus(t, c, a, "x")
	ukctRequireStatus(t, c, b, "fixed")
	ukctRequireClaimed(t, c, model, "x")
	ukctRequireClaimed(t, c, model, "fixed")
	ukctRequireFree(t, c, model, "b")
}
