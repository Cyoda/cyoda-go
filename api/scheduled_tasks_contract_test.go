package api

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	openapi_types "github.com/oapi-codegen/runtime/types"
)

func sortedParamNames(m map[string]*openapi3.Parameter) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedPropNames(m openapi3.Schemas) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// TestListScheduledTasks_Contract pins GET /scheduled-tasks: its parameters
// and their rules, and the statuses it can answer. No X-Tx-Token: the
// operation is not a callback target. No 403 (no role is required) and no 404
// (an unknown or foreign model or entity is an empty list).
func TestListScheduledTasks_Contract(t *testing.T) {
	doc, err := GetSwagger()
	if err != nil {
		t.Fatalf("GetSwagger: %v", err)
	}
	item := doc.Paths.Find("/scheduled-tasks")
	if item == nil || item.Get == nil {
		t.Fatal("GET /scheduled-tasks is not declared")
	}
	op := item.Get
	if op.OperationID != "listScheduledTasks" {
		t.Errorf("operationId = %q, want listScheduledTasks", op.OperationID)
	}
	if op.Security == nil || len(*op.Security) != 1 {
		t.Errorf("security must be exactly bearerAuth")
	} else if _, ok := (*op.Security)[0]["bearerAuth"]; !ok {
		t.Errorf("security must be bearerAuth, got %v", *op.Security)
	}

	params := map[string]*openapi3.Parameter{}
	for _, ref := range op.Parameters {
		params[ref.Value.Name] = ref.Value
	}
	if got := strings.Join(sortedParamNames(params), ","); got != "cursor,entityId,limit,modelName,modelVersion,status" {
		t.Fatalf("parameters = %s, want cursor,entityId,limit,modelName,modelVersion,status", got)
	}
	for name, p := range params {
		if p.In != "query" || p.Required {
			t.Errorf("%s must be an optional query parameter", name)
		}
	}

	status := params["status"]
	if s := status.Schema.Value; !s.Type.Is("array") || s.Items == nil {
		t.Errorf("status must be an array")
	} else if got := fmt.Sprint(s.Items.Value.Enum); got != "[WAITING RUNNING FAILED]" {
		t.Errorf("status values = %s, want [WAITING RUNNING FAILED]", got)
	}
	if status.Explode == nil || !*status.Explode {
		t.Errorf("status must be exploded, so it is repeatable")
	}
	if s := params["modelName"].Schema.Value; !s.Type.Is("string") || s.MinLength != 1 || s.MaxLength == nil || *s.MaxLength != 256 {
		t.Errorf("modelName must be a string of 1 to 256 characters")
	}
	if s := params["modelVersion"].Schema.Value; !s.Type.Is("integer") || s.Min == nil || *s.Min != 1 {
		t.Errorf("modelVersion must be an integer of at least 1")
	}
	if s := params["entityId"].Schema.Value; !s.Type.Is("string") || s.Format != "uuid" {
		t.Errorf("entityId must be a uuid string")
	}
	if s := params["cursor"].Schema.Value; !s.Type.Is("string") || s.MaxLength == nil || *s.MaxLength != 256 {
		t.Errorf("cursor must be a string of at most 256 characters")
	}
	if s := params["limit"].Schema.Value; !s.Type.Is("integer") || s.Min == nil || *s.Min != 1 ||
		s.Max == nil || *s.Max != 1000 || fmt.Sprint(s.Default) != "20" {
		t.Errorf("limit must be an integer 1..1000 with default 20")
	}

	var codes []string
	for code := range op.Responses.Map() {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	if got := strings.Join(codes, ","); got != "200,400,401,403,500,503" {
		t.Errorf("responses = %s, want 200,400,401,403,500,503", got)
	}
	ok := op.Responses.Status(200)
	if ok == nil || ok.Value.Content["application/json"] == nil ||
		ok.Value.Content["application/json"].Schema.Ref != "#/components/schemas/ScheduledTaskPageDto" {
		t.Errorf("200 must be application/json ScheduledTaskPageDto")
	}
}

// TestScheduledTaskDtos_TypedButOpen pins the three response schemas: every
// field the server emits is declared, none is sealed (ADR 0003 decision 2),
// status and failureReason are open value sets (decision 4), and no token,
// claim, owner or tenant field exists.
func TestScheduledTaskDtos_TypedButOpen(t *testing.T) {
	doc, err := GetSwagger()
	if err != nil {
		t.Fatalf("GetSwagger: %v", err)
	}
	want := map[string]struct{ props, required []string }{
		"ScheduledTaskPageDto": {
			props:    []string{"items", "pagination"},
			required: []string{"items", "pagination"},
		},
		"ScheduledTaskDto": {
			props: []string{"armedBy", "armedTime", "attempts", "entityId", "expiresTime", "failedTime",
				"failureReason", "lastAttemptTime", "lastError", "lostOwners", "modelName", "modelVersion",
				"nextAttemptTime", "scheduledTime", "sourceState", "status", "taskId", "transition"},
			required: []string{"armedTime", "attempts", "entityId", "lostOwners", "modelName", "modelVersion",
				"scheduledTime", "sourceState", "status", "taskId", "transition"},
		},
		"ScheduledTaskArmedByDto": {
			props:    []string{"id", "kind"},
			required: []string{"id", "kind"},
		},
	}
	for name, w := range want {
		ref := doc.Components.Schemas[name]
		if ref == nil || ref.Value == nil {
			t.Errorf("%s is not declared", name)
			continue
		}
		s := ref.Value
		if got := strings.Join(sortedPropNames(s.Properties), ","); got != strings.Join(w.props, ",") {
			t.Errorf("%s properties = %s, want %s", name, got, strings.Join(w.props, ","))
		}
		if got := strings.Join(sortedCopy(s.Required), ","); got != strings.Join(w.required, ",") {
			t.Errorf("%s required = %s, want %s", name, got, strings.Join(w.required, ","))
		}
		if s.AdditionalProperties.Has != nil && !*s.AdditionalProperties.Has {
			t.Errorf("%s is sealed; response schemas stay open (ADR 0003)", name)
		}
	}

	dto := doc.Components.Schemas["ScheduledTaskDto"].Value.Properties
	for _, open := range []string{"status", "failureReason"} {
		if len(dto[open].Value.Enum) != 0 {
			t.Errorf("%s must be an open value set, not an enum", open)
		}
	}
	for _, known := range []string{"WAITING", "RUNNING", "FAILED"} {
		if !strings.Contains(dto["status"].Value.Description, known) {
			t.Errorf("status description does not name %s", known)
		}
	}
	for _, known := range []string{"UNSAFE_WORK_NOT_COMPLETED", "OWNER_LOST_REPEATEDLY",
		"EXPIRED_AFTER_FAILED_ATTEMPTS", "RUN_PANICKED", "STOPPED_AFTER_PARTIAL_COMMIT"} {
		if !strings.Contains(dto["failureReason"].Value.Description, known) {
			t.Errorf("failureReason description does not name %s", known)
		}
	}
	for _, ts := range []string{"scheduledTime", "armedTime", "expiresTime", "nextAttemptTime", "lastAttemptTime", "failedTime"} {
		if dto[ts].Value.Format != "date-time" {
			t.Errorf("%s must be a date-time", ts)
		}
	}
	if dto["entityId"].Value.Format != "uuid" {
		t.Errorf("entityId must be a uuid")
	}
}

// TestScheduledTaskDto_GeneratedTypes pins the Go types the handler builds
// the response from. It fails to compile when a field changes type.
func TestScheduledTaskDto_GeneratedTypes(t *testing.T) {
	now := time.Now()
	s := "x"
	v := int32(1)
	id := openapi_types.UUID{}
	_ = ListScheduledTasksParams{
		Status:       &[]ListScheduledTasksParamsStatus{"WAITING"},
		ModelName:    &s,
		ModelVersion: &v,
		EntityId:     &id,
		Cursor:       &s,
		Limit:        &v,
	}
	_ = ScheduledTaskPageDto{
		Items: []ScheduledTaskDto{{
			TaskId: s, EntityId: id, ModelName: s, ModelVersion: v, SourceState: s, Transition: s, Status: s,
			ScheduledTime: now, ArmedTime: now, ExpiresTime: &now, Attempts: v, LostOwners: v,
			NextAttemptTime: &now, LastAttemptTime: &now, LastError: &s, FailureReason: &s, FailedTime: &now,
			ArmedBy: &ScheduledTaskArmedByDto{Id: s, Kind: s},
		}},
		Pagination: CursorPaginationInfoDto{HasNext: false},
	}
}

// TestStateMachineAuditEventType_HasScheduledTransitionFail pins the audit
// event a FAILED scheduled task records. Every e2e test that reads it through
// the validated audit API needs it in the enum.
func TestStateMachineAuditEventType_HasScheduledTransitionFail(t *testing.T) {
	doc, err := GetSwagger()
	if err != nil {
		t.Fatalf("GetSwagger: %v", err)
	}
	sm := doc.Components.Schemas["StateMachineAuditEventDto"]
	if sm == nil || sm.Value == nil {
		t.Fatal("StateMachineAuditEventDto is not declared")
	}
	var found bool
	for _, part := range sm.Value.AllOf {
		et := part.Value.Properties["eventType"]
		if et == nil {
			continue
		}
		for _, v := range et.Value.Enum {
			if v == "SCHEDULED_TRANSITION_FAIL" {
				found = true
			}
		}
	}
	if !found {
		t.Error("StateMachineAuditEventDto.eventType does not list SCHEDULED_TRANSITION_FAIL")
	}
	if !SCHEDULEDTRANSITIONFAIL.Valid() {
		t.Error("generated SCHEDULEDTRANSITIONFAIL is not Valid()")
	}
}
