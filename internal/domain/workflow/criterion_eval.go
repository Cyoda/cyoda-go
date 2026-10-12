package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/internal/fence"
	"github.com/cyoda-platform/cyoda-go/internal/match"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
)

// criterionOutcome is the result of evaluating one criterion.
type criterionOutcome struct {
	matched bool
	// reason is the compute member's reason when the criterion is one
	// function condition and it was called; "" otherwise.
	reason string
	// functions holds one record per function condition the evaluation
	// reached, in evaluation order. nil when the criterion holds none.
	functions []functionRecord
	// whole reports that the criterion is a single function condition.
	whole bool
}

// functionRecord is what happened to one function condition.
type functionRecord struct {
	path            string // JSON Pointer in the criterion; "" for a whole-criterion function
	name            string
	hasPreCheck     bool
	preCheck        any // the pre-check as written
	preCheckMatched bool
	called          bool
	matches         bool
	reason          string // the member's reason, capped
}

func (l *critLocal) eval(_ *Engine, _ *criterionContext, entity *spi.Entity, _ *[]functionRecord) (bool, error) {
	return l.prepared.Match(entity.Data, entity.Meta), nil
}

// eval evaluates the members in declaration order and stops as soon as the
// group's value is known: AND at the first false member, OR at the first
// true one. Criteria and functions have no effects, so stopping changes
// which callouts are made, never the value.
func (g *critGroup) eval(e *Engine, cc *criterionContext, entity *spi.Entity, rec *[]functionRecord) (bool, error) {
	if g.op == "NOT" {
		v, err := g.members[0].eval(e, cc, entity, rec)
		if err != nil {
			return false, err
		}
		return !v, nil
	}
	stopOn := g.op == "OR"
	for _, m := range g.members {
		v, err := m.eval(e, cc, entity, rec)
		if err != nil {
			return false, err
		}
		if v == stopOn {
			return stopOn, nil
		}
	}
	return !stopOn, nil
}

// eval makes the callout unless the pre-check does not match, in which case
// the function condition is false.
func (f *critFunction) eval(e *Engine, cc *criterionContext, entity *spi.Entity, rec *[]functionRecord) (bool, error) {
	r := functionRecord{path: f.ptr, name: f.fn.Name}
	if f.preCheck != nil {
		r.hasPreCheck, r.preCheck = true, f.preCheckValue
		r.preCheckMatched = f.preCheck.prepared.Match(entity.Data, entity.Meta)
		if !r.preCheckMatched {
			*rec = append(*rec, r)
			return false, nil
		}
	}
	matches, reason, err := e.dispatchCriterionFunction(cc, entity, f.raw)
	if err != nil {
		return false, err
	}
	r.called, r.matches, r.reason = true, matches, capReason(reason)
	*rec = append(*rec, r)
	return matches, nil
}

// dispatchCriterionFunction makes one criterion callout with the function
// condition's own JSON.
func (e *Engine) dispatchCriterionFunction(cc *criterionContext, entity *spi.Entity, raw json.RawMessage) (bool, string, error) {
	// Release any per-tx gate this call chain holds across the blocking
	// FUNCTION-criterion dispatch — same H3 rationale as executeSyncProcessor:
	// the callout can re-enter with a descendant joined callback on the same
	// txID. No-op for the owner / non-joined calls. The explicit resume
	// re-acquires the lock so the fence's check below is made while holding
	// it, which is what gives the chain the right to carry on; the deferred
	// one covers a panicking dispatch.
	resume := txgate.Suspend(cc.ctx)
	defer resume()
	callCtx, stop := runCallCtx(cc.ctx)
	defer stop()
	matches, reason, err := e.extProc.DispatchCriteria(callCtx, entity, raw, cc.target, cc.workflowName, cc.transitionName, "", cc.txID)
	stop()
	resume()
	if cerr := fence.Check(cc.ctx); cerr != nil {
		return false, "", cerr
	}
	return matches, reason, err
}

// prepareLocalUnits checks every local unit of a criterion against the
// entity's model and prepares it, before any callout: a fault in any unit —
// also one evaluation would not reach — fails the criterion.
func (e *Engine) prepareLocalUnits(ctx context.Context, entity *spi.Entity, units []*critLocal) error {
	if len(units) == 0 {
		return nil
	}

	// Type-directed evaluation: the predicate kernel compares data leaves by
	// their declared model types (a temporal data field compares temporally,
	// consistent with the search path). The model READ is gated on the
	// condition carrying at least one data-field path (search.ConditionFieldPaths
	// walks the WHOLE tree up front, not just as far as Prepare's own walk
	// reaches) — a purely lifecycle criterion never touches the model store.
	// This is deliberately EAGER rather than the previous lazily-triggered-
	// during-Prepare load: computing the path set over the whole tree first
	// makes the infra-failure precedence below hold regardless of which child
	// of a group a structural fault happens to live in (see the note on
	// loadErr below).
	//
	// A load failure on a criterion that DOES reference data leaves is
	// surfaced (fail closed): the model schema is a required input for
	// correct typing, so we reject rather than silently mis-evaluate.
	seen := make(map[string]struct{})
	var paths []string
	for _, u := range units {
		for _, p := range search.ConditionFieldPaths(u.cond) {
			if _, ok := seen[p]; !ok {
				seen[p] = struct{}{}
				paths = append(paths, p)
			}
		}
	}

	var loadErr error
	var pathErr error
	var fields map[string]schema.FieldDescriptor

	if len(paths) > 0 {
		modelStore, err := e.factory.ModelStore(ctx)
		if err != nil {
			loadErr = fmt.Errorf("%w: model store unavailable: %w", ErrCriterionTypingInfra, err)
		} else if fm, err := search.LoadFieldsMap(ctx, modelStore, entity.Meta.ModelRef); err != nil {
			loadErr = fmt.Errorf("%w: model %s/%s: %w", ErrCriterionTypingInfra,
				entity.Meta.ModelRef.EntityName, entity.Meta.ModelRef.ModelVersion, err)
		} else {
			// ONE read: the fields map feeds the path check below, the
			// type-soundness check (ValidateConditionValueTypes) and
			// Prepare's fieldTypes, so all three type against the same
			// schema snapshot.
			fields = fm
			// A query never executes against a field the model does not
			// declare (ruling, spec §5): hold every path the criterion
			// names to the model's declared fields. This carries the one
			// bounded schema refresh a cluster needs (path-grammar.md
			// §6) — a field a peer node just added is not falsely
			// refused. That refresh is itself a SECOND read, but only on
			// the rare path where the first one missed a path — the common
			// case above pays for exactly one. The (possibly refreshed)
			// fields map is what feeds both the type-soundness check and
			// fieldTypes below, so the match evaluation and the type check
			// see the same authoritative schema this check validated
			// against. Typing against the pre-refresh schema instead would
			// leave a path a peer just added with no declared type, and
			// skip the check that refuses an operator its type cannot
			// satisfy.
			fields, pathErr = search.ValidateKnownPaths(ctx, modelStore, entity.Meta.ModelRef, paths, fields)
			if errors.Is(pathErr, search.ErrPathRefreshInfra) {
				// The bounded refresh itself failed — RefreshAndGet errored
				// for a reason other than the model being legitimately
				// deleted. That failure cannot tell "this field is
				// genuinely undeclared" from "the cache is merely stale",
				// so per correctness-over-availability it is infrastructure,
				// not the caller's fault. Fold it into loadErr (wrapped with
				// %w on both sides, so the underlying cause — a
				// context.DeadlineExceeded included — stays reachable via
				// errors.Is for classifyWorkflowError's own cancellation
				// branch and, beyond it, common.ClassifyRequestTimeout) so
				// it shares loadErr's precedence: infra wins over every
				// structural fault below, exactly like a model-store outage
				// on the read above.
				loadErr = fmt.Errorf("%w: %w", ErrCriterionTypingInfra, pathErr)
				pathErr = nil
			}
		}
	}

	fieldTypes := func(p string) []spi.DataType {
		if fd, ok := fields[p]; ok {
			return fd.Types
		}
		return nil
	}

	// If an infra failure was observed, it wins: a model-store outage — on
	// either read, or on the bounded refresh ValidateKnownPaths performs —
	// is a server-side condition and must not surface as a client error just
	// because the same criterion also carries a malformed operator or an
	// undeclared field. Unlike the previous lazily-triggered load, paths is
	// now computed over the WHOLE tree before Prepare ever runs, so this
	// precedence holds regardless of which child of a group carries the data
	// leaf versus the structural fault.
	if loadErr != nil {
		return loadErr
	}

	// Model-boundary validation, in the same order the search HTTP boundary
	// enforces it (path existence, then pattern compilability, then declared
	// type/operator soundness):
	//
	//  1. ValidateKnownPaths (above) — the field itself must be declared.
	//  2. ValidatePatterns — a MATCHES_PATTERN/LIKE operand the kernel cannot
	//     compile is rejected regardless of whether the field is typed; a
	//     workflow stored before pattern validation existed is never
	//     re-validated at import, so this must run at evaluation. Not
	//     redundant with match.Prepare below: for the string-operator family
	//     (LIKE/MATCHES_PATTERN included), the SPI kernel's ExpandLeaf
	//     deliberately SWALLOWS a pattern-compile failure into a leaf that
	//     never matches (its own doc comment: "Callers wanting a rejection
	//     ask ValidateLeafPattern FIRST") — so Prepare alone would silently
	//     never-match rather than abort.
	//  3. ValidateConditionValueTypes — ALWAYS called, with fields nil when
	//     the condition carries no data path. This is deliberate: gating the
	//     CALL (rather than just the model READ, which is correctly gated
	//     above on len(paths)>0) would leave validateLifecycleType unreached,
	//     and that is the one check that refuses a text/pattern operator on
	//     a temporal meta field (e.g. `creationDate CONTAINS "2024"`, which
	//     carries no data path at all). Without it, such a criterion would
	//     reach match.Prepare, which tests the instant's RFC3339 text — and
	//     a NOT node would select every entity whose text does not match.
	//     Nor is it redundant with Prepare for a data leaf: Prepare evaluates
	//     a string operator on a numeric field by polarity, so only this
	//     check refuses an operator the field's type cannot satisfy. And a
	//     KNOWN CONTAINER path (declared, but only via a nested leaf beneath
	//     it) compared with a scalar-carrying operator like CONTAINS is
	//     accepted by ValidateKnownPaths (a container with a known leaf
	//     beneath it counts as known) and by Prepare (CONTAINS is
	//     declaration-independent), but is not a sound comparison — only
	//     this check's container-vs-scalar guard refuses it.
	//
	// pathErr, when it names a genuinely unknown path, is re-wrapped as a
	// plain error rather than returned as-is: search.ValidateKnownPaths
	// hands back an already-classified *common.AppError (400
	// INVALID_FIELD_PATH), the correct code for its other callers —
	// SearchService and grouped stats, where the client's request IS a field
	// path. Here the client asked to save/transition an entity and knows
	// nothing about the criterion's internal JSONPath; classifyWorkflowError
	// checks errors.As(*common.AppError) FIRST and would otherwise let that
	// unrelated code leak through unchanged instead of falling to the
	// uniform 400 WORKFLOW_FAILED every other criterion structural fault
	// gets (the "existing unevaluable-criterion contract"). The AppError's
	// Message already carries its own "INVALID_FIELD_PATH: " code prefix
	// (common.Operational bakes the code into Message); that prefix is
	// stripped so the re-wrapped detail does not carry two codes
	// ("WORKFLOW_FAILED: ...: INVALID_FIELD_PATH: ...") — only the domain
	// detail (which names the offending path) survives.
	if pathErr != nil {
		var appErr *common.AppError
		if errors.As(pathErr, &appErr) {
			return errors.New(strings.TrimPrefix(appErr.Message, appErr.Code+": "))
		}
		return errors.New(pathErr.Error())
	}
	for _, u := range units {
		if err := search.ValidatePatterns(u.cond); err != nil {
			return err
		}
	}
	for _, u := range units {
		if err := search.ValidateConditionValueTypes(fields, u.cond); err != nil {
			return err
		}
	}

	// Prepared only now, after every model-boundary refusal above has
	// cleared: a criterion the model-boundary checks would abort is never
	// even compiled, just to be discarded. Every structural fault the
	// criterion carries surfaces here, from the condition's own shape,
	// rather than from whichever entity happens to reach it — so a
	// criterion that cannot be evaluated fails the transition instead of
	// being silently read as "not satisfied".
	for _, u := range units {
		prepared, prepErr := match.Prepare(u.cond, fieldTypes)
		if prepErr != nil {
			return prepErr
		}
		u.prepared = prepared
	}
	return nil
}
