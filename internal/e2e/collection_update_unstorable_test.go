package e2e_test

import (
	"net/http"
	"strings"
	"testing"
)

// TestUpdateCollection_UnstorableText_400: a collection update whose body holds
// U+0000 outside an item's payload — here an item's `ifMatch` — is refused with
// 400 BAD_REQUEST before any item runs, as a workflow import or an entity
// payload holding it is. The item's `ifMatch` and `transition` reach that item's
// audit events, which record no event holding U+0000, and a failed item is
// isolated while its siblings commit, so accepting such a body would commit
// the batch without the failed item's record.
func TestUpdateCollection_UnstorableText_400(t *testing.T) {
	const model = "e2e-ucol-unstorable"
	setupModelWithWorkflow(t, model, trivialModelWF())
	id1, id2 := seedTwo(t, model)
	before := getEntityTxID(t, id2)

	status, body := putUpdateCollection(t, []string{
		updateCollectionItem(id1, `{"name":"A","amount":5,"status":"new"}`, `\u0000`),
		updateCollectionItem(id2, `{"name":"B","amount":6,"status":"new"}`, ""),
	}, "")
	if status != http.StatusBadRequest || !strings.Contains(body, "BAD_REQUEST") {
		t.Fatalf("expected 400 BAD_REQUEST for a body holding U+0000, got %d: %s", status, body)
	}
	if strings.ContainsRune(body, 0) {
		t.Errorf("the response echoes the raw U+0000: %q", body)
	}
	if after := getEntityTxID(t, id2); after != before {
		t.Errorf("the sibling item was written (transactionId %s -> %s); want nothing committed", before, after)
	}
}
