package publication

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Standard-Syntax/basic/go/internal/workflow"
	"github.com/google/uuid"
)

type Service struct {
	config    Config
	artifacts ArtifactStore
	workflow  WorkflowStore
	git       GitPublisher
	pulls     PullRequestClient
	ledger    PublicationLedger
}

func NewService(
	config Config,
	artifacts ArtifactStore,
	workflowStore WorkflowStore,
	gitPublisher GitPublisher,
	pulls PullRequestClient,
	ledger PublicationLedger,
) (*Service, error) {
	normalized, err := normalizeConfig(config)
	if err != nil {
		return nil, err
	}
	if artifacts == nil || workflowStore == nil || gitPublisher == nil ||
		pulls == nil || ledger == nil {
		return nil, errors.New("publication service ports are required")
	}
	if _, err := uuid.Parse(normalized.ActorID); err != nil {
		return nil, fmt.Errorf("%w: publication actor ID", ErrInvalidRequest)
	}
	return &Service{
		config: normalized, artifacts: artifacts, workflow: workflowStore,
		git: gitPublisher, pulls: pulls, ledger: ledger,
	}, nil
}

func (s *Service) Publish(ctx context.Context, request Request) (Result, error) {
	if err := validateRequestHeader(s.config, request); err != nil {
		return Result{}, err
	}
	inputs, err := validateArtifacts(ctx, s.artifacts, s.config, request)
	if err != nil {
		return Result{}, err
	}
	rendered, err := renderPullRequest(s.config, request, inputs)
	if err != nil {
		return Result{}, err
	}
	digest, err := publicationRequestDigest(request)
	if err != nil {
		return Result{}, err
	}
	handle, err := s.ledger.Begin(ctx, PublicationStart{
		PublicationID: request.PublicationID, RequestDigest: digest,
		RequestedAt: request.PublicationTimestamp,
		Repository:  s.config.RepositoryOwner + "/" + s.config.RepositoryName,
		BaseBranch:  s.config.BaseBranch, HeadBranch: rendered.Head,
		BaseCommit: request.BaseCommit, CandidateCommit: request.CandidateCommit,
		SpecificationDigest:  request.Specification.Digest,
		ImplementationDigest: request.Implementation.Digest,
		ExecutionDigest:      request.Execution.Digest,
		VerificationDigest:   request.Verification.Digest,
		ReviewDigest:         request.Review.Digest, ApprovalDigest: request.Approval.Digest,
		ExpectedRunRevision: request.ExpectedRunRevision,
	})
	if err != nil {
		return Result{}, fmt.Errorf("reserve publication: %w", err)
	}
	if replay, ok := handle.Replay(); ok {
		replay.Replay = true
		return replay, nil
	}
	return s.publishReserved(ctx, request, rendered, handle)
}

func (s *Service) publishReserved(
	ctx context.Context,
	request Request,
	rendered DraftPullRequestInput,
	handle PublicationHandle,
) (Result, error) {
	branch, ready := handle.Branch()
	if !ready {
		head, err := s.git.BaseHead(ctx)
		if err != nil {
			_ = handle.Rollback(ctx)
			return Result{}, fmt.Errorf("inspect publication base: %w", err)
		}
		if head != request.BaseCommit {
			_ = handle.Rollback(ctx)
			return Result{}, ErrBaseDrift
		}
		if _, err := s.git.Publish(ctx, rendered.Head, request.CandidateCommit); err != nil {
			_ = handle.Rollback(ctx)
			return Result{}, err
		}
		branch = BranchCheckpoint{Branch: rendered.Head, CandidateCommit: request.CandidateCommit}
		if err := handle.SaveBranch(ctx, branch); err != nil {
			return Result{}, fmt.Errorf("checkpoint publication branch: %w", err)
		}
	}
	if branch.Branch != rendered.Head || branch.CandidateCommit != request.CandidateCommit {
		return Result{}, ErrPublicationConflict
	}
	checkpoint, ready := handle.PullRequest()
	if !ready {
		head, err := s.git.BaseHead(ctx)
		if err != nil {
			return Result{}, fmt.Errorf("recheck publication base: %w", err)
		}
		if head != request.BaseCommit {
			return Result{}, ErrBaseDrift
		}
		pr, exists, err := s.pulls.FindDraft(ctx, rendered)
		if err != nil {
			return Result{}, fmt.Errorf("find draft pull request: %w", err)
		}
		if !exists {
			pr, err = s.pulls.CreateDraft(ctx, rendered)
			if err != nil {
				recovered, found, findErr := s.pulls.FindDraft(ctx, rendered)
				if findErr != nil || !found {
					return Result{}, fmt.Errorf("create draft pull request: %w", err)
				}
				pr = recovered
			}
		}
		checkpoint = PullRequestCheckpoint{
			Branch: rendered.Head, CandidateCommit: request.CandidateCommit,
			PullRequestNumber: pr.Number, PullRequestURL: pr.URL,
		}
		if err := handle.SavePullRequest(ctx, checkpoint); err != nil {
			return Result{}, fmt.Errorf("checkpoint draft pull request: %w", err)
		}
	}
	return s.complete(ctx, request, checkpoint, handle)
}

func (s *Service) complete(
	ctx context.Context,
	request Request,
	checkpoint PullRequestCheckpoint,
	handle PublicationHandle,
) (Result, error) {
	artifact := DraftPullRequestArtifact{
		SchemaVersion: "1", PublicationID: request.PublicationID,
		PublishedAt:     request.PublicationTimestamp.UTC().Format(time.RFC3339Nano),
		RepositoryOwner: s.config.RepositoryOwner, RepositoryName: s.config.RepositoryName,
		BaseBranch: s.config.BaseBranch, HeadBranch: checkpoint.Branch,
		BaseCommit: request.BaseCommit, CandidateCommit: checkpoint.CandidateCommit,
		PullRequestNumber: checkpoint.PullRequestNumber, PullRequestURL: checkpoint.PullRequestURL,
		Draft: true, Specification: request.Specification, Implementation: request.Implementation,
		Execution: request.Execution, Verification: request.Verification,
		Review: request.Review, Approval: request.Approval,
	}
	body, err := json.Marshal(artifact)
	if err != nil {
		return Result{}, fmt.Errorf("encode publication artifact: %w", err)
	}
	ref, err := s.artifacts.Put(ctx, body)
	if err != nil {
		return Result{}, fmt.Errorf("store publication artifact: %w", err)
	}
	sum := sha256.Sum256(body)
	if err := ref.Validate(); err != nil || ref.Digest != hex.EncodeToString(sum[:]) {
		return Result{}, ErrArtifactIntegrity
	}
	recorded, err := s.workflow.ExecuteRun(ctx, workflow.RecordDraftPullRequest{
		Meta: workflow.CommandEnvelope{
			CommandID:        request.PublicationID,
			Actor:            workflow.Actor{ID: s.config.ActorID, Kind: workflow.ActorPublicationService},
			ExpectedRevision: request.ExpectedRunRevision,
			Timestamp:        request.PublicationTimestamp.UTC(),
			CorrelationID:    request.RunID, CausationID: request.PublicationID,
		},
		ID: request.RunID, CandidateCommit: request.CandidateCommit,
		Approval: request.Approval, Publication: ref,
	})
	if err != nil {
		return Result{}, fmt.Errorf("record draft pull request: %w", err)
	}
	result := Result{
		PublicationID: request.PublicationID, Branch: checkpoint.Branch,
		CandidateCommit:   request.CandidateCommit,
		PullRequestNumber: checkpoint.PullRequestNumber, PullRequestURL: checkpoint.PullRequestURL,
		PublicationArtifact: ref, Replay: recorded.Replay,
	}
	if err := handle.Complete(ctx, result); err != nil {
		return Result{}, fmt.Errorf("complete publication: %w", err)
	}
	return result, nil
}

func publicationRequestDigest(request Request) (string, error) {
	encoded, err := json.Marshal(request)
	if err != nil {
		return "", fmt.Errorf("encode publication request: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}
