package dispatch_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/dispatch"
)

func TestDispatchCalloutRequest_ProcessorJSONRoundTrip(t *testing.T) {
	entityData := json.RawMessage(`{"foo":"bar","count":42}`)
	processor := spi.ProcessorDefinition{
		Type: "HTTP",
		Name: "my-processor",
		Config: spi.ProcessorConfig{
			AttachEntity:         true,
			CalculationNodesTags: "gpu",
		},
	}
	req := dispatch.DispatchCalloutRequest{
		Kind:   "processor",
		Entity: entityData,
		EntityMeta: spi.EntityMeta{
			ID:       "ent-123",
			TenantID: "tenant-abc",
			ModelRef: spi.ModelRef{EntityName: "Order", ModelVersion: "1"},
			State:    "CREATED",
			Version:  3,
		},
		Processor:      &processor,
		WorkflowName:   "order-workflow",
		TransitionName: "approve",
		TxID:           "tx-999",
		TenantID:       "tenant-abc",
		Tags:           "gpu",
		UserID:         "user-1",
		PrincipalKind:  spi.PrincipalUser,
		Roles:          []string{"admin", "editor"},
	}

	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got dispatch.DispatchCalloutRequest
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if got.Kind != "processor" {
		t.Errorf("Kind = %q, want processor", got.Kind)
	}
	if got.WorkflowName != req.WorkflowName {
		t.Errorf("WorkflowName = %q, want %q", got.WorkflowName, req.WorkflowName)
	}
	if got.TransitionName != req.TransitionName {
		t.Errorf("TransitionName = %q, want %q", got.TransitionName, req.TransitionName)
	}
	if got.TxID != req.TxID {
		t.Errorf("TxID = %q, want %q", got.TxID, req.TxID)
	}
	if got.TenantID != req.TenantID {
		t.Errorf("TenantID = %q, want %q", got.TenantID, req.TenantID)
	}
	if got.Tags != req.Tags {
		t.Errorf("Tags = %q, want %q", got.Tags, req.Tags)
	}
	if got.UserID != req.UserID {
		t.Errorf("UserID = %q, want %q", got.UserID, req.UserID)
	}
	if len(got.Roles) != 2 || got.Roles[0] != "admin" || got.Roles[1] != "editor" {
		t.Errorf("Roles = %v, want [admin editor]", got.Roles)
	}
	if got.PrincipalKind != spi.PrincipalUser {
		t.Errorf("PrincipalKind = %q, want %q", got.PrincipalKind, spi.PrincipalUser)
	}
	if got.Processor == nil || got.Processor.Type != "HTTP" {
		t.Errorf("Processor.Type = %v, want HTTP", got.Processor)
	}
	if got.Processor.Config.CalculationNodesTags != "gpu" {
		t.Errorf("Processor.Config.CalculationNodesTags = %q, want gpu", got.Processor.Config.CalculationNodesTags)
	}
	if got.EntityMeta.ID != "ent-123" {
		t.Errorf("EntityMeta.ID = %q, want ent-123", got.EntityMeta.ID)
	}
	if string(got.Entity) != string(entityData) {
		t.Errorf("Entity = %s, want %s", got.Entity, entityData)
	}
}

func TestDispatchCalloutResponse_ProcessorJSONRoundTrip(t *testing.T) {
	entityData := []byte(`{"updated":true}`)
	resp := dispatch.DispatchCalloutResponse{
		EntityData: entityData,
		Success:    true,
		Error:      "",
		Warnings:   []string{"warn1", "warn2"},
	}

	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got dispatch.DispatchCalloutResponse
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if !got.Success {
		t.Error("Success = false, want true")
	}
	if got.Error != "" {
		t.Errorf("Error = %q, want empty", got.Error)
	}
	if len(got.Warnings) != 2 {
		t.Errorf("len(Warnings) = %d, want 2", len(got.Warnings))
	}
	if string(got.EntityData) != string(entityData) {
		t.Errorf("EntityData = %s, want %s", got.EntityData, entityData)
	}
}

func TestDispatchCalloutResponse_ProcessorError_JSONRoundTrip(t *testing.T) {
	resp := dispatch.DispatchCalloutResponse{
		Success:  false,
		Error:    "PROCESSOR_FAILED: something went wrong",
		Warnings: nil,
	}

	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got dispatch.DispatchCalloutResponse
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if got.Success {
		t.Error("Success = true, want false")
	}
	if got.Error != "PROCESSOR_FAILED: something went wrong" {
		t.Errorf("Error = %q, want PROCESSOR_FAILED message", got.Error)
	}
	if got.EntityData != nil {
		t.Errorf("EntityData should be nil/omitted, got %s", got.EntityData)
	}
}

func TestDispatchCalloutRequest_CriteriaJSONRoundTrip(t *testing.T) {
	entityData := json.RawMessage(`{"status":"pending"}`)
	criterion := json.RawMessage(`{"type":"FIELD_MATCH","field":"status","value":"pending"}`)

	req := dispatch.DispatchCalloutRequest{
		Kind:           "criteria",
		Entity:         entityData,
		EntityMeta:     spi.EntityMeta{ID: "ent-456", TenantID: "tenant-xyz"},
		Criterion:      criterion,
		Target:         "TRANSITION",
		WorkflowName:   "order-workflow",
		TransitionName: "approve",
		ProcessorName:  "check-status",
		TxID:           "tx-100",
		TenantID:       "tenant-xyz",
		Tags:           "cpu",
		UserID:         "user-2",
		PrincipalKind:  spi.PrincipalService,
		Roles:          []string{"viewer"},
	}

	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got dispatch.DispatchCalloutRequest
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if got.Kind != "criteria" {
		t.Errorf("Kind = %q, want criteria", got.Kind)
	}
	if got.Target != "TRANSITION" {
		t.Errorf("Target = %q, want TRANSITION", got.Target)
	}
	if got.ProcessorName != "check-status" {
		t.Errorf("ProcessorName = %q, want check-status", got.ProcessorName)
	}
	if got.WorkflowName != "order-workflow" {
		t.Errorf("WorkflowName = %q, want order-workflow", got.WorkflowName)
	}
	if got.TransitionName != "approve" {
		t.Errorf("TransitionName = %q, want approve", got.TransitionName)
	}
	if got.TxID != "tx-100" {
		t.Errorf("TxID = %q, want tx-100", got.TxID)
	}
	if got.TenantID != "tenant-xyz" {
		t.Errorf("TenantID = %q, want tenant-xyz", got.TenantID)
	}
	if got.Tags != "cpu" {
		t.Errorf("Tags = %q, want cpu", got.Tags)
	}
	if got.UserID != "user-2" {
		t.Errorf("UserID = %q, want user-2", got.UserID)
	}
	if len(got.Roles) != 1 || got.Roles[0] != "viewer" {
		t.Errorf("Roles = %v, want [viewer]", got.Roles)
	}
	if got.PrincipalKind != spi.PrincipalService {
		t.Errorf("PrincipalKind = %q, want %q", got.PrincipalKind, spi.PrincipalService)
	}
	if string(got.Entity) != string(entityData) {
		t.Errorf("Entity = %s, want %s", got.Entity, entityData)
	}
	if string(got.Criterion) != string(criterion) {
		t.Errorf("Criterion = %s, want %s", got.Criterion, criterion)
	}
}

func TestDispatchCalloutResponse_CriteriaReasonRoundTrip(t *testing.T) {
	matches := false
	in := dispatch.DispatchCalloutResponse{Matches: &matches, Success: true, Reason: "amount 5 below minimum 10"}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out dispatch.DispatchCalloutResponse
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Reason != in.Reason {
		t.Errorf("reason not round-tripped: got %q want %q", out.Reason, in.Reason)
	}
}

func TestDispatchCalloutResponse_CriteriaJSONRoundTrip(t *testing.T) {
	matches := true
	resp := dispatch.DispatchCalloutResponse{
		Matches:  &matches,
		Success:  true,
		Error:    "",
		Warnings: []string{"w1"},
	}

	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got dispatch.DispatchCalloutResponse
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if got.Matches == nil || !*got.Matches {
		t.Error("Matches = false, want true")
	}
	if !got.Success {
		t.Error("Success = false, want true")
	}
	if len(got.Warnings) != 1 || got.Warnings[0] != "w1" {
		t.Errorf("Warnings = %v, want [w1]", got.Warnings)
	}
}

func TestDispatchCalloutResponse_CriteriaNoMatch_JSONRoundTrip(t *testing.T) {
	matches := false
	resp := dispatch.DispatchCalloutResponse{
		Matches: &matches,
		Success: true,
	}

	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got dispatch.DispatchCalloutResponse
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if got.Matches == nil || *got.Matches {
		t.Error("Matches = true, want false")
	}
	if !got.Success {
		t.Error("Success = false, want true")
	}
}

// TestDispatchCalloutRequest_EntityMetaTimestamps ensures time.Time fields
// in EntityMeta survive a JSON round-trip.
func TestDispatchCalloutRequest_EntityMetaTimestamps(t *testing.T) {
	now := time.Date(2026, 3, 29, 12, 0, 0, 0, time.UTC)
	req := dispatch.DispatchCalloutRequest{
		Kind:   "processor",
		Entity: json.RawMessage(`{}`),
		EntityMeta: spi.EntityMeta{
			ID:               "ent-ts",
			CreationDate:     now,
			LastModifiedDate: now.Add(time.Hour),
		},
	}

	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got dispatch.DispatchCalloutRequest
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if !got.EntityMeta.CreationDate.Equal(now) {
		t.Errorf("CreationDate = %v, want %v", got.EntityMeta.CreationDate, now)
	}
	if !got.EntityMeta.LastModifiedDate.Equal(now.Add(time.Hour)) {
		t.Errorf("LastModifiedDate = %v, want %v", got.EntityMeta.LastModifiedDate, now.Add(time.Hour))
	}
}

func TestDispatchCalloutRequest_HandOverFieldsRoundTrip(t *testing.T) {
	req := dispatch.DispatchCalloutRequest{
		Kind: "processor", RequestID: "rid", TriesLeft: 3, AnswerLimitMs: 30000, OwnerNodeID: "n1",
		Major: 4, RepeatSafe: true, Outer: []dispatch.WirePair{{Callout: "o", Major: 2, Minor: 1}},
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{`"requestID":"rid"`, `"triesLeft":3`, `"answerLimitMs":30000`, `"ownerNodeID":"n1"`, `"major":4`, `"repeatSafe":true`, `"outer":[{"callout":"o","major":2,"minor":1}]`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("wire form lacks %s: %s", key, raw)
		}
	}
	var got dispatch.DispatchCalloutRequest
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.RequestID != "rid" || got.TriesLeft != 3 || got.Major != 4 || len(got.Outer) != 1 {
		t.Errorf("round trip lost fields: %+v", got)
	}
}

func TestDispatchCalloutResponse_TriesUsedZeroIsNotAbsent(t *testing.T) {
	zero := 0
	with, _ := json.Marshal(dispatch.DispatchCalloutResponse{Outcome: "no_handoff", TriesUsed: &zero})
	without, _ := json.Marshal(dispatch.DispatchCalloutResponse{Outcome: "no_handoff"})
	if !strings.Contains(string(with), `"triesUsed":0`) {
		t.Errorf("triesUsed 0 must be on the wire: %s", with)
	}
	if strings.Contains(string(without), "triesUsed") {
		t.Errorf("an absent triesUsed must stay absent: %s", without)
	}
}
