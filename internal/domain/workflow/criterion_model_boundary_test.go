package workflow

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/internal/match"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// Task 7 (spec §5): a query never executes against a field the model does
// not declare. A stored criterion naming an undeclared field must abort the
// evaluation (400 WORKFLOW_FAILED at the HTTP boundary, see
// internal/e2e/workflow_criterion_undeclared_field_test.go for the
// full-stack/rollback proof) rather than silently answering "not satisfied".

// TestEvaluateCriterion_UndeclaredPathAbortsTheSave proves the rule for all
// 26 operators by sampling one from each disposition group: GREATER_THAN
// (Group A — a comparison operator that, before this task, already failed
// closed via internal/match's own "no declared type" guard, since it needs a
// declared type to expand its operand) and IS_NULL/CONTAINS (Group B — a
// null-presence test and a string operator, both declaration-independent at
// the match-kernel level per internal/match/match.go's FieldTypes doc, so
// they answered a real true/false today for a field the model never
// declared). The model here declares only "amount"; the criterion names
// "amonut" — a plausible typo, the exact shape of the defect this task
// closes.
func TestEvaluateCriterion_UndeclaredPathAbortsTheSave(t *testing.T) {
	for _, op := range []string{"GREATER_THAN", "IS_NULL", "CONTAINS"} {
		t.Run(op, func(t *testing.T) {
			engine, factory := setupEngine(t)
			ctx := ctxWithTenant(testTenant)
			ref := spi.ModelRef{EntityName: "person", ModelVersion: "1.0"}
			registerModelFields(t, ctx, factory, ref, map[string]schema.DataType{"amount": schema.Integer})

			entity := makeEntity("e1", ref, map[string]any{"amount": 5})
			entity.Meta.State = "CREATED"

			var value any = "x"
			if op == "GREATER_THAN" {
				value = 3
			}
			criterion := simpleCriterion("$.amonut", op, value)

			_, _, err := engine.evaluateCriterion(criterion, entity, &criterionContext{ctx: ctx})
			if err == nil {
				t.Fatalf("%s on undeclared path $.amonut must abort, got no error", op)
			}
			// The error must NOT be a pre-classified *common.AppError: that
			// would let search.ValidateKnownPaths' own 400 INVALID_FIELD_PATH
			// (correct for /search, which IS about field paths) leak through
			// classifyWorkflowError's `errors.As` fast path unchanged. A
			// criterion failure is a workflow-domain failure — matching the
			// existing unevaluable-criterion contract means it falls through
			// to the uniform 400 WORKFLOW_FAILED catch-all instead, exactly
			// like every other structural criterion fault (an unsupported
			// operator, a type mismatch) already does.
			var appErr *common.AppError
			if errors.As(err, &appErr) {
				t.Fatalf("%s: error must not be a *common.AppError (got code %q); "+
					"it must fall through to the workflow-domain WORKFLOW_FAILED classification", op, appErr.Code)
			}
			// The detail must carry exactly ONE code (WORKFLOW_FAILED, stamped
			// by classifyWorkflowError above this layer) — not two
			// ("WORKFLOW_FAILED: ...: INVALID_FIELD_PATH: ..."). The re-wrap
			// in evaluateCriterion strips search.ValidateKnownPaths' own
			// "INVALID_FIELD_PATH: " prefix (baked into its AppError.Message)
			// before discarding the AppError typing.
			if strings.Contains(err.Error(), "INVALID_FIELD_PATH") {
				t.Errorf("%s: error must not carry the INVALID_FIELD_PATH code prefix (that classification is wrong here, not merely redundant): %v", op, err)
			}
		})
	}
}

// TestEvaluateCriterion_UndeclaredPathLeavesStateUnchanged is the
// save-outcome half of the rule: a transition guarded by a criterion naming
// an undeclared field must not fire, and the entity's state must be left
// exactly as fireTransition's own contract promises on any criterion
// evaluation error (see fireTransition's doc comment) — no partial advance.
func TestEvaluateCriterion_UndeclaredPathLeavesStateUnchanged(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	ref := spi.ModelRef{EntityName: "order", ModelVersion: "1.0"}
	registerModelFields(t, ctx, factory, ref, map[string]schema.DataType{"amount": schema.Integer})

	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "undeclared-field-wf", InitialState: "NONE", Active: true,
		States: map[string]spi.StateDefinition{
			"NONE": {Transitions: []spi.TransitionDefinition{
				{Name: "advance", Next: "ADVANCED", Manual: false,
					Criterion: simpleCriterion("$.amonut", "GREATER_THAN", 3)},
			}},
			"ADVANCED": {},
		},
	}
	saveWorkflow(t, factory, ctx, ref, []spi.WorkflowDefinition{wf})

	entity := makeEntity("e1", ref, map[string]any{"amount": 5})
	_, err := engine.Execute(ctx, entity, "")
	if err == nil {
		t.Fatal("expected the cascade to abort on the undeclared-field criterion")
	}
	if entity.Meta.State == "ADVANCED" {
		t.Fatalf("state must not have advanced past the aborted criterion, got %q", entity.Meta.State)
	}
}

// TestEvaluateCriterion_TemporalMetaUnderTextOperatorIsRefused is the case a
// whole-block gate on the model read would miss: `creationDate CONTAINS
// "2024"` carries NO data path at all (a LifecycleCondition contributes
// nothing to search.ConditionFieldPaths), so gating the model READ correctly
// skips it — but the VALIDATION CALL must still run with a nil model,
// because search.ValidateConditionValueTypes's lifecycle branch
// (validateLifecycleType) is the one check that refuses a text operator on a
// temporal meta field. Without it, this criterion reaches
// internal/match's deliberate temporal-meta never-match guard, which a
// later task's NOT node would invert into matching every entity — the exact
// fail-open this task closes. The model store is made to error so a
// regression that accidentally started reading the model on this path would
// surface as an infra error instead of the expected structural one.
func TestEvaluateCriterion_TemporalMetaUnderTextOperatorIsRefused(t *testing.T) {
	baseFactory := memory.NewStoreFactory()
	t.Cleanup(func() { baseFactory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := baseFactory.NewTransactionManager(uuids)
	factory := &errModelStoreFactory{StoreFactory: baseFactory, err: errors.New("model store down")}
	engine := NewEngine(factory, uuids, txMgr)

	ctx := ctxWithTenant(testTenant)
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1.0"}
	entity := makeEntity("e1", ref, map[string]any{})
	entity.Meta.State = "CREATED"

	_, _, err := engine.evaluateCriterion(lifecycleCriterion("creationDate", "CONTAINS", "2024"), entity, &criterionContext{ctx: ctx})
	if err == nil {
		t.Fatal("a text operator on the temporal meta field creationDate must be refused, not silently non-matched")
	}
	if errors.Is(err, ErrCriterionTypingInfra) {
		t.Fatalf("must be a structural refusal (no model read happened for a lifecycle-only criterion), got infra error: %v", err)
	}
	if !errors.Is(err, search.ErrInvalidCondition) {
		t.Fatalf("expected search.ErrInvalidCondition (unsupported operator on temporal field), got: %v", err)
	}
}

// refreshingModelStore is a minimal ModelStore fake mirroring
// internal/domain/search/path_validate_test.go's fixture of the same name:
// Get returns the head of getQueue (simulating a possibly-stale cached
// descriptor); RefreshAndGet returns the head of refreshQueue and is
// counted, so a test can assert the refresh happened exactly once (the
// bounded refresh path-grammar.md section 6 describes). Once both queues
// are drained, Get falls back to the LAST descriptor a refresh produced —
// mirroring a real caching model store, whose Get keeps serving the
// now-updated cache indefinitely after a RefreshAndGet repopulates it,
// rather than reverting to nothing. evaluateCriterion itself performs only
// ONE read (search.LoadModelNode, consolidated — the fields map it types
// against is derived from that same node, not a second independent read)
// plus, on the rare path where that read misses a path, the ONE bounded
// refresh search.ValidateKnownPaths performs internally; this fallback
// keeps the fixture correct for any OTHER caller in this file that issues
// a Get after a refresh (e.g. a future test), not because evaluateCriterion
// itself needs a second Get today.
type refreshingModelStore struct {
	mu            sync.Mutex
	getQueue      []*spi.ModelDescriptor
	refreshQueue  []*spi.ModelDescriptor
	refreshErr    error // when set, RefreshAndGet returns this instead of consuming refreshQueue
	lastRefreshed *spi.ModelDescriptor
	getCount      int
	refreshCount  int
}

func (s *refreshingModelStore) Get(context.Context, spi.ModelRef) (*spi.ModelDescriptor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.getCount++
	if len(s.getQueue) == 0 {
		if len(s.refreshQueue) > 0 {
			return s.refreshQueue[0], nil
		}
		if s.lastRefreshed != nil {
			return s.lastRefreshed, nil
		}
		return nil, nil
	}
	d := s.getQueue[0]
	s.getQueue = s.getQueue[1:]
	return d, nil
}

func (s *refreshingModelStore) RefreshAndGet(context.Context, spi.ModelRef) (*spi.ModelDescriptor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshCount++
	if s.refreshErr != nil {
		return nil, s.refreshErr
	}
	if len(s.refreshQueue) == 0 {
		return nil, nil
	}
	d := s.refreshQueue[0]
	s.refreshQueue = s.refreshQueue[1:]
	s.lastRefreshed = d
	return d, nil
}

func (s *refreshingModelStore) Save(context.Context, *spi.ModelDescriptor) error { return nil }
func (s *refreshingModelStore) GetAll(context.Context) ([]spi.ModelRef, error)   { return nil, nil }
func (s *refreshingModelStore) Delete(context.Context, spi.ModelRef) error       { return nil }
func (s *refreshingModelStore) Lock(context.Context, spi.ModelRef) error         { return nil }
func (s *refreshingModelStore) Unlock(context.Context, spi.ModelRef) error       { return nil }
func (s *refreshingModelStore) IsLocked(context.Context, spi.ModelRef) (bool, error) {
	return true, nil
}
func (s *refreshingModelStore) SetChangeLevel(context.Context, spi.ModelRef, spi.ChangeLevel) error {
	return nil
}
func (s *refreshingModelStore) ExtendSchema(context.Context, spi.ModelRef, spi.SchemaDelta) error {
	return nil
}

var _ spi.ModelStore = (*refreshingModelStore)(nil)

func (s *refreshingModelStore) RefreshCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refreshCount
}

func (s *refreshingModelStore) GetCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getCount
}

// refreshingModelStoreFactory wraps a StoreFactory but returns a fixed
// ModelStore, mirroring errModelStoreFactory's shape.
type refreshingModelStoreFactory struct {
	spi.StoreFactory
	store spi.ModelStore
}

func (f *refreshingModelStoreFactory) ModelStore(context.Context) (spi.ModelStore, error) {
	return f.store, nil
}

func buildBoundaryDescriptor(t *testing.T, ref spi.ModelRef, fields map[string]schema.DataType) *spi.ModelDescriptor {
	t.Helper()
	node := schema.NewObjectNode()
	for name, dt := range fields {
		node.SetChild(name, schema.NewLeafNode(dt))
	}
	raw, err := schema.Marshal(node)
	if err != nil {
		t.Fatalf("schema.Marshal: %v", err)
	}
	return &spi.ModelDescriptor{Ref: ref, Schema: raw}
}

// TestEvaluateCriterion_PathAddedByAPeerIsNotRefused proves the bounded
// refresh half of the rule: a path absent from the CACHED schema but present
// in the AUTHORITATIVE (post-RefreshAndGet) schema — the shape of a cluster
// peer having just extended the model — is not refused. Exactly one
// RefreshAndGet call must happen, and the criterion must evaluate normally
// (using the refreshed declared type) rather than being rejected as unknown.
func TestEvaluateCriterion_PathAddedByAPeerIsNotRefused(t *testing.T) {
	baseFactory := memory.NewStoreFactory()
	t.Cleanup(func() { baseFactory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := baseFactory.NewTransactionManager(uuids)

	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1.0"}
	stale := buildBoundaryDescriptor(t, ref, map[string]schema.DataType{"a": schema.String})
	fresh := buildBoundaryDescriptor(t, ref, map[string]schema.DataType{"a": schema.String, "peer_field": schema.Integer})

	ms := &refreshingModelStore{
		getQueue:     []*spi.ModelDescriptor{stale},
		refreshQueue: []*spi.ModelDescriptor{fresh},
	}
	factory := &refreshingModelStoreFactory{StoreFactory: baseFactory, store: ms}
	engine := NewEngine(factory, uuids, txMgr)

	ctx := ctxWithTenant(testTenant)
	entity := makeEntity("e1", ref, map[string]any{"peer_field": 10})

	got, _, err := engine.evaluateCriterion(simpleCriterion("$.peer_field", "GREATER_THAN", 5), entity, &criterionContext{ctx: ctx})
	if err != nil {
		t.Fatalf("a path a peer node just added must not be refused after one refresh, got: %v", err)
	}
	if !got {
		t.Fatal("peer_field(10) GREATER_THAN 5 must match under the refreshed declared type")
	}
	if rc := ms.RefreshCount(); rc != 1 {
		t.Errorf("expected exactly 1 bounded RefreshAndGet call, got %d", rc)
	}
	// Regression guard for the consolidated single-read shape: evaluateCriterion
	// must issue exactly ONE Get (via search.LoadModelNode) on this path, not
	// two (the old LoadFieldsMap-then-LoadModelNode shape this test's fixture
	// comment used to describe).
	if gc := ms.GetCount(); gc != 1 {
		t.Errorf("expected exactly 1 Get call (the model node is loaded once and the fields map derived from it), got %d", gc)
	}
}

// buildNestedAddressModel saves a model whose schema declares ONLY
// "address.street" (String) — "address" itself is a KNOWN CONTAINER (a
// structural node with substructure) but never a leaf. Used to isolate
// search.ValidateConditionValueTypes' container-vs-scalar check: a
// container path IS accepted by search.ValidateKnownPaths (a container with
// at least one known leaf beneath it counts as "known" — see
// isKnownContainerPath), so ONLY the type-soundness check refuses a scalar
// comparison against it.
func buildNestedAddressModel(t *testing.T, ctx context.Context, factory spi.StoreFactory, ref spi.ModelRef) {
	t.Helper()
	addr := schema.NewObjectNode()
	addr.SetChild("street", schema.NewLeafNode(schema.String))
	root := schema.NewObjectNode()
	root.SetChild("address", addr)
	raw, err := schema.Marshal(root)
	if err != nil {
		t.Fatalf("schema.Marshal: %v", err)
	}
	ms, err := factory.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	if err := ms.Save(ctx, &spi.ModelDescriptor{Ref: ref, Schema: raw}); err != nil {
		t.Fatalf("Save model: %v", err)
	}
}

// TestEvaluateCriterion_ContainerPathScalarComparisonAbortsTheSave covers the
// search.ValidateConditionValueTypes matrix row: a scalar-carrying operator
// (CONTAINS) compared against a path the model declares as a CONTAINER (an
// object with substructure, never itself a leaf) must abort — "$.address"
// is a JSON object in the entity's data, not a string to substring-match.
//
// This is NOT the same gap the undeclared-path checks close: the model DOES
// declare "$.address" (as a container), so search.ValidateKnownPaths accepts
// it (a container with a known leaf beneath it counts as known). And
// match.Prepare does not reject it either — CONTAINS is one of the
// declaration-independent string operators (internal/match/match.go's
// FieldTypes doc), so Prepare builds the leaf fine regardless of declared
// types, and Match() would silently answer non-match at runtime (the
// container value never string-matches). Only
// search.ValidateConditionValueTypes's container-vs-scalar check catches
// this — deleting its call site in evaluateCriterion makes this test fail
// (verified: see task 7 fix-round-1 report).
func TestEvaluateCriterion_ContainerPathScalarComparisonAbortsTheSave(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1.0"}
	buildNestedAddressModel(t, ctx, factory, ref)

	entity := makeEntity("e1", ref, map[string]any{"address": map[string]any{"street": "Main St"}})

	_, _, err := engine.evaluateCriterion(simpleCriterion("$.address", "CONTAINS", "Main"), entity, &criterionContext{ctx: ctx})
	if err == nil {
		t.Fatal("CONTAINS against a container path ($.address has substructure, not a scalar) must abort, got no error")
	}
}

// TestEvaluateCriterion_UncompilablePatternAbortsTheSave covers the
// search.ValidatePatterns matrix row: a stored criterion carrying an
// operand the pattern kernel cannot compile (a LIKE glob with a trailing
// unpaired escape) must abort, not silently evaluate to a permanent
// non-match.
//
// match.Prepare ALSO rejects this specific case today — internal/match's
// expandNamed re-validates a LIKE/MATCHES_PATTERN operand via
// spi.ValidateLeafPattern after calling spi.ExpandLeaf, closing the exact
// swallow ExpandLeaf's own doc comment describes ("Callers wanting a
// rejection ask ValidateLeafPattern FIRST") — a fail-open fix that landed
// ahead of this task. So `err == nil` alone would NOT distinguish the two
// checks: deleting search.ValidatePatterns' call site still leaves an
// error, from match.Prepare's own errUnevaluableLeaf path (verified: see
// task 7 fix-round-1 report). The distinguishing assertion is the
// classification: search.ValidatePatterns' error names the jsonPath and is
// a PLAIN error (never wraps match.ErrUnevaluableLeaf) — that is precisely
// the "only the diagnostic changes" the reviewer described, and pinning it
// is what the spec's coverage-matrix row requires regardless of whether a
// deeper layer is also, today, defense-in-depth. The field is declared and
// correctly typed so the OTHER two checks (ValidateKnownPaths,
// ValidateConditionValueTypes) have nothing to say about this criterion.
func TestEvaluateCriterion_UncompilablePatternAbortsTheSave(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1.0"}
	registerModelFields(t, ctx, factory, ref, map[string]schema.DataType{"name": schema.String})

	entity := makeEntity("e1", ref, map[string]any{"name": "Alice"})

	_, _, err := engine.evaluateCriterion(simpleCriterion("$.name", "LIKE", `a\`), entity, &criterionContext{ctx: ctx})
	if err == nil {
		t.Fatal("an uncompilable LIKE pattern (trailing unpaired escape) must abort, got no error")
	}
	if errors.Is(err, match.ErrUnevaluableLeaf) {
		t.Fatalf("expected search.ValidatePatterns' own classification (plain error naming the jsonPath), "+
			"got match.Prepare's errUnevaluableLeaf instead — search.ValidatePatterns' call site was skipped: %v", err)
	}
	if !strings.Contains(err.Error(), "$.name") {
		t.Errorf("expected search.ValidatePatterns' error to name the jsonPath, got: %v", err)
	}
}

// TestEvaluateCriterion_FailedRefreshIsInfraNotClientFault pins review round
// 1's Important-1 finding: a failed bounded schema refresh (the store's Get
// serves a warm cache, but RefreshAndGet itself errors) cannot tell "this
// field is genuinely undeclared" from "the cache is merely stale and we
// couldn't confirm which". Per correctness-over-availability that ambiguity
// is infrastructure, not a client fault, so it must be classified alongside
// every other criterion-typing infra failure — ErrCriterionTypingInfra — and
// the underlying cause must stay reachable via errors.Is (a
// context.DeadlineExceeded riding inside the refresh failure must still be
// detectable further up the chain), not severed the way the genuine
// unknown-path 400 case's message is.
func TestEvaluateCriterion_FailedRefreshIsInfraNotClientFault(t *testing.T) {
	baseFactory := memory.NewStoreFactory()
	t.Cleanup(func() { baseFactory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := baseFactory.NewTransactionManager(uuids)

	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1.0"}
	stale := buildBoundaryDescriptor(t, ref, map[string]schema.DataType{"a": schema.String})
	wantCause := errors.New("connection refused")

	ms := &refreshingModelStore{
		getQueue:   []*spi.ModelDescriptor{stale},
		refreshErr: wantCause,
	}
	factory := &refreshingModelStoreFactory{StoreFactory: baseFactory, store: ms}
	engine := NewEngine(factory, uuids, txMgr)

	ctx := ctxWithTenant(testTenant)
	entity := makeEntity("e1", ref, map[string]any{"peer_field": 10})

	_, _, err := engine.evaluateCriterion(simpleCriterion("$.peer_field", "GREATER_THAN", 5), entity, &criterionContext{ctx: ctx})
	if err == nil {
		t.Fatal("expected an error: the refresh failed, so $.peer_field's status is unknown")
	}
	if !errors.Is(err, ErrCriterionTypingInfra) {
		t.Errorf("expected errors.Is(err, ErrCriterionTypingInfra) — a failed refresh is infra, not a client fault; got: %v", err)
	}
	if !errors.Is(err, search.ErrPathRefreshInfra) {
		t.Errorf("expected errors.Is(err, search.ErrPathRefreshInfra) to still hold in the chain, got: %v", err)
	}
	if !errors.Is(err, wantCause) {
		t.Errorf("expected the underlying refresh cause to stay reachable via errors.Is (chain must not be severed), got: %v", err)
	}
	var appErr *common.AppError
	if errors.As(err, &appErr) {
		t.Errorf("expected a plain error wrapping ErrCriterionTypingInfra (classifyWorkflowError classifies it), got a pre-classified *common.AppError: %+v", appErr)
	}
	if rc := ms.RefreshCount(); rc != 1 {
		t.Errorf("expected exactly 1 bounded RefreshAndGet call, got %d", rc)
	}
	// Regression guard for the consolidated single-read shape: evaluateCriterion
	// must issue exactly ONE Get (via search.LoadModelNode) on this path, not
	// two (the old LoadFieldsMap-then-LoadModelNode shape this test's fixture
	// comment used to describe).
	if gc := ms.GetCount(); gc != 1 {
		t.Errorf("expected exactly 1 Get call (the model node is loaded once and the fields map derived from it), got %d", gc)
	}
}
