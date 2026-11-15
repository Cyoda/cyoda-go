package e2e_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sync/atomic"
	"testing"
)

// by_transaction_read_test.go — GET /entity/{id}?transactionId=X through the
// full HTTP stack. An entity has at most one version per transaction, carrying
// the transaction's last write, so the read returns the entity as X left it.
// It reads committed versions only: before X commits it is 404
// ENTITY_NOT_FOUND for every caller, a callback inside X too. A transaction
// that deleted the entity is 404 as well: its one version is the tombstone.
//
// Each test runs the manual transition "go" of conflictDoorWorkflow on an
// entity E, whose SYNC processor writes another entity T through joined
// callbacks, so X writes an entity more than once.

const byTxGoBody = `{"name":"e","amount":1,"status":"draft"}`

// byTxSetup returns a harness whose "go" runs the scripted SYNC processor
// script (once per request), and the model its workflow is imported under.
func byTxSetup(t *testing.T, script func(rc *reqCtx) cnodeReply) (*callbackHarness, string) {
	t.Helper()
	h := newCalloutHarness(t, nil)
	model, tag := uniq("bytx"), uniq("bytx-tag")
	h.AttachCnode(t, cnodeSpec{name: "p", tags: []string{tag}, script: func(_ context.Context, _ receivedCallout, rc *reqCtx) cnodeReply {
		return script(rc)
	}})
	h.SetupModelWithWorkflow(t, model, conflictDoorWorkflow(model, tag, conflictProc{name: "bytx-p", mode: "SYNC"}))
	return h, model
}

// byTxGo runs the manual transition "go" on id and returns the request's
// transaction id.
func byTxGo(t *testing.T, h *callbackHarness, id string) string {
	t.Helper()
	res, err := h.callback(http.MethodPut, "/api/entity/JSON/"+id+"/go", byTxGoBody, "")
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("go: %v %d %s", err, res.StatusCode, res.Body)
	}
	return txIDOf(t, res.Body)
}

// txIDOf is the transactionId of an update's answer
// {"transactionId":..., "entityIds":[...]}.
func txIDOf(t *testing.T, body string) string {
	t.Helper()
	txID := txIDOfBody(body)
	if txID == "" {
		t.Fatalf("no transactionId in %s", body)
	}
	return txID
}

// txIDOfBody is txIDOf off the test goroutine: "" when body names none.
func txIDOfBody(body string) string {
	var res struct {
		TransactionID string `json:"transactionId"`
	}
	if json.Unmarshal([]byte(body), &res) != nil {
		return ""
	}
	return res.TransactionID
}

func byTxPath(id, txID string) string {
	return "/api/entity/" + id + "?transactionId=" + url.QueryEscape(txID)
}

// byTxGet is the client read of id as txID left it.
func byTxGet(t *testing.T, h *callbackHarness, id, txID string) (int, string) {
	t.Helper()
	resp := h.DoAuth(t, http.MethodGet, byTxPath(id, txID), "", "")
	return resp.StatusCode, h.readBody(t, resp)
}

// entityStateAndData decodes an entity envelope.
func entityStateAndData(t *testing.T, body string) (string, map[string]any) {
	t.Helper()
	var env struct {
		Meta struct {
			State string `json:"state"`
		} `json:"meta"`
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode entity: %v: %s", err, body)
	}
	return env.Meta.State, env.Data
}

// TestByTransaction_FinalEntityAfterCommit — X writes T twice through the
// processor's joined callbacks (amount 7, then 8), and writes E twice itself
// (the save before the callout, as the request changed E, and the final save
// in DONE). The read of each by X returns X's final entity, and /changes of
// each lists one entry for X.
func TestByTransaction_FinalEntityAfterCommit(t *testing.T) {
	var target atomic.Value // string: T's id
	var calls atomic.Int32
	failed := make(chan string, 1)
	h, model := byTxSetup(t, func(rc *reqCtx) cnodeReply {
		if calls.Add(1) > 1 {
			return answerOK()
		}
		tID, _ := target.Load().(string)
		for _, amount := range []string{"7", "8"} {
			res, err := rc.UpdateEntity(tID, `{"name":"t","amount":`+amount+`,"status":"self"}`)
			if err != nil || res.StatusCode != http.StatusOK {
				failed <- res.Body
				return answerFail("the joined write of T was refused")
			}
		}
		return answerOK()
	})
	tID, _ := createWithTx(t, h, model, workflowSampleModel)
	target.Store(tID)
	eID, _ := createWithTx(t, h, model, workflowSampleModel)

	txID := byTxGo(t, h, eID)
	select {
	case body := <-failed:
		t.Fatalf("a joined write of T failed: %s", body)
	default:
	}

	status, body := byTxGet(t, h, tID, txID)
	if status != http.StatusOK {
		t.Fatalf("GET T ?transactionId=%s answered %d; want 200: %s", txID, status, body)
	}
	if state, data := entityStateAndData(t, body); state != "OPEN" || data["amount"] != float64(8) {
		t.Fatalf("by-transaction read of T is %s with amount %v; want X's last write, OPEN with amount 8", state, data["amount"])
	}
	status, body = byTxGet(t, h, eID, txID)
	if status != http.StatusOK {
		t.Fatalf("GET E ?transactionId=%s answered %d; want 200: %s", txID, status, body)
	}
	if state, data := entityStateAndData(t, body); state != "DONE" || data["amount"] != float64(1) {
		t.Fatalf("by-transaction read of E is %s with amount %v; want X's final entity, DONE with amount 1", state, data["amount"])
	}

	for what, id := range map[string]string{"T": tID, "E": eID} {
		changes := h.getChanges(t, id)
		if len(changes) != 2 || changes[0]["transactionId"] != txID || changes[0]["changeType"] != "UPDATE" || changes[1]["changeType"] != "CREATE" {
			t.Fatalf("changes of %s = %v; want [UPDATE by %s, CREATE]: one version per transaction", what, changes, txID)
		}
	}
}

// TestByTransaction_InsideWritingTx_404 — a callback inside X writes T and
// reads it with ?transactionId=X. X has not committed, so the read is 404
// ENTITY_NOT_FOUND inside X as outside it; a plain read under the token finds
// T (the callback did join X). Once X commits the same read returns X's T.
func TestByTransaction_InsideWritingTx_404(t *testing.T) {
	type observed struct {
		writeTx                     string
		plainStatus                 int
		insideStatus, outsideStatus int
		insideBody, outsideBody     string
	}
	var target atomic.Value // string: T's id
	var calls atomic.Int32
	seen := make(chan observed, 1)
	h, model := byTxSetup(t, func(rc *reqCtx) cnodeReply {
		if calls.Add(1) > 1 {
			return answerOK()
		}
		var o observed
		defer func() { seen <- o }()
		tID, _ := target.Load().(string)
		res, err := rc.UpdateEntity(tID, `{"name":"t","amount":7,"status":"self"}`)
		if err != nil || res.StatusCode != http.StatusOK {
			return answerFail("the joined write of T was refused")
		}
		o.writeTx = txIDOfBody(res.Body)
		if plain, err := rc.GetEntity(tID); err == nil {
			o.plainStatus = plain.StatusCode
		}
		if inside, err := rc.h.callback(http.MethodGet, byTxPath(tID, o.writeTx), "", rc.token); err == nil {
			o.insideStatus, o.insideBody = inside.StatusCode, inside.Body
		}
		if outside, err := rc.h.callback(http.MethodGet, byTxPath(tID, o.writeTx), "", ""); err == nil {
			o.outsideStatus, o.outsideBody = outside.StatusCode, outside.Body
		}
		return answerOK()
	})
	tID, _ := createWithTx(t, h, model, workflowSampleModel)
	target.Store(tID)
	eID, _ := createWithTx(t, h, model, workflowSampleModel)

	txID := byTxGo(t, h, eID)
	var o observed
	select {
	case o = <-seen:
	default:
		t.Fatal("the processor did not run")
	}
	if o.writeTx != txID {
		t.Fatalf("the joined write answered transaction %q; want the request's %s", o.writeTx, txID)
	}
	if o.plainStatus != http.StatusOK {
		t.Fatalf("plain read of T under the token answered %d; want 200 (the callback joined X)", o.plainStatus)
	}
	if code := problemErrorCode(o.insideBody); o.insideStatus != http.StatusNotFound || code != "ENTITY_NOT_FOUND" {
		t.Fatalf("GET ?transactionId=X inside X answered %d %s; want 404 ENTITY_NOT_FOUND: %s", o.insideStatus, code, o.insideBody)
	}
	if code := problemErrorCode(o.outsideBody); o.outsideStatus != http.StatusNotFound || code != "ENTITY_NOT_FOUND" {
		t.Fatalf("GET ?transactionId=X outside X before its commit answered %d %s; want 404 ENTITY_NOT_FOUND: %s", o.outsideStatus, code, o.outsideBody)
	}

	status, body := byTxGet(t, h, tID, txID)
	if status != http.StatusOK {
		t.Fatalf("GET ?transactionId=%s after the commit answered %d; want 200: %s", txID, status, body)
	}
	if state, data := entityStateAndData(t, body); state != "OPEN" || data["amount"] != float64(7) {
		t.Fatalf("by-transaction read of T after the commit is %s with amount %v; want OPEN with amount 7", state, data["amount"])
	}
}

// TestByTransaction_TxDeletedEntity_404 — X writes another entity T through a
// joined callback and then deletes it. T's one version for X is the
// tombstone, so the read of T by X is 404 ENTITY_NOT_FOUND, and /changes of T
// lists the delete and the create only.
func TestByTransaction_TxDeletedEntity_404(t *testing.T) {
	var target atomic.Value // string: T's id
	var calls atomic.Int32
	failed := make(chan string, 1)
	h, model := byTxSetup(t, func(rc *reqCtx) cnodeReply {
		if calls.Add(1) > 1 {
			return answerOK()
		}
		tID, _ := target.Load().(string)
		upd, err := rc.UpdateEntity(tID, `{"name":"t","amount":7,"status":"self"}`)
		if err != nil || upd.StatusCode != http.StatusOK {
			failed <- "update: " + upd.Body
			return answerFail("the joined write of T was refused")
		}
		del, err := rc.DeleteEntity(tID)
		if err != nil || del.StatusCode != http.StatusOK {
			failed <- "delete: " + del.Body
			return answerFail("the joined delete of T was refused")
		}
		return answerOK()
	})
	tID, tCreateTx := createWithTx(t, h, model, workflowSampleModel)
	target.Store(tID)
	eID, _ := createWithTx(t, h, model, workflowSampleModel)

	txID := byTxGo(t, h, eID)
	select {
	case body := <-failed:
		t.Fatalf("the joined %s", body)
	default:
	}

	status, body := byTxGet(t, h, tID, txID)
	if code := problemErrorCode(body); status != http.StatusNotFound || code != "ENTITY_NOT_FOUND" {
		t.Fatalf("GET ?transactionId=X of the entity X deleted answered %d %s; want 404 ENTITY_NOT_FOUND: %s", status, code, body)
	}
	// A tombstone's entry carries no transactionId (the handler omits it when
	// the version has no entity), so X is named by elimination: the only
	// other entry is T's own create.
	changes := h.getChanges(t, tID)
	if len(changes) != 2 || changes[0]["changeType"] != "DELETE" || changes[1]["changeType"] != "CREATE" {
		t.Fatalf("changes of T = %v; want [DELETE, CREATE]: X's update and delete are one version", changes)
	}
	if _, named := changes[0]["transactionId"]; named || changes[1]["transactionId"] != tCreateTx {
		t.Fatalf("changes of T = %v; want the DELETE without a transactionId and the CREATE by %s", changes, tCreateTx)
	}
}
