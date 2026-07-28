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
	Envelope                       Envelope
	ApprovedSpecificationDigest    string
	ApprovedTaskDigest             string
	BaseCommit                     string
	CandidateCommit                string
	ImplementationProposalDigest   string
	ActualDiff                     []ActualDiffFile
	ScopeReport                    ScopeReport
	IndependentEvidence            []IndependentEvidence
	AcceptanceCoverage             []AcceptanceEvidence
	Policy                         ReviewPolicy
	ApprovedAcceptanceCriterionIDs []string
	AuthorizedWritablePaths        []string
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
	if err := validateReviewCandidate(candidate); err != nil {
		return ReviewRequest{}, err
	}
	if err := validateReviewInput(value); err != nil {
		return ReviewRequest{}, err
	}
	actualDiff, actualPaths, err := mapActualDiff(value)
	if err != nil {
		return ReviewRequest{}, err
	}
	authorizedPaths, err := validateScopeReport(value.GetScopeReport(), actualPaths)
	if err != nil {
		return ReviewRequest{}, err
	}
	evidenceValues, passingEvidence, err := mapIndependentEvidence(
		value.GetIndependentEvidence(), candidate.GetCandidateCommit(),
	)
	if err != nil {
		return ReviewRequest{}, err
	}
	coverageValues, err := mapAcceptanceCoverage(
		value.GetAcceptanceCoverage(),
		value.GetApprovedAcceptanceCriterionIds(),
		passingEvidence,
	)
	if err != nil {
		return ReviewRequest{}, err
	}
	blockingSeverities, err := mapReviewPolicy(value.GetReviewPolicy())
	if err != nil {
		return ReviewRequest{}, err
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
		ApprovedAcceptanceCriterionIDs: value.GetApprovedAcceptanceCriterionIds(),
		AuthorizedWritablePaths:        value.GetAuthorizedWritablePaths(),
	}, nil
}

func validateReviewCandidate(candidate *reasoningv1.ReviewCandidateIdentity) error {
	if !digestPattern.MatchString(candidate.GetApprovedSpecificationDigest()) ||
		!digestPattern.MatchString(candidate.GetApprovedTaskDigest()) ||
		!commitPattern.MatchString(candidate.GetBaseCommit()) ||
		!commitPattern.MatchString(candidate.GetCandidateCommit()) ||
		!digestPattern.MatchString(candidate.GetImplementationProposalDigest()) ||
		candidate.GetBaseCommit() == candidate.GetCandidateCommit() {
		return errors.New("invalid review candidate identity")
	}
	return nil
}

func validateReviewInput(value *reasoningv1.ReviewRequest) error {
	if len(value.GetActualDiff()) == 0 || value.GetScopeReport() == nil ||
		len(value.GetIndependentEvidence()) == 0 || len(value.GetAcceptanceCoverage()) == 0 ||
		value.GetReviewPolicy() == nil || len(value.GetApprovedAcceptanceCriterionIds()) == 0 ||
		len(value.GetAuthorizedWritablePaths()) == 0 {
		return errors.New("incomplete independent review input")
	}
	if !validCriteria(value.GetApprovedAcceptanceCriterionIds()) {
		return errors.New("invalid approved acceptance criteria")
	}
	for _, writablePath := range value.GetAuthorizedWritablePaths() {
		if !validRepoPath(writablePath) {
			return errors.New("invalid authorized writable path")
		}
	}
	if len(value.GetScopeReport().GetUnexpectedChangedPaths()) != 0 {
		return errors.New("scope report contains unexpected changes")
	}
	return nil
}

func mapActualDiff(
	value *reasoningv1.ReviewRequest,
) ([]ActualDiffFile, map[string]struct{}, error) {
	actualDiff := make([]ActualDiffFile, 0, len(value.GetActualDiff()))
	actualPaths := make(map[string]struct{}, len(value.GetActualDiff()))
	for _, changed := range value.GetActualDiff() {
		operation, ok := mapReviewFileOperation(changed.GetOperation())
		if !validRepoPath(changed.GetPath()) || !ok ||
			!pathWithin(changed.GetPath(), value.GetAuthorizedWritablePaths()) ||
			!digestPattern.MatchString(changed.GetBeforeSha256()) ||
			!digestPattern.MatchString(changed.GetAfterSha256()) {
			return nil, nil, errors.New("invalid actual diff")
		}
		if _, exists := actualPaths[changed.GetPath()]; exists {
			return nil, nil, errors.New("duplicate actual diff path")
		}
		actualPaths[changed.GetPath()] = struct{}{}
		actualDiff = append(actualDiff, ActualDiffFile{
			Path: changed.GetPath(), Operation: operation,
			BeforeSHA256: changed.GetBeforeSha256(), AfterSHA256: changed.GetAfterSha256(),
		})
	}
	return actualDiff, actualPaths, nil
}

func validateScopeReport(
	value *reasoningv1.ScopeReport, actualPaths map[string]struct{},
) ([]string, error) {
	authorizedPaths := value.GetAuthorizedChangedPaths()
	if len(authorizedPaths) != len(actualPaths) {
		return nil, errors.New("scope report does not match actual diff")
	}
	seenAuthorized := make(map[string]struct{}, len(authorizedPaths))
	for _, authorizedPath := range authorizedPaths {
		if _, exists := actualPaths[authorizedPath]; !exists {
			return nil, errors.New("scope report does not match actual diff")
		}
		if _, exists := seenAuthorized[authorizedPath]; exists {
			return nil, errors.New("scope report contains duplicate path")
		}
		seenAuthorized[authorizedPath] = struct{}{}
	}
	return authorizedPaths, nil
}

func mapIndependentEvidence(
	values []*reasoningv1.IndependentEvidence, candidateCommit string,
) ([]IndependentEvidence, map[string]struct{}, error) {
	knownEvidence := make(map[string]struct{}, len(values))
	passingEvidence := make(map[string]struct{}, len(values))
	evidenceValues := make([]IndependentEvidence, 0, len(values))
	for _, evidence := range values {
		if !evidenceIDPattern.MatchString(evidence.GetEvidenceId()) ||
			!checkIDPattern.MatchString(evidence.GetCheckId()) ||
			evidence.GetCandidateCommit() != candidateCommit ||
			!digestPattern.MatchString(evidence.GetOutputSha256()) ||
			evidence.GetArtifactUri() != "artifact://sha256/"+evidence.GetOutputSha256() ||
			evidence.GetStartedAt() == nil || evidence.GetCompletedAt() == nil {
			return nil, nil, errors.New("invalid independent evidence")
		}
		if evidence.GetStartedAt().CheckValid() != nil || evidence.GetCompletedAt().CheckValid() != nil ||
			!evidence.GetCompletedAt().AsTime().After(evidence.GetStartedAt().AsTime()) {
			return nil, nil, errors.New("invalid independent evidence timestamps")
		}
		if _, exists := knownEvidence[evidence.GetEvidenceId()]; exists {
			return nil, nil, errors.New("duplicate independent evidence")
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
	return evidenceValues, passingEvidence, nil
}

func mapAcceptanceCoverage(
	values []*reasoningv1.AcceptanceEvidence,
	approvedCriteria []string,
	passingEvidence map[string]struct{},
) ([]AcceptanceEvidence, error) {
	coverageValues := make([]AcceptanceEvidence, 0, len(values))
	coveredCriteria := make(map[string]struct{}, len(values))
	for _, coverage := range values {
		if !criterionID.MatchString(coverage.GetAcceptanceCriterionId()) ||
			!slices.Contains(approvedCriteria, coverage.GetAcceptanceCriterionId()) ||
			len(coverage.GetEvidenceIds()) == 0 {
			return nil, errors.New("invalid acceptance evidence coverage")
		}
		if _, exists := coveredCriteria[coverage.GetAcceptanceCriterionId()]; exists {
			return nil, errors.New("duplicate acceptance evidence coverage")
		}
		coveredCriteria[coverage.GetAcceptanceCriterionId()] = struct{}{}
		for _, evidenceID := range coverage.GetEvidenceIds() {
			if _, exists := passingEvidence[evidenceID]; !exists {
				return nil, errors.New("acceptance coverage requires passing evidence")
			}
		}
		coverageValues = append(coverageValues, AcceptanceEvidence{
			AcceptanceCriterionID: coverage.GetAcceptanceCriterionId(),
			EvidenceIDs:           coverage.GetEvidenceIds(),
		})
	}
	if len(coveredCriteria) != len(approvedCriteria) {
		return nil, errors.New("required acceptance evidence coverage missing")
	}
	return coverageValues, nil
}

func mapReviewPolicy(value *reasoningv1.ReviewPolicy) ([]string, error) {
	blockingSeverities := make([]string, 0, len(value.GetBlockingSeverities()))
	hasCritical := false
	for _, severity := range value.GetBlockingSeverities() {
		if severity == reasoningv1.FindingSeverity_FINDING_SEVERITY_UNSPECIFIED {
			return nil, errors.New("invalid blocking severity")
		}
		blockingSeverities = append(blockingSeverities, severity.String())
		if severity == reasoningv1.FindingSeverity_FINDING_SEVERITY_CRITICAL {
			hasCritical = true
		}
	}
	if len(blockingSeverities) == 0 || !hasCritical {
		return nil, errors.New("review policy must block critical findings")
	}
	return blockingSeverities, nil
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
	recommendation, err := mapReviewRecommendation(value.GetRecommendation())
	if err != nil {
		return ReviewProposal{}, err
	}
	knownEvidence := collectReviewEvidenceIDs(request.IndependentEvidence)
	findings, findingIDs, hasBlockingFinding, err := mapReviewFindings(
		value.GetFindings(), knownEvidence, request.Policy,
	)
	if err != nil {
		return ReviewProposal{}, err
	}
	if hasBlockingFinding && recommendation != ReviewReworkRequired {
		return ReviewProposal{}, errors.New("blocking finding requires rework recommendation")
	}
	actions, err := mapRequiredActions(value.GetRequiredActions(), findingIDs)
	if err != nil {
		return ReviewProposal{}, err
	}
	risks, err := mapResidualRisks(value.GetResidualRisks())
	if err != nil {
		return ReviewProposal{}, err
	}
	return ReviewProposal{
		Recommendation: recommendation, Findings: findings, RequiredActions: actions,
		UnrequestedChanges: value.GetUnrequestedChanges(), ResidualRisks: risks,
		Assumptions: value.GetAssumptions(),
	}, nil
}

func mapReviewRecommendation(
	value reasoningv1.ReviewRecommendation,
) (ReviewRecommendation, error) {
	switch value {
	case reasoningv1.ReviewRecommendation_REVIEW_RECOMMENDATION_REWORK_REQUIRED:
		return ReviewReworkRequired, nil
	case reasoningv1.ReviewRecommendation_REVIEW_RECOMMENDATION_ADVISORY_ACCEPT:
		return ReviewAdvisoryAccept, nil
	default:
		return "", errors.New("invalid review recommendation")
	}
}

func collectReviewEvidenceIDs(values []IndependentEvidence) map[string]struct{} {
	knownEvidence := make(map[string]struct{}, len(values))
	for _, evidence := range values {
		knownEvidence[evidence.ID] = struct{}{}
	}
	return knownEvidence
}

func mapReviewFindings(
	values []*reasoningv1.ReviewFinding,
	knownEvidence map[string]struct{},
	policy ReviewPolicy,
) ([]ReviewFinding, map[string]struct{}, bool, error) {
	findingIDs := make(map[string]struct{}, len(values))
	hasBlockingFinding := false
	findings := make([]ReviewFinding, 0, len(values))
	for _, finding := range values {
		if finding.GetFindingId() == "" ||
			finding.GetSeverity() == reasoningv1.FindingSeverity_FINDING_SEVERITY_UNSPECIFIED ||
			finding.GetCategory() == reasoningv1.FindingCategory_FINDING_CATEGORY_UNSPECIFIED ||
			finding.GetSummary() == "" || len(finding.GetEvidenceReferences()) == 0 {
			return nil, nil, false, errors.New("invalid review finding")
		}
		if _, exists := findingIDs[finding.GetFindingId()]; exists {
			return nil, nil, false, errors.New("duplicate review finding")
		}
		for _, reference := range finding.GetEvidenceReferences() {
			if _, exists := knownEvidence[reference]; !exists {
				return nil, nil, false, errors.New("finding references unknown evidence")
			}
		}
		severity := finding.GetSeverity().String()
		if slices.Contains(policy.BlockingSeverities, severity) {
			hasBlockingFinding = true
		}
		findingIDs[finding.GetFindingId()] = struct{}{}
		findings = append(findings, ReviewFinding{
			ID: finding.GetFindingId(), Severity: severity,
			Category: finding.GetCategory().String(), Summary: finding.GetSummary(),
			EvidenceReferences: finding.GetEvidenceReferences(),
		})
	}
	return findings, findingIDs, hasBlockingFinding, nil
}

func mapRequiredActions(
	values []*reasoningv1.RequiredAction, findingIDs map[string]struct{},
) ([]RequiredAction, error) {
	actions := make([]RequiredAction, 0, len(values))
	for _, action := range values {
		if action.GetActionId() == "" || action.GetFindingId() == "" || action.GetDescription() == "" {
			return nil, errors.New("invalid required action")
		}
		if _, exists := findingIDs[action.GetFindingId()]; !exists {
			return nil, errors.New("required action references unknown finding")
		}
		actions = append(actions, RequiredAction{
			ID: action.GetActionId(), FindingID: action.GetFindingId(),
			Description: action.GetDescription(),
		})
	}
	return actions, nil
}

func mapResidualRisks(values []*reasoningv1.ResidualRisk) ([]ResidualRisk, error) {
	risks := make([]ResidualRisk, 0, len(values))
	for _, risk := range values {
		if risk.GetRiskId() == "" || risk.GetDescription() == "" ||
			risk.GetSeverity() == reasoningv1.FindingSeverity_FINDING_SEVERITY_UNSPECIFIED {
			return nil, errors.New("invalid residual risk")
		}
		risks = append(risks, ResidualRisk{
			ID: risk.GetRiskId(), Description: risk.GetDescription(),
			Severity: risk.GetSeverity().String(),
		})
	}
	return risks, nil
}
