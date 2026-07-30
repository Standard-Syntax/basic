package publication

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	reasoningv1 "github.com/Standard-Syntax/basic/go/gen/harness/reasoning/v1"
	"github.com/Standard-Syntax/basic/go/internal/approval"
	"github.com/Standard-Syntax/basic/go/internal/execution"
	"github.com/Standard-Syntax/basic/go/internal/review"
	"github.com/Standard-Syntax/basic/go/internal/verification"
	"github.com/Standard-Syntax/basic/go/internal/workflow"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

type validatedInputs struct {
	specification []byte
	execution     execution.ExecutionReport
	verification  verification.VerificationReport
	review        review.ReviewReport
	approval      approval.TaskApproval
}

func validateRequestHeader(config Config, request Request) error {
	if _, err := uuid.Parse(request.PublicationID); err != nil {
		return fmt.Errorf("%w: publication ID", ErrInvalidRequest)
	}
	if _, err := uuid.Parse(request.RunID); err != nil {
		return fmt.Errorf("%w: run ID", ErrInvalidRequest)
	}
	if request.PublicationTimestamp.IsZero() || request.ExpectedRunRevision == 0 ||
		!commitPattern.MatchString(request.BaseCommit) ||
		!commitPattern.MatchString(request.CandidateCommit) ||
		request.BaseCommit == request.CandidateCommit {
		return fmt.Errorf("%w: publication identity", ErrInvalidRequest)
	}
	if !branchPattern.MatchString(config.BranchPrefix + request.RunID) {
		return fmt.Errorf("%w: publication branch", ErrInvalidRequest)
	}
	for _, ref := range []workflow.ArtifactRef{
		request.Specification, request.Implementation, request.Execution,
		request.Verification, request.Review, request.Approval,
	} {
		if err := ref.Validate(); err != nil {
			return fmt.Errorf("%w: artifact reference", ErrInvalidRequest)
		}
	}
	return nil
}

func validateArtifacts(
	ctx context.Context, store ArtifactStore, config Config, request Request,
) (validatedInputs, error) {
	specification, err := loadArtifact(ctx, store, request.Specification, config.MaxArtifactBytes)
	if err != nil {
		return validatedInputs{}, err
	}
	if !validSpecificationArtifact(specification) {
		return validatedInputs{}, fmt.Errorf("%w: approved specification", ErrArtifactIntegrity)
	}
	implementation, err := loadArtifact(
		ctx, store, request.Implementation, config.MaxArtifactBytes,
	)
	if err != nil || len(implementation) == 0 {
		return validatedInputs{}, fmt.Errorf("%w: implementation proposal", ErrArtifactIntegrity)
	}
	executionReport, err := loadJSON[execution.ExecutionReport](
		ctx, store, request.Execution, config.MaxArtifactBytes,
	)
	if err != nil {
		return validatedInputs{}, err
	}
	verificationReport, err := loadJSON[verification.VerificationReport](
		ctx, store, request.Verification, config.MaxArtifactBytes,
	)
	if err != nil {
		return validatedInputs{}, err
	}
	reviewReport, err := loadJSON[review.ReviewReport](
		ctx, store, request.Review, config.MaxArtifactBytes,
	)
	if err != nil {
		return validatedInputs{}, err
	}
	approvalReport, err := loadJSON[approval.TaskApproval](
		ctx, store, request.Approval, config.MaxArtifactBytes,
	)
	if err != nil {
		return validatedInputs{}, err
	}
	inputs := validatedInputs{
		specification: specification, execution: executionReport,
		verification: verificationReport, review: reviewReport, approval: approvalReport,
	}
	if !artifactsMatch(request, inputs) {
		return validatedInputs{}, fmt.Errorf("%w: upstream bindings", ErrInvalidRequest)
	}
	return inputs, nil
}

func validSpecificationArtifact(body []byte) bool {
	var proposal reasoningv1.SpecificationProposal
	if err := proto.Unmarshal(body, &proposal); err == nil &&
		strings.TrimSpace(proposal.GetTitle()) != "" {
		return true
	}
	var legacy map[string]json.RawMessage
	return decodeJSON(body, &legacy) == nil && len(legacy) > 0
}

func artifactsMatch(request Request, inputs validatedInputs) bool {
	if inputs.execution.SchemaVersion != "1" || inputs.execution.RunID != request.RunID ||
		inputs.execution.BaseCommit != request.BaseCommit ||
		inputs.execution.CandidateCommit != request.CandidateCommit ||
		!inputs.execution.Proposal.Equal(request.Implementation) {
		return false
	}
	if inputs.verification.SchemaVersion != "1" ||
		inputs.verification.RunID != request.RunID ||
		inputs.verification.BaseCommit != request.BaseCommit ||
		inputs.verification.CandidateCommit != request.CandidateCommit ||
		!inputs.verification.Execution.Equal(request.Execution) ||
		!inputs.verification.Passed || !verificationPassed(inputs.verification) {
		return false
	}
	if inputs.review.SchemaVersion != "1" || inputs.review.RunID != request.RunID ||
		inputs.review.CandidateCommit != request.CandidateCommit ||
		inputs.review.ApprovedSpecificationDigest != request.Specification.Digest ||
		inputs.review.ImplementationProposalDigest != request.Implementation.Digest ||
		!inputs.review.Execution.Equal(request.Execution) ||
		!inputs.review.Verification.Equal(request.Verification) ||
		!inputs.review.Passed || inputs.review.Recommendation !=
		"REVIEW_RECOMMENDATION_ADVISORY_ACCEPT" {
		return false
	}
	return inputs.approval.SchemaVersion == "1" &&
		inputs.approval.Decision == "approve" &&
		inputs.approval.RunID == request.RunID &&
		inputs.approval.CandidateCommit == request.CandidateCommit &&
		inputs.approval.ApprovedSpecificationDigest == request.Specification.Digest &&
		inputs.approval.Implementation.Equal(request.Implementation) &&
		inputs.approval.Execution.Equal(request.Execution) &&
		inputs.approval.Verification.Equal(request.Verification) &&
		inputs.approval.Review.Equal(request.Review)
}

func verificationPassed(report verification.VerificationReport) bool {
	if len(report.Checks) == 0 || len(report.Coverage) == 0 {
		return false
	}
	for _, check := range report.Checks {
		if !check.Passed || check.TimedOut || check.CandidateCommit != report.CandidateCommit {
			return false
		}
	}
	for _, coverage := range report.Coverage {
		if !coverage.Covered || !coverage.Passed || len(coverage.CheckIDs) == 0 {
			return false
		}
	}
	return true
}

func loadJSON[T any](
	ctx context.Context, store ArtifactStore, ref workflow.ArtifactRef, limit int64,
) (T, error) {
	var value T
	body, err := loadArtifact(ctx, store, ref, limit)
	if err != nil {
		return value, err
	}
	if err := decodeJSON(body, &value); err != nil {
		return value, fmt.Errorf("%w: decode upstream artifact", ErrArtifactIntegrity)
	}
	return value, nil
}

func loadArtifact(
	ctx context.Context, store ArtifactStore, ref workflow.ArtifactRef, limit int64,
) ([]byte, error) {
	body, err := store.Get(ctx, ref, limit)
	if err != nil {
		return nil, fmt.Errorf("load publication input: %w", err)
	}
	if int64(len(body)) > limit {
		return nil, ErrResponseLimit
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != ref.Digest {
		return nil, ErrArtifactIntegrity
	}
	return body, nil
}

func decodeJSON(body []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("unexpected JSON trailer")
	}
	return nil
}

func normalizeConfig(config Config) (Config, error) {
	if config.BranchPrefix == "" {
		config.BranchPrefix = DefaultBranchPrefix
	}
	if config.MaxArtifactBytes == 0 {
		config.MaxArtifactBytes = DefaultMaxArtifactBytes
	}
	if config.MaxBodyBytes == 0 {
		config.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if config.Timeout == 0 {
		config.Timeout = DefaultTimeout
	}
	for _, value := range []string{
		config.RepositoryRoot, config.RepositoryOwner, config.RepositoryName,
		config.Remote, config.BaseBranch, config.BranchPrefix, config.ActorID,
	} {
		if strings.TrimSpace(value) == "" {
			return Config{}, fmt.Errorf("%w: publication configuration", ErrInvalidRequest)
		}
	}
	if !branchPattern.MatchString(config.BaseBranch) ||
		!branchPattern.MatchString(config.BranchPrefix+"example") ||
		config.MaxArtifactBytes <= 0 || config.MaxBodyBytes <= 0 || config.Timeout <= 0 {
		return Config{}, fmt.Errorf("%w: publication limits", ErrInvalidRequest)
	}
	return config, nil
}
