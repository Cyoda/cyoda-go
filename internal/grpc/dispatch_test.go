package grpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

const testTenantID = spi.TenantID("tenant-1")

func setupTestDispatcher(t *testing.T) (*ProcessorDispatcher, *MemberRegistry, string, chan *cepb.CloudEvent) {
	t.Helper()
	registry := NewMemberRegistry()
	sentCh := make(chan *cepb.CloudEvent, 10)
	memberID := registry.Register(testTenantID, []string{"python"}, func(ce *cepb.CloudEvent) error {
		sentCh <- ce
		return nil
	})
	uuids := common.NewTestUUIDGenerator()
	signer, _ := token.NewSigner(make32(t))
	dispatcher := NewProcessorDispatcher(registry, uuids, signer, "node-test", time.Minute)
	return dispatcher, registry, memberID, sentCh
}

func testContext() context.Context {
	return spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID:   "user-1",
		UserName: "test-user",
		Tenant:   spi.Tenant{ID: testTenantID, Name: "Test Tenant"},
	})
}

func testEntity() *spi.Entity {
	return &spi.Entity{
		Meta: spi.EntityMeta{
			ID:       "entity-123",
			TenantID: testTenantID,
		},
		Data: []byte(`{"foo":"bar"}`),
	}
}

// extractRequestID parses the request ID from a sent CloudEvent.
// Returns an error instead of calling t.Fatal so it is safe to call from goroutines.
func extractRequestID(ce *cepb.CloudEvent) (string, error) {
	_, payload, err := ParseCloudEvent(ce)
	if err != nil {
		return "", fmt.Errorf("failed to parse cloud event: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		return "", fmt.Errorf("failed to unmarshal payload: %w", err)
	}
	rid, ok := m["requestId"].(string)
	if !ok {
		return "", fmt.Errorf("requestId not found in payload")
	}
	return rid, nil
}

func TestDispatchProcessor_HappyPath(t *testing.T) {
	dispatcher, registry, memberID, sentCh := setupTestDispatcher(t)
	ctx := testContext()
	entity := testEntity()

	processor := spi.ProcessorDefinition{
		Name: "my-proc",
		Config: spi.ProcessorConfig{
			AttachEntity:         true,
			CalculationNodesTags: "python",
			ResponseTimeoutMs:    5000,
		},
	}

	// Goroutine to respond.
	// Note: uses t.Error (not t.Fatal) because t.Fatal calls runtime.Goexit
	// which has undefined behavior when called from a non-test goroutine.
	go func() {
		ce := <-sentCh
		if ce.Type != EntityProcessorCalculationRequest {
			t.Errorf("expected event type %s, got %s", EntityProcessorCalculationRequest, ce.Type)
		}
		reqID, err := extractRequestID(ce)
		if err != nil {
			t.Errorf("extractRequestID: %v", err)
			return
		}

		// Verify payload is attached with data and meta.
		_, payload, _ := ParseCloudEvent(ce)
		var m map[string]any
		json.Unmarshal(payload, &m)
		payloadObj, ok := m["payload"].(map[string]any)
		if !ok {
			t.Error("expected payload to be present when AttachEntity=true")
			return
		}
		if _, ok := payloadObj["data"]; !ok {
			t.Error("expected payload.data to be present")
		}
		meta, ok := payloadObj["meta"].(map[string]any)
		if !ok {
			t.Error("expected payload.meta to be present (EntityMetadata)")
			return
		}
		if meta["id"] != entity.Meta.ID {
			t.Errorf("expected meta.id=%s, got %v", entity.Meta.ID, meta["id"])
		}
		if _, ok := meta["state"]; !ok {
			t.Error("expected meta.state to be present")
		}

		// Verify payload matches the generated typed struct schema.
		var typedReq events.EntityProcessorCalculationRequestJson
		if err := json.Unmarshal(payload, &typedReq); err != nil {
			t.Errorf("sent processor request doesn't match schema: %v", err)
			return
		}
		if typedReq.ProcessorName != "my-proc" {
			t.Errorf("expected processorName my-proc, got %s", typedReq.ProcessorName)
		}

		// Verify auth context extension attributes on the CloudEvent.
		if ce.Attributes == nil {
			t.Error("expected CloudEvent attributes (auth context)")
			return
		}
		authType, ok := ce.Attributes["authtype"]
		if !ok {
			t.Error("expected authtype attribute")
			return
		}
		if authType.GetCeString() != "user" {
			t.Errorf("expected authtype=user, got %s", authType.GetCeString())
		}
		authId, ok := ce.Attributes["authid"]
		if !ok {
			t.Error("expected authid attribute")
			return
		}
		if authId.GetCeString() != "user-1" {
			t.Errorf("expected authid=user-1, got %s", authId.GetCeString())
		}

		member := registry.Get(memberID)
		member.CompleteRequest(reqID, &ProcessingResponse{
			Payload: json.RawMessage(`{"data":{"foo":"updated"}}`),
			Success: true,
		})
	}()

	result, err := dispatcher.DispatchProcessor(ctx, entity, processor, "wf1", "t1", "tx-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var data map[string]any
	if err := json.Unmarshal(result.Data, &data); err != nil {
		t.Fatalf("failed to unmarshal result data: %v", err)
	}
	if data["foo"] != "updated" {
		t.Errorf("expected foo=updated, got %v", data["foo"])
	}
	if result.Meta.ID != entity.Meta.ID {
		t.Error("meta should be preserved")
	}
}

func TestDispatchProcessor_NoMember(t *testing.T) {
	registry := NewMemberRegistry()
	uuids := common.NewTestUUIDGenerator()
	signer, _ := token.NewSigner(make32(t))
	dispatcher := NewProcessorDispatcher(registry, uuids, signer, "node-test", time.Minute)
	ctx := testContext()
	entity := testEntity()

	processor := spi.ProcessorDefinition{
		Name: "my-proc",
		Config: spi.ProcessorConfig{
			CalculationNodesTags: "java",
		},
	}

	_, err := dispatcher.DispatchProcessor(ctx, entity, processor, "wf1", "t1", "tx-1")
	if err == nil {
		t.Fatal("expected error for missing member")
	}
	if !errors.Is(err, ErrNoMatchingMember) {
		t.Errorf("expected ErrNoMatchingMember, got: %s", err)
	}
}

func TestDispatchProcessor_Timeout(t *testing.T) {
	dispatcher, _, _, _ := setupTestDispatcher(t)
	ctx := testContext()
	entity := testEntity()

	processor := spi.ProcessorDefinition{
		Name: "my-proc",
		Config: spi.ProcessorConfig{
			CalculationNodesTags: "python",
			ResponseTimeoutMs:    1, // 1ms timeout
		},
	}

	_, err := dispatcher.DispatchProcessor(ctx, entity, processor, "wf1", "t1", "tx-1")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if got := err.Error(); got != "processor dispatch timed out after 1ms" {
		t.Errorf("unexpected error: %s", got)
	}
}

func TestDispatchProcessor_NoAttachEntity(t *testing.T) {
	dispatcher, registry, memberID, sentCh := setupTestDispatcher(t)
	ctx := testContext()
	entity := testEntity()

	processor := spi.ProcessorDefinition{
		Name: "my-proc",
		Config: spi.ProcessorConfig{
			AttachEntity:         false,
			CalculationNodesTags: "python",
			ResponseTimeoutMs:    5000,
		},
	}

	go func() {
		ce := <-sentCh
		reqID, err := extractRequestID(ce)
		if err != nil {
			t.Errorf("extractRequestID: %v", err)
			return
		}

		// Verify payload is NOT attached.
		_, payload, _ := ParseCloudEvent(ce)
		var m map[string]any
		json.Unmarshal(payload, &m)
		if _, ok := m["payload"]; ok {
			t.Error("expected no payload when AttachEntity=false")
		}

		member := registry.Get(memberID)
		member.CompleteRequest(reqID, &ProcessingResponse{
			Success: true,
		})
	}()

	result, err := dispatcher.DispatchProcessor(ctx, entity, processor, "wf1", "t1", "tx-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// With no payload in response, original entity is returned.
	if result != entity {
		t.Error("expected original entity when response has no payload")
	}
}

func TestDispatchCriteria_MatchesTrue(t *testing.T) {
	dispatcher, registry, memberID, sentCh := setupTestDispatcher(t)
	ctx := testContext()
	entity := testEntity()

	criterion := json.RawMessage(`{
		"name": "my-criteria",
		"config": {
			"calculationNodesTags": "python",
			"attachEntity": true,
			"responseTimeoutMs": 5000
		}
	}`)

	go func() {
		ce := <-sentCh
		if ce.Type != EntityCriteriaCalculationRequest {
			t.Errorf("expected event type %s, got %s", EntityCriteriaCalculationRequest, ce.Type)
		}
		reqID, err := extractRequestID(ce)
		if err != nil {
			t.Errorf("extractRequestID: %v", err)
			return
		}

		matchesTrue := true
		member := registry.Get(memberID)
		member.CompleteRequest(reqID, &ProcessingResponse{
			Success: true,
			Matches: &matchesTrue,
		})
	}()

	result, _, err := dispatcher.DispatchCriteria(ctx, entity, criterion, "transition", "wf1", "t1", "proc1", "tx-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result {
		t.Error("expected matches=true")
	}
}

func TestDispatchCriteria_MatchesFalse(t *testing.T) {
	dispatcher, registry, memberID, sentCh := setupTestDispatcher(t)
	ctx := testContext()
	entity := testEntity()

	criterion := json.RawMessage(`{
		"name": "my-criteria",
		"config": {
			"calculationNodesTags": "python",
			"responseTimeoutMs": 5000
		}
	}`)

	go func() {
		ce := <-sentCh
		reqID, err := extractRequestID(ce)
		if err != nil {
			t.Errorf("extractRequestID: %v", err)
			return
		}

		matchesFalse := false
		member := registry.Get(memberID)
		member.CompleteRequest(reqID, &ProcessingResponse{
			Success: true,
			Matches: &matchesFalse,
			Reason:  "amount 5 below minimum 10",
		})
	}()

	result, reason, err := dispatcher.DispatchCriteria(ctx, entity, criterion, "transition", "wf1", "t1", "", "tx-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result {
		t.Error("expected matches=false")
	}
	if reason != "amount 5 below minimum 10" {
		t.Errorf("expected reason returned, got %q", reason)
	}
}

// extractParameters returns the "parameters" field of the request payload as a
// raw JSON value (or nil if absent). Cloud's contract for ProcessorConfig.context
// is pass-as-string into the request's parameters node — see
// docs/WORKFLOW_IMPORT_EXPORT_AUDIT.md §M1 and api/grpc/events/types.go:822, 2403.
func extractParameters(ce *cepb.CloudEvent) (json.RawMessage, bool, error) {
	_, payload, err := ParseCloudEvent(ce)
	if err != nil {
		return nil, false, fmt.Errorf("failed to parse cloud event: %w", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(payload, &m); err != nil {
		return nil, false, fmt.Errorf("failed to unmarshal payload: %w", err)
	}
	raw, ok := m["parameters"]
	if !ok {
		return nil, false, nil
	}
	return raw, true, nil
}

// TestDispatchProcessor_ContextSurfacesAsParametersString verifies that
// processor.Config.Context, when non-empty, is placed verbatim into the
// request's `parameters` JSON node so a single external processor
// implementation can serve multiple workflow roles distinguished by the
// context value.
func TestDispatchProcessor_ContextSurfacesAsParametersString(t *testing.T) {
	dispatcher, registry, memberID, sentCh := setupTestDispatcher(t)
	ctx := testContext()
	entity := testEntity()

	const ctxValue = `{"role":"premium-approver"}`
	processor := spi.ProcessorDefinition{
		Name: "my-proc",
		Config: spi.ProcessorConfig{
			AttachEntity:         false,
			CalculationNodesTags: "python",
			ResponseTimeoutMs:    5000,
			Context:              ctxValue,
		},
	}

	go func() {
		ce := <-sentCh
		reqID, err := extractRequestID(ce)
		if err != nil {
			t.Errorf("extractRequestID: %v", err)
			return
		}

		raw, present, err := extractParameters(ce)
		if err != nil {
			t.Errorf("extractParameters: %v", err)
			return
		}
		if !present {
			t.Error("expected parameters field to be present when Context is set")
		} else {
			var got string
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Errorf("expected parameters to be a JSON string, got %s: %v", raw, err)
			} else if got != ctxValue {
				t.Errorf("expected parameters=%q, got %q", ctxValue, got)
			}
		}

		// Also assert via the generated typed schema decodes cleanly with the
		// string-shaped parameters (Parameters is interface{}).
		_, payload, _ := ParseCloudEvent(ce)
		var typed events.EntityProcessorCalculationRequestJson
		if err := json.Unmarshal(payload, &typed); err != nil {
			t.Errorf("sent processor request does not match schema: %v", err)
		} else if s, ok := typed.Parameters.(string); !ok || s != ctxValue {
			t.Errorf("expected typed.Parameters as string %q, got %T %v", ctxValue, typed.Parameters, typed.Parameters)
		}

		member := registry.Get(memberID)
		member.CompleteRequest(reqID, &ProcessingResponse{Success: true})
	}()

	if _, err := dispatcher.DispatchProcessor(ctx, entity, processor, "wf1", "t1", "tx-1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestDispatchProcessor_EmptyContextOmitsParameters verifies that when
// Context is the zero value the dispatcher omits parameters entirely (no
// `"parameters":null` and no empty string) so existing requests on the wire
// are unchanged. The `parameters` field carries `omitempty` for that reason.
func TestDispatchProcessor_EmptyContextOmitsParameters(t *testing.T) {
	dispatcher, registry, memberID, sentCh := setupTestDispatcher(t)
	ctx := testContext()
	entity := testEntity()

	processor := spi.ProcessorDefinition{
		Name: "my-proc",
		Config: spi.ProcessorConfig{
			CalculationNodesTags: "python",
			ResponseTimeoutMs:    5000,
			// Context deliberately empty.
		},
	}

	go func() {
		ce := <-sentCh
		reqID, err := extractRequestID(ce)
		if err != nil {
			t.Errorf("extractRequestID: %v", err)
			return
		}
		if _, present, err := extractParameters(ce); err != nil {
			t.Errorf("extractParameters: %v", err)
		} else if present {
			t.Error("expected parameters field to be omitted when Context is empty")
		}
		member := registry.Get(memberID)
		member.CompleteRequest(reqID, &ProcessingResponse{Success: true})
	}()

	if _, err := dispatcher.DispatchProcessor(ctx, entity, processor, "wf1", "t1", "tx-1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestDispatchCriteria_ContextSurfacesAsParametersString verifies that
// FunctionCondition.config.context follows the same pass-through-string rule
// as the processor path. The criterion JSON shape carries the function
// wrapper emitted by the engine's evaluateCriterion routing.
func TestDispatchCriteria_ContextSurfacesAsParametersString(t *testing.T) {
	dispatcher, registry, memberID, sentCh := setupTestDispatcher(t)
	ctx := testContext()
	entity := testEntity()

	const ctxValue = `gold-tier`
	criterion := json.RawMessage(`{
		"type": "function",
		"function": {
			"name": "my-criteria",
			"config": {
				"calculationNodesTags": "python",
				"responseTimeoutMs": 5000,
				"context": "` + ctxValue + `"
			}
		}
	}`)

	go func() {
		ce := <-sentCh
		reqID, err := extractRequestID(ce)
		if err != nil {
			t.Errorf("extractRequestID: %v", err)
			return
		}

		raw, present, err := extractParameters(ce)
		if err != nil {
			t.Errorf("extractParameters: %v", err)
			return
		}
		if !present {
			t.Error("expected parameters field to be present when criterion context is set")
		} else {
			var got string
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Errorf("expected parameters to be a JSON string, got %s: %v", raw, err)
			} else if got != ctxValue {
				t.Errorf("expected parameters=%q, got %q", ctxValue, got)
			}
		}

		matchesTrue := true
		member := registry.Get(memberID)
		member.CompleteRequest(reqID, &ProcessingResponse{Success: true, Matches: &matchesTrue})
	}()

	if _, _, err := dispatcher.DispatchCriteria(ctx, entity, criterion, "transition", "wf1", "t1", "proc1", "tx-1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestDispatchCriteria_EmptyContextOmitsParameters verifies that an absent
// or empty criterion context omits the request's parameters field — mirror
// of TestDispatchProcessor_EmptyContextOmitsParameters for the criteria path.
func TestDispatchCriteria_EmptyContextOmitsParameters(t *testing.T) {
	dispatcher, registry, memberID, sentCh := setupTestDispatcher(t)
	ctx := testContext()
	entity := testEntity()

	criterion := json.RawMessage(`{
		"type": "function",
		"function": {
			"name": "my-criteria",
			"config": {
				"calculationNodesTags": "python",
				"responseTimeoutMs": 5000
			}
		}
	}`)

	go func() {
		ce := <-sentCh
		reqID, err := extractRequestID(ce)
		if err != nil {
			t.Errorf("extractRequestID: %v", err)
			return
		}
		if _, present, err := extractParameters(ce); err != nil {
			t.Errorf("extractParameters: %v", err)
		} else if present {
			t.Error("expected parameters field to be omitted when criterion context is empty")
		}
		matchesTrue := true
		member := registry.Get(memberID)
		member.CompleteRequest(reqID, &ProcessingResponse{Success: true, Matches: &matchesTrue})
	}()

	if _, _, err := dispatcher.DispatchCriteria(ctx, entity, criterion, "transition", "wf1", "t1", "proc1", "tx-1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestDispatchProcessor_AnnotationsNotSentToMember verifies that
// ProcessorDefinition.Annotations — client-owned renderer metadata — never
// reaches the compute member on the wire. dispatch.go builds a field-selected
// EntityProcessorCalculationRequestJson rather than marshalling the whole
// spi.ProcessorDefinition, so there is no annotations field to leak; this
// test pins that behavior and would fail if the request builder ever grew
// one.
func TestDispatchProcessor_AnnotationsNotSentToMember(t *testing.T) {
	dispatcher, registry, memberID, sentCh := setupTestDispatcher(t)
	ctx := testContext()
	entity := testEntity()

	processor := spi.ProcessorDefinition{
		Name:        "my-proc",
		Type:        "externalized",
		Annotations: json.RawMessage(`{"displayName":"SECRET-LABEL"}`),
		Config: spi.ProcessorConfig{
			AttachEntity:         true,
			CalculationNodesTags: "python",
			ResponseTimeoutMs:    5000,
		},
	}

	// Goroutine to respond.
	// Note: uses t.Error (not t.Fatal) because t.Fatal calls runtime.Goexit
	// which has undefined behavior when called from a non-test goroutine.
	go func() {
		ce := <-sentCh
		_, payload, err := ParseCloudEvent(ce)
		if err != nil {
			t.Errorf("ParseCloudEvent: %v", err)
			return
		}
		if strings.Contains(string(payload), "SECRET-LABEL") || strings.Contains(string(payload), "annotations") {
			t.Errorf("processor annotations leaked to compute member: %s", payload)
		}

		reqID, err := extractRequestID(ce)
		if err != nil {
			t.Errorf("extractRequestID: %v", err)
			return
		}
		member := registry.Get(memberID)
		member.CompleteRequest(reqID, &ProcessingResponse{Success: true})
	}()

	// Dispatch should succeed (no error path) despite the processor carrying annotations.
	_, err := dispatcher.DispatchProcessor(ctx, entity, processor, "wf", "t", "tx-1")
	if err != nil {
		t.Fatalf("DispatchProcessor: %v", err)
	}
}
