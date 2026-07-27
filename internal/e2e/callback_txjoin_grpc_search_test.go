package e2e_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"

	"google.golang.org/grpc/metadata"

	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
	cyodapb "github.com/cyoda-platform/cyoda-go/api/grpc/cyoda"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
)

// callback_txjoin_grpc_search_test.go proves that a compute-node callback which
// SEARCHES over the gRPC entity API joins the originating transaction T, just as
// the write RPCs do. The joining txRouteInterceptor originally covered only the
// write RPCs (EntityManage / EntityManageCollection); the read RPCs
// (EntitySearch / EntitySearchCollection) were not intercepted, so a callback's
// tx-token was silently ignored on searches and they ran unjoined against
// last-committed state.
//
// Unlike callback_txjoin_test.go (which exercises the HTTP callback path, whose
// route-agnostic TxJoin middleware always joined), these callbacks go back over
// the REAL gRPC entity API carrying the tx-token as gRPC metadata — the exact
// path the interceptor gates. Each search is issued twice: once WITH the token
// (must observe the uncommitted same-transaction write) and once WITHOUT (the
// control: must NOT observe it), so the assertion isolates the join as the cause.

// grpcSearchResult is the outcome of a gRPC search callback, recorded into the
// primary entity's data so it can be asserted after commit.
type grpcSearchResult struct {
	// GetFoundJoined is true when the unary EntitySearch (EntityGetRequest) issued
	// WITH the tx-token observed the uncommitted secondary.
	GetFoundJoined bool
	// GetFoundStandalone is true when the same unary get WITHOUT the token
	// observed it — must be false (the row is uncommitted).
	GetFoundStandalone bool
	// SearchCountJoined is the number of EntitySearchCollection (EntitySearchRequest)
	// matches WITH the token — must be >= 1.
	SearchCountJoined int
	// SearchCountStandalone is the match count WITHOUT the token — must be 0.
	SearchCountStandalone int
	// JoinedMarker is the secondary's data.status observed by the joined get.
	JoinedMarker string
}

// grpcCtx builds an outgoing gRPC context carrying the member's bearer and,
// when joinTok is non-empty, the tx-token metadata that joins T.
func (h *callbackHarness) grpcCtx(joinTok string) context.Context {
	ctx := context.Background()
	tok, _ := h.bearerVal.Load().(string)
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+tok)
	if joinTok != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, internalgrpcTxTokenKey, joinTok)
	}
	return ctx
}

// internalgrpcTxTokenKey mirrors proxy.GRPCTxTokenKey ("tx-token"); duplicated
// here to avoid importing the internal proxy package into the e2e test.
const internalgrpcTxTokenKey = "tx-token"

// getEntityGRPC issues a unary EntitySearch (EntityGetRequest) for entityID.
// When joinTok is set the read joins T. Returns whether the entity was observed
// and its data.status marker.
func (h *callbackHarness) getEntityGRPC(entityID, joinTok string) (found bool, marker string, err error) {
	client := cyodapb.NewCloudEventsServiceClient(h.member.conn)
	reqCE, err := internalgrpc.NewCloudEvent(internalgrpc.EntityGetRequest, map[string]any{
		"id":       "cb-grpc-get",
		"entityId": entityID,
	})
	if err != nil {
		return false, "", fmt.Errorf("build get request: %w", err)
	}
	respCE, err := client.EntitySearch(h.grpcCtx(joinTok), reqCE)
	if err != nil {
		return false, "", fmt.Errorf("EntitySearch: %w", err)
	}
	ok, data, perr := parseEntityResponse(respCE)
	if perr != nil {
		return false, "", perr
	}
	if !ok {
		return false, "", nil
	}
	m, _ := data["status"].(string)
	return true, m, nil
}

// searchGRPC issues a streaming EntitySearchCollection (EntitySearchRequest)
// matching data.status == statusEq over (entityName, version). When joinTok is
// set the search joins T. Returns the number of matched entities.
func (h *callbackHarness) searchGRPC(entityName string, version int, statusEq, joinTok string) (count int, err error) {
	client := cyodapb.NewCloudEventsServiceClient(h.member.conn)
	reqCE, err := internalgrpc.NewCloudEvent(internalgrpc.EntitySearchRequest, map[string]any{
		"id":    "cb-grpc-search",
		"model": map[string]any{"name": entityName, "version": version},
		"condition": map[string]any{
			"type":         "simple",
			"jsonPath":     "$.status",
			"operatorType": "EQUALS",
			"value":        statusEq,
		},
	})
	if err != nil {
		return 0, fmt.Errorf("build search request: %w", err)
	}
	stream, err := client.EntitySearchCollection(h.grpcCtx(joinTok), reqCE)
	if err != nil {
		return 0, fmt.Errorf("EntitySearchCollection: %w", err)
	}
	for {
		frame, rerr := stream.Recv()
		if errors.Is(rerr, io.EOF) {
			break // stream completed normally
		}
		if rerr != nil {
			return count, fmt.Errorf("search stream recv: %w", rerr)
		}
		ok, _, perr := parseEntityResponse(frame)
		if perr != nil {
			return count, perr
		}
		if !ok {
			// A Success=false frame is an error envelope, not a match.
			return count, fmt.Errorf("search returned error frame")
		}
		count++
	}
	return count, nil
}

// parseEntityResponse decodes an EntityResponse CloudEvent, returning whether it
// is a successful result carrying entity data (ok=false for a 404/error envelope).
func parseEntityResponse(ce *cepb.CloudEvent) (ok bool, data map[string]any, err error) {
	_, payload, perr := internalgrpc.ParseCloudEvent(ce)
	if perr != nil {
		return false, nil, fmt.Errorf("parse response: %w", perr)
	}
	var resp struct {
		Success bool `json:"success"`
		Payload *struct {
			Data map[string]any `json:"data"`
		} `json:"payload"`
	}
	if uerr := json.Unmarshal(payload, &resp); uerr != nil {
		return false, nil, fmt.Errorf("unmarshal response: %w", uerr)
	}
	if !resp.Success {
		return false, nil, nil
	}
	if resp.Payload == nil {
		return true, map[string]any{}, nil
	}
	return true, resp.Payload.Data, nil
}

// TestCallback_GRPCSearch_SeesUncommittedWrite proves read-your-own-writes for
// gRPC SEARCH callbacks: a SYNC processor creates a secondary entity inside T
// (uncommitted), then reads it back via the gRPC EntitySearch (unary) and
// EntitySearchCollection (streaming) RPCs. WITH the tx-token both observe the
// uncommitted row; WITHOUT it neither does. Before the interceptor was wired for
// the search RPCs the joined variants behaved like the standalone ones (the
// token was ignored), so the joined-vs-standalone assertions would collapse.
func TestCallback_GRPCSearch_SeesUncommittedWrite(t *testing.T) {
	h := newCallbackHarness(t)

	const primary = "cb-grpc-search-primary"
	const secondary = "cb-grpc-search-secondary"
	h.SetupModelWithWorkflow(t, secondary, secondaryWorkflow)

	const marker = "grpc-search-marker-77"

	h.RegisterProc("cb-grpc-search", func(rc *reqCtx) (map[string]any, error) {
		// 1. Create a secondary entity inside T (uncommitted) via the proven HTTP
		//    join path.
		created, err := rc.CreateEntity(secondary, 1, fmt.Sprintf(`{"name":"child","amount":7,"status":%q}`, marker))
		if err != nil {
			return nil, fmt.Errorf("callback create failed: %w", err)
		}
		if created.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("callback create status=%d body=%s", created.StatusCode, created.Body)
		}

		var res grpcSearchResult

		// 2. Unary gRPC get WITH the tx-token — must observe the uncommitted row.
		foundJoined, joinedMarker, err := rc.h.getEntityGRPC(created.EntityID, rc.token)
		if err != nil {
			return nil, fmt.Errorf("joined gRPC get: %w", err)
		}
		res.GetFoundJoined = foundJoined
		res.JoinedMarker = joinedMarker

		// 3. Unary gRPC get WITHOUT the token (control) — must NOT observe it.
		foundStandalone, _, err := rc.h.getEntityGRPC(created.EntityID, "")
		if err != nil {
			return nil, fmt.Errorf("standalone gRPC get: %w", err)
		}
		res.GetFoundStandalone = foundStandalone

		// 4. Streaming gRPC search WITH the token — must match the uncommitted row.
		cJoined, err := rc.h.searchGRPC(secondary, 1, marker, rc.token)
		if err != nil {
			return nil, fmt.Errorf("joined gRPC search: %w", err)
		}
		res.SearchCountJoined = cJoined

		// 5. Streaming gRPC search WITHOUT the token (control) — must match nothing.
		cStandalone, err := rc.h.searchGRPC(secondary, 1, marker, "")
		if err != nil {
			return nil, fmt.Errorf("standalone gRPC search: %w", err)
		}
		res.SearchCountStandalone = cStandalone

		out := cloneData(rc.entityData)
		out["getFoundJoined"] = res.GetFoundJoined
		out["getFoundStandalone"] = res.GetFoundStandalone
		out["searchCountJoined"] = float64(res.SearchCountJoined)
		out["searchCountStandalone"] = float64(res.SearchCountStandalone)
		out["joinedMarker"] = res.JoinedMarker
		out["secondaryId"] = created.EntityID
		return out, nil
	})

	primaryWF := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "primary-grpc-search-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE":   {"transitions": [{"name": "init", "next": "ACTIVE", "manual": false,
					"processors": [{"type": "calculator", "name": "cb-grpc-search", "executionMode": "SYNC",
						"config": {"attachEntity": true, "calculationNodesTags": ""}}]
				}]},
				"ACTIVE": {}
			}
		}]
	}`
	h.SetupModelWithWorkflow(t, primary, primaryWF)

	primaryID, status, body := h.CreateEntity(t, primary, 1, `{"name":"parent","amount":100,"status":"new"}`)
	if status != http.StatusOK {
		t.Fatalf("primary create: status=%d body=%s", status, body)
	}

	data := h.GetEntityData(t, primaryID)

	// The joined unary get saw the uncommitted secondary; the standalone one did not.
	if found, _ := data["getFoundJoined"].(bool); !found {
		t.Errorf("joined gRPC EntitySearch did NOT observe the uncommitted secondary — the tx-token was ignored on the read RPC (read-your-own-writes broken for gRPC search)")
	}
	if found, _ := data["getFoundStandalone"].(bool); found {
		t.Errorf("standalone gRPC EntitySearch (no token) observed the uncommitted secondary — the control must not see it; the test would not isolate the join")
	}
	if got := data["joinedMarker"]; got != marker {
		t.Errorf("joined gRPC get observed status=%v; want the uncommitted marker %q", got, marker)
	}

	// The joined streaming search matched the uncommitted secondary; standalone matched nothing.
	if c, _ := data["searchCountJoined"].(float64); c < 1 {
		t.Errorf("joined gRPC EntitySearchCollection matched %v entities; want >= 1 (uncommitted secondary must be visible within T)", data["searchCountJoined"])
	}
	if c, _ := data["searchCountStandalone"].(float64); c != 0 {
		t.Errorf("standalone gRPC EntitySearchCollection matched %v entities; want 0 (uncommitted secondary must be invisible outside T)", data["searchCountStandalone"])
	}

	// After commit the secondary is durable (belt-and-braces).
	secondaryID, _ := data["secondaryId"].(string)
	if secondaryID == "" {
		t.Fatal("primary data missing secondaryId")
	}
	if st, code := h.GetEntityState(t, secondaryID); code != http.StatusOK {
		t.Fatalf("secondary GET after commit: http %d; want 200", code)
	} else if st != "STORED" {
		t.Errorf("secondary state = %q; want STORED", st)
	}
}
