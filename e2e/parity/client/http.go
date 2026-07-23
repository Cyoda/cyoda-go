package client

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Client is the parity HTTP client. It wraps net/http and adds
// DisallowUnknownFields decoding to catch API drift.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// NewClient constructs a Client targeting the given cyoda HTTP base URL,
// authenticated with the given JWT bearer token. The HTTP client uses
// a 30-second timeout per request — long enough for slow-processor
// tests but bounded so a hung request fails the test rather than
// hanging the suite.
func NewClient(baseURL, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// reqOption configures a single doJSON request. Future per-method
// options (custom Accept header, raw body sink, etc.) implement this
// interface and are passed as variadic args. No options are defined
// in Task 1.3; this is the seam future methods will use.
type reqOption func(*reqConfig)

type reqConfig struct {
	// Reserved for future per-method options. Currently unused.
}

// decodeJSONResponse decodes a successful HTTP response body into out
// using DisallowUnknownFields. Skips decoding when out is nil. Treats
// an empty body (io.EOF immediately) as "nothing to decode" rather
// than an error so endpoints that legitimately return 200 with no
// body work correctly. Draining the body before return enables
// connection reuse by the underlying transport. The caller is
// responsible for closing the response body.
func decodeJSONResponse(resp *http.Response, out any) error {
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	dec := json.NewDecoder(resp.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		if errors.Is(err, io.EOF) {
			// Empty body with a non-nil out — endpoint returned no
			// content. Treat as "nothing to decode" rather than an
			// error. This also handles chunked responses where
			// ContentLength == -1 and the body is empty.
			return nil
		}
		return fmt.Errorf("decode response (status %d): %w", resp.StatusCode, err)
	}
	return nil
}

// doJSON issues an HTTP request, optionally with a JSON body, and
// decodes the response into out (if non-nil) using DisallowUnknownFields.
// Returns the HTTP status code and any transport, status, or decode error.
// Non-2xx responses are returned as errors that include the captured
// response body so cyoda's JSON error envelopes are visible in
// parity-test output. The opts parameter is a seam for future per-method
// options (custom Accept header, raw body sink, etc.).
func (c *Client) doJSON(t *testing.T, method, path string, body any, out any, opts ...reqOption) (int, error) {
	t.Helper()

	var cfg reqConfig
	for _, opt := range opts {
		opt(&cfg)
	}

	var bodyReader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("marshal request body: %w", err)
		}
		bodyReader = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(t.Context(), method, c.baseURL+path, bodyReader)
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("transport: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Capture the body for inclusion in the error message. This is the
		// hook for cyoda's JSON error envelopes (errorCode, message, etc.)
		// — useful for parity-test debugging.
		rawBody, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, fmt.Errorf("%s %s: status %d: %s", method, path, resp.StatusCode, string(rawBody))
	}

	if err := decodeJSONResponse(resp, out); err != nil {
		return resp.StatusCode, err
	}
	return resp.StatusCode, nil
}

// --- Operation methods ---
//
// Each method maps to one cyoda HTTP API operation. The methods are
// added incrementally as parity scenarios need them. Methods that fail
// (non-2xx status, decode error, transport error) call t.Fatalf with
// a clear message including the operation name and the response body
// where applicable.

// doRaw issues an HTTP request with the given method, a raw string body,
// and the standard Content-Type/Authorization headers. Returns the raw
// response body on success (2xx). Returns a descriptive error
// wrapping the response body for non-2xx status codes.
//
// On 409 Conflict with properties.retryable=true (SERIALIZABLE 40001/40P01
// aborts, classified by the server) the request is retried up to 5 times
// with a short backoff — the client's job is to replay against a fresh
// snapshot. Non-retryable 409s (business-logic conflicts) surface
// immediately so tests that assert them can see the first response. This
// is the minimum viable client retry; production clients would use
// bounded jitter and per-operation policies.
func (c *Client) doRaw(t *testing.T, method, path, body string) ([]byte, error) {
	t.Helper()
	return c.doRawWithHeaders(t, method, path, body, nil)
}

// isRetryableConflict reports whether a 409 body advertises
// properties.retryable=true (the server's signal that the transaction
// aborted cleanly and replaying against a fresh snapshot is safe).
func isRetryableConflict(body []byte) bool {
	var problem struct {
		Properties struct {
			Retryable bool `json:"retryable"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(body, &problem); err != nil {
		return false
	}
	return problem.Properties.Retryable
}

// ImportModel issues POST /api/model/import/JSON/SAMPLE_DATA/{name}/{version}
// with the given sample-data document as the body.
func (c *Client) ImportModel(t *testing.T, modelName string, modelVersion int, sampleDoc string) error {
	t.Helper()
	path := fmt.Sprintf("/api/model/import/JSON/SAMPLE_DATA/%s/%d", modelName, modelVersion)
	_, err := c.doRaw(t, http.MethodPost, path, sampleDoc)
	return err
}

// LockModel issues PUT /api/model/{name}/{version}/lock.
func (c *Client) LockModel(t *testing.T, modelName string, modelVersion int) error {
	t.Helper()
	path := fmt.Sprintf("/api/model/%s/%d/lock", modelName, modelVersion)
	_, err := c.doJSON(t, http.MethodPut, path, nil, nil)
	return err
}

// SetChangeLevel issues POST /api/model/{name}/{version}/changeLevel/{level}.
// Levels: STRUCTURAL, TYPE, ARRAY_ELEMENTS, ARRAY_LENGTH (or "" to unset).
func (c *Client) SetChangeLevel(t *testing.T, modelName string, modelVersion int, level string) error {
	t.Helper()
	path := fmt.Sprintf("/api/model/%s/%d/changeLevel/%s", modelName, modelVersion, level)
	_, err := c.doJSON(t, http.MethodPost, path, nil, nil)
	return err
}

// CreateEntityRaw issues POST /api/entity/JSON/{name}/{version} and returns
// the HTTP status code without decoding the body. Used by tests that expect
// non-200 responses (e.g., strict-validate rejections).
func (c *Client) CreateEntityRaw(t *testing.T, modelName string, modelVersion int, body string) (int, []byte, error) {
	t.Helper()
	path := fmt.Sprintf("/api/entity/JSON/%s/%d", modelName, modelVersion)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, c.baseURL+path, strings.NewReader(body))
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("transport: %w", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, raw, nil
}

// ImportWorkflow issues POST /api/model/{name}/{version}/workflow/import
// with the given workflow JSON as the body.
func (c *Client) ImportWorkflow(t *testing.T, modelName string, modelVersion int, workflowJSON string) error {
	t.Helper()
	path := fmt.Sprintf("/api/model/%s/%d/workflow/import", modelName, modelVersion)
	_, err := c.doRaw(t, http.MethodPost, path, workflowJSON)
	return err
}

// CreateEntity issues POST /api/entity/JSON/{name}/{version} with the
// given entity body. Returns the new entity ID as uuid.UUID so callers
// can pass it directly to GetEntity (which also takes uuid.UUID).
func (c *Client) CreateEntity(t *testing.T, modelName string, modelVersion int, body string) (uuid.UUID, error) {
	t.Helper()
	path := fmt.Sprintf("/api/entity/JSON/%s/%d", modelName, modelVersion)
	raw, err := c.doRaw(t, http.MethodPost, path, body)
	if err != nil {
		return uuid.Nil, err
	}
	// The response is an array of EntityTransactionInfo objects, even for
	// a single entity create: [{"transactionId":"...","entityIds":["uuid"]}].
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var txInfos []EntityTransactionInfo
	if err := dec.Decode(&txInfos); err != nil {
		return uuid.Nil, fmt.Errorf("decode CreateEntity response: %w", err)
	}
	if len(txInfos) == 0 {
		return uuid.Nil, fmt.Errorf("CreateEntity returned empty array")
	}
	if len(txInfos[0].EntityIDs) != 1 {
		return uuid.Nil, fmt.Errorf("CreateEntity returned %d ids, want 1", len(txInfos[0].EntityIDs))
	}
	id, err := uuid.Parse(txInfos[0].EntityIDs[0])
	if err != nil {
		return uuid.Nil, fmt.Errorf("parse entity ID %q: %w", txInfos[0].EntityIDs[0], err)
	}
	return id, nil
}

// CreateEntityWithTxID issues POST /api/entity/JSON/{name}/{version} and
// returns both the entity ID and the transactionId from the response.
func (c *Client) CreateEntityWithTxID(t *testing.T, modelName string, modelVersion int, body string) (uuid.UUID, string, error) {
	t.Helper()
	path := fmt.Sprintf("/api/entity/JSON/%s/%d", modelName, modelVersion)
	raw, err := c.doRaw(t, http.MethodPost, path, body)
	if err != nil {
		return uuid.Nil, "", err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var txInfos []EntityTransactionInfo
	if err := dec.Decode(&txInfos); err != nil {
		return uuid.Nil, "", fmt.Errorf("decode CreateEntityWithTxID response: %w", err)
	}
	if len(txInfos) == 0 || len(txInfos[0].EntityIDs) != 1 {
		return uuid.Nil, "", fmt.Errorf("unexpected CreateEntityWithTxID response: %v", txInfos)
	}
	id, err := uuid.Parse(txInfos[0].EntityIDs[0])
	if err != nil {
		return uuid.Nil, "", fmt.Errorf("parse entity ID: %w", err)
	}
	return id, txInfos[0].TransactionID, nil
}

// ListModels issues GET /api/model/ and returns the parsed model list.
// Canonical: docs/cyoda/openapi.yml:2764 (getAvailableEntityModels).
func (c *Client) ListModels(t *testing.T) ([]EntityModelDto, error) {
	t.Helper()
	var models []EntityModelDto
	if _, err := c.doJSON(t, http.MethodGet, "/api/model/", nil, &models); err != nil {
		return nil, err
	}
	return models, nil
}

// ExportModel issues GET /api/model/export/{converter}/{name}/{version}.
// Returns raw JSON. Canonical: docs/cyoda/openapi.yml:2805 (exportMetadata).
func (c *Client) ExportModel(t *testing.T, converter, modelName string, modelVersion int) (json.RawMessage, error) {
	t.Helper()
	path := fmt.Sprintf("/api/model/export/%s/%s/%d", converter, modelName, modelVersion)
	var raw json.RawMessage
	if _, err := c.doJSON(t, http.MethodGet, path, nil, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// ExportWorkflow issues GET /api/model/{name}/{version}/workflow/export.
// Returns raw JSON. Canonical: docs/cyoda/openapi.yml:3415.
func (c *Client) ExportWorkflow(t *testing.T, modelName string, modelVersion int) (json.RawMessage, error) {
	t.Helper()
	path := fmt.Sprintf("/api/model/%s/%d/workflow/export", modelName, modelVersion)
	var raw json.RawMessage
	if _, err := c.doJSON(t, http.MethodGet, path, nil, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// UnlockModel issues PUT /api/model/{name}/{version}/unlock.
// Canonical: docs/cyoda/openapi.yml:3338.
func (c *Client) UnlockModel(t *testing.T, modelName string, modelVersion int) error {
	t.Helper()
	path := fmt.Sprintf("/api/model/%s/%d/unlock", modelName, modelVersion)
	_, err := c.doJSON(t, http.MethodPut, path, nil, nil)
	return err
}

// DeleteModel issues DELETE /api/model/{name}/{version}.
// Canonical: docs/cyoda/openapi.yml:3094 (deleteEntityModel).
func (c *Client) DeleteModel(t *testing.T, modelName string, modelVersion int) error {
	t.Helper()
	path := fmt.Sprintf("/api/model/%s/%d", modelName, modelVersion)
	_, err := c.doJSON(t, http.MethodDelete, path, nil, nil)
	return err
}

// GetEntity issues GET /api/entity/{entityId}.
//
// Canonical: docs/cyoda/openapi.yml line 1055 (`getOneEntity`).
// Per approved deviation A1, the response is the {type, data, meta}
// envelope (the parity-local EntityResult type), not bare data as the
// published OpenAPI spec shows. Per approved deviation A2, the meta
// envelope on getOneEntity includes modelKey.
func (c *Client) GetEntity(t *testing.T, entityID uuid.UUID) (EntityResult, error) {
	t.Helper()
	var ent EntityResult
	if _, err := c.doJSON(t, http.MethodGet, "/api/entity/"+entityID.String(), nil, &ent); err != nil {
		return EntityResult{}, err
	}
	return ent, nil
}

// DeleteEntity issues DELETE /api/entity/{entityId}.
// Canonical: docs/cyoda/openapi.yml:1147 (deleteSingleEntity).
func (c *Client) DeleteEntity(t *testing.T, entityID uuid.UUID) error {
	t.Helper()
	path := "/api/entity/" + entityID.String()
	_, err := c.doJSON(t, http.MethodDelete, path, nil, nil)
	return err
}

// GetEntityChanges issues GET /api/entity/{entityId}/changes.
// Returns the change history as []EntityChangeMeta.
// Canonical: docs/cyoda/openapi.yml:1207 (getEntityChangesMetadata).
func (c *Client) GetEntityChanges(t *testing.T, entityID uuid.UUID) ([]EntityChangeMeta, error) {
	t.Helper()
	path := "/api/entity/" + entityID.String() + "/changes"
	var changes []EntityChangeMeta
	if _, err := c.doJSON(t, http.MethodGet, path, nil, &changes); err != nil {
		return nil, err
	}
	return changes, nil
}

// GetEntityChangesAt issues GET /api/entity/{entityId}/changes?pointInTime=<t>.
// Returns the change history truncated to entries at or before the supplied
// timestamp.
// Canonical: docs/cyoda/openapi.yml (getEntityChangesMetadata with pointInTime query param).
func (c *Client) GetEntityChangesAt(t *testing.T, entityID uuid.UUID, pointInTime time.Time) ([]EntityChangeMeta, error) {
	t.Helper()
	path := fmt.Sprintf("/api/entity/%s/changes?pointInTime=%s", entityID.String(), pointInTime.Format(time.RFC3339Nano))
	var changes []EntityChangeMeta
	if _, err := c.doJSON(t, http.MethodGet, path, nil, &changes); err != nil {
		return nil, err
	}
	return changes, nil
}

// ListEntitiesByModel issues GET /api/entity/{name}/{version}.
// Returns the entity list; each EntityResult includes modelKey (A2 abandoned).
// Canonical: docs/cyoda/openapi.yml:1326 (getAllEntities).
func (c *Client) ListEntitiesByModel(t *testing.T, modelName string, modelVersion int) ([]EntityResult, error) {
	t.Helper()
	path := fmt.Sprintf("/api/entity/%s/%d", modelName, modelVersion)
	var entities []EntityResult
	if _, err := c.doJSON(t, http.MethodGet, path, nil, &entities); err != nil {
		return nil, err
	}
	return entities, nil
}

// ListEntitiesByModelAt issues GET /api/entity/{name}/{version}?pointInTime=<t>.
// Returns the entity list as it existed at the given point in time (E3).
// Canonical: docs/cyoda/openapi.yml (getAllEntities with pointInTime query param).
func (c *Client) ListEntitiesByModelAt(t *testing.T, modelName string, modelVersion int, pointInTime time.Time) ([]EntityResult, error) {
	t.Helper()
	path := fmt.Sprintf("/api/entity/%s/%d?pointInTime=%s", modelName, modelVersion, pointInTime.Format(time.RFC3339Nano))
	var entities []EntityResult
	if _, err := c.doJSON(t, http.MethodGet, path, nil, &entities); err != nil {
		return nil, err
	}
	return entities, nil
}

// GetEntityAt issues GET /api/entity/{entityId}?pointInTime=<t>.
// Returns the entity as it was at the given point in time.
// Canonical: docs/cyoda/openapi.yml:1055 (getOneEntity with pointInTime query param).
// This is the code path that exercised the GetAsAt bug (PR #173).
func (c *Client) GetEntityAt(t *testing.T, entityID uuid.UUID, pointInTime time.Time) (EntityResult, error) {
	t.Helper()
	path := fmt.Sprintf("/api/entity/%s?pointInTime=%s", entityID.String(), pointInTime.Format(time.RFC3339Nano))
	var ent EntityResult
	if _, err := c.doJSON(t, http.MethodGet, path, nil, &ent); err != nil {
		return EntityResult{}, err
	}
	return ent, nil
}

// GetEntityAtRaw issues GET /api/entity/{entityId}?pointInTime=<t> and
// returns the HTTP status + raw body bytes without raising on non-2xx,
// mirroring the *Raw pattern of LockModelRaw / GetEntityChangesRaw.
// Used by negative-path tests (e.g. external-api 12/04) that need to
// inspect the error envelope on 404.
func (c *Client) GetEntityAtRaw(t *testing.T, entityID uuid.UUID, pointInTime time.Time) (int, []byte, error) {
	t.Helper()
	path := fmt.Sprintf("/api/entity/%s?pointInTime=%s", entityID.String(), pointInTime.Format(time.RFC3339Nano))
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, c.baseURL+path, strings.NewReader(""))
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("transport: %w", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, raw, nil
}

// GetEntityByTransactionID issues GET /api/entity/{entityId}?transactionId=<tx>
// and decodes the EntityResult envelope. Returned for the
// transactionId-scoped GET surface used by external-api 07/02.
// Canonical: docs/cyoda/openapi.yml:1055 (getOneEntity with transactionId query param).
func (c *Client) GetEntityByTransactionID(t *testing.T, entityID uuid.UUID, txID string) (EntityResult, error) {
	t.Helper()
	path := fmt.Sprintf("/api/entity/%s?transactionId=%s", entityID.String(), url.QueryEscape(txID))
	var ent EntityResult
	if _, err := c.doJSON(t, http.MethodGet, path, nil, &ent); err != nil {
		return EntityResult{}, err
	}
	return ent, nil
}

// GetEntityByTransactionIDRaw issues GET /api/entity/{entityId}?transactionId=<tx>
// and returns the HTTP status + raw body bytes without raising on non-2xx,
// mirroring the *Raw pattern. Used by external-api 12/05 to assert the
// 404 body for a bogus transactionId.
func (c *Client) GetEntityByTransactionIDRaw(t *testing.T, entityID uuid.UUID, txID string) (int, []byte, error) {
	t.Helper()
	path := fmt.Sprintf("/api/entity/%s?transactionId=%s", entityID.String(), url.QueryEscape(txID))
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, c.baseURL+path, strings.NewReader(""))
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("transport: %w", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, raw, nil
}

// GetEntityAtBodyRaw issues GET /api/entity/{entityId}?pointInTime=<t> and
// returns the raw status code and response body. Used by tests that need
// to compare error-response bodies byte-for-byte (tenant-isolation
// existence-oracle pinning).
func (c *Client) GetEntityAtBodyRaw(t *testing.T, entityID uuid.UUID, pointInTime time.Time) (int, []byte, error) {
	t.Helper()
	path := fmt.Sprintf("/api/entity/%s?pointInTime=%s", entityID.String(), pointInTime.UTC().Format(time.RFC3339Nano))
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, c.baseURL+path, strings.NewReader(""))
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("transport: %w", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, raw, nil
}

// GetEntityByTransactionIDBodyRaw issues GET /api/entity/{entityId}?transactionId=<tx>
// and returns the raw status code and response body. Used by tenant-isolation
// tests that need to compare error-response bodies byte-for-byte to assert
// no existence oracle leaks across tenants via the transactionId temporal
// query param.
func (c *Client) GetEntityByTransactionIDBodyRaw(t *testing.T, entityID uuid.UUID, txID string) (int, []byte, error) {
	t.Helper()
	path := fmt.Sprintf("/api/entity/%s?transactionId=%s", entityID.String(), txID)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, c.baseURL+path, strings.NewReader(""))
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("transport: %w", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, raw, nil
}

// GetEntityChangesAtBodyRaw issues GET /api/entity/{entityId}/changes?pointInTime=<t>
// and returns the raw status code and response body. Used by tenant-isolation
// tests that need to compare error-response bodies byte-for-byte across the
// change-history temporal query path.
func (c *Client) GetEntityChangesAtBodyRaw(t *testing.T, entityID uuid.UUID, pointInTime time.Time) (int, []byte, error) {
	t.Helper()
	path := fmt.Sprintf("/api/entity/%s/changes?pointInTime=%s", entityID.String(), pointInTime.UTC().Format(time.RFC3339Nano))
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, c.baseURL+path, strings.NewReader(""))
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("transport: %w", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, raw, nil
}

// UpdateEntityData issues PUT /api/entity/JSON/{entityId} to update
// entity data without firing a workflow transition.
// Canonical: docs/cyoda/openapi.yml (collection updateOne).
func (c *Client) UpdateEntityData(t *testing.T, entityID uuid.UUID, body string) error {
	t.Helper()
	path := "/api/entity/JSON/" + entityID.String()
	_, err := c.doRaw(t, http.MethodPut, path, body)
	return err
}

// UpdateEntity issues PUT /api/entity/JSON/{entityId}/{transition} with the
// given entity body. Returns an error if the request fails.
// Canonical: docs/cyoda/openapi.yml:2037 (updateOne / transition).
func (c *Client) UpdateEntity(t *testing.T, entityID uuid.UUID, transition, body string) error {
	t.Helper()
	path := fmt.Sprintf("/api/entity/JSON/%s/%s", entityID.String(), transition)
	_, err := c.doRaw(t, http.MethodPut, path, body)
	return err
}

// CollectionItem is one entry in a POST /api/entity/{format} body for
// heterogeneous collection creation. Payload is a JSON-encoded string
// (not a nested object) per the wire contract — the handler in
// internal/domain/entity.Handler.CreateCollection unmarshals it as such.
type CollectionItem struct {
	ModelName    string
	ModelVersion int
	Payload      string
}

// CreateEntitiesCollection issues POST /api/entity/JSON with a
// heterogeneous batch. Returns the list of created entity IDs (parsed
// from the response array's entityIds field).
func (c *Client) CreateEntitiesCollection(t *testing.T, items []CollectionItem) ([]uuid.UUID, error) {
	t.Helper()
	return c.CreateEntitiesCollectionWithWindow(t, items, 0)
}

// CreateEntitiesCollectionWithWindow issues POST /api/entity/JSON with the
// given heterogeneous batch and an optional `transactionWindow` query
// parameter. window <= 0 omits the query parameter (server applies its
// default). Returns the list of created entity IDs concatenated across
// all chunk elements in commit order. Used by parity scenarios that pin
// the chunking contract from issue #227.
func (c *Client) CreateEntitiesCollectionWithWindow(t *testing.T, items []CollectionItem, window int) ([]uuid.UUID, error) {
	t.Helper()
	raw, err := c.CreateEntitiesCollectionRawWithWindow(t, items, window)
	if err != nil {
		return nil, err
	}
	// Response shape: [{"transactionId":"...","entityIds":["<uuid>", ...]}]
	var parsed []EntityTransactionInfo
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("decode CreateEntitiesCollection response: %w (body=%s)", err, string(raw))
	}
	var out []uuid.UUID
	for _, tx := range parsed {
		for _, idStr := range tx.EntityIDs {
			id, perr := uuid.Parse(idStr)
			if perr != nil {
				return nil, fmt.Errorf("parse entityId %q: %w", idStr, perr)
			}
			out = append(out, id)
		}
	}
	return out, nil
}

// CreateEntitiesCollectionRawWithWindow issues POST /api/entity/JSON with
// the given items and an optional `transactionWindow` query parameter,
// returning the raw response body. Used by chunking-contract parity
// scenarios that need to inspect the per-chunk response array directly.
func (c *Client) CreateEntitiesCollectionRawWithWindow(t *testing.T, items []CollectionItem, window int) ([]byte, error) {
	t.Helper()
	type modelRef struct {
		Name    string `json:"name"`
		Version int    `json:"version"`
	}
	type rawItem struct {
		Model   modelRef `json:"model"`
		Payload string   `json:"payload"`
	}
	rawItems := make([]rawItem, 0, len(items))
	for _, it := range items {
		rawItems = append(rawItems, rawItem{
			Model:   modelRef{Name: it.ModelName, Version: it.ModelVersion},
			Payload: it.Payload,
		})
	}
	body, err := json.Marshal(rawItems)
	if err != nil {
		return nil, fmt.Errorf("marshal CreateEntitiesCollection items: %w", err)
	}
	path := "/api/entity/JSON"
	if window > 0 {
		path = fmt.Sprintf("%s?transactionWindow=%d", path, window)
	}
	return c.doRaw(t, http.MethodPost, path, string(body))
}

// UpdateCollectionItem is one entry in a PUT /api/entity/{format} body.
// Payload is a JSON-encoded string (not a nested object) per the collection
// update wire contract. IfMatch is the optional per-item optimistic-
// concurrency precondition added by issue #228; when populated, the server
// rejects the item with ENTITY_MODIFIED if the entity's current
// transactionId no longer matches.
type UpdateCollectionItem struct {
	ID         uuid.UUID
	Payload    string
	Transition string // optional; "" = loopback
	IfMatch    string // optional per-item ifMatch (issue #228)
}

// UpdateCollection issues PUT /api/entity/JSON with a batch of
// UpdateCollectionItem. Returns the raw response body on success so
// callers can assert the [{transactionId, entityIds}] shape, or an error
// wrapping the body on non-2xx.
// Canonical: docs/cyoda/openapi.yml (collection update).
func (c *Client) UpdateCollection(t *testing.T, items []UpdateCollectionItem) ([]byte, error) {
	t.Helper()
	return c.UpdateCollectionWithWindow(t, items, 0)
}

// UpdateCollectionWithWindow issues PUT /api/entity/JSON with the given
// batch and an optional `transactionWindow` query parameter. window <= 0
// omits the query parameter (server applies its default). Returns the
// raw response body on success or an error wrapping the body on non-2xx.
func (c *Client) UpdateCollectionWithWindow(t *testing.T, items []UpdateCollectionItem, window int) ([]byte, error) {
	t.Helper()
	body, err := marshalUpdateCollectionItems(items)
	if err != nil {
		return nil, err
	}
	path := "/api/entity/JSON"
	if window > 0 {
		path = fmt.Sprintf("%s?transactionWindow=%d", path, window)
	}
	return c.doRaw(t, http.MethodPut, path, string(body))
}

// UpdateCollectionRawWithWindow is the negative-path companion to
// UpdateCollectionWithWindow: it returns (status, body, transport-err)
// without raising on non-2xx. Used by parity scenarios that assert the
// chunk-rollback contract (e.g. invalid transition rejecting at chunk 0
// with a 4xx error envelope).
func (c *Client) UpdateCollectionRawWithWindow(t *testing.T, items []UpdateCollectionItem, window int) (int, []byte, error) {
	t.Helper()
	body, err := marshalUpdateCollectionItems(items)
	if err != nil {
		return 0, nil, err
	}
	path := "/api/entity/JSON"
	if window > 0 {
		path = fmt.Sprintf("%s?transactionWindow=%d", path, window)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("transport: %w", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, raw, nil
}

// marshalUpdateCollectionItems renders the per-item update wire shape.
// IfMatch is emitted with `omitempty` so existing scenarios that do not
// supply it produce identical bytes to the pre-#228 wire format.
func marshalUpdateCollectionItems(items []UpdateCollectionItem) ([]byte, error) {
	type rawItem struct {
		ID         string `json:"id"`
		Payload    string `json:"payload"`
		Transition string `json:"transition,omitempty"`
		IfMatch    string `json:"ifMatch,omitempty"`
	}
	raw := make([]rawItem, 0, len(items))
	for _, it := range items {
		raw = append(raw, rawItem{
			ID:         it.ID.String(),
			Payload:    it.Payload,
			Transition: it.Transition,
			IfMatch:    it.IfMatch,
		})
	}
	body, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("marshal UpdateCollection items: %w", err)
	}
	return body, nil
}

// CollectionChunkResult mirrors the per-chunk response shape from
// PUT/POST /api/entity/{format}. Used by parity scenarios that need to
// assert per-chunk fields (transactionId, entityIds, failed[]) without
// repeating the map[string]any decode dance. EntityIDs is decoded as a
// concrete slice — the server emits `[]` (not `null`) on a chunk where
// every item failed via per-item isolation (issue #228 I2).
type CollectionChunkResult struct {
	TransactionID string                       `json:"transactionId,omitempty"`
	EntityIDs     []string                     `json:"entityIds"`
	Error         *CollectionChunkError        `json:"error,omitempty"`
	Failed        []CollectionChunkItemFailure `json:"failed,omitempty"`
}

// CollectionChunkError carries the per-chunk failure shape (chunk-wide
// rollback path). ChunkIndex is the zero-based position of the failing
// chunk in commit order.
type CollectionChunkError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	ChunkIndex int    `json:"chunkIndex"`
}

// CollectionChunkItemFailure documents a single per-item failure that did
// NOT roll the chunk back. Reserved for ENTITY_MODIFIED conflicts on items
// carrying an IfMatch precondition (issue #228).
type CollectionChunkItemFailure struct {
	EntityID string                 `json:"entityId"`
	Error    CollectionChunkItemErr `json:"error"`
}

// CollectionChunkItemErr is the per-item failure inner object.
type CollectionChunkItemErr struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	ItemIndex int    `json:"itemIndex"`
}

// GetEntityRaw issues GET /api/entity/{entityId} and returns the HTTP
// status code without decoding the body. Used by tests that expect
// non-200 responses (e.g., tenant isolation cross-tenant GET → 404).
func (c *Client) GetEntityRaw(t *testing.T, entityID uuid.UUID) (int, error) {
	t.Helper()
	path := "/api/entity/" + entityID.String()
	return c.doJSON(t, http.MethodGet, path, nil, nil)
}

// DeleteEntityRaw issues DELETE /api/entity/{entityId} and returns the
// HTTP status code without fataling. Used by tests that expect the
// delete to fail (e.g., tenant isolation cross-tenant delete → 404).
func (c *Client) DeleteEntityRaw(t *testing.T, entityID uuid.UUID) (int, error) {
	t.Helper()
	path := "/api/entity/" + entityID.String()
	return c.doJSON(t, http.MethodDelete, path, nil, nil)
}

// GetWorkflowFinished issues GET /api/audit/entity/{entityId}/workflow/{txId}/finished
// and returns the HTTP status code and the decoded JSON response body.
// On non-2xx responses the returned map is nil and the error contains the
// response body for diagnostics.
func (c *Client) GetWorkflowFinished(t *testing.T, entityID uuid.UUID, txID string) (int, map[string]any, error) {
	t.Helper()
	path := fmt.Sprintf("/api/audit/entity/%s/workflow/%s/finished", entityID.String(), txID)
	var result map[string]any
	status, err := c.doJSON(t, http.MethodGet, path, nil, &result)
	if err != nil {
		return status, nil, err
	}
	return status, result, nil
}

// GetAuditEventsRaw issues GET /api/audit/entity/{entityId} and returns
// the HTTP status code without decoding. Used by tests that expect
// non-200 responses (e.g., tenant isolation cross-tenant audit → 404).
func (c *Client) GetAuditEventsRaw(t *testing.T, entityID uuid.UUID) (int, error) {
	t.Helper()
	path := "/api/audit/entity/" + entityID.String()
	return c.doJSON(t, http.MethodGet, path, nil, nil)
}

// MessageHeaderInput collects the optional message-header fields cyoda-go
// reads from HTTP headers on POST /api/message/new/{subject}. Subject is
// in the path, so it is not part of this struct.
//
// Content-Type is sent as the standard HTTP Content-Type header; if the
// caller leaves it empty it defaults to "application/json". Content-Encoding
// is sent as the standard Content-Encoding header. The X-* fields are sent
// as the corresponding cyoda-specific request headers. Empty fields are
// omitted from the request.
//
// Source of truth: api/generated.go NewMessageParams — all fields are
// ParamLocationHeader (Content-Type and Content-Length are required;
// Content-Encoding and X-* are optional).
type MessageHeaderInput struct {
	ContentType     string
	ContentEncoding string
	MessageID       string
	UserID          string
	Recipient       string
	ReplyTo         string
	CorrelationID   string
}

// doRawWithHeaders is like doRaw but accepts caller-supplied HTTP headers.
// Headers in extraHeaders are applied first; the client's Authorization
// header is always set last from c.token, so caller-supplied headers
// CANNOT override Authorization. Content-Type defaults to
// "application/json" when extraHeaders does not contain one.
func (c *Client) doRawWithHeaders(t *testing.T, method, path, body string, extraHeaders http.Header) ([]byte, error) {
	t.Helper()
	const maxAttempts = 5
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		req, err := http.NewRequestWithContext(t.Context(), method, c.baseURL+path, strings.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("build request: %w", err)
		}
		// Apply caller-supplied headers first; Authorization is overwritten below.
		for k, vs := range extraHeaders {
			req.Header[k] = append([]string(nil), vs...) // defensive copy; replaces any existing
		}
		// Fall back to application/json if the caller didn't specify Content-Type.
		if req.Header.Get("Content-Type") == "" {
			req.Header.Set("Content-Type", "application/json")
		}
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("transport: %w", err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return raw, nil
		}
		if resp.StatusCode == http.StatusConflict && isRetryableConflict(raw) && attempt < maxAttempts-1 {
			time.Sleep(time.Duration(10*(attempt+1)) * time.Millisecond)
			lastErr = fmt.Errorf("%s %s: status 409: %s", method, path, string(raw))
			continue
		}
		return nil, fmt.Errorf("%s %s: status %d: %s", method, path, resp.StatusCode, string(raw))
	}
	return nil, lastErr
}

// CreateMessage issues POST /api/message/new/{subject} with the given
// payload wrapped in the edge-message envelope {payload, metaData}.
// Returns the message ID.
// Canonical: docs/cyoda/openapi.yml:2401.
func (c *Client) CreateMessage(t *testing.T, subject, payload string) (string, error) {
	t.Helper()
	path := "/api/message/new/" + subject
	body := fmt.Sprintf(`{"payload": %s, "metaData": {"source": "parity"}}`, payload)
	raw, err := c.doRaw(t, http.MethodPost, path, body)
	if err != nil {
		return "", err
	}
	// Response is an array of EntityTransactionInfo-like objects.
	var results []EntityTransactionInfo
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&results); err != nil {
		return "", fmt.Errorf("decode CreateMessage response: %w", err)
	}
	if len(results) == 0 || len(results[0].EntityIDs) == 0 {
		return "", fmt.Errorf("CreateMessage returned empty entity IDs")
	}
	return results[0].EntityIDs[0], nil
}

// CreateMessageWithHeaders is the header-rich variant of CreateMessage.
// It sends the fields in MessageHeaderInput as HTTP request headers so
// cyoda-go's generated handler reads them via NewMessageParams. The body
// envelope is identical to CreateMessage: {"payload": <payload>,
// "metaData": {"source": "parity"}}.
//
// If header.ContentType is empty it defaults to "application/json".
// Empty fields in header are omitted from the request.
// Returns the new message ID.
func (c *Client) CreateMessageWithHeaders(t *testing.T, subject, payload string, header MessageHeaderInput) (string, error) {
	t.Helper()
	path := "/api/message/new/" + subject
	body := fmt.Sprintf(`{"payload": %s, "metaData": {"source": "parity"}}`, payload)

	h := make(http.Header)
	ct := header.ContentType
	if ct == "" {
		ct = "application/json"
	}
	h.Set("Content-Type", ct)
	if header.ContentEncoding != "" {
		h.Set("Content-Encoding", header.ContentEncoding)
	}
	if header.MessageID != "" {
		h.Set("X-Message-ID", header.MessageID)
	}
	if header.UserID != "" {
		h.Set("X-User-ID", header.UserID)
	}
	if header.Recipient != "" {
		h.Set("X-Recipient", header.Recipient)
	}
	if header.ReplyTo != "" {
		h.Set("X-Reply-To", header.ReplyTo)
	}
	if header.CorrelationID != "" {
		h.Set("X-Correlation-ID", header.CorrelationID)
	}

	raw, err := c.doRawWithHeaders(t, http.MethodPost, path, body, h)
	if err != nil {
		return "", err
	}
	var results []EntityTransactionInfo
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&results); err != nil {
		return "", fmt.Errorf("decode CreateMessageWithHeaders response: %w", err)
	}
	if len(results) == 0 || len(results[0].EntityIDs) == 0 {
		return "", fmt.Errorf("CreateMessageWithHeaders returned empty entity IDs")
	}
	return results[0].EntityIDs[0], nil
}

// GetMessage issues GET /api/message/{messageId} and returns the raw
// response body as a map. The response shape is {header, metaData, content}.
// Canonical: docs/cyoda/openapi.yml:2598.
func (c *Client) GetMessage(t *testing.T, messageID string) (map[string]any, error) {
	t.Helper()
	path := "/api/message/" + messageID
	var result map[string]any
	if _, err := c.doJSON(t, http.MethodGet, path, nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// DeleteMessage issues DELETE /api/message/{messageId}.
func (c *Client) DeleteMessage(t *testing.T, messageID string) error {
	t.Helper()
	path := "/api/message/" + messageID
	_, err := c.doJSON(t, http.MethodDelete, path, nil, nil)
	return err
}

// DeleteMessages issues DELETE /api/message with a JSON-array body of
// message IDs. Returns the list of actually-deleted IDs from the
// response. Paging by transactionSize is supported by the server via
// query param (default 1000); this helper does not expose it because
// every parity test deletes well under 1000 IDs at a time.
//
// Canonical: api/openapi.yaml deleteMessages operation. Despite the
// generated DeleteMessagesParams struct only carrying TransactionSize,
// the server reads the ID list from the request body — the param
// struct is just for the query knob.
func (c *Client) DeleteMessages(t *testing.T, ids []string) ([]string, error) {
	t.Helper()
	body, err := json.Marshal(ids)
	if err != nil {
		return nil, fmt.Errorf("marshal DeleteMessages ids: %w", err)
	}
	raw, err := c.doRaw(t, http.MethodDelete, "/api/message", string(body))
	if err != nil {
		return nil, err
	}
	// Response is [{"entityIds":[...],"success":true}].
	var results []struct {
		EntityIDs []string `json:"entityIds"`
		Success   bool     `json:"success"`
	}
	if err := json.Unmarshal(raw, &results); err != nil {
		return nil, fmt.Errorf("decode DeleteMessages response: %w (body=%s)", err, string(raw))
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("DeleteMessages returned empty results array")
	}
	return results[0].EntityIDs, nil
}

// SubmitAsyncSearch issues POST /api/search/async/{name}/{version} with
// the given condition JSON. Returns the jobId (bare JSON string) for
// status/results polling.
// Canonical: api/openapi.yaml /search/async/{entityName}/{modelVersion}.
func (c *Client) SubmitAsyncSearch(t *testing.T, modelName string, modelVersion int, condition string) (string, error) {
	t.Helper()
	path := fmt.Sprintf("/api/search/async/%s/%d", modelName, modelVersion)
	raw, err := c.doRaw(t, http.MethodPost, path, condition)
	if err != nil {
		return "", err
	}
	var jobID string
	if err := json.Unmarshal(raw, &jobID); err != nil {
		return "", fmt.Errorf("decode SubmitAsyncSearch response: %w (body=%s)", err, string(raw))
	}
	if jobID == "" {
		return "", fmt.Errorf("SubmitAsyncSearch returned empty jobId (body=%s)", string(raw))
	}
	return jobID, nil
}

// GetAsyncSearchStatus issues GET /api/search/async/{jobId}/status.
// Returns the searchJobStatus field only: one of RUNNING, SUCCESSFUL,
// FAILED, CANCELLED, NOT_FOUND.
// Canonical: api/openapi.yaml /search/async/{jobId}/status.
func (c *Client) GetAsyncSearchStatus(t *testing.T, jobID string) (string, error) {
	t.Helper()
	path := fmt.Sprintf("/api/search/async/%s/status", jobID)
	raw, err := c.doRaw(t, http.MethodGet, path, "")
	if err != nil {
		return "", err
	}
	var resp struct {
		SearchJobStatus string `json:"searchJobStatus"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", fmt.Errorf("decode GetAsyncSearchStatus response: %w (body=%s)", err, string(raw))
	}
	return resp.SearchJobStatus, nil
}

// GetAsyncSearchResults issues GET /api/search/async/{jobId}. Returns
// the Spring-style page envelope (PagedEntityResults) with entity
// results in Content and pagination metadata under Page.
// Canonical: api/openapi.yaml /search/async/{jobId}.
func (c *Client) GetAsyncSearchResults(t *testing.T, jobID string) (PagedEntityResults, error) {
	t.Helper()
	path := fmt.Sprintf("/api/search/async/%s", jobID)
	raw, err := c.doRaw(t, http.MethodGet, path, "")
	if err != nil {
		return PagedEntityResults{}, err
	}
	var page PagedEntityResults
	if err := json.Unmarshal(raw, &page); err != nil {
		return PagedEntityResults{}, fmt.Errorf("decode GetAsyncSearchResults response: %w (body=%s)", err, string(raw))
	}
	return page, nil
}

// CancelAsyncSearch issues PUT /api/search/async/{jobId}/cancel.
// Returns an error if the request fails (non-2xx). The response body
// is not consumed — only the HTTP status matters.
// Canonical: api/openapi.yaml /search/async/{jobId}/cancel (PUT method
// per Phase 0.1 wire probe, not POST as the plan originally assumed).
func (c *Client) CancelAsyncSearch(t *testing.T, jobID string) error {
	t.Helper()
	path := fmt.Sprintf("/api/search/async/%s/cancel", jobID)
	_, err := c.doRaw(t, http.MethodPut, path, "")
	return err
}

// AwaitAsyncSearchResults submits an async search and polls
// GetAsyncSearchStatus until the job reaches a terminal state or
// timeout elapses. On SUCCESSFUL, fetches and returns the results via
// GetAsyncSearchResults. Terminal failure states (FAILED, CANCELLED,
// NOT_FOUND) return an error. Unknown status values also return an
// error. The polling interval is 100ms.
func (c *Client) AwaitAsyncSearchResults(t *testing.T, modelName string, modelVersion int, condition string, timeout time.Duration) (PagedEntityResults, error) {
	t.Helper()
	jobID, err := c.SubmitAsyncSearch(t, modelName, modelVersion, condition)
	if err != nil {
		return PagedEntityResults{}, fmt.Errorf("submit: %w", err)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		status, err := c.GetAsyncSearchStatus(t, jobID)
		if err != nil {
			return PagedEntityResults{}, fmt.Errorf("status (jobId=%s): %w", jobID, err)
		}
		switch status {
		case "SUCCESSFUL":
			return c.GetAsyncSearchResults(t, jobID)
		case "FAILED", "CANCELLED", "NOT_FOUND":
			return PagedEntityResults{}, fmt.Errorf("async search reached terminal status %s (jobId=%s)", status, jobID)
		case "RUNNING", "":
			// continue polling
		default:
			return PagedEntityResults{}, fmt.Errorf("unexpected async search status %q (jobId=%s)", status, jobID)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return PagedEntityResults{}, fmt.Errorf("timeout (%s) waiting for async search jobId=%s", timeout, jobID)
}

// GetEntityStatsRaw issues GET /api/entity/stats and returns the raw
// status code. The response shape is backend-specific; we only verify
// it returns 200 (not 500).
func (c *Client) GetEntityStatsRaw(t *testing.T) (int, error) {
	t.Helper()
	return c.doJSON(t, http.MethodGet, "/api/entity/stats", nil, nil)
}

// SyncSearchRaw issues POST /api/search/direct/{name}/{version} and
// returns the raw HTTP status code and body without erroring on
// non-2xx. Used for negative-path discover-and-compare.
func (c *Client) SyncSearchRaw(t *testing.T, modelName string, modelVersion int, condition string) (int, []byte, error) {
	t.Helper()
	path := fmt.Sprintf("/api/search/direct/%s/%d", modelName, modelVersion)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, c.baseURL+path, strings.NewReader(condition))
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("transport: %w", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, raw, nil
}

// SubmitAsyncSearchRaw issues POST /api/search/async/{name}/{version}
// and returns the raw HTTP status code and body without erroring on
// non-2xx. Used for negative-path discover-and-compare.
func (c *Client) SubmitAsyncSearchRaw(t *testing.T, modelName string, modelVersion int, condition string) (int, []byte, error) {
	t.Helper()
	path := fmt.Sprintf("/api/search/async/%s/%d", modelName, modelVersion)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, c.baseURL+path, strings.NewReader(condition))
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("transport: %w", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, raw, nil
}

// decodeEntityResultNDJSON parses an application/x-ndjson response body into
// a slice of EntityResult. Each non-empty line is decoded as one object using
// DisallowUnknownFields to catch API drift. Used by SyncSearch, SyncSearchAt,
// SyncSearchSorted, and SyncSearchSortedAt.
func decodeEntityResultNDJSON(raw []byte) ([]EntityResult, error) {
	var results []EntityResult
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if line == "" {
			continue
		}
		var r EntityResult
		dec := json.NewDecoder(strings.NewReader(line))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&r); err != nil {
			return nil, fmt.Errorf("decode NDJSON line: %w", err)
		}
		results = append(results, r)
	}
	return results, nil
}

// SyncSearchSorted issues POST /api/search/direct/{name}/{version}?sort=k1&sort=k2...
// with the given condition JSON and ordered sort keys. Returns entity results in
// the server-applied sort order.  An empty sortKeys slice is equivalent to
// SyncSearch (no sort params — entity_id ascending default).
// Canonical: docs/cyoda/api/openapi-entity-search.yml:471 (searchEntities).
func (c *Client) SyncSearchSorted(t *testing.T, modelName string, modelVersion int, condition string, sortKeys []string) ([]EntityResult, error) {
	t.Helper()
	path := fmt.Sprintf("/api/search/direct/%s/%d", modelName, modelVersion)
	if len(sortKeys) > 0 {
		vals := url.Values{}
		for _, k := range sortKeys {
			vals.Add("sort", k)
		}
		path += "?" + vals.Encode()
	}
	raw, err := c.doRaw(t, http.MethodPost, path, condition)
	if err != nil {
		return nil, err
	}
	return decodeEntityResultNDJSON(raw)
}

// SyncSearchSortedAt is like SyncSearchSorted but also adds a pointInTime
// query parameter so the search operates on the snapshot at the given instant.
func (c *Client) SyncSearchSortedAt(t *testing.T, modelName string, modelVersion int, condition string, sortKeys []string, at time.Time) ([]EntityResult, error) {
	t.Helper()
	vals := url.Values{}
	vals.Set("pointInTime", at.UTC().Format(time.RFC3339Nano))
	for _, k := range sortKeys {
		vals.Add("sort", k)
	}
	path := fmt.Sprintf("/api/search/direct/%s/%d?%s", modelName, modelVersion, vals.Encode())
	raw, err := c.doRaw(t, http.MethodPost, path, condition)
	if err != nil {
		return nil, err
	}
	return decodeEntityResultNDJSON(raw)
}

// SyncSearch issues POST /api/search/direct/{name}/{version} with the
// given condition JSON. Returns the entity results.
// The sync search endpoint returns application/x-ndjson. This method
// reads the response line-by-line (NDJSON).
// Canonical: docs/cyoda/api/openapi-entity-search.yml:471 (searchEntities).
func (c *Client) SyncSearch(t *testing.T, modelName string, modelVersion int, condition string) ([]EntityResult, error) {
	t.Helper()
	path := fmt.Sprintf("/api/search/direct/%s/%d", modelName, modelVersion)
	raw, err := c.doRaw(t, http.MethodPost, path, condition)
	if err != nil {
		return nil, err
	}
	return decodeEntityResultNDJSON(raw)
}

// SyncSearchAt issues POST /api/search/direct/{name}/{version}?pointInTime=<t>
// with the given condition JSON and returns the entity results at the snapshot.
// Mirrors SyncSearch (NDJSON response); the direct-search handler honours the
// pointInTime query param.
func (c *Client) SyncSearchAt(t *testing.T, modelName string, modelVersion int, condition string, at time.Time) ([]EntityResult, error) {
	t.Helper()
	path := fmt.Sprintf("/api/search/direct/%s/%d?pointInTime=%s",
		modelName, modelVersion, at.UTC().Format(time.RFC3339Nano))
	raw, err := c.doRaw(t, http.MethodPost, path, condition)
	if err != nil {
		return nil, err
	}
	return decodeEntityResultNDJSON(raw)
}

// GetAuditEvents issues GET /api/audit/entity/{entityId} with optional
// query parameters for filtering.
// Canonical: docs/cyoda/api/openapi-audit.yml:31 (SearchEntityAuditEvents).
// Returns the canonical EntityAuditEventsResponse with discriminated-union
// AuditEvent items — use AsStateMachine() / AsEntityChange() / AsSystem()
// to decode specific subtypes.
func (c *Client) GetAuditEvents(t *testing.T, entityID uuid.UUID) (EntityAuditEventsResponse, error) {
	t.Helper()
	path := "/api/audit/entity/" + entityID.String()
	var resp EntityAuditEventsResponse
	if _, err := c.doJSON(t, http.MethodGet, path, nil, &resp); err != nil {
		return EntityAuditEventsResponse{}, err
	}
	return resp, nil
}

// SetLogLevel issues POST /api/admin/log-level to change the target node's
// runtime log level (e.g. "debug", "info"). Requires a ROLE_ADMIN token.
// Used by cross-node scenarios that need a peer node to emit its (Debug-level)
// scheduled-fire log lines so a test can positively assert peer execution.
func (c *Client) SetLogLevel(t *testing.T, level string) error {
	t.Helper()
	_, err := c.doJSON(t, http.MethodPost, "/api/admin/log-level", map[string]string{"level": level}, nil)
	return err
}

// DeleteEntitiesByModel issues DELETE /api/entity/{name}/{version},
// removing all entities in that (name, version) namespace for the
// calling tenant. Returns nil on 2xx; the response body's delete-stats
// shape is not returned because tests typically re-verify via
// ListEntitiesByModel rather than parsing stats.
func (c *Client) DeleteEntitiesByModel(t *testing.T, name string, version int) error {
	t.Helper()
	path := fmt.Sprintf("/api/entity/%s/%d", name, version)
	_, err := c.doRaw(t, http.MethodDelete, path, "")
	return err
}

// DeleteEntitiesByModelAt issues DELETE /api/entity/{name}/{version}?pointInTime=<ISO8601>,
// removing only entities whose creation time is at or before pointInTime
// for the calling tenant. Wraps DeleteEntitiesByModel with a temporal
// filter; everything else is identical.
func (c *Client) DeleteEntitiesByModelAt(t *testing.T, name string, version int, pointInTime time.Time) error {
	t.Helper()
	path := fmt.Sprintf("/api/entity/%s/%d?pointInTime=%s", name, version, pointInTime.UTC().Format(time.RFC3339Nano))
	_, err := c.doRaw(t, http.MethodDelete, path, "")
	return err
}

// LockModelRaw issues PUT /api/model/{name}/{version}/lock and returns
// the HTTP status code + raw body without raising on non-2xx. Used by
// negative-path tests that assert on the error body shape via
// e2e/externalapi/errorcontract.Match. Mirrors the *Raw pattern of
// CreateEntityRaw/GetEntityRaw/DeleteEntityRaw.
func (c *Client) LockModelRaw(t *testing.T, name string, version int) (int, []byte, error) {
	t.Helper()
	path := fmt.Sprintf("/api/model/%s/%d/lock", name, version)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, c.baseURL+path, strings.NewReader(""))
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("transport: %w", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, raw, nil
}

// SetChangeLevelRaw issues POST /api/model/{name}/{version}/changeLevel/{level}
// and returns status+body for negative-path assertions via errorcontract.Match.
func (c *Client) SetChangeLevelRaw(t *testing.T, name string, version int, level string) (int, []byte, error) {
	t.Helper()
	path := fmt.Sprintf("/api/model/%s/%d/changeLevel/%s", name, version, level)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, c.baseURL+path, strings.NewReader(""))
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("transport: %w", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, raw, nil
}

// ImportModelRaw issues POST /api/model/import/JSON/SAMPLE_DATA/{name}/{version}
// with the given sample document as the body, and returns status+body for
// negative-path assertions.
func (c *Client) ImportModelRaw(t *testing.T, name string, version int, sample string) (int, []byte, error) {
	t.Helper()
	path := fmt.Sprintf("/api/model/import/JSON/SAMPLE_DATA/%s/%d", name, version)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, c.baseURL+path, strings.NewReader(sample))
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("transport: %w", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, raw, nil
}

// UpdateEntityRaw issues PUT /api/entity/JSON/{entityId}/{transition} with the
// given body and returns status+body for negative-path assertions.
func (c *Client) UpdateEntityRaw(t *testing.T, id uuid.UUID, transition, body string) (int, []byte, error) {
	t.Helper()
	path := fmt.Sprintf("/api/entity/JSON/%s/%s", id.String(), transition)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, c.baseURL+path, strings.NewReader(body))
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("transport: %w", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, raw, nil
}

// UpdateEntityDataWithIfMatchRaw issues PUT /api/entity/JSON/{entityId}
// (loopback — no transition path segment) with an `If-Match` HTTP
// header carrying the supplied ifMatch token. Returns status + body
// for negative-path assertions. ifMatch must be non-empty; pass "" via
// UpdateEntityData for the no-precondition path.
func (c *Client) UpdateEntityDataWithIfMatchRaw(t *testing.T, id uuid.UUID, body, ifMatch string) (int, []byte, error) {
	t.Helper()
	path := "/api/entity/JSON/" + id.String()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, c.baseURL+path, strings.NewReader(body))
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("transport: %w", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, raw, nil
}

// GetEntityBodyRaw issues GET /api/entity/{entityId} and returns the raw
// HTTP status code and response body. Used by tests that need to decode the
// entity JSON with non-default settings (e.g. UseNumber for big-number
// round-trip precision tests).
func (c *Client) GetEntityBodyRaw(t *testing.T, entityID uuid.UUID) (int, []byte, error) {
	t.Helper()
	path := "/api/entity/" + entityID.String()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, c.baseURL+path, strings.NewReader(""))
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("transport: %w", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, raw, nil
}

// GetEntityChangesRaw issues GET /api/entity/{entityId}/changes and returns
// status+body for negative-path assertions.
func (c *Client) GetEntityChangesRaw(t *testing.T, id uuid.UUID) (int, []byte, error) {
	t.Helper()
	path := fmt.Sprintf("/api/entity/%s/changes", id.String())
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, c.baseURL+path, strings.NewReader(""))
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("transport: %w", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, raw, nil
}

// ImportWorkflowRaw issues POST /api/model/{name}/{version}/workflow/import
// with the given workflow JSON as the body and returns status+body for
// negative-path assertions.
func (c *Client) ImportWorkflowRaw(t *testing.T, name string, version int, body string) (int, []byte, error) {
	t.Helper()
	path := fmt.Sprintf("/api/model/%s/%d/workflow/import", name, version)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, c.baseURL+path, strings.NewReader(body))
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("transport: %w", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, raw, nil
}

// QueryGroupedStats issues POST /api/entity/stats/{name}/{version}/query
// with the supplied request body and decodes the response into
// []GroupedStatsBucket. Used by the grouped-stats parity scenarios to
// drive the public surface — capability detection and pushdown vs.
// streaming-tally selection are handled by the service layer.
//
// Canonical handler: internal/domain/entity/grouped_stats_handler.go.
func (c *Client) QueryGroupedStats(t *testing.T, modelName string, modelVersion int, req GroupedStatsRequest) ([]GroupedStatsBucket, error) {
	t.Helper()
	path := fmt.Sprintf("/api/entity/stats/%s/%d/query", modelName, modelVersion)
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal QueryGroupedStats body: %w", err)
	}
	raw, err := c.doRaw(t, http.MethodPost, path, string(body))
	if err != nil {
		return nil, err
	}
	var buckets []GroupedStatsBucket
	if err := json.Unmarshal(raw, &buckets); err != nil {
		return nil, fmt.Errorf("decode QueryGroupedStats response: %w (body=%s)", err, string(raw))
	}
	return buckets, nil
}

// DoJSONBodyRaw issues an HTTP request with an optional JSON-marshalled body
// and returns (status, body, transport-error) without raising on non-2xx.
// This is the general-purpose negative-path companion to doJSON, following
// the *Raw naming convention established by the rest of the parity client.
// OIDC helpers (and any future domain client) call this instead of duplicating
// the pattern. The body argument may be nil for methods that send no payload.
func (c *Client) DoJSONBodyRaw(t *testing.T, method, path string, body any) (int, []byte, error) {
	t.Helper()

	var bodyReader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("marshal request body: %w", err)
		}
		bodyReader = strings.NewReader(string(raw))
	} else {
		bodyReader = strings.NewReader("")
	}

	req, err := http.NewRequestWithContext(t.Context(), method, c.baseURL+path, bodyReader)
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("transport: %w", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, raw, nil
}

// QueryGroupedStatsRaw is the negative-path companion to
// QueryGroupedStats: it returns (status, body, transport-err) without
// raising on non-2xx, used by parity scenarios that assert error
// responses (e.g. 422 GROUP_CARDINALITY_EXCEEDED).
func (c *Client) QueryGroupedStatsRaw(t *testing.T, modelName string, modelVersion int, req GroupedStatsRequest) (int, []byte, error) {
	t.Helper()
	path := fmt.Sprintf("/api/entity/stats/%s/%d/query", modelName, modelVersion)
	body, err := json.Marshal(req)
	if err != nil {
		return 0, nil, fmt.Errorf("marshal QueryGroupedStats body: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(t.Context(), http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return 0, nil, fmt.Errorf("transport: %w", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, raw, nil
}

// PatchEntityRaw issues PATCH /api/entity/{format}/{entityId} (loopback) or
// PATCH /api/entity/{format}/{entityId}/{transition} (named transition) and
// returns the HTTP status, response body, and a transport error only. A
// non-2xx status is returned without raising, so scenarios can assert
// 200/400/404/412/415/428/501 directly.
//
// transition == "" selects the loopback path (no transition segment).
// ifMatch == "" omits the If-Match header entirely (to exercise the 428 path).
// format is the path format segment (normally "JSON"; pass "XML" to exercise
// the 415 unsupported-format path).
// contentType is sent verbatim as the Content-Type header (e.g.
// "application/merge-patch+json").
//
// No 409 retry is performed — concurrency scenarios need to observe 409/412
// directly. The request is issued once via the underlying http.Client, which
// mirrors the pattern of all other *Raw methods in this file.
func (c *Client) PatchEntityRaw(t *testing.T, entityID uuid.UUID, format, transition, contentType, ifMatch, body string) (int, []byte, error) {
	t.Helper()

	var path string
	if transition == "" {
		path = fmt.Sprintf("/api/entity/%s/%s", format, entityID.String())
	} else {
		path = fmt.Sprintf("/api/entity/%s/%s/%s", format, entityID.String(), transition)
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPatch, c.baseURL+path, strings.NewReader(body))
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("transport: %w", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, raw, nil
}

// PatchEntityMerge issues a merge-patch (application/merge-patch+json) PATCH
// against /api/entity/JSON/{entityId}[/{transition}] and returns status+body.
// Delegates to PatchEntityRaw with format="JSON" and the merge-patch
// Content-Type so scenarios don't have to repeat the MIME string.
func (c *Client) PatchEntityMerge(t *testing.T, entityID uuid.UUID, transition, ifMatch, body string) (int, []byte, error) {
	t.Helper()
	return c.PatchEntityRaw(t, entityID, "JSON", transition, "application/merge-patch+json", ifMatch, body)
}

// SetUniqueKeys issues PUT /api/model/{name}/{version}/unique-keys with the
// given raw JSON body (must be a valid SetUniqueKeysRequest). Returns nil on
// 2xx; returns a descriptive error on non-2xx or transport failure.
//
// Call this on an UNLOCKED model, after ImportModel and before LockModel.
// Mirrors the helper pattern of ImportModel and LockModel.
func (c *Client) SetUniqueKeys(t *testing.T, modelName string, modelVersion int, keysJSON string) error {
	t.Helper()
	path := fmt.Sprintf("/api/model/%s/%d/unique-keys", modelName, modelVersion)
	_, err := c.doRaw(t, http.MethodPut, path, keysJSON)
	return err
}

// SetUniqueKeysRaw issues PUT /api/model/{name}/{version}/unique-keys and
// returns (status, body, transportErr) without raising on non-2xx. Used by
// capability-gate detection and negative-path parity scenarios.
//
// Call pattern: if status == 422 && strings.Contains(body, "COMPOSITE_KEY_UNSUPPORTED")
// → t.Skip("backend does not support composite unique keys").
// Mirrors the *Raw helpers LockModelRaw / ImportModelRaw.
func (c *Client) SetUniqueKeysRaw(t *testing.T, modelName string, modelVersion int, body string) (int, []byte, error) {
	t.Helper()
	path := fmt.Sprintf("/api/model/%s/%d/unique-keys", modelName, modelVersion)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, c.baseURL+path, strings.NewReader(body))
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("transport: %w", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, raw, nil
}
