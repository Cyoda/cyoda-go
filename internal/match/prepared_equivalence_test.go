// The merge gate for the predicate-evaluator prepare/execute split.
//
// The frozen* functions below are a verbatim copy of the pre-split evaluator,
// taken before it was deleted. They are a COPY on purpose: they must keep
// answering the old way after the originals are gone.
//
// Do not "simplify" them by calling live code, and do not update them when the
// live evaluator changes. If they and Prepare disagree, the live code is wrong.
package match

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
)

// --- frozen reference --------------------------------------------------

func frozenMatch(condition predicate.Condition, entityData []byte, entityMeta spi.EntityMeta, fieldTypes FieldTypes) (bool, error) {
	switch c := condition.(type) {
	case *predicate.SimpleCondition:
		return frozenMatchSimple(c, entityData, fieldTypes)
	case *predicate.LifecycleCondition:
		return frozenMatchLifecycle(c, entityMeta)
	case *predicate.GroupCondition:
		return frozenMatchGroup(c, entityData, entityMeta, fieldTypes)
	case *predicate.ArrayCondition:
		return frozenMatchArray(c, entityData, fieldTypes)
	case *predicate.FunctionCondition:
		return false, fmt.Errorf("function conditions not implemented")
	default:
		return false, fmt.Errorf("unknown condition type: %T", condition)
	}
}

func frozenMatchSimple(c *predicate.SimpleCondition, data []byte, fieldTypes FieldTypes) (bool, error) {
	path := convertJSONPath(c.JsonPath)
	result := gjson.GetBytes(data, path)

	var declared []spi.DataType
	if fieldTypes != nil {
		declared = fieldTypes(fieldMapKey(c.JsonPath))
	}

	// If the path produced an array result (from # wildcard), check if ANY
	// element matches for applicable operators.
	if result.IsArray() {
		return frozenMatchArrayWildcard(c.OperatorType, result, c.Value, declared)
	}

	return frozenApplyOperator(c.OperatorType, result, c.Value, declared)
}

// frozenMatchArrayWildcard checks if any element in an array result matches
// the operator. declared is the array element's declared type set (the
// wildcard path is itself the FieldsMap key, e.g. "$.laureates[*].motivation").
func frozenMatchArrayWildcard(operatorType string, arrayResult gjson.Result, expected any, declared []spi.DataType) (bool, error) {
	var lastErr error
	matched := false

	arrayResult.ForEach(func(_, value gjson.Result) bool {
		ok, err := frozenApplyOperator(operatorType, value, expected, declared)
		if err != nil {
			lastErr = err
			return false // stop iteration
		}
		if ok {
			matched = true
			return false // short-circuit
		}
		return true // continue
	})

	if lastErr != nil {
		return false, lastErr
	}
	return matched, nil
}

// frozenMatchLifecycle evaluates a lifecycle (meta) condition. Field routing
// is identity-driven, never operand-driven: creationDate/lastUpdateTime
// always compare chronologically via the temporal kernel branch (declared
// ZonedDateTime) regardless of operator, and the remaining canonical meta
// fields compare as declared strings via the same kernel.
func frozenMatchLifecycle(c *predicate.LifecycleCondition, meta spi.EntityMeta) (bool, error) {
	field := c.Field
	if field == "previousTransition" {
		field = "transitionForLatestSave"
	}

	switch field {
	case "creationDate":
		return frozenMatchTemporalMeta(c.OperatorType, meta.CreationDate, c.Value)
	case "lastUpdateTime":
		return frozenMatchTemporalMeta(c.OperatorType, meta.LastModifiedDate, c.Value)
	case "state":
		return frozenApplyStringLifecycle(c, meta.State)
	case "transitionForLatestSave":
		return frozenApplyStringLifecycle(c, meta.TransitionForLatestSave)
	case "transactionId":
		return frozenApplyStringLifecycle(c, meta.TransactionID)
	case "id":
		return frozenApplyStringLifecycle(c, meta.ID)
	default:
		return false, fmt.Errorf("unknown lifecycle field: %s", c.Field)
	}
}

// frozenApplyStringLifecycle evaluates a string-valued meta field: wrap the
// value in a gjson document and route it through the kernel with a declared
// String type, so meta string comparison shares the one comparison core with
// data leaves and the search pushdown.
func frozenApplyStringLifecycle(c *predicate.LifecycleCondition, value string) (bool, error) {
	fakeJSON := fmt.Sprintf(`{"v":%q}`, value)
	result := gjson.Get(fakeJSON, "v")
	return frozenApplyOperator(c.OperatorType, result, c.Value, []spi.DataType{spi.String})
}

// frozenMatchTemporalMeta compares a stored meta time.Time chronologically
// against the condition operand(s) via the kernel. The stored instant is
// bridged to a gjson.Result (an RFC3339 string from json.Marshal) and
// evaluated with a declared ZonedDateTime type, so meta temporal comparison
// shares the single EvalLeaf kernel with data-field temporal comparison. A
// zero-value time (unset) bridges to an absent Result: IS_NULL matches, every
// binary op (including NOT_EQUAL) non-matches under the kernel's null
// uniformity.
func frozenMatchTemporalMeta(op string, stored time.Time, value any) (bool, error) {
	// A temporal field admits only comparison / range / null operators. A
	// non-comparison operator (e.g. CONTAINS) is invalid on a temporal field
	// and degrades to non-match here — it must NOT lexically substring-match
	// the formatted RFC3339 string.
	if !isTemporalOperator(op) {
		return false, nil
	}
	var result gjson.Result
	if !stored.IsZero() {
		b, err := json.Marshal(stored)
		if err != nil {
			return false, nil
		}
		result = gjson.ParseBytes(b)
	}
	return frozenApplyOperator(op, result, value, []spi.DataType{spi.ZonedDateTime})
}

func frozenMatchGroup(c *predicate.GroupCondition, data []byte, meta spi.EntityMeta, fieldTypes FieldTypes) (bool, error) {
	switch c.Operator {
	case "AND":
		for _, child := range c.Conditions {
			ok, err := frozenMatch(child, data, meta, fieldTypes)
			if err != nil {
				return false, err
			}
			if !ok {
				return false, nil // short-circuit
			}
		}
		return true, nil

	case "OR":
		for _, child := range c.Conditions {
			ok, err := frozenMatch(child, data, meta, fieldTypes)
			if err != nil {
				return false, err
			}
			if ok {
				return true, nil // short-circuit
			}
		}
		return false, nil

	default:
		return false, fmt.Errorf("unknown group operator: %s", c.Operator)
	}
}

func frozenMatchArray(c *predicate.ArrayCondition, data []byte, fieldTypes FieldTypes) (bool, error) {
	basePath := convertJSONPath(c.JsonPath)

	var declared []spi.DataType
	if fieldTypes != nil {
		declared = fieldTypes(arrayElementFieldPath(c.JsonPath))
	}

	for i, expected := range c.Values {
		if expected == nil {
			continue // skip null positions
		}

		elemPath := fmt.Sprintf("%s.%d", basePath, i)
		result := gjson.GetBytes(data, elemPath)

		// Each positional value is an equality check on the array element,
		// routed through the kernel so numeric/type-directed semantics match
		// scalar EQUALS and the search pushdown's arrayToFilter. A missing
		// element (absent Result) non-matches under the kernel's null rule.
		ok, err := frozenApplyOperator("EQUALS", result, expected, declared)
		if err != nil || !ok {
			return false, nil
		}
	}

	return true, nil
}

// frozenApplyOperator evaluates a single predicate leaf against a stored
// gjson value by routing through the type-directed EvalLeaf kernel.
//
// One deliberate departure from the pre-split original: it calls
// spi.ExpandLeaf + spi.EvalLeaf directly rather than spi.EvalLeafString,
// because EvalLeafString is deleted alongside the pre-split evaluator. The
// swallow-to-non-match behaviour on an expansion failure is inlined here
// exactly as EvalLeafString performed it, so the observable behaviour is
// unchanged.
func frozenApplyOperator(operatorType string, actual gjson.Result, expected any, declared []spi.DataType) (bool, error) {
	op, ok := opNameToFilterOp(operatorType)
	if !ok {
		return false, fmt.Errorf("unsupported operator: %s", operatorType)
	}
	var values []string
	if op == spi.FilterBetween || op == spi.FilterBetweenInclusive {
		values = betweenBounds(expected)
	}
	exp, err := spi.ExpandLeaf(op, spi.OperandString(expected), values, declared)
	if err != nil {
		return false, nil // swallowed to a per-entity non-match, as before
	}
	return spi.EvalLeaf(exp, actual), nil
}

// --- generated corpus ----------------------------------------------------

var genOperators = []string{
	"EQUALS", "NOT_EQUAL", "GREATER_THAN", "LESS_THAN", "GREATER_OR_EQUAL", "LESS_OR_EQUAL",
	"CONTAINS", "STARTS_WITH", "ENDS_WITH", "LIKE", "MATCHES_PATTERN",
	"NOT_CONTAINS", "NOT_STARTS_WITH", "NOT_ENDS_WITH",
	"IEQUALS", "INOT_EQUAL", "ICONTAINS", "INOT_CONTAINS",
	"ISTARTS_WITH", "INOT_STARTS_WITH", "IENDS_WITH", "INOT_ENDS_WITH",
	"IS_NULL", "NOT_NULL", "BETWEEN", "BETWEEN_INCLUSIVE",
}

// genMetaFields is the canonical meta vocabulary matchLifecycle handles. The
// generator stays inside it: an unknown field is a structural error, covered
// by the hand-written table in Task 3, and the generator emits only
// well-formed conditions.
var genMetaFields = []string{
	"state", "id", "transactionId", "transitionForLatestSave",
	"previousTransition", "creationDate", "lastUpdateTime",
}

var genJSONPaths = []string{
	"$.name", "$.qty", "$.price", "$.flag", "$.uid", "$.when",
	"$.missing", "$.nested.inner", "name", "$.laureates[*].motivation", "$.tags",
}

var genValues = []any{
	"Alice", "alice", "", "A%", "A.*", "%ice",
	"30", 30, 30.5, -5, 0, true, false,
	"6ba7b810-9dad-11d1-80b4-00c04fd430c8",
	"2024-01-01T00:00:00Z", "2024-06-01", "2024",
	[]any{"1", "100"}, []any{"a", "z"}, []any{"1"},
	"não",
}

var genFieldTypeSets = [][]spi.DataType{
	{spi.String}, {spi.Integer}, {spi.Long}, {spi.UnboundDecimal}, {spi.Double},
	{spi.Boolean}, {spi.UUIDType}, {spi.ZonedDateTime}, {spi.LocalDate},
	{spi.Integer, spi.String}, {spi.Double, spi.Integer}, nil,
}

var genEqDocs = []string{
	`{"name":"Alice","qty":30,"price":12.78,"flag":true,"uid":"6ba7b810-9dad-11d1-80b4-00c04fd430c8","when":"2024-01-01T00:00:00Z","nested":{"inner":"deep"},"tags":["red","blue"],"laureates":[{"motivation":"for peace"}]}`,
	`{"name":"alice","qty":31,"when":"2024-06-01","tags":["red"],"laureates":[]}`,
	`{"name":null,"qty":null,"tags":[]}`,
	`{}`,
	`{"name":30,"qty":"30"}`,
	`{"laureates":[{"motivation":"for war"},{"motivation":"for peace"}]}`,
}

// genMode selects which corpus genCondition builds. The two are generated,
// asserted and env-tunable completely separately — no probabilistic mixing —
// because they need different assertions: a genValid tree is never allowed to
// error on EITHER evaluator, while a genInvalid tree always carries exactly
// one structural fault that Prepare must report.
type genMode int

const (
	// genValid conditions are the only shapes either evaluator can answer
	// without erroring. This is the pre-split corpus's generation logic,
	// unchanged: for genValid the RNG draw sequence is identical to before
	// this mode parameter was introduced.
	genValid genMode = iota
	// genInvalid conditions are a single bare fault leaf (see genFaultLeaf).
	// Wrapping the fault at a varied position is a separate step performed
	// by genFaultCase, not by genCondition itself.
	genInvalid
)

func genCondition(r *rand.Rand, depth int, mode genMode) predicate.Condition {
	if mode == genInvalid {
		return genFaultLeaf(r)
	}

	if depth <= 0 || r.Intn(3) == 0 {
		switch r.Intn(3) {
		case 0:
			return &predicate.LifecycleCondition{
				Field:        genMetaFields[r.Intn(len(genMetaFields))],
				OperatorType: genOperators[r.Intn(len(genOperators))],
				Value:        genValues[r.Intn(len(genValues))],
			}
		case 1:
			return &predicate.ArrayCondition{
				JsonPath: "$.tags",
				Values:   []any{genValues[r.Intn(len(genValues))], nil},
			}
		default:
			return &predicate.SimpleCondition{
				JsonPath:     genJSONPaths[r.Intn(len(genJSONPaths))],
				OperatorType: genOperators[r.Intn(len(genOperators))],
				Value:        genValues[r.Intn(len(genValues))],
			}
		}
	}
	op := "AND"
	if r.Intn(2) == 0 {
		op = "OR"
	}
	g := &predicate.GroupCondition{Operator: op}
	for i := 0; i < r.Intn(4); i++ {
		g.Conditions = append(g.Conditions, genCondition(r, depth-1, mode))
	}
	return g
}

// Corpus size and seed are overridable so a one-off widened exploration is
// reproducible. The committed defaults ARE the standing gate; -count alone
// widens nothing, because a fixed seed regenerates the same corpus.
func equivCases() int  { return envInt("MATCH_EQUIV_CASES", 200000) }
func equivSeed() int64 { return int64(envInt("MATCH_EQUIV_SEED", 0x30C0DE)) }

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// equivMetas is the fixed set of entity metas both equivalence tests draw
// from.
var equivMetas = []spi.EntityMeta{
	{ID: "ent-1", State: "active", TransactionID: "tx-1", TransitionForLatestSave: "approve",
		CreationDate:     time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		LastModifiedDate: time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)},
	{ID: "ent-2", State: ""},
	{},
}

// TestPrepare_EquivalentToFrozenMatch is the merge gate for the predicate
// evaluator: exact answer agreement on every well-formed condition.
//
// The generator emits only conditions neither evaluator can error on —
// structural faults are a deterministic corpus of their own, covered by
// TestPrepare_ReportsExactlyTheFrozenFault — so an error from EITHER side
// here is a generator bug or a real defect, never a case to skip.
func TestPrepare_EquivalentToFrozenMatch(t *testing.T) {
	cases := equivCases()
	r := rand.New(rand.NewSource(equivSeed()))

	for i := 0; i < cases; i++ {
		cond := genCondition(r, 3, genValid)
		data := []byte(genEqDocs[r.Intn(len(genEqDocs))])
		meta := equivMetas[r.Intn(len(equivMetas))]
		types := genFieldTypeSets[r.Intn(len(genFieldTypeSets))]
		fieldTypes := func(string) []spi.DataType { return types }

		wantMatch, wantErr := frozenMatch(cond, data, meta, fieldTypes)
		if wantErr != nil {
			t.Fatalf("case %d: frozen evaluator errored on a well-formed condition: %v\n  cond=%#v\n  data=%s",
				i, wantErr, cond, data)
		}

		prepared, prepErr := Prepare(cond, fieldTypes)
		if prepErr != nil {
			t.Fatalf("case %d: Prepare errored on a well-formed condition: %v\n  cond=%#v",
				i, prepErr, cond)
		}

		if got := prepared.Match(data, meta); got != wantMatch {
			t.Fatalf("DIVERGENCE at case %d\n  prepared=%v frozen=%v\n  cond=%#v\n  data=%s\n  meta=%+v\n  types=%v",
				i, got, wantMatch, cond, data, meta, types)
		}
	}
}

// --- invalid corpus: one structural fault per case ------------------------
//
// genValid never produces a condition either evaluator can error on, so the
// "Prepare reports a genuine fault, not an invented one" property needs its
// own deterministic corpus: every case here carries EXACTLY one of the five
// structural faults, at a position varied across four wrapper shapes.

// genFaultKind enumerates the five structural faults a condition tree can
// carry — one per error-returning default/error branch shared by frozenMatch
// and Prepare.
type genFaultKind int

const (
	faultFunction genFaultKind = iota
	faultUnknownType
	faultUnknownLifecycleField
	faultBadGroupOperator
	faultUnsupportedOperator
	numFaultKinds
)

// genUnknownCondition implements predicate.Condition without being one of the
// four kinds Match/Prepare's type switches recognise — the "unknown condition
// type" fault.
type genUnknownCondition struct{}

func (genUnknownCondition) Type() string { return "unknown" }

// genFaultLeaf returns a single bare condition carrying exactly one
// structural fault. The path/field/operator names are chosen so the fault
// fires independently of which document or meta the case is later paired
// with — every fault is a property of the condition's own shape, never of a
// row.
func genFaultLeaf(r *rand.Rand) predicate.Condition {
	switch genFaultKind(r.Intn(int(numFaultKinds))) {
	case faultFunction:
		return &predicate.FunctionCondition{}
	case faultUnknownType:
		return genUnknownCondition{}
	case faultUnknownLifecycleField:
		return &predicate.LifecycleCondition{
			Field:        "not-a-real-lifecycle-field",
			OperatorType: genOperators[r.Intn(len(genOperators))],
			Value:        genValues[r.Intn(len(genValues))],
		}
	case faultBadGroupOperator:
		return &predicate.GroupCondition{Operator: "XOR"}
	default: // faultUnsupportedOperator
		return &predicate.SimpleCondition{
			JsonPath:     "$.not_a_real_field_marker",
			OperatorType: "NOT_A_REAL_OPERATOR",
			Value:        genValues[r.Intn(len(genValues))],
		}
	}
}

// genAlwaysFalseCondition is a well-formed leaf guaranteed false for every
// meta in equivMetas, independent of document data. It is the AND-first-child
// used to build the position that hides a fault from a short-circuiting
// row-walk.
func genAlwaysFalseCondition() predicate.Condition {
	return &predicate.LifecycleCondition{
		Field: "state", OperatorType: "EQUALS", Value: "\x00never-a-real-state\x00",
	}
}

// genFaultPosition enumerates the wrapper shapes a fault leaf is placed at.
// Position must not change the outcome — Prepare must report the fault
// wherever it sits — and that is exactly what varying it proves.
type genFaultPosition int

const (
	posStandalone genFaultPosition = iota
	posFirstOfAnd
	posSecondOfAndFirstFalse
	posNestedTwoDeep
	numFaultPositions
)

// faultCase pairs a condition tree carrying exactly one structural fault with
// the bare fault leaf itself. The test asserts Prepare's error text against
// the bare leaf, not against whatever a lazy row-walk happens to reach in the
// wrapped form — see posSecondOfAndFirstFalse for why that distinction
// matters.
type faultCase struct {
	wrapped predicate.Condition
	fault   predicate.Condition
}

func genFaultCase(r *rand.Rand) faultCase {
	fault := genFaultLeaf(r)
	switch genFaultPosition(r.Intn(int(numFaultPositions))) {
	case posStandalone:
		return faultCase{wrapped: fault, fault: fault}

	case posFirstOfAnd:
		return faultCase{
			wrapped: &predicate.GroupCondition{Operator: "AND",
				Conditions: []predicate.Condition{fault, genAlwaysFalseCondition()}},
			fault: fault,
		}

	case posSecondOfAndFirstFalse:
		// The first child is false for every meta this test uses, so a
		// short-circuiting row-walk (the frozen evaluator) NEVER reaches
		// fault, for any document. Prepare's eager, non-short-circuiting walk
		// must still report it — this is the case that makes "some document
		// reaches it" the wrong property to assert.
		return faultCase{
			wrapped: &predicate.GroupCondition{Operator: "AND",
				Conditions: []predicate.Condition{genAlwaysFalseCondition(), fault}},
			fault: fault,
		}

	default: // posNestedTwoDeep
		return faultCase{
			wrapped: &predicate.GroupCondition{Operator: "AND", Conditions: []predicate.Condition{
				&predicate.GroupCondition{Operator: "OR", Conditions: []predicate.Condition{fault}},
			}},
			fault: fault,
		}
	}
}

// The invalid corpus has only 5 fault kinds x 4 positions to explore, so a
// few thousand cases is ample — it does not need the 200k-case budget the
// valid corpus does. Overridable for the same reason as MATCH_EQUIV_CASES.
func invalidEquivCases() int  { return envInt("MATCH_EQUIV_INVALID_CASES", 5000) }
func invalidEquivSeed() int64 { return int64(envInt("MATCH_EQUIV_INVALID_SEED", 0xFA07)) }

// TestPrepare_ReportsExactlyTheFrozenFault is the merge gate's other half: on
// a condition that carries a genuine structural fault, Prepare's eager,
// non-short-circuiting walk must report exactly the fault the frozen
// evaluator raises for the BARE fault leaf — regardless of where in the tree
// the fault sits, and regardless of whether a lazy row-walk would ever reach
// it for any document.
func TestPrepare_ReportsExactlyTheFrozenFault(t *testing.T) {
	cases := invalidEquivCases()
	r := rand.New(rand.NewSource(invalidEquivSeed()))

	for i := 0; i < cases; i++ {
		fc := genFaultCase(r)
		doc := []byte(genEqDocs[r.Intn(len(genEqDocs))])
		meta := equivMetas[r.Intn(len(equivMetas))]
		fieldTypes := func(string) []spi.DataType { return nil }

		_, wantErr := frozenMatch(fc.fault, doc, meta, fieldTypes)
		if wantErr == nil {
			t.Fatalf("case %d: bare fault leaf did not error on the frozen evaluator\n  fault=%#v", i, fc.fault)
		}

		_, prepErr := Prepare(fc.wrapped, fieldTypes)
		if prepErr == nil {
			t.Fatalf("case %d: Prepare accepted a condition carrying a structural fault\n  wrapped=%#v", i, fc.wrapped)
		}
		if prepErr.Error() != wantErr.Error() {
			t.Fatalf("case %d: fault text moved: frozen(bare fault)=%q prepared(wrapped)=%q\n  wrapped=%#v",
				i, wantErr, prepErr, fc.wrapped)
		}
	}
}
