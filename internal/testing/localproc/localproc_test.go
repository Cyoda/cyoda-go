package localproc_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/testing/localproc"
)

func testEntity() *spi.Entity {
	return &spi.Entity{
		Meta: spi.EntityMeta{
			ID:    "ent-1",
			State: "CREATED",
			ModelRef: spi.ModelRef{
				EntityName:   "TestModel",
				ModelVersion: "1",
			},
		},
		Data: []byte(`{"name":"Alice","age":30}`),
	}
}

func TestLocalProc_DispatchProcessor_Registered(t *testing.T) {
	svc := localproc.New()
	svc.RegisterProcessor("enrich", func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition) (*spi.Entity, error) {
		var data map[string]any
		json.Unmarshal(entity.Data, &data)
		data["enriched"] = true
		updated, _ := json.Marshal(data)
		return &spi.Entity{Meta: entity.Meta, Data: updated}, nil
	})

	entity := testEntity()
	proc := spi.ProcessorDefinition{Name: "enrich"}
	result, err := svc.DispatchProcessor(context.Background(), entity, proc, "wf", "tr", "tx-1")
	if err != nil {
		t.Fatalf("DispatchProcessor: %v", err)
	}

	var data map[string]any
	json.Unmarshal(result.Data, &data)
	if data["enriched"] != true {
		t.Errorf("expected enriched=true, got %v", data["enriched"])
	}
}

func TestLocalProc_DispatchProcessor_Unregistered(t *testing.T) {
	svc := localproc.New()
	entity := testEntity()
	proc := spi.ProcessorDefinition{Name: "missing"}
	_, err := svc.DispatchProcessor(context.Background(), entity, proc, "wf", "tr", "tx-1")
	if err == nil {
		t.Fatal("expected error for unregistered processor")
	}
}

func TestLocalProc_DispatchProcessor_ReturnsError(t *testing.T) {
	svc := localproc.New()
	svc.RegisterProcessor("fail", func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition) (*spi.Entity, error) {
		return nil, fmt.Errorf("processor failed")
	})

	entity := testEntity()
	_, err := svc.DispatchProcessor(context.Background(), entity, spi.ProcessorDefinition{Name: "fail"}, "wf", "tr", "tx-1")
	if err == nil {
		t.Fatal("expected error from failing processor")
	}
}

func TestLocalProc_DispatchCriteria_Registered(t *testing.T) {
	svc := localproc.New()
	svc.RegisterCriteria("check-age", func(ctx context.Context, entity *spi.Entity, criterion json.RawMessage) (bool, error) {
		var data map[string]any
		json.Unmarshal(entity.Data, &data)
		age, _ := data["age"].(float64)
		return age >= 18, nil
	})

	entity := testEntity()
	criterion := json.RawMessage(`{"type":"function","function":{"name":"check-age"}}`)
	matched, _, err := svc.DispatchCriteria(context.Background(), entity, criterion, "TRANSITION", "wf", "tr", "", "tx-1")
	if err != nil {
		t.Fatalf("DispatchCriteria: %v", err)
	}
	if !matched {
		t.Error("expected criteria to match (age=30 >= 18)")
	}
}

func TestLocalProc_DispatchCriteria_ReturnsFalse(t *testing.T) {
	svc := localproc.New()
	svc.RegisterCriteria("is-minor", func(ctx context.Context, entity *spi.Entity, criterion json.RawMessage) (bool, error) {
		return false, nil
	})

	entity := testEntity()
	criterion := json.RawMessage(`{"type":"function","function":{"name":"is-minor"}}`)
	matched, _, err := svc.DispatchCriteria(context.Background(), entity, criterion, "TRANSITION", "wf", "tr", "", "tx-1")
	if err != nil {
		t.Fatalf("DispatchCriteria: %v", err)
	}
	if matched {
		t.Error("expected criteria not to match")
	}
}

func TestLocalProc_DispatchCriteria_Unregistered(t *testing.T) {
	svc := localproc.New()
	entity := testEntity()
	criterion := json.RawMessage(`{"type":"function","function":{"name":"missing"}}`)
	_, _, err := svc.DispatchCriteria(context.Background(), entity, criterion, "TRANSITION", "wf", "tr", "", "tx-1")
	if err == nil {
		t.Fatal("expected error for unregistered criteria")
	}
}

func TestLocalProc_InvocationCount(t *testing.T) {
	svc := localproc.New()
	svc.RegisterProcessor("counter", func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition) (*spi.Entity, error) {
		return entity, nil
	})

	if svc.ProcessorCallCount("counter") != 0 {
		t.Fatal("expected 0 calls before invocation")
	}

	entity := testEntity()
	for i := 0; i < 3; i++ {
		svc.DispatchProcessor(context.Background(), entity, spi.ProcessorDefinition{Name: "counter"}, "wf", "tr", "tx")
	}

	if svc.ProcessorCallCount("counter") != 3 {
		t.Errorf("expected 3 calls, got %d", svc.ProcessorCallCount("counter"))
	}
	if svc.ProcessorCallCount("other") != 0 {
		t.Error("expected 0 calls for unregistered processor")
	}
}

func TestLocalProc_CriteriaCallCount(t *testing.T) {
	svc := localproc.New()
	svc.RegisterCriteria("gate", func(ctx context.Context, entity *spi.Entity, criterion json.RawMessage) (bool, error) {
		return true, nil
	})

	entity := testEntity()
	criterion := json.RawMessage(`{"type":"function","function":{"name":"gate"}}`)
	for i := 0; i < 2; i++ {
		svc.DispatchCriteria(context.Background(), entity, criterion, "TRANSITION", "wf", "tr", "", "tx")
	}

	if svc.CriteriaCallCount("gate") != 2 {
		t.Errorf("expected 2 calls, got %d", svc.CriteriaCallCount("gate"))
	}
}

func TestLocalProc_Reset(t *testing.T) {
	svc := localproc.New()
	svc.RegisterProcessor("p", func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition) (*spi.Entity, error) {
		return entity, nil
	})

	entity := testEntity()
	svc.DispatchProcessor(context.Background(), entity, spi.ProcessorDefinition{Name: "p"}, "wf", "tr", "tx")

	svc.Reset()
	if svc.ProcessorCallCount("p") != 0 {
		t.Error("expected 0 calls after reset")
	}
	// Processor should still be registered after reset.
	_, err := svc.DispatchProcessor(context.Background(), entity, spi.ProcessorDefinition{Name: "p"}, "wf", "tr", "tx")
	if err != nil {
		t.Fatalf("processor should still be registered after reset: %v", err)
	}
}
