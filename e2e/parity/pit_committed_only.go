package parity

import (
	"net/http"
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// cbSamplePITCommittedOnly — cb-pit-committed-only's primary sample doc.
const cbSamplePITCommittedOnly = `{"name":"Test","amount":10,"status":"new","secondaryId":"","plainReadStatus":0,"pitReadStatus":0}`

// RunPITCommittedOnlyInJoinedTx pins that a point-in-time read issued INSIDE a
// joined transaction is committed-only on every backend: it ignores the ambient
// transaction and answers from committed state, so an entity that transaction
// created but has not committed is not found — however far forward the
// requested instant is.
//
// Backend-agnostic and HTTP-reachable, which is why it belongs here rather than
// in a per-plugin test. The X-Tx-Token join middleware wraps the whole API mux,
// so a compute-node callback issuing GET /api/entity/{id}?pointInTime= inside a
// transition's transaction takes exactly this path — this is a real client
// shape, not an SPI-only contract.
//
// It is here because the backends diverged on it and nothing at this layer
// noticed: memory and sqlite buffer in-transaction writes off the store, so a
// point-in-time query never saw them, while postgres ran the query on the
// caller's own transaction connection and did. Only a cross-backend assertion
// makes that class of divergence fail rather than pass three times over.
//
// The two dimensions do not compose: "as at instant T" and "plus writes that
// have no commit time yet" have no consistent joint answer, so a backend
// serving both returns rows whose visibility depends on which connection ran
// the query rather than on T.
//
// The plain read is the control. A scenario asserting only the point-in-time
// 404 would also pass if the callback had never joined T, or if the create had
// silently not happened — both of which make every read miss.
func RunPITCommittedOnlyInJoinedTx(t *testing.T, fixture BackendFixture) {
	tenant := fixture.ComputeTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const secondary = "cbtj-pitco-secondary"
	const primary = "cbtj-pitco-primary"
	const marker = "cbtj-pitco-marker"

	cbSetupModel(t, c, secondary, cbSampleSecondary, cbSecondaryWorkflow)
	cbSetupModel(t, c, primary, cbSamplePITCommittedOnly,
		cbPrimaryProcWorkflow("cbtj-pitco-wf", "cb-pit-committed-only", "SYNC", cbContext(secondary, marker)))

	primaryID, err := c.CreateEntity(t, primary, 1, `{"name":"parent","amount":100,"status":"new"}`)
	if err != nil {
		t.Fatalf("primary create: %v", err)
	}
	prim, err := c.GetEntity(t, primaryID)
	if err != nil {
		t.Fatalf("GetEntity primary: %v", err)
	}

	plain, _ := prim.Data["plainReadStatus"].(float64)
	if int(plain) != http.StatusOK {
		t.Fatalf("control: the plain joined read of the uncommitted secondary returned %v, want 200 — "+
			"the callback did not join the transaction, so the point-in-time assertion below proves nothing: data=%+v",
			prim.Data["plainReadStatus"], prim.Data)
	}

	pit, _ := prim.Data["pitReadStatus"].(float64)
	if int(pit) != http.StatusNotFound {
		t.Errorf("in-transaction point-in-time read of an entity the transaction created but has not committed "+
			"returned %v, want 404 — a point-in-time read must ignore the ambient transaction and answer from "+
			"committed state: data=%+v", prim.Data["pitReadStatus"], prim.Data)
	}
}
