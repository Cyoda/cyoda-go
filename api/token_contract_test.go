package api

import (
	"strings"
	"testing"
)

// TestTokenOperation_DeclaresTheJoinLayersAnswers: in mock IAM mode the
// generated route serves POST /tenants/{tenant}/oauth/token behind the join
// layer, so a request that carries an X-Tx-Token gets the join layer's
// answers, and the operation declares them as its sibling administration
// routes do: 404, 409, 410 and 413 by the shared components, and a 403 and a
// 503 that name the joined refusals with a problem body beside the
// operation's own OAuth answers.
func TestTokenOperation_DeclaresTheJoinLayersAnswers(t *testing.T) {
	doc, err := GetSwagger()
	if err != nil {
		t.Fatalf("GetSwagger: %v", err)
	}
	item := doc.Paths.Find("/tenants/{tenant}/oauth/token")
	if item == nil || item.Post == nil {
		t.Fatal("POST /tenants/{tenant}/oauth/token is not declared")
	}
	op := item.Post

	for status, ref := range map[int]string{
		404: "#/components/responses/TransactionNotFound",
		409: "#/components/responses/JoinedTransactionConflict",
		410: "#/components/responses/TransactionGone",
		413: "#/components/responses/JoinedPayloadTooLarge",
	} {
		if got := op.Responses.Status(status); got == nil || got.Ref != ref {
			t.Errorf("%d must be %s", status, ref)
		}
	}

	for status, codes := range map[int][]string{
		403: {"X-Tx-Token", "FORBIDDEN"},
		503: {"X-Tx-Token", "TRANSACTION_NODE_UNAVAILABLE", "TOO_MANY_JOINED_REQUESTS", "TRANSACTION_BUSY"},
	} {
		resp := op.Responses.Status(status)
		if resp == nil || resp.Value == nil {
			t.Errorf("%d is not declared", status)
			continue
		}
		pd := resp.Value.Content["application/problem+json"]
		if pd == nil || pd.Schema == nil || pd.Schema.Ref != "#/components/schemas/ProblemDetail" {
			t.Errorf("%d must declare an application/problem+json ProblemDetail body", status)
		}
		if resp.Value.Content["application/json"] == nil {
			t.Errorf("%d must keep the operation's own application/json body", status)
		}
		desc := ""
		if resp.Value.Description != nil {
			desc = *resp.Value.Description
		}
		for _, code := range codes {
			if !strings.Contains(desc, code) {
				t.Errorf("%d description does not name %s", status, code)
			}
		}
	}
}
