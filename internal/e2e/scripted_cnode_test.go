package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
)

// scripted_cnode_test.go lets a test attach SEVERAL cnodes to one callout
// harness, each with tags of its own and a script deciding what it does with
// each callout, and records what every cnode received. A pass is held in
// memory only and is never printed (Gate 3).

// receivedCallout is one callout as a scripted cnode received it.
type receivedCallout struct {
	Seq       int    // 1-based arrival order across every scripted cnode of the harness
	Cnode     string // cnodeSpec.name
	MemberID  string // the id the server gave this cnode in its greet
	Kind      string // calloutProcessor | calloutCriterion | calloutFunction
	Name      string // processor / criterion / function name
	RequestID string // the payload's requestId
	EventID   string // the CloudEvent id
	EntityID  string

	pass string
}

// Pass returns the transaction token the callout carried ("" when it carried
// none), for presenting it again later. Never log or print it.
func (r receivedCallout) Pass() string { return r.pass }

// String and GoString keep the pass out of every fmt verb.
func (r receivedCallout) String() string {
	return fmt.Sprintf("{#%d cnode=%s member=%s %s %q requestId=%s eventId=%s entity=%s pass=%t}",
		r.Seq, r.Cnode, r.MemberID, r.Kind, r.Name, r.RequestID, r.EventID, r.EntityID, r.pass != "")
}
func (r receivedCallout) GoString() string { return r.String() }

// calloutLog is the harness-wide record, in arrival order.
type calloutLog struct {
	mu   sync.Mutex
	recs []receivedCallout
}

func (l *calloutLog) add(r receivedCallout) receivedCallout {
	l.mu.Lock()
	defer l.mu.Unlock()
	r.Seq = len(l.recs) + 1
	l.recs = append(l.recs, r)
	return r
}

func (l *calloutLog) snapshot() []receivedCallout {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]receivedCallout(nil), l.recs...)
}

// cnodeScript decides what a cnode does with one callout. It runs on a
// goroutine of its own: it may block (typically on a channel the test closes)
// and may make callbacks through rc before it replies, but it must not call
// t.Fatal. ctx ends when the cnode is detached or closes its stream.
type cnodeScript func(ctx context.Context, call receivedCallout, rc *reqCtx) cnodeReply

// scriptAlways gives every callout the same reply.
func scriptAlways(r cnodeReply) cnodeScript {
	return func(context.Context, receivedCallout, *reqCtx) cnodeReply { return r }
}

// scriptSequence gives the n-th callout this cnode receives the n-th reply;
// the last reply repeats.
func scriptSequence(replies ...cnodeReply) cnodeScript {
	var n atomic.Int64
	return func(context.Context, receivedCallout, *reqCtx) cnodeReply {
		i := int(n.Add(1)) - 1
		if i >= len(replies) {
			i = len(replies) - 1
		}
		return replies[i]
	}
}

// cnodeSpec describes one scripted cnode.
type cnodeSpec struct {
	name   string      // label in records and failure messages; required
	tags   []string    // join tags
	bearer string      // M2M bearer to join with; "" = the harness's tenant
	script cnodeScript // nil = scriptAlways(answerOK())
}

type scriptedCnode struct {
	h      *callbackHarness
	name   string
	script cnodeScript
	m      *computeMember
}

// AttachCnode connects a scripted cnode and returns once the server has
// registered it. It may be called at any point in a test.
func (h *callbackHarness) AttachCnode(t *testing.T, spec cnodeSpec) *scriptedCnode {
	t.Helper()
	if spec.name == "" {
		t.Fatal("cnodeSpec.name is required")
	}
	c := &scriptedCnode{h: h, name: spec.name, script: spec.script}
	if c.script == nil {
		c.script = scriptAlways(answerOK())
	}
	c.m = newComputeMember(t, h, memberSpec{bearer: spec.bearer, tags: spec.tags, handle: c.handle})
	t.Cleanup(c.m.stop)
	return c
}

// handle records the callout, runs the script and carries out its reply. It
// takes the member from its argument, not from c.m: work can arrive before
// AttachCnode has returned.
func (c *scriptedCnode) handle(m *computeMember, send func(*cepb.CloudEvent) error, req calcRequest) {
	call := c.h.callouts.add(receivedCallout{
		Cnode: c.name, MemberID: m.id, Kind: req.kind, Name: req.name,
		RequestID: req.requestID, EventID: req.eventID, EntityID: req.rc.entityID,
		pass: req.rc.token,
	})
	reply := c.script(m.ctx, call, req.rc)
	if reply.kind == replyCloseStream {
		m.closeStream()
		return
	}
	sendReply(send, req, reply)
}

func (c *scriptedCnode) MemberID() string { return c.m.id }

// Received returns what this cnode received, in arrival order.
func (c *scriptedCnode) Received() []receivedCallout {
	var out []receivedCallout
	for _, r := range c.h.callouts.snapshot() {
		if r.MemberID == c.m.id {
			out = append(out, r)
		}
	}
	return out
}

// AwaitGone returns once the server's registry no longer holds this cnode.
func (c *scriptedCnode) AwaitGone(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for c.h.app.MemberRegistry().Get(c.m.id) != nil {
		if time.Now().After(deadline) {
			t.Fatalf("cnode %s (member %s) is still registered 5s after its stream ended", c.name, c.m.id)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Detach closes the cnode's connection and returns once the server has
// dropped it. Calling it again is harmless.
func (c *scriptedCnode) Detach(t *testing.T) {
	t.Helper()
	c.m.stop()
	c.AwaitGone(t)
}

// ReceivedCallouts returns every callout any scripted cnode of this harness
// received, in arrival order.
func (h *callbackHarness) ReceivedCallouts() []receivedCallout { return h.callouts.snapshot() }

// AwaitCallouts waits until at least n callouts are recorded and returns them.
func (h *callbackHarness) AwaitCallouts(t *testing.T, n int, within time.Duration) []receivedCallout {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		recs := h.callouts.snapshot()
		if len(recs) >= n {
			return recs
		}
		if time.Now().After(deadline) {
			t.Fatalf("recorded %d callouts within %s; want at least %d: %v", len(recs), within, n, recs)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// registeredScript is the default cnode's script: it runs the closure
// registered under the callout's name.
func (h *callbackHarness) registeredScript(_ context.Context, call receivedCallout, rc *reqCtx) cnodeReply {
	return h.registeredReply(call.Kind, call.Name, rc)
}

// procWorkflowJSON builds a NONE -> DONE workflow whose one automated
// transition carries one processor. config is merged over
// {"attachEntity": true, "calculationNodesTags": ""}.
func procWorkflowJSON(wfName, procName, mode string, config map[string]any) string {
	cfg := map[string]any{"attachEntity": true, "calculationNodesTags": ""}
	for k, v := range config {
		cfg[k] = v
	}
	b, _ := json.Marshal(map[string]any{
		"importMode": "REPLACE",
		"workflows": []any{map[string]any{
			"version": "1.1", "name": wfName, "initialState": "NONE", "active": true,
			"states": map[string]any{
				"NONE": map[string]any{"transitions": []any{map[string]any{
					"name": "init", "next": "DONE", "manual": false,
					"processors": []any{map[string]any{
						"type": "calculator", "name": procName, "executionMode": mode, "config": cfg,
					}},
				}}},
				"DONE": map[string]any{},
			},
		}},
	})
	return string(b)
}
