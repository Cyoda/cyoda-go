package e2e_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	cyodapb "github.com/cyoda-platform/cyoda-go/api/grpc/cyoda"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
)

// callback_read_current_test.go — a callout that carries the transaction
// token sees the entity its workflow runs for (the anchor) as the workflow
// has it at that moment: with the data the request sent, the state reached
// so far, and found on a create. Every read door under the token answers so —
// GET, search, count and page, over HTTP and gRPC — for a SYNC and an
// ASYNC_NEW_TX processor, a function criterion of the cascade, the workflow
// selection criterion and a schedule function (on a client door and in a
// scheduled fire).
//
// The callouts only READ the anchor. A callout never writes the entity it
// runs for; the tests that write here write other entities, or are the test
// itself acting as another client.

// anchorSeen is what one callout's joined GET of its anchor returned.
type anchorSeen struct {
	Status int    // HTTP status of the joined GET
	State  string // meta.state
	Marker string // data.status
	TxID   string // meta.transactionId the read returned
	// PayloadTxID is the meta.transactionId the callout's own payload
	// carried (empty when the callout attached no entity).
	PayloadTxID string
}

// readAnchorJoined GETs the callout's anchor under the callout's token.
func readAnchorJoined(rc *reqCtx) anchorSeen {
	res, err := rc.GetEntity(rc.entityID)
	if err != nil {
		return anchorSeen{Status: -1, Marker: err.Error()}
	}
	seen := anchorSeen{Status: res.StatusCode}
	var env struct {
		Meta map[string]any `json:"meta"`
		Data map[string]any `json:"data"`
	}
	if res.StatusCode == http.StatusOK && json.Unmarshal([]byte(res.Body), &env) == nil {
		seen.State, _ = env.Meta["state"].(string)
		seen.TxID, _ = env.Meta["transactionId"].(string)
		seen.Marker, _ = env.Data["status"].(string)
	}
	seen.PayloadTxID, _ = rc.entityMeta["transactionId"].(string)
	return seen
}

// seenLog records what callouts observed, by a key the test chooses. The
// callouts run on the cnode's goroutines; the test reads after its request.
type seenLog struct {
	mu   sync.Mutex
	vals map[string]any
}

func newSeenLog() *seenLog { return &seenLog{vals: map[string]any{}} }

func (l *seenLog) put(key string, v any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.vals[key] = v
}

func (l *seenLog) get(t *testing.T, key string) any {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	v, ok := l.vals[key]
	if !ok {
		t.Fatalf("no callout recorded %q", key)
	}
	return v
}

func (l *seenLog) lookup(key string) (any, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	v, ok := l.vals[key]
	return v, ok
}

func (l *seenLog) keys() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	keys := make([]string, 0, len(l.vals))
	for k := range l.vals {
		keys = append(keys, k)
	}
	return keys
}

func (l *seenLog) anchor(t *testing.T, key string) anchorSeen {
	t.Helper()
	return l.get(t, key).(anchorSeen)
}

// requireCurrentAnchor asserts a joined read found the anchor with the data
// and state the workflow had when the callout was made.
func requireCurrentAnchor(t *testing.T, what string, seen anchorSeen, wantState, wantMarker string) {
	t.Helper()
	if seen.Status != http.StatusOK {
		t.Fatalf("%s: joined GET of the anchor answered %d; want 200 (the transaction holds the anchor before the callout)", what, seen.Status)
	}
	if seen.State != wantState || seen.Marker != wantMarker {
		t.Errorf("%s: joined GET saw state=%q status=%q; want state=%q status=%q (the anchor as the workflow has it)",
			what, seen.State, seen.Marker, wantState, wantMarker)
	}
}

// updateTransitionHTTP issues PUT /entity/JSON/{id}/{transition} with data and
// no token, off the test goroutine if needed.
func (h *callbackHarness) updateTransitionHTTP(entityID, transition, data string) (callbackResult, error) {
	return h.callback(http.MethodPut, fmt.Sprintf("/api/entity/JSON/%s/%s", entityID, transition), data, "")
}

// pageStatuses lists the model's first page under txToken (HTTP) and returns
// each entity's data.status by entity id.
func (rc *reqCtx) pageStatuses(model string) (map[string]string, int, error) {
	res, err := rc.h.callback(http.MethodGet, fmt.Sprintf("/api/entity/%s/1?pageSize=50", model), "", rc.token)
	if err != nil {
		return nil, 0, err
	}
	out := map[string]string{}
	if res.StatusCode != http.StatusOK {
		return out, res.StatusCode, nil
	}
	var page []struct {
		Meta struct {
			ID string `json:"id"`
		} `json:"meta"`
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(res.Body), &page); err != nil {
		return nil, res.StatusCode, fmt.Errorf("decode page: %w", err)
	}
	for _, e := range page {
		s, _ := e.Data["status"].(string)
		out[e.Meta.ID] = s
	}
	return out, res.StatusCode, nil
}

// statusCountHTTP counts the model's entities whose data.status is marker,
// under txToken, with the grouped statistics endpoint.
func (rc *reqCtx) statusCountHTTP(model, marker string) (int, error) {
	res, err := rc.h.callback(http.MethodPost, fmt.Sprintf("/api/entity/stats/%s/1/query", model),
		`{"groupBy":["$.status"]}`, rc.token)
	if err != nil {
		return 0, err
	}
	if res.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("grouped stats: %d %s", res.StatusCode, res.Body)
	}
	var buckets []map[string]any
	if err := json.Unmarshal([]byte(res.Body), &buckets); err != nil {
		return 0, fmt.Errorf("decode grouped stats: %w", err)
	}
	if b := findBucket(buckets, "$.status", marker); b != nil {
		n, _ := b["count"].(float64)
		return int(n), nil
	}
	return 0, nil
}

// modelCountHTTP is GET /entity/stats/{model}/1 under txToken.
func (rc *reqCtx) modelCountHTTP(model string) (int, error) {
	res, err := rc.h.callback(http.MethodGet, fmt.Sprintf("/api/entity/stats/%s/1", model), "", rc.token)
	if err != nil {
		return 0, err
	}
	if res.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("stats: %d %s", res.StatusCode, res.Body)
	}
	var body struct {
		Count float64 `json:"count"`
	}
	if err := json.Unmarshal([]byte(res.Body), &body); err != nil {
		return 0, fmt.Errorf("decode stats: %w", err)
	}
	return int(body.Count), nil
}

// searchCollectionGRPC runs one EntitySearchCollection request under joinTok
// and returns the data.status of every entity frame. An error frame is an
// error.
func (h *callbackHarness) searchCollectionGRPC(body map[string]any, eventType, joinTok string) ([]map[string]any, error) {
	reqCE, err := internalgrpc.NewCloudEvent(eventType, body)
	if err != nil {
		return nil, err
	}
	stream, err := cyodapb.NewCloudEventsServiceClient(h.apiConn).EntitySearchCollection(h.grpcCtx(joinTok), reqCE)
	if err != nil {
		return nil, err
	}
	var frames []map[string]any
	for {
		frame, rerr := stream.Recv()
		if errors.Is(rerr, io.EOF) {
			return frames, nil
		}
		if rerr != nil {
			return frames, rerr
		}
		_, payload, perr := internalgrpc.ParseCloudEvent(frame)
		if perr != nil {
			return frames, perr
		}
		var m map[string]any
		if err := json.Unmarshal(payload, &m); err != nil {
			return frames, err
		}
		if ok, _ := m["success"].(bool); !ok {
			return frames, fmt.Errorf("error frame: %s", payload)
		}
		frames = append(frames, m)
	}
}

// pageStatusesGRPC is pageStatuses over gRPC (EntityGetAllRequest).
func (rc *reqCtx) pageStatusesGRPC(model string) (map[string]string, error) {
	frames, err := rc.h.searchCollectionGRPC(map[string]any{
		"id": "cbrc-getall", "model": map[string]any{"name": model, "version": 1}, "pageSize": 50,
	}, internalgrpc.EntityGetAllRequest, rc.token)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, f := range frames {
		p, _ := f["payload"].(map[string]any)
		meta, _ := p["meta"].(map[string]any)
		data, _ := p["data"].(map[string]any)
		id, _ := meta["id"].(string)
		s, _ := data["status"].(string)
		out[id] = s
	}
	return out, nil
}

// modelCountGRPC is EntityStatsGetRequest for the model under joinTok.
func (rc *reqCtx) modelCountGRPC(model string) (int, error) {
	frames, err := rc.h.searchCollectionGRPC(map[string]any{
		"id": "cbrc-stats", "model": map[string]any{"name": model, "version": 1},
	}, internalgrpc.EntityStatsGetRequest, rc.token)
	if err != nil {
		return 0, err
	}
	if len(frames) != 1 {
		return 0, fmt.Errorf("stats answered %d frames, want 1", len(frames))
	}
	n, _ := frames[0]["count"].(float64)
	return int(n), nil
}

// TestCallbackReadCurrent shows, on one stack, every callout kind reading its
// anchor under the token and finding it as the workflow has it.
func TestCallbackReadCurrent(t *testing.T) {
	h := newCallbackHarness(t)
	anyNode := map[string]any{"attachEntity": true, "calculationNodesTags": ""}

	// An update names the manual transition "go" from READY. The anchor is
	// stored with status "old"; the request sends "new". While "go"'s
	// processors run the anchor is still in READY.
	readyGo := func(goTransition map[string]any, extra map[string]any) map[string]any {
		states := map[string]any{
			"NONE":  map[string]any{"transitions": []any{map[string]any{"name": "init", "next": "READY", "manual": false}}},
			"READY": map[string]any{"transitions": []any{goTransition}},
			"DONE":  map[string]any{},
		}
		for k, v := range extra {
			states[k] = v
		}
		return states
	}
	const oldData = `{"name":"anchor","amount":1,"status":"old"}`
	const newData = `{"name":"anchor","amount":2,"status":"new"}`
	seen := newSeenLog()

	update := func(t *testing.T, model string) string {
		t.Helper()
		id, status, body := h.CreateEntity(t, model, 1, oldData)
		if status != http.StatusOK {
			t.Fatalf("create %s: %d %s", model, status, body)
		}
		res, err := h.updateTransitionHTTP(id, "go", newData)
		if err != nil {
			t.Fatalf("update %s: %v", model, err)
		}
		if res.StatusCode != http.StatusOK {
			t.Fatalf("update %s: %d %s", model, res.StatusCode, res.Body)
		}
		return id
	}

	t.Run("UpdateSyncProcessor_EveryReadDoor", func(t *testing.T) {
		const model = "cbrc-update-sync"
		h.RegisterProc("cbrc-upd-sync", func(rc *reqCtx) (map[string]any, error) {
			seen.put("upd-sync/get", readAnchorJoined(rc))
			found, marker, err := h.getEntityGRPC(rc.entityID, rc.token)
			seen.put("upd-sync/grpc-get", fmt.Sprintf("found=%v status=%s err=%v", found, marker, err))

			httpSearch, err := rc.searchHTTP(model, 1, simpleStatusCond("new"), false, true)
			if err != nil {
				return nil, err
			}
			seen.put("upd-sync/http-search", ndjsonStatusMatches(httpSearch.Body, "new"))
			grpcSearch, err := h.searchGRPC(model, 1, "new", rc.token)
			seen.put("upd-sync/grpc-search", fmt.Sprintf("%d err=%v", grpcSearch, err))

			page, _, err := rc.pageStatuses(model)
			seen.put("upd-sync/http-page", fmt.Sprintf("%s err=%v", page[rc.entityID], err))
			gpage, err := rc.pageStatusesGRPC(model)
			seen.put("upd-sync/grpc-page", fmt.Sprintf("%s err=%v", gpage[rc.entityID], err))

			n, err := rc.statusCountHTTP(model, "new")
			seen.put("upd-sync/http-count", fmt.Sprintf("%d err=%v", n, err))
			return nil, nil
		})
		h.SetupModelWithWorkflow(t, model, workflowDocJSON(model+"-wf", readyGo(map[string]any{
			"name": "go", "next": "DONE", "manual": true,
			"processors": []any{map[string]any{"type": "externalized", "name": "cbrc-upd-sync", "executionMode": "SYNC", "config": anyNode}},
		}, nil)))
		update(t, model)

		got := seen.anchor(t, "upd-sync/get")
		requireCurrentAnchor(t, "SYNC processor, HTTP GET", got, "READY", "new")
		if got.TxID == "" || got.TxID != got.PayloadTxID {
			t.Errorf("payload meta.transactionId = %q, joined read's = %q; want them equal", got.PayloadTxID, got.TxID)
		}
		for key, want := range map[string]string{
			"upd-sync/grpc-get":    "found=true status=new err=<nil>",
			"upd-sync/grpc-search": "1 err=<nil>",
			"upd-sync/http-page":   "new err=<nil>",
			"upd-sync/grpc-page":   "new err=<nil>",
			"upd-sync/http-count":  "1 err=<nil>",
		} {
			if got := seen.get(t, key); got != want {
				t.Errorf("%s = %v; want %q", key, got, want)
			}
		}
		if n := seen.get(t, "upd-sync/http-search"); n != 1 {
			t.Errorf("joined HTTP search by the request's data matched %v; want 1 (the anchor)", n)
		}
	})

	t.Run("CreateSyncProcessor_FoundAndCounted", func(t *testing.T) {
		const model = "cbrc-create-sync"
		h.RegisterProc("cbrc-create-sync", func(rc *reqCtx) (map[string]any, error) {
			seen.put("create/get", readAnchorJoined(rc))
			n, err := rc.modelCountHTTP(model)
			seen.put("create/http-count", fmt.Sprintf("%d err=%v", n, err))
			g, err := rc.modelCountGRPC(model)
			seen.put("create/grpc-count", fmt.Sprintf("%d err=%v", g, err))
			page, _, err := rc.pageStatuses(model)
			seen.put("create/http-page", fmt.Sprintf("%s err=%v", page[rc.entityID], err))
			return nil, nil
		})
		h.SetupModelWithWorkflow(t, model, workflowDocJSON(model+"-wf", map[string]any{
			"NONE": map[string]any{"transitions": []any{map[string]any{
				"name": "init", "next": "DONE", "manual": false,
				"processors": []any{map[string]any{"type": "externalized", "name": "cbrc-create-sync", "executionMode": "SYNC", "config": anyNode}},
			}}},
			"DONE": map[string]any{},
		}))
		if _, status, body := h.CreateEntity(t, model, 1, newData); status != http.StatusOK {
			t.Fatalf("create: %d %s", status, body)
		}
		got := seen.anchor(t, "create/get")
		requireCurrentAnchor(t, "create, SYNC processor", got, "NONE", "new")
		for key, want := range map[string]string{
			"create/http-count": "1 err=<nil>",
			"create/grpc-count": "1 err=<nil>",
			"create/http-page":  "new err=<nil>",
		} {
			if got := seen.get(t, key); got != want {
				t.Errorf("%s = %v; want %q", key, got, want)
			}
		}
	})

	t.Run("CascadeFunctionCriterion", func(t *testing.T) {
		const model = "cbrc-cascade-crit"
		h.RegisterCriteria("cbrc-crit", func(rc *reqCtx) (bool, error) {
			seen.put("crit/get", readAnchorJoined(rc))
			return true, nil
		})
		h.SetupModelWithWorkflow(t, model, workflowDocJSON(model+"-wf", readyGo(
			map[string]any{"name": "go", "next": "MID", "manual": true},
			map[string]any{"MID": map[string]any{"transitions": []any{map[string]any{
				"name": "check", "next": "DONE", "manual": false,
				"criterion": map[string]any{"type": "function", "function": map[string]any{"name": "cbrc-crit", "config": anyNode}},
			}}}})))
		id := update(t, model)
		requireCurrentAnchor(t, "cascade function criterion", seen.anchor(t, "crit/get"), "MID", "new")
		if st, _ := h.GetEntityState(t, id); st != "DONE" {
			t.Errorf("state = %q; want DONE", st)
		}
	})

	t.Run("WorkflowSelectionCriterion_OnCreate", func(t *testing.T) {
		const model = "cbrc-select-crit"
		h.RegisterCriteria("cbrc-select", func(rc *reqCtx) (bool, error) {
			seen.put("select/get", readAnchorJoined(rc))
			return true, nil
		})
		h.SetupModelWithWorkflow(t, model, workflowDocWithCriterionJSON(model+"-wf",
			map[string]any{"type": "function", "function": map[string]any{"name": "cbrc-select", "config": anyNode}},
			map[string]any{
				"NONE": map[string]any{"transitions": []any{map[string]any{"name": "init", "next": "DONE", "manual": false}}},
				"DONE": map[string]any{},
			}))
		if _, status, body := h.CreateEntity(t, model, 1, newData); status != http.StatusOK {
			t.Fatalf("create: %d %s", status, body)
		}
		// Workflow selection runs before the anchor has a state.
		requireCurrentAnchor(t, "workflow selection criterion", seen.anchor(t, "select/get"), "", "new")
	})

	t.Run("AsyncNewTxProcessor", func(t *testing.T) {
		const model = "cbrc-async-new-tx"
		h.RegisterProc("cbrc-anx", func(rc *reqCtx) (map[string]any, error) {
			seen.put("anx/get", readAnchorJoined(rc))
			return nil, nil
		})
		h.SetupModelWithWorkflow(t, model, workflowDocJSON(model+"-wf", readyGo(map[string]any{
			"name": "go", "next": "DONE", "manual": true,
			"processors": []any{map[string]any{"type": "externalized", "name": "cbrc-anx", "executionMode": "ASYNC_NEW_TX", "config": anyNode}},
		}, nil)))
		update(t, model)
		requireCurrentAnchor(t, "ASYNC_NEW_TX processor", seen.anchor(t, "anx/get"), "READY", "new")
	})
}

// TestCallbackReadCurrent_ScheduleFunction: a schedule function reads its
// anchor under the token on a client door (a create entering a state whose
// transition is scheduled by a function) and in a scheduled fire (the fire
// enters a second such state). Both find the anchor as it is: on the create it
// is not committed yet, and in the fire it is in the state the fire reached.
func TestCallbackReadCurrent_ScheduleFunction(t *testing.T) {
	h, _ := newSchedulerCallbackHarness(t, nil)
	const model = "cbrc-sched-fn"

	seen := newSeenLog()
	var calls atomic.Int32
	h.RegisterFunction("cbrc-sched", func(rc *reqCtx) (string, map[string]any, error) {
		got := readAnchorJoined(rc)
		key := "fn/" + got.State
		if got.Status != http.StatusOK {
			key = fmt.Sprintf("fn/refused-%d", got.Status)
		}
		seen.put(key, got)
		if calls.Add(1) == 1 {
			// The create's: fire soon, so the fire's own schedule function
			// runs.
			return "Schedule", map[string]any{"fireAfterMs": 300}, nil
		}
		return "Schedule", map[string]any{"fireAfterMs": 3_600_000}, nil
	})
	fn := map[string]any{"name": "cbrc-sched", "resultKind": "Schedule", "calculationNodesTags": scheduledFnTag}
	wf := workflowDocJSON(model+"-wf", map[string]any{
		"NONE": map[string]any{"transitions": []any{map[string]any{"name": "init", "next": "Open", "manual": false}}},
		"Open": map[string]any{"transitions": []any{map[string]any{"name": "toMid", "next": "Mid", "manual": false,
			"schedule": map[string]any{"function": fn}}}},
		"Mid": map[string]any{"transitions": []any{map[string]any{"name": "toEnd", "next": "End", "manual": false,
			"schedule": map[string]any{"function": fn}}}},
		"End": map[string]any{},
	})
	h.SetupModelWithWorkflow(t, model, wf)

	id, status, body := h.CreateEntity(t, model, 1, `{"name":"anchor","amount":1,"status":"created"}`)
	if status != http.StatusOK {
		t.Fatalf("create: %d %s", status, body)
	}
	if got, ok := seen.lookup("fn/Open"); ok {
		requireCurrentAnchor(t, "schedule function on the create", got.(anchorSeen), "Open", "created")
	} else {
		t.Errorf("schedule function on the create: the joined read did not find the anchor in Open (recorded %v)", seen.keys())
	}

	if st := awaitCallbackEntityState(t, h, id, "Mid", scheduledFireTimeout); st != "Mid" {
		t.Fatalf("state = %q; want the fire to reach Mid", st)
	}
	// The fire's schedule function runs before the fire commits, so it has
	// recorded by the time the entity reads Mid.
	if calls.Load() < 2 {
		t.Fatalf("the fire's schedule function ran %d time(s) in all; want the create's and the fire's", calls.Load())
	}
	if _, ok := seen.lookup("fn/Mid"); !ok {
		t.Fatalf("schedule function in the scheduled fire: the joined read did not find the anchor in Mid (recorded %v)", seen.keys())
	}
	requireCurrentAnchor(t, "schedule function in the scheduled fire", seen.anchor(t, "fn/Mid"), "Mid", "created")
}
