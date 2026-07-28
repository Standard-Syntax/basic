package contracts

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"time"

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
	ActualDiff                   []ActualDiffFile
	ScopeReport                  ScopeReport
	IndependentEvidence          []IndependentEvidence
	AcceptanceCoverage           []AcceptanceEvidence
	Policy                       ReviewPolicy
}

type ActualDiffFile struct {
	Path         string
	Operation    FileOperation
	BeforeSHA256 string
	AfterSHA256  string
}

type ScopeReport struct {
	AuthorizedChangedPaths []string
	UnexpectedChangedPaths []string
}

type IndependentEvidence struct {
	ID              string
	CheckID         string
	CandidateCommit string
	ExitCode        int32
	OutputSHA256    string
	ArtifactURI     string
	StartedAt       time.Time
	CompletedAt     time.Time
}

type AcceptanceEvidence struct {
	AcceptanceCriterionID string
	EvidenceIDs           []string
}

type ReviewPolicy struct {
	BlockingSeverities       []string
	ReportUnrequestedChanges bool
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

type RequiredAction struct {
	ID          string
	FindingID   string
	Description string
}

type ResidualRisk struct {
	ID          string
	Description string
	Severity    string
}

type ReviewProposal struct {
	Recommendation     ReviewRecommendation
	Findings           []ReviewFinding
	RequiredActions    []RequiredAction
	UnrequestedChanges []string
	ResidualRisks      []ResidualRisk
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

	actualDiff := make([]ActualDiffFile, 0, len(value.GetActualDiff()))
	actualPaths := make(map[string]struct{}, len(value.GetActualDiff()))
	for _, changed := range value.GetActualDiff() {
		operation, ok := mapReviewFileOperation(changed.GetOperation())
		if !validRepoPath(changed.GetPath()) || !ok ||
			!digestPattern.MatchString(changed.GetBeforeSha256()) ||
			!digestPattern.MatchString(changed.GetAfterSha256()) {
			return ReviewRequest{}, errors.New("invalid actual diff")
		}
		if _, exists := actualPaths[changed.GetPath()]; exists {
			return ReviewRequest{}, errors.New("duplicate actual diff path")
		}
		actualPaths[changed.GetPath()] = struct{}{}
		actualDiff = append(actualDiff, ActualDiffFile{
			Path: changed.GetPath(), Operation: operation,
			BeforeSHA256: changed.GetBeforeSha256(), AfterSHA256: changed.GetAfterSha256(),
		})
	}
	authorizedPaths := value.GetScopeReport().GetAuthorizedChangedPaths()
	if len(authorizedPaths) != len(actualPaths) {
		return ReviewRequest{}, errors.New("scope report does not match actual diff")
	}
	seenAuthorized := make(map[string]struct{}, len(authorizedPaths))
	for _, authorizedPath := range authorizedPaths {
		if _, exists := actualPaths[authorizedPath]; !exists {
			return ReviewRequest{}, errors.New("scope report does not match actual diff")
		}
		if _, exists := seenAuthorized[authorizedPath]; exists {
			return ReviewRequest{}, errors.New("scope report contains duplicate path")
		}
		seenAuthorized[authorizedPath] = struct{}{}
	}

	knownEvidence := make(map[string]struct{}, len(value.GetIndependentEvidence()))
	passingEvidence := make(map[string]struct{}, len(value.GetIndependentEvidence()))
	evidenceValues := make([]IndependentEvidence, 0, len(value.GetIndependentEvidence()))
	for _, evidence := range value.GetIndependentEvidence() {
		if !evidenceIDPattern.MatchString(evidence.GetEvidenceId()) ||
			!checkIDPattern.MatchString(evidence.GetCheckId()) ||
			evidence.GetCandidateCommit() != candidate.GetCandidateCommit() ||
			!digestPattern.MatchString(evidence.GetOutputSha256()) ||
			evidence.GetArtifactUri() != "artifact://sha256/"+evidence.GetOutputSha256() ||
			evidence.GetStartedAt() == nil || evidence.GetCompletedAt() == nil {
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
		if evidence.GetExitCode() == 0 {
			passingEvidence[evidence.GetEvidenceId()] = struct{}{}
		}
		evidenceValues = append(evidenceValues, IndependentEvidence{
			ID: evidence.GetEvidenceId(), CheckID: evidence.GetCheckId(),
			CandidateCommit: evidence.GetCandidateCommit(), ExitCode: evidence.GetExitCode(),
			OutputSHA256: evidence.GetOutputSha256(), ArtifactURI: evidence.GetArtifactUri(),
			StartedAt: evidence.GetStartedAt().AsTime(), CompletedAt: evidence.GetCompletedAt().AsTime(),
		})
	}

	coverageValues := make([]AcceptanceEvidence, 0, len(value.GetAcceptanceCoverage()))
	for _, coverage := range value.GetAcceptanceCoverage() {
		if !criterionID.MatchString(coverage.GetAcceptanceCriterionId()) ||
			len(coverage.GetEvidenceIds()) == 0 {
			return ReviewRequest{}, errors.New("invalid acceptance evidence coverage")
		}
		for _, evidenceID := range coverage.GetEvidenceIds() {
			if _, exists := passingEvidence[evidenceID]; !exists {
				return ReviewRequest{}, errors.New("acceptance coverage requires passing evidence")
			}
		}
		coverageValues = append(coverageValues, AcceptanceEvidence{
			AcceptanceCriterionID: coverage.GetAcceptanceCriterionId(),
			EvidenceIDs:           coverage.GetEvidenceIds(),
		})
	}

	blockingSeverities := make([]string, 0, len(value.GetReviewPolicy().GetBlockingSeverities()))
	for _, severity := range value.GetReviewPolicy().GetBlockingSeverities() {
		if severity == reasoningv1.FindingSeverity_FINDING_SEVERITY_UNSPECIFIED {
			return ReviewRequest{}, errors.New("invalid blocking severity")
		}
		blockingSeverities = append(blockingSeverities, severity.String())
	}
	return ReviewRequest{
		Envelope:                     envelope,
		ApprovedSpecificationDigest:  candidate.GetApprovedSpecificationDigest(),
		ApprovedTaskDigest:           candidate.GetApprovedTaskDigest(),
		BaseCommit:                   candidate.GetBaseCommit(),
		CandidateCommit:              candidate.GetCandidateCommit(),
		ImplementationProposalDigest: candidate.GetImplementationProposalDigest(),
		ActualDiff:                   actualDiff,
		ScopeReport: ScopeReport{
			AuthorizedChangedPaths: authorizedPaths,
			UnexpectedChangedPaths: value.GetScopeReport().GetUnexpectedChangedPaths(),
		},
		IndependentEvidence: evidenceValues,
		AcceptanceCoverage:  coverageValues,
		Policy: ReviewPolicy{
			BlockingSeverities:       blockingSeverities,
			ReportUnrequestedChanges: value.GetReviewPolicy().GetReportUnrequestedChanges(),
		},
	}, nil
}

func mapReviewFileOperation(value reasoningv1.FileOperation) (FileOperation, bool) {
	switch value {
	case reasoningv1.FileOperation_FILE_OPERATION_CREATE:
		return FileCreate, true
	case reasoningv1.FileOperation_FILE_OPERATION_UPDATE:
		return FileUpdate, true
	case reasoningv1.FileOperation_FILE_OPERATION_DELETE:
		return FileDelete, true
	default:
		return "", false
	}
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

	knownEvidence := make(map[string]struct{}, len(request.IndependentEvidence))
	for _, evidence := range request.IndependentEvidence {
		knownEvidence[evidence.ID] = struct{}{}
	}
	findingIDs := make(map[string]struct{}, len(value.GetFindings()))
	hasBlockingFinding := false
	findings := make([]ReviewFinding, 0, len(value.GetFindings()))
	for _, finding := range value.GetFindings() {
		if finding.GetFindingId() == "" ||
			finding.GetSeverity() == reasoningv1.FindingSeverity_FINDING_SEVERITY_UNSPECIFIED ||
			finding.GetCategory() == reasoningv1.FindingCategory_FINDING_CATEGORY_UNSPECIFIED ||
			finding.GetSummary() == "" || len(finding.GetEvidenceReferences()) == 0 {
			return ReviewProposal{}, errors.New("invalid review finding")
		}
		if _, exists := findingIDs[finding.GetFindingId()]; exists {
			return ReviewProposal{}, errors.New("duplicate review finding")
		}
		for _, reference := range finding.GetEvidenceReferences() {
			if _, exists := knownEvidence[reference]; !exists {
				return ReviewProposal{}, errors.New("finding references unknown evidence")
			}
		}
		severity := finding.GetSeverity().String()
		if slices.Contains(request.Policy.BlockingSeverities, severity) {
			hasBlockingFinding = true
		}
		findingIDs[finding.GetFindingId()] = struct{}{}
		findings = append(findings, ReviewFinding{
			ID: finding.GetFindingId(), Severity: severity,
			Category: finding.GetCategory().String(), Summary: finding.GetSummary(),
			EvidenceReferences: finding.GetEvidenceReferences(),
		})
	}
	if hasBlockingFinding && recommendation != ReviewReworkRequired {
		return ReviewProposal{}, errors.New("blocking finding requires rework recommendation")
	}

	actions := make([]RequiredAction, 0, len(value.GetRequiredActions()))
	for _, action := range value.GetRequiredActions() {
		if action.GetActionId() == "" || action.GetFindingId() == "" || action.GetDescription() == "" {
			return ReviewProposal{}, errors.New("invalid required action")
		}
		if _, exists := findingIDs[action.GetFindingId()]; !exists {
			return ReviewProposal{}, errors.New("required action references unknown finding")
		}
		actions = append(actions, RequiredAction{
			ID: action.GetActionId(), FindingID: action.GetFindingId(),
			Description: action.GetDescription(),
		})
	}
	risks := make([]ResidualRisk, 0, len(value.GetResidualRisks()))
	for _, risk := range value.GetResidualRisks() {
		if risk.GetRiskId() == "" || risk.GetDescription() == "" ||
			risk.GetSeverity() == reasoningv1.FindingSeverity_FINDING_SEVERITY_UNSPECIFIED {
			return ReviewProposal{}, errors.New("invalid residual risk")
		}
		risks = append(risks, ResidualRisk{
			ID: risk.GetRiskId(), Description: risk.GetDescription(),
			Severity: risk.GetSeverity().String(),
		})
	}
	return ReviewProposal{
		Recommendation: recommendation, Findings: findings, RequiredActions: actions,
		UnrequestedChanges: value.GetUnrequestedChanges(), ResidualRisks: risks,
		Assumptions: value.GetAssumptions(),
	}, nil
}
