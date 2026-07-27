package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
	cyodapb "github.com/cyoda-platform/cyoda-go/api/grpc/cyoda"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
)

// search_intx_test.go — in-transaction SEARCH over the FULL HTTP and gRPC stack
// via the compute-node tx-token callback harness (callback_harness_test.go).
//
// In-tx search over the wire is reachable ONLY through a joined compute-node
// callback: HTTP carries the X-Tx-Token header (route-agnostic TxJoin
// middleware joins T) and gRPC carries tx-token metadata (txRouteInterceptor
// joins T on the search RPCs). There is no client-facing "begin tx" API, so a
// processor callback is the only entry point that can search inside a live
// transaction.
//
// These tests fill the gaps left by the existing suite:
//   - callback_txjoin_grpc_search_test.go proves gRPC in-tx search RYW WITHOUT
//     the trackingRead field. Here we prove the trackingRead field is accepted
//     and threaded end-to-end over BOTH transports (HTTP query param + gRPC
//     payload field), returning the same read-your-own-writes results.
//   - callback_txjoin_test.go proves the HTTP callback path with a joined GET,
//     not a joined SEARCH. Here we prove a joined HTTP SEARCH sees T's
//     uncommitted writes (and an unjoined one does not).
//   - the documented status/error codes on the in-tx search path: 200 (results),
//     400 INVALID_FIELD_PATH (unknown condition path), 404 MODEL_NOT_FOUND
//     (unregistered model) — asserted on the JOINED path over HTTP and gRPC.
//
// The FCW conflict outcome of a tracked read is covered at the engine level by
// search_intx_tracking_test.go; it is intentionally not re-proven here.

// searchHTTP issues POST /api/search/direct/{model}/{version} from inside a
// processor callback. When trackingRead is true the trackingRead query param is
// set; when join is true the tx-token is echoed as X-Tx-Token (joining T). The
// condition JSON is the request body. Returns the raw HTTP result (ndjson body
// on success, RFC 9457 problem body on error).
func (rc *reqCtx) searchHTTP(model string, version int, condition string, trackingRead, join bool) (callbackResult, error) {
	path := fmt.Sprintf("/api/search/direct/%s/%d", model, version)
	if trackingRead {
		path += "?trackingRead=true"
	}
	tok := ""
	if join {
		tok = rc.token
	}
	return rc.h.callback(http.MethodPost, path, condition, tok)
}

// ndjsonStatusMatches counts the lines of an application/x-ndjson search body
// whose data.status field equals marker. Robust against blank trailing lines.
func ndjsonStatusMatches(body, marker string) int {
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var env struct {
			Data map[string]any `json:"data"`
		}
		if json.Unmarshal([]byte(line), &env) != nil {
			continue
		}
		if s, _ := env.Data["status"].(string); s == marker {
			n++
		}
	}
	return n
}

// simpleStatusCond builds a "$.status EQUALS value" search condition body.
func simpleStatusCond(value string) string {
	return fmt.Sprintf(`{"type":"simple","jsonPath":"$.status","operatorType":"EQUALS","value":%q}`, value)
}

// searchGRPCCond issues an EntitySearchCollection (EntitySearchRequest) over the
// real gRPC entity API. When joinTok is set the search joins T; trackingRead
// sets the payload field the plugin Searcher honours to record returned rows
// into T's read-set. The condition is supplied as a decoded map. Returns the
// number of successful result frames and, if the stream instead returned an
// error envelope, that frame's code and message (code is the gRPC envelope
// class "CLIENT_ERROR"; the DOMAIN code, e.g. MODEL_NOT_FOUND, is carried in
// the message per buildErrorFields).
func (h *callbackHarness) searchGRPCCond(model string, version int, cond map[string]any, joinTok string, trackingRead bool) (count int, errCode, errMsg string, err error) {
	client := cyodapb.NewCloudEventsServiceClient(h.member.conn)
	reqCE, err := internalgrpc.NewCloudEvent(internalgrpc.EntitySearchRequest, map[string]any{
		"id":           "cb-intx-grpc-search",
		"model":        map[string]any{"name": model, "version": version},
		"condition":    cond,
		"trackingRead": trackingRead,
	})
	if err != nil {
		return 0, "", "", fmt.Errorf("build search request: %w", err)
	}
	stream, err := client.EntitySearchCollection(h.grpcCtx(joinTok), reqCE)
	if err != nil {
		return 0, "", "", fmt.Errorf("EntitySearchCollection: %w", err)
	}
	for {
		frame, rerr := stream.Recv()
		if rerr != nil {
			if strings.Contains(rerr.Error(), "EOF") {
				break // stream completed normally
			}
			return count, errCode, errMsg, fmt.Errorf("search stream recv: %w", rerr)
		}
		ok, code, msg, perr := parseSearchFrame(frame)
		if perr != nil {
			return count, errCode, errMsg, perr
		}
		if !ok {
			// An error envelope frame — capture its code/message and stop.
			errCode, errMsg = code, msg
			continue
		}
		count++
	}
	return count, errCode, errMsg, nil
}

// parseSearchFrame decodes an EntityResponse search frame, returning success,
// and (on an error frame) the envelope error code and message. Unlike
// parseEntityResponse (callback_txjoin_grpc_search_test.go) it surfaces the
// error fields so the in-tx error-code assertions can inspect them.
func parseSearchFrame(ce *cepb.CloudEvent) (ok bool, errCode, errMsg string, err error) {
	_, payload, perr := internalgrpc.ParseCloudEvent(ce)
	if perr != nil {
		return false, "", "", fmt.Errorf("parse response: %w", perr)
	}
	var resp struct {
		Success bool `json:"success"`
		Error   *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if uerr := json.Unmarshal(payload, &resp); uerr != nil {
		return false, "", "", fmt.Errorf("unmarshal response: %w", uerr)
	}
	if resp.Success {
		return true, "", "", nil
	}
	if resp.Error != nil {
		return false, resp.Error.Code, resp.Error.Message, nil
	}
	return false, "", "", nil
}

// intxSearchPrimaryWF builds a REPLACE workflow whose NONE->ACTIVE transition
// fires a single SYNC calculator processor by name.
func intxSearchPrimaryWF(wfName, procName string) string {
	return fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": %q, "initialState": "NONE", "active": true,
			"states": {
				"NONE":   {"transitions": [{"name": "init", "next": "ACTIVE", "manual": false,
					"processors": [{"type": "calculator", "name": %q, "executionMode": "SYNC",
						"config": {"attachEntity": true, "calculationNodesTags": ""}}]
				}]},
				"ACTIVE": {}
			}
		}]
	}`, wfName, procName)
}

// TestIntxSearch_HTTP_TrackingRead_SeesUncommittedWrite proves in-tx SEARCH over
// the HTTP entity API end-to-end: a SYNC processor creates a secondary entity
// inside T (uncommitted), then searches for it over POST /search/direct with
// trackingRead=true. WITH the tx-token the joined search observes the
// uncommitted row (read-your-own-writes) and returns HTTP 200 — proving the
// trackingRead field is accepted and threaded full-stack. WITHOUT the token the
// control search observes nothing (the row is uncommitted outside T).
func TestIntxSearch_HTTP_TrackingRead_SeesUncommittedWrite(t *testing.T) {
	h := newCallbackHarness(t)

	const primary = "intx-http-search-primary"
	const secondary = "intx-http-search-secondary"
	h.SetupModelWithWorkflow(t, secondary, secondaryWorkflow)

	const marker = "intx-http-marker-51"

	h.RegisterProc("cb-intx-http-search", func(rc *reqCtx) (map[string]any, error) {
		created, err := rc.CreateEntity(secondary, 1, fmt.Sprintf(`{"name":"child","amount":7,"status":%q}`, marker))
		if err != nil {
			return nil, fmt.Errorf("callback create failed: %w", err)
		}
		if created.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("callback create status=%d body=%s", created.StatusCode, created.Body)
		}

		cond := simpleStatusCond(marker)

		// Joined tracking search WITH the tx-token — must observe the uncommitted row.
		joined, err := rc.searchHTTP(secondary, 1, cond, true /*trackingRead*/, true /*join*/)
		if err != nil {
			return nil, fmt.Errorf("joined HTTP search: %w", err)
		}
		// Standalone control WITHOUT the token — must observe nothing.
		standalone, err := rc.searchHTTP(secondary, 1, cond, false, false)
		if err != nil {
			return nil, fmt.Errorf("standalone HTTP search: %w", err)
		}

		out := cloneData(rc.entityData)
		out["joinedStatus"] = float64(joined.StatusCode)
		out["joinedCount"] = float64(ndjsonStatusMatches(joined.Body, marker))
		out["standaloneStatus"] = float64(standalone.StatusCode)
		out["standaloneCount"] = float64(ndjsonStatusMatches(standalone.Body, marker))
		out["secondaryId"] = created.EntityID
		return out, nil
	})

	h.SetupModelWithWorkflow(t, primary, intxSearchPrimaryWF("primary-intx-http-wf", "cb-intx-http-search"))

	primaryID, status, body := h.CreateEntity(t, primary, 1, `{"name":"parent","amount":100,"status":"new"}`)
	if status != http.StatusOK {
		t.Fatalf("primary create: status=%d body=%s", status, body)
	}

	data := h.GetEntityData(t, primaryID)

	// trackingRead accepted full-stack: the joined search returned HTTP 200.
	if got, _ := data["joinedStatus"].(float64); int(got) != http.StatusOK {
		t.Errorf("joined HTTP search with trackingRead=true returned status=%v; want 200 (field must be accepted and threaded, not rejected)", data["joinedStatus"])
	}
	// RYW: the joined search observed the uncommitted secondary.
	if c, _ := data["joinedCount"].(float64); c < 1 {
		t.Errorf("joined HTTP search matched %v rows; want >= 1 (uncommitted secondary must be visible within T)", data["joinedCount"])
	}
	// Control: the standalone search (no token) is a plain 200 that sees nothing.
	if got, _ := data["standaloneStatus"].(float64); int(got) != http.StatusOK {
		t.Errorf("standalone HTTP search returned status=%v; want 200", data["standaloneStatus"])
	}
	if c, _ := data["standaloneCount"].(float64); c != 0 {
		t.Errorf("standalone HTTP search matched %v rows; want 0 (uncommitted secondary must be invisible outside T)", data["standaloneCount"])
	}

	// The tracked-read transaction committed cleanly (no concurrent writer) and
	// the secondary is durable.
	if st, code := h.GetEntityState(t, primaryID); code != http.StatusOK || st != "ACTIVE" {
		t.Fatalf("primary state=%q http=%d; want ACTIVE/200 (tracked-read tx must commit)", st, code)
	}
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

// TestIntxSearch_GRPC_TrackingRead_SeesUncommittedWrite proves the trackingRead
// field is accepted and threaded over the gRPC search payload (the
// callback_txjoin_grpc_search_test.go RYW proof deliberately omits it): a SYNC
// processor creates a secondary inside T, then searches over the gRPC
// EntitySearchCollection RPC with trackingRead=true. WITH the tx-token metadata
// the joined search returns a success envelope carrying the uncommitted row;
// WITHOUT it the control returns nothing.
func TestIntxSearch_GRPC_TrackingRead_SeesUncommittedWrite(t *testing.T) {
	h := newCallbackHarness(t)

	const primary = "intx-grpc-search-primary"
	const secondary = "intx-grpc-search-secondary"
	h.SetupModelWithWorkflow(t, secondary, secondaryWorkflow)

	const marker = "intx-grpc-marker-63"

	h.RegisterProc("cb-intx-grpc-search", func(rc *reqCtx) (map[string]any, error) {
		created, err := rc.CreateEntity(secondary, 1, fmt.Sprintf(`{"name":"child","amount":7,"status":%q}`, marker))
		if err != nil {
			return nil, fmt.Errorf("callback create failed: %w", err)
		}
		if created.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("callback create status=%d body=%s", created.StatusCode, created.Body)
		}

		cond := map[string]any{
			"type":         "simple",
			"jsonPath":     "$.status",
			"operatorType": "EQUALS",
			"value":        marker,
		}

		// Joined tracking search WITH the tx-token metadata.
		cJoined, ec, em, err := rc.h.searchGRPCCond(secondary, 1, cond, rc.token, true)
		if err != nil {
			return nil, fmt.Errorf("joined gRPC search: %w", err)
		}
		if ec != "" || em != "" {
			return nil, fmt.Errorf("joined gRPC search returned an error envelope: code=%q msg=%q", ec, em)
		}
		// Standalone control WITHOUT the token.
		cStandalone, _, _, err := rc.h.searchGRPCCond(secondary, 1, cond, "", false)
		if err != nil {
			return nil, fmt.Errorf("standalone gRPC search: %w", err)
		}

		out := cloneData(rc.entityData)
		out["searchCountJoined"] = float64(cJoined)
		out["searchCountStandalone"] = float64(cStandalone)
		out["secondaryId"] = created.EntityID
		return out, nil
	})

	h.SetupModelWithWorkflow(t, primary, intxSearchPrimaryWF("primary-intx-grpc-wf", "cb-intx-grpc-search"))

	primaryID, status, body := h.CreateEntity(t, primary, 1, `{"name":"parent","amount":100,"status":"new"}`)
	if status != http.StatusOK {
		t.Fatalf("primary create: status=%d body=%s", status, body)
	}

	data := h.GetEntityData(t, primaryID)

	// trackingRead accepted over gRPC + RYW: the joined search matched the uncommitted row.
	if c, _ := data["searchCountJoined"].(float64); c < 1 {
		t.Errorf("joined gRPC search (trackingRead=true) matched %v rows; want >= 1 (field must be accepted and threaded; uncommitted row visible within T)", data["searchCountJoined"])
	}
	if c, _ := data["searchCountStandalone"].(float64); c != 0 {
		t.Errorf("standalone gRPC search matched %v rows; want 0 (uncommitted row invisible outside T)", data["searchCountStandalone"])
	}

	// The tracked-read transaction committed cleanly; the secondary is durable.
	if st, code := h.GetEntityState(t, primaryID); code != http.StatusOK || st != "ACTIVE" {
		t.Fatalf("primary state=%q http=%d; want ACTIVE/200", st, code)
	}
	secondaryID, _ := data["secondaryId"].(string)
	if st, code := h.GetEntityState(t, secondaryID); code != http.StatusOK || st != "STORED" {
		t.Errorf("secondary state=%q http=%d; want STORED/200", st, code)
	}
}

// TestIntxSearch_InTx_ErrorCodes proves the documented status/error codes on the
// JOINED in-tx search path over BOTH transports. From inside a live transaction
// T a SYNC processor issues four searches carrying the tx-token:
//   - HTTP unregistered model      -> 404 MODEL_NOT_FOUND
//   - HTTP registered + bad path   -> 400 INVALID_FIELD_PATH
//   - gRPC unregistered model      -> error envelope, message names MODEL_NOT_FOUND
//   - gRPC registered + bad path   -> error envelope, message names INVALID_FIELD_PATH
//
// (Over gRPC the envelope's code field is the generic class CLIENT_ERROR; the
// domain code travels in the message per buildErrorFields, so we assert on the
// message.) The processor then returns success, so the primary must still commit
// to ACTIVE — additionally proving that an in-tx search that returns a 4xx does
// NOT poison the joined transaction.
func TestIntxSearch_InTx_ErrorCodes(t *testing.T) {
	h := newCallbackHarness(t)

	const primary = "intx-search-errcodes-primary"
	const secondary = "intx-search-errcodes-secondary"
	const unregistered = "intx-search-never-registered"
	h.SetupModelWithWorkflow(t, secondary, secondaryWorkflow)

	const badPathCond = `{"type":"simple","jsonPath":"$.unknownField","operatorType":"EQUALS","value":"whatever"}`
	badPathCondMap := map[string]any{
		"type":         "simple",
		"jsonPath":     "$.unknownField",
		"operatorType": "EQUALS",
		"value":        "whatever",
	}

	h.RegisterProc("cb-intx-search-errcodes", func(rc *reqCtx) (map[string]any, error) {
		out := cloneData(rc.entityData)

		// HTTP: unregistered model -> 404 MODEL_NOT_FOUND.
		httpNotFound, err := rc.searchHTTP(unregistered, 1, simpleStatusCond("x"), false, true)
		if err != nil {
			return nil, fmt.Errorf("http unregistered search: %w", err)
		}
		out["httpNotFoundStatus"] = float64(httpNotFound.StatusCode)
		out["httpNotFoundCode"] = problemErrorCode(httpNotFound.Body)

		// HTTP: registered model, unknown condition path -> 400 INVALID_FIELD_PATH.
		httpBadPath, err := rc.searchHTTP(secondary, 1, badPathCond, false, true)
		if err != nil {
			return nil, fmt.Errorf("http bad-path search: %w", err)
		}
		out["httpBadPathStatus"] = float64(httpBadPath.StatusCode)
		out["httpBadPathCode"] = problemErrorCode(httpBadPath.Body)

		// gRPC: unregistered model -> error envelope naming MODEL_NOT_FOUND.
		_, _, grpcNFMsg, err := rc.h.searchGRPCCond(unregistered, 1, badPathCondMap, rc.token, false)
		if err != nil {
			return nil, fmt.Errorf("grpc unregistered search: %w", err)
		}
		out["grpcNotFoundMsg"] = grpcNFMsg

		// gRPC: registered model, unknown condition path -> error envelope naming INVALID_FIELD_PATH.
		_, _, grpcBPMsg, err := rc.h.searchGRPCCond(secondary, 1, badPathCondMap, rc.token, false)
		if err != nil {
			return nil, fmt.Errorf("grpc bad-path search: %w", err)
		}
		out["grpcBadPathMsg"] = grpcBPMsg

		return out, nil
	})

	h.SetupModelWithWorkflow(t, primary, intxSearchPrimaryWF("primary-intx-errcodes-wf", "cb-intx-search-errcodes"))

	primaryID, status, body := h.CreateEntity(t, primary, 1, `{"name":"parent","amount":100,"status":"new"}`)
	if status != http.StatusOK {
		t.Fatalf("primary create: status=%d body=%s", status, body)
	}

	data := h.GetEntityData(t, primaryID)

	// HTTP 404 MODEL_NOT_FOUND.
	if got, _ := data["httpNotFoundStatus"].(float64); int(got) != http.StatusNotFound {
		t.Errorf("in-tx HTTP search on unregistered model: status=%v; want 404", data["httpNotFoundStatus"])
	}
	if got, _ := data["httpNotFoundCode"].(string); got != "MODEL_NOT_FOUND" {
		t.Errorf("in-tx HTTP search on unregistered model: errorCode=%q; want MODEL_NOT_FOUND", got)
	}

	// HTTP 400 INVALID_FIELD_PATH.
	if got, _ := data["httpBadPathStatus"].(float64); int(got) != http.StatusBadRequest {
		t.Errorf("in-tx HTTP search with unknown path: status=%v; want 400", data["httpBadPathStatus"])
	}
	if got, _ := data["httpBadPathCode"].(string); got != "INVALID_FIELD_PATH" {
		t.Errorf("in-tx HTTP search with unknown path: errorCode=%q; want INVALID_FIELD_PATH", got)
	}

	// gRPC error envelopes carry the domain code in the message.
	if got, _ := data["grpcNotFoundMsg"].(string); !strings.Contains(got, "MODEL_NOT_FOUND") {
		t.Errorf("in-tx gRPC search on unregistered model: error message=%q; want it to name MODEL_NOT_FOUND", got)
	}
	if got, _ := data["grpcBadPathMsg"].(string); !strings.Contains(got, "INVALID_FIELD_PATH") {
		t.Errorf("in-tx gRPC search with unknown path: error message=%q; want it to name INVALID_FIELD_PATH", got)
	}

	// The processor succeeded, so T must commit despite the 4xx search responses:
	// an in-tx search returning a client error must not poison the transaction.
	if st, code := h.GetEntityState(t, primaryID); code != http.StatusOK || st != "ACTIVE" {
		t.Fatalf("primary state=%q http=%d; want ACTIVE/200 (4xx in-tx search must not poison T)", st, code)
	}
}
