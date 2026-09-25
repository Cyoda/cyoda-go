// Package taskconflict gives tests a scheduled-task store that refuses
// chosen calls the way first-committer-wins does (spi.ErrConflict), and
// otherwise delegates to the real store. A test uses it to put a task-row
// conflict exactly where it wants one. It counts the calls, so a test can
// assert how many retries ran.
//
// Only _test.go files import it.
package taskconflict

import (
	"context"
	"fmt"
	"sync"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// Method names a ScheduledTaskStore method a Plan can refuse.
type Method string

const (
	DeleteForEntities  Method = "DeleteForEntities"
	DeleteForModel     Method = "DeleteForModel"
	ReconcileForEntity Method = "ReconcileForEntity"
)

// Plan says which calls fail, and counts every call. Safe for concurrent use.
type Plan struct {
	mu     sync.Mutex
	refuse map[Method]int
	fail   map[Method]error
	calls  map[Method]int
}

// NewPlan returns a plan that refuses nothing.
func NewPlan() *Plan {
	return &Plan{refuse: map[Method]int{}, fail: map[Method]error{}, calls: map[Method]int{}}
}

// Refuse makes the next n calls of m fail with a conflict.
func (p *Plan) Refuse(m Method, n int) *Plan {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refuse[m] = n
	return p
}

// Fail makes every later call of m fail with err.
func (p *Plan) Fail(m Method, err error) *Plan {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fail[m] = err
	return p
}

// Reset removes every refusal and failure. The counts stay.
func (p *Plan) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refuse = map[Method]int{}
	p.fail = map[Method]error{}
}

// Calls reports how many times m was called.
func (p *Plan) Calls(m Method) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[m]
}

func (p *Plan) next(m Method) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls[m]++
	if err := p.fail[m]; err != nil {
		return err
	}
	if p.refuse[m] > 0 {
		p.refuse[m]--
		return fmt.Errorf("taskconflict: %s: task row written after this transaction began: %w", m, spi.ErrConflict)
	}
	return nil
}

// Factory is a StoreFactory whose ScheduledTaskStore applies Plan.
type Factory struct {
	spi.StoreFactory
	Plan *Plan
}

// ScheduledTaskStore wraps the real store of the embedded factory.
func (f *Factory) ScheduledTaskStore(ctx context.Context) (spi.ScheduledTaskStore, error) {
	inner, err := f.StoreFactory.ScheduledTaskStore(ctx)
	if err != nil {
		return nil, err
	}
	return &store{ScheduledTaskStore: inner, plan: f.Plan}, nil
}

type store struct {
	spi.ScheduledTaskStore
	plan *Plan
}

func (s *store) DeleteForEntities(ctx context.Context, tenant spi.TenantID, entityIDs []string) error {
	if err := s.plan.next(DeleteForEntities); err != nil {
		return err
	}
	return s.ScheduledTaskStore.DeleteForEntities(ctx, tenant, entityIDs)
}

func (s *store) DeleteForModel(ctx context.Context, tenant spi.TenantID, modelName string, modelVersion int,
	keep func(sourceState, transition string) bool) error {
	if err := s.plan.next(DeleteForModel); err != nil {
		return err
	}
	return s.ScheduledTaskStore.DeleteForModel(ctx, tenant, modelName, modelVersion, keep)
}

func (s *store) ReconcileForEntity(ctx context.Context, req spi.ReconcileRequest) ([]spi.ScheduledTask, error) {
	if err := s.plan.next(ReconcileForEntity); err != nil {
		return nil, err
	}
	return s.ScheduledTaskStore.ReconcileForEntity(ctx, req)
}
