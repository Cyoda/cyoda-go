package openapivalidator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteReport_FormatsMismatches(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "report.md")

	mm := []Mismatch{
		{Operation: "getOne", Method: "GET", Path: "/x/1", Status: 200,
			Reason: "missing required field 'transactionId'", TestName: "TestEntity_Create"},
		{Operation: "create", Method: "POST", Path: "/x", Status: 418,
			Reason: "status not declared", TestName: "TestEntity_BadCase"},
	}
	exercised := map[string]bool{"getOne": true, "create": true}
	all := []string{"getOne", "create", "deleteOne"} // deleteOne is uncovered

	if err := WriteReport(out, mm, exercised, all); err != nil {
		t.Fatalf("WriteReport: %v", err)
	}

	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(got)

	for _, must := range []string{
		"OpenAPI Conformance Report",
		"## Mismatches (2)",
		"GET /x/1 -> 200",
		"POST /x -> 418",
		"## Uncovered Operations (1)",
		"deleteOne",
	} {
		if !strings.Contains(body, must) {
			t.Errorf("report missing %q\n--- got ---\n%s", must, body)
		}
	}
}
