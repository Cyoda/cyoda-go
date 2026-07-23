package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// funcSchedData is the entity-data shape "sched-fn-resolve" reads to select
// which resolveSchedule result it returns — driven entirely by entity data
// (schedMode/offsetMs/expireOffsetMs) rather than per-call closures, since
// the compute-test-client serves a single, process-lifetime catalog shared
// by every parity scenario (unlike internal/e2e's callback harness, which
// registers a fresh closure per test). See the "sched-fn-resolve" entry
// below for the five supported modes.
type funcSchedData struct {
	SchedMode      string `json:"schedMode"`
	OffsetMs       int64  `json:"offsetMs"`
	ExpireOffsetMs int64  `json:"expireOffsetMs"`
}

// Entity is the compute-test-client's local view of a cyoda entity.
// Decoupled from internal/spi.Entity so this binary builds with
// no internal/ imports.
type Entity struct {
	ID    string          `json:"id"`
	State string          `json:"state"`
	Data  json.RawMessage `json:"data"`

	// AuthType is the executor's principal kind (user|service|system) carried
	// by the dispatch's CloudEvents authtype attribute. Set by the dispatcher
	// from the calc-request CloudEvent, not decoded from entity JSON — hence
	// json:"-". Lets a processor observe the faithful executor kind, including
	// the kind a cross-node forwarded dispatch reconstructs (Task 7).
	AuthType string `json:"-"`
}

// processorFunc is the signature of a registered processor.
type processorFunc func(ctx context.Context, entity *Entity, config json.RawMessage) (*Entity, error)

// criterionFunc is the signature of a registered criterion.
type criterionFunc func(ctx context.Context, entity *Entity, config json.RawMessage) (bool, error)

// functionFunc is the signature of a registered generic Function callout
// (spi.ScheduleFunction — issue #419's scheduled-transition Function
// arm-time timing computation). Returns the response's resultKind
// discriminator and result payload, or an error to have the dispatcher
// reply with a failed EntityFunctionCalculationResponse.
type functionFunc func(ctx context.Context, entity *Entity, config json.RawMessage) (resultKind string, result map[string]any, err error)

// catalog holds the named processors and criteria the compute test
// client serves over gRPC.
type catalog struct {
	processors map[string]processorFunc
	criteria   map[string]criterionFunc
	functions  map[string]functionFunc

	// Callback-capable entries (feature #287): these issue joined HTTP
	// callbacks presenting the calc request's tx-token. cb may be nil when
	// no CYODA_COMPUTE_HTTP_BASE is configured, in which case they fail loudly.
	callbackProcessors map[string]callbackProcessorFunc
	callbackCriteria   map[string]callbackCriterionFunc
	cb                 *callbackClient
}

// newCatalog returns a catalog populated with all registered entries. cb is the
// callback HTTP client used by the #287 callback-join processors/criteria; gcb is
// the gRPC EntityManage callback client used by the cross-node gRPC-callback
// processor. Pass nil for either when the corresponding transport is unconfigured.
func newCatalog(cb *callbackClient, gcb *grpcCallbackClient) *catalog {
	callbackProcs, callbackCrit := newCallbackCatalog(gcb)
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
			// record-authtype — records the executor principal kind observed on
			// the dispatch (Entity.AuthType, from the CloudEvents authtype
			// attribute) into entity data at `observedAuthType`. Used by the
			// cross-node attribution parity scenario to assert a forwarded
			// processor dispatch (A→B) reconstructs the originating executor's
			// true kind on the member-hosting node (Task 7). attachEntity:true
			// is required so entity.Data is present to merge into.
			"record-authtype": func(ctx context.Context, entity *Entity, config json.RawMessage) (*Entity, error) {
				data := map[string]any{}
				if len(entity.Data) > 0 {
					if err := json.Unmarshal(entity.Data, &data); err != nil {
						return nil, err
					}
				}
				data["observedAuthType"] = entity.AuthType
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
		functions: map[string]functionFunc{
			// sched-fn-resolve serves every schedule.function parity scenario
			// (e2e/parity/scheduledfunction). It requires attachEntity:true so
			// entity.Data carries the funcSchedData shape above, and returns
			// the matching resolveSchedule Schedule result:
			//
			//   - "absolute":     fireAt = now + offsetMs (default 300ms)
			//   - "relative":     fireAfterMs = offsetMs (default 300ms)
			//   - "bornExpired":  fireAfterMs = offsetMs (default 1000ms),
			//                     expireAfterMs = 0 — expiry == fire time,
			//                     resolveSchedule's "born expired" branch.
			//   - "pastFire":     fireAt = now - offsetMs (default 5000ms) —
			//                     already due; the real scheduler fires it
			//                     on its very next scan.
			//   - "expiryElapsed": fireAt = now - offsetMs (default 5000ms)
			//                     and expireAt = fireAt + expireOffsetMs
			//                     (default 110ms) — a VALID (non-born-expired)
			//                     arm whose lateness already exceeds
			//                     timeoutMs+grace at arm time, so the very
			//                     first scan expires it deterministically
			//                     regardless of scan cadence (no need to
			//                     race a live scheduler against a real-time
			//                     sleep, unlike internal/e2e's bespoke-clock
			//                     variant of this scenario).
			"sched-fn-resolve": func(ctx context.Context, entity *Entity, config json.RawMessage) (string, map[string]any, error) {
				var data funcSchedData
				if err := json.Unmarshal(entity.Data, &data); err != nil {
					return "", nil, fmt.Errorf("sched-fn-resolve: decode entity data: %w", err)
				}
				now := time.Now().UnixMilli()
				switch data.SchedMode {
				case "absolute":
					offset := data.OffsetMs
					if offset == 0 {
						offset = 300
					}
					return "Schedule", map[string]any{"fireAt": now + offset}, nil
				case "relative":
					offset := data.OffsetMs
					if offset == 0 {
						offset = 300
					}
					return "Schedule", map[string]any{"fireAfterMs": offset}, nil
				case "bornExpired":
					offset := data.OffsetMs
					if offset == 0 {
						offset = 1000
					}
					return "Schedule", map[string]any{"fireAfterMs": offset, "expireAfterMs": int64(0)}, nil
				case "pastFire":
					offset := data.OffsetMs
					if offset == 0 {
						offset = 5000
					}
					return "Schedule", map[string]any{"fireAt": now - offset}, nil
				case "expiryElapsed":
					offset := data.OffsetMs
					if offset == 0 {
						offset = 5000
					}
					expireOffset := data.ExpireOffsetMs
					if expireOffset == 0 {
						expireOffset = 110
					}
					fireAt := now - offset
					return "Schedule", map[string]any{"fireAt": fireAt, "expireAt": fireAt + expireOffset}, nil
				default:
					return "", nil, fmt.Errorf("sched-fn-resolve: unknown schedMode %q", data.SchedMode)
				}
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

func (c *catalog) function(name string) (functionFunc, bool) {
	fn, ok := c.functions[name]
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
