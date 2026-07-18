package skeleton

import (
	"context"
	"encoding/json"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

type ExternalProcessingService struct{}

func NewExternalProcessingService() *ExternalProcessingService {
	return &ExternalProcessingService{}
}

func (s *ExternalProcessingService) DispatchProcessor(_ context.Context, entity *spi.Entity, _ spi.ProcessorDefinition, _ string, _ string, _ string) (*spi.Entity, error) {
	return entity, nil
}

func (s *ExternalProcessingService) DispatchCriteria(_ context.Context, _ *spi.Entity, _ json.RawMessage, _ string, _ string, _ string, _ string, _ string) (bool, string, error) {
	return true, "", nil
}

func (s *ExternalProcessingService) DispatchFunction(_ context.Context, _ *spi.Entity, _ spi.ScheduleFunction, _ string, _ string, _ string) (contract.FunctionResult, error) {
	return contract.FunctionResult{}, nil
}
