package e2e_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// TestUpdateCollection_UnstorableText_400: a collection update whose body holds
// text no backend can store outside an item's payload — in an item's `ifMatch`
// or `transition` — is refused with 400 BAD_REQUEST before any item runs, as a
// workflow import or an entity payload holding it is. Those fields reach the
// item's audit events, and the audit trail cannot hold U+0000 on PostgreSQL.
func TestUpdateCollection_UnstorableText_400(t *testing.T) {
	for name, item := range map[string]func(id string) string{
		"ifMatch holding U+0000": func(id string) string {
			return updateCollectionItem(id, `{"name":"A","amount":5,"status":"new"}`, `\u0000`)
		},
		"ifMatch holding an unpaired surrogate": func(id string) string {
			return updateCollectionItem(id, `{"name":"A","amount":5,"status":"new"}`, `\ud800`)
		},
		"transition holding U+0000": func(id string) string {
			return fmt.Sprintf(`{"id":"%s","payload":"{\"name\":\"A\",\"amount\":5,\"status\":\"new\"}","transition":"go\u0000"}`, id)
		},
	} {
		t.Run(name, func(t *testing.T) {
			model := "e2e-ucol-unstorable-" + randSuffix(t)
			setupModelWithWorkflow(t, model, trivialModelWF())
			id1, id2 := seedTwo(t, model)
			before := getEntityTxID(t, id2)

			status, body := putUpdateCollection(t, []string{
				item(id1),
				updateCollectionItem(id2, `{"name":"B","amount":6,"status":"new"}`, ""),
			}, "")
			if status != http.StatusBadRequest || !strings.Contains(body, "BAD_REQUEST") {
				t.Fatalf("expected 400 BAD_REQUEST, got %d: %s", status, body)
			}
			if strings.ContainsRune(body, 0) {
				t.Errorf("the response echoes the raw U+0000: %q", body)
			}
			if after := getEntityTxID(t, id2); after != before {
				t.Errorf("the sibling item was written (transactionId %s -> %s); want nothing committed", before, after)
			}
		})
	}
}

// TestUpdateCollection_PayloadHoldingEscapedBackslashU_IsStored: the whole-body
// check refuses only text no backend can store. A payload whose JSON holds the
// six characters `\u0000` as text (an escaped backslash, then `u0000`) holds no
// U+0000, so the update commits.
func TestUpdateCollection_PayloadHoldingEscapedBackslashU_IsStored(t *testing.T) {
	model := "e2e-ucol-backslash-u-" + randSuffix(t)
	setupModelWithWorkflow(t, model, trivialModelWF())
	id1, _ := seedTwo(t, model)

	// The payload's JSON is {"name":"a\\u0000b",...}: its name is the text
	// a\u0000b. updateCollectionItem escapes the quotes for the outer string;
	// the backslashes are escaped here.
	payload := `{"name":"a\\\\u0000b","amount":5,"status":"new"}`
	status, body := putUpdateCollection(t, []string{updateCollectionItem(id1, payload, "")}, "")
	if status != http.StatusOK {
		t.Fatalf("expected 200 for a payload holding the text \\u0000, got %d: %s", status, body)
	}
}
