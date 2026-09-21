package grpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

const testTenantID = spi.TenantID("tenant-1")

// make32 returns a 32-byte secret for token signing in tests.
func make32(t *testing.T) []byte {
	t.Helper()
	b := make([]byte, 32)
	for i := range b {
		b[i] = byte(i + 1)
	}
	return b
}

func setupTestDispatcher(t *testing.T) (*ProcessorDispatcher, *MemberRegistry, string, chan *cepb.CloudEvent) {
	t.Helper()
	registry := NewMemberRegistry()
	sentCh := make(chan *cepb.CloudEvent, 10)
	member := registry.Register("member-test", testTenantID, []string{"python"}, func(ce *cepb.CloudEvent) error {
		sentCh <- ce
		return nil
	}, nil)
	dispatcher := newTestDispatcher(t, registry)
	return dispatcher, registry, member.ID, sentCh
}

// newTestDispatcher builds a dispatcher over registry the way app.go does, with
// node id "node-test".
func newTestDispatcher(t *testing.T, registry *MemberRegistry) *ProcessorDispatcher {
	t.Helper()
	return newTestDispatcherWith(t, registry, NewRoundRobinSelector(registry))
}

// newTestDispatcherWith is newTestDispatcher with a selector of the test's own.
func newTestDispatcherWith(t *testing.T, registry *MemberRegistry, selector MemberSelector) *ProcessorDispatcher {
	t.Helper()
	signer, err := token.NewSigner(make32(t))
	if err != nil {
		t.Fatalf("token.NewSigner: %v", err)
	}
	return NewProcessorDispatcher(registry, selector, signer, "node-test", 30*time.Second, 60*time.Second, 3*time.Second)
}

// oneTry is the local procedure with one try, armed the way an owner arms a
// callout: what these tests pin is what one try sends and how its answer is
// read.
func oneTry(d *ProcessorDispatcher, ctx context.Context, call Callout) (CalloutResult, error) {
	limit, failure := d.ResolveAnswerLimit(call.ResponseTimeoutMs)
	if failure != nil {
		return CalloutResult{}, failure
	}
	call.RequestID = uuid.NewString()
	call.AnswerLimit = limit
	call.OwnerNodeID = "node-test"
	call.Number = &countingNumberer{}
	res := d.RunLocal(ctx, call, 1)
	return res.Result, res.Err()
}

func dispatchProcessor(d *ProcessorDispatcher, ctx context.Context, entity *spi.Entity, processor spi.ProcessorDefinition, workflowName, transitionName, txID string) (*spi.Entity, error) {
	res, err := oneTry(d, ctx, NewProcessorCallout(spi.MustGetUserContext(ctx).Tenant.ID, entity, processor, workflowName, transitionName, txID))
	if err != nil {
		return nil, err
	}
	return res.Entity, nil
}

func dispatchCriteria(d *ProcessorDispatcher, ctx context.Context, entity *spi.Entity, criterion json.RawMessage, target, workflowName, transitionName, processorName, txID string) (bool, string, error) {
	call, failure := NewCriteriaCallout(spi.MustGetUserContext(ctx).Tenant.ID, entity, criterion, target, workflowName, transitionName, processorName, txID)
	if failure != nil {
		return false, "", failure
	}
	res, err := oneTry(d, ctx, call)
	if err != nil {
		return false, "", err
	}
	return res.Matches, res.Reason, nil
}

func dispatchFunction(d *ProcessorDispatcher, ctx context.Context, entity *spi.Entity, fn spi.ScheduleFunction, workflowName, transitionName, txID string) (contract.FunctionResult, error) {
	res, err := oneTry(d, ctx, NewFunctionCallout(spi.MustGetUserContext(ctx).Tenant.ID, entity, fn, workflowName, transitionName, txID))
	if err != nil {
		return contract.FunctionResult{}, err
	}
	return res.Function, nil
}

func testProcessor(tags string, responseTimeoutMs int64) spi.ProcessorDefinition {
	return spi.ProcessorDefinition{
		Name:   "my-proc",
		Config: spi.ProcessorConfig{CalculationNodesTags: tags, ResponseTimeoutMs: responseTimeoutMs},
	}
}

// tryOnce makes one try against member with a raw payload carrying only the
// request id, and folds the three outcomes of a try into (response, error) —
// the shape the tests of the single try were written against. timeoutMs is the
// answer limit.
func tryOnce(d *ProcessorDispatcher, ctx context.Context, member *Member, requestID, txID string, timeoutMs int64) (*ProcessingResponse, error) {
	var got *ProcessingResponse
	call := Callout{
		Kind:        ProcessorCallout,
		Name:        "my-proc",
		TenantID:    member.TenantID,
		TxID:        txID,
		RequestID:   requestID,
		AnswerLimit: time.Duration(timeoutMs) * time.Millisecond,
		OwnerNodeID: "node-test",
		eventType:   EntityProcessorCalculationRequest,
		buildRequest: func(id string) any {
			return map[string]any{"requestId": id}
		},
		mapResponse: func(resp *ProcessingResponse) (CalloutResult, error) {
			got = resp
			return CalloutResult{}, nil
		},
	}
	pass, err := d.mintPass(call, 1, 0)
	if err != nil {
		return nil, err
	}
	_, failure, ctxErr := d.dispatchCalloutToMember(ctx, member, call, pass)
	switch {
	case ctxErr != nil:
		return nil, ctxErr
	case failure != nil:
		return nil, failure
	}
	return got, nil
}

func testContext() context.Context {
	return spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID:   "user-1",
		UserName: "test-user",
		Kind:     spi.PrincipalUser,
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

	result, err := dispatchProcessor(dispatcher, ctx, entity, processor, "wf1", "t1", "tx-1")
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
	dispatcher := newTestDispatcher(t, registry)
	ctx := testContext()
	entity := testEntity()

	processor := spi.ProcessorDefinition{
		Name: "my-proc",
		Config: spi.ProcessorConfig{
			CalculationNodesTags: "java",
		},
	}

	_, err := dispatchProcessor(dispatcher, ctx, entity, processor, "wf1", "t1", "tx-1")
	if err == nil {
		t.Fatal("expected error for missing member")
	}
	if !errors.Is(err, contract.ErrNoMatchingMember) {
		t.Errorf("expected contract.ErrNoMatchingMember, got: %s", err)
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
			// Long enough that the enqueue always succeeds and the response
			// deadline is what fires: a 1ms budget can expire before the
			// request reaches the writer, which reports "member not draining"
			// instead and loses the race with the assertion below.
			ResponseTimeoutMs: 50,
		},
	}

	_, err := dispatchProcessor(dispatcher, ctx, entity, processor, "wf1", "t1", "tx-1")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *common.AppError, got %T: %v", err, err)
	}
	if appErr.Code != common.ErrCodeDispatchTimeout {
		t.Errorf("expected code %s, got %s", common.ErrCodeDispatchTimeout, appErr.Code)
	}
	if appErr.Status != 503 {
		t.Errorf("expected status 503, got %d", appErr.Status)
	}
	if !appErr.Retryable {
		t.Error("expected timeout error to be retryable")
	}
	if got := appErr.Error(); got != "DISPATCH_TIMEOUT: processor dispatch timed out after 50ms: no response" {
		t.Errorf("unexpected message: %s", got)
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

	result, err := dispatchProcessor(dispatcher, ctx, entity, processor, "wf1", "t1", "tx-1")
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

	result, _, err := dispatchCriteria(dispatcher, ctx, entity, criterion, "transition", "wf1", "t1", "proc1", "tx-1")
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

	result, reason, err := dispatchCriteria(dispatcher, ctx, entity, criterion, "transition", "wf1", "t1", "", "tx-1")
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

	if _, err := dispatchProcessor(dispatcher, ctx, entity, processor, "wf1", "t1", "tx-1"); err != nil {
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

	if _, err := dispatchProcessor(dispatcher, ctx, entity, processor, "wf1", "t1", "tx-1"); err != nil {
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

	if _, _, err := dispatchCriteria(dispatcher, ctx, entity, criterion, "transition", "wf1", "t1", "proc1", "tx-1"); err != nil {
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

	if _, _, err := dispatchCriteria(dispatcher, ctx, entity, criterion, "transition", "wf1", "t1", "proc1", "tx-1"); err != nil {
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
	_, err := dispatchProcessor(dispatcher, ctx, entity, processor, "wf", "t", "tx-1")
	if err != nil {
		t.Fatalf("DispatchProcessor: %v", err)
	}
}

// TestDispatchCalloutToMember_SuccessAndTimeout drives the shared transport
// primitive directly (rather than through DispatchProcessor/DispatchCriteria)
// to pin its two outcomes: a tracked response is returned on success, and a
// non-nil error is returned when nobody answers before the timeout.
func TestDispatchCalloutToMember_SuccessAndTimeout(t *testing.T) {
	dispatcher, registry, memberID, sentCh := setupTestDispatcher(t)
	ctx := testContext()
	member := registry.Get(memberID)

	// Success: answer the tracked request.
	go func() {
		ce := <-sentCh
		if ce.Type != EntityProcessorCalculationRequest {
			t.Errorf("expected event type %s, got %s", EntityProcessorCalculationRequest, ce.Type)
			return
		}
		reqID, err := extractRequestID(ce)
		if err != nil {
			t.Errorf("extractRequestID: %v", err)
			return
		}
		if reqID != "req-success" {
			t.Errorf("expected requestId=req-success, got %s", reqID)
		}
		member.CompleteRequest(reqID, &ProcessingResponse{Success: true, Payload: json.RawMessage(`{"data":{}}`)})
	}()

	resp, err := tryOnce(dispatcher, ctx, member, "req-success", "tx-1", 5000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil || !resp.Success {
		t.Fatalf("expected successful response, got %v", resp)
	}

	// Timeout: nobody answers the second request.
	_, err = tryOnce(dispatcher, ctx, member, "req-timeout", "tx-1", 20)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *common.AppError, got %T: %v", err, err)
	}
	if appErr.Code != common.ErrCodeDispatchTimeout {
		t.Errorf("expected code %s, got %s", common.ErrCodeDispatchTimeout, appErr.Code)
	}
	if appErr.Status != 503 {
		t.Errorf("expected status 503, got %d", appErr.Status)
	}
	if !appErr.Retryable {
		t.Error("expected timeout error to be retryable")
	}
}

// TestDispatchCalloutToMember_MemberDisconnects guards that a member
// disconnecting mid-request (evicted, e.g. on stream drop/Unregister)
// surfaces a distinguishable 503 COMPUTE_MEMBER_DISCONNECTED to the waiting
// dispatch, not a generic failure that maps to 400.
func TestDispatchCalloutToMember_MemberDisconnects(t *testing.T) {
	dispatcher, registry, memberID, sentCh := setupTestDispatcher(t)
	ctx := testContext()
	member := registry.Get(memberID)

	// Simulate the stream dropping mid-flight: once the request is sent
	// (and therefore tracked), evict the member as Unregister does on
	// disconnect, instead of ever completing the request normally.
	go func() {
		<-sentCh
		member.Evict(errors.New("compute member disconnected"))
	}()

	_, err := tryOnce(dispatcher, ctx, member, "req-disconnect", "tx-1", 5000)
	if err == nil {
		t.Fatal("expected error for member disconnect")
	}
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *common.AppError, got %T: %v", err, err)
	}
	if appErr.Code != common.ErrCodeComputeMemberDisconnected {
		t.Errorf("expected code %s, got %s", common.ErrCodeComputeMemberDisconnected, appErr.Code)
	}
	if appErr.Status != 503 {
		t.Errorf("expected status 503, got %d", appErr.Status)
	}
	if !appErr.Retryable {
		t.Error("expected disconnect error to be retryable")
	}
}

// TestDispatchCalloutToMember_NilUserContext guards the fail-loud rule end
// to end: a dispatch path with no UserContext on ctx must fail the dispatch
// and never send the callout to the member.
func TestDispatchCalloutToMember_NilUserContext(t *testing.T) {
	dispatcher, registry, memberID, sentCh := setupTestDispatcher(t)
	member := registry.Get(memberID)

	_, err := tryOnce(dispatcher, context.Background(), member, "req-no-uc", "tx-1", 5000)
	if err == nil {
		t.Fatal("expected error for missing user context")
	}
	select {
	case ce := <-sentCh:
		t.Fatalf("expected no callout to be sent, got %v", ce)
	default:
	}
}

// TestDispatchCalloutToMember_UnsetKind guards that an unset principal Kind
// fails the dispatch and never sends the callout.
func TestDispatchCalloutToMember_UnsetKind(t *testing.T) {
	dispatcher, registry, memberID, sentCh := setupTestDispatcher(t)
	member := registry.Get(memberID)
	ctx := spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID: "user-1",
		Tenant: spi.Tenant{ID: testTenantID, Name: "Test Tenant"},
	})

	_, err := tryOnce(dispatcher, ctx, member, "req-unset-kind", "tx-1", 5000)
	if err == nil {
		t.Fatal("expected error for unset principal kind")
	}
	select {
	case ce := <-sentCh:
		t.Fatalf("expected no callout to be sent, got %v", ce)
	default:
	}
}

// TestDispatchCalloutToMember_InvalidKind guards the pinned wire contract
// (authtype in {user,service,system}): a Kind outside that set — e.g. from a
// misconfigured mock — must fail the dispatch rather than emit a bogus
// authtype onto the wire.
func TestDispatchCalloutToMember_InvalidKind(t *testing.T) {
	dispatcher, registry, memberID, sentCh := setupTestDispatcher(t)
	member := registry.Get(memberID)
	ctx := spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID: "user-1",
		Kind:   spi.PrincipalKind("bogus"),
		Tenant: spi.Tenant{ID: testTenantID, Name: "Test Tenant"},
	})

	_, err := tryOnce(dispatcher, ctx, member, "req-invalid-kind", "tx-1", 5000)
	if err == nil {
		t.Fatal("expected error for invalid principal kind")
	}
	select {
	case ce := <-sentCh:
		t.Fatalf("expected no callout to be sent, got %v", ce)
	default:
	}
}

// TestDispatchCalloutToMember_KindDrivenAuthType guards that authtype flows
// end to end from the explicit principal Kind for service and system
// principals (not just the default user case covered by
// TestDispatchProcessor_HappyPath), and that a user-kind principal carrying
// ROLE_M2M still emits authtype=user (role-sniffing regression guard).
func TestDispatchCalloutToMember_KindDrivenAuthType(t *testing.T) {
	tests := []struct {
		name         string
		kind         spi.PrincipalKind
		roles        []string
		wantAuthType string
	}{
		{"service kind", spi.PrincipalService, nil, "service"},
		{"system kind", spi.PrincipalSystem, nil, "system"},
		{"user kind with ROLE_M2M regression", spi.PrincipalUser, []string{"ROLE_M2M"}, "user"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dispatcher, registry, memberID, sentCh := setupTestDispatcher(t)
			member := registry.Get(memberID)
			ctx := spi.WithUserContext(context.Background(), &spi.UserContext{
				UserID: "principal-1",
				Kind:   tt.kind,
				Roles:  tt.roles,
				Tenant: spi.Tenant{ID: testTenantID, Name: "Test Tenant"},
			})

			go func() {
				ce := <-sentCh
				authType, ok := ce.Attributes["authtype"]
				if !ok {
					t.Error("expected authtype attribute")
					return
				}
				if got := authType.GetCeString(); got != tt.wantAuthType {
					t.Errorf("expected authtype=%s, got %s", tt.wantAuthType, got)
				}
				reqID, err := extractRequestID(ce)
				if err != nil {
					t.Errorf("extractRequestID: %v", err)
					return
				}
				member.CompleteRequest(reqID, &ProcessingResponse{Success: true, Payload: json.RawMessage(`{"data":{}}`)})
			}()

			resp, err := tryOnce(dispatcher, ctx, member, "req-kind-driven", "tx-1", 5000)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if resp == nil || !resp.Success {
				t.Fatalf("expected successful response, got %v", resp)
			}
		})
	}
}

// TestDispatchProcessor_WarningPropagatesName guards that response warnings
// surface to the client keyed by the processor NAME, not the opaque request
// ID. These strings appear in the gRPC warnings array and HTTP body
// (.claude/rules/error-handling.md); a refactor must not swap the name for a
// UUID.
func TestDispatchProcessor_WarningPropagatesName(t *testing.T) {
	dispatcher, registry, memberID, sentCh := setupTestDispatcher(t)
	ctx := common.WithDiagnostics(testContext())
	entity := testEntity()

	processor := spi.ProcessorDefinition{
		Name: "validate-order",
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
		member := registry.Get(memberID)
		member.CompleteRequest(reqID, &ProcessingResponse{
			Success:  true,
			Warnings: []string{"stock low"},
		})
	}()

	if _, err := dispatchProcessor(dispatcher, ctx, entity, processor, "wf1", "t1", "tx-1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	warnings := common.GetDiagnostics(ctx).GetWarnings()
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d: %v", len(warnings), warnings)
	}
	if warnings[0] != "processor validate-order: stock low" {
		t.Errorf("warning must carry processor name, got %q", warnings[0])
	}
	if strings.Contains(warnings[0], "-") && !strings.Contains(warnings[0], "validate-order") {
		t.Errorf("warning appears to leak requestId instead of name: %q", warnings[0])
	}
}

// TestDispatchCriteria_FailurePropagatesName guards the error path: a failed
// criteria response surfaces to the client keyed by the criteria NAME.
func TestDispatchCriteria_FailurePropagatesName(t *testing.T) {
	dispatcher, registry, memberID, sentCh := setupTestDispatcher(t)
	ctx := common.WithDiagnostics(testContext())
	entity := testEntity()

	// Production criterion shape: name/config nested under "function".
	criterion := json.RawMessage(`{
		"type": "function",
		"function": {
			"name": "amount-check",
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
		member := registry.Get(memberID)
		member.CompleteRequest(reqID, &ProcessingResponse{
			Success: false,
			Error:   "boom",
		})
	}()

	if _, _, err := dispatchCriteria(dispatcher, ctx, entity, criterion, "transition", "wf1", "t1", "", "tx-1"); err == nil {
		t.Fatal("expected dispatch failure error")
	}

	errs := common.GetDiagnostics(ctx).GetErrors()
	if len(errs) != 1 {
		t.Fatalf("expected 1 error diagnostic, got %d: %v", len(errs), errs)
	}
	if errs[0] != "criteria amount-check: boom" {
		t.Errorf("error diagnostic must carry criteria name, got %q", errs[0])
	}
}

func TestDispatchFunction_HappyPath(t *testing.T) {
	dispatcher, registry, memberID, sentCh := setupTestDispatcher(t)
	ctx := testContext()
	entity := testEntity()

	fn := spi.ScheduleFunction{
		Name:                 "my-schedule-fn",
		ResultKind:           "Schedule",
		CalculationNodesTags: "python",
		AttachEntity:         true,
		ResponseTimeoutMs:    5000,
	}

	go func() {
		ce := <-sentCh
		if ce.Type != EntityFunctionCalculationRequest {
			t.Errorf("expected event type %s, got %s", EntityFunctionCalculationRequest, ce.Type)
		}
		reqID, err := extractRequestID(ce)
		if err != nil {
			t.Errorf("extractRequestID: %v", err)
			return
		}

		var typedReq events.EntityFunctionCalculationRequestJson
		_, payload, _ := ParseCloudEvent(ce)
		if err := json.Unmarshal(payload, &typedReq); err != nil {
			t.Errorf("sent function request doesn't match schema: %v", err)
			return
		}
		if typedReq.FunctionName != "my-schedule-fn" {
			t.Errorf("expected functionName my-schedule-fn, got %s", typedReq.FunctionName)
		}
		if typedReq.Payload == nil {
			t.Error("expected payload to be attached when AttachEntity=true")
		}

		member := registry.Get(memberID)
		result := json.RawMessage(`{"fireAfterMs":1000}`)
		member.CompleteRequest(reqID, &ProcessingResponse{
			Success:    true,
			Result:     result,
			ResultKind: "Schedule",
		})
	}()

	result, err := dispatchFunction(dispatcher, ctx, entity, fn, "wf1", "t1", "tx-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Kind != "Schedule" {
		t.Errorf("expected Kind=Schedule, got %s", result.Kind)
	}
	if string(result.Value) != `{"fireAfterMs":1000}` {
		t.Errorf("expected Value={\"fireAfterMs\":1000}, got %s", string(result.Value))
	}
}

func TestDispatchFunction_NoMember(t *testing.T) {
	registry := NewMemberRegistry()
	dispatcher := newTestDispatcher(t, registry)
	ctx := testContext()
	entity := testEntity()

	fn := spi.ScheduleFunction{
		Name:                 "my-schedule-fn",
		ResultKind:           "Schedule",
		CalculationNodesTags: "java",
	}

	_, err := dispatchFunction(dispatcher, ctx, entity, fn, "wf1", "t1", "tx-1")
	if err == nil {
		t.Fatal("expected error for missing member")
	}
	if !errors.Is(err, contract.ErrNoMatchingMember) {
		t.Errorf("expected contract.ErrNoMatchingMember, got: %s", err)
	}
}

// TestDispatchCalloutToMember_AbandonOnCtxCancel guards spec D11: when the
// caller's context is cancelled while dispatchCalloutToMember is waiting on
// the ctx.Done() arm, the tracked pending-request entry must be cleared
// before return. Otherwise a late compute-node reply finds a dangling
// channel and the entry leaks in the member's pending map forever.
func TestDispatchCalloutToMember_AbandonOnCtxCancel(t *testing.T) {
	dispatcher, registry, memberID, sentCh := setupTestDispatcher(t)
	member := registry.Get(memberID)
	ctx, cancel := context.WithCancel(testContext())

	// Cancel only once the request has actually been sent (and therefore
	// tracked), so the cancellation races the ctx.Done() arm rather than a
	// pre-send failure.
	go func() {
		<-sentCh
		cancel()
	}()

	_, err := tryOnce(dispatcher, ctx, member, "req-ctx-cancel", "tx-1", 5000)
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
	if got := member.PendingCount(); got != 0 {
		t.Fatalf("expected pending map empty after ctx cancellation, got %d entries", got)
	}
}

// TestDispatchCalloutToMember_AbandonOnTimeout guards spec D11 for the
// response-deadline arm: a dispatch that times out because nobody answers must
// not leave the requestID behind in the member's pending map.
func TestDispatchCalloutToMember_AbandonOnTimeout(t *testing.T) {
	dispatcher, registry, memberID, _ := setupTestDispatcher(t)
	member := registry.Get(memberID)
	ctx := testContext()

	_, err := tryOnce(dispatcher, ctx, member, "req-timeout-abandon", "tx-1", 1)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if got := member.PendingCount(); got != 0 {
		t.Fatalf("expected pending map empty after timeout, got %d entries", got)
	}
}

// TestDispatchCalloutToMember_AbandonOnWriterFailure guards spec D11 for the
// writer-failure path: a request whose write to the stream fails is evicted
// by the writer (Member.write), so the dispatch observes it as the member
// going away — COMPUTE_MEMBER_DISCONNECTED via the response arm's
// resp.Disconnected case, not a raw "send failed" error — and must still
// have its (pre-registered) tracking entry cleared.
func TestDispatchCalloutToMember_AbandonOnWriterFailure(t *testing.T) {
	registry := NewMemberRegistry()
	member := registry.Register("member-send-fail", testTenantID, []string{"python"}, func(_ *cepb.CloudEvent) error {
		return fmt.Errorf("send boom")
	}, nil)
	dispatcher := newTestDispatcher(t, registry)
	ctx := testContext()

	_, err := tryOnce(dispatcher, ctx, member, "req-send-fail", "tx-1", 5000)
	var appErr *common.AppError
	if !errors.As(err, &appErr) || appErr.Code != common.ErrCodeComputeMemberDisconnected {
		t.Fatalf("err = %v, want COMPUTE_MEMBER_DISCONNECTED", err)
	}
	if got := member.PendingCount(); got != 0 {
		t.Fatalf("expected pending map empty after send failure, got %d entries", got)
	}
}

// TestMember_LateResponseAfterAbandon_NoOp guards the far side of spec D11:
// once a pending-request entry has been abandoned (dispatch already
// returned via timeout/ctx-cancel/send-failure), a late compute-node reply
// for that requestID must be a no-op — no panic, and nothing delivered to
// the now-unread channel the abandoned caller is no longer waiting on.
func TestMember_LateResponseAfterAbandon_NoOp(t *testing.T) {
	reg := NewMemberRegistry()
	m := reg.Register("m-1", "tenant-1", []string{"a"}, noopSend, nil)

	ch, err := m.TrackRequest("req-1")
	if err != nil {
		t.Fatalf("TrackRequest: %v", err)
	}
	m.AbandonRequest("req-1")

	if got := m.PendingCount(); got != 0 {
		t.Fatalf("expected pending map empty after abandon, got %d entries", got)
	}

	// Late response: must not panic and must not deliver to the abandoned
	// channel.
	m.CompleteRequest("req-1", &ProcessingResponse{Success: true})

	select {
	case resp := <-ch:
		t.Fatalf("expected no delivery to abandoned channel, got %v", resp)
	default:
	}
}

func TestBuildEntityPayload(t *testing.T) {
	e := &spi.Entity{Meta: spi.EntityMeta{
		ID: "e1", State: "S1", TransactionID: "tx1",
		ModelRef:         spi.ModelRef{EntityName: "order", ModelVersion: "3"},
		CreationDate:     time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		LastModifiedDate: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
	}, Data: []byte(`{"k":"v"}`)}
	p := buildEntityPayload(e)
	if p.Type != "JSON" {
		t.Fatalf("Type = %q, want JSON", p.Type)
	}
	meta := p.Meta.(map[string]any)
	mk := meta["modelKey"].(map[string]any)
	if mk["version"] != 3 {
		t.Fatalf("version = %v, want int 3", mk["version"])
	}
	if meta["state"] != "S1" {
		t.Fatalf("state = %v", meta["state"])
	}
}

// A member unregistered after it was chosen but before the request is
// tracked yields COMPUTE_MEMBER_DISCONNECTED at once, not DISPATCH_TIMEOUT
// after the full response timeout.
func TestDispatch_MemberGoneBeforeTrack_IsDisconnectedImmediately(t *testing.T) {
	dispatcher, registry, memberID, _ := setupTestDispatcher(t)
	member := registry.Get(memberID)
	registry.Unregister(member)

	start := time.Now()
	_, err := tryOnce(dispatcher, testContext(), member, "r1", "", 30_000)
	if time.Since(start) > 2*time.Second {
		t.Fatalf("took %v; should not wait out the response timeout", time.Since(start))
	}
	var appErr *common.AppError
	if !errors.As(err, &appErr) || appErr.Code != common.ErrCodeComputeMemberDisconnected {
		t.Fatalf("err = %v, want COMPUTE_MEMBER_DISCONNECTED", err)
	}
}

// A writer that never drains makes the enqueue itself time out, reported as
// DISPATCH_TIMEOUT with the "member not draining" message, within the
// dispatch's own timeout.
func TestDispatch_EnqueueTimeout_IsDispatchTimeoutNotDraining(t *testing.T) {
	dispatcher, member := newWedgedDispatcher(t)

	start := time.Now()
	_, err := tryOnce(dispatcher, testContext(), member, "r1", "", 100)
	var appErr *common.AppError
	if !errors.As(err, &appErr) || appErr.Code != common.ErrCodeDispatchTimeout {
		t.Fatalf("err = %v, want DISPATCH_TIMEOUT", err)
	}
	if !strings.Contains(appErr.Message, "member not draining") {
		t.Fatalf("message %q should say the member is not draining", appErr.Message)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("dispatcher blocked %v on a wedged member; the 100ms deadline should have released it", d)
	}
}

// A parent context cancelled during the enqueue wait is reported as the
// caller's cancellation, never as a timeout.
func TestDispatch_ParentCancelDuringEnqueue_IsCtxErr(t *testing.T) {
	dispatcher, member := newWedgedDispatcher(t)

	ctx, cancel := context.WithCancel(testContext())
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()
	_, err := tryOnce(dispatcher, ctx, member, "r1", "", 30_000)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// A member evicted while a dispatcher is parked in Send behind a wedged
// writer is reported as COMPUTE_MEMBER_DISCONNECTED promptly — the genuine
// Send-error arm, as opposed to the race in
// TestDispatchCalloutToMember_AbandonOnWriterFailure where Send returns nil
// before the writer's own failure evicts the member.
func TestDispatch_MemberEvictedDuringEnqueue_IsDisconnectedPromptly(t *testing.T) {
	dispatcher, member := newWedgedDispatcher(t)

	go func() {
		time.Sleep(30 * time.Millisecond)
		member.Evict(errors.New("member disconnected"))
	}()

	start := time.Now()
	_, err := tryOnce(dispatcher, testContext(), member, "r1", "", 30_000)
	var appErr *common.AppError
	if !errors.As(err, &appErr) || appErr.Code != common.ErrCodeComputeMemberDisconnected {
		t.Fatalf("err = %v, want COMPUTE_MEMBER_DISCONNECTED", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("dispatcher blocked %v after eviction; should return promptly", d)
	}
}

// newWedgedDispatcher builds a dispatcher over a member whose writer is parked
// in a raw send that never returns, so the writer cannot take another event and
// the next enqueue has nowhere to go. The parked send is released on cleanup.
func newWedgedDispatcher(t *testing.T) (*ProcessorDispatcher, *Member) {
	t.Helper()
	registry := NewMemberRegistry()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	member := registry.Register("m-wedged", testTenantID, []string{"python"},
		func(*cepb.CloudEvent) error { <-release; return nil }, nil)
	t.Cleanup(func() { registry.Unregister(member) })
	_ = member.Send(context.Background(), mustCE(t)) // wedge the writer
	return newTestDispatcher(t, registry), member
}

func mustCE(t *testing.T) *cepb.CloudEvent {
	t.Helper()
	ce, err := NewCloudEvent(CalculationMemberKeepAliveEvent, map[string]any{"success": true})
	if err != nil {
		t.Fatal(err)
	}
	return ce
}
