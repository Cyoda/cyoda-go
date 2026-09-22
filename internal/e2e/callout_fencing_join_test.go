package e2e_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	cyodapb "github.com/cyoda-platform/cyoda-go/api/grpc/cyoda"
)

// callout_fencing_join_test.go is the join layer itself: one transaction, one
// user at a time. Every joined request — a read as much as a write, an entity
// request or not — holds the transaction's lock for the whole of its handler,
// and the lock is held only while the request is wholly in memory, so a compute
// node that stalls its body or never sends its message holds nothing.

// TestCalloutFence_ParallelJoinedRequests: one cnode fires reads and writes at
// its transaction in parallel. Each is answered 200, the operation succeeds
// and every write is in the result — on PostgreSQL, no "conn busy".
//
// It is a consistency assertion, not an interleaving: what is claimed is that
// no request is refused, nothing is torn and the committed result equals a
// serial execution of the successful ones.
func TestCalloutFence_ParallelJoinedRequests(t *testing.T) {
	h := newCalloutHarness(t, calloutTuning(3, 100*time.Millisecond))
	sfx := randSuffix(t) // repeated runs (go test -count=N) share this package's Postgres testcontainer
	model, secondary, tag := "s9-parallel-"+sfx, "s9-parallel-secondary-"+sfx, "s9-parallel-"+sfx
	const reads, writes, rounds = 8, 4, 3
	h.SetupModelWithWorkflow(t, secondary, secondaryWorkflow)
	h.SetupModelWithWorkflow(t, model, chainWorkflowJSON("s9-parallel-wf",
		procSpec{"s9-proc", "SYNC", map[string]any{"calculationNodesTags": tag}}))
	// The read target is a committed entity, not the cascade's own primary: the
	// primary is saved when the transition completes, so it is not there to be
	// read while the processor runs. A joined read of it passes the same join
	// layer either way.
	target := h.seedVictim(t, "s9-parallel-target-"+sfx)

	var mu sync.Mutex
	var bad []string
	h.AttachCnode(t, cnodeSpec{name: "parallel", tags: []string{tag},
		script: func(_ context.Context, _ receivedCallout, rc *reqCtx) cnodeReply {
			var wg sync.WaitGroup
			note := func(what string, res callbackResult, err error) {
				if err == nil && res.StatusCode == http.StatusOK {
					return
				}
				mu.Lock()
				defer mu.Unlock()
				bad = append(bad, fmt.Sprintf("%s: status=%d err=%v body=%s", what, res.StatusCode, err, res.Body))
			}
			for i := 0; i < reads; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					res, err := rc.GetEntity(target)
					note("read", res, err)
				}()
			}
			for i := 0; i < writes; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					res, err := rc.CreateEntity(secondary, 1, `{"name":"parallel-child","amount":1,"status":"new"}`)
					note("write", res, err)
				}()
			}
			wg.Wait()
			return answerOK()
		}})

	for round := 0; round < rounds; round++ {
		if _, status, body := h.CreateEntity(t, model, 1, workflowSampleModel); status != http.StatusOK {
			t.Fatalf("round %d create: %d %s", round, status, body)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bad) != 0 {
		t.Fatalf("%d joined requests were not answered 200: %v", len(bad), bad)
	}
	if n := h.countEntities(t, secondary); n != writes*rounds {
		t.Errorf("%d secondary entities committed; want %d", n, writes*rounds)
	}
}

// TestCalloutFence_NonEntityJoinedRequests (shape F-ended): model, message,
// search, statistics and audit requests pass the same join layer as entity
// requests — admitted with the pass of the callout in progress, refused 410
// with the pass of one that has ended. The gRPC interceptor covers the four
// entity RPCs and no others, so its non-entity family is the search pair,
// unary and server-streaming; both join through the same helper.
func TestCalloutFence_NonEntityJoinedRequests(t *testing.T) {
	h := newCalloutHarness(t, calloutTuning(3, 100*time.Millisecond))
	sfx := randSuffix(t) // repeated runs (go test -count=N) share this package's Postgres testcontainer
	model, committedModel, tagA, tagB := "s9-nonentity-"+sfx, "s9-nonentity-committed-"+sfx, "s9-nonentity-a-"+sfx, "s9-nonentity-b-"+sfx
	h.SetupModelWithWorkflow(t, model, chainWorkflowJSON("s9-nonentity-wf",
		procSpec{"s9-proc-a", "SYNC", map[string]any{"calculationNodesTags": tagA}},
		procSpec{"s9-proc-b", "SYNC", map[string]any{"calculationNodesTags": tagB}}))
	committed := h.seedVictim(t, committedModel)

	resp := h.DoAuth(t, http.MethodPost, "/api/message/new/s9-seed", `{"payload":{"x":1}}`, "")
	if body := h.readBody(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("seed message: %d %s", resp.StatusCode, body)
	}

	release := make(chan struct{})
	releaseNow := closeOnce(release)
	t.Cleanup(releaseNow)
	a := h.AttachCnode(t, cnodeSpec{name: "a", tags: []string{tagA}})
	b := h.AttachCnode(t, cnodeSpec{name: "b", tags: []string{tagB}, script: scriptHold(release, answerOK())})

	done := make(chan createEntityResult, 1)
	go func() { done <- h.CreateEntityRaw(model, 1, workflowSampleModel) }()
	current := awaitCnodeReceived(t, b, 1, 15*time.Second)[0].Pass()
	if got := a.Received(); len(got) == 0 {
		t.Fatalf("cnode a received no callouts; want at least 1 before b was given the work")
	}
	ended := a.Received()[0].Pass()

	requests := []struct{ name, method, path, body string }{
		{"model list", http.MethodGet, "/api/model/", ""},
		{"message create", http.MethodPost, "/api/message/new/s9-joined", `{"payload":{"x":2}}`},
		{"search", http.MethodPost, "/api/search/direct/" + committedModel + "/1", txctlSimpleCond},
		{"statistics", http.MethodGet, "/api/entity/stats", ""},
		{"audit", http.MethodGet, "/api/audit/entity/" + committed, ""},
	}
	for _, rq := range requests {
		res, err := h.callback(rq.method, rq.path, rq.body, ended)
		if err != nil {
			t.Fatalf("%s with the ended pass: %v", rq.name, err)
		}
		pd := assertProblem(t, res.StatusCode, res.Body, http.StatusGone, "CALLOUT_SUPERSEDED", false)
		if pd.Detail == "" {
			t.Errorf("%s: empty detail", rq.name)
		}
		res, err = h.callback(rq.method, rq.path, rq.body, current)
		if err != nil || res.StatusCode != http.StatusOK {
			t.Errorf("%s with the current pass: status=%d err=%v body=%s; want 200", rq.name, res.StatusCode, err, res.Body)
		}
	}

	grpcDoors := []struct {
		name string
		call func(pass string) (txEnvelope, error)
	}{
		{"grpc-search-unary", func(pass string) (txEnvelope, error) { return h.ReplayGetGRPC(pass, committed) }},
		{"grpc-search-stream", func(pass string) (txEnvelope, error) {
			return h.ReplaySearchCollectionGRPC(pass, committedModel, 1)
		}},
	}
	for _, door := range grpcDoors {
		env, err := door.call(ended)
		assertEnvelope(t, door.name, env, err, "CALLOUT_SUPERSEDED", false)
		env, err = door.call(current)
		if err != nil || !env.Success {
			t.Errorf("%s with the current pass: success=%t err=%v error=%v; want a success", door.name, env.Success, err, env.Error)
		}
	}

	releaseNow()
	if res := awaitCreate(t, done, 15*time.Second); res.status != http.StatusOK {
		t.Fatalf("create: %d %s; want 200", res.status, res.body)
	}
}

// TestCalloutFence_StalledBodyHoldsNothing: a cnode opens a joined HTTP request
// and sends only part of its body, and opens a joined gRPC server-streaming
// call and never sends its message. Neither holds the transaction: the cnode's
// next joined request is served, and the operation completes.
//
// Both stalls are gated on the server, with no sleep. The HTTP stall is written
// on a connection whose earlier request the server has already answered, so the
// server is reading that connection when the stalled headers arrive. The gRPC
// stall is opened on the same connection as the joined unary call that follows
// it: an HTTP/2 server reads the connection's frames in order, so the stalled
// call's headers were taken before that unary call's. What remains in either
// case is the server's own step from reading the headers to the join layer,
// which no client can delay — and revert 3 below shows it is reached, because
// taking the lock there makes this scenario hang.
func TestCalloutFence_StalledBodyHoldsNothing(t *testing.T) {
	h := newCalloutHarness(t, calloutTuning(3, 100*time.Millisecond))
	sfx := randSuffix(t) // repeated runs (go test -count=N) share this package's Postgres testcontainer
	model, secondary, tag := "s9-stall-"+sfx, "s9-stall-secondary-"+sfx, "s9-stall-"+sfx
	h.SetupModelWithWorkflow(t, secondary, secondaryWorkflow)
	h.SetupModelWithWorkflow(t, model, chainWorkflowJSON("s9-stall-wf",
		procSpec{"s9-proc", "SYNC", map[string]any{"calculationNodesTags": tag}}))
	target := h.seedVictim(t, "s9-stall-target-"+sfx) // see ParallelJoinedRequests on why not the primary

	u, err := url.Parse(h.baseURL)
	if err != nil {
		t.Fatalf("parse base url: %v", err)
	}
	bearer, _ := h.bearerVal.Load().(string)
	stallCtx, stopStalling := context.WithCancel(context.Background())
	t.Cleanup(stopStalling)

	problems := make(chan string, 8)
	h.AttachCnode(t, cnodeSpec{name: "staller", tags: []string{tag},
		script: func(_ context.Context, call receivedCallout, rc *reqCtx) cnodeReply {
			// 1. HTTP: a complete request first, so the server is demonstrably
			// reading this connection; then headers and 9 of 64 promised body
			// bytes, then silence.
			conn, err := net.Dial("tcp", u.Host)
			if err != nil {
				problems <- "dial: " + err.Error()
				return answerFail("harness")
			}
			go func() { <-stallCtx.Done(); _ = conn.Close() }()
			fmt.Fprintf(conn, "GET /api/model/ HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\n\r\n", u.Host, bearer)
			probe, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				problems <- "read the handshake response: " + err.Error()
				return answerFail("harness")
			}
			_, _ = io.Copy(io.Discard, probe.Body)
			_ = probe.Body.Close()
			if probe.StatusCode != http.StatusOK {
				problems <- fmt.Sprintf("the handshake request was answered %d; want 200", probe.StatusCode)
				return answerFail("harness")
			}
			fmt.Fprintf(conn, "POST /api/entity/JSON/%s/1 HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\n"+
				"X-Tx-Token: %s\r\nContent-Type: application/json\r\nContent-Length: 64\r\n\r\n{\"name\":\"", secondary, u.Host, bearer, call.Pass())

			// 2. gRPC server-streaming: the call is opened, its one message never sent.
			md := h.grpcCtx(call.Pass())
			streamCtx, cancel := context.WithCancel(md)
			go func() { <-stallCtx.Done(); cancel() }()
			if _, err := h.apiConn.NewStream(streamCtx, &grpc.StreamDesc{ServerStreams: true},
				cyodapb.CloudEventsService_EntityManageCollection_FullMethodName); err != nil {
				problems <- "open stream: " + err.Error()
			}

			// 3. An ordinary joined read and write must not wait for either. The
			// gRPC read goes over the same connection as the stalled call, which
			// is what orders the two.
			if env, err := h.ReplayGetGRPC(call.Pass(), target); err != nil || !env.Success {
				problems <- fmt.Sprintf("joined gRPC read behind the stalled call: success=%t err=%v error=%v", env.Success, err, env.Error)
			}
			if res, err := rc.GetEntity(target); err != nil || res.StatusCode != http.StatusOK {
				problems <- fmt.Sprintf("joined read behind the stalled requests: status=%d err=%v", res.StatusCode, err)
			}
			if res, err := rc.CreateEntity(secondary, 1, `{"name":"after-stall","amount":1,"status":"new"}`); err != nil || res.StatusCode != http.StatusOK {
				problems <- fmt.Sprintf("joined write behind the stalled requests: status=%d err=%v", res.StatusCode, err)
			}
			return answerOK()
		}})

	done := make(chan createEntityResult, 1)
	go func() { done <- h.CreateEntityRaw(model, 1, workflowSampleModel) }()
	if res := awaitCreate(t, done, 10*time.Second); res.status != http.StatusOK {
		t.Fatalf("create: %d %s; want 200 while both stalled requests are still open", res.status, res.body)
	}
	close(problems) // safe: the script has returned — its reply is what let the create finish
	for p := range problems {
		t.Error(p)
	}
	if n := h.countEntities(t, secondary); n != 1 {
		t.Errorf("%d secondary entities committed; want 1 (the stalled request wrote nothing)", n)
	}
}
