package match

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/tidwall/gjson"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
)

// prepared.go is the prepare/execute split of the predicate-tree evaluator.
// Prepare resolves everything that depends only on the query — declared types,
// operand parsing, type bucketing, regex compilation, and the filter-path
// parse (spi.ParseFilterPath) — into an immutable tree. Match then walks that
// tree per row, resolving each leaf's parsed hops against the row's own data
// through spi.ResolvePath — the one resolver the SPI kernel's PreparedFilter
// also calls (docs/cloud-parity/path-grammar.md section 10).
//
// Prepare and the Filter-side spi.Prepare both fail a leaf they cannot
// evaluate rather than silently building one that never matches — the two
// consume different input types (spi.FilterOp is a closed enum, while
// predicate.Condition carries free-text operator and field names that can
// name nothing), so this package's error set is its own, not a mirror of
// spi.ErrUnevaluableLeaf, but the disposition is the same: an unevaluable
// leaf is a structural fault, not a row-dependent non-match.
//
// Errors are structural properties of the CONDITION, never of the row.
// Exactly one swallow stays a deliberate non-match rather than an error: the
// temporal-meta guard in prepareLifecycle (a text or pattern operator on
// creationDate/lastUpdateTime), pinned as permanent by
// TestPrepare_TemporalMetaGuardStaysANonMatch and
// TestMatch_TemporalMetaField_StringOperatorNeverMatches. Every other
// row-dependent failure stays a non-match, exactly as before.

// errUnsupportedOperator marks an operator NAME with no kernel op — a
// structural fault that fails Prepare. It is deliberately a distinct
// sentinel from errUnevaluableLeaf (an operand that parses into no declared
// type, an empty leaf path, or a path outside the filter-path grammar) so a
// caller that wants to tell "no such operator" apart from "operator exists
// but this leaf cannot be evaluated" can do so with errors.Is.
var errUnsupportedOperator = errors.New("unsupported operator")

// errUnevaluableLeaf marks a leaf that IS a recognised operator but cannot be
// evaluated: an operand that parses into no declared type (or a malformed
// range arity), an empty leaf path, or a jsonPath outside the filter-path
// grammar. Prepare fails closed on all three rather than building a node that
// silently never matches — see prepKind's doc for why a silently-unfinished
// node is exactly the failure mode this guards against.
var errUnevaluableLeaf = errors.New("unevaluable leaf")

// prepKind discriminates the prepared node shapes.
type prepKind int

const (
	// prepNever is the ZERO VALUE on purpose: an unpopulated node, and the
	// zero Prepared that Prepare returns alongside an error, must fail closed.
	//
	// Exactly one node deliberately carries this kind as a real, wired-in
	// value with a nil error: prepareLifecycle's temporal-meta guard (a text
	// or pattern operator on creationDate/lastUpdateTime never matches,
	// permanently — see that function). Everywhere else, prepNever is
	// reached only as the discarded zero value paired with a non-nil error —
	// leafNode and prepareSimple no longer construct a bare
	// prepNode{kind: prepNever} to swallow a failure; they return an error
	// instead, so a half-built node can never be wired into a live tree. The
	// caller either propagates that error immediately (prepareGroup) or, at
	// the top, Prepare itself discards its own result and returns the zero
	// Prepared — which is prepNever by construction and the correct
	// fail-closed answer for a caller that ignores the error.
	prepNever prepKind = iota
	prepGroup
	prepLeaf         // data leaf, addressed by a parsed filter path
	prepMetaString   // lifecycle leaf on a string-valued meta field
	prepMetaTemporal // lifecycle leaf on creationDate / lastUpdateTime
)

// Prepared is a predicate.Condition compiled for repeated evaluation. Build it
// once per query with Prepare, then call Match once per candidate row. It is
// immutable after Prepare returns and safe to share across goroutines.
//
// The zero Prepared never matches.
type Prepared struct {
	root prepNode
}

type prepNode struct {
	kind prepKind

	// prepGroup
	or       bool
	children []prepNode

	// prepLeaf
	hops []spi.PathHop

	// prepMetaString / prepMetaTemporal
	metaField string

	// prepLeaf / prepMetaString / prepMetaTemporal
	exp spi.Expansion
}

// Prepare compiles cond against the declared types fieldTypes supplies.
//
// fieldTypes is consumed during preparation and never retained. Callers whose
// closure mutates captured state (the workflow engine's does) rely on this:
// calling it once, on one goroutine, is what makes the result safe to share.
//
// Declared types are resolved for exactly the leaf set the pre-split evaluator
// resolved — every SimpleCondition and every ArrayCondition, whatever the
// operator, not only the comparison and range leaves that consume them.
// Narrowing the set would stop resolving types for leaves that resolve them
// today, and those are precisely the leaves whose lookup failure currently
// fails a criterion closed. Resolving fewer would be a fail-open movement on
// the write path.
func Prepare(cond predicate.Condition, fieldTypes FieldTypes) (Prepared, error) {
	n, err := prepare(cond, fieldTypes)
	if err != nil {
		return Prepared{}, err
	}
	return Prepared{root: n}, nil
}

// prepare desugars cond before dispatching on its shape, so no evaluator
// below this point ever sees a *predicate.ArrayCondition — spi.DesugarCondition
// rewrites it into an AND of positional EQUALS leaves (see that function's
// doc). This is the same call spi.ConditionToFilter makes at its own entry
// point; one implementation, two entry points, so a bracket-indexed array
// clause means the same thing under a pushdown and under this evaluator.
func prepare(cond predicate.Condition, fieldTypes FieldTypes) (prepNode, error) {
	return prepareDesugared(spi.DesugarCondition(cond), fieldTypes)
}

// prepareDesugared is prepare's dispatch, factored out so prepareGroup can
// recurse into it directly for each already-desugared child rather than
// calling prepare again.
//
// spi.DesugarCondition fully recurses into every descendant of a
// *predicate.GroupCondition in ONE call (its own GroupCondition case walks
// Conditions and desugars each), so by the time prepare reaches this switch,
// cond's WHOLE tree is already desugared — every descendant, not just the
// direct children. A prepareGroup child is therefore always
// already-desugared, and re-running spi.DesugarCondition on it (as calling
// prepare again would) is pure redundant work: it re-walks and
// re-allocates a children slice for every group node in that child's
// subtree, and because this happened once per ancestor level, a depth-D
// chain paid D + (D-1) + ... + 1 = O(D²) group-node revisits instead of
// O(D). Mirrors spi.desugaredToFilter (condition_filter.go), the identical
// fix for spi.ConditionToFilter/groupToFilter. See
// TestPrepare_DesugarIsNotReappliedPerLevel.
func prepareDesugared(cond predicate.Condition, fieldTypes FieldTypes) (prepNode, error) {
	switch c := cond.(type) {
	case *predicate.SimpleCondition:
		return prepareSimple(c, fieldTypes)
	case *predicate.LifecycleCondition:
		return prepareLifecycle(c)
	case *predicate.GroupCondition:
		return prepareGroup(c, fieldTypes)
	case *predicate.FunctionCondition:
		return prepNode{}, fmt.Errorf("function conditions not implemented")
	default:
		return prepNode{}, fmt.Errorf("unknown condition type: %T", cond)
	}
}

// expandNamed maps an operator NAME to its kernel op and expands the operand.
// A name with no kernel op is a structural fault (errUnsupportedOperator);
// anything else the kernel rejects is an expansion failure the caller
// (leafNode) turns into a Prepare error (errUnevaluableLeaf) — an unevaluable
// leaf is a structural fault too, not a leaf that silently never matches.
func expandNamed(operatorType string, value any, declared []spi.DataType) (spi.Expansion, error) {
	op, ok := opNameToFilterOp(operatorType)
	if !ok {
		return spi.Expansion{}, fmt.Errorf("%w: %s", errUnsupportedOperator, operatorType)
	}
	var values []string
	if op == spi.FilterBetween || op == spi.FilterBetweenInclusive {
		values = betweenBounds(value)
	}
	exp, err := spi.ExpandLeaf(op, spi.OperandString(value), values, declared)
	if err != nil {
		return spi.Expansion{}, err
	}
	// spi.ExpandLeaf deliberately swallows a pattern-compile failure for
	// LIKE and MATCHES_PATTERN (its own doc comment): it still returns a
	// leaf, with the built Expansion's matcher left nil (never matches),
	// for callers outside the SPI package that still want that leaf built
	// rather than an error. The SPI kernel's own Prepare closes exactly this
	// swallow (prepared_filter.go) by re-deriving the compile ONLY when it
	// can see, via the Expansion's unexported matcher field, that the first
	// attempt actually failed. This package has no such visibility — the
	// field is private to spi, and Expansion exposes no accessor for it —
	// so it cannot gate the re-derivation on "did it actually fail"; it
	// gates on "is this a pattern operator at all" instead, paying one
	// extra ExpandLeaf-equivalent compile for every LIKE/MATCHES_PATTERN
	// leaf (matching or not) rather than only the ones that fail. That cost
	// is paid once per query at Prepare time, same as every other leaf here
	// — never once per row. spi.ValidateLeafPattern is a no-op (returns nil
	// immediately) for every non-pattern operator, so it is safe to reach
	// for unconditionally once op is known to be one of the two.
	//
	// Left unguarded, a malformed pattern (e.g. an unclosed regex character
	// class, "[") would silently build a leaf that never matches instead of
	// failing Prepare — exactly the fail-open shape this whole task removes,
	// and one a future NOT arm would invert into matching every entity.
	if op == spi.FilterLike || op == spi.FilterMatchesRegex {
		if err := spi.ValidateLeafPattern(op, value); err != nil {
			return spi.Expansion{}, err
		}
	}
	return exp, nil
}

// leafNode builds a prepared leaf of the given kind, or fails Prepare when
// expansion fails. On success, the returned node's kind is always exactly
// kind — leafNode never returns a partially-built node alongside a nil
// error, so a caller checking only the error can rely on that invariant.
func leafNode(kind prepKind, operatorType string, value any, declared []spi.DataType) (prepNode, error) {
	exp, err := expandNamed(operatorType, value, declared)
	if err != nil {
		if errors.Is(err, errUnsupportedOperator) {
			return prepNode{}, err
		}
		// An operand that parses into no declared type, or a malformed range
		// arity, cannot be evaluated: the swallowed expansion error the
		// per-row evaluator used to absorb into a silent non-match, made an
		// explicit Prepare failure instead.
		return prepNode{}, fmt.Errorf("%w: %v", errUnevaluableLeaf, err)
	}
	return prepNode{kind: kind, exp: exp}, nil
}

func prepareSimple(c *predicate.SimpleCondition, fieldTypes FieldTypes) (prepNode, error) {
	var declared []spi.DataType
	if fieldTypes != nil {
		declared = fieldTypes(fieldMapKey(c.JsonPath))
	}
	// leafNode(prepLeaf, ...) either fails or returns a node whose kind is
	// exactly prepLeaf — see leafNode's doc — so nothing below needs to
	// re-check n.kind before touching n.hops.
	n, err := leafNode(prepLeaf, c.OperatorType, c.Value, declared)
	if err != nil {
		return prepNode{}, err
	}
	stripped := stripLeader(c.JsonPath)
	if stripped == "" {
		// An empty path is legal ONLY for a tree operator (AND/OR,
		// handled by prepareGroup and never reaching this function) — it
		// is how a condition spells "addresses no field at all". A LEAF
		// with an empty path (raw "", "$", or "$." — stripLeader collapses
		// all three) addresses no field either, so it cannot be evaluated.
		// Mirrors the SPI kernel's own guard for exactly this case
		// (prepareNode in prepared_filter.go): without it,
		// spi.ParseFilterPath("") legitimately returns (nil, nil) — the
		// shape the tree-operator case is allowed to take — and
		// spi.ResolvePath(data, nil) resolves that nil hop slice to the
		// parsed ROOT DOCUMENT, so a presence test (IS_NULL/NOT_NULL)
		// would match every row regardless of its shape. A STRING
		// operator's exposure is narrower — spi.EvalLeaf's kindStringOp
		// branch requires stored.Type == gjson.String, which an
		// object- or array-rooted document (the normal entity shape)
		// never is, so it stays a non-match there — but for a document
		// whose raw bytes parse to a bare JSON string at the root, the
		// resolved "field" IS that root scalar, and a string operator
		// would substring-match it directly.
		return prepNode{}, fmt.Errorf("%w: leaf addresses no field (empty path)", errUnevaluableLeaf)
	}
	hops, err := spi.ParseFilterPath(stripped)
	if err != nil {
		// A path outside the grammar cannot be evaluated — the same fault
		// class an expansion failure already is, and the same answer the
		// SPI kernel gives a malformed SourceData path (prepareNode in
		// prepared_filter.go).
		return prepNode{}, fmt.Errorf("%w: path %q: %v", errUnevaluableLeaf, c.JsonPath, err)
	}
	n.hops = hops
	return n, nil
}

func prepareLifecycle(c *predicate.LifecycleCondition) (prepNode, error) {
	// Canonicalise BEFORE the field check, or previousTransition — a working
	// field name — starts erroring.
	field := c.Field
	if field == "previousTransition" {
		field = "transitionForLatestSave"
	}

	switch field {
	case "creationDate", "lastUpdateTime":
		// Field-identity guard, sitting in FRONT of the operator check: a
		// temporal field admits only comparison, range and null operators, and
		// anything else is a never-match leaf rather than an error. It must
		// never lexically substring-match the formatted RFC3339 rendering.
		//
		// A string or pattern operator on a temporal field is now rejected at
		// the shared validation boundary (search.validateLifecycleType) —
		// operator-semantics.md §4/§7 — for every VALIDATED entry point. This
		// guard stays regardless, unconditionally, for every caller: a
		// workflow criterion is validated once at import and then stored
		// verbatim, evaluated on every subsequent save by calling this
		// package's Prepare directly with NO revalidation
		// (workflow/engine.go), so a criterion imported before the boundary
		// existed — or any future caller that reaches Prepare without going
		// through the boundary — must still answer never-match, not a lexical
		// substring match. Removing this guard would silently reactivate a
		// dormant criterion's transition, on the binary upgrade alone, for a
		// predicate the system has just declared unsupported. Per
		// .claude/rules/correctness-over-availability.md, never-match is the
		// fail-closed answer here; lexical match is a false positive nobody
		// authored.
		//
		// Do NOT resolve the divergence with the kernel by aligning this
		// evaluator to it. A text or pattern operator on a temporal field is
		// not a supported predicate; the validation boundary is what makes
		// the disagreement unreachable for a validated request, and there is
		// no "two doors" problem to reconcile for a criterion, which never
		// routes through the SPI kernel at all — match.Prepare is and always
		// has been its sole evaluator.
		if !IsTemporalOperator(c.OperatorType) {
			return prepNode{kind: prepNever}, nil
		}
		// leafNode(prepMetaTemporal, ...) either fails or returns a node
		// whose kind is exactly prepMetaTemporal — see leafNode's doc.
		n, err := leafNode(prepMetaTemporal, c.OperatorType, c.Value, []spi.DataType{spi.ZonedDateTime})
		if err != nil {
			return prepNode{}, err
		}
		n.metaField = field
		return n, nil

	case "state", "transitionForLatestSave", "transactionId", "id":
		// leafNode(prepMetaString, ...) either fails or returns a node whose
		// kind is exactly prepMetaString — see leafNode's doc.
		n, err := leafNode(prepMetaString, c.OperatorType, c.Value, []spi.DataType{spi.String})
		if err != nil {
			return prepNode{}, err
		}
		n.metaField = field
		return n, nil

	default:
		// Reported with the ORIGINAL field name, not the canonicalised one.
		return prepNode{}, fmt.Errorf("unknown lifecycle field: %s", c.Field)
	}
}

func prepareGroup(c *predicate.GroupCondition, fieldTypes FieldTypes) (prepNode, error) {
	var or bool
	switch c.Operator {
	case "AND":
	case "OR":
		or = true
	default:
		return prepNode{}, fmt.Errorf("unknown group operator: %s", c.Operator)
	}

	n := prepNode{kind: prepGroup, or: or}
	if len(c.Conditions) > 0 {
		n.children = make([]prepNode, 0, len(c.Conditions))
		for _, child := range c.Conditions {
			// child is already desugared — see prepareDesugared's doc — so
			// this calls the dispatch directly rather than prepare, which
			// would re-run spi.DesugarCondition on it.
			cn, err := prepareDesugared(child, fieldTypes)
			if err != nil {
				return prepNode{}, err
			}
			n.children = append(n.children, cn)
		}
	}
	return n, nil
}

// Match reports whether the entity satisfies the prepared condition. It cannot
// fail: every way this evaluation could error is a structural property of the
// condition and was already reported by Prepare.
func (p Prepared) Match(data []byte, meta spi.EntityMeta) bool {
	return p.root.match(data, meta)
}

func (n *prepNode) match(data []byte, meta spi.EntityMeta) bool {
	switch n.kind {
	case prepGroup:
		if n.or {
			for i := range n.children {
				if n.children[i].match(data, meta) {
					return true
				}
			}
			return false
		}
		for i := range n.children {
			if !n.children[i].match(data, meta) {
				return false
			}
		}
		return true

	case prepLeaf:
		// The path's SYNTAX decides what it addresses, never the stored
		// value's shape (docs/cloud-parity/path-grammar.md section 3): a bare
		// hop resolves to the value there, whatever its shape, unwrapped
		// never; a "[*]" hop resolves to that array's elements, and to
		// nothing when the value there is not an array. ResolvePath is the
		// one resolver that encodes this — the same one the SPI kernel's
		// PreparedFilter.Match calls — so this evaluator cannot answer a
		// bare-vs-wildcard path differently than a pushed-down query would.
		// A leaf holds when SOME addressed value satisfies it (existential).
		for _, r := range spi.ResolvePath(data, n.hops) {
			if spi.EvalLeaf(n.exp, r) {
				return true
			}
		}
		return false

	case prepMetaString:
		return spi.EvalLeaf(n.exp, metaStringResult(n.metaField, meta))

	case prepMetaTemporal:
		return spi.EvalLeaf(n.exp, metaTemporalResult(n.metaField, meta))
	}

	// prepNever, and any unpopulated node.
	return false
}

// metaStringResult wraps a string-valued meta field in a one-key document and
// reads it back, so meta string comparison goes through the same kernel as
// data leaves with a declared String type.
func metaStringResult(field string, meta spi.EntityMeta) gjson.Result {
	var v string
	switch field {
	case "state":
		v = meta.State
	case "transitionForLatestSave":
		v = meta.TransitionForLatestSave
	case "transactionId":
		v = meta.TransactionID
	case "id":
		v = meta.ID
	}
	return gjson.Get(fmt.Sprintf(`{"v":%q}`, v), "v")
}

// metaTemporalResult bridges a stored meta instant to a gjson.Result. A zero
// time (unset) bridges to an ABSENT result, not to a comparable ~year-1
// instant: IS_NULL matches it and every binary op, negatives included,
// non-matches under the kernel's null uniformity.
func metaTemporalResult(field string, meta spi.EntityMeta) gjson.Result {
	var t time.Time
	switch field {
	case "creationDate":
		t = meta.CreationDate
	case "lastUpdateTime":
		t = meta.LastModifiedDate
	}
	if t.IsZero() {
		return gjson.Result{}
	}
	b, err := json.Marshal(t)
	if err != nil {
		return gjson.Result{}
	}
	return gjson.ParseBytes(b)
}
