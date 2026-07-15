package skeleton

import (
	"context"
	"encoding/json"

	spi "github.com/cyoda-platform/cyoda-go-spi"
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
