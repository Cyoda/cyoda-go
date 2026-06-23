package observability_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/observability"
)

type fakeDispatcher struct {
	processorCalled bool
	criteriaCalled  bool
	returnErr       error
}

func (f *fakeDispatcher) DispatchProcessor(
	ctx context.Context, entity *spi.Entity, processor spi.ProcessorDefinition,
	workflowName, transitionName, txID string,
) (*spi.Entity, error) {
	f.processorCalled = true
	if f.returnErr != nil {
		return nil, f.returnErr
	}
	return entity, nil
}

func (f *fakeDispatcher) DispatchCriteria(
	ctx context.Context, entity *spi.Entity, criterion json.RawMessage,
	target, workflowName, transitionName, processorName, txID string,
) (bool, error) {
	f.criteriaCalled = true
	if f.returnErr != nil {
		return false, f.returnErr
	}
	return true, nil
}

func TestTracingExternalProcessingService_DispatchProcessor_DelegatesToInner(t *testing.T) {
	shutdown, _ := observability.Init(context.Background(), "test", "node-test", true)
	defer shutdown(context.Background())

	inner := &fakeDispatcher{}
	traced := observability.NewTracingExternalProcessingService(inner)

	entity := &spi.Entity{Meta: spi.EntityMeta{ID: "ent-1"}}
	processor := spi.ProcessorDefinition{
		Name:          "myProcessor",
		ExecutionMode: "async",
		Config: spi.ProcessorConfig{
			CalculationNodesTags: "tag1",
		},
	}

	result, err := traced.DispatchProcessor(context.Background(), entity, processor, "wf1", "t1", "tx-1")
	if err != nil {
		t.Fatalf("DispatchProcessor: %v", err)
	}
	if result != entity {
		t.Error("expected inner entity to be returned")
	}
	if !inner.processorCalled {
		t.Error("inner.DispatchProcessor not called")
	}
}

func TestTracingExternalProcessingService_DispatchCriteria_DelegatesToInner(t *testing.T) {
	shutdown, _ := observability.Init(context.Background(), "test", "node-test", true)
	defer shutdown(context.Background())

	inner := &fakeDispatcher{}
	traced := observability.NewTracingExternalProcessingService(inner)

	entity := &spi.Entity{Meta: spi.EntityMeta{ID: "ent-2"}}
	criterion := json.RawMessage(`{"op":"eq","field":"status","value":"active"}`)

	matches, err := traced.DispatchCriteria(context.Background(), entity, criterion, "target1", "wf1", "t1", "proc1", "tx-1")
	if err != nil {
		t.Fatalf("DispatchCriteria: %v", err)
	}
	if !matches {
		t.Error("expected matches=true from inner")
	}
	if !inner.criteriaCalled {
		t.Error("inner.DispatchCriteria not called")
	}
}

func TestTracingExternalProcessingService_DispatchProcessor_PropagatesError(t *testing.T) {
	shutdown, _ := observability.Init(context.Background(), "test", "node-test", true)
	defer shutdown(context.Background())

	wantErr := errors.New("dispatch failed")
	inner := &fakeDispatcher{returnErr: wantErr}
	traced := observability.NewTracingExternalProcessingService(inner)

	entity := &spi.Entity{Meta: spi.EntityMeta{ID: "ent-3"}}
	processor := spi.ProcessorDefinition{Name: "failProc"}

	_, err := traced.DispatchProcessor(context.Background(), entity, processor, "wf1", "t1", "tx-1")
	if !errors.Is(err, wantErr) {
		t.Errorf("expected %v, got %v", wantErr, err)
	}
}

func TestTracingExternalProcessingService_DispatchCriteria_PropagatesError(t *testing.T) {
	shutdown, _ := observability.Init(context.Background(), "test", "node-test", true)
	defer shutdown(context.Background())

	wantErr := errors.New("criteria failed")
	inner := &fakeDispatcher{returnErr: wantErr}
	traced := observability.NewTracingExternalProcessingService(inner)

	entity := &spi.Entity{Meta: spi.EntityMeta{ID: "ent-4"}}
	criterion := json.RawMessage(`{}`)

	_, err := traced.DispatchCriteria(context.Background(), entity, criterion, "target1", "wf1", "t1", "proc1", "tx-1")
	if !errors.Is(err, wantErr) {
		t.Errorf("expected %v, got %v", wantErr, err)
	}
}
