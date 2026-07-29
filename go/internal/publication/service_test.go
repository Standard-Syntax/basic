package publication

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Standard-Syntax/basic/go/internal/approval"
	"github.com/Standard-Syntax/basic/go/internal/execution"
	"github.com/Standard-Syntax/basic/go/internal/review"
	"github.com/Standard-Syntax/basic/go/internal/verification"
	"github.com/Standard-Syntax/basic/go/internal/workflow"
	"github.com/google/uuid"
)

type memoryArtifacts struct {
	mu     sync.Mutex
	values map[string][]byte
}

func newMemoryArtifacts() *memoryArtifacts {
	return &memoryArtifacts{values: make(map[string][]byte)}
}

func (m *memoryArtifacts) Get(
	_ context.Context, ref workflow.ArtifactRef, limit int64,
) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	value, ok := m.values[ref.URI]
	if !ok {
		return nil, errors.New("not found")
	}
	if int64(len(value)) > limit {
		return nil, ErrResponseLimit
	}
	return append([]byte(nil), value...), nil
}

func (m *memoryArtifacts) Put(
	_ context.Context, value []byte,
) (workflow.ArtifactRef, error) {
	sum := sha256.Sum256(value)
	digest := hex.EncodeToString(sum[:])
	ref := workflow.ArtifactRef{URI: "artifact://sha256/" + digest, Digest: digest}
	m.mu.Lock()
	m.values[ref.URI] = append([]byte(nil), value...)
	m.mu.Unlock()
	return ref, nil
}

type fakeGit struct {
	base         string
	branch       string
	candidate    string
	baseCalls    atomic.Int32
	publishCalls atomic.Int32
}

func (f *fakeGit) BaseHead(context.Context) (string, error) {
	f.baseCalls.Add(1)
	return f.base, nil
}
func (f *fakeGit) BranchHead(context.Context, string) (string, bool, error) {
	return f.candidate, f.candidate != "", nil
}
func (f *fakeGit) Publish(_ context.Context, branch, candidate string) (bool, error) {
	f.publishCalls.Add(1)
	if f.candidate != "" && f.candidate != candidate {
		return false, ErrBranchConflict
	}
	replay := f.candidate == candidate
	f.branch, f.candidate = branch, candidate
	return replay, nil
}

type fakePulls struct {
	mu          sync.Mutex
	value       *DraftPullRequest
	findCalls   int
	createCalls int
	ambiguous   bool
}

func (f *fakePulls) FindDraft(
	_ context.Context, input DraftPullRequestInput,
) (DraftPullRequest, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.findCalls++
	if f.value == nil {
		return DraftPullRequest{}, false, nil
	}
	return *f.value, true, nil
}

func (f *fakePulls) CreateDraft(
	_ context.Context, input DraftPullRequestInput,
) (DraftPullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCalls++
	value := DraftPullRequest{
		Number: 41, URL: "https://example.invalid/pull/41", State: "open",
		Draft: true, Head: input.Head, Base: input.Base, Marker: input.Marker,
	}
	f.value = &value
	if f.ambiguous {
		f.ambiguous = false
		return DraftPullRequest{}, errors.New("connection reset")
	}
	return value, nil
}

type fakeWorkflow struct {
	mu      sync.Mutex
	command workflow.RunCommand
	calls   int
}

func (f *fakeWorkflow) ExecuteRun(
	_ context.Context, command workflow.RunCommand,
) (workflow.CommandResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.command != nil {
		return workflow.CommandResult{Replay: true}, nil
	}
	f.command = command
	return workflow.CommandResult{Revision: command.Envelope().ExpectedRevision + 1}, nil
}

func TestServicePublishesDraftAndExactReplayHasNoExternalEffects(t *testing.T) {
	service, request, git, pulls, workflowPort := publicationFixture(t)
	result, err := service.Publish(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.PullRequestNumber != 41 || result.Branch != "harness/"+request.RunID ||
		result.PublicationArtifact.Digest == "" || result.Replay {
		t.Fatalf("result = %#v", result)
	}
	command, ok := workflowPort.command.(workflow.RecordDraftPullRequest)
	if !ok || command.ID != request.RunID || command.CandidateCommit != request.CandidateCommit ||
		!command.Approval.Equal(request.Approval) ||
		!command.Publication.Equal(result.PublicationArtifact) {
		t.Fatalf("workflow command = %#v", workflowPort.command)
	}
	baseCalls, publishCalls := git.baseCalls.Load(), git.publishCalls.Load()
	findCalls, createCalls, workflowCalls := pulls.findCalls, pulls.createCalls, workflowPort.calls
	replay, err := service.Publish(t.Context(), request)
	if err != nil || !replay.Replay ||
		git.baseCalls.Load() != baseCalls || git.publishCalls.Load() != publishCalls ||
		pulls.findCalls != findCalls || pulls.createCalls != createCalls ||
		workflowPort.calls != workflowCalls {
		t.Fatalf("replay=%#v err=%v effects=%d/%d %d/%d %d", replay, err,
			git.baseCalls.Load(), git.publishCalls.Load(), pulls.findCalls,
			pulls.createCalls, workflowPort.calls)
	}
}

func TestServiceRecoversAmbiguousPullRequestCreation(t *testing.T) {
	service, request, _, pulls, _ := publicationFixture(t)
	pulls.ambiguous = true
	result, err := service.Publish(t.Context(), request)
	if err != nil || result.PullRequestNumber != 41 ||
		pulls.createCalls != 1 || pulls.findCalls != 2 {
		t.Fatalf("result=%#v err=%v find=%d create=%d",
			result, err, pulls.findCalls, pulls.createCalls)
	}
}

func TestServiceRejectsInvalidEvidenceAndBaseDriftBeforeMutation(t *testing.T) {
	service, request, git, pulls, workflowPort := publicationFixture(t)
	request.CandidateCommit = "1111111111111111111111111111111111111111"
	if _, err := service.Publish(t.Context(), request); err == nil {
		t.Fatal("mismatched evidence accepted")
	}
	if git.baseCalls.Load() != 0 || git.publishCalls.Load() != 0 ||
		pulls.findCalls != 0 || workflowPort.calls != 0 {
		t.Fatal("invalid evidence caused an external effect")
	}

	service, request, git, pulls, workflowPort = publicationFixture(t)
	git.base = "2222222222222222222222222222222222222222"
	if _, err := service.Publish(t.Context(), request); !errors.Is(err, ErrBaseDrift) {
		t.Fatalf("base drift error = %v", err)
	}
	if git.publishCalls.Load() != 0 || pulls.findCalls != 0 || workflowPort.calls != 0 {
		t.Fatal("base drift caused publication")
	}
}

func TestServiceConflictingPublicationIDFailsClosed(t *testing.T) {
	service, request, _, pulls, _ := publicationFixture(t)
	if _, err := service.Publish(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	conflict := request
	conflict.ExpectedRunRevision++
	if _, err := service.Publish(t.Context(), conflict); !errors.Is(err, ErrPublicationConflict) {
		t.Fatalf("conflict error = %v", err)
	}
	if pulls.createCalls != 1 {
		t.Fatalf("duplicate PR effects = %d", pulls.createCalls)
	}
}

func publicationFixture(
	t *testing.T,
) (*Service, Request, *fakeGit, *fakePulls, *fakeWorkflow) {
	t.Helper()
	store := newMemoryArtifacts()
	putJSON := func(value any) workflow.ArtifactRef {
		body, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		ref, err := store.Put(t.Context(), body)
		if err != nil {
			t.Fatal(err)
		}
		return ref
	}
	runID := uuid.NewString()
	base := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	candidate := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	specification := putJSON(map[string]any{
		"schema_version": "1", "title": "Implement approved publication",
	})
	implementation := putJSON(map[string]any{
		"schema_version": "1", "candidate": candidate,
	})
	executionRef := putJSON(execution.ExecutionReport{
		SchemaVersion: "1", ExecutionID: uuid.NewString(), RunID: runID,
		TaskID: uuid.NewString(), Attempt: 1, Proposal: implementation,
		BaseCommit: base, CandidateCommit: candidate, CandidateRef: "refs/candidates/1",
	})
	verificationRef := putJSON(verification.VerificationReport{
		SchemaVersion: "1", VerificationID: uuid.NewString(), RunID: runID,
		TaskID: uuid.NewString(), Attempt: 1, Execution: executionRef,
		BaseCommit: base, CandidateCommit: candidate, Passed: true,
		Checks: []verification.CheckResult{{
			CheckID: "make-check-v1", CandidateCommit: candidate, Passed: true,
		}},
		Coverage: []verification.CriterionCoverage{{
			CriterionID: "acceptance-1", CheckIDs: []string{"make-check-v1"},
			Covered: true, Passed: true,
		}},
	})
	reviewRef := putJSON(review.ReviewReport{
		SchemaVersion: "1", ReviewID: uuid.NewString(), RunID: runID,
		TaskID: uuid.NewString(), Attempt: 1, CandidateCommit: candidate,
		ApprovedSpecificationDigest:  specification.Digest,
		ApprovedTaskDigest:           strings.Repeat("c", 64),
		ImplementationProposalDigest: implementation.Digest,
		Execution:                    executionRef, Verification: verificationRef,
		Recommendation: "REVIEW_RECOMMENDATION_ADVISORY_ACCEPT", Passed: true,
	})
	approvalRef := putJSON(approval.TaskApproval{
		SchemaVersion: "1", ApprovalID: uuid.NewString(), Decision: "approve",
		RunID: runID, TaskID: uuid.NewString(), CandidateCommit: candidate,
		ApprovedSpecificationDigest: specification.Digest,
		ApprovedTaskDigest:          strings.Repeat("c", 64),
		Implementation:              implementation, Execution: executionRef,
		Verification: verificationRef, Review: reviewRef,
	})
	request := Request{
		PublicationID: uuid.NewString(), PublicationTimestamp: time.Now().UTC(),
		RunID: runID, BaseCommit: base, CandidateCommit: candidate,
		Specification: specification, Implementation: implementation,
		Execution: executionRef, Verification: verificationRef,
		Review: reviewRef, Approval: approvalRef, ExpectedRunRevision: 11,
	}
	git := &fakeGit{base: base}
	pulls := &fakePulls{}
	workflowPort := &fakeWorkflow{}
	service, err := NewService(Config{
		RepositoryRoot: t.TempDir(), RepositoryOwner: "owner", RepositoryName: "repo",
		Remote: "origin", BaseBranch: "main", BranchPrefix: "harness/",
		ActorID: uuid.NewString(),
	}, store, workflowPort, git, pulls, NewMemoryPublicationLedger())
	if err != nil {
		t.Fatal(err)
	}
	return service, request, git, pulls, workflowPort
}
