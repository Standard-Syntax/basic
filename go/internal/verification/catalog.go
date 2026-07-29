package verification

import (
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/Standard-Syntax/basic/go/internal/reasoning/contracts"
)

var checkIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

type Catalog struct {
	definitions []CheckDefinition
	byID        map[string]CheckDefinition
}

func DefaultCatalog() Catalog {
	catalog, err := NewCatalog([]CheckDefinition{{
		ID:               "make-check-v1",
		CommandReference: "make-check-v1",
		Argv:             []string{"make", "check"},
		Timeout:          DefaultCheckTimeout,
		Limits: ResourceLimits{
			CPUs: 1, MemoryBytes: 1 << 30, PIDs: 256, OutputBytes: DefaultMaxOutputBytes,
		},
	}})
	if err != nil {
		panic(err)
	}
	return catalog
}

func NewCatalog(definitions []CheckDefinition) (Catalog, error) {
	if len(definitions) == 0 || len(definitions) > DefaultMaxChecks {
		return Catalog{}, fmt.Errorf("%w: catalog size", ErrInvalidRequest)
	}
	catalog := Catalog{
		definitions: make([]CheckDefinition, 0, len(definitions)),
		byID:        make(map[string]CheckDefinition, len(definitions)),
	}
	for _, definition := range definitions {
		if !validCheckDefinition(definition) {
			return Catalog{}, fmt.Errorf("%w: invalid check %q", ErrInvalidRequest, definition.ID)
		}
		if _, exists := catalog.byID[definition.ID]; exists {
			return Catalog{}, fmt.Errorf("%w: duplicate check %q", ErrInvalidRequest, definition.ID)
		}
		definition.Argv = slices.Clone(definition.Argv)
		catalog.definitions = append(catalog.definitions, definition)
		catalog.byID[definition.ID] = definition
	}
	return catalog, nil
}

func validCheckDefinition(definition CheckDefinition) bool {
	return checkIDPattern.MatchString(definition.ID) &&
		checkIDPattern.MatchString(definition.CommandReference) &&
		len(definition.Argv) > 0 &&
		durationIsBounded(definition.Timeout) &&
		resourceLimitsAreBounded(definition.Limits)
}

func durationIsBounded(value time.Duration) bool {
	return value > 0 && value <= DefaultCheckTimeout
}

func resourceLimitsAreBounded(limits ResourceLimits) bool {
	return limits.CPUs == 1 &&
		limits.MemoryBytes > 0 && limits.MemoryBytes <= 1<<30 &&
		limits.PIDs > 0 && limits.PIDs <= 256 &&
		limits.OutputBytes > 0 && limits.OutputBytes <= DefaultMaxOutputBytes
}

func (c Catalog) Resolve(
	request contracts.ImplementationRequest,
	requirements []AcceptanceRequirement,
) (ResolvedPlan, error) {
	if len(requirements) != len(request.AcceptanceCriterionIDs) {
		return ResolvedPlan{}, fmt.Errorf("%w: criterion set mismatch", ErrInvalidRequest)
	}
	available := make(map[string]struct{}, len(request.AvailableCheckIDs))
	for _, id := range request.AvailableCheckIDs {
		available[id] = struct{}{}
	}
	criteria := make(map[string]struct{}, len(request.AcceptanceCriterionIDs))
	for _, id := range request.AcceptanceCriterionIDs {
		criteria[id] = struct{}{}
	}
	seenCriteria := make(map[string]struct{}, len(requirements))
	selected := make(map[string]struct{})
	resolvedRequirements := make([]AcceptanceRequirement, 0, len(requirements))
	for _, requirement := range requirements {
		if _, ok := criteria[requirement.CriterionID]; !ok {
			return ResolvedPlan{}, fmt.Errorf("%w: unknown criterion %q", ErrInvalidRequest, requirement.CriterionID)
		}
		if _, duplicate := seenCriteria[requirement.CriterionID]; duplicate {
			return ResolvedPlan{}, fmt.Errorf("%w: duplicate criterion %q", ErrInvalidRequest, requirement.CriterionID)
		}
		seenCriteria[requirement.CriterionID] = struct{}{}
		checks := slices.Clone(requirement.CheckIDs)
		seenChecks := make(map[string]struct{}, len(checks))
		for _, id := range checks {
			if _, duplicate := seenChecks[id]; duplicate {
				return ResolvedPlan{}, fmt.Errorf("%w: duplicate mapped check %q", ErrInvalidRequest, id)
			}
			seenChecks[id] = struct{}{}
			if _, ok := available[id]; !ok {
				return ResolvedPlan{}, fmt.Errorf("%w: unavailable check %q", ErrInvalidRequest, id)
			}
			if _, ok := c.byID[id]; !ok {
				return ResolvedPlan{}, fmt.Errorf("%w: untrusted check %q", ErrInvalidRequest, id)
			}
			selected[id] = struct{}{}
		}
		resolvedRequirements = append(resolvedRequirements, AcceptanceRequirement{
			CriterionID: requirement.CriterionID, CheckIDs: checks,
		})
	}
	checks := make([]CheckDefinition, 0, len(selected))
	for _, definition := range c.definitions {
		if _, ok := selected[definition.ID]; ok {
			definition.Argv = slices.Clone(definition.Argv)
			checks = append(checks, definition)
		}
	}
	if len(checks) > DefaultMaxChecks {
		return ResolvedPlan{}, fmt.Errorf("%w: too many checks", ErrInvalidRequest)
	}
	return ResolvedPlan{Checks: checks, Requirements: resolvedRequirements}, nil
}

func CalculateCoverage(
	requirements []AcceptanceRequirement, results []CheckResult,
) ([]CriterionCoverage, bool) {
	passedChecks := make(map[string]bool, len(results))
	for _, result := range results {
		passedChecks[result.CheckID] = result.Passed
	}
	coverage := make([]CriterionCoverage, 0, len(requirements))
	overall := true
	for _, requirement := range requirements {
		item := CriterionCoverage{
			CriterionID: requirement.CriterionID,
			CheckIDs:    slices.Clone(requirement.CheckIDs),
			Covered:     len(requirement.CheckIDs) > 0,
			Passed:      len(requirement.CheckIDs) > 0,
		}
		for _, checkID := range requirement.CheckIDs {
			passed, present := passedChecks[checkID]
			if !present || !passed {
				item.Passed = false
			}
		}
		if !item.Covered || !item.Passed {
			overall = false
		}
		coverage = append(coverage, item)
	}
	return coverage, overall
}
