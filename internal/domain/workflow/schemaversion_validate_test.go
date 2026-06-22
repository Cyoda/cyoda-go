package workflow

import (
	"errors"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func TestValidateSchemaVersions_AcceptsCurrent(t *testing.T) {
	t.Parallel()
	wfs := []spi.WorkflowDefinition{
		{Name: "wf", Version: CurrentSchemaVersion, InitialState: "S",
			States: map[string]spi.StateDefinition{"S": {}}},
	}
	if err := validateSchemaVersions(wfs); err != nil {
		t.Fatalf("validateSchemaVersions(current) = %v; want nil", err)
	}
}

func TestValidateSchemaVersions_RejectsMalformed(t *testing.T) {
	t.Parallel()
	wfs := []spi.WorkflowDefinition{
		{Name: "wf-bad", Version: "1.0.0", InitialState: "S",
			States: map[string]spi.StateDefinition{"S": {}}},
	}
	err := validateSchemaVersions(wfs)
	if err == nil {
		t.Fatalf("validateSchemaVersions(\"1.0.0\") = nil; want error")
	}
	if !strings.Contains(err.Error(), "wf-bad") {
		t.Fatalf("error message %q does not name workflow wf-bad", err.Error())
	}
	if !strings.Contains(err.Error(), "MAJOR.MINOR") {
		t.Fatalf("error message %q does not mention MAJOR.MINOR form", err.Error())
	}
}

func TestValidateSchemaVersions_RejectsMajorUnsupported(t *testing.T) {
	t.Parallel()
	wfs := []spi.WorkflowDefinition{
		{Name: "wf", Version: "2.0", InitialState: "S",
			States: map[string]spi.StateDefinition{"S": {}}},
	}
	err := validateSchemaVersions(wfs)
	if err == nil {
		t.Fatalf("validateSchemaVersions(\"2.0\") = nil; want error")
	}
	if !errors.Is(err, ErrSchemaMajorUnsupported) {
		t.Fatalf("error %v is not ErrSchemaMajorUnsupported", err)
	}
}

func TestValidateSchemaVersions_RejectsMinorTooNew(t *testing.T) {
	t.Parallel()
	wfs := []spi.WorkflowDefinition{
		{Name: "wf", Version: "1.99", InitialState: "S",
			States: map[string]spi.StateDefinition{"S": {}}},
	}
	err := validateSchemaVersions(wfs)
	if err == nil {
		t.Fatalf("validateSchemaVersions(\"1.99\") = nil; want error")
	}
	if !errors.Is(err, ErrSchemaMinorTooNew) {
		t.Fatalf("error %v is not ErrSchemaMinorTooNew", err)
	}
}

func TestValidateSchemaVersions_RejectsMinorTooOld(t *testing.T) {
	// Mutates the package-level SupportedSchemaRanges to model a
	// deprecation window where MinMinor has been raised. Cannot use
	// t.Parallel() — see TestSupports for the same constraint.
	orig := SupportedSchemaRanges
	SupportedSchemaRanges = []SchemaRange{
		{Major: 1, MinMinor: 2, MaxMinor: 5},
	}
	t.Cleanup(func() { SupportedSchemaRanges = orig })

	wfs := []spi.WorkflowDefinition{
		{Name: "wf-aged-out", Version: "1.1", InitialState: "S",
			States: map[string]spi.StateDefinition{"S": {}}},
	}
	err := validateSchemaVersions(wfs)
	if err == nil {
		t.Fatalf("validateSchemaVersions(\"1.1\") with MinMinor=2 = nil; want error")
	}
	if !errors.Is(err, ErrSchemaMinorTooOld) {
		t.Fatalf("error %v is not ErrSchemaMinorTooOld", err)
	}
	if !strings.Contains(err.Error(), "wf-aged-out") {
		t.Fatalf("error message %q does not name workflow wf-aged-out", err.Error())
	}
}

func TestValidateSchemaVersions_NamesOffendingWorkflowInMixedList(t *testing.T) {
	t.Parallel()
	wfs := []spi.WorkflowDefinition{
		{Name: "good-wf", Version: "1.1", InitialState: "S",
			States: map[string]spi.StateDefinition{"S": {}}},
		{Name: "bad-wf", Version: "2.0", InitialState: "S",
			States: map[string]spi.StateDefinition{"S": {}}},
	}
	err := validateSchemaVersions(wfs)
	if err == nil {
		t.Fatalf("validateSchemaVersions(mixed) = nil; want error")
	}
	if !strings.Contains(err.Error(), "bad-wf") {
		t.Fatalf("error message %q does not name offending workflow bad-wf", err.Error())
	}
	if strings.Contains(err.Error(), "good-wf") {
		t.Fatalf("error message %q wrongly names compliant workflow good-wf", err.Error())
	}
}
