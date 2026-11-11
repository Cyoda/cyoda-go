package parity

import (
	"encoding/json"
	"net/http"
	"strings"
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
// COMMIT_IN_JOINED_TRANSACTION before T is flushed or committed. The refusal
// is a non-retryable failure of a joined write: the callback's scope is
// discarded and T made rollback-only, so the owner is refused 409
// TRANSACTION_ROLLBACK_ONLY, naming the callback's code.
//
// The scenario proves it on every backend: the primary's SYNC processor makes
// the joined create, records the answer outside T, and succeeds, and the owner
// is still refused.
//   - The callback was answered 409 COMMIT_IN_JOINED_TRANSACTION, not
//     retryable, and it carried the token: it joined T.
//   - The owner's refusal names COMMIT_IN_JOINED_TRANSACTION: that is what the
//     callback was answered.
//   - Nothing is stored: had the refused secondary's COMMIT_BEFORE_DISPATCH
//     run, the secondary would have been flushed and committed with T; and the
//     owner committed nothing.
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

// cbHopRecordSample declares the fields recordHop writes.
const cbHopRecordSample = `{"marker":"","hopStatus":0,"hopBody":"","tokenWasEmpty":false}`

// SetupHopRecordModel imports the model a recording processor writes what its
// joined callback was answered into. The record is written with no tx token,
// so it is committed outside the joined transaction whatever its fate: the
// scenario reads the crossed-back answer from it even when the owner commits
// nothing.
func SetupHopRecordModel(t *testing.T, c *client.Client, model string) {
	t.Helper()
	cbSetupModel(t, c, model, cbHopRecordSample, cbSecondaryWorkflow)
}

// HopRecordContext is the processor context of cb-create-secondary-record and
// cb-tx-control-param-joined with a record model: the joined create goes to
// secondaryModel, and its answer is recorded in recordModel under marker.
func HopRecordContext(secondaryModel, marker, recordModel string) string {
	inner, _ := json.Marshal(map[string]any{
		"secondaryModel":   secondaryModel,
		"secondaryVersion": 1,
		"marker":           marker,
		"recordModel":      recordModel,
	})
	return string(inner)
}

// HopRecord is what a recording processor's joined callback was answered.
type HopRecord struct {
	Status        int
	Body          string
	TokenWasEmpty bool
	ErrorCode     string
	Retryable     bool
	Detail        string
}

// ReadHopRecord returns the record written under marker in recordModel,
// failing the test unless there is exactly one.
func ReadHopRecord(t *testing.T, c *client.Client, recordModel, marker string) HopRecord {
	t.Helper()
	all, err := c.ListEntitiesByModel(t, recordModel, 1)
	if err != nil {
		t.Fatalf("list %s: %v", recordModel, err)
	}
	var found []client.EntityResult
	for _, e := range all {
		if e.Data["marker"] == marker {
			found = append(found, e)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d records of the callback's answer under %q; want 1 (the processor did not run, or ran twice)", len(found), marker)
	}
	d := found[0].Data
	status, _ := d["hopStatus"].(float64)
	body, _ := d["hopBody"].(string)
	empty, _ := d["tokenWasEmpty"].(bool)
	r := HopRecord{Status: int(status), Body: body, TokenWasEmpty: empty}
	var pd struct {
		Detail     string `json:"detail"`
		Properties struct {
			ErrorCode string `json:"errorCode"`
			Retryable bool   `json:"retryable"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(body), &pd); err != nil {
		t.Fatalf("the recorded answer is not a problem document: %v (body: %s)", err, body)
	}
	r.ErrorCode, r.Retryable, r.Detail = pd.Properties.ErrorCode, pd.Properties.Retryable, pd.Detail
	return r
}

// cbCommitBeforeDispatchWorkflow gives the secondary model a
// COMMIT_BEFORE_DISPATCH processor on its create's automated transition.
func cbCommitBeforeDispatchWorkflow(wfName string, startNewTx bool) string {
	return cbWorkflowDoc(wfName, map[string]any{
		"NONE": map[string]any{"transitions": []any{map[string]any{
			"name": "store", "next": "STORED", "manual": false,
			"processors": []any{map[string]any{
				"type": "externalized", "name": "noop", "executionMode": "COMMIT_BEFORE_DISPATCH", "startNewTxOnDispatch": startNewTx,
				"config": map[string]any{"attachEntity": true, "calculationNodesTags": cbNoMemberTag},
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
			record := "cbjc-record-" + suffix

			SetupHopRecordModel(t, c, record)
			cbSetupModel(t, c, secondary, cbSampleSecondary, cbCommitBeforeDispatchWorkflow("cbjc-secondary-wf-"+suffix, tc.startNewTx))
			cbSetupModel(t, c, primary, cbJoinedCommitSample,
				cbPrimaryProcWorkflow("cbjc-primary-wf-"+suffix, "cb-create-secondary-record", "SYNC", HopRecordContext(secondary, marker, record)))

			status, raw, err := c.CreateEntityRaw(t, primary, 1, `{"name":"parent","amount":100,"status":"new"}`)
			if err != nil {
				t.Fatalf("primary create: %v", err)
			}
			var pd struct {
				Detail     string `json:"detail"`
				Properties struct {
					ErrorCode string `json:"errorCode"`
					Retryable bool   `json:"retryable"`
				} `json:"properties"`
			}
			if err := json.Unmarshal(raw, &pd); err != nil {
				t.Fatalf("primary create answered %d, not a problem document: %v (body: %s)", status, err, raw)
			}
			if status != http.StatusConflict || pd.Properties.ErrorCode != "TRANSACTION_ROLLBACK_ONLY" || pd.Properties.Retryable {
				t.Fatalf("primary create = %d %s retryable=%t; want 409 TRANSACTION_ROLLBACK_ONLY, not retryable (body: %s)",
					status, pd.Properties.ErrorCode, pd.Properties.Retryable, raw)
			}
			if !strings.Contains(pd.Detail, "COMMIT_IN_JOINED_TRANSACTION") {
				t.Fatalf("detail = %q; want it to name the callback's COMMIT_IN_JOINED_TRANSACTION", pd.Detail)
			}
			if all, err := c.ListEntitiesByModel(t, primary, 1); err != nil {
				t.Fatalf("list primary: %v", err)
			} else if len(all) != 0 {
				t.Fatalf("%d primary entities committed; the owner's refused operation commits nothing", len(all))
			}

			hop := ReadHopRecord(t, c, record, marker)
			if hop.TokenWasEmpty {
				t.Fatal("tokenWasEmpty=true; the callback must have joined T")
			}
			if hop.Status != http.StatusConflict || hop.ErrorCode != "COMMIT_IN_JOINED_TRANSACTION" || hop.Retryable {
				t.Fatalf("joined create = %d %s retryable=%t; want 409 COMMIT_IN_JOINED_TRANSACTION, not retryable (body: %s)",
					hop.Status, hop.ErrorCode, hop.Retryable, hop.Body)
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
