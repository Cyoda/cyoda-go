package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	spi "github.com/cyoda/cyoda-go-spi"
)

const workflowNamespace = "workflow"

// workflowStore implements spi.WorkflowStore by delegating to a KeyValueStore.
type workflowStore struct {
	kv spi.KeyValueStore
}

func (s *workflowStore) Save(ctx context.Context, modelRef spi.ModelRef, workflows []spi.WorkflowDefinition) error {
	data, err := json.Marshal(workflows)
	if err != nil {
		return fmt.Errorf("failed to marshal workflows: %w", err)
	}
	return s.kv.Put(ctx, workflowNamespace, modelRef.String(), data)
}

func (s *workflowStore) Get(ctx context.Context, modelRef spi.ModelRef) ([]spi.WorkflowDefinition, error) {
	data, err := s.kv.Get(ctx, workflowNamespace, modelRef.String())
	if errors.Is(err, spi.ErrNotFound) {
		// No workflows stored for this model — return empty slice (not an error).
		return []spi.WorkflowDefinition{}, nil
	} else if err != nil {
		return nil, fmt.Errorf("failed to load workflows for model %s: %w", modelRef, err)
	}
	var wfs []spi.WorkflowDefinition
	if err := json.Unmarshal(data, &wfs); err != nil {
		return nil, fmt.Errorf("failed to unmarshal workflows for model %s: %w", modelRef, err)
	}
	return wfs, nil
}

func (s *workflowStore) Delete(ctx context.Context, modelRef spi.ModelRef) error {
	return s.kv.Delete(ctx, workflowNamespace, modelRef.String())
}
