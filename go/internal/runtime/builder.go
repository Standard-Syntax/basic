package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	reasoningv1 "github.com/Standard-Syntax/basic/go/gen/harness/reasoning/v1"
	"github.com/Standard-Syntax/basic/go/internal/beta"
	"github.com/Standard-Syntax/basic/go/internal/execution"
	"github.com/Standard-Syntax/basic/go/internal/reasoning/contracts"
	"github.com/Standard-Syntax/basic/go/internal/verification"
	"github.com/Standard-Syntax/basic/go/internal/workflow"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var (
	ErrScopeLimit = errors.New("repository context exceeds approved limit")
	ErrScope      = errors.New("repository path is outside approved scope")
)

type ArtifactWriter interface {
	Put(context.Context, []byte) (workflow.ArtifactRef, error)
}

type RepositorySnapshot struct {
	BaseCommit string                         `json:"base_commit"`
	Entries    []*reasoningv1.RepositoryEntry `json:"entries"`
}

// DecodeRepositorySnapshot accepts only the deterministic intake snapshot
// shape and verifies that it is bound to the requested base commit.
func DecodeRepositorySnapshot(body []byte, baseCommit string) (RepositorySnapshot, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var snapshot RepositorySnapshot
	if err := decoder.Decode(&snapshot); err != nil {
		return RepositorySnapshot{}, fmt.Errorf("%w: decode repository map", ErrScope)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return RepositorySnapshot{}, fmt.Errorf("%w: repository map trailing content", ErrScope)
	}
	if err := snapshot.Validate(baseCommit); err != nil {
		return RepositorySnapshot{}, err
	}
	return snapshot, nil
}

func (s RepositorySnapshot) Validate(baseCommit string) error {
	if s.BaseCommit != baseCommit || len(s.BaseCommit) != 40 || len(s.Entries) == 0 {
		return ErrScope
	}
	for _, value := range s.BaseCommit {
		if !strings.ContainsRune("0123456789abcdef", value) {
			return ErrScope
		}
	}
	previous := ""
	for _, entry := range s.Entries {
		if entry == nil || entry.GetKind() != "blob" || !safeRepoPath(entry.GetPath()) ||
			entry.GetPath() <= previous || len(entry.GetSha256()) != 64 {
			return ErrScope
		}
		for _, value := range entry.GetSha256() {
			if !strings.ContainsRune("0123456789abcdef", value) {
				return ErrScope
			}
		}
		previous = entry.GetPath()
	}
	return nil
}

type ContextLimits struct {
	MaxFiles int
	MaxBytes int64
}

type ReasoningLimits struct {
	MaximumInputTokens      uint64
	MaximumOutputTokens     uint64
	MaximumProviderRequests uint32
}

type ImplementationInput struct {
	RequestID, RunID, TaskID, ManifestDigest string
	Attempt                                  uint32
	CreatedAt, ExpiresAt                     time.Time
	BaseCommit, ApprovedSpecificationID      string
	ApprovedSpecification, ApprovedTask      workflow.ArtifactRef
	Task                                     *reasoningv1.PlannedTask
	Snapshot                                 RepositorySnapshot
	RepositoryRoot                           string
	ContextLimits                            ContextLimits
	Budget                                   ReasoningLimits
}

func BuildImplementationRequest(
	ctx context.Context, store ArtifactWriter, input ImplementationInput,
) (*reasoningv1.ImplementationRequest, error) {
	if input.Task == nil || input.Task.GetTaskId() != input.TaskID ||
		input.BaseCommit != input.Snapshot.BaseCommit {
		return nil, ErrScope
	}
	contextFiles, err := BuildImplementationContext(
		ctx, input.RepositoryRoot, input.BaseCommit, input.Snapshot,
		input.Task.GetReadablePaths(), input.Task.GetProhibitedPaths(), input.ContextLimits,
	)
	if err != nil {
		return nil, err
	}
	inputArtifacts := []*reasoningv1.ArtifactDigest{
		artifactDigest(input.ApprovedSpecification), artifactDigest(input.ApprovedTask),
	}
	for _, file := range contextFiles {
		ref, err := store.Put(ctx, []byte(file.GetContent()))
		if err != nil {
			return nil, err
		}
		if ref.Digest != file.GetSha256() {
			return nil, ErrScope
		}
		inputArtifacts = append(inputArtifacts, artifactDigest(ref))
	}
	return &reasoningv1.ImplementationRequest{
		Envelope: requestEnvelope(
			input.RequestID, input.RunID, input.TaskID, input.Attempt,
			reasoningv1.ReasoningStage_REASONING_STAGE_IMPLEMENTATION,
			input.CreatedAt, input.ExpiresAt, input.ManifestDigest, input.Budget, inputArtifacts,
		),
		ApprovedTaskId: input.TaskID, ApprovedTaskDigest: input.ApprovedTask.Digest,
		ApprovedSpecificationId:     input.ApprovedSpecificationID,
		ApprovedSpecificationDigest: input.ApprovedSpecification.Digest,
		BaseCommit:                  input.BaseCommit,
		ReadablePaths:               input.Task.GetReadablePaths(), WritablePaths: input.Task.GetWritablePaths(),
		ProhibitedPaths:        input.Task.GetProhibitedPaths(),
		AcceptanceCriterionIds: input.Task.GetAcceptanceCriterionIds(),
		AvailableCheckIds:      input.Task.GetRequiredCheckIds(), RepositoryContext: contextFiles,
	}, nil
}

type ReviewInput struct {
	RequestID, RunID, TaskID, ManifestDigest string
	Attempt                                  uint32
	CreatedAt, ExpiresAt                     time.Time
	ApprovedSpecification, ApprovedTask      workflow.ArtifactRef
	ImplementationProposal                   workflow.ArtifactRef
	ExecutionArtifact, VerificationArtifact  workflow.ArtifactRef
	Execution                                execution.ExecutionReport
	Verification                             verification.VerificationReport
	AuthorizedWritablePaths                  []string
	ApprovedAcceptanceCriterionIDs           []string
	Budget                                   ReasoningLimits
}

func BuildReviewRequest(input *ReviewInput) (*reasoningv1.ReviewRequest, error) {
	if input == nil || !input.Verification.Passed ||
		input.Execution.CandidateCommit != input.Verification.CandidateCommit ||
		input.Execution.CandidateCommit == "" {
		return nil, ErrScope
	}
	actualDiff := make([]*reasoningv1.ActualDiffFile, 0, len(input.Execution.ActualDiff))
	authorized, unexpected := make([]string, 0), make([]string, 0)
	for _, entry := range input.Execution.ActualDiff {
		operation, ok := reviewFileOperation(entry.Operation)
		if !ok {
			return nil, ErrScope
		}
		actualDiff = append(actualDiff, &reasoningv1.ActualDiffFile{
			Path: entry.Path, Operation: operation,
			BeforeSha256: entry.BeforeSHA256, AfterSha256: entry.AfterSHA256,
		})
		if withinAny(entry.Path, input.AuthorizedWritablePaths) {
			authorized = append(authorized, entry.Path)
		} else {
			unexpected = append(unexpected, entry.Path)
		}
	}
	evidence := make([]*reasoningv1.IndependentEvidence, 0, len(input.Verification.Checks))
	evidenceIDs := make(map[string]string, len(input.Verification.Checks))
	for index, check := range input.Verification.Checks {
		started, err := time.Parse(time.RFC3339Nano, check.StartedAt)
		if err != nil {
			return nil, err
		}
		completed, err := time.Parse(time.RFC3339Nano, check.FinishedAt)
		if err != nil {
			return nil, err
		}
		id := fmt.Sprintf("EVIDENCE-%03d", index+1)
		evidenceIDs[check.CheckID] = id
		evidence = append(evidence, &reasoningv1.IndependentEvidence{
			EvidenceId: id, CheckId: check.CheckID,
			CandidateCommit: check.CandidateCommit, ExitCode: int32(check.ExitCode),
			OutputSha256: check.OutputDigest, ArtifactUri: check.Output.URI,
			StartedAt: timestamppb.New(started), CompletedAt: timestamppb.New(completed),
		})
	}
	coverage := make([]*reasoningv1.AcceptanceEvidence, 0, len(input.Verification.Coverage))
	for _, criterion := range input.Verification.Coverage {
		ids := make([]string, 0, len(criterion.CheckIDs))
		for _, check := range criterion.CheckIDs {
			id, ok := evidenceIDs[check]
			if !ok {
				return nil, ErrScope
			}
			ids = append(ids, id)
		}
		coverage = append(coverage, &reasoningv1.AcceptanceEvidence{
			AcceptanceCriterionId: criterion.CriterionID, EvidenceIds: ids,
		})
	}
	inputArtifacts := []*reasoningv1.ArtifactDigest{
		artifactDigest(input.ApprovedSpecification), artifactDigest(input.ApprovedTask),
		artifactDigest(input.ImplementationProposal), artifactDigest(input.ExecutionArtifact),
		artifactDigest(input.VerificationArtifact),
	}
	return &reasoningv1.ReviewRequest{
		Envelope: requestEnvelope(
			input.RequestID, input.RunID, input.TaskID, input.Attempt,
			reasoningv1.ReasoningStage_REASONING_STAGE_REVIEW,
			input.CreatedAt, input.ExpiresAt, input.ManifestDigest, input.Budget, inputArtifacts,
		),
		Candidate: &reasoningv1.ReviewCandidateIdentity{
			ApprovedSpecificationDigest:  input.ApprovedSpecification.Digest,
			ApprovedTaskDigest:           input.ApprovedTask.Digest,
			BaseCommit:                   input.Execution.BaseCommit,
			CandidateCommit:              input.Execution.CandidateCommit,
			ImplementationProposalDigest: input.ImplementationProposal.Digest,
		},
		ActualDiff: actualDiff,
		ScopeReport: &reasoningv1.ScopeReport{
			AuthorizedChangedPaths: authorized, UnexpectedChangedPaths: unexpected,
		},
		IndependentEvidence: evidence, AcceptanceCoverage: coverage,
		ReviewPolicy: &reasoningv1.ReviewPolicy{
			BlockingSeverities: []reasoningv1.FindingSeverity{
				reasoningv1.FindingSeverity_FINDING_SEVERITY_HIGH,
				reasoningv1.FindingSeverity_FINDING_SEVERITY_CRITICAL,
			},
			ReportUnrequestedChanges: true,
		},
		ApprovedAcceptanceCriterionIds: input.ApprovedAcceptanceCriterionIDs,
		AuthorizedWritablePaths:        input.AuthorizedWritablePaths,
	}, nil
}

func requestEnvelope(
	requestID, runID, taskID string, attempt uint32, stage reasoningv1.ReasoningStage,
	createdAt, expiresAt time.Time, manifestDigest string, budget ReasoningLimits,
	artifacts []*reasoningv1.ArtifactDigest,
) *reasoningv1.ReasoningRequestEnvelope {
	return &reasoningv1.ReasoningRequestEnvelope{
		SchemaVersion: "1", RequestId: requestID, RunId: runID, TaskId: &taskID,
		Stage: stage, Attempt: attempt, CreatedAt: timestamppb.New(createdAt.UTC()),
		ExpiresAt: timestamppb.New(expiresAt.UTC()),
		Authority: &reasoningv1.AuthorityConstraints{
			Mode: reasoningv1.AuthorityMode_AUTHORITY_MODE_PROPOSAL_ONLY,
		},
		Budget: &reasoningv1.ReasoningBudget{
			MaximumInputTokens:      budget.MaximumInputTokens,
			MaximumOutputTokens:     budget.MaximumOutputTokens,
			MaximumProviderRequests: budget.MaximumProviderRequests,
		},
		InputArtifacts: artifacts, AgentManifestDigest: manifestDigest,
	}
}

func artifactDigest(ref workflow.ArtifactRef) *reasoningv1.ArtifactDigest {
	return &reasoningv1.ArtifactDigest{ArtifactUri: ref.URI, Sha256: ref.Digest}
}

func reviewFileOperation(value contracts.FileOperation) (reasoningv1.FileOperation, bool) {
	switch value {
	case contracts.FileCreate:
		return reasoningv1.FileOperation_FILE_OPERATION_CREATE, true
	case contracts.FileUpdate:
		return reasoningv1.FileOperation_FILE_OPERATION_UPDATE, true
	case contracts.FileDelete:
		return reasoningv1.FileOperation_FILE_OPERATION_DELETE, true
	default:
		return reasoningv1.FileOperation_FILE_OPERATION_UNSPECIFIED, false
	}
}

func BuildApprovedSpecification(
	ctx context.Context,
	store ArtifactWriter,
	request *reasoningv1.SpecificationRequest,
	proposal *reasoningv1.SpecificationProposal,
) (workflow.ArtifactRef, error) {
	mapped, err := contracts.MapSpecificationRequest(request)
	if err != nil {
		return workflow.ArtifactRef{}, err
	}
	if _, err := contracts.MapSpecificationProposal(proposal, mapped); err != nil {
		return workflow.ArtifactRef{}, err
	}
	body, err := proto.MarshalOptions{Deterministic: true}.Marshal(proposal)
	if err != nil {
		return workflow.ArtifactRef{}, err
	}
	return store.Put(ctx, body)
}

func BuildApprovedTask(
	ctx context.Context,
	store ArtifactWriter,
	request *reasoningv1.TaskPlanningRequest,
	proposal *reasoningv1.TaskGraphProposal,
	trustedChecks map[string]struct{},
) (workflow.ArtifactRef, *reasoningv1.PlannedTask, error) {
	mapped, err := contracts.MapTaskPlanningRequest(request)
	if err != nil {
		return workflow.ArtifactRef{}, nil, err
	}
	graph, err := contracts.MapTaskGraphProposal(proposal, mapped)
	if err != nil {
		return workflow.ArtifactRef{}, nil, err
	}
	if len(graph.Tasks) != 1 || len(graph.Tasks[0].Dependencies) != 0 ||
		len(proposal.GetTasks()) != 1 {
		return workflow.ArtifactRef{}, nil, fmt.Errorf("%w: first slice requires one dependency-free task", ErrScope)
	}
	for _, check := range graph.Tasks[0].RequiredCheckIDs {
		if _, ok := trustedChecks[check]; !ok {
			return workflow.ArtifactRef{}, nil, fmt.Errorf("%w: untrusted check %q", ErrScope, check)
		}
	}
	body, err := proto.MarshalOptions{Deterministic: true}.Marshal(proposal.GetTasks()[0])
	if err != nil {
		return workflow.ArtifactRef{}, nil, err
	}
	ref, err := store.Put(ctx, body)
	if err != nil {
		return workflow.ArtifactRef{}, nil, err
	}
	return ref, proto.Clone(proposal.GetTasks()[0]).(*reasoningv1.PlannedTask), nil
}

func BuildApprovedTaskGraph(
	ctx context.Context,
	store ArtifactWriter,
	request *reasoningv1.TaskPlanningRequest,
	proposal *reasoningv1.TaskGraphProposal,
	trustedChecks map[string]struct{},
	policy beta.Policy,
) (workflow.ArtifactRef, workflow.ArtifactRef, *reasoningv1.PlannedTask, error) {
	if err := policy.Validate(); err != nil || !slices.Equal(request.GetReadablePaths(), policy.Paths.Readable) ||
		!slices.Equal(request.GetWritablePaths(), policy.Paths.Writable) ||
		!slices.Equal(request.GetProhibitedPaths(), policy.Paths.Prohibited) ||
		request.GetTaskCountLimit() != uint32(policy.Limits.MaximumTasks) ||
		request.GetParallelismLimit() != uint32(policy.Limits.ExecutionConcurrency) {
		return workflow.ArtifactRef{}, workflow.ArtifactRef{}, nil, fmt.Errorf("%w: task policy mismatch", ErrScope)
	}
	mapped, err := contracts.MapTaskPlanningRequest(request)
	if err != nil {
		return workflow.ArtifactRef{}, workflow.ArtifactRef{}, nil, err
	}
	graph, err := contracts.MapTaskGraphProposal(proposal, mapped)
	if err != nil {
		return workflow.ArtifactRef{}, workflow.ArtifactRef{}, nil, err
	}
	if len(graph.Tasks) != 1 || len(graph.Tasks[0].Dependencies) != 0 ||
		len(proposal.GetTasks()) != 1 {
		return workflow.ArtifactRef{}, workflow.ArtifactRef{}, nil,
			fmt.Errorf("%w: first slice requires one dependency-free task", ErrScope)
	}
	for _, check := range graph.Tasks[0].RequiredCheckIDs {
		if _, ok := trustedChecks[check]; !ok {
			return workflow.ArtifactRef{}, workflow.ArtifactRef{}, nil,
				fmt.Errorf("%w: untrusted check %q", ErrScope, check)
		}
	}
	planned := graph.Tasks[0]
	for _, value := range planned.ReadablePaths {
		if !beta.WithinAny(value, policy.Paths.Readable) {
			return workflow.ArtifactRef{}, workflow.ArtifactRef{}, nil, ErrScope
		}
	}
	for _, value := range planned.WritablePaths {
		if !beta.WithinAny(value, policy.Paths.Writable) {
			return workflow.ArtifactRef{}, workflow.ArtifactRef{}, nil, ErrScope
		}
	}
	for _, value := range policy.Paths.Prohibited {
		if !beta.WithinAny(value, planned.ProhibitedPaths) {
			return workflow.ArtifactRef{}, workflow.ArtifactRef{}, nil, ErrScope
		}
	}
	graphBody, err := proto.MarshalOptions{Deterministic: true}.Marshal(proposal)
	if err != nil {
		return workflow.ArtifactRef{}, workflow.ArtifactRef{}, nil, err
	}
	graphRef, err := store.Put(ctx, graphBody)
	if err != nil {
		return workflow.ArtifactRef{}, workflow.ArtifactRef{}, nil, err
	}
	taskBody, err := proto.MarshalOptions{Deterministic: true}.Marshal(proposal.GetTasks()[0])
	if err != nil {
		return workflow.ArtifactRef{}, workflow.ArtifactRef{}, nil, err
	}
	taskRef, err := store.Put(ctx, taskBody)
	if err != nil {
		return workflow.ArtifactRef{}, workflow.ArtifactRef{}, nil, err
	}
	return graphRef, taskRef,
		proto.Clone(proposal.GetTasks()[0]).(*reasoningv1.PlannedTask), nil
}

func SnapshotRepository(ctx context.Context, root, base string) (RepositorySnapshot, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return RepositorySnapshot{}, ErrScope
	}
	commitBody, err := gitOutput(ctx, root, "rev-parse", "--verify", base+"^{commit}")
	if err != nil {
		return RepositorySnapshot{}, fmt.Errorf("resolve base commit: %w", err)
	}
	commit := strings.TrimSpace(string(commitBody))
	if len(commit) != 40 {
		return RepositorySnapshot{}, ErrScope
	}
	tree, err := gitOutput(ctx, root, "ls-tree", "-rz", "--full-tree", commit)
	if err != nil {
		return RepositorySnapshot{}, err
	}
	var entries []*reasoningv1.RepositoryEntry
	for _, record := range bytes.Split(tree, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		header, name, ok := bytes.Cut(record, []byte{'\t'})
		fields := bytes.Fields(header)
		if !ok || len(fields) != 3 || string(fields[1]) != "blob" || !safeRepoPath(string(name)) {
			return RepositorySnapshot{}, ErrScope
		}
		body, err := gitOutput(ctx, root, "cat-file", "blob", string(fields[2]))
		if err != nil {
			return RepositorySnapshot{}, err
		}
		sum := sha256.Sum256(body)
		entries = append(entries, &reasoningv1.RepositoryEntry{
			Path: string(name), Kind: "blob", Sha256: hex.EncodeToString(sum[:]),
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	snapshot := RepositorySnapshot{BaseCommit: commit, Entries: entries}
	if err := snapshot.Validate(commit); err != nil {
		return RepositorySnapshot{}, err
	}
	return snapshot, nil
}

func BuildImplementationContext(
	ctx context.Context,
	root, commit string,
	snapshot RepositorySnapshot,
	readable, _ []string,
	limits ContextLimits,
) ([]*reasoningv1.RepositoryContextFile, error) {
	if commit != snapshot.BaseCommit || limits.MaxFiles <= 0 || limits.MaxBytes <= 0 {
		return nil, ErrScope
	}
	var result []*reasoningv1.RepositoryContextFile
	var total int64
	for _, entry := range snapshot.Entries {
		if !withinAny(entry.Path, readable) {
			continue
		}
		if len(result) == limits.MaxFiles {
			return nil, ErrScopeLimit
		}
		body, err := gitOutput(ctx, root, "show", commit+":"+entry.Path)
		if err != nil {
			return nil, err
		}
		total += int64(len(body))
		if total > limits.MaxBytes {
			return nil, ErrScopeLimit
		}
		if Digest(body) != entry.Sha256 {
			return nil, ErrScope
		}
		result = append(result, &reasoningv1.RepositoryContextFile{
			Path: entry.Path, Sha256: entry.Sha256, Content: string(body),
		})
	}
	if len(result) == 0 {
		return nil, ErrScope
	}
	return result, nil
}

func gitOutput(ctx context.Context, root string, args ...string) ([]byte, error) {
	base := []string{"-C", root, "-c", "core.hooksPath=/dev/null", "-c", "diff.external=", "-c", "filter.lfs.smudge=", "-c", "filter.lfs.clean="}
	command := exec.CommandContext(ctx, "git", append(base, args...)...)
	command.Env = []string{"PATH=/usr/bin:/bin", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_OPTIONAL_LOCKS=0"}
	output, err := command.Output()
	if err != nil {
		return nil, err
	}
	return output, nil
}

func safeRepoPath(value string) bool {
	return value != "" && value == path.Clean(value) && !path.IsAbs(value) &&
		value != "." && value != ".git" && !strings.HasPrefix(value, "../") &&
		!strings.HasPrefix(value, ".git/")
}

func withinAny(value string, roots []string) bool {
	for _, root := range roots {
		root = strings.TrimSuffix(path.Clean(root), "/")
		if safeRepoPath(root) && (value == root || strings.HasPrefix(value, root+"/")) {
			return true
		}
	}
	return false
}
