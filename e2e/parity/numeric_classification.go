package parity

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// RunNumericClassification18DigitDecimal verifies that an 18-fractional-digit
// decimal ingested via HTTP round-trips as BIG_DECIMAL in the exported schema.
func RunNumericClassification18DigitDecimal(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-num-bigdecimal"
	const modelVersion = 1
	const payload = `{"name":"x","value":3.141592653589793238}`

	if err := c.ImportModel(t, modelName, modelVersion, payload); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	if err := c.LockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	raw, err := c.ExportModel(t, "SIMPLE_VIEW", modelName, modelVersion)
	if err != nil {
		t.Fatalf("ExportModel: %v", err)
	}
	if !strings.Contains(string(raw), "BIG_DECIMAL") {
		t.Errorf("expected BIG_DECIMAL classification for 18-fractional-digit value; schema: %s", raw)
	}
}

// RunNumericClassification20DigitDecimal verifies that a 20-fractional-digit
// decimal (exceeds Trino scale-18) ingested via HTTP round-trips as
// UNBOUND_DECIMAL.
func RunNumericClassification20DigitDecimal(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-num-unbounddecimal"
	const modelVersion = 1
	const payload = `{"name":"x","value":3.14159265358979323846}`

	if err := c.ImportModel(t, modelName, modelVersion, payload); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	if err := c.LockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	raw, err := c.ExportModel(t, "SIMPLE_VIEW", modelName, modelVersion)
	if err != nil {
		t.Fatalf("ExportModel: %v", err)
	}
	if !strings.Contains(string(raw), "UNBOUND_DECIMAL") {
		t.Errorf("expected UNBOUND_DECIMAL classification for 20-fractional-digit value; schema: %s", raw)
	}
}

// RunNumericClassificationLargeInteger verifies that a 2^53+1 integer
// ingested via HTTP round-trips as LONG.
func RunNumericClassificationLargeInteger(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-num-long"
	const modelVersion = 1
	// 2^53 + 1 = 9007199254740993
	const payload = `{"id":9007199254740993,"name":"x"}`

	if err := c.ImportModel(t, modelName, modelVersion, payload); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	if err := c.LockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	raw, err := c.ExportModel(t, "SIMPLE_VIEW", modelName, modelVersion)
	if err != nil {
		t.Fatalf("ExportModel: %v", err)
	}
	if !strings.Contains(string(raw), "LONG") {
		t.Errorf("expected LONG classification for 2^53+1 integer; schema: %s", raw)
	}
}

// RunNumericClassificationIntegerSchemaAcceptsInteger confirms strict
// validation accepts integer data on an integer schema.
func RunNumericClassificationIntegerSchemaAcceptsInteger(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-num-strict-int-accept"
	const modelVersion = 1
	// Seed model with an integer field; ChangeLevel left empty ("") → strict validate.
	if err := c.ImportModel(t, modelName, modelVersion, `{"qty":5}`); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	if err := c.LockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	// Integer against integer schema — must succeed.
	if _, err := c.CreateEntity(t, modelName, modelVersion, `{"qty":42}`); err != nil {
		t.Errorf("expected integer payload against integer schema to succeed; got %v", err)
	}
}

// RunNumericClassificationIntegerSchemaRejectsDecimal confirms strict
// validation rejects decimal data on an integer schema.
func RunNumericClassificationIntegerSchemaRejectsDecimal(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-num-strict-int-reject"
	const modelVersion = 1
	if err := c.ImportModel(t, modelName, modelVersion, `{"qty":5}`); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	if err := c.LockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	// Decimal against integer schema under strict validate — must be rejected.
	status, body, err := c.CreateEntityRaw(t, modelName, modelVersion, `{"qty":13.111}`)
	if err != nil {
		t.Fatalf("CreateEntityRaw transport: %v", err)
	}
	if status == http.StatusOK {
		t.Errorf("expected rejection of decimal value against integer schema; got 200: %s", body)
	}
}

// RunSchemaExtensionsSequentialFoldAcrossRequests verifies that sequential
// entity creates on a STRUCTURAL-level model accumulate schema extensions
// correctly across requests (the Get-fold path for Postgres; apply-in-place
// for memory/sqlite). After N creates each introducing a new field, the
// exported schema reflects every accumulated field.
func RunSchemaExtensionsSequentialFoldAcrossRequests(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-schema-ext-sequential"
	const modelVersion = 1

	if err := c.ImportModel(t, modelName, modelVersion, `{"name":"Test","amount":0,"status":"new"}`); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	if err := c.LockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	if err := c.SetChangeLevel(t, modelName, modelVersion, "STRUCTURAL"); err != nil {
		t.Fatalf("SetChangeLevel STRUCTURAL: %v", err)
	}

	// Six sequential writes, each adding a new field.
	for i := 0; i < 6; i++ {
		payload := fmt.Sprintf(
			`{"name":"Sequential-%d","amount":%d,"status":"new","seq_field_%d":"val_%d"}`,
			i, i, i, i,
		)
		if _, err := c.CreateEntity(t, modelName, modelVersion, payload); err != nil {
			t.Fatalf("create #%d failed: %v", i, err)
		}
	}

	raw, err := c.ExportModel(t, "SIMPLE_VIEW", modelName, modelVersion)
	if err != nil {
		t.Fatalf("ExportModel: %v", err)
	}
	rawStr := string(raw)
	for i := 0; i < 6; i++ {
		want := fmt.Sprintf("seq_field_%d", i)
		if !strings.Contains(rawStr, want) {
			t.Errorf("folded schema missing %q after sequential writes: %s", want, raw)
		}
	}
}

// RunNumericClassificationDoubleSchemaAcceptsWholeNumber confirms a leaf
// declared DOUBLE admits a whole number under a below-TYPE change level. The
// write path classifies the incoming value's type from the value alone —
// every whole number is INTEGER, whatever its spelling — but INTEGER widens
// into DOUBLE, so the model's admitted value space is unchanged and the write
// spends no ChangeLevel permission. Under ARRAY_LENGTH every such write was
// refused as a spurious "type change at .amount requires TYPE level".
func RunNumericClassificationDoubleSchemaAcceptsWholeNumber(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-num-double-whole"
	const modelVersion = 1
	if err := c.ImportModel(t, modelName, modelVersion, `{"amount":10.5,"amounts":[1.5,2.5]}`); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	if err := c.LockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	if err := c.SetChangeLevel(t, modelName, modelVersion, "ARRAY_LENGTH"); err != nil {
		t.Fatalf("SetChangeLevel: %v", err)
	}
	before, err := c.ExportModel(t, "SIMPLE_VIEW", modelName, modelVersion)
	if err != nil {
		t.Fatalf("ExportModel before: %v", err)
	}

	// The three spellings classify identically — the walker strips trailing
	// zeros and normalises exponents before classifying.
	for _, payload := range []string{
		`{"amount":1000,"amounts":[3,4]}`,
		`{"amount":1000.0,"amounts":[3.0,4.0]}`,
		`{"amount":1e3,"amounts":[3e0,4e0]}`,
	} {
		status, body, err := c.CreateEntityRaw(t, modelName, modelVersion, payload)
		if err != nil {
			t.Fatalf("CreateEntityRaw transport for %s: %v", payload, err)
		}
		if status != http.StatusOK {
			t.Fatalf("whole number into a DOUBLE leaf must be accepted; %s got %d: %s", payload, status, body)
		}
	}

	after, err := c.ExportModel(t, "SIMPLE_VIEW", modelName, modelVersion)
	if err != nil {
		t.Fatalf("ExportModel after: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("a whole-number write to a DOUBLE leaf must not change the schema\n  before: %s\n  after:  %s",
			before, after)
	}

	// The lattice bounds the relaxation, and every backend must fail closed at
	// the same place: past 2^31 the value classifies LONG, which does not widen
	// into DOUBLE, so it is a genuine type change and stays refused here.
	status, body, err := c.CreateEntityRaw(t, modelName, modelVersion, `{"amount":2147483648,"amounts":[1.5,2.5]}`)
	if err != nil {
		t.Fatalf("CreateEntityRaw transport: %v", err)
	}
	if status != http.StatusBadRequest {
		t.Errorf("LONG into a DOUBLE leaf is a type change; got %d: %s", status, body)
	}
}
