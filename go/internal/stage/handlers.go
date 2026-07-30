// Package stage composes the durable runtime stages from owning services.
package stage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	reasoningv1 "github.com/Standard-Syntax/basic/go/gen/harness/reasoning/v1"
	"github.com/Standard-Syntax/basic/go/internal/execution"
	"github.com/Standard-Syntax/basic/go/internal/orchestration"
	"github.com/Standard-Syntax/basic/go/internal/reasoning/gateway"
	"github.com/Standard-Syntax/basic/go/internal/review"
	"github.com/Standard-Syntax/basic/go/internal/runtime"
	"github.com/Standard-Syntax/basic/go/internal/verification"
	"github.com/Standard-Syntax/basic/go/internal/workflow"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

type ArtifactStore interface {
	Put(context.Context, []byte) (workflow.ArtifactRef, error)
	Get(context.Context, workflow.ArtifactRef) ([]byte, error)
}

type RuntimeStore interface {
	GetRun(context.Context, string) (runtime.RunBinding, error)
	GetTask(context.Context, string, string) (runtime.TaskBinding, error)
	CheckpointRepository(context.Context, string, workflow.ArtifactRef) error
	CompletedResult(
		context.Context, string, *string, uint32, string,
	) (workflow.ArtifactRef, error)
}

type WorkflowStore interface {
	ExecuteRun(context.Context, workflow.RunCommand) (workflow.CommandResult, error)
	ExecuteTask(context.Context, workflow.TaskCommand) (workflow.CommandResult, error)
	GetRun(context.Context, string) (workflow.Run, error)
	GetTask(context.Context, string, string) (workflow.Task, error)
}

type ImplementationGateway interface {
	ProposeImplementation(
		context.Context, *reasoningv1.ImplementationRequest,
	) (gateway.Outcome, error)
}

type ExecutionService interface {
	Execute(context.Context, execution.Request) (execution.Result, error)
}

type VerificationService interface {
	Verify(context.Context, verification.Request) (verification.Result, error)
}

type ReviewService interface {
	Review(context.Context, review.Request) (review.Result, error)
}

type Config struct {
	RepositoryRoot               string
	ServiceActorID               string
	ReasoningActorID             string
	ImplementationManifestDigest string
	ReviewManifestDigest         string
	TaskLeaseDuration            time.Duration
	ContextLimits                runtime.ContextLimits
	ImplementationBudget         runtime.ReasoningLimits
	ReviewBudget                 runtime.ReasoningLimits
}

type Handlers struct {
	config       Config
	artifacts    ArtifactStore
	runtime      RuntimeStore
	workflow     WorkflowStore
	gateway      ImplementationGateway
	execution    ExecutionService
	verification VerificationService
	review       ReviewService
	now          func() time.Time
}

func New(
	config Config, artifacts ArtifactStore, runtimeStore RuntimeStore,
	workflowStore WorkflowStore, implementationGateway ImplementationGateway,
	executionService ExecutionService, verificationService VerificationService,
	reviewService ReviewService,
) (*Handlers, error) {
	if artifacts == nil || runtimeStore == nil || workflowStore == nil ||
		implementationGateway == nil || executionService == nil ||
		verificationService == nil || reviewService == nil {
		return nil, errors.New("all stage service ports are required")
	}
	if _, err := uuid.Parse(config.ServiceActorID); err != nil {
		return nil, errors.New("workflow service actor ID is required")
	}
	if _, err := uuid.Parse(config.ReasoningActorID); err != nil {
		return nil, errors.New("reasoning service actor ID is required")
	}
	if config.TaskLeaseDuration <= 0 || config.ImplementationManifestDigest == "" ||
		config.ReviewManifestDigest == "" {
		return nil, errors.New("incomplete stage configuration")
	}
	return &Handlers{
		config: config, artifacts: artifacts, runtime: runtimeStore,
		workflow: workflowStore, gateway: implementationGateway,
		execution: executionService, verification: verificationService,
		review: reviewService, now: time.Now,
	}, nil
}

func (h *Handlers) Map() map[string]orchestration.Handler {
	return map[string]orchestration.Handler{
		orchestration.StageStart: orchestration.HandlerFunc(h.start),
		orchestration.StageImplementationRequest: orchestration.HandlerFunc(
			h.implementationRequest,
		),
		orchestration.StageReasoning:        orchestration.HandlerFunc(h.reason),
		orchestration.StageExecution:        orchestration.HandlerFunc(h.execute),
		orchestration.StageVerification:     orchestration.HandlerFunc(h.verify),
		orchestration.StageReview:           orchestration.HandlerFunc(h.reviewTask),
		orchestration.StageAwaitingApproval: orchestration.HandlerFunc(h.awaitApproval),
	}
}

func (h *Handlers) start(
	ctx context.Context, job runtime.Job, ids orchestration.Identities,
) (orchestration.HandlerResult, error) {
	if job.TaskID == nil {
		return orchestration.HandlerResult{}, runtime.ErrConflict
	}
	binding, err := h.runtime.GetRun(ctx, job.RunID)
	if err != nil {
		return orchestration.HandlerResult{}, err
	}
	snapshot, err := runtime.SnapshotRepository(ctx, h.config.RepositoryRoot, binding.BaseCommit)
	if err != nil {
		return orchestration.HandlerResult{}, err
	}
	body, err := json.Marshal(snapshot)
	if err != nil {
		return orchestration.HandlerResult{}, err
	}
	ref, err := h.artifacts.Put(ctx, body)
	if err != nil {
		return orchestration.HandlerResult{}, err
	}
	if err := h.runtime.CheckpointRepository(ctx, job.RunID, ref); err != nil {
		return orchestration.HandlerResult{}, err
	}
	run, err := h.workflow.GetRun(ctx, job.RunID)
	if err != nil {
		return orchestration.HandlerResult{}, err
	}
	at := h.now().UTC()
	if run.State == workflow.RunStateReady {
		if _, err := h.workflow.ExecuteRun(ctx, workflow.StartRun{
			Meta: h.envelope(ids.CommandID, h.config.ServiceActorID,
				workflow.ActorWorkflowService, run.Revision, at),
			ID: run.ID,
		}); err != nil {
			return orchestration.HandlerResult{}, err
		}
	}
	task, err := h.workflow.GetTask(ctx, job.RunID, *job.TaskID)
	if err != nil {
		return orchestration.HandlerResult{}, err
	}
	if task.State == workflow.TaskStateReady {
		lease := workflow.LeaseRef{
			ID: orchestration.StableID(
				job.RunID, *job.TaskID, fmt.Sprint(job.Attempt), "lease",
			),
			OwnerID: h.config.ServiceActorID, ExpiresAt: at.Add(h.config.TaskLeaseDuration),
			FencingToken: job.Attempt,
		}
		if _, err := h.workflow.ExecuteTask(ctx, workflow.LeaseTask{
			Meta: h.envelope(orchestration.StableID(ids.CommandID, "lease"),
				h.config.ServiceActorID, workflow.ActorWorkflowService, task.Revision, at),
			Run: job.RunID, ID: *job.TaskID, Lease: lease,
		}); err != nil {
			return orchestration.HandlerResult{}, err
		}
	}
	return orchestration.HandlerResult{Artifact: ref, Continue: true}, nil
}

func (h *Handlers) implementationRequest(
	ctx context.Context, job runtime.Job, ids orchestration.Identities,
) (orchestration.HandlerResult, error) {
	if job.TaskID == nil {
		return orchestration.HandlerResult{}, runtime.ErrConflict
	}
	runBinding, taskBinding, task, snapshot, err := h.boundTask(ctx, job)
	if err != nil {
		return orchestration.HandlerResult{}, err
	}
	taskBody, err := h.artifacts.Get(ctx, taskBinding.ApprovedTask)
	if err != nil {
		return orchestration.HandlerResult{}, err
	}
	var plannedTask reasoningv1.PlannedTask
	if err := proto.Unmarshal(taskBody, &plannedTask); err != nil {
		return orchestration.HandlerResult{}, err
	}
	request, err := runtime.BuildImplementationRequest(ctx, h.artifacts,
		runtime.ImplementationInput{
			RequestID: ids.InvocationID, RunID: job.RunID, TaskID: *job.TaskID,
			ManifestDigest: h.config.ImplementationManifestDigest, Attempt: job.Attempt,
			CreatedAt: task.UpdatedAt, ExpiresAt: task.Lease.ExpiresAt,
			BaseCommit:              runBinding.BaseCommit,
			ApprovedSpecificationID: runBinding.ApprovedSpecification.Digest,
			ApprovedSpecification:   *runBinding.ApprovedSpecification,
			ApprovedTask:            taskBinding.ApprovedTask, Task: &plannedTask,
			Snapshot: snapshot, RepositoryRoot: h.config.RepositoryRoot,
			ContextLimits: h.config.ContextLimits, Budget: h.config.ImplementationBudget,
		})
	if err != nil {
		return orchestration.HandlerResult{}, err
	}
	body, err := proto.MarshalOptions{Deterministic: true}.Marshal(request)
	if err != nil {
		return orchestration.HandlerResult{}, err
	}
	ref, err := h.artifacts.Put(ctx, body)
	return orchestration.HandlerResult{Artifact: ref, Continue: true}, err
}

func (h *Handlers) reason(
	ctx context.Context, job runtime.Job, ids orchestration.Identities,
) (orchestration.HandlerResult, error) {
	requestRef, request, task, err := h.implementation(ctx, job)
	if err != nil {
		return orchestration.HandlerResult{}, err
	}
	if task.State == workflow.TaskStateLeased {
		if _, err := h.workflow.ExecuteTask(ctx, workflow.StartReasoning{
			Meta: h.envelope(ids.CommandID, h.config.ReasoningActorID,
				workflow.ActorReasoningService, task.Revision, h.now()),
			Run: job.RunID, ID: *job.TaskID, Lease: *task.Lease,
		}); err != nil {
			return orchestration.HandlerResult{}, err
		}
		task, err = h.workflow.GetTask(ctx, job.RunID, *job.TaskID)
		if err != nil {
			return orchestration.HandlerResult{}, err
		}
	}
	outcome, err := h.gateway.ProposeImplementation(ctx, request)
	if err != nil {
		return orchestration.HandlerResult{}, err
	}
	if outcome.Rejection != nil {
		body, err := proto.MarshalOptions{Deterministic: true}.Marshal(outcome.Rejection)
		if err != nil {
			return orchestration.HandlerResult{}, err
		}
		ref, err := h.artifacts.Put(ctx, body)
		if err != nil {
			return orchestration.HandlerResult{}, err
		}
		_, err = h.workflow.ExecuteTask(ctx, workflow.RejectTaskProposal{
			Meta: h.envelope(orchestration.StableID(ids.CommandID, "reject"),
				h.config.ReasoningActorID, workflow.ActorReasoningService,
				task.Revision, h.now()),
			Run: job.RunID, ID: *job.TaskID, Proposal: ref,
			Reason: outcome.Rejection.GetSummary(),
		})
		return orchestration.HandlerResult{Artifact: ref, Continue: false}, err
	}
	if outcome.Proposal == nil {
		return orchestration.HandlerResult{}, errors.New("reasoning returned no proposal")
	}
	ref := workflow.ArtifactRef{
		URI: outcome.ProposalArtifact.URI, Digest: outcome.ProposalArtifact.SHA256,
	}
	_ = requestRef
	return orchestration.HandlerResult{Artifact: ref, Continue: true}, nil
}

func (h *Handlers) execute(
	ctx context.Context, job runtime.Job, ids orchestration.Identities,
) (orchestration.HandlerResult, error) {
	_, request, task, err := h.implementation(ctx, job)
	if err != nil {
		return orchestration.HandlerResult{}, err
	}
	proposalRef, err := h.previous(ctx, job, orchestration.StageReasoning)
	if err != nil {
		return orchestration.HandlerResult{}, err
	}
	proposalBody, err := h.artifacts.Get(ctx, proposalRef)
	if err != nil {
		return orchestration.HandlerResult{}, err
	}
	var proposal reasoningv1.ImplementationProposal
	if err := proto.Unmarshal(proposalBody, &proposal); err != nil {
		return orchestration.HandlerResult{}, err
	}
	expectedRevision, err := executionExpectedRevision(task)
	if err != nil {
		return orchestration.HandlerResult{}, err
	}
	result, err := h.execution.Execute(ctx, execution.Request{
		ExecutionID:        ids.ExecutionID,
		ExecutionTimestamp: request.GetEnvelope().GetCreatedAt().AsTime(),
		Implementation:     request, Proposal: &proposal, ProposalArtifact: proposalRef,
		Lease: *task.Lease, ExpectedTaskRevision: expectedRevision,
	})
	if err != nil {
		return orchestration.HandlerResult{}, err
	}
	return orchestration.HandlerResult{Artifact: result.ReportArtifact, Continue: true}, nil
}

func executionExpectedRevision(task workflow.Task) (uint64, error) {
	switch task.State {
	case workflow.TaskStateReasoning:
		return task.Revision, nil
	case workflow.TaskStateExecuting:
		if task.Revision > 1 {
			return task.Revision - 1, nil
		}
	case workflow.TaskStateVerifying:
		if task.Revision > 2 {
			return task.Revision - 2, nil
		}
	}
	return 0, fmt.Errorf("task state %s cannot resume execution", task.State)
}

func (h *Handlers) verify(
	ctx context.Context, job runtime.Job, ids orchestration.Identities,
) (orchestration.HandlerResult, error) {
	_, request, task, err := h.implementation(ctx, job)
	if err != nil {
		return orchestration.HandlerResult{}, err
	}
	executionRef, err := h.previous(ctx, job, orchestration.StageExecution)
	if err != nil {
		return orchestration.HandlerResult{}, err
	}
	executionReport, err := h.executionReport(ctx, executionRef)
	if err != nil {
		return orchestration.HandlerResult{}, err
	}
	requirements := make([]verification.AcceptanceRequirement, 0,
		len(request.GetAcceptanceCriterionIds()))
	for _, criterion := range request.GetAcceptanceCriterionIds() {
		requirements = append(requirements, verification.AcceptanceRequirement{
			CriterionID: criterion, CheckIDs: request.GetAvailableCheckIds(),
		})
	}
	expectedRevision, err := verificationExpectedRevision(task)
	if err != nil {
		return orchestration.HandlerResult{}, err
	}
	result, err := h.verification.Verify(ctx, verification.Request{
		VerificationID:        ids.VerificationID,
		VerificationTimestamp: request.GetEnvelope().GetCreatedAt().AsTime(),
		Implementation:        request, ExecutionArtifact: executionRef,
		CandidateCommit:      executionReport.CandidateCommit,
		ExpectedTaskRevision: expectedRevision, Requirements: requirements,
	})
	if err != nil {
		return orchestration.HandlerResult{}, err
	}
	return orchestration.HandlerResult{
		Artifact: result.ReportArtifact, Continue: result.Passed,
	}, nil
}

func verificationExpectedRevision(task workflow.Task) (uint64, error) {
	switch task.State {
	case workflow.TaskStateVerifying:
		return task.Revision, nil
	case workflow.TaskStateReviewing:
		if task.Revision > 1 {
			return task.Revision - 1, nil
		}
	}
	return 0, fmt.Errorf("task state %s cannot resume verification", task.State)
}

func (h *Handlers) reviewTask(
	ctx context.Context, job runtime.Job, ids orchestration.Identities,
) (orchestration.HandlerResult, error) {
	_, implementation, task, err := h.implementation(ctx, job)
	if err != nil {
		return orchestration.HandlerResult{}, err
	}
	runBinding, err := h.runtime.GetRun(ctx, job.RunID)
	if err != nil {
		return orchestration.HandlerResult{}, err
	}
	taskBinding, err := h.runtime.GetTask(ctx, job.RunID, *job.TaskID)
	if err != nil {
		return orchestration.HandlerResult{}, err
	}
	proposalRef, err := h.previous(ctx, job, orchestration.StageReasoning)
	if err != nil {
		return orchestration.HandlerResult{}, err
	}
	executionRef, err := h.previous(ctx, job, orchestration.StageExecution)
	if err != nil {
		return orchestration.HandlerResult{}, err
	}
	verificationRef, err := h.previous(ctx, job, orchestration.StageVerification)
	if err != nil {
		return orchestration.HandlerResult{}, err
	}
	executionReport, err := h.executionReport(ctx, executionRef)
	if err != nil {
		return orchestration.HandlerResult{}, err
	}
	verificationReport, err := h.verificationReport(ctx, verificationRef)
	if err != nil {
		return orchestration.HandlerResult{}, err
	}
	request, err := runtime.BuildReviewRequest(&runtime.ReviewInput{
		RequestID: ids.InvocationID, RunID: job.RunID, TaskID: *job.TaskID,
		ManifestDigest: h.config.ReviewManifestDigest, Attempt: job.Attempt,
		CreatedAt: task.UpdatedAt, ExpiresAt: task.Lease.ExpiresAt,
		ApprovedSpecification: *runBinding.ApprovedSpecification,
		ApprovedTask:          taskBinding.ApprovedTask, ImplementationProposal: proposalRef,
		ExecutionArtifact: executionRef, VerificationArtifact: verificationRef,
		Execution: executionReport, Verification: verificationReport,
		AuthorizedWritablePaths:        implementation.GetWritablePaths(),
		ApprovedAcceptanceCriterionIDs: implementation.GetAcceptanceCriterionIds(),
		Budget:                         h.config.ReviewBudget,
	})
	if err != nil {
		return orchestration.HandlerResult{}, err
	}
	result, err := h.review.Review(ctx, review.Request{
		ReviewID: ids.ReviewID, ReviewTimestamp: h.now(), Review: request,
		ExecutionArtifact: executionRef, VerificationArtifact: verificationRef,
		ExclusiveResourceLabels: nil, ExpectedTaskRevision: task.Revision,
	})
	if err != nil {
		return orchestration.HandlerResult{}, err
	}
	return orchestration.HandlerResult{
		Artifact: result.ReportArtifact, Continue: result.Passed,
	}, nil
}

func (h *Handlers) awaitApproval(
	ctx context.Context, job runtime.Job, _ orchestration.Identities,
) (orchestration.HandlerResult, error) {
	ref, err := h.previous(ctx, job, orchestration.StageReview)
	return orchestration.HandlerResult{Artifact: ref, Continue: false}, err
}

func (h *Handlers) boundTask(
	ctx context.Context, job runtime.Job,
) (runtime.RunBinding, runtime.TaskBinding, workflow.Task, runtime.RepositorySnapshot, error) {
	runBinding, err := h.runtime.GetRun(ctx, job.RunID)
	if err != nil || runBinding.RepositoryMap == nil ||
		runBinding.ApprovedSpecification == nil || job.TaskID == nil {
		return runtime.RunBinding{}, runtime.TaskBinding{}, workflow.Task{},
			runtime.RepositorySnapshot{}, firstError(err, runtime.ErrNotFound)
	}
	taskBinding, err := h.runtime.GetTask(ctx, job.RunID, *job.TaskID)
	if err != nil {
		return runtime.RunBinding{}, runtime.TaskBinding{}, workflow.Task{},
			runtime.RepositorySnapshot{}, err
	}
	task, err := h.workflow.GetTask(ctx, job.RunID, *job.TaskID)
	if err != nil || task.Lease == nil {
		return runtime.RunBinding{}, runtime.TaskBinding{}, workflow.Task{},
			runtime.RepositorySnapshot{}, firstError(err, workflow.ErrInvalidTransition)
	}
	body, err := h.artifacts.Get(ctx, *runBinding.RepositoryMap)
	if err != nil {
		return runtime.RunBinding{}, runtime.TaskBinding{}, workflow.Task{},
			runtime.RepositorySnapshot{}, err
	}
	var snapshot runtime.RepositorySnapshot
	if err := json.Unmarshal(body, &snapshot); err != nil {
		return runtime.RunBinding{}, runtime.TaskBinding{}, workflow.Task{},
			runtime.RepositorySnapshot{}, err
	}
	return runBinding, taskBinding, task, snapshot, nil
}

func (h *Handlers) implementation(
	ctx context.Context, job runtime.Job,
) (workflow.ArtifactRef, *reasoningv1.ImplementationRequest, workflow.Task, error) {
	if job.TaskID == nil {
		return workflow.ArtifactRef{}, nil, workflow.Task{}, runtime.ErrConflict
	}
	ref, err := h.previous(ctx, job, orchestration.StageImplementationRequest)
	if err != nil {
		return workflow.ArtifactRef{}, nil, workflow.Task{}, err
	}
	body, err := h.artifacts.Get(ctx, ref)
	if err != nil {
		return workflow.ArtifactRef{}, nil, workflow.Task{}, err
	}
	var request reasoningv1.ImplementationRequest
	if err := proto.Unmarshal(body, &request); err != nil {
		return workflow.ArtifactRef{}, nil, workflow.Task{}, err
	}
	task, err := h.workflow.GetTask(ctx, job.RunID, *job.TaskID)
	return ref, &request, task, err
}

func (h *Handlers) previous(
	ctx context.Context, job runtime.Job, stage string,
) (workflow.ArtifactRef, error) {
	return h.runtime.CompletedResult(ctx, job.RunID, job.TaskID, job.Attempt, stage)
}

func (h *Handlers) executionReport(
	ctx context.Context, ref workflow.ArtifactRef,
) (execution.ExecutionReport, error) {
	var report execution.ExecutionReport
	body, err := h.artifacts.Get(ctx, ref)
	if err == nil {
		err = json.Unmarshal(body, &report)
	}
	return report, err
}

func (h *Handlers) verificationReport(
	ctx context.Context, ref workflow.ArtifactRef,
) (verification.VerificationReport, error) {
	var report verification.VerificationReport
	body, err := h.artifacts.Get(ctx, ref)
	if err == nil {
		err = json.Unmarshal(body, &report)
	}
	return report, err
}

func (*Handlers) envelope(
	commandID, actorID string, kind workflow.ActorKind, revision uint64, at time.Time,
) workflow.CommandEnvelope {
	return workflow.CommandEnvelope{
		CommandID: commandID, Actor: workflow.Actor{ID: actorID, Kind: kind},
		ExpectedRevision: revision, Timestamp: at.UTC(),
		CorrelationID: commandID, CausationID: commandID,
	}
}

func firstError(actual, fallback error) error {
	if actual != nil {
		return actual
	}
	return fallback
}
