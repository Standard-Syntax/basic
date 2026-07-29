package verification

import (
	"errors"
	"reflect"
	"testing"

	"github.com/Standard-Syntax/basic/go/internal/reasoning/contracts"
)

func TestCatalogResolvesTrustedChecksInCatalogOrder(t *testing.T) {
	catalog := testCatalog(t)
	request := contracts.ImplementationRequest{
		AcceptanceCriterionIDs: []string{"AC-001", "AC-002"},
		AvailableCheckIDs:      []string{"second-v1", "first-v1"},
	}
	plan, err := catalog.Resolve(request, []AcceptanceRequirement{
		{CriterionID: "AC-002", CheckIDs: []string{"second-v1"}},
		{CriterionID: "AC-001", CheckIDs: []string{"first-v1", "second-v1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := []string{plan.Checks[0].ID, plan.Checks[1].ID}
	if !reflect.DeepEqual(got, []string{"first-v1", "second-v1"}) {
		t.Fatalf("resolved order = %v", got)
	}
}

func TestCatalogRejectsKernelBindingErrors(t *testing.T) {
	catalog := DefaultCatalog()
	request := contracts.ImplementationRequest{
		AcceptanceCriterionIDs: []string{"AC-001"},
		AvailableCheckIDs:      []string{"make-check-v1", "unknown-v1"},
	}
	cases := map[string][]AcceptanceRequirement{
		"criterion mismatch": {},
		"duplicate criterion": {
			{CriterionID: "AC-001"}, {CriterionID: "AC-001"},
		},
		"untrusted check": {{CriterionID: "AC-001", CheckIDs: []string{"unknown-v1"}}},
		"duplicate check": {{
			CriterionID: "AC-001", CheckIDs: []string{"make-check-v1", "make-check-v1"},
		}},
	}
	for name, requirements := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := catalog.Resolve(request, requirements); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestCatalogBoundsCombinedOutput(t *testing.T) {
	definition := CheckDefinition{
		ID: "bounded-v1", CommandReference: "bounded-v1", Argv: []string{"true"},
		Timeout: DefaultCheckTimeout,
		Limits: ResourceLimits{
			CPUs: 1, MemoryBytes: 1 << 30, PIDs: 256, OutputBytes: DefaultMaxOutputBytes,
		},
	}
	if _, err := NewCatalog([]CheckDefinition{definition}); err != nil {
		t.Fatalf("maximum output limit rejected: %v", err)
	}
	definition.Limits.OutputBytes++
	if _, err := NewCatalog([]CheckDefinition{definition}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("over-maximum output limit error = %v", err)
	}
}

func TestCoverageUsesOnlyCheckEvidence(t *testing.T) {
	requirements := []AcceptanceRequirement{
		{CriterionID: "AC-001", CheckIDs: []string{"make-check-v1"}},
		{CriterionID: "AC-002"},
	}
	// Model completion prose is deliberately absent from this API. A passing
	// claim cannot affect failed evidence or missing kernel-selected coverage.
	coverage, passed := CalculateCoverage(requirements, []CheckResult{{
		CheckID: "make-check-v1", Passed: false,
	}})
	if passed || coverage[0].Passed || coverage[1].Covered || coverage[1].Passed {
		t.Fatalf("false evidence passed: %#v", coverage)
	}
}

func TestCoverageRequiresEveryMappedCheckAndRunsAreDeduplicated(t *testing.T) {
	requirements := []AcceptanceRequirement{{
		CriterionID: "AC-001", CheckIDs: []string{"first-v1", "second-v1"},
	}}
	coverage, passed := CalculateCoverage(requirements, []CheckResult{
		{CheckID: "first-v1", Passed: true},
		{CheckID: "second-v1", Passed: false},
	})
	if passed || coverage[0].Passed {
		t.Fatalf("partial evidence passed: %#v", coverage)
	}
}

func testCatalog(t *testing.T) Catalog {
	t.Helper()
	definition := func(id string) CheckDefinition {
		return CheckDefinition{
			ID: id, CommandReference: id, Argv: []string{"true"},
			Timeout: DefaultCheckTimeout,
			Limits: ResourceLimits{
				CPUs: 1, MemoryBytes: 1 << 30, PIDs: 256, OutputBytes: 1024,
			},
		}
	}
	catalog, err := NewCatalog([]CheckDefinition{definition("first-v1"), definition("second-v1")})
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}
