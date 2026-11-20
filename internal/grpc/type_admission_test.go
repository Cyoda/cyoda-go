package grpc

import (
	"encoding/json"
	"strings"
	"testing"

	events "github.com/cyoda/cyoda-go/api/grpc/events"
)

// type_admission_test.go covers the design's §9 gRPC rows for BOTH entity
// entry points — EntityCreateRequest and EntityUpdateRequest
// (internal/grpc/entity.go:36,88) — asserting the envelope (Success,
// Error.Code) per .claude/rules/test-coverage.md. See
// docs/superpowers/specs/2026-09-04-555-type-admission-design.md.
//
// model_double_whole_number_test.go's TestRPC_EntityCreate_WholeNumberIntoDoubleLeafNeedsNoLevel
// covers only the trivial case (1000, at ARRAY_LENGTH) — it does NOT cover
// DOUBLE's mantissa boundary (2147483648, the largest ten-significant-digit
// whole number) or strict validation (no changeLevel at all), so
// TestRPC_EntityCreate_DoubleMantissaBoundary_StrictAndBelowType_Succeeds
// below covers those specifically. Everything else uses a different
// declared type (STRING) or a different value (past DOUBLE's precision
// boundary) so nothing else duplicates it.

// --- EntityCreateRequest ---

// Final review I6: DOUBLE's mantissa boundary (2147483648 — ten significant
// digits, exactly representable) must be held with no model change under
// BOTH strict validation (no changeLevel: Validate, not Extend) and the most
// restrictive changeLevel (ARRAY_LENGTH: Extend, but no permission needed
// for a value the leaf already admits) — the same property
// model_double_whole_number_test.go's HTTP twin
// (TestModelExtension_WholeNumberPastIntegerRangeIntoDoubleLeaf) asserts,
// but gRPC is a separate entry point and gets its own coverage.
func TestRPC_EntityCreate_DoubleMantissaBoundary_StrictAndBelowType_Succeeds(t *testing.T) {
	for _, tc := range []struct {
		name        string
		changeLevel string // "" means strict validation: no SetChangeLevel call at all.
	}{
		{"strict", ""},
		{"array_length", "ARRAY_LENGTH"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, ctx := newTestEnv(t)
			model := "typeadm-create-double-boundary-" + tc.name
			importAndLockModel(t, svc, ctx, model, "1", map[string]any{"amount": 10.5})
			if tc.changeLevel != "" {
				if err := svc.modelHandler.SetChangeLevel(ctx, model, "1", tc.changeLevel); err != nil {
					t.Fatalf("SetChangeLevel(%s): %v", tc.changeLevel, err)
				}
			}

			ce := makeCE(EntityCreateRequest, map[string]any{
				"id":         "test",
				"dataFormat": "JSON",
				"payload": map[string]any{
					"model": map[string]any{"name": model, "version": 1},
					"data":  map[string]any{"amount": 2147483648},
				},
			})
			resp, err := svc.EntityManage(ctx, ce)
			if err != nil {
				t.Fatalf("unexpected transport error: %v", err)
			}
			var typed events.EntityTransactionResponseJson
			validateResponse(t, resp, &typed)
			if !typed.Success {
				t.Fatalf("2147483648 needs 10 significant digits, DOUBLE holds it; expected success=true, got error %+v", typed.Error)
			}

			// The model's declared types are unchanged: still DOUBLE, not
			// widened by a value the leaf already admitted.
			exportCE := makeCE(EntityModelExportRequest, map[string]any{
				"id":        "test",
				"model":     map[string]any{"name": model, "version": 1},
				"converter": "SIMPLE_VIEW",
			})
			exportResp, err := svc.EntityModelManage(ctx, exportCE)
			if err != nil {
				t.Fatalf("unexpected transport error on export: %v", err)
			}
			var exported events.EntityModelExportResponseJson
			validateResponse(t, exportResp, &exported)
			if !exported.Success {
				t.Fatalf("export failed: %+v", exported.Error)
			}
			payload, err := json.Marshal(exported.Payload)
			if err != nil {
				t.Fatalf("marshal exported payload: %v", err)
			}
			if !strings.Contains(string(payload), "DOUBLE") {
				t.Errorf("model must still declare DOUBLE; payload: %s", payload)
			}
			if strings.Contains(string(payload), "UNBOUND_DECIMAL") || strings.Contains(string(payload), "LONG") {
				t.Errorf("model must not have widened away from DOUBLE; payload: %s", payload)
			}
		})
	}
}

func TestRPC_EntityCreate_HeldValueStrict_Succeeds(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "typeadm-create-strict", "1", map[string]any{"note": "hello"})
	// No SetChangeLevel call at all: strict validation.

	ce := makeCE(EntityCreateRequest, map[string]any{
		"id":         "test",
		"dataFormat": "JSON",
		"payload": map[string]any{
			"model": map[string]any{"name": "typeadm-create-strict", "version": 1},
			"data":  map[string]any{"note": "2026-03-01"},
		},
	})
	resp, err := svc.EntityManage(ctx, ce)
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	var typed events.EntityTransactionResponseJson
	validateResponse(t, resp, &typed)
	if !typed.Success {
		t.Fatalf("a date-shaped string is held by a STRING leaf; expected success=true, got error %+v", typed.Error)
	}
}

func TestRPC_EntityCreate_HeldValueBelowTypeLevel_Succeeds(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "typeadm-create-below-type", "1", map[string]any{"note": "hello"})
	if err := svc.modelHandler.SetChangeLevel(ctx, "typeadm-create-below-type", "1", "ARRAY_LENGTH"); err != nil {
		t.Fatalf("SetChangeLevel(ARRAY_LENGTH): %v", err)
	}

	ce := makeCE(EntityCreateRequest, map[string]any{
		"id":         "test",
		"dataFormat": "JSON",
		"payload": map[string]any{
			"model": map[string]any{"name": "typeadm-create-below-type", "version": 1},
			"data":  map[string]any{"note": "2026-03-01"},
		},
	})
	resp, err := svc.EntityManage(ctx, ce)
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	var typed events.EntityTransactionResponseJson
	validateResponse(t, resp, &typed)
	if !typed.Success {
		t.Fatalf("held values need no level permission; expected success=true, got error %+v", typed.Error)
	}
}

func TestRPC_EntityCreate_UnheldValueStrict_IncompatibleType(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "typeadm-create-unheld", "1", map[string]any{"amount": 10.5})
	// No SetChangeLevel call at all: strict validation.

	// 9007199254740993 needs 16 significant digits, past DOUBLE's mantissa —
	// a genuine type change, refused at strict with no level to raise.
	ce := makeCE(EntityCreateRequest, map[string]any{
		"id":         "test",
		"dataFormat": "JSON",
		"payload": map[string]any{
			"model": map[string]any{"name": "typeadm-create-unheld", "version": 1},
			"data":  map[string]any{"amount": 9007199254740993},
		},
	})
	resp, err := svc.EntityManage(ctx, ce)
	if err != nil {
		t.Fatalf("unexpected transport error (this must be an envelope error): %v", err)
	}
	var typed events.EntityTransactionResponseJson
	validateResponse(t, resp, &typed)
	if typed.Success {
		t.Fatal("9007199254740993 exceeds DOUBLE's mantissa; expected success=false")
	}
	if typed.Error == nil || typed.Error.Code != "CLIENT_ERROR" {
		t.Fatalf("expected CLIENT_ERROR envelope, got %+v", typed.Error)
	}
	if !strings.Contains(typed.Error.Message, "INCOMPATIBLE_TYPE") {
		t.Errorf("message must carry INCOMPATIBLE_TYPE: %q", typed.Error.Message)
	}
}

func TestRPC_EntityCreate_NumberIntoString_IncompatibleType(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "typeadm-create-num-into-string", "1", map[string]any{"note": "hello"})

	ce := makeCE(EntityCreateRequest, map[string]any{
		"id":         "test",
		"dataFormat": "JSON",
		"payload": map[string]any{
			"model": map[string]any{"name": "typeadm-create-num-into-string", "version": 1},
			"data":  map[string]any{"note": 5},
		},
	})
	resp, err := svc.EntityManage(ctx, ce)
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	var typed events.EntityTransactionResponseJson
	validateResponse(t, resp, &typed)
	if typed.Success {
		t.Fatal("a JSON number is not a string; expected success=false")
	}
	if typed.Error == nil || typed.Error.Code != "CLIENT_ERROR" {
		t.Fatalf("expected CLIENT_ERROR envelope, got %+v", typed.Error)
	}
	if !strings.Contains(typed.Error.Message, "INCOMPATIBLE_TYPE") {
		t.Errorf("message must carry INCOMPATIBLE_TYPE: %q", typed.Error.Message)
	}
}

// --- EntityUpdateRequest: entity.go:88 is a separate code path from create
// (entity.go:36), and the rule says HTTP and gRPC are separate entry points —
// so are create and update within gRPC itself. ---

func TestRPC_EntityUpdate_HeldValueStrict_Succeeds(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "typeadm-update-strict", "1", map[string]any{"note": "hello"})
	// No SetChangeLevel call at all: strict validation.

	createResp, err := svc.EntityManage(ctx, makeCE(EntityCreateRequest, map[string]any{
		"id":         "test",
		"dataFormat": "JSON",
		"payload": map[string]any{
			"model": map[string]any{"name": "typeadm-update-strict", "version": 1},
			"data":  map[string]any{"note": "plain"},
		},
	}))
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	createPayload := parseResponsePayload(t, createResp)
	txInfo := createPayload["transactionInfo"].(map[string]any)
	entityID := txInfo["entityIds"].([]any)[0].(string)

	updateResp, err := svc.EntityManage(ctx, makeCE(EntityUpdateRequest, map[string]any{
		"id":         "test",
		"dataFormat": "JSON",
		"payload": map[string]any{
			"entityId": entityID,
			"data":     map[string]any{"note": "2026-03-01"},
		},
	}))
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	var typed events.EntityTransactionResponseJson
	validateResponse(t, updateResp, &typed)
	if !typed.Success {
		t.Fatalf("a date-shaped string is held by a STRING leaf; expected success=true, got error %+v", typed.Error)
	}
}

func TestRPC_EntityUpdate_HeldValueBelowTypeLevel_Succeeds(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "typeadm-update-below-type", "1", map[string]any{"note": "hello"})
	if err := svc.modelHandler.SetChangeLevel(ctx, "typeadm-update-below-type", "1", "ARRAY_LENGTH"); err != nil {
		t.Fatalf("SetChangeLevel(ARRAY_LENGTH): %v", err)
	}

	createResp, err := svc.EntityManage(ctx, makeCE(EntityCreateRequest, map[string]any{
		"id":         "test",
		"dataFormat": "JSON",
		"payload": map[string]any{
			"model": map[string]any{"name": "typeadm-update-below-type", "version": 1},
			"data":  map[string]any{"note": "plain"},
		},
	}))
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	createPayload := parseResponsePayload(t, createResp)
	txInfo := createPayload["transactionInfo"].(map[string]any)
	entityID := txInfo["entityIds"].([]any)[0].(string)

	updateResp, err := svc.EntityManage(ctx, makeCE(EntityUpdateRequest, map[string]any{
		"id":         "test",
		"dataFormat": "JSON",
		"payload": map[string]any{
			"entityId": entityID,
			"data":     map[string]any{"note": "2026-03-01"},
		},
	}))
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	var typed events.EntityTransactionResponseJson
	validateResponse(t, updateResp, &typed)
	if !typed.Success {
		t.Fatalf("held values need no level permission; expected success=true, got error %+v", typed.Error)
	}
}

func TestRPC_EntityUpdate_UnheldValueStrict_IncompatibleType(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "typeadm-update-unheld", "1", map[string]any{"amount": 10.5})
	// No SetChangeLevel call at all: strict validation.

	createResp, err := svc.EntityManage(ctx, makeCE(EntityCreateRequest, map[string]any{
		"id":         "test",
		"dataFormat": "JSON",
		"payload": map[string]any{
			"model": map[string]any{"name": "typeadm-update-unheld", "version": 1},
			"data":  map[string]any{"amount": 1},
		},
	}))
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	createPayload := parseResponsePayload(t, createResp)
	txInfo := createPayload["transactionInfo"].(map[string]any)
	entityID := txInfo["entityIds"].([]any)[0].(string)

	updateResp, err := svc.EntityManage(ctx, makeCE(EntityUpdateRequest, map[string]any{
		"id":         "test",
		"dataFormat": "JSON",
		"payload": map[string]any{
			"entityId": entityID,
			"data":     map[string]any{"amount": 9007199254740993},
		},
	}))
	if err != nil {
		t.Fatalf("unexpected transport error (this must be an envelope error): %v", err)
	}
	var typed events.EntityTransactionResponseJson
	validateResponse(t, updateResp, &typed)
	if typed.Success {
		t.Fatal("9007199254740993 exceeds DOUBLE's mantissa; expected success=false")
	}
	if typed.Error == nil || typed.Error.Code != "CLIENT_ERROR" {
		t.Fatalf("expected CLIENT_ERROR envelope, got %+v", typed.Error)
	}
	if !strings.Contains(typed.Error.Message, "INCOMPATIBLE_TYPE") {
		t.Errorf("message must carry INCOMPATIBLE_TYPE: %q", typed.Error.Message)
	}
}

func TestRPC_EntityUpdate_NumberIntoString_IncompatibleType(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "typeadm-update-num-into-string", "1", map[string]any{"note": "hello"})

	createResp, err := svc.EntityManage(ctx, makeCE(EntityCreateRequest, map[string]any{
		"id":         "test",
		"dataFormat": "JSON",
		"payload": map[string]any{
			"model": map[string]any{"name": "typeadm-update-num-into-string", "version": 1},
			"data":  map[string]any{"note": "plain"},
		},
	}))
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	createPayload := parseResponsePayload(t, createResp)
	txInfo := createPayload["transactionInfo"].(map[string]any)
	entityID := txInfo["entityIds"].([]any)[0].(string)

	updateResp, err := svc.EntityManage(ctx, makeCE(EntityUpdateRequest, map[string]any{
		"id":         "test",
		"dataFormat": "JSON",
		"payload": map[string]any{
			"entityId": entityID,
			"data":     map[string]any{"note": 5},
		},
	}))
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	var typed events.EntityTransactionResponseJson
	validateResponse(t, updateResp, &typed)
	if typed.Success {
		t.Fatal("a JSON number is not a string; expected success=false")
	}
	if typed.Error == nil || typed.Error.Code != "CLIENT_ERROR" {
		t.Fatalf("expected CLIENT_ERROR envelope, got %+v", typed.Error)
	}
	if !strings.Contains(typed.Error.Message, "INCOMPATIBLE_TYPE") {
		t.Errorf("message must carry INCOMPATIBLE_TYPE: %q", typed.Error.Message)
	}
}
