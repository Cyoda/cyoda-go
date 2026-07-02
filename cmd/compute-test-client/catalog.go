package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Entity is the compute-test-client's local view of a cyoda entity.
// Decoupled from internal/spi.Entity so this binary builds with
// no internal/ imports.
type Entity struct {
	ID    string          `json:"id"`
	State string          `json:"state"`
	Data  json.RawMessage `json:"data"`
}

// processorFunc is the signature of a registered processor.
type processorFunc func(ctx context.Context, entity *Entity, config json.RawMessage) (*Entity, error)

// criterionFunc is the signature of a registered criterion.
type criterionFunc func(ctx context.Context, entity *Entity, config json.RawMessage) (bool, error)

// catalog holds the named processors and criteria the compute test
// client serves over gRPC.
type catalog struct {
	processors map[string]processorFunc
	criteria   map[string]criterionFunc

	// Callback-capable entries (feature #287): these issue joined HTTP
	// callbacks presenting the calc request's tx-token. cb may be nil when
	// no CYODA_COMPUTE_HTTP_BASE is configured, in which case they fail loudly.
	callbackProcessors map[string]callbackProcessorFunc
	callbackCriteria   map[string]callbackCriterionFunc
	cb                 *callbackClient
}

// newCatalog returns a catalog populated with all registered entries. cb is the
// callback HTTP client used by the #287 callback-join processors/criteria; pass
// nil when no HTTP base URL is configured.
func newCatalog(cb *callbackClient) *catalog {
	callbackProcs, callbackCrit := newCallbackCatalog()
	return &catalog{
		callbackProcessors: callbackProcs,
		callbackCriteria:   callbackCrit,
		cb:                 cb,
		processors: map[string]processorFunc{
			"noop": func(ctx context.Context, entity *Entity, config json.RawMessage) (*Entity, error) {
				return entity, nil
			},
			"tag-with-foo": func(ctx context.Context, entity *Entity, config json.RawMessage) (*Entity, error) {
				var data map[string]any
				if err := json.Unmarshal(entity.Data, &data); err != nil {
					return nil, err
				}
				data["tag"] = "foo"
				out, err := json.Marshal(data)
				if err != nil {
					return nil, err
				}
				return &Entity{ID: entity.ID, State: entity.State, Data: out}, nil
			},
			"bump-amount": func(ctx context.Context, entity *Entity, config json.RawMessage) (*Entity, error) {
				var data map[string]any
				if err := json.Unmarshal(entity.Data, &data); err != nil {
					return nil, err
				}
				if a, ok := data["amount"].(float64); ok {
					data["amount"] = a + 1
				}
				out, _ := json.Marshal(data)
				return &Entity{ID: entity.ID, State: entity.State, Data: out}, nil
			},
			"inject-error": func(ctx context.Context, entity *Entity, config json.RawMessage) (*Entity, error) {
				return nil, fmt.Errorf("inject-error: deliberate failure")
			},
			"slow-configurable": func(ctx context.Context, entity *Entity, config json.RawMessage) (*Entity, error) {
				var cfg struct {
					SleepMS int `json:"sleep_ms"`
				}
				_ = json.Unmarshal(config, &cfg)
				if cfg.SleepMS > 0 {
					select {
					case <-time.After(time.Duration(cfg.SleepMS) * time.Millisecond):
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}
				return entity, nil
			},
			// echo-context-to-field — records the pass-through Context
			// (delivered in EntityProcessorCalculationRequest.parameters as a
			// JSON string per the cyoda-go contract) into entity data at
			// `_context` so callers can observe it through the entity HTTP
			// API. Absence of a context surfaces as no field write —
			// distinguishable from the "context was empty string" case via
			// field presence.
			"echo-context-to-field": func(ctx context.Context, entity *Entity, config json.RawMessage) (*Entity, error) {
				var data map[string]any
				if err := json.Unmarshal(entity.Data, &data); err != nil {
					return nil, err
				}
				if len(config) > 0 {
					var ctxStr string
					if err := json.Unmarshal(config, &ctxStr); err != nil {
						return nil, fmt.Errorf("echo-context-to-field: expected JSON-string parameters, got %s: %w", config, err)
					}
					data["_context"] = ctxStr
				}
				out, err := json.Marshal(data)
				if err != nil {
					return nil, err
				}
				return &Entity{ID: entity.ID, State: entity.State, Data: out}, nil
			},
			"set-field": func(ctx context.Context, entity *Entity, config json.RawMessage) (*Entity, error) {
				var cfg struct {
					Field string `json:"field"`
					Value any    `json:"value"`
				}
				if err := json.Unmarshal(config, &cfg); err != nil {
					return nil, fmt.Errorf("set-field config: %w", err)
				}
				var data map[string]any
				if err := json.Unmarshal(entity.Data, &data); err != nil {
					return nil, err
				}
				data[cfg.Field] = cfg.Value
				out, _ := json.Marshal(data)
				return &Entity{ID: entity.ID, State: entity.State, Data: out}, nil
			},
		},
		criteria: map[string]criterionFunc{
			"always-true": func(ctx context.Context, entity *Entity, config json.RawMessage) (bool, error) {
				return true, nil
			},
			"always-false": func(ctx context.Context, entity *Entity, config json.RawMessage) (bool, error) {
				return false, nil
			},
			"amount-gt-100": func(ctx context.Context, entity *Entity, config json.RawMessage) (bool, error) {
				var data map[string]any
				if err := json.Unmarshal(entity.Data, &data); err != nil {
					return false, err
				}
				if a, ok := data["amount"].(float64); ok {
					return a > 100, nil
				}
				return false, nil
			},
			"select-premium": func(ctx context.Context, entity *Entity, config json.RawMessage) (bool, error) {
				var data map[string]any
				if err := json.Unmarshal(entity.Data, &data); err != nil {
					return false, err
				}
				if a, ok := data["amount"].(float64); ok {
					return a > 1000, nil
				}
				return false, nil
			},
			"select-standard": func(ctx context.Context, entity *Entity, config json.RawMessage) (bool, error) {
				return true, nil
			},
			// context-equals — matches when the pass-through Context
			// (delivered in EntityCriteriaCalculationRequest.parameters as a
			// JSON string) equals the literal "match". Anything else
			// (including a missing Context) returns false. Used by the
			// workflow_externalization parity test to assert the criterion
			// path forwards Context faithfully.
			"context-equals": func(ctx context.Context, entity *Entity, config json.RawMessage) (bool, error) {
				if len(config) == 0 {
					return false, nil
				}
				var ctxStr string
				if err := json.Unmarshal(config, &ctxStr); err != nil {
					return false, fmt.Errorf("context-equals: expected JSON-string parameters, got %s: %w", config, err)
				}
				return ctxStr == "match", nil
			},
			"field-equals": func(ctx context.Context, entity *Entity, config json.RawMessage) (bool, error) {
				var cfg struct {
					Field string `json:"field"`
					Value any    `json:"value"`
				}
				if err := json.Unmarshal(config, &cfg); err != nil {
					return false, fmt.Errorf("field-equals config: %w", err)
				}
				var data map[string]any
				if err := json.Unmarshal(entity.Data, &data); err != nil {
					return false, err
				}
				return data[cfg.Field] == cfg.Value, nil
			},
		},
	}
}

func (c *catalog) processor(name string) (processorFunc, bool) {
	fn, ok := c.processors[name]
	return fn, ok
}

func (c *catalog) criterion(name string) (criterionFunc, bool) {
	fn, ok := c.criteria[name]
	return fn, ok
}

func (c *catalog) callbackProcessor(name string) (callbackProcessorFunc, bool) {
	fn, ok := c.callbackProcessors[name]
	return fn, ok
}

func (c *catalog) callbackCriterion(name string) (callbackCriterionFunc, bool) {
	fn, ok := c.callbackCriteria[name]
	return fn, ok
}
