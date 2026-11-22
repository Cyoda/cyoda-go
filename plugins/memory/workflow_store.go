package memory

import (
	"context"
	"encoding/json"
	"fmt"

	spi "github.com/cyoda/cyoda-go-spi"
)

type WorkflowStore struct {
	tenant  spi.TenantID
	factory *StoreFactory
}

func (s *WorkflowStore) Save(ctx context.Context, modelRef spi.ModelRef, workflows []spi.WorkflowDefinition) error {
	s.factory.wfMu.Lock()
	defer s.factory.wfMu.Unlock()
	if s.factory.wfData[s.tenant] == nil {
		s.factory.wfData[s.tenant] = make(map[spi.ModelRef][]spi.WorkflowDefinition)
	}
	cp, err := copyWorkflows(workflows)
	if err != nil {
		return fmt.Errorf("failed to copy workflows: %w", err)
	}
	s.factory.wfData[s.tenant][modelRef] = cp
	return nil
}

func (s *WorkflowStore) Get(ctx context.Context, modelRef spi.ModelRef) ([]spi.WorkflowDefinition, error) {
	s.factory.wfMu.RLock()
	defer s.factory.wfMu.RUnlock()
	tenantData, ok := s.factory.wfData[s.tenant]
	if !ok {
		return []spi.WorkflowDefinition{}, nil
	}
	wfs, ok := tenantData[modelRef]
	if !ok {
		return []spi.WorkflowDefinition{}, nil
	}
	cp, err := copyWorkflows(wfs)
	if err != nil {
		return nil, fmt.Errorf("failed to copy workflows: %w", err)
	}
	return cp, nil
}

func (s *WorkflowStore) Delete(ctx context.Context, modelRef spi.ModelRef) error {
	s.factory.wfMu.Lock()
	defer s.factory.wfMu.Unlock()
	if tenantData, ok := s.factory.wfData[s.tenant]; ok {
		delete(tenantData, modelRef)
	}
	return nil
}

// copyWorkflows performs a deep copy of workflow definitions via JSON round-trip.
func copyWorkflows(wfs []spi.WorkflowDefinition) ([]spi.WorkflowDefinition, error) {
	data, err := json.Marshal(wfs)
	if err != nil {
		return nil, err
	}
	var out []spi.WorkflowDefinition
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}
