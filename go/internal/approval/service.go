package approval

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Standard-Syntax/basic/go/internal/review"
	"github.com/Standard-Syntax/basic/go/internal/workflow"
	"github.com/google/uuid"
)

var (
	commitPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)
	digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
	taskPattern   = regexp.MustCompile(`^TASK-[0-9]{3}$`)
	labelPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
)

var protectedLabels = map[string]struct{}{
	"dependency-manifest": {}, "database-schema": {}, "public-api-contract": {},
	"authentication-policy": {}, "authorization-policy": {},
	"deployment-configuration": {},
}

type Service struct {
	artifacts ArtifactStore
	workflow  WorkflowStore
}

func NewService(artifacts ArtifactStore, workflowStore WorkflowStore) (*Service, error) {
	if artifacts == nil || workflowStore == nil {
		return nil, errors.New("approval artifact store and workflow store are required")
	}
	return &Service{artifacts: artifacts, workflow: workflowStore}, nil
}

func (s *Service) ApproveTask(ctx context.Context, request Request) (Result, error) {
	return s.decide(ctx, request, "approve", "")
}

func (s *Service) RequireTaskRework(
	ctx context.Context, request Request, reason string,
) (Result, error) {
	if strings.TrimSpace(reason) == "" || len(reason) > 4096 {
		return Result{}, fmt.Errorf("%w: rework reason", ErrInvalidRequest)
	}
	return s.decide(ctx, request, "rework", reason)
}

func (s *Service) decide(
	ctx context.Context, request Request, decision, reason string,
) (Result, error) {
	if err := s.validateRequest(ctx, request); err != nil {
		return Result{}, err
	}
	riskReasons := ClassifyElevatedRisk(
		request.ActualChangedPaths, request.ExclusiveResourceLabels,
	)
	elevated := len(riskReasons) > 0
	if err := authorize(request.Principal, elevated, decision); err != nil {
		return Result{}, err
	}
	approval := TaskApproval{
		SchemaVersion: "1", ApprovalID: request.ApprovalID,
		Approver: normalizedPrincipal(request.Principal), Decision: decision,
		DecisionTimestamp: request.DecisionTimestamp.UTC().Format(time.RFC3339Nano),
		Reason:            reason, Elevated: elevated, RiskReasons: riskReasons,
		RunID: request.RunID, TaskID: request.TaskID,
		CandidateCommit:             request.CandidateCommit,
		ApprovedSpecificationDigest: request.ApprovedSpecificationDigest,
		ApprovedTaskDigest:          request.ApprovedTaskDigest,
		Implementation:              request.Implementation, Execution: request.Execution,
		Verification: request.Verification, Review: request.Review,
	}
	body, err := json.Marshal(approval)
	if err != nil {
		return Result{}, fmt.Errorf("encode task approval: %w", err)
	}
	artifact, err := s.storeApproval(ctx, body)
	if err != nil {
		return Result{}, err
	}
	command := approvalCommand(request, decision, reason, artifact)
	workflowResult, err := s.workflow.ExecuteTask(ctx, command)
	if err != nil {
		return Result{}, fmt.Errorf("apply human task decision: %w", err)
	}
	return Result{
		ApprovalID: request.ApprovalID, Decision: decision, ApprovalArtifact: artifact,
		Elevated: elevated, RiskReasons: riskReasons, Replay: workflowResult.Replay,
	}, nil
}

func (s *Service) validateRequest(ctx context.Context, request Request) error {
	if err := validateRequestIdentity(request); err != nil {
		return err
	}
	if err := validateRiskInputs(request); err != nil {
		return err
	}
	if err := s.validateInputArtifacts(ctx, request); err != nil {
		return err
	}
	reportBody, err := fetchVerifiedArtifact(ctx, s.artifacts, request.Review)
	if err != nil {
		return err
	}
	report, err := decodeReviewReport(reportBody)
	if err != nil {
		return err
	}
	if !reviewReportMatches(report, request) {
		return fmt.Errorf("%w: trusted review binding", ErrInvalidRequest)
	}
	return nil
}

func validateRequestIdentity(request Request) error {
	_, approvalIDErr := uuid.Parse(request.ApprovalID)
	_, principalIDErr := uuid.Parse(request.Principal.ID)
	if approvalIDErr != nil || principalIDErr != nil ||
		request.DecisionTimestamp.IsZero() || request.RunID == "" ||
		!taskPattern.MatchString(request.TaskID) || !commitPattern.MatchString(request.CandidateCommit) ||
		!digestPattern.MatchString(request.ApprovedSpecificationDigest) ||
		!digestPattern.MatchString(request.ApprovedTaskDigest) ||
		request.ExpectedTaskRevision == 0 || len(request.ActualChangedPaths) == 0 {
		return ErrInvalidRequest
	}
	return nil
}

func validateRiskInputs(request Request) error {
	for _, changedPath := range request.ActualChangedPaths {
		if !safeRepoPath(changedPath) {
			return fmt.Errorf("%w: changed path", ErrInvalidRequest)
		}
	}
	for _, label := range request.ExclusiveResourceLabels {
		if !labelPattern.MatchString(label) {
			return fmt.Errorf("%w: resource label", ErrInvalidRequest)
		}
	}
	return nil
}

func (s *Service) validateInputArtifacts(ctx context.Context, request Request) error {
	for _, ref := range []workflow.ArtifactRef{
		request.Implementation, request.Execution, request.Verification,
	} {
		if err := verifyArtifact(ctx, s.artifacts, ref); err != nil {
			return err
		}
	}
	return nil
}

func reviewReportMatches(report review.ReviewReport, request Request) bool {
	return report.Passed && report.RunID == request.RunID && report.TaskID == request.TaskID &&
		report.CandidateCommit == request.CandidateCommit &&
		report.ApprovedSpecificationDigest == request.ApprovedSpecificationDigest &&
		report.ApprovedTaskDigest == request.ApprovedTaskDigest &&
		report.ImplementationProposalDigest == request.Implementation.Digest &&
		report.Execution.Equal(request.Execution) &&
		report.Verification.Equal(request.Verification) &&
		reviewInputMatches(report)
}

func reviewInputMatches(r review.ReviewReport) bool {
	return r.SchemaVersion == "1" && r.Request.URI != "" && r.Proposal.URI != ""
}

func authorize(principal Principal, elevated bool, decision string) error {
	hasApprover := slices.Contains(principal.Roles, RoleApprover)
	hasElevated := slices.Contains(principal.Roles, RoleElevatedApprover)
	if !hasApprover && !hasElevated {
		return ErrUnauthorized
	}
	if decision == "approve" && elevated && !hasElevated {
		return ErrElevatedRole
	}
	return nil
}

func normalizedPrincipal(value Principal) Principal {
	roles := slices.Clone(value.Roles)
	sort.Slice(roles, func(i, j int) bool { return roles[i] < roles[j] })
	return Principal{ID: value.ID, Roles: roles}
}

// ClassifyElevatedRisk deterministically returns sorted risk reasons.
func ClassifyElevatedRisk(changedPaths, resourceLabels []string) []string {
	reasons := make(map[string]struct{})
	for _, label := range resourceLabels {
		if _, protected := protectedLabels[label]; protected {
			reasons["resource:"+label] = struct{}{}
		}
	}
	for _, changedPath := range changedPaths {
		if protectedPath(changedPath) {
			reasons["path:"+changedPath] = struct{}{}
		}
	}
	result := make([]string, 0, len(reasons))
	for reason := range reasons {
		result = append(result, reason)
	}
	sort.Strings(result)
	return result
}

func protectedPath(value string) bool {
	base := path.Base(value)
	switch base {
	case "go.mod", "go.sum", "pyproject.toml", "uv.lock":
		return true
	}
	lower := strings.ToLower(value)
	return strings.HasSuffix(lower, ".proto") ||
		(strings.HasSuffix(lower, ".sql") && strings.Contains(lower, "migration")) ||
		strings.Contains(lower, "/schema/") || strings.HasPrefix(lower, "schema/") ||
		strings.HasPrefix(value, ".github/workflows/") ||
		strings.HasPrefix(lower, "deploy/") || strings.Contains(lower, "/deploy/") ||
		strings.HasPrefix(lower, "k8s/") || strings.Contains(lower, "/k8s/") ||
		strings.HasPrefix(base, "Dockerfile")
}

func safeRepoPath(value string) bool {
	return value != "" && !path.IsAbs(value) && path.Clean(value) == value &&
		!strings.HasPrefix(value, "../") && !strings.Contains(value, `\`)
}

func verifyArtifact(
	ctx context.Context, store ArtifactStore, ref workflow.ArtifactRef,
) error {
	_, err := fetchVerifiedArtifact(ctx, store, ref)
	return err
}

func fetchVerifiedArtifact(
	ctx context.Context, store ArtifactStore, ref workflow.ArtifactRef,
) ([]byte, error) {
	if err := ref.Validate(); err != nil {
		return nil, fmt.Errorf("%w: artifact reference", ErrInvalidRequest)
	}
	body, err := store.Get(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("load approval input artifact: %w", err)
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != ref.Digest {
		return nil, ErrArtifactIntegrity
	}
	return body, nil
}

func decodeReviewReport(body []byte) (review.ReviewReport, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var report review.ReviewReport
	if err := decoder.Decode(&report); err != nil {
		return review.ReviewReport{}, fmt.Errorf("%w: decode review report", ErrArtifactIntegrity)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return review.ReviewReport{}, fmt.Errorf("%w: review report trailer", ErrArtifactIntegrity)
	}
	return report, nil
}

func (s *Service) storeApproval(
	ctx context.Context, body []byte,
) (workflow.ArtifactRef, error) {
	artifact, err := s.artifacts.Put(ctx, body)
	if err != nil {
		return workflow.ArtifactRef{}, fmt.Errorf("store task approval: %w", err)
	}
	sum := sha256.Sum256(body)
	if err := artifact.Validate(); err != nil ||
		artifact.Digest != hex.EncodeToString(sum[:]) {
		return workflow.ArtifactRef{}, ErrArtifactIntegrity
	}
	return artifact, nil
}

func approvalCommand(
	request Request, decision, reason string, approval workflow.ArtifactRef,
) workflow.TaskCommand {
	id := func(label string) string {
		return uuid.NewSHA1(
			uuid.NameSpaceURL, []byte("harness:approval:"+request.ApprovalID+":"+label),
		).String()
	}
	meta := workflow.CommandEnvelope{
		CommandID: id("apply"), Actor: workflow.Actor{
			ID: request.Principal.ID, Kind: workflow.ActorHuman,
		},
		ExpectedRevision: request.ExpectedTaskRevision,
		Timestamp:        request.DecisionTimestamp.UTC(),
		CorrelationID:    id("correlation"), CausationID: id("apply:cause"),
	}
	if decision == "approve" {
		return workflow.ApproveTask{
			Meta: meta, Run: request.RunID, ID: request.TaskID,
			CandidateCommit: request.CandidateCommit, Review: request.Review,
			Approval: approval,
		}
	}
	return workflow.RequireTaskRework{
		Meta: meta, Run: request.RunID, ID: request.TaskID,
		CandidateCommit: request.CandidateCommit, Review: request.Review, Reason: reason,
	}
}
