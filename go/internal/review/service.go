package review

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"time"

	reasoningv1 "github.com/Standard-Syntax/basic/go/gen/harness/reasoning/v1"
	"github.com/Standard-Syntax/basic/go/internal/execution"
	"github.com/Standard-Syntax/basic/go/internal/reasoning/contracts"
	"github.com/Standard-Syntax/basic/go/internal/reasoning/gateway"
	"github.com/Standard-Syntax/basic/go/internal/verification"
	"github.com/Standard-Syntax/basic/go/internal/workflow"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

var resourceLabelPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

type Service struct {
	config    Config
	artifacts ArtifactStore
	gateway   ReviewGateway
	workflow  WorkflowStore
	now       func() time.Time
}

func NewService(
	config Config, artifacts ArtifactStore, gateway ReviewGateway, workflowStore WorkflowStore,
) (*Service, error) {
	if _, err := uuid.Parse(config.ActorID); err != nil {
		return nil, errors.New("review actor ID is required")
	}
	if artifacts == nil || gateway == nil || workflowStore == nil {
		return nil, errors.New("artifact store, review gateway, and workflow store are required")
	}
	return &Service{
		config: config, artifacts: artifacts, gateway: gateway,
		workflow: workflowStore, now: time.Now,
	}, nil
}

func (s *Service) Review(ctx context.Context, request Request) (Result, error) {
	mapped, executionReport, verificationReport, err := s.validateRequest(ctx, request)
	if err != nil {
		return Result{}, err
	}
	outcome, err := s.gateway.ProposeReview(ctx, proto.Clone(request.Review).(*reasoningv1.ReviewRequest))
	if err != nil {
		return Result{}, fmt.Errorf("propose advisory review: %w", err)
	}
	if outcome.Rejection != nil || outcome.Proposal == nil {
		return Result{}, fmt.Errorf("%w: reviewer rejected request", ErrInvalidRequest)
	}
	proposal, err := contracts.MapReviewProposal(outcome.Proposal, mapped)
	if err != nil {
		return Result{}, fmt.Errorf("%w: review proposal: %v", ErrInvalidRequest, err)
	}
	blocking := blockingFindingIDs(outcome.Proposal.GetFindings())
	if err := enforceProposalPolicy(request.Review, outcome.Proposal, blocking); err != nil {
		return Result{}, err
	}
	passed := len(blocking) == 0 &&
		proposal.Recommendation == contracts.ReviewAdvisoryAccept
	report := ReviewReport{
		SchemaVersion: "1", ReviewID: request.ReviewID,
		ReviewedAt: request.ReviewTimestamp.UTC().Format(time.RFC3339Nano),
		RunID:      mapped.Envelope.RunID, TaskID: *mapped.Envelope.TaskID,
		Attempt: mapped.Envelope.Attempt, CandidateCommit: mapped.CandidateCommit,
		ApprovedSpecificationDigest:  mapped.ApprovedSpecificationDigest,
		ApprovedTaskDigest:           mapped.ApprovedTaskDigest,
		ImplementationProposalDigest: mapped.ImplementationProposalDigest,
		Request:                      artifactRef(outcome.RequestArtifact), Proposal: artifactRef(outcome.ProposalArtifact),
		Execution: request.ExecutionArtifact, Verification: request.VerificationArtifact,
		Findings:        cloneFindings(outcome.Proposal.GetFindings()),
		RequiredActions: cloneActions(outcome.Proposal.GetRequiredActions()),
		Recommendation:  outcome.Proposal.GetRecommendation().String(),
		Risk: RiskAssessment{
			BlockingFindingIDs: blocking,
			UnrequestedChanges: slices.Clone(outcome.Proposal.GetUnrequestedChanges()),
			ExclusiveResources: slices.Clone(request.ExclusiveResourceLabels),
		},
		Passed: passed,
	}
	body, err := json.Marshal(report)
	if err != nil {
		return Result{}, fmt.Errorf("encode review report: %w", err)
	}
	reportArtifact, err := s.storeContent(ctx, body)
	if err != nil {
		return Result{}, err
	}
	commandResult, err := s.workflow.ExecuteTask(ctx, workflow.RecordTaskReview{
		Meta: s.commandEnvelope(request, s.now().UTC()),
		Run:  mapped.Envelope.RunID, ID: *mapped.Envelope.TaskID,
		CandidateCommit: mapped.CandidateCommit, Review: reportArtifact, Passed: passed,
	})
	if err != nil {
		return Result{}, fmt.Errorf("record task review: %w", err)
	}
	_ = executionReport
	_ = verificationReport
	return Result{
		ReviewID: request.ReviewID, CandidateCommit: mapped.CandidateCommit,
		ReportArtifact: reportArtifact, Recommendation: outcome.Proposal.GetRecommendation(),
		Passed: passed, Replay: commandResult.Replay || outcome.Replay,
	}, nil
}

func (s *Service) validateRequest(
	ctx context.Context, request Request,
) (contracts.ReviewRequest, execution.ExecutionReport, verification.VerificationReport, error) {
	if _, err := uuid.Parse(request.ReviewID); err != nil || request.ReviewTimestamp.IsZero() ||
		request.Review == nil || request.ExpectedTaskRevision == 0 {
		return contracts.ReviewRequest{}, execution.ExecutionReport{},
			verification.VerificationReport{}, ErrInvalidRequest
	}
	for _, label := range request.ExclusiveResourceLabels {
		if !resourceLabelPattern.MatchString(label) {
			return contracts.ReviewRequest{}, execution.ExecutionReport{},
				verification.VerificationReport{}, fmt.Errorf("%w: resource label", ErrInvalidRequest)
		}
	}
	if err := fixedPolicy(request.Review.GetReviewPolicy()); err != nil {
		return contracts.ReviewRequest{}, execution.ExecutionReport{},
			verification.VerificationReport{}, err
	}
	mapped, err := contracts.MapReviewRequestAt(request.Review, s.now().UTC())
	if err != nil {
		return contracts.ReviewRequest{}, execution.ExecutionReport{},
			verification.VerificationReport{}, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	executionReport, err := loadJSON[execution.ExecutionReport](
		ctx, s.artifacts, request.ExecutionArtifact,
	)
	if err != nil {
		return contracts.ReviewRequest{}, execution.ExecutionReport{},
			verification.VerificationReport{}, err
	}
	verificationReport, err := loadJSON[verification.VerificationReport](
		ctx, s.artifacts, request.VerificationArtifact,
	)
	if err != nil {
		return contracts.ReviewRequest{}, execution.ExecutionReport{},
			verification.VerificationReport{}, err
	}
	if !executionMatches(executionReport, mapped, request.ExecutionArtifact) ||
		!verificationMatches(verificationReport, mapped, request.ExecutionArtifact) ||
		!request.VerificationArtifact.Equal(request.ReviewArtifactInput(
			request.VerificationArtifact.Digest,
		)) {
		return contracts.ReviewRequest{}, execution.ExecutionReport{},
			verification.VerificationReport{}, fmt.Errorf("%w: upstream evidence binding", ErrInvalidRequest)
	}
	if err := crossCheckDiff(request.Review, executionReport); err != nil {
		return contracts.ReviewRequest{}, execution.ExecutionReport{},
			verification.VerificationReport{}, err
	}
	if err := crossCheckEvidence(request.Review, verificationReport); err != nil {
		return contracts.ReviewRequest{}, execution.ExecutionReport{},
			verification.VerificationReport{}, err
	}
	if !envelopeContains(request.Review, request.ExecutionArtifact.Digest) ||
		!envelopeContains(request.Review, request.VerificationArtifact.Digest) ||
		!envelopeContains(request.Review, mapped.ImplementationProposalDigest) {
		return contracts.ReviewRequest{}, execution.ExecutionReport{},
			verification.VerificationReport{}, fmt.Errorf("%w: input artifact digest", ErrInvalidRequest)
	}
	return mapped, executionReport, verificationReport, nil
}

// ReviewArtifactInput returns the exact artifact reference represented by a
// digest in the review envelope. It is intentionally unexported authority-wise:
// callers cannot create workflow commands with it.
func (r Request) ReviewArtifactInput(digest string) workflow.ArtifactRef {
	for _, value := range r.Review.GetEnvelope().GetInputArtifacts() {
		if value.GetSha256() == digest {
			return workflow.ArtifactRef{URI: value.GetArtifactUri(), Digest: digest}
		}
	}
	return workflow.ArtifactRef{}
}

func fixedPolicy(policy *reasoningv1.ReviewPolicy) error {
	if policy == nil || !policy.GetReportUnrequestedChanges() ||
		len(policy.GetBlockingSeverities()) != 2 ||
		!slices.Contains(policy.GetBlockingSeverities(), reasoningv1.FindingSeverity_FINDING_SEVERITY_HIGH) ||
		!slices.Contains(policy.GetBlockingSeverities(), reasoningv1.FindingSeverity_FINDING_SEVERITY_CRITICAL) {
		return fmt.Errorf("%w: HIGH and CRITICAL must block", ErrPolicyViolation)
	}
	return nil
}

func executionMatches(
	report execution.ExecutionReport, request contracts.ReviewRequest, artifact workflow.ArtifactRef,
) bool {
	return report.SchemaVersion == "1" && report.RunID == request.Envelope.RunID &&
		report.TaskID == *request.Envelope.TaskID && report.Attempt == request.Envelope.Attempt &&
		report.BaseCommit == request.BaseCommit && report.CandidateCommit == request.CandidateCommit &&
		report.Proposal.Digest == request.ImplementationProposalDigest && artifact.URI != ""
}

func verificationMatches(
	report verification.VerificationReport,
	request contracts.ReviewRequest,
	executionArtifact workflow.ArtifactRef,
) bool {
	return report.SchemaVersion == "1" && report.Passed &&
		report.RunID == request.Envelope.RunID && report.TaskID == *request.Envelope.TaskID &&
		report.Attempt == request.Envelope.Attempt && report.BaseCommit == request.BaseCommit &&
		report.CandidateCommit == request.CandidateCommit &&
		report.Execution.Equal(executionArtifact)
}

func crossCheckDiff(request *reasoningv1.ReviewRequest, report execution.ExecutionReport) error {
	if len(request.GetActualDiff()) != len(report.ActualDiff) {
		return fmt.Errorf("%w: actual diff length", ErrInvalidRequest)
	}
	for index, actual := range report.ActualDiff {
		provided := request.GetActualDiff()[index]
		if provided.GetPath() != actual.Path ||
			provided.GetOperation().String() != fileOperation(actual.Operation).String() ||
			provided.GetBeforeSha256() != actual.BeforeSHA256 ||
			provided.GetAfterSha256() != actual.AfterSHA256 {
			return fmt.Errorf("%w: actual diff", ErrInvalidRequest)
		}
	}
	return nil
}

func fileOperation(value contracts.FileOperation) reasoningv1.FileOperation {
	switch value {
	case contracts.FileCreate:
		return reasoningv1.FileOperation_FILE_OPERATION_CREATE
	case contracts.FileUpdate:
		return reasoningv1.FileOperation_FILE_OPERATION_UPDATE
	case contracts.FileDelete:
		return reasoningv1.FileOperation_FILE_OPERATION_DELETE
	default:
		return reasoningv1.FileOperation_FILE_OPERATION_UNSPECIFIED
	}
}

func crossCheckEvidence(
	request *reasoningv1.ReviewRequest, report verification.VerificationReport,
) error {
	if len(request.GetIndependentEvidence()) != len(report.Checks) ||
		len(request.GetAcceptanceCoverage()) != len(report.Coverage) {
		return fmt.Errorf("%w: verification evidence length", ErrInvalidRequest)
	}
	evidenceByCheck := make(map[string]string, len(report.Checks))
	for index, check := range report.Checks {
		evidence := request.GetIndependentEvidence()[index]
		if evidence.GetCheckId() != check.CheckID ||
			evidence.GetCandidateCommit() != check.CandidateCommit ||
			evidence.GetExitCode() != int32(check.ExitCode) ||
			evidence.GetOutputSha256() != check.OutputDigest ||
			evidence.GetArtifactUri() != check.Output.URI ||
			evidence.GetStartedAt().AsTime().UTC().Format(time.RFC3339Nano) != check.StartedAt ||
			evidence.GetCompletedAt().AsTime().UTC().Format(time.RFC3339Nano) != check.FinishedAt {
			return fmt.Errorf("%w: verification evidence", ErrInvalidRequest)
		}
		evidenceByCheck[check.CheckID] = evidence.GetEvidenceId()
	}
	for index, coverage := range report.Coverage {
		provided := request.GetAcceptanceCoverage()[index]
		if provided.GetAcceptanceCriterionId() != coverage.CriterionID ||
			len(provided.GetEvidenceIds()) != len(coverage.CheckIDs) {
			return fmt.Errorf("%w: acceptance coverage", ErrInvalidRequest)
		}
		for checkIndex, checkID := range coverage.CheckIDs {
			if provided.GetEvidenceIds()[checkIndex] != evidenceByCheck[checkID] {
				return fmt.Errorf("%w: acceptance coverage", ErrInvalidRequest)
			}
		}
	}
	return nil
}

func enforceProposalPolicy(
	request *reasoningv1.ReviewRequest,
	proposal *reasoningv1.ReviewProposal,
	blocking []string,
) error {
	if len(blocking) > 0 &&
		proposal.GetRecommendation() != reasoningv1.ReviewRecommendation_REVIEW_RECOMMENDATION_REWORK_REQUIRED {
		return fmt.Errorf("%w: blocking finding accepted", ErrPolicyViolation)
	}
	for _, changed := range request.GetScopeReport().GetUnexpectedChangedPaths() {
		if !slices.Contains(proposal.GetUnrequestedChanges(), changed) {
			return fmt.Errorf("%w: unrequested change not reported", ErrPolicyViolation)
		}
	}
	return nil
}

func blockingFindingIDs(findings []*reasoningv1.ReviewFinding) []string {
	var result []string
	for _, finding := range findings {
		if finding.GetSeverity() == reasoningv1.FindingSeverity_FINDING_SEVERITY_HIGH ||
			finding.GetSeverity() == reasoningv1.FindingSeverity_FINDING_SEVERITY_CRITICAL {
			result = append(result, finding.GetFindingId())
		}
	}
	return result
}

func (s *Service) storeContent(
	ctx context.Context, body []byte,
) (workflow.ArtifactRef, error) {
	ref, err := s.artifacts.Put(ctx, body)
	if err != nil {
		return workflow.ArtifactRef{}, fmt.Errorf("store review report: %w", err)
	}
	sum := sha256.Sum256(body)
	if err := ref.Validate(); err != nil || ref.Digest != hex.EncodeToString(sum[:]) {
		return workflow.ArtifactRef{}, ErrArtifactIntegrity
	}
	return ref, nil
}

func loadJSON[T any](
	ctx context.Context, store ArtifactStore, ref workflow.ArtifactRef,
) (T, error) {
	var zero T
	if err := ref.Validate(); err != nil {
		return zero, fmt.Errorf("%w: artifact reference", ErrInvalidRequest)
	}
	body, err := store.Get(ctx, ref)
	if err != nil {
		return zero, fmt.Errorf("load review input artifact: %w", err)
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != ref.Digest {
		return zero, ErrArtifactIntegrity
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var value T
	if err := decoder.Decode(&value); err != nil {
		return zero, fmt.Errorf("%w: decode upstream report", ErrArtifactIntegrity)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return zero, fmt.Errorf("%w: upstream report trailer", ErrArtifactIntegrity)
	}
	return value, nil
}

func envelopeContains(request *reasoningv1.ReviewRequest, digest string) bool {
	for _, artifact := range request.GetEnvelope().GetInputArtifacts() {
		if artifact.GetSha256() == digest &&
			artifact.GetArtifactUri() == "artifact://sha256/"+digest {
			return true
		}
	}
	return false
}

func artifactRef(value gateway.ArtifactReference) workflow.ArtifactRef {
	return workflow.ArtifactRef{URI: value.URI, Digest: value.SHA256}
}

func cloneFindings(values []*reasoningv1.ReviewFinding) []*reasoningv1.ReviewFinding {
	result := make([]*reasoningv1.ReviewFinding, len(values))
	for index, value := range values {
		result[index] = proto.Clone(value).(*reasoningv1.ReviewFinding)
	}
	return result
}

func cloneActions(values []*reasoningv1.RequiredAction) []*reasoningv1.RequiredAction {
	result := make([]*reasoningv1.RequiredAction, len(values))
	for index, value := range values {
		result[index] = proto.Clone(value).(*reasoningv1.RequiredAction)
	}
	return result
}

func (s *Service) commandEnvelope(request Request, timestamp time.Time) workflow.CommandEnvelope {
	id := func(label string) string {
		return uuid.NewSHA1(
			uuid.NameSpaceURL, []byte("harness:review:"+request.ReviewID+":"+label),
		).String()
	}
	return workflow.CommandEnvelope{
		CommandID: id("record"), Actor: workflow.Actor{
			ID: s.config.ActorID, Kind: workflow.ActorReviewService,
		},
		ExpectedRevision: request.ExpectedTaskRevision, Timestamp: timestamp,
		CorrelationID: id("correlation"), CausationID: id("record:cause"),
	}
}
