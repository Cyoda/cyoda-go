package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
)

// callback.go — feature #287 callback-capable processors/criteria for the
// compute-test-client. These read the signed cyodatxtoken the engine attaches
// to a calc request and echo it as the X-Tx-Token HTTP header on a callback into
// cyoda-go, exercising the transaction-join path (JoinFromToken → participate)
// across all backends in the parity suite.
//
// The token value is never logged (Gate 3 / spec §8-H10) — only its emptiness
// is ever surfaced (as a derived boolean written into entity data).

// txTokenAttr is the CloudEvent extension attribute carrying the signed
// transaction routing token. Duplicated here (not imported from internal/grpc)
// so this binary stays free of internal/ imports.
const txTokenAttr = "cyodatxtoken"

// txTokenFromCloudEvent extracts the tx-token from a calc-request CloudEvent, or
// "" when absent (e.g. COMMIT_BEFORE_DISPATCH default dispatch, which carries no
// transaction context).
func txTokenFromCloudEvent(ce *cepb.CloudEvent) string {
	if ce == nil || ce.Attributes == nil {
		return ""
	}
	v, ok := ce.Attributes[txTokenAttr]
	if !ok {
		return ""
	}
	return v.GetCeString()
}

// cbConfig is the per-scenario configuration delivered via the pass-through
// ProcessorConfig.context string (JSON-encoded). It tells a callback processor
// which secondary model to write and the marker to stamp.
type cbConfig struct {
	SecondaryModel   string `json:"secondaryModel"`
	SecondaryVersion int    `json:"secondaryVersion"`
	Marker           string `json:"marker"`
}

// parseCallbackConfig decodes the calc request's parameters node — which the
// engine populates verbatim from ProcessorConfig.context as a JSON string — into
// a cbConfig. An empty parameters node yields a zero cbConfig.
func parseCallbackConfig(parameters json.RawMessage) (cbConfig, error) {
	var cfg cbConfig
	if len(parameters) == 0 {
		return cfg, nil
	}
	var ctxStr string
	if err := json.Unmarshal(parameters, &ctxStr); err != nil {
		return cfg, fmt.Errorf("callback config: expected JSON-string parameters, got %s: %w", parameters, err)
	}
	if ctxStr == "" {
		return cfg, nil
	}
	if err := json.Unmarshal([]byte(ctxStr), &cfg); err != nil {
		return cfg, fmt.Errorf("callback config: parse context %q: %w", ctxStr, err)
	}
	return cfg, nil
}

// callbackClient issues HTTP callbacks into cyoda-go, presenting the tx-token as
// X-Tx-Token so writes/reads join the originating transaction. It authenticates
// with the compute client's M2M bearer.
type callbackClient struct {
	baseURL string
	bearer  string
	hc      *http.Client
}

// newCallbackClient constructs a callback client, or nil when baseURL is empty
// (callback processors then report a clear error rather than panicking).
func newCallbackClient(baseURL, bearer string) *callbackClient {
	if baseURL == "" {
		return nil
	}
	return &callbackClient{
		baseURL: baseURL,
		bearer:  bearer,
		hc:      &http.Client{Timeout: 15 * time.Second},
	}
}

// cbResult is the HTTP outcome of a callback.
type cbResult struct {
	Status int
	Body   string
}

// do issues an HTTP callback. When txToken is non-empty it is sent as X-Tx-Token
// (joining T); when ifMatch is non-empty it is sent as If-Match.
func (c *callbackClient) do(ctx context.Context, method, path, body, txToken, ifMatch string) (cbResult, error) {
	var r io.Reader
	if body != "" {
		r = bytes.NewReader([]byte(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, r)
	if err != nil {
		return cbResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.bearer)
	req.Header.Set("Content-Type", "application/json")
	if txToken != "" {
		req.Header.Set("X-Tx-Token", txToken)
	}
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return cbResult{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return cbResult{Status: resp.StatusCode, Body: string(raw)}, nil
}

// createSecondary issues a POST /api/entity callback that creates a secondary
// entity within the joined transaction. On a 200 it returns the created entity
// id and the transactionId from the response envelope.
func (c *callbackClient) createSecondary(ctx context.Context, cfg cbConfig, txToken, status string) (res cbResult, entityID, txID string, err error) {
	version := cfg.SecondaryVersion
	if version == 0 {
		version = 1
	}
	path := fmt.Sprintf("/api/entity/JSON/%s/%d", cfg.SecondaryModel, version)
	body := fmt.Sprintf(`{"name":"child","amount":1,"status":%q}`, status)
	res, err = c.do(ctx, http.MethodPost, path, body, txToken, "")
	if err != nil {
		return res, "", "", err
	}
	if res.Status == http.StatusOK {
		entityID, txID = parseCreateResponse(res.Body)
	}
	return res, entityID, txID, nil
}

// getEntity issues a GET /api/entity/{id} callback within the joined
// transaction (read-your-own-writes).
func (c *callbackClient) getEntity(ctx context.Context, entityID, txToken string) (cbResult, error) {
	return c.do(ctx, http.MethodGet, "/api/entity/"+entityID, "", txToken, "")
}

// loopbackUpdate issues a PUT /api/entity/JSON/{id} (loopback, no transition)
// callback carrying an If-Match precondition, within the joined transaction.
func (c *callbackClient) loopbackUpdate(ctx context.Context, entityID, ifMatch, txToken, status string) (cbResult, error) {
	body := fmt.Sprintf(`{"name":"child","amount":2,"status":%q}`, status)
	return c.do(ctx, http.MethodPut, "/api/entity/JSON/"+entityID, body, txToken, ifMatch)
}

// parseCreateResponse extracts the first entity id and transactionId from a
// create/update response body: [{"transactionId":"...","entityIds":["uuid"]}].
func parseCreateResponse(body string) (entityID, txID string) {
	var arr []struct {
		TransactionID string   `json:"transactionId"`
		EntityIDs     []string `json:"entityIds"`
	}
	if json.Unmarshal([]byte(body), &arr) != nil || len(arr) == 0 {
		return "", ""
	}
	txID = arr[0].TransactionID
	if len(arr[0].EntityIDs) > 0 {
		entityID = arr[0].EntityIDs[0]
	}
	return entityID, txID
}

// entityDataStatus extracts data.status (as a string) from an entity envelope
// body (`{"meta":{...},"data":{...}}`). Returns "" when absent or on a non-200
// body (e.g. a 404 problem document).
func entityDataStatus(body string) string {
	var env struct {
		Data map[string]any `json:"data"`
	}
	if json.Unmarshal([]byte(body), &env) != nil {
		return ""
	}
	s, _ := env.Data["status"].(string)
	return s
}

// callbackProcessorFunc is a processor that may issue joined callbacks. It
// receives the tx-token (echoed on callbacks) and the callback client.
type callbackProcessorFunc func(ctx context.Context, entity *Entity, cfg cbConfig, token string, cb *callbackClient) (*Entity, error)

// callbackCriterionFunc is a criterion that may issue joined callbacks.
type callbackCriterionFunc func(ctx context.Context, entity *Entity, cfg cbConfig, token string, cb *callbackClient) (bool, error)

// withData returns a copy of entity with data replaced by the given map.
func withData(entity *Entity, data map[string]any) (*Entity, error) {
	out, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	return &Entity{ID: entity.ID, State: entity.State, Data: out}, nil
}

// decodeData unmarshals an entity's data into a fresh map (nil-safe).
func decodeData(entity *Entity) (map[string]any, error) {
	data := map[string]any{}
	if len(entity.Data) > 0 {
		if err := json.Unmarshal(entity.Data, &data); err != nil {
			return nil, err
		}
	}
	return data, nil
}

// newCallbackCatalog returns the callback processors + criteria. cb may be nil
// (no HTTP base configured); the processors then fail loudly on first use.
func newCallbackCatalog() (map[string]callbackProcessorFunc, map[string]callbackCriterionFunc) {
	requireCB := func(cb *callbackClient) error {
		if cb == nil {
			return fmt.Errorf("callback client unavailable: CYODA_COMPUTE_HTTP_BASE not set")
		}
		return nil
	}

	procs := map[string]callbackProcessorFunc{
		// cb-create-secondary — creates a secondary entity via a joined
		// callback and records lineage (secondaryId, secondaryTxId) plus the
		// derived tokenWasEmpty flag into the primary's data.
		"cb-create-secondary": func(ctx context.Context, entity *Entity, cfg cbConfig, token string, cb *callbackClient) (*Entity, error) {
			if err := requireCB(cb); err != nil {
				return nil, err
			}
			res, secID, secTx, err := cb.createSecondary(ctx, cfg, token, cfg.Marker)
			if err != nil {
				return nil, fmt.Errorf("callback create: %w", err)
			}
			if res.Status != http.StatusOK {
				return nil, fmt.Errorf("callback create status=%d body=%s", res.Status, res.Body)
			}
			data, err := decodeData(entity)
			if err != nil {
				return nil, err
			}
			data["secondaryId"] = secID
			data["secondaryTxId"] = secTx
			data["tokenWasEmpty"] = token == ""
			return withData(entity, data)
		},

		// cb-create-then-fail — creates a secondary via a joined callback, then
		// deliberately fails so T rolls back (atomicity proof).
		"cb-create-then-fail": func(ctx context.Context, entity *Entity, cfg cbConfig, token string, cb *callbackClient) (*Entity, error) {
			if err := requireCB(cb); err != nil {
				return nil, err
			}
			res, _, _, err := cb.createSecondary(ctx, cfg, token, cfg.Marker)
			if err != nil {
				return nil, fmt.Errorf("callback create: %w", err)
			}
			if res.Status != http.StatusOK {
				return nil, fmt.Errorf("callback create status=%d body=%s", res.Status, res.Body)
			}
			return nil, fmt.Errorf("processor deliberately fails after callback write")
		},

		// cb-read-your-writes — creates a secondary inside T (uncommitted), then
		// reads it back through a joined callback and records what it observed.
		"cb-read-your-writes": func(ctx context.Context, entity *Entity, cfg cbConfig, token string, cb *callbackClient) (*Entity, error) {
			if err := requireCB(cb); err != nil {
				return nil, err
			}
			res, secID, _, err := cb.createSecondary(ctx, cfg, token, cfg.Marker)
			if err != nil {
				return nil, fmt.Errorf("callback create: %w", err)
			}
			if res.Status != http.StatusOK {
				return nil, fmt.Errorf("callback create status=%d body=%s", res.Status, res.Body)
			}
			got, err := cb.getEntity(ctx, secID, token)
			if err != nil {
				return nil, fmt.Errorf("callback read: %w", err)
			}
			data, err := decodeData(entity)
			if err != nil {
				return nil, err
			}
			data["readbackFound"] = got.Status == http.StatusOK
			data["readbackMarker"] = entityDataStatus(got.Body)
			data["secondaryId"] = secID
			return withData(entity, data)
		},

		// cb-ifmatch-update — creates a secondary inside T, then issues a
		// loopback update with If-Match set to the create's in-T transactionId.
		"cb-ifmatch-update": func(ctx context.Context, entity *Entity, cfg cbConfig, token string, cb *callbackClient) (*Entity, error) {
			if err := requireCB(cb); err != nil {
				return nil, err
			}
			res, secID, secTx, err := cb.createSecondary(ctx, cfg, token, cfg.Marker)
			if err != nil {
				return nil, fmt.Errorf("callback create: %w", err)
			}
			if res.Status != http.StatusOK {
				return nil, fmt.Errorf("callback create status=%d body=%s", res.Status, res.Body)
			}
			upd, err := cb.loopbackUpdate(ctx, secID, secTx, token, cfg.Marker+"-updated")
			if err != nil {
				return nil, fmt.Errorf("callback if-match update: %w", err)
			}
			data, err := decodeData(entity)
			if err != nil {
				return nil, err
			}
			data["ifMatchStatus"] = float64(upd.Status)
			data["ifMatchOK"] = upd.Status == http.StatusOK
			data["secondaryId"] = secID
			return withData(entity, data)
		},
	}

	crit := map[string]callbackCriterionFunc{
		// cb-criterion-reads — GETs, through a joined callback, the secondary
		// entity a prior cascade step wrote into T (its id rides on the primary's
		// in-T data as secondaryId) and matches only when that uncommitted
		// secondary is visible with the expected marker. Outside T the GET 404s →
		// no match. This proves a criteria service's read joins T.
		"cb-criterion-reads": func(ctx context.Context, entity *Entity, cfg cbConfig, token string, cb *callbackClient) (bool, error) {
			if err := requireCB(cb); err != nil {
				return false, err
			}
			data, err := decodeData(entity)
			if err != nil {
				return false, err
			}
			secID, _ := data["secondaryId"].(string)
			if secID == "" {
				return false, nil
			}
			got, err := cb.getEntity(ctx, secID, token)
			if err != nil {
				return false, fmt.Errorf("callback read: %w", err)
			}
			if got.Status != http.StatusOK {
				return false, nil
			}
			return entityDataStatus(got.Body) == cfg.Marker, nil
		},
	}

	return procs, crit
}
