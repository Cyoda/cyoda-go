package workflow

import (
	"bytes"
	"encoding/json"
	"fmt"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go-spi/strictjson"

	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/internal/match"
)

// A criterion that holds a function condition is evaluated as a tree of three
// kinds of node: a local unit (a function-free subtree, or a pre-check), which
// the in-memory matcher evaluates; an engine group (a group that holds a
// function condition), whose members the engine evaluates in declaration order
// and stops as soon as the group's value is known; and a function condition,
// which makes a callout unless its pre-check does not match. A criterion with
// no function condition is one local unit. Import and the engine build the
// same tree, so they accept and refuse the same criteria.
type critNode interface {
	// eval evaluates the node on entity and appends one record per function
	// condition it reaches to rec.
	eval(e *Engine, cc *criterionContext, entity *spi.Entity, rec *[]functionRecord) (bool, error)
	// collect appends the node's local units and function conditions, in
	// declaration order.
	collect(units *[]*critLocal, fns *[]*critFunction)
}

// critLocal is a function-free condition: a maximal function-free subtree of
// the criterion, or a pre-check.
type critLocal struct {
	ptr      string // JSON Pointer of the condition in the criterion
	cond     predicate.Condition
	prepared match.Prepared // set by Engine.prepareLocalUnits
}

// critGroup is a group that holds at least one function condition.
type critGroup struct {
	ptr     string
	op      string // AND, OR or NOT; NOT has exactly one member
	members []critNode
}

// critFunction is one function condition.
type critFunction struct {
	ptr           string
	raw           json.RawMessage // the function condition's own JSON, dispatched as is
	fn            contract.CriterionFunction
	preCheck      *critLocal // nil when the function condition has none
	preCheckValue any        // the pre-check as written, decoded for the audit
}

func (l *critLocal) collect(units *[]*critLocal, _ *[]*critFunction) { *units = append(*units, l) }

func (g *critGroup) collect(units *[]*critLocal, fns *[]*critFunction) {
	for _, m := range g.members {
		m.collect(units, fns)
	}
}

func (f *critFunction) collect(units *[]*critLocal, fns *[]*critFunction) {
	if f.preCheck != nil {
		*units = append(*units, f.preCheck)
	}
	*fns = append(*fns, f)
}

// collectCriterion returns root's local units and function conditions in
// declaration order.
func collectCriterion(root critNode) (units []*critLocal, fns []*critFunction) {
	root.collect(&units, &fns)
	return units, fns
}

// buildCriterionTree splits a parsed criterion at its function conditions.
// It refuses a function condition whose `function` member does not parse
// (a `config` that is not a valid callout config included), a function
// condition without a `function` object or a name, a function condition in
// a pre-check, and a group around a function condition whose operator is not
// AND, OR or NOT-of-one. ptr is cond's JSON Pointer in the criterion ("" for
// the criterion itself).
func buildCriterionTree(cond predicate.Condition, ptr string) (critNode, error) {
	if _, ok := firstFunction(cond, ptr); !ok {
		return &critLocal{ptr: ptr, cond: cond}, nil
	}
	if g, ok := cond.(*predicate.GroupCondition); ok {
		if err := search.ValidateGroupOperator(g.Operator, len(g.Conditions)); err != nil {
			return nil, fmt.Errorf("group%s: %w", atPointer(ptr), err)
		}
		node := &critGroup{ptr: ptr, op: g.Operator, members: make([]critNode, 0, len(g.Conditions))}
		for i, child := range g.Conditions {
			m, err := buildCriterionTree(child, fmt.Sprintf("%s/conditions/%d", ptr, i))
			if err != nil {
				return nil, err
			}
			node.members = append(node.members, m)
		}
		return node, nil
	}
	// firstFunction finds a function condition only in a group or as cond itself.
	return buildFunctionNode(cond.(*predicate.FunctionCondition), ptr)
}

func buildFunctionNode(c *predicate.FunctionCondition, ptr string) (*critFunction, error) {
	fn, present, err := contract.ParseCriterionFunction(c.Raw)
	if err != nil {
		// The decoder's location is relative to this condition; place it
		// under the condition's own, as the condition parser does.
		return nil, fmt.Errorf("function condition: %w", strictjson.Within(err, ptr))
	}
	if !present {
		return nil, fmt.Errorf("function condition%s has no `function` object", atPointer(ptr))
	}
	if fn.Name == "" {
		return nil, fmt.Errorf("function condition%s has no function name", atPointer(ptr))
	}
	node := &critFunction{ptr: ptr, raw: c.Raw, fn: fn}
	if c.PreCheck == nil {
		return node, nil
	}
	pcPtr := ptr + "/function/criterion"
	if at, ok := firstFunction(c.PreCheck, pcPtr); ok {
		return nil, fmt.Errorf("function condition %q%s: its pre-check holds a function condition at %s; a pre-check holds only local conditions",
			fn.Name, atPointer(ptr), at)
	}
	node.preCheck = &critLocal{ptr: pcPtr, cond: c.PreCheck}
	value, decodeErr := decodePreCheck(fn.Criterion)
	if decodeErr != nil {
		return nil, fmt.Errorf("function condition %q%s: %w", fn.Name, atPointer(ptr), decodeErr)
	}
	node.preCheckValue = value
	return node, nil
}

// firstFunction returns the JSON Pointer of the first function condition in
// cond, in declaration order, without entering a function condition's
// pre-check.
func firstFunction(cond predicate.Condition, ptr string) (string, bool) {
	switch c := cond.(type) {
	case *predicate.FunctionCondition:
		return ptr, true
	case *predicate.GroupCondition:
		for i, child := range c.Conditions {
			if at, ok := firstFunction(child, fmt.Sprintf("%s/conditions/%d", ptr, i)); ok {
				return at, true
			}
		}
	}
	return "", false
}

// decodePreCheck decodes the pre-check as written into plain JSON values for
// the audit, keeping numbers exact.
func decodePreCheck(raw json.RawMessage) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("failed to decode the pre-check: %w", err)
	}
	return v, nil
}

// atPointer renders a position for a message: nothing for the criterion
// itself, " at <pointer>" otherwise.
func atPointer(ptr string) string {
	if ptr == "" {
		return ""
	}
	return " at " + ptr
}
