package execution

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/Standard-Syntax/basic/go/internal/reasoning/contracts"
	"github.com/Standard-Syntax/basic/go/internal/workflow"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

type Service struct {
	config     Config
	artifacts  ArtifactStore
	applicator Applicator
	workflow   WorkflowStore
	now        func() time.Time
}

func NewService(
	config Config, artifacts ArtifactStore, applicator Applicator, workflowStore WorkflowStore,
) (*Service, error) {
	if artifacts == nil || applicator == nil || workflowStore == nil {
		return nil, errors.New("artifact store, applicator, and workflow store are required")
	}
	if config.RepositoryRoot == "" || config.WorktreeRoot == "" || config.WorkerImage == "" {
		return nil, errors.New("repository, worktree root, and worker image are required")
	}
	config.RepositoryRoot = filepath.Clean(config.RepositoryRoot)
	config.WorktreeRoot = filepath.Clean(config.WorktreeRoot)
	if !filepath.IsAbs(config.RepositoryRoot) || !filepath.IsAbs(config.WorktreeRoot) {
		return nil, errors.New("repository and worktree roots must be absolute")
	}
	if config.Limits == (Limits{}) {
		config.Limits = DefaultLimits()
	}
	if err := validateLimits(config.Limits); err != nil {
		return nil, err
	}
	if _, err := uuid.Parse(config.ActorID); err != nil ||
		strings.TrimSpace(config.AuthorName) == "" || strings.TrimSpace(config.AuthorEmail) == "" {
		return nil, errors.New("execution actor and author metadata are required")
	}
	return &Service{
		config: config, artifacts: artifacts, applicator: applicator,
		workflow: workflowStore, now: time.Now,
	}, nil
}

func (s *Service) Execute(ctx context.Context, request Request) (Result, error) {
	mappedRequest, mappedProposal, err := s.validateRequest(ctx, request)
	if err != nil {
		return Result{}, err
	}
	worktree, err := createWorktree(
		ctx, s.config, request.ExecutionID, mappedRequest.BaseCommit,
	)
	if err != nil {
		return Result{}, err
	}
	defer func() {
		_ = removeWorktree(context.Background(), s.config.RepositoryRoot, worktree)
	}()
	if err := validateTargets(worktree, mappedProposal.Changes); err != nil {
		return Result{}, err
	}
	if err := validateTargetDigests(worktree, mappedProposal.Changes); err != nil {
		return Result{}, err
	}
	accepted, err := s.workflow.ExecuteTask(ctx, workflow.AcceptTaskProposal{
		Meta: s.commandEnvelope(
			request.ExecutionID, "accept", request.ExpectedTaskRevision, s.now().UTC(),
		),
		Run: mappedRequest.Envelope.RunID, ID: mappedRequest.ApprovedTaskID,
		Proposal: request.ProposalArtifact, Lease: request.Lease,
	})
	if err != nil {
		return Result{}, fmt.Errorf("accept task proposal: %w", err)
	}
	applyContext, cancel := context.WithTimeout(ctx, s.config.Limits.Timeout)
	defer cancel()
	if err := s.applicator.Apply(
		applyContext, worktree, mappedProposal.Changes, s.config.Limits,
	); err != nil {
		return Result{}, fmt.Errorf("apply implementation proposal: %w", err)
	}
	candidate, candidateRef, actualDiff, err := buildCandidate(
		ctx, s.config, worktree, request, mappedRequest, mappedProposal,
	)
	if err != nil {
		return Result{}, err
	}
	keepRef := false
	defer func() {
		if !keepRef {
			_ = deleteCandidateRef(
				context.Background(), s.config.RepositoryRoot, candidateRef, candidate,
			)
		}
	}()
	report := ExecutionReport{
		SchemaVersion: "1", ExecutionID: request.ExecutionID,
		ExecutedAt: request.ExecutionTimestamp.UTC().Format(time.RFC3339Nano),
		RunID:      mappedRequest.Envelope.RunID, TaskID: mappedRequest.ApprovedTaskID,
		Attempt: mappedRequest.Envelope.Attempt, Proposal: request.ProposalArtifact,
		Lease: request.Lease, BaseCommit: mappedRequest.BaseCommit,
		CandidateCommit: candidate, CandidateRef: candidateRef,
		Limits: s.config.Limits, ActualDiff: actualDiff,
	}
	reportBytes, err := marshalReport(report)
	if err != nil {
		return Result{}, err
	}
	reportArtifact, err := s.artifacts.Put(ctx, reportBytes)
	if err != nil {
		return Result{}, fmt.Errorf("store execution report: %w", err)
	}
	recordedAt := s.now().UTC()
	_, err = s.workflow.ExecuteTask(ctx, workflow.RecordTaskExecution{
		Meta: s.commandEnvelope(
			request.ExecutionID, "record", accepted.Revision, recordedAt,
		),
		Run: mappedRequest.Envelope.RunID, ID: mappedRequest.ApprovedTaskID,
		Proposal: request.ProposalArtifact, Execution: reportArtifact,
		CandidateCommit: candidate, Lease: request.Lease,
	})
	if err != nil {
		return Result{}, fmt.Errorf("record task execution: %w", err)
	}
	keepRef = true
	return Result{
		ExecutionID: request.ExecutionID, BaseCommit: mappedRequest.BaseCommit,
		CandidateCommit: candidate, CandidateRef: candidateRef,
		ReportArtifact: reportArtifact, Lease: request.Lease,
		Limits: s.config.Limits, ActualDiff: actualDiff,
	}, nil
}

func (s *Service) validateRequest(
	ctx context.Context, request Request,
) (contracts.ImplementationRequest, contracts.ImplementationProposal, error) {
	if err := validateExecutionIdentity(request); err != nil {
		return contracts.ImplementationRequest{}, contracts.ImplementationProposal{}, err
	}
	mappedRequest, err := contracts.MapImplementationRequestAt(request.Implementation, s.now().UTC())
	if err != nil {
		return contracts.ImplementationRequest{}, contracts.ImplementationProposal{},
			fmt.Errorf("%w: implementation request: %v", ErrInvalidRequest, err)
	}
	if mappedRequest.ApprovedTaskID != request.Implementation.GetEnvelope().GetTaskId() ||
		mappedRequest.Envelope.Attempt != request.Lease.FencingToken {
		return contracts.ImplementationRequest{}, contracts.ImplementationProposal{},
			fmt.Errorf("%w: task lease binding", ErrInvalidRequest)
	}
	if err := s.verifyProposalArtifact(ctx, request); err != nil {
		return contracts.ImplementationRequest{}, contracts.ImplementationProposal{}, err
	}
	mappedProposal, err := contracts.MapImplementationProposal(request.Proposal, mappedRequest)
	if err != nil {
		return contracts.ImplementationRequest{}, contracts.ImplementationProposal{},
			fmt.Errorf("%w: implementation proposal: %v", ErrInvalidRequest, err)
	}
	if err := preflightChanges(mappedProposal.Changes, s.config.Limits); err != nil {
		return contracts.ImplementationRequest{}, contracts.ImplementationProposal{}, err
	}
	return mappedRequest, mappedProposal, nil
}

func (s *Service) verifyProposalArtifact(ctx context.Context, request Request) error {
	if err := request.ProposalArtifact.Validate(); err != nil {
		return fmt.Errorf("%w: proposal reference: %v", ErrInvalidRequest, err)
	}
	body, err := s.artifacts.Get(ctx, request.ProposalArtifact)
	if err != nil {
		return fmt.Errorf("load proposal artifact: %w", err)
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != request.ProposalArtifact.Digest {
		return ErrArtifactIntegrity
	}
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(request.Proposal)
	if err != nil {
		return fmt.Errorf("%w: serialize proposal: %v", ErrInvalidRequest, err)
	}
	if !bytes.Equal(body, encoded) {
		return ErrArtifactIntegrity
	}
	return nil
}

func validateExecutionIdentity(request Request) error {
	if _, err := uuid.Parse(request.ExecutionID); err != nil ||
		request.Implementation == nil || request.Proposal == nil ||
		request.ExpectedTaskRevision == 0 || request.ExecutionTimestamp.IsZero() {
		return ErrInvalidRequest
	}
	if err := request.Lease.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	return nil
}

func (s *Service) commandEnvelope(
	executionID, purpose string, revision uint64, timestamp time.Time,
) workflow.CommandEnvelope {
	id := func(label string) string {
		return uuid.NewSHA1(
			uuid.NameSpaceURL, []byte("harness:execution:"+executionID+":"+label),
		).String()
	}
	return workflow.CommandEnvelope{
		CommandID: id(purpose), Actor: workflow.Actor{
			ID: s.config.ActorID, Kind: workflow.ActorExecutionService,
		},
		ExpectedRevision: revision, Timestamp: timestamp,
		CorrelationID: id("correlation"), CausationID: id(purpose + ":cause"),
	}
}

func validateLimits(limits Limits) error {
	if limits.MaxChangedFiles < 1 || limits.MaxFileBytes < 1 ||
		limits.MaxTotalBytes < 1 || limits.Timeout <= 0 {
		return errors.New("positive execution limits are required")
	}
	if limits.MaxChangedFiles > DefaultMaxChangedFiles ||
		limits.MaxFileBytes > DefaultMaxFileBytes ||
		limits.MaxTotalBytes > DefaultMaxTotalBytes ||
		limits.Timeout > DefaultTimeout {
		return errors.New("execution limits exceed service maximums")
	}
	return nil
}

func preflightChanges(changes []contracts.FileChange, limits Limits) error {
	if len(changes) > limits.MaxChangedFiles {
		return fmt.Errorf("%w: changed files", ErrLimitExceeded)
	}
	seen := make(map[string]struct{}, len(changes))
	var total int64
	for _, change := range changes {
		normalized, err := normalizePath(change.Path)
		if err != nil || normalized != change.Path {
			return fmt.Errorf("%w: %q", ErrUnsafePath, change.Path)
		}
		if _, exists := seen[normalized]; exists {
			return fmt.Errorf("%w: duplicate path %q", ErrUnsafePath, change.Path)
		}
		seen[normalized] = struct{}{}
		if change.ReplacementContent == nil {
			continue
		}
		size := int64(len(*change.ReplacementContent))
		if size > limits.MaxFileBytes {
			return fmt.Errorf("%w: file %q", ErrLimitExceeded, change.Path)
		}
		if strings.IndexByte(*change.ReplacementContent, 0) >= 0 {
			return fmt.Errorf("%w: binary content %q", ErrInvalidRequest, change.Path)
		}
		total += size
		if total > limits.MaxTotalBytes {
			return fmt.Errorf("%w: replacement content", ErrLimitExceeded)
		}
	}
	return nil
}

func normalizePath(value string) (string, error) {
	if value == "" || filepath.IsAbs(value) || strings.Contains(value, `\`) {
		return "", ErrUnsafePath
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return "", ErrUnsafePath
		}
	}
	cleaned := filepath.ToSlash(filepath.Clean(value))
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", ErrUnsafePath
	}
	for _, component := range strings.Split(cleaned, "/") {
		if component == ".git" {
			return "", ErrUnsafePath
		}
	}
	return cleaned, nil
}

func validateTargets(worktree string, changes []contracts.FileChange) error {
	for _, change := range changes {
		parts := strings.Split(change.Path, "/")
		current := worktree
		for index, part := range parts {
			current = filepath.Join(current, part)
			info, err := os.Lstat(current)
			if errors.Is(err, os.ErrNotExist) {
				if change.Operation == contracts.FileCreate && index == len(parts)-1 {
					break
				}
				return fmt.Errorf("%w: missing target %q", ErrUnsafePath, change.Path)
			}
			if err != nil {
				return fmt.Errorf("inspect target %q: %w", change.Path, err)
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("%w: symlink target %q", ErrUnsafePath, change.Path)
			}
			if index < len(parts)-1 && !info.IsDir() {
				return fmt.Errorf("%w: non-directory ancestor %q", ErrUnsafePath, change.Path)
			}
			if index == len(parts)-1 && (!info.Mode().IsRegular() ||
				(info.Mode().Perm() != 0o644 && info.Mode().Perm() != 0o755)) {
				return fmt.Errorf("%w: unsupported target %q", ErrUnsafePath, change.Path)
			}
		}
	}
	return nil
}

func validateTargetDigests(worktree string, changes []contracts.FileChange) error {
	for _, change := range changes {
		if change.Operation == contracts.FileCreate {
			continue
		}
		body, err := os.ReadFile(filepath.Join(worktree, filepath.FromSlash(change.Path)))
		if err != nil {
			return fmt.Errorf("read target %q: %w", change.Path, err)
		}
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != change.ExpectedOriginalSHA256 {
			return fmt.Errorf("%w: original digest %q", ErrArtifactIntegrity, change.Path)
		}
	}
	return nil
}
