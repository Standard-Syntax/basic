package contracts

import (
	"context"
	"errors"
	"regexp"

	reasoningv1 "github.com/Standard-Syntax/basic/go/gen/harness/reasoning/v1"
)

var criterionID = regexp.MustCompile(`^AC-[0-9]{3}$`)

type SpecificationRequest struct {
	Envelope          Envelope
	ProblemStatement  string
	DesiredOutcome    string
	KnownConstraints  []string
	KnownNonGoals     []string
	Stakeholders      []string
	RepositorySummary *string
}

type AcceptanceCriterion struct {
	ID                 string
	Description        string
	VerificationMethod string
}

type SpecificationProposal struct {
	Title              string
	Goal               string
	Actors             []string
	Constraints        []string
	NonGoals           []string
	AcceptanceCriteria []AcceptanceCriterion
	Assumptions        []string
}

type SpecificationReasoner interface {
	DraftSpecification(context.Context, SpecificationRequest) (SpecificationProposal, error)
}

func MapSpecificationRequest(value *reasoningv1.SpecificationRequest) (SpecificationRequest, error) {
	if value == nil {
		return SpecificationRequest{}, errors.New("specification request is required")
	}
	envelope, err := MapEnvelope(value.GetEnvelope(), StageSpecification)
	if err != nil {
		return SpecificationRequest{}, err
	}
	if value.GetProblemStatement() == "" || value.GetDesiredOutcome() == "" ||
		len(value.GetStakeholders()) == 0 {
		return SpecificationRequest{}, errors.New("incomplete specification request")
	}
	return SpecificationRequest{
		Envelope:          envelope,
		ProblemStatement:  value.GetProblemStatement(),
		DesiredOutcome:    value.GetDesiredOutcome(),
		KnownConstraints:  value.GetKnownConstraints(),
		KnownNonGoals:     value.GetKnownNonGoals(),
		Stakeholders:      value.GetStakeholders(),
		RepositorySummary: value.RepositorySummary,
	}, nil
}

func MapSpecificationProposal(value *reasoningv1.SpecificationProposal) (SpecificationProposal, error) {
	if value == nil || value.GetIdentity() == nil {
		return SpecificationProposal{}, errors.New("specification proposal identity is required")
	}
	if value.GetIdentity().GetStage() != reasoningv1.ReasoningStage_REASONING_STAGE_SPECIFICATION ||
		value.GetIdentity().GetRequestId() == "" || value.GetIdentity().GetRunId() == "" ||
		value.GetIdentity().GetAttempt() == 0 ||
		!digestPattern.MatchString(value.GetIdentity().GetAgentManifestDigest()) {
		return SpecificationProposal{}, errors.New("invalid specification proposal identity")
	}
	if value.GetTitle() == "" || value.GetGoal() == "" || len(value.GetActors()) == 0 ||
		len(value.GetAcceptanceCriteria()) == 0 {
		return SpecificationProposal{}, errors.New("incomplete specification proposal")
	}
	seen := make(map[string]struct{}, len(value.GetAcceptanceCriteria()))
	criteria := make([]AcceptanceCriterion, 0, len(value.GetAcceptanceCriteria()))
	for _, criterion := range value.GetAcceptanceCriteria() {
		if !criterionID.MatchString(criterion.GetCriterionId()) ||
			criterion.GetDescription() == "" || criterion.GetVerificationMethod() == "" {
			return SpecificationProposal{}, errors.New("invalid acceptance criterion")
		}
		if _, exists := seen[criterion.GetCriterionId()]; exists {
			return SpecificationProposal{}, errors.New("duplicate acceptance criterion")
		}
		seen[criterion.GetCriterionId()] = struct{}{}
		criteria = append(criteria, AcceptanceCriterion{
			ID:                 criterion.GetCriterionId(),
			Description:        criterion.GetDescription(),
			VerificationMethod: criterion.GetVerificationMethod(),
		})
	}
	return SpecificationProposal{
		Title:              value.GetTitle(),
		Goal:               value.GetGoal(),
		Actors:             value.GetActors(),
		Constraints:        value.GetConstraints(),
		NonGoals:           value.GetNonGoals(),
		AcceptanceCriteria: criteria,
		Assumptions:        value.GetAssumptions(),
	}, nil
}
