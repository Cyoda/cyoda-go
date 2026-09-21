package e2e_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// TestCalloutSelection_RoundRobin: two cnodes of one tag take turns, and a
// cnode that attaches later goes first.
func TestCalloutSelection_RoundRobin(t *testing.T) {
	h := newCalloutHarness(t, calloutTuning(3, 0))
	const model, tag = "s11-rr", "s11-rr"
	h.SetupModelWithWorkflow(t, model, chainWorkflowJSON("s11-rr-wf",
		procSpec{"s11-proc", "SYNC", map[string]any{"calculationNodesTags": tag}}))
	h.AttachCnode(t, cnodeSpec{name: "x", tags: []string{tag}})
	h.AttachCnode(t, cnodeSpec{name: "y", tags: []string{tag}})

	create := func() {
		t.Helper()
		if _, status, body := h.CreateEntity(t, model, 1, workflowSampleModel); status != http.StatusOK {
			t.Fatalf("create: %d %s", status, body)
		}
	}
	for i := 0; i < 4; i++ {
		create()
	}
	h.AttachCnode(t, cnodeSpec{name: "z", tags: []string{tag}})
	create()

	var order []string
	for _, r := range h.ReceivedCallouts() {
		order = append(order, r.Cnode)
	}
	if got, want := strings.Join(order, ","), "x,y,x,y,z"; got != want {
		t.Errorf("callouts went to %s; want %s", got, want)
	}
}

// TestCalloutSelection_TwoTenantsOneTag: cnodes of two tenants join under the
// same tag. Each tenant's callouts go only to its own cnodes, and a failure
// names only its own.
func TestCalloutSelection_TwoTenantsOneTag(t *testing.T) {
	h := newCalloutHarness(t, calloutTuning(1, 0))
	_ = h.token(t)
	clientB, secretB := h.provisionTenant(t, "s11-tenant-b", "s11-user-b")
	bearerB := h.fetchTokenFor(t, clientB, secretB)

	asB := func(method, path, body string) (int, string) {
		resp := h.doAuthBearer(t, bearerB, method, path, body, "")
		return resp.StatusCode, h.readBody(t, resp)
	}
	setupAsB := func(model, wf string) {
		t.Helper()
		for _, step := range []struct{ method, path, body string }{
			{http.MethodPost, fmt.Sprintf("/api/model/import/JSON/SAMPLE_DATA/%s/1", model), workflowSampleModel},
			{http.MethodPut, fmt.Sprintf("/api/model/%s/1/lock", model), ""},
			{http.MethodPost, fmt.Sprintf("/api/model/%s/1/workflow/import", model), wf},
		} {
			if status, body := asB(step.method, step.path, step.body); status != http.StatusOK {
				t.Fatalf("tenant B %s %s: %d %s", step.method, step.path, status, body)
			}
		}
	}

	t.Run("routing", func(t *testing.T) {
		const tag = "s11-shared"
		wf := chainWorkflowJSON("s11-shared-wf", procSpec{"s11-proc", "SYNC", map[string]any{"calculationNodesTags": tag}})
		h.SetupModelWithWorkflow(t, "s11-shared-a", wf)
		setupAsB("s11-shared-b", wf)
		a := h.AttachCnode(t, cnodeSpec{name: "tenant-a", tags: []string{tag}})
		b := h.AttachCnode(t, cnodeSpec{name: "tenant-b", tags: []string{tag}, bearer: bearerB})

		for i := 0; i < 2; i++ { // twice: round robin would alternate if the tenants shared a pool
			if _, status, body := h.CreateEntity(t, "s11-shared-a", 1, workflowSampleModel); status != http.StatusOK {
				t.Fatalf("tenant A create: %d %s", status, body)
			}
		}
		if na, nb := len(a.Received()), len(b.Received()); na != 2 || nb != 0 {
			t.Fatalf("after tenant A's two creates: A's cnode received %d, B's %d; want 2 and 0", na, nb)
		}
		for i := 0; i < 2; i++ {
			if status, body := asB(http.MethodPost, "/api/entity/JSON/s11-shared-b/1", workflowSampleModel); status != http.StatusOK {
				t.Fatalf("tenant B create: %d %s", status, body)
			}
		}
		if na, nb := len(a.Received()), len(b.Received()); na != 2 || nb != 2 {
			t.Errorf("after tenant B's two creates: A's cnode received %d, B's %d; want 2 and 2", na, nb)
		}
	})

	t.Run("a-failure-names-only-the-tenants-own-cnodes", func(t *testing.T) {
		const tag = "s11-shared-fail"
		h.SetupModelWithWorkflow(t, "s11-shared-fail-a", chainWorkflowJSON("s11-shared-fail-wf", procSpec{"s11-proc", "SYNC",
			map[string]any{"calculationNodesTags": tag, "responseTimeoutMs": 300, "idempotent": true}}))
		// b-healthy attaches FIRST: without the tenant comparison in
		// MemberRegistry.Candidates, round robin's "never picked yet" tie-break
		// would put it first in tenant A's own pool and try 1 would go to it —
		// giving this scenario teeth against that revert (see the report for
		// the failure text). With the tenant comparison in place, b-healthy is
		// never a candidate for tenant A's callout regardless of attach order.
		b := h.AttachCnode(t, cnodeSpec{name: "b-healthy", tags: []string{tag}, bearer: bearerB})
		a1 := h.AttachCnode(t, cnodeSpec{name: "a1", tags: []string{tag}, script: scriptAlways(neverAnswer())})
		a2 := h.AttachCnode(t, cnodeSpec{name: "a2", tags: []string{tag}, script: scriptAlways(neverAnswer())})

		_, status, body := h.CreateEntity(t, "s11-shared-fail-a", 1, workflowSampleModel)
		pd := assertProblem(t, status, body, http.StatusServiceUnavailable, "CALLOUT_FAILED", true)
		msg := parseCalloutFailed(t, pd.Detail)
		if msg.n != 2 || msg.perMember[a1.MemberID()] != 1 || msg.perMember[a2.MemberID()] != 1 {
			t.Errorf("detail = %q; want one failure for each of tenant A's two cnodes", pd.Detail)
		}
		if strings.Contains(pd.Detail, b.MemberID()) {
			t.Errorf("detail = %q; it names tenant B's cnode %s", pd.Detail, b.MemberID())
		}
		if got := b.Received(); len(got) != 0 {
			t.Errorf("tenant B's healthy cnode received %d of tenant A's callouts", len(got))
		}
	})

	// TestCalloutSelection_TwoTenantsOneTag/a-tenant-with-no-member-of-its-own
	// covers the matrix cell distinct from "routing": there, both tenants had
	// their own cnode; here, tenant A has none at all for this tag, and only
	// tenant B does. Tenant A must be told NO_COMPUTE_MEMBER_FOR_TAG — never
	// served by borrowing tenant B's cnode.
	t.Run("a-tenant-with-no-member-of-its-own-gets-none-of-anothers", func(t *testing.T) {
		const tag = "s11-only-b"
		h.SetupModelWithWorkflow(t, "s11-only-b-a", chainWorkflowJSON("s11-only-b-wf",
			procSpec{"s11-proc", "SYNC", map[string]any{"calculationNodesTags": tag}}))
		b := h.AttachCnode(t, cnodeSpec{name: "b-only", tags: []string{tag}, bearer: bearerB})

		_, status, body := h.CreateEntity(t, "s11-only-b-a", 1, workflowSampleModel)
		pd := assertProblem(t, status, body, http.StatusServiceUnavailable, "NO_COMPUTE_MEMBER_FOR_TAG", true)
		if strings.Contains(pd.Detail, b.MemberID()) {
			t.Errorf("detail = %q; it names tenant B's cnode %s", pd.Detail, b.MemberID())
		}
		if got := b.Received(); len(got) != 0 {
			t.Errorf("tenant B's cnode received %d of tenant A's callouts; want 0 — tenant A has no member of its own for this tag", len(got))
		}
	})
}
