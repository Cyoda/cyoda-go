package workflow_test

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/audit"
	"github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
	"github.com/cyoda-platform/cyoda-go/internal/e2e/openapivalidator"
	"github.com/cyoda-platform/cyoda-go/internal/scheduler"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// neverSent are the mapped event types cyoda-go does not record: a processor
// has no criterion, and the default workflow is always installed.
var neverSent = []string{"PROCESS_NOT_MATCH_CRITERION", "WORKFLOW_NOT_FOUND"}

// TestEveryEventTypeValidatesWhenDrivenThroughTheEngine records every event
// type cyoda-go sends by driving the engine and the scheduler, reads each
// event through the audit endpoint's handler, and validates it against
// AuditEventDto in the validator's view: its data against the member schema
// its eventType maps to.
func TestEveryEventTypeValidatesWhenDrivenThroughTheEngine(t *testing.T) {
	doc, err := api.GetSwagger()
	if err != nil {
		t.Fatalf("GetSwagger: %v", err)
	}
	if err := openapivalidator.ApplyView(doc); err != nil {
		t.Fatalf("ApplyView: %v", err)
	}
	auditEvent, err := openapivalidator.ComponentUse(doc, "AuditEventDto")
	if err != nil {
		t.Fatalf("ComponentUse: %v", err)
	}

	type source struct {
		factory spi.StoreFactory
		ctx     context.Context
		ids     []string
	}
	factory, ctx, ids := workflow.RecordEngineEvents(t)
	failFactory, failCtx, failID := recordScheduledFailure(t)
	sources := []source{{factory, ctx, ids}, {failFactory, failCtx, []string{failID}}}

	seen := map[string]bool{}
	// variantsSeen records, for the event types whose data has an optional
	// field, which of the two shapes was validated.
	variantsSeen := map[string]bool{}
	for _, src := range sources {
		for _, id := range src.ids {
			for _, item := range auditItems(t, src.factory, src.ctx, id) {
				if item["auditEventType"] != "StateMachine" {
					continue
				}
				eventType, _ := item["eventType"].(string)
				seen[eventType] = true
				data, ok := item["data"].(map[string]any)
				if !ok {
					t.Errorf("%s event of %s has no data object: %v", eventType, id, item)
					continue
				}
				switch eventType {
				case "STATE_PROCESS_RESULT":
					_, has := data["failReason"]
					variantsSeen[optionalVariant(eventType, "failReason", has)] = true
				case "TRANSITION_ABORTED":
					_, has := data["transition"]
					variantsSeen[optionalVariant(eventType, "transition", has)] = true
				}
				if err := openapivalidator.ValidateValue(doc, auditEvent, item); err != nil {
					raw, _ := json.Marshal(item)
					t.Errorf("%s event of %s does not validate: %v\n%s", eventType, id, err, raw)
				}
			}
		}
	}

	base := doc.Components.Schemas["StateMachineEventDto"].Value
	var want []string
	for k := range base.Discriminator.Mapping {
		if !slices.Contains(neverSent, k) {
			want = append(want, k)
		}
	}
	slices.Sort(want)
	got := slices.Sorted(maps.Keys(seen))
	if !slices.Equal(got, want) {
		t.Errorf("event types seen = %v\nwant the mapping less the never-sent types = %v", got, want)
	}

	for _, v := range []string{
		optionalVariant("STATE_PROCESS_RESULT", "failReason", true),
		optionalVariant("STATE_PROCESS_RESULT", "failReason", false),
		optionalVariant("TRANSITION_ABORTED", "transition", true),
		optionalVariant("TRANSITION_ABORTED", "transition", false),
	} {
		if !variantsSeen[v] {
			t.Errorf("no %s event was validated", v)
		}
	}
}

// optionalVariant names one shape of an event whose data has an optional
// field.
func optionalVariant(eventType, field string, present bool) string {
	if present {
		return eventType + " with " + field
	}
	return eventType + " without " + field
}

// auditItems reads every audit event of entityID through the audit handler.
func auditItems(t *testing.T, factory spi.StoreFactory, ctx context.Context, entityID string) []map[string]any {
	t.Helper()
	id, err := uuid.Parse(entityID)
	if err != nil {
		t.Fatalf("entity id %q is not a UUID: %v", entityID, err)
	}
	limit := "1000"
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/audit/entity/"+entityID, nil)
	rec := httptest.NewRecorder()
	audit.New(factory).SearchEntityAuditEvents(rec, req, id, api.SearchEntityAuditEventsParams{Limit: &limit})
	if rec.Code != http.StatusOK {
		t.Fatalf("audit of %s: status %d: %s", entityID, rec.Code, rec.Body)
	}
	var body struct {
		Items      []map[string]any `json:"items"`
		Pagination struct {
			HasNext bool `json:"hasNext"`
		} `json:"pagination"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode audit of %s: %v", entityID, err)
	}
	if body.Pagination.HasNext {
		t.Fatalf("audit of %s has more than one page", entityID)
	}
	return body.Items
}

// unsafeFailure is a run whose unsafe work did not complete: the scheduler
// fails the task and records SCHEDULED_TRANSITION_FAIL.
type unsafeFailure struct{}

func (unsafeFailure) FireScheduledTransition(context.Context, spi.ScheduledTask, int, time.Duration) workflow.RunReport {
	return workflow.RunReport{Outcome: workflow.OutcomeFailed, UnsafeReached: true, Err: errors.New("no answer")}
}

// recordScheduledFailure arms a due task on a memory store of its own, runs
// the scheduler until it records the task's SCHEDULED_TRANSITION_FAIL, and
// returns the store, a context of the tenant and the entity's id.
func recordScheduledFailure(t *testing.T) (spi.StoreFactory, context.Context, string) {
	t.Helper()
	const tenant = spi.TenantID("fail-tenant")
	mem := memory.NewStoreFactory()
	t.Cleanup(func() { _ = mem.Close() })
	ctx := spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID: "alice", Kind: spi.PrincipalUser, Tenant: spi.Tenant{ID: tenant, Name: string(tenant)}, Roles: []string{"USER"},
	})
	entityID := uuid.NewString()
	ref := spi.ModelRef{EntityName: "fail-model", ModelVersion: "1"}
	es, err := mem.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	if _, err := es.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: entityID, TenantID: tenant, State: "OPEN", ModelRef: ref, TransactionID: "seed-tx"},
		Data: []byte(`{}`),
	}); err != nil {
		t.Fatalf("save entity: %v", err)
	}
	sts, err := mem.ScheduledTaskStore(ctx)
	if err != nil {
		t.Fatalf("ScheduledTaskStore: %v", err)
	}
	now := time.Now().UnixMilli()
	if _, err := sts.ReconcileForEntity(ctx, spi.ReconcileRequest{
		TenantID: tenant, EntityID: entityID, CurrentState: "OPEN",
		Arm: []spi.ScheduledTask{{
			ID: "fail-task", TenantID: tenant, Type: spi.ScheduledTaskFireTransition, ScheduledTime: now - 1,
			EntityID: entityID, ModelName: ref.EntityName, ModelVersion: 1, Transition: "AutoClose",
			SourceState: "OPEN", ArmedAt: now - 1, ArmedBy: spi.Principal{ID: "alice", Kind: spi.PrincipalUser},
		}},
	}); err != nil {
		t.Fatalf("arm task: %v", err)
	}

	svc := scheduler.New(scheduler.Config{
		Enabled: true, ScanInterval: 5 * time.Millisecond, MaxRuns: 1, MaxRunsPerTenant: 1,
		HeartbeatInterval: 10 * time.Millisecond, StaleAfter: time.Hour, MaxLostOwners: 3,
		RetryDelay: 30 * time.Second, RetryDelayMax: 15 * time.Minute, ShutdownDrain: 50 * time.Millisecond,
	}, scheduler.Deps{
		Store:     mem,
		TxManager: mem.NewTransactionManager(common.NewTestUUIDGenerator()),
		Firer:     unsafeFailure{},
		Clock:     scheduler.NewRealClock(),
	})
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("scheduler Start: %v", err)
	}
	defer svc.Stop()

	smAudit, err := mem.StateMachineAuditStore(ctx)
	if err != nil {
		t.Fatalf("StateMachineAuditStore: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		events, err := smAudit.GetEvents(ctx, entityID)
		if err != nil {
			t.Fatalf("GetEvents: %v", err)
		}
		if slices.ContainsFunc(events, func(ev spi.StateMachineEvent) bool {
			return ev.EventType == spi.SMEventScheduledTransitionFailed
		}) {
			return mem, ctx, entityID
		}
		if time.Now().After(deadline) {
			t.Fatal("the scheduler recorded no SCHEDULED_TRANSITION_FAIL")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
