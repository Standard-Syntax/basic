package contracts

import (
	"context"
	"errors"
	"regexp"

	reasoningv1 "github.com/Standard-Syntax/basic/go/gen/harness/reasoning/v1"
)

var evidenceIDPattern = regexp.MustCompile(`^EVIDENCE-[0-9]{3}$`)

type ReviewRequest struct {
	Envelope                     Envelope
	ApprovedSpecificationDigest  string
	ApprovedTaskDigest           string
	BaseCommit                   string
	CandidateCommit              string
	ImplementationProposalDigest string
}

type ReviewRecommendation string

const (
	ReviewReworkRequired ReviewRecommendation = "rework_required"
	ReviewAdvisoryAccept ReviewRecommendation = "advisory_accept"
)

type ReviewFinding struct {
	ID                 string
	Severity           string
	Category           string
	Summary            string
	EvidenceReferences []string
}

type ReviewProposal struct {
	Recommendation     ReviewRecommendation
	Findings           []ReviewFinding
	RequiredActions    []string
	UnrequestedChanges []string
	ResidualRisks      []string
	Assumptions        []string
}

type ReviewReasoner interface {
	ReviewCandidate(context.Context, ReviewRequest) (ReviewProposal, error)
}

func MapReviewRequest(value *reasoningv1.ReviewRequest) (ReviewRequest, error) {
	if value == nil || value.GetCandidate() == nil {
		return ReviewRequest{}, errors.New("review request and candidate are required")
	}
	envelope, err := MapEnvelope(value.GetEnvelope(), StageReview)
	if err != nil {
		return ReviewRequest{}, err
	}
	candidate := value.GetCandidate()
	if !digestPattern.MatchString(candidate.GetApprovedSpecificationDigest()) ||
		!digestPattern.MatchString(candidate.GetApprovedTaskDigest()) ||
		!commitPattern.MatchString(candidate.GetBaseCommit()) ||
		!commitPattern.MatchString(candidate.GetCandidateCommit()) ||
		!digestPattern.MatchString(candidate.GetImplementationProposalDigest()) ||
		candidate.GetBaseCommit() == candidate.GetCandidateCommit() {
		return ReviewRequest{}, errors.New("invalid review candidate identity")
	}
	if len(value.GetActualDiff()) == 0 || value.GetScopeReport() == nil ||
		len(value.GetIndependentEvidence()) == 0 || len(value.GetAcceptanceCoverage()) == 0 ||
		value.GetReviewPolicy() == nil {
		return ReviewRequest{}, errors.New("incomplete independent review input")
	}
	if len(value.GetScopeReport().GetUnexpectedChangedPaths()) != 0 {
		return ReviewRequest{}, errors.New("scope report contains unexpected changes")
	}
	for _, changed := range value.GetActualDiff() {
		if !validRepoPath(changed.GetPath()) ||
			changed.GetOperation() == reasoningv1.FileOperation_FILE_OPERATION_UNSPECIFIED ||
			!digestPattern.MatchString(changed.GetBeforeSha256()) ||
			!digestPattern.MatchString(changed.GetAfterSha256()) {
			return ReviewRequest{}, errors.New("invalid actual diff")
		}
	}
	knownEvidence := make(map[string]struct{}, len(value.GetIndependentEvidence()))
	for _, evidence := range value.GetIndependentEvidence() {
		if !evidenceIDPattern.MatchString(evidence.GetEvidenceId()) ||
			!checkIDPattern.MatchString(evidence.GetCheckId()) ||
			evidence.GetCandidateCommit() != candidate.GetCandidateCommit() ||
			!digestPattern.MatchString(evidence.GetOutputSha256()) ||
			evidence.GetArtifactUri() == "" || evidence.GetStartedAt() == nil ||
			evidence.GetCompletedAt() == nil {
			return ReviewRequest{}, errors.New("invalid independent evidence")
		}
		if evidence.GetStartedAt().CheckValid() != nil || evidence.GetCompletedAt().CheckValid() != nil ||
			!evidence.GetCompletedAt().AsTime().After(evidence.GetStartedAt().AsTime()) {
			return ReviewRequest{}, errors.New("invalid independent evidence timestamps")
		}
		if _, exists := knownEvidence[evidence.GetEvidenceId()]; exists {
			return ReviewRequest{}, errors.New("duplicate independent evidence")
		}
		knownEvidence[evidence.GetEvidenceId()] = struct{}{}
	}
	for _, coverage := range value.GetAcceptanceCoverage() {
		if !criterionID.MatchString(coverage.GetAcceptanceCriterionId()) ||
			len(coverage.GetEvidenceIds()) == 0 {
			return ReviewRequest{}, errors.New("invalid acceptance evidence coverage")
		}
		for _, evidenceID := range coverage.GetEvidenceIds() {
			if _, exists := knownEvidence[evidenceID]; !exists {
				return ReviewRequest{}, errors.New("unknown evidence reference")
			}
		}
	}
	return ReviewRequest{
		Envelope:                    envelope,
		ApprovedSpecificationDigest: candidate.GetApprovedSpecificationDigest(),
		ApprovedTaskDigest:          candidate.GetApprovedTaskDigest(),
		BaseCommit:                  candidate.GetBaseCommit(), CandidateCommit: candidate.GetCandidateCommit(),
		ImplementationProposalDigest: candidate.GetImplementationProposalDigest(),
	}, nil
}

func MapReviewProposal(
	value *reasoningv1.ReviewProposal, request ReviewRequest,
) (ReviewProposal, error) {
	if value == nil || value.GetIdentity() == nil {
		return ReviewProposal{}, errors.New("review proposal identity is required")
	}
	if err := validateProposalIdentity(value.GetIdentity(), request.Envelope, StageReview); err != nil {
		return ReviewProposal{}, err
	}
	var recommendation ReviewRecommendation
	switch value.GetRecommendation() {
	case reasoningv1.ReviewRecommendation_REVIEW_RECOMMENDATION_REWORK_REQUIRED:
		recommendation = ReviewReworkRequired
	case reasoningv1.ReviewRecommendation_REVIEW_RECOMMENDATION_ADVISORY_ACCEPT:
		recommendation = ReviewAdvisoryAccept
	default:
		return ReviewProposal{}, errors.New("invalid review recommendation")
	}
	findings := make([]ReviewFinding, 0, len(value.GetFindings()))
	for _, finding := range value.GetFindings() {
		if finding.GetFindingId() == "" ||
			finding.GetSeverity() == reasoningv1.FindingSeverity_FINDING_SEVERITY_UNSPECIFIED ||
			finding.GetCategory() == reasoningv1.FindingCategory_FINDING_CATEGORY_UNSPECIFIED ||
			finding.GetSummary() == "" {
			return ReviewProposal{}, errors.New("invalid review finding")
		}
		findings = append(findings, ReviewFinding{
			ID: finding.GetFindingId(), Severity: finding.GetSeverity().String(),
			Category: finding.GetCategory().String(), Summary: finding.GetSummary(),
			EvidenceReferences: finding.GetEvidenceReferences(),
		})
	}
	actions := make([]string, 0, len(value.GetRequiredActions()))
	for _, action := range value.GetRequiredActions() {
		if action.GetActionId() == "" || action.GetFindingId() == "" || action.GetDescription() == "" {
			return ReviewProposal{}, errors.New("invalid required action")
		}
		actions = append(actions, action.GetDescription())
	}
	risks := make([]string, 0, len(value.GetResidualRisks()))
	for _, risk := range value.GetResidualRisks() {
		if risk.GetRiskId() == "" || risk.GetDescription() == "" ||
			risk.GetSeverity() == reasoningv1.FindingSeverity_FINDING_SEVERITY_UNSPECIFIED {
			return ReviewProposal{}, errors.New("invalid residual risk")
		}
		risks = append(risks, risk.GetDescription())
	}
	return ReviewProposal{
		Recommendation: recommendation, Findings: findings, RequiredActions: actions,
		UnrequestedChanges: value.GetUnrequestedChanges(), ResidualRisks: risks,
		Assumptions: value.GetAssumptions(),
	}, nil
}
