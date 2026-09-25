package parity

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// callback_joined_commit.go — a callback never commits the transaction it
// joined.
//
// A compute node's callback made with the transaction token runs in the
// transaction T of the operation that called it out. When that callback's
// write runs a workflow that reaches a COMMIT_BEFORE_DISPATCH processor — which
// commits the transaction it runs in — the write is refused with 409
// COMMIT_IN_JOINED_TRANSACTION before T is flushed or committed, and T stays
// the owner's.
//
// The scenario proves it on every backend: the primary's SYNC processor makes
// the joined create, records the answer, and succeeds, so the owner commits T.
//   - The owner's commit succeeds, so T was still open: had the callback
//     committed it, the owner's commit would fail.
//   - The refused secondary is not stored: had its COMMIT_BEFORE_DISPATCH run,
//     the secondary would have been flushed and committed with T.
//
// The COMMIT_BEFORE_DISPATCH processor carries a tag no compute member serves.
// The refusal comes before any dispatch, so the tag never matters when the
// refusal holds; when it does not, the segment commits T and the dispatch then
// fails at once for want of a member — rather than waiting on the compute test
// client, which serves one callout at a time and is busy with the outer one.

// cbJoinedCommitSample declares exactly the fields cb-create-secondary-record
// writes back, at zero value.
const cbJoinedCommitSample = `{"name":"Test","amount":10,"status":"new","tokenWasEmpty":false,"hopStatus":0,"hopBody":""}`

// cbNoMemberTag is served by no compute member (see the file comment).
const cbNoMemberTag = "cbjc-no-member-serves-this"

// cbCommitBeforeDispatchWorkflow gives the secondary model a
// COMMIT_BEFORE_DISPATCH processor on its create's automated transition.
func cbCommitBeforeDispatchWorkflow(wfName string, startNewTx bool) string {
	return cbWorkflowDoc(wfName, map[string]any{
		"NONE": map[string]any{"transitions": []any{map[string]any{
			"name": "store", "next": "STORED", "manual": false,
			"processors": []any{map[string]any{
				"type": "calculator", "name": "noop", "executionMode": "COMMIT_BEFORE_DISPATCH",
				"config": map[string]any{"attachEntity": true, "calculationNodesTags": cbNoMemberTag, "startNewTxOnDispatch": startNewTx},
			}},
		}}},
		"STORED": map[string]any{},
	})
}

// RunCallbackJoinedCommitBeforeDispatchRefused: see the file comment. Both
// startNewTxOnDispatch values are refused.
func RunCallbackJoinedCommitBeforeDispatchRefused(t *testing.T, fixture BackendFixture) {
	tenant := fixture.ComputeTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	for _, tc := range []struct {
		name       string
		startNewTx bool
	}{{"startNewTxOnDispatch=false", false}, {"startNewTxOnDispatch=true", true}} {
		t.Run(tc.name, func(t *testing.T) {
			suffix := "false"
			if tc.startNewTx {
				suffix = "true"
			}
			secondary := "cbjc-secondary-" + suffix
			primary := "cbjc-primary-" + suffix
			marker := "cbjc-marker-" + suffix

			cbSetupModel(t, c, secondary, cbSampleSecondary, cbCommitBeforeDispatchWorkflow("cbjc-secondary-wf-"+suffix, tc.startNewTx))
			cbSetupModel(t, c, primary, cbJoinedCommitSample,
				cbPrimaryProcWorkflow("cbjc-primary-wf-"+suffix, "cb-create-secondary-record", "SYNC", cbContext(secondary, marker)))

			primaryID, err := c.CreateEntity(t, primary, 1, `{"name":"parent","amount":100,"status":"new"}`)
			if err != nil {
				t.Fatalf("primary create: %v — the owner must still commit T after its callback was refused", err)
			}
			prim, err := c.GetEntity(t, primaryID)
			if err != nil {
				t.Fatalf("GetEntity primary: %v", err)
			}
			if prim.Meta.State != "ACTIVE" {
				t.Fatalf("primary state = %q; want ACTIVE (the owner committed T): data=%+v", prim.Meta.State, prim.Data)
			}
			if empty, _ := prim.Data["tokenWasEmpty"].(bool); empty {
				t.Fatal("tokenWasEmpty=true; the callback must have joined T")
			}

			status, _ := prim.Data["hopStatus"].(float64)
			body, _ := prim.Data["hopBody"].(string)
			if int(status) != http.StatusConflict {
				t.Fatalf("joined create status = %v; want 409 (body: %s)", status, body)
			}
			var pd struct {
				Properties struct {
					ErrorCode string `json:"errorCode"`
					Retryable bool   `json:"retryable"`
				} `json:"properties"`
			}
			if err := json.Unmarshal([]byte(body), &pd); err != nil {
				t.Fatalf("joined create answer is not a problem document: %v (body: %s)", err, body)
			}
			if pd.Properties.ErrorCode != "COMMIT_IN_JOINED_TRANSACTION" || pd.Properties.Retryable {
				t.Fatalf("errorCode=%q retryable=%t; want COMMIT_IN_JOINED_TRANSACTION, not retryable (body: %s)",
					pd.Properties.ErrorCode, pd.Properties.Retryable, body)
			}

			hits, err := c.SyncSearch(t, secondary, 1, cbStatusEquals(marker))
			if err != nil {
				t.Fatalf("search secondary: %v", err)
			}
			if len(hits) != 0 {
				t.Fatalf("refused secondary stored %d time(s); want 0 — its COMMIT_BEFORE_DISPATCH ran inside the joined transaction", len(hits))
			}
		})
	}
}
