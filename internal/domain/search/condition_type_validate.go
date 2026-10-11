package search

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

// ValidateConditionValueTypes walks a condition tree and checks each simple
// clause against the field's declared types, in two steps:
//
//   - the operator must apply to at least one declared type
//     (applicableTypes): a string or pattern operator needs a text type, an
//     ordering or range operator an ordered one. CONTAINS on a numeric field
//     or GREATER_THAN on a boolean can never match, so it is refused rather
//     than answered with an empty result.
//   - a comparison or range operand must PARSE into at least one declared
//     type — the same type-directed parse the leaf-comparison kernel
//     (spi.ExpandLeaf) performs at evaluation time. A numeric-looking string
//     on an [INTEGER, STRING] field parses and is accepted.
//
// The model's FieldsMap provides a lookup from JSONPath (e.g. "$.price") to
// a FieldDescriptor carrying the observed DataType(s). Conditions referencing
// unknown paths are accepted (the condition may traverse a path not yet seen
// in training data); a field with no declared types carries no constraint.
//
// Returns a non-nil error only when an operator does not apply to the field's
// declared types or an operand parses into none of them
// (errConditionTypeMismatch), or a lifecycle field is unknown
// (errInvalidFieldPath). Operand shape/arity — an object operand
// (never valid for any operator), null on a binary op, a range op's
// 2-element bounds shape — is enforced separately by
// ValidateCondition/validateOperandShape/validateBetweenArity, upstream of
// this type check.
func ValidateConditionValueTypes(model *schema.ModelNode, cond predicate.Condition) error {
	if cond == nil {
		return nil
	}
	// fm stays nil when model is nil. walkConditionTypes/validateSimpleConditionType
	// gracefully skip the data-field-vs-schema check on a nil map (an
	// unknown-path lookup returns ok=false, the "accept" branch) — so the
	// only checks that still run without a model are the model-independent
	// ones: operator/BETWEEN-arity (via the caller's ValidateCondition) and
	// lifecycle/temporal type-soundness (validateLifecycleType below). This
	// lets callers with no schema plumbing (e.g. grouped-stats) reuse this
	// function for temporal/lifecycle validation by passing model=nil.
	var fm map[string]schema.FieldDescriptor
	if model != nil {
		fm = model.FieldsMap()
	}
	return ValidateConditionFieldTypes(fm, cond)
}

// ValidateConditionFieldTypes is ValidateConditionValueTypes against a fields
// map rather than a model node: the same checks, the same errors. A caller
// whose fields map may come from a bounded schema refresh (the workflow
// engine, after search.ValidateKnownPaths) passes that map, so the type check
// runs against the schema the leaves are typed against at evaluation. A nil
// map runs only the model-independent checks.
func ValidateConditionFieldTypes(fields map[string]schema.FieldDescriptor, cond predicate.Condition) error {
	return walkConditionTypes(fields, cond, 0)
}

func walkConditionTypes(fm map[string]schema.FieldDescriptor, cond predicate.Condition, depth int) error {
	if cond == nil {
		return nil
	}
	if depth >= MaxConditionDepth {
		return fmt.Errorf("condition depth exceeded (max %d)", MaxConditionDepth)
	}
	switch c := cond.(type) {
	case *predicate.SimpleCondition:
		return validateSimpleConditionType(fm, c)
	case *predicate.GroupCondition:
		for _, child := range c.Conditions {
			if err := walkConditionTypes(fm, child, depth+1); err != nil {
				return err
			}
		}
		return nil
	case *predicate.LifecycleCondition:
		return validateLifecycleType(c)
	case *predicate.ArrayCondition:
		// An array clause's positional values become EQUALS leaves once
		// desugared, each folding back to the wildcard key
		// validateSimpleConditionType already resolves declared types
		// through (schema.CanonicalFieldPath). Routing through the same
		// desugar spi.ConditionToFilter uses, rather than writing a second
		// type check here, keeps the two definitions of the clause's
		// semantics from drifting apart.
		return walkConditionTypes(fm, spi.DesugarCondition(c), depth+1)
	case *predicate.FunctionCondition:
		return nil
	default:
		return nil
	}
}

// maxTruncatedOperandRunes bounds how much of a rejected operand this
// package's error paths echo back to the caller, mirroring the SPI kernel's
// own truncateOperand convention (spi.ExpandLeaf, eval_leaf.go): search
// request bodies are capped far larger than this, so echoing a mismatched
// operand verbatim would let a single oversized-but-otherwise-ordinary
// request inflate a 400 body (and the WARN log line that repeats it) to
// request size.
const maxTruncatedOperandRunes = 64

// truncateOperand renders v for inclusion in a client-facing error message,
// capped to maxTruncatedOperandRunes runes with a trailing "…" marker so a
// reader can tell truncation happened rather than mistaking the cut string
// for the operand in full.
func truncateOperand(v any) string {
	s := fmt.Sprintf("%v", v)
	r := []rune(s)
	if len(r) <= maxTruncatedOperandRunes {
		return s
	}
	return string(r[:maxTruncatedOperandRunes]) + "…"
}

func validateSimpleConditionType(fm map[string]schema.FieldDescriptor, c *predicate.SimpleCondition) error {
	// FieldsMap keys carry the "$." prefix and spell every array hop "[*]"; a
	// condition may legitimately omit the prefix, and may address one array
	// element positionally ("$.arr[0]"). Looking either up raw made the type
	// check silently skip such a leaf, so an operand that should be rejected 400
	// CONDITION_TYPE_MISMATCH was accepted and evaluated to an empty page
	// instead — and the wildcard and positional spellings of one path
	// disagreed. Only the LOOKUP is canonicalised: every diagnostic below names
	// c.JsonPath, the spelling the request actually sent.
	key := schema.CanonicalFieldPath(normalisePath(c.JsonPath))
	fd, ok := fm[key]
	if !ok {
		// Not a leaf. In a schema'd model (non-empty FieldsMap), a path that is
		// a KNOWN CONTAINER — a strict prefix of one or more leaf paths, but not
		// itself a leaf — has substructure and cannot be compared to a scalar:
		// you must navigate to a leaf sub-path. Reject a scalar-operand
		// comparison on such a path as INVALID_FIELD_PATH. Unary presence tests
		// (IS_NULL/NOT_NULL) carry no scalar operand — they test presence, not a
		// value — so they are NOT rejected. A mixed object-or-scalar node is a
		// leaf after schema field-collection (it carries its scalar types), so it
		// never reaches this branch. A genuinely-unknown (non-container) path
		// carries no type constraint here; the separate field-path validation
		// pass classifies it.
		if len(fm) > 0 && carriesScalarOperand(spi.MapOperator(c.OperatorType)) && isKnownContainerPath(key, fm) {
			return fmt.Errorf("field %q is a container with substructure and cannot be compared to a scalar; navigate to a leaf sub-path: %w",
				c.JsonPath, errInvalidFieldPath)
		}
		// Unknown path — no type constraint here (INVALID_FIELD_PATH for data
		// leaves is raised by the separate field-path validation pass).
		return nil
	}
	if len(fd.Types) == 0 {
		// No declared types recorded — no constraint; accept.
		return nil
	}

	// An object operand is rejected upstream, at the model-independent shape
	// layer (validateOperandShape in operators.go, wired into ValidateCondition
	// — the single boundary every transport funnels through before this
	// type check runs) as INVALID_CONDITION, not here: it is a shape/arity
	// error (spec §6/§8), not a field-type mismatch.

	op := spi.MapOperator(c.OperatorType)
	applicable := applicableTypes(op, fd.Types)
	if len(applicable) == 0 {
		return fmt.Errorf("operator %q does not apply to field %q's declared types %v: %w",
			c.OperatorType, c.JsonPath, fd.Types, errConditionTypeMismatch)
	}

	// Only the comparison/range family constrains the operand's type. String
	// operators and the null-presence tests parse any operand.
	if !isParseConstrainedOp(op) {
		return nil
	}

	switch v := c.Value.(type) {
	case nil:
		// Null operand carries no type to mismatch; arity is enforced elsewhere.
		return nil
	case []any:
		// Array operand — BETWEEN's [lo, hi] bounds or a legacy positional
		// (IN-style) set. Every non-null element must parse into a declared
		// type; a null element is compatible with any type; an empty array has
		// nothing to mismatch. Range-op arity (exactly two bounds) is enforced
		// by validateBetweenArity, not here.
		for i, elem := range v {
			if elem == nil {
				continue
			}
			if !operandParsesDeclared(applicable, elem) {
				return fmt.Errorf("value[%d] %s parses into none of field %q's types %v that operator %q applies to: %w",
					i, truncateOperand(elem), c.JsonPath, applicable, c.OperatorType, errConditionTypeMismatch)
			}
		}
		if !rangeBoundsParseTogether(op, applicable, v) {
			return fmt.Errorf("bounds %s and %s parse into no single type of field %q's types %v: %w",
				truncateOperand(v[0]), truncateOperand(v[1]), c.JsonPath, applicable, errConditionTypeMismatch)
		}
		return nil
	default:
		if !operandParsesDeclared(applicable, v) {
			return fmt.Errorf("operand %s parses into none of field %q's types %v that operator %q applies to: %w",
				truncateOperand(v), c.JsonPath, applicable, c.OperatorType, errConditionTypeMismatch)
		}
		return nil
	}
}

// isParseConstrainedOp reports whether op's operand must parse into a declared
// type for the condition to be valid. Only the six comparison operators and the
// two range operators are constrained; string operators (CONTAINS, LIKE, the
// case-insensitive/negated variants, ...) and the null-presence tests (IS_NULL,
// NOT_NULL) carry no operand-parse constraint — mirroring the kernel,
// where ExpandLeaf only reports a "parses into no declared type" error for the
// compare (expandCompare) and range (expandBetween) families.
func isParseConstrainedOp(op spi.FilterOp) bool {
	switch op {
	case spi.FilterEq, spi.FilterNe, spi.FilterGt, spi.FilterGte, spi.FilterLt, spi.FilterLte,
		spi.FilterBetween, spi.FilterBetweenInclusive:
		return true
	}
	return false
}

// applicableTypes returns the declared types op applies to; none means the
// leaf is refused. The rule follows what the kernel (spi.ExpandLeaf/EvalLeaf)
// can evaluate, narrowed by one decision:
//
//   - a string or pattern operator applies to a text type (STRING,
//     CHARACTER). The kernel would test any value stored as a JSON string,
//     temporal and identifier values included, but those compare by their own
//     type, never as text: a date is compared with the ordering and range
//     operators. On a field with no text type at all, a string operator could
//     only ever match nothing, or, negated, everything.
//   - an ordering operator applies to a numeric, text or temporal type; the
//     kernel compares a boolean or an identifier for equality only, so
//     GREATER_THAN on one matches nothing.
//   - a range operator applies to a numeric, STRING or temporal type; the
//     kernel has no range over a single CHARACTER.
//   - every other operator applies to every declared type.
//
// A comparison or range operand must then parse into one of the applicable
// types, so GREATER_THAN with a UUID operand on a [UUID, DOUBLE] field is
// refused rather than answered with a result no entity could ever be in.
func applicableTypes(op spi.FilterOp, declared []schema.DataType) []schema.DataType {
	var applies func(schema.DataType) bool
	switch {
	case isTextOp(op):
		applies = isTextType
	case op == spi.FilterBetween, op == spi.FilterBetweenInclusive:
		applies = isRangeType
	case op == spi.FilterGt, op == spi.FilterGte, op == spi.FilterLt, op == spi.FilterLte:
		applies = isOrderedType
	default:
		return declared
	}
	var out []schema.DataType
	for _, t := range declared {
		if applies(t) {
			out = append(out, t)
		}
	}
	return out
}

// isOrderedType reports whether the kernel orders values of t: numbers, text
// and the temporal subtypes.
func isOrderedType(t schema.DataType) bool {
	return schema.IsNumeric(t) || isTextType(t) || isTemporalType(t)
}

// isRangeType reports whether the kernel evaluates a range over values of t:
// numbers, STRING and the temporal subtypes (spi's expandBetween).
func isRangeType(t schema.DataType) bool {
	return schema.IsNumeric(t) || t == schema.String || isTemporalType(t)
}

// isTemporalType reports whether t is one of the six temporal subtypes.
func isTemporalType(t schema.DataType) bool {
	switch t {
	case schema.LocalDate, schema.LocalDateTime, schema.LocalTime,
		schema.ZonedDateTime, schema.Year, schema.YearMonth:
		return true
	}
	return false
}

// isTextOp reports whether op is one of the sixteen string and pattern
// operators — the set spi.ExpandLeaf evaluates as a text test.
func isTextOp(op spi.FilterOp) bool {
	switch op {
	case spi.FilterContains, spi.FilterNotContains,
		spi.FilterStartsWith, spi.FilterNotStartsWith,
		spi.FilterEndsWith, spi.FilterNotEndsWith,
		spi.FilterLike, spi.FilterMatchesRegex,
		spi.FilterIEq, spi.FilterINe,
		spi.FilterIContains, spi.FilterINotContains,
		spi.FilterIStartsWith, spi.FilterINotStartsWith,
		spi.FilterIEndsWith, spi.FilterINotEndsWith:
		return true
	}
	return false
}

// isTextType reports whether t is a text type. A temporal or identifier value
// is stored as a JSON string too, but it compares by its own type, never as
// text.
func isTextType(t schema.DataType) bool {
	return t == schema.String || t == schema.Character
}

// carriesScalarOperand reports whether op compares the field against a scalar
// operand (and therefore cannot address a container path). Every operator does
// EXCEPT the unary null-presence tests (IS_NULL, NOT_NULL), which test presence
// rather than a value and so remain valid on a container path.
func carriesScalarOperand(op spi.FilterOp) bool {
	switch op {
	case spi.FilterIsNull, spi.FilterNotNull:
		return false
	}
	return true
}

// isKnownContainerPath reports whether p names a KNOWN CONTAINER in fm — a
// strict prefix of one or more leaf paths, without being a leaf itself. Both
// dot- and wildcard-delimited descents count as substructure: FieldsMap
// records an array's element under the "[*]" key and never the container, so
// omitting the "[" form would miss every array container. p is assumed absent
// from fm as a direct leaf (both callers check that first).
//
// It is the single container predicate for the two questions that need it, and
// they read the answer oppositely: path_validate.go's pathOrContainerKnown
// ACCEPTS the path (the structural field exists), while the caller above
// REJECTS a scalar comparison ON the interior node. One predicate, so the two
// cannot disagree about what a container is.
func isKnownContainerPath(p string, fm map[string]schema.FieldDescriptor) bool {
	dotPrefix := p + "."
	arrPrefix := p + "["
	for known := range fm {
		if strings.HasPrefix(known, dotPrefix) || strings.HasPrefix(known, arrPrefix) {
			return true
		}
	}
	return false
}

// rangeBoundsParseTogether reports whether a range operator's two bounds parse
// into one declared type family together, by asking the kernel's own range
// expansion (spi.ExpandLeaf). Each bound parsing into some declared type is
// not enough: [5.5, "2024-01-01"] on an [INTEGER, LOCAL_DATE] field has no
// family holding both, so no range exists to evaluate. Anything other than a
// range operator with exactly two non-null bounds is left to the arity and
// per-element checks.
func rangeBoundsParseTogether(op spi.FilterOp, declared []schema.DataType, bounds []any) bool {
	if op != spi.FilterBetween && op != spi.FilterBetweenInclusive {
		return true
	}
	if len(bounds) != 2 || bounds[0] == nil || bounds[1] == nil {
		return true
	}
	values := []string{spi.OperandString(bounds[0]), spi.OperandString(bounds[1])}
	_, err := spi.ExpandLeaf(op, "", values, declared)
	return err == nil
}

// operandParsesDeclared reports whether a single scalar operand parses into at
// least one of the declared types. It calls the kernel's own comparison-parse
// (spi.ExpandLeaf with FilterEq): the parse decision — "does this operand
// denote a value of a declared type" — is operator-independent across the
// comparison family (expandCompare's engaged check depends only on the operand
// and declared set, not the operator), so FilterEq is a faithful oracle that
// also applies per array element, where a range operator cannot be expanded in
// isolation. The operand is normalised with spi.OperandString — the single
// shared operand→string form the evaluators (internal/match's Prepare,
// spi.Prepare) feed the kernel — so validation and evaluation agree. A Void
// expansion (parses but every bucket dropped, e.g. EQUALS 12.5 on [INTEGER])
// is NOT a mismatch — it is accepted and evaluates to non-match.
func operandParsesDeclared(declared []schema.DataType, v any) bool {
	_, err := spi.ExpandLeaf(spi.FilterEq, spi.OperandString(v), nil, declared)
	return err == nil
}

// errConditionTypeMismatch is the sentinel error for condition type mismatch.
// Handlers check errors.Is(err, errConditionTypeMismatch) to emit HTTP 400
// with ErrCodeConditionTypeMismatch.
var errConditionTypeMismatch = fmt.Errorf("condition type mismatch")

// errInvalidFieldPath is the sentinel error for a condition referencing a
// meta field path the vocabulary does not recognize. Handlers check
// errors.Is(err, errInvalidFieldPath) to emit HTTP 400 with
// ErrCodeInvalidFieldPath (distinct from errConditionTypeMismatch's
// CONDITION_TYPE_MISMATCH: the field itself is unknown, not merely
// type-incompatible with its operator/operand).
var errInvalidFieldPath = fmt.Errorf("invalid field path")

// ErrConditionTypeMismatch and ErrInvalidFieldPath are exported aliases of
// the sentinels above, letting other domain packages (e.g. entity's
// grouped-stats validation, which calls ValidateConditionValueTypes(nil, ...)
// for its own model-independent temporal/lifecycle type-soundness check)
// classify the returned error via errors.Is without duplicating the sentinel
// or depending on package-internal identifiers.
var (
	ErrConditionTypeMismatch = errConditionTypeMismatch
	ErrInvalidFieldPath      = errInvalidFieldPath
)

// metaTemporalDeclared is the declared type set for temporal meta fields
// (creationDate, lastUpdateTime): a single ZonedDateTime, matching
// lifecycleToFilter's stamping. A coarser operand (e.g. "2024", or an
// offset-less "2021-01-01T00:00:00") parses as its own natural subtype and
// upscales to ZonedDateTime (spec §4), so it parses into this set and is
// accepted.
var metaTemporalDeclared = []spi.DataType{spi.ZonedDateTime}

// validateLifecycleType enforces type-soundness for LifecycleCondition
// (meta) clauses, by the same rules a data field gets:
//   - the field must be a known meta filter field (sortableMetaFields key,
//     or the previousTransition alias) — otherwise errInvalidFieldPath.
//   - on a temporal meta field (creationDate, lastUpdateTime), declared
//     metaTemporalDeclared, the operator must apply to that type
//     (applicableTypes) — a string or pattern operator does not, and is
//     errConditionTypeMismatch;
//   - and a comparison/range operand must parse into a temporal type —
//     otherwise errConditionTypeMismatch. A coarse operand upscales rather
//     than being rejected.
//
// Non-temporal meta fields (state, transitionForLatestSave, transactionId,
// id) carry no further constraint here: they compare as their stored
// text/string form regardless of operator.

// ValidateLifecycleCondition checks a lifecycle/meta condition for type
// soundness (known meta field; a comparison/range operand that parses into a
// temporal type on temporal fields). Shared by the search API boundary and
// workflow-criterion import so both reject the same malformed conditions.
// Returns a descriptive error; callers map it to their own 4xx code.
func ValidateLifecycleCondition(c *predicate.LifecycleCondition) error {
	return validateLifecycleType(c)
}

func validateLifecycleType(c *predicate.LifecycleCondition) error {
	if !isKnownMetaFilterField(c.Field) {
		return fmt.Errorf("unknown meta filter field %q: %w", c.Field, errInvalidFieldPath)
	}
	field := c.Field
	if field == "previousTransition" {
		field = "transitionForLatestSave"
	}
	if !isTemporalMetaField(field) {
		return nil
	}
	op := spi.MapOperator(c.OperatorType)
	applicable := applicableTypes(op, metaTemporalDeclared)
	if len(applicable) == 0 {
		return fmt.Errorf("operator %q does not apply to temporal meta field %q: %w",
			c.OperatorType, c.Field, errConditionTypeMismatch)
	}
	// The two null tests skip operand parsing (the value is unused); every
	// other operator that survived the check above is one of the eight
	// comparison/range operators and requires a temporal-parsing operand.
	if !isParseConstrainedOp(op) {
		return nil
	}
	for i, elem := range operandElements(c.Value) {
		if elem == nil {
			continue
		}
		if !operandParsesDeclared(applicable, elem) {
			return fmt.Errorf("operand[%d] %s parses into no temporal type for field %q: %w",
				i, truncateOperand(elem), c.Field, errConditionTypeMismatch)
		}
	}
	return nil
}

// operandElements normalises a condition value into its operand elements: a
// scalar becomes a single-element slice; a []any (BETWEEN's [lo, hi] pair, or a
// positional set) becomes one element per member. Callers skip nil elements.
func operandElements(v any) []any {
	if arr, ok := v.([]any); ok {
		return arr
	}
	return []any{v}
}

// loadModelNode fetches and parses the model schema for ref, returning the
// *schema.ModelNode used for condition-type validation.
//
// A load or parse FAILURE is an error, not an absent node: the schema is what
// the check needs, and answering the request without it is the fail-open this
// function used to perform. A (nil, nil) return means something different and
// benign — the descriptor carries no schema, so the model declares no typed
// fields and there is no constraint to apply. EnsureModelRegistered has
// already confirmed the model exists by the time this runs.
func loadModelNode(ctx context.Context, store spi.ModelStore, ref spi.ModelRef) (*schema.ModelNode, error) {
	// Reuse the store's cached parse when it has one; see loadFieldsMap.
	if p, ok := store.(schemaNodeProvider); ok {
		return p.SchemaNode(ctx, ref)
	}
	desc, err := store.Get(ctx, ref)
	if err != nil {
		return nil, err
	}
	if desc == nil || len(desc.Schema) == 0 {
		return nil, nil
	}
	return schema.Unmarshal(desc.Schema)
}

// classifyConditionTypeErrCode maps a ValidateConditionValueTypes error to
// its 400 error code: errInvalidFieldPath → INVALID_FIELD_PATH (the field
// itself is unknown), anything else → CONDITION_TYPE_MISMATCH (the operator
// or the operand does not fit a known field's type).
//
// Shared by every caller of ValidateConditionValueTypes —
// validateConditionTypes below (SearchService), and grouped stats' handler
// and service layers (internal/domain/entity) via ClassifyConditionTypeErrCode
// — so the four call sites cannot drift on what code an operator-class
// rejection answers the way they had before this function existed.
func classifyConditionTypeErrCode(err error) string {
	switch {
	case errors.Is(err, errInvalidFieldPath):
		return common.ErrCodeInvalidFieldPath
	default:
		return common.ErrCodeConditionTypeMismatch
	}
}

// ClassifyConditionTypeErrCode is the exported entry point for
// classifyConditionTypeErrCode, for callers outside this package that
// validate a condition via the exported ValidateConditionValueTypes —
// currently entity.Handler's grouped-stats and conditional-delete paths,
// which hold their own model/schema plumbing and so call
// ValidateConditionValueTypes directly rather than through Search. Mirrors
// the StructuralConditionErrCode/structuralConditionErrCode exported-wrapper
// shape already used in this package (service.go).
func ClassifyConditionTypeErrCode(err error) string {
	return classifyConditionTypeErrCode(err)
}

// validateConditionTypes is the single boundary enforcing condition
// type-soundness for every SearchService entry point (HTTP, gRPC, and any
// future transport funnel through Search/SubmitAsync). It loads the model
// schema and delegates to ValidateConditionValueTypes, mapping the returned
// sentinel error to the appropriate 400-classified *common.AppError via
// classifyConditionTypeErrCode.
//
// A schema-load failure fails the request. The previous behaviour — skip the
// check and search anyway — was justified in a comment here as "empty results,
// never a wrong match", on the reasoning that a missing model leaves every leaf
// with an empty Declared and so degrades uniformly. That reasoning is wrong,
// and spi.ConditionToFilter's own godoc says why: an empty declared set
// annihilates the eight comparison and ordering leaves to a non-match while the
// other eighteen — the presence tests, the string and pattern operators, and
// the whole case-insensitive family — keep evaluating normally. The result set
// is skewed, not empty, and it is returned as though it were complete.
//
// The workflow engine already fails closed on the same load error; search now
// matches it, per .claude/rules/correctness-over-availability.md.
func (s *SearchService) validateConditionTypes(ctx context.Context, modelStore spi.ModelStore, modelRef spi.ModelRef, cond predicate.Condition) *common.AppError {
	// Gate the model READ on whether cond addresses any data path — a
	// lifecycle-only condition needs no schema to validate (mirrors
	// workflow/engine.go's evaluateCriterion, Task 7) — but never gate the
	// VALIDATION CALL itself on whether that read was attempted, succeeded,
	// or found a schema. This used to return nil without calling
	// ValidateConditionValueTypes at all in two cases — an unreadable
	// schema paired with a lifecycle-only condition, and a model that
	// loaded cleanly but carries no schema yet — and both silently skipped
	// its model-independent half too: validateLifecycleType, the one check
	// that refuses a text or pattern operator on a temporal meta field
	// (creationDate/lastUpdateTime). Left unrejected, that predicate reaches
	// the evaluators unvalidated and tests the instant's RFC3339 text — a
	// question nobody asked — and a NOT wrapping it selects every entity
	// whose text does not match. The workflow-criterion path already
	// refuses it the same way. ValidateConditionValueTypes tolerates a nil
	// model by design (its own doc: the model-independent checks still run),
	// so calling it unconditionally here, with node possibly nil, is always
	// safe and never a behaviour change for a condition with a genuine data
	// path against a loadable schema.
	var node *schema.ModelNode
	if len(extractFieldPaths(cond)) > 0 {
		var err error
		node, err = loadModelNode(ctx, modelStore, modelRef)
		if err != nil {
			return common.Internal("failed to load model schema for condition type validation", err)
		}
	}
	if err := ValidateConditionValueTypes(node, cond); err != nil {
		return common.Operational(http.StatusBadRequest, classifyConditionTypeErrCode(err), err.Error())
	}
	return nil
}

// LoadModelNode fetches and parses the model schema for ref.
//
// Exported for callers outside this package that must run
// [ValidateConditionValueTypes] against the real model rather than against nil
// — currently the grouped-stats handler, whose type check was schema-blind
// while every other condition surface's was not. Failure policy is the one
// stated on the unexported loader: a load or parse failure is an error, while
// (nil, nil) means the descriptor carries no schema and there is no type
// constraint to apply.
func LoadModelNode(ctx context.Context, store spi.ModelStore, ref spi.ModelRef) (*schema.ModelNode, error) {
	return loadModelNode(ctx, store, ref)
}
