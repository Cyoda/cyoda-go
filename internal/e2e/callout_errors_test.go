package e2e_test

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/app"
)

var (
	calloutFailedRe      = regexp.MustCompile(`the callout could not be completed, got (\d+) failures: (.*)$`)
	calloutFailedEntryRe = regexp.MustCompile(`\[member ?<?([^>:\]\s]+)>?: [^\]]*?(?: \((\d+) times\))?\]`)
)

// calloutFailedMsg is a parsed CALLOUT_FAILED detail.
type calloutFailedMsg struct {
	n         int            // the count the message states
	perMember map[string]int // member id -> failures attributed to it
}

func parseCalloutFailed(t *testing.T, detail string) calloutFailedMsg {
	t.Helper()
	m := calloutFailedRe.FindStringSubmatch(detail)
	if m == nil {
		t.Fatalf("detail = %q; want \"the callout could not be completed, got N failures: [...]\"", detail)
	}
	out := calloutFailedMsg{perMember: map[string]int{}}
	out.n, _ = strconv.Atoi(m[1])
	sum := 0
	for _, e := range calloutFailedEntryRe.FindAllStringSubmatch(m[2], -1) {
		k := 1
		if e[2] != "" {
			k, _ = strconv.Atoi(e[2])
		}
		out.perMember[e[1]] += k
		sum += k
	}
	if sum != out.n {
		t.Errorf("detail = %q; it states %d failures but its entries add up to %d", detail, out.n, sum)
	}
	return out
}

// TestCalloutErrors_EveryTryUsed: two tries, two cnodes, both silent on an
// idempotent processor -> 503 CALLOUT_FAILED naming both members, and nothing
// that identifies a pnode.
func TestCalloutErrors_EveryTryUsed(t *testing.T) {
	const nodeID = "s3-owner-pnode"
	h := newCalloutHarness(t, func(cfg *app.Config) {
		calloutTuning(1, 0)(cfg)
		cfg.Cluster.NodeID = nodeID
	})
	sfx := randSuffix(t) // repeated runs (go test -count=N) share this package's Postgres testcontainer
	tag, model := "s3-all-used-"+sfx, "s3-model-all-used-"+sfx
	a := h.AttachCnode(t, cnodeSpec{name: "a", tags: []string{tag}, script: scriptAlways(neverAnswer())})
	b := h.AttachCnode(t, cnodeSpec{name: "b", tags: []string{tag}, script: scriptAlways(neverAnswer())})
	h.SetupModelWithWorkflow(t, model, chainWorkflowJSON("s3-all-used-wf", procSpec{"s3-proc", "SYNC",
		map[string]any{"calculationNodesTags": tag, "responseTimeoutMs": 300, "idempotent": true}}))

	_, status, body := h.CreateEntity(t, model, 1, workflowSampleModel)
	pd := assertProblem(t, status, body, http.StatusServiceUnavailable, "CALLOUT_FAILED", true)
	msg := parseCalloutFailed(t, pd.Detail)
	if msg.n != 2 || msg.perMember[a.MemberID()] != 1 || msg.perMember[b.MemberID()] != 1 {
		t.Errorf("detail = %q; want 2 failures, one for each of %s and %s", pd.Detail, a.MemberID(), b.MemberID())
	}
	// The full message, character for character (§8.2's shape): both bracket
	// kinds literal, N counted before collapsing, entries in the order they were
	// first tried (a before b: MemberRegistry.Candidates orders by (ConnectedAt,
	// ID), and a was attached first), each CAUSE the try's own "CODE: text".
	cause := "DISPATCH_TIMEOUT: processor dispatch timed out after 300ms: no response"
	wantDetail := fmt.Sprintf("CALLOUT_FAILED: the callout could not be completed, got 2 failures: [member<%s>: %s], [member<%s>: %s]",
		a.MemberID(), cause, b.MemberID(), cause)
	if pd.Detail != wantDetail {
		t.Errorf("detail = %q; want the literal %q", pd.Detail, wantDetail)
	}
	for _, leak := range []string{"127.0.0.1", "localhost", nodeID} {
		if strings.Contains(pd.Detail, leak) {
			t.Errorf("detail = %q; it names a pnode (%q)", pd.Detail, leak)
		}
	}
	if n := h.countEntities(t, model); n != 0 {
		t.Errorf("%d entities committed; want 0", n)
	}
}

// TestCalloutErrors_OneAttemptIsNotWrapped: one try allowed, a second cnode is
// available but never asked -> the try's own code, unwrapped.
func TestCalloutErrors_OneAttemptIsNotWrapped(t *testing.T) {
	h := newCalloutHarness(t, calloutTuning(0, 0))
	sfx := randSuffix(t) // repeated runs (go test -count=N) share this package's Postgres testcontainer
	tag, model := "s3-one-"+sfx, "s3-model-one-"+sfx
	h.AttachCnode(t, cnodeSpec{name: "silent", tags: []string{tag}, script: scriptAlways(neverAnswer())})
	spare := h.AttachCnode(t, cnodeSpec{name: "spare", tags: []string{tag}})
	h.SetupModelWithWorkflow(t, model, chainWorkflowJSON("s3-one-wf", procSpec{"s3-proc", "SYNC",
		map[string]any{"calculationNodesTags": tag, "responseTimeoutMs": 300, "idempotent": true}}))

	_, status, body := h.CreateEntity(t, model, 1, workflowSampleModel)
	pd := assertProblem(t, status, body, http.StatusServiceUnavailable, "DISPATCH_TIMEOUT", true)
	if strings.Contains(pd.Detail, "the callout could not be completed") {
		t.Errorf("detail = %q; a single attempt is reported as itself, not wrapped", pd.Detail)
	}
	if got := spare.Received(); len(got) != 0 {
		t.Errorf("the spare cnode received %d callouts; the one try was used", len(got))
	}
}

// TestCalloutErrors_AttemptsBeatNoCnode: every cnode drops on receiving the
// work, so when the patience runs out there is no cnode left — but tries were
// made, and the error reports them, not NO_COMPUTE_MEMBER_FOR_TAG.
func TestCalloutErrors_AttemptsBeatNoCnode(t *testing.T) {
	h := newCalloutHarness(t, calloutTuning(3, 200*time.Millisecond))
	sfx := randSuffix(t) // repeated runs (go test -count=N) share this package's Postgres testcontainer

	t.Run("one-attempt", func(t *testing.T) {
		tag, model := "s3-beat-1-"+sfx, "s3-model-beat-1-"+sfx
		c := h.AttachCnode(t, cnodeSpec{name: "drop", tags: []string{tag}, script: scriptAlways(closeStream())})
		h.SetupModelWithWorkflow(t, model, chainWorkflowJSON("s3-beat-1-wf", procSpec{"s3-proc", "SYNC",
			map[string]any{"calculationNodesTags": tag, "idempotent": true}}))
		_, status, body := h.CreateEntity(t, model, 1, workflowSampleModel)
		assertProblem(t, status, body, http.StatusServiceUnavailable, "COMPUTE_MEMBER_DISCONNECTED", true)
		c.AwaitGone(t)
	})

	t.Run("two-attempts-tries-left", func(t *testing.T) {
		tag, model := "s3-beat-2-"+sfx, "s3-model-beat-2-"+sfx
		a := h.AttachCnode(t, cnodeSpec{name: "drop-a", tags: []string{tag}, script: scriptAlways(closeStream())})
		b := h.AttachCnode(t, cnodeSpec{name: "drop-b", tags: []string{tag}, script: scriptAlways(closeStream())})
		h.SetupModelWithWorkflow(t, model, chainWorkflowJSON("s3-beat-2-wf", procSpec{"s3-proc", "SYNC",
			map[string]any{"calculationNodesTags": tag, "idempotent": true}}))
		_, status, body := h.CreateEntity(t, model, 1, workflowSampleModel)
		pd := assertProblem(t, status, body, http.StatusServiceUnavailable, "CALLOUT_FAILED", true)
		msg := parseCalloutFailed(t, pd.Detail)
		if msg.n != 2 || msg.perMember[a.MemberID()] != 1 || msg.perMember[b.MemberID()] != 1 {
			t.Errorf("detail = %q; want 2 failures, one per dropped cnode", pd.Detail)
		}
	})
}

// TestCalloutErrors_CollapsedRepeat: the tag's only cnode fails identically in
// two passes — a membership change (any cnode joining, whatever its tag: the
// registry's Changed() fires for every join/leave) wakes the wait the owner
// started once that one pass's only candidate was used up, and the new pass
// retries the same cnode, with the asked set cleared. No sleep: the owner
// takes its wait channel before running the pass, so firing the change any
// time before it gets there — here, as soon as the first try is recorded —
// is enough; the second try is the only cnode there is. Two identical
// failures collapse into one bracketed entry with "(k times)" (§8.2, R§5).
func TestCalloutErrors_CollapsedRepeat(t *testing.T) {
	h := newCalloutHarness(t, calloutTuning(1, time.Second))
	sfx := randSuffix(t) // repeated runs (go test -count=N) share this package's Postgres testcontainer
	tag, model := "s3-collapse-"+sfx, "s3-model-collapse-"+sfx
	solo := h.AttachCnode(t, cnodeSpec{name: "solo", tags: []string{tag}, script: scriptAlways(neverAnswer())})
	h.SetupModelWithWorkflow(t, model, chainWorkflowJSON("s3-collapse-wf", procSpec{"s3-proc", "SYNC",
		map[string]any{"calculationNodesTags": tag, "responseTimeoutMs": 300, "idempotent": true}}))

	created := make(chan createEntityResult, 1)
	go func() { created <- h.CreateEntityRaw(model, 1, workflowSampleModel) }()

	// Wait for the first (and, within this pass, only) try to land, then attach
	// an unrelated cnode: any Register() fires MemberRegistry.Changed()
	// harness-wide, waking the owner's wait into a second pass that retries
	// the same, still-only, candidate for this tag.
	h.AwaitCallouts(t, 1, 5*time.Second)
	throwaway := h.AttachCnode(t, cnodeSpec{name: "throwaway", tags: []string{"s3-collapse-unrelated"}})
	defer throwaway.Detach(t)

	res := <-created
	if res.err != nil {
		t.Fatalf("%v", res.err)
	}
	pd := assertProblem(t, res.status, res.body, http.StatusServiceUnavailable, "CALLOUT_FAILED", true)
	msg := parseCalloutFailed(t, pd.Detail)
	if msg.n != 2 || msg.perMember[solo.MemberID()] != 2 {
		t.Errorf("detail = %q; want 2 failures collapsed onto member %s", pd.Detail, solo.MemberID())
	}
	if !strings.Contains(pd.Detail, "(2 times)") {
		t.Errorf("detail = %q; want the collapsed form \"(2 times)\"", pd.Detail)
	}
	if got := solo.Received(); len(got) != 2 {
		t.Errorf("solo cnode received %d callouts; want 2 (retried across two passes): %v", len(got), got)
	}
}
