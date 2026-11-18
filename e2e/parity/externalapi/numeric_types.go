package externalapi

// External API Scenario Suite — 13-numeric-types
//
// Cyoda assigns each JSON number a DataType when a model is registered.
// External REST default ParsingSpec (parseStrings=true) with
// intScope=INTEGER, decimalScope=DOUBLE. Scenarios that depend on
// narrowed scopes (numeric/03, numeric/05) are not externally reachable
// — recorded as internal_only_skip in the mapping doc.
//
// Comparison conventions (per dictionary):
//   - DOUBLE values: float64 equality (entity_equals_json).
//   - BIG_DECIMAL: stripTrailingZeros normalisation via math/big.Float.
//   - UNBOUND_DECIMAL: toPlainString normalisation via math/big.Float.
//   - BIG_INTEGER / UNBOUND_INTEGER: big.Int string comparison.
//
// SIMPLE_VIEW export shape (per internal/domain/model/exporter/simple_view.go):
//
//	{"currentState": "LOCKED|UNLOCKED", "model": {"$": {".key": "TYPE", ...}, ...}}
//
// simpleViewFieldType handles this wrapper transparently.
//
// Big-number round-trip note: GetEntity decodes via json.Decoder without
// UseNumber, so large decimals/integers arrive as float64 and lose precision.
// Scenarios 13/07–13/10 use GetEntityBodyRaw + UseNumber to preserve the
// original JSON number string for faithful big.Float / big.Int comparison.
//
// Polymorphic type ordering: cyoda-go's TypeSet sorts DataType by its iota
// constant value. The ordering is:
//
//	INTEGER(0) < STRING(7) < BOOLEAN(19)
//
// so ".key1" from samples "abc" then 456 → [INTEGER, STRING],
// ".key2" from samples 123 then false → [INTEGER, BOOLEAN].
//
// SIMPLE_VIEW model sample used for 13/11 must be a genuine decimal (e.g.
// 100.1) so that price is classified as DOUBLE, not INTEGER. The classifier
// routes whole-number values (e.g. 100.0 → 100) to the integer branch.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/cyoda/cyoda-go/e2e/externalapi/driver"
	"github.com/cyoda/cyoda-go/e2e/parity"
)

func init() {
	parity.Register(
		parity.NamedTest{Name: "ExternalAPI_13_01_IntegerLandsInDoubleField", Fn: RunExternalAPI_13_01_IntegerLandsInDoubleField},
		parity.NamedTest{Name: "ExternalAPI_13_04_DefaultIntegerScopeINTEGER", Fn: RunExternalAPI_13_04_DefaultIntegerScopeINTEGER},
		parity.NamedTest{Name: "ExternalAPI_13_05ext_PolymorphicMergeWithDefaultScopes", Fn: RunExternalAPI_13_05ext_PolymorphicMergeWithDefaultScopes},
		parity.NamedTest{Name: "ExternalAPI_13_06_DoubleAtMaxBoundary", Fn: RunExternalAPI_13_06_DoubleAtMaxBoundary},
		parity.NamedTest{Name: "ExternalAPI_13_07_BigDecimal20Plus18", Fn: RunExternalAPI_13_07_BigDecimal20Plus18},
		parity.NamedTest{Name: "ExternalAPI_13_08_UnboundDecimalGT18Frac", Fn: RunExternalAPI_13_08_UnboundDecimalGT18Frac},
		parity.NamedTest{Name: "ExternalAPI_13_09_BigInteger38Digit", Fn: RunExternalAPI_13_09_BigInteger38Digit},
		parity.NamedTest{Name: "ExternalAPI_13_10_UnboundInteger40Digit", Fn: RunExternalAPI_13_10_UnboundInteger40Digit},
		parity.NamedTest{Name: "ExternalAPI_13_11_SearchIntegerAgainstDouble", Fn: RunExternalAPI_13_11_SearchIntegerAgainstDouble},
	)
}

// simpleViewFieldType decodes a SIMPLE_VIEW model export and returns the
// type descriptor for the given path/key. The export shape is:
//
//	{"currentState": "...", "model": {"$": {".key": "TYPE_NAME", ...}, ...}}
//
// Returns the raw descriptor string ("DOUBLE", "BIG_DECIMAL",
// "[INTEGER, STRING]", etc.) or an error if the path/key is absent.
func simpleViewFieldType(t *testing.T, exported json.RawMessage, path, key string) (string, error) {
	t.Helper()
	// SIMPLE_VIEW is always wrapped in {"currentState": "...", "model": {...}}.
	var wrapped struct {
		Model map[string]map[string]any `json:"model"`
	}
	if err := json.Unmarshal(exported, &wrapped); err != nil {
		return "", fmt.Errorf("unmarshal SIMPLE_VIEW wrapper: %w (body=%s)", err, string(exported))
	}
	if wrapped.Model == nil {
		// Fallback: try treating the whole export as a flat path map.
		var flat map[string]map[string]any
		if err2 := json.Unmarshal(exported, &flat); err2 != nil {
			return "", fmt.Errorf("SIMPLE_VIEW has no 'model' key and flat decode failed: %w", err2)
		}
		wrapped.Model = flat
	}
	pathMap, ok := wrapped.Model[path]
	if !ok {
		return "", fmt.Errorf("path %q not in SIMPLE_VIEW model (have %v)", path, keysOf(wrapped.Model))
	}
	descriptor, ok := pathMap[key]
	if !ok {
		return "", fmt.Errorf("key %q not under path %q (have %v)", key, path, keysOfAny(pathMap))
	}
	descStr, ok := descriptor.(string)
	if !ok {
		return "", fmt.Errorf("descriptor at %q.%q is not a string: %T %v", path, key, descriptor, descriptor)
	}
	return descStr, nil
}

func keysOf(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func keysOfAny(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// getNumericFieldStr parses a raw entity body JSON with UseNumber and
// returns the numeric field value at key as a string. The entity body has
// the shape {"type":"ENTITY","data":{...},"meta":{...}}; big numbers in
// "data" are preserved as json.Number strings rather than being coerced to
// float64 (which would lose precision for values > 2^53).
func getNumericFieldStr(t *testing.T, body []byte, key string) (string, error) {
	t.Helper()
	// Two-pass approach: first decode outer envelope, then decode "data" separately.
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var raw map[string]json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return "", fmt.Errorf("decode entity body: %w (body=%s)", err, string(body))
	}
	dataDec := json.NewDecoder(bytes.NewReader(raw["data"]))
	dataDec.UseNumber()
	var dataMap map[string]json.Number
	if err := dataDec.Decode(&dataMap); err != nil {
		return "", fmt.Errorf("decode data field: %w (data=%s)", err, string(raw["data"]))
	}
	v, ok := dataMap[key]
	if !ok {
		return "", fmt.Errorf("key %q not in data: %v", key, dataMap)
	}
	return string(v), nil
}

// RunExternalAPI_13_01_IntegerLandsInDoubleField — dictionary 13/01.
// Model locked with {"price": 13.111} → $.price lands as DOUBLE.
// POST {"price": 13} (JSON integer) must be accepted and listing yields 1.
func RunExternalAPI_13_01_IntegerLandsInDoubleField(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	d := driver.NewInProcess(t, fixture)
	if err := d.CreateModelFromSample("num1301", 1, `{"price": 13.111}`); err != nil {
		t.Fatalf("create model: %v", err)
	}
	if err := d.LockModel("num1301", 1); err != nil {
		t.Fatalf("lock: %v", err)
	}
	if _, err := d.CreateEntity("num1301", 1, `{"price": 13}`); err != nil {
		t.Fatalf("create entity (integer into DOUBLE): %v", err)
	}
	list, err := d.ListEntitiesByModel("num1301", 1)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("entity count: got %d want 1", len(list))
	}
}

// RunExternalAPI_13_04_DefaultIntegerScopeINTEGER — dictionary 13/04.
// Without a ParsingSpec override (only mode external surfaces support),
// {"key1":"abc","key2":123} must land as STRING / INTEGER.
func RunExternalAPI_13_04_DefaultIntegerScopeINTEGER(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	d := driver.NewInProcess(t, fixture)
	if err := d.CreateModelFromSample("num1304", 1, `{"key1":"abc","key2":123}`); err != nil {
		t.Fatalf("create model: %v", err)
	}
	exported, err := d.ExportModel("SIMPLE_VIEW", "num1304", 1)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if got, err := simpleViewFieldType(t, exported, "$", ".key1"); err != nil {
		t.Errorf("$.key1 lookup: %v", err)
	} else if got != "STRING" {
		t.Errorf("$.key1: got %q want STRING", got)
	}
	if got, err := simpleViewFieldType(t, exported, "$", ".key2"); err != nil {
		t.Errorf("$.key2 lookup: %v", err)
	} else if got != "INTEGER" {
		t.Errorf("$.key2: got %q want INTEGER", got)
	}
}

// RunExternalAPI_13_05ext_PolymorphicMergeWithDefaultScopes — dictionary 13/05ext.
// Two-sample merge with default scopes → polymorphic types
// [INTEGER, STRING], [INTEGER, BOOLEAN], [BOOLEAN].
//
// The first sample is registered via CreateModelFromSample; the second
// is merged in via UpdateModelFromSample (CreateModelFromSample would
// return an error since the model already exists).
//
// Polymorphic type ordering: TypeSet sorts by DataType iota order:
//
//	Integer(0) < String(7) < Boolean(19)
//
// so key1 (first seen: STRING, then INTEGER) → "[INTEGER, STRING]"
// and key2 (first seen: INTEGER, then BOOLEAN) → "[INTEGER, BOOLEAN]".
func RunExternalAPI_13_05ext_PolymorphicMergeWithDefaultScopes(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	d := driver.NewInProcess(t, fixture)
	if err := d.CreateModelFromSample("num1305ext", 1, `{"key1":"abc","key2":123}`); err != nil {
		t.Fatalf("create model: %v", err)
	}
	// Merge a second sample — UpdateModelFromSample folds new types into
	// the existing schema rather than rejecting "model exists".
	if err := d.UpdateModelFromSample("num1305ext", 1, `{"key1":456,"key2":false,"key3":true}`); err != nil {
		t.Fatalf("update model (second sample): %v", err)
	}
	exported, err := d.ExportModel("SIMPLE_VIEW", "num1305ext", 1)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	for _, c := range []struct {
		key  string
		want string
	}{
		{".key1", "[INTEGER, STRING]"},
		{".key2", "[INTEGER, BOOLEAN]"},
		{".key3", "BOOLEAN"},
	} {
		if got, err := simpleViewFieldType(t, exported, "$", c.key); err != nil {
			t.Errorf("%s lookup: %v", c.key, err)
		} else if got != c.want {
			t.Errorf("%s: got %q want %q", c.key, got, c.want)
		}
	}
}

// RunExternalAPI_13_06_DoubleAtMaxBoundary — dictionary 13/06.
// Field declared DOUBLE accepts Double.MAX_VALUE; entity round-trips.
func RunExternalAPI_13_06_DoubleAtMaxBoundary(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	d := driver.NewInProcess(t, fixture)
	const sample = `{"v": 1.7976931348623157E308}`
	if err := d.CreateModelFromSample("num1306", 1, sample); err != nil {
		t.Fatalf("create model: %v", err)
	}
	if err := d.LockModel("num1306", 1); err != nil {
		t.Fatalf("lock: %v", err)
	}
	id, err := d.CreateEntity("num1306", 1, sample)
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	got, err := d.GetEntity(id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	v, ok := got.Data["v"].(float64)
	if !ok {
		t.Fatalf("readback $.v not a float64: %T %v", got.Data["v"], got.Data["v"])
	}
	if v != 1.7976931348623157e308 {
		t.Errorf("readback $.v: got %g want 1.7976931348623157e308", v)
	}
}

// RunExternalAPI_13_07_BigDecimal20Plus18 — dictionary 13/07.
// 38-significant-digit decimal lands as BIG_DECIMAL; round-trip via
// stripTrailingZeros numeric comparison using math/big.Float.
//
// Uses GetEntityBodyRaw + UseNumber to preserve JSON number precision
// through the HTTP round-trip (standard json.Decoder would lose digits
// by decoding the big number as float64).
func RunExternalAPI_13_07_BigDecimal20Plus18(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	d := driver.NewInProcess(t, fixture)
	const numStr = "12345678901234567800.123456789012345670"
	const sample = `{"v": ` + numStr + `}`
	if err := d.CreateModelFromSample("num1307", 1, sample); err != nil {
		t.Fatalf("create model: %v", err)
	}
	if err := d.LockModel("num1307", 1); err != nil {
		t.Fatalf("lock: %v", err)
	}
	id, err := d.CreateEntity("num1307", 1, sample)
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	_, rawBody, err := d.GetEntityBodyRaw(id)
	if err != nil {
		t.Fatalf("get entity raw: %v", err)
	}
	gotStr, err := getNumericFieldStr(t, rawBody, "v")
	if err != nil {
		t.Fatalf("extract field v: %v (body=%s)", err, string(rawBody))
	}
	want, _, wantErr := new(big.Float).SetPrec(256).Parse(numStr, 10)
	if wantErr != nil {
		t.Fatalf("parse expected value %q as big.Float: %v", numStr, wantErr)
	}
	gotF, _, err2 := new(big.Float).SetPrec(256).Parse(gotStr, 10)
	if err2 != nil {
		t.Fatalf("parse readback %q as big.Float: %v", gotStr, err2)
	}
	if want.Cmp(gotF) != 0 {
		t.Errorf("BIG_DECIMAL round-trip: got %s want %s (stripTrailingZeros equivalent)",
			gotF.Text('f', 18), want.Text('f', 18))
	}
	exported, err := d.ExportModel("SIMPLE_VIEW", "num1307", 1)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if typ, err := simpleViewFieldType(t, exported, "$", ".v"); err != nil {
		t.Errorf("$.v lookup: %v", err)
	} else if typ != "BIG_DECIMAL" {
		t.Errorf("$.v type: got %q want BIG_DECIMAL", typ)
	}
}

// RunExternalAPI_13_08_UnboundDecimalGT18Frac — dictionary 13/08.
// 19-fractional-digit value lands as UNBOUND_DECIMAL; round-trip via
// toPlainString numeric comparison.
//
// Uses GetEntityBodyRaw + UseNumber to preserve precision.
func RunExternalAPI_13_08_UnboundDecimalGT18Frac(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	d := driver.NewInProcess(t, fixture)
	const numStr = "12345678901234567800.1234567890123456789"
	const sample = `{"v": ` + numStr + `}`
	if err := d.CreateModelFromSample("num1308", 1, sample); err != nil {
		t.Fatalf("create model: %v", err)
	}
	if err := d.LockModel("num1308", 1); err != nil {
		t.Fatalf("lock: %v", err)
	}
	id, err := d.CreateEntity("num1308", 1, sample)
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	_, rawBody, err := d.GetEntityBodyRaw(id)
	if err != nil {
		t.Fatalf("get entity raw: %v", err)
	}
	gotStr, err := getNumericFieldStr(t, rawBody, "v")
	if err != nil {
		t.Fatalf("extract field v: %v (body=%s)", err, string(rawBody))
	}
	want, _, wantErr := new(big.Float).SetPrec(256).Parse(numStr, 10)
	if wantErr != nil {
		t.Fatalf("parse expected value %q as big.Float: %v", numStr, wantErr)
	}
	gotF, _, err2 := new(big.Float).SetPrec(256).Parse(gotStr, 10)
	if err2 != nil {
		t.Fatalf("parse readback %q: %v", gotStr, err2)
	}
	if want.Cmp(gotF) != 0 {
		t.Errorf("UNBOUND_DECIMAL round-trip: got %s want %s (toPlainString equivalent)",
			gotF.Text('f', 19), want.Text('f', 19))
	}
	exported, err := d.ExportModel("SIMPLE_VIEW", "num1308", 1)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if typ, err := simpleViewFieldType(t, exported, "$", ".v"); err != nil {
		t.Errorf("$.v lookup: %v", err)
	} else if typ != "UNBOUND_DECIMAL" {
		t.Errorf("$.v type: got %q want UNBOUND_DECIMAL", typ)
	}
}

// RunExternalAPI_13_09_BigInteger38Digit — dictionary 13/09.
// 38-digit integer lands as BIG_INTEGER; round-trip via big.Int comparison.
//
// Uses GetEntityBodyRaw + UseNumber to preserve precision.
func RunExternalAPI_13_09_BigInteger38Digit(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	d := driver.NewInProcess(t, fixture)
	const numStr = "12345678901234567890123456789012345678"
	const sample = `{"v": ` + numStr + `}`
	if err := d.CreateModelFromSample("num1309", 1, sample); err != nil {
		t.Fatalf("create model: %v", err)
	}
	if err := d.LockModel("num1309", 1); err != nil {
		t.Fatalf("lock: %v", err)
	}
	id, err := d.CreateEntity("num1309", 1, sample)
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	_, rawBody, err := d.GetEntityBodyRaw(id)
	if err != nil {
		t.Fatalf("get entity raw: %v", err)
	}
	gotStr, err := getNumericFieldStr(t, rawBody, "v")
	if err != nil {
		t.Fatalf("extract field v: %v (body=%s)", err, string(rawBody))
	}
	want, _ := new(big.Int).SetString(numStr, 10)
	gotI, ok := new(big.Int).SetString(gotStr, 10)
	if !ok {
		t.Fatalf("parse readback %q as big.Int failed", gotStr)
	}
	if want.Cmp(gotI) != 0 {
		t.Errorf("BIG_INTEGER round-trip: got %s want %s", gotI.String(), want.String())
	}
	exported, err := d.ExportModel("SIMPLE_VIEW", "num1309", 1)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if typ, err := simpleViewFieldType(t, exported, "$", ".v"); err != nil {
		t.Errorf("$.v lookup: %v", err)
	} else if typ != "BIG_INTEGER" {
		t.Errorf("$.v type: got %q want BIG_INTEGER", typ)
	}
}

// RunExternalAPI_13_10_UnboundInteger40Digit — dictionary 13/10.
// 40-digit integer lands as UNBOUND_INTEGER; round-trip via big.Int comparison.
//
// Uses GetEntityBodyRaw + UseNumber to preserve precision.
func RunExternalAPI_13_10_UnboundInteger40Digit(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	d := driver.NewInProcess(t, fixture)
	const numStr = "1234567890123456789012345678901234567890"
	const sample = `{"v": ` + numStr + `}`
	if err := d.CreateModelFromSample("num1310", 1, sample); err != nil {
		t.Fatalf("create model: %v", err)
	}
	if err := d.LockModel("num1310", 1); err != nil {
		t.Fatalf("lock: %v", err)
	}
	id, err := d.CreateEntity("num1310", 1, sample)
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	_, rawBody, err := d.GetEntityBodyRaw(id)
	if err != nil {
		t.Fatalf("get entity raw: %v", err)
	}
	gotStr, err := getNumericFieldStr(t, rawBody, "v")
	if err != nil {
		t.Fatalf("extract field v: %v (body=%s)", err, string(rawBody))
	}
	want, _ := new(big.Int).SetString(numStr, 10)
	gotI, ok := new(big.Int).SetString(gotStr, 10)
	if !ok {
		t.Fatalf("parse readback %q as big.Int failed", gotStr)
	}
	if want.Cmp(gotI) != 0 {
		t.Errorf("UNBOUND_INTEGER round-trip: got %s want %s", gotI.String(), want.String())
	}
	exported, err := d.ExportModel("SIMPLE_VIEW", "num1310", 1)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if typ, err := simpleViewFieldType(t, exported, "$", ".v"); err != nil {
		t.Errorf("$.v lookup: %v", err)
	} else if typ != "UNBOUND_INTEGER" {
		t.Errorf("$.v type: got %q want UNBOUND_INTEGER", typ)
	}
}

// RunExternalAPI_13_11_SearchIntegerAgainstDouble — dictionary 13/11.
// Ingest 4 entities with $.price as DOUBLE values >= 70. Search with an
// INTEGER condition value (70) via async + direct must each return 4.
//
// Sample must use a genuine decimal (100.1 not 100.0) so that price is
// classified as DOUBLE. The classifier routes whole-number values
// (e.g. 100.0 → scale=0 after strip → INTEGER branch) to avoid
// classifying "100.0" as DOUBLE.
func RunExternalAPI_13_11_SearchIntegerAgainstDouble(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	d := driver.NewInProcess(t, fixture)
	// 100.1 has genuine fractional scale after StripTrailingZeros → DOUBLE.
	if err := d.CreateModelFromSample("num1311", 1, `{"price": 100.1}`); err != nil {
		t.Fatalf("create model: %v", err)
	}
	if err := d.LockModel("num1311", 1); err != nil {
		t.Fatalf("lock: %v", err)
	}
	for _, price := range []float64{70.5, 80.0, 100.0, 200.5} {
		body := fmt.Sprintf(`{"price": %g}`, price)
		if _, err := d.CreateEntity("num1311", 1, body); err != nil {
			t.Fatalf("create entity price=%g: %v", price, err)
		}
	}
	const condition = `{
		"type": "group", "operator": "AND",
		"conditions": [
			{"type": "simple", "jsonPath": "$.price", "operatorType": "GREATER_OR_EQUAL", "value": 70}
		]
	}`

	// Direct (sync) search.
	directResults, err := d.SyncSearch("num1311", 1, condition)
	if err != nil {
		t.Fatalf("SyncSearch: %v", err)
	}
	if len(directResults) != 4 {
		t.Errorf("direct: got %d results want 4", len(directResults))
	}

	// Async search via Await wrapper.
	page, err := d.AwaitAsyncSearchResults("num1311", 1, condition, 10*time.Second)
	if err != nil {
		t.Fatalf("AwaitAsyncSearchResults: %v", err)
	}
	if len(page.Content) != 4 {
		t.Errorf("async: got %d results want 4", len(page.Content))
	}
}
