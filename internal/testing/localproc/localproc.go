// Package localproc provides an in-process ExternalProcessingService for testing.
// Processors and criteria are registered as Go function callbacks, enabling
// deterministic workflow testing without gRPC or network I/O.
package localproc

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// ProcessorFunc is a callback invoked when a processor is dispatched.
type ProcessorFunc func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition) (*spi.Entity, error)

// CriteriaFunc is a callback invoked when a function criterion is evaluated.
type CriteriaFunc func(ctx context.Context, entity *spi.Entity, criterion json.RawMessage) (bool, error)

// criteriaFuncR is the internal reason-returning criterion form.
type criteriaFuncR func(ctx context.Context, entity *spi.Entity, criterion json.RawMessage) (bool, string, error)

// LocalProcessingService dispatches processors and criteria to registered
// Go function callbacks. It implements contract.ExternalProcessingService.
type LocalProcessingService struct {
	mu             sync.RWMutex
	processors     map[string]ProcessorFunc
	criteria       map[string]criteriaFuncR
	processorCalls map[string]*atomic.Int64
	criteriaCalls  map[string]*atomic.Int64
}

// New creates a new LocalProcessingService.
func New() *LocalProcessingService {
	return &LocalProcessingService{
		processors:     make(map[string]ProcessorFunc),
		criteria:       make(map[string]criteriaFuncR),
		processorCalls: make(map[string]*atomic.Int64),
		criteriaCalls:  make(map[string]*atomic.Int64),
	}
}

// RegisterProcessor registers a processor callback by name.
func (s *LocalProcessingService) RegisterProcessor(name string, fn ProcessorFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.processors[name] = fn
	if _, ok := s.processorCalls[name]; !ok {
		s.processorCalls[name] = &atomic.Int64{}
	}
}

// RegisterCriteria registers a criteria callback by function name (no reason).
func (s *LocalProcessingService) RegisterCriteria(name string, fn CriteriaFunc) {
	s.RegisterCriteriaReason(name, func(ctx context.Context, e *spi.Entity, c json.RawMessage) (bool, string, error) {
		m, err := fn(ctx, e, c)
		return m, "", err
	})
}

// RegisterCriteriaReason registers a criteria callback that also returns a reason.
func (s *LocalProcessingService) RegisterCriteriaReason(name string, fn criteriaFuncR) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.criteria[name] = fn
	if _, ok := s.criteriaCalls[name]; !ok {
		s.criteriaCalls[name] = &atomic.Int64{}
	}
}

// DispatchProcessor dispatches to the registered processor callback.
// Panics in callbacks are recovered and returned as errors.
func (s *LocalProcessingService) DispatchProcessor(ctx context.Context, entity *spi.Entity, processor spi.ProcessorDefinition, workflowName string, transitionName string, txID string) (result *spi.Entity, err error) {
	s.mu.RLock()
	fn, ok := s.processors[processor.Name]
	counter := s.processorCalls[processor.Name]
	s.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("no local processor registered for %q", processor.Name)
	}
	counter.Add(1)

	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("processor %q panicked: %v", processor.Name, r)
		}
	}()
	return fn(ctx, entity, processor)
}

// DispatchCriteria dispatches to the registered criteria callback.
// It parses the function name from the criterion JSON envelope.
func (s *LocalProcessingService) DispatchCriteria(ctx context.Context, entity *spi.Entity, criterion json.RawMessage, target string, workflowName string, transitionName string, processorName string, txID string) (bool, string, error) {
	var parsed struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(criterion, &parsed); err != nil {
		return false, "", fmt.Errorf("invalid criterion JSON: %w", err)
	}
	name := parsed.Function.Name

	s.mu.RLock()
	fn, ok := s.criteria[name]
	counter := s.criteriaCalls[name]
	s.mu.RUnlock()

	if !ok {
		return false, "", fmt.Errorf("no local criteria registered for %q", name)
	}
	counter.Add(1)
	return fn(ctx, entity, criterion)
}

// ProcessorCallCount returns how many times a processor was invoked.
func (s *LocalProcessingService) ProcessorCallCount(name string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if c, ok := s.processorCalls[name]; ok {
		return int(c.Load())
	}
	return 0
}

// CriteriaCallCount returns how many times a criteria function was invoked.
func (s *LocalProcessingService) CriteriaCallCount(name string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if c, ok := s.criteriaCalls[name]; ok {
		return int(c.Load())
	}
	return 0
}

// Reset clears all invocation counters but keeps registrations.
func (s *LocalProcessingService) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.processorCalls {
		c.Store(0)
	}
	for _, c := range s.criteriaCalls {
		c.Store(0)
	}
}
