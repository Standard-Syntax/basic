package stage

import (
	"context"
	"errors"
	"time"

	reasoningv1 "github.com/Standard-Syntax/basic/go/gen/harness/reasoning/v1"
	"github.com/Standard-Syntax/basic/go/internal/beta"
	"github.com/Standard-Syntax/basic/go/internal/orchestration"
	"github.com/Standard-Syntax/basic/go/internal/reasoning/gateway"
	"github.com/Standard-Syntax/basic/go/internal/runtime"
	"github.com/Standard-Syntax/basic/go/internal/workflow"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

type SpecificationGateway interface {
	ProposeSpecification(context.Context, *reasoningv1.SpecificationRequest) (gateway.SpecificationOutcome, error)
}

type PlanningGateway interface {
	ProposeTaskGraph(context.Context, *reasoningv1.TaskPlanningRequest) (gateway.PlanningOutcome, error)
}

type LivePlanningConfig struct {
	ServiceActorID, ReasoningActorID                    string
	SpecificationManifestDigest, PlanningManifestDigest string
	ReasoningTimeout                                    time.Duration
	SpecificationBudget, PlanningBudget                 runtime.ReasoningLimits
	Policy                                              beta.Policy
}

type LivePlanningHandlers struct {
	config         LivePlanningConfig
	artifacts      ArtifactStore
	runtime        RuntimeStore
	workflow       WorkflowStore
	specifications SpecificationGateway
	planning       PlanningGateway
	now            func() time.Time
}

func NewLivePlanning(config LivePlanningConfig, artifacts ArtifactStore, runtimeStore RuntimeStore,
	workflowStore WorkflowStore, specifications SpecificationGateway,
	planning PlanningGateway) (*LivePlanningHandlers, error) {
	if artifacts == nil || runtimeStore == nil || workflowStore == nil ||
		specifications == nil || planning == nil {
		return nil, errors.New("all live planning service ports are required")
	}
	if _, err := uuid.Parse(config.ServiceActorID); err != nil {
		return nil, errors.New("workflow service actor ID is required")
	}
	if _, err := uuid.Parse(config.ReasoningActorID); err != nil {
		return nil, errors.New("reasoning service actor ID is required")
	}
	if config.SpecificationManifestDigest == "" || config.PlanningManifestDigest == "" ||
		config.ReasoningTimeout <= 0 {
		return nil, errors.New("incomplete live planning configuration")
	}
	if err := config.Policy.Validate(); err != nil {
		return nil, err
	}
	return &LivePlanningHandlers{config: config, artifacts: artifacts, runtime: runtimeStore,
		workflow: workflowStore, specifications: specifications, planning: planning, now: time.Now}, nil
}

func (h *LivePlanningHandlers) Map() map[string]orchestration.Handler {
	return map[string]orchestration.Handler{
		orchestration.StageSpecificationReasoning: orchestration.HandlerFunc(h.specify),
		orchestration.StagePlanningReasoning:      orchestration.HandlerFunc(h.plan),
	}
}

func (h *LivePlanningHandlers) specify(ctx context.Context, job runtime.Job,
	ids orchestration.Identities) (orchestration.HandlerResult, error) {
	if job.TaskID != nil {
		return orchestration.HandlerResult{}, runtime.ErrConflict
	}
	binding, err := h.runtime.GetRun(ctx, job.RunID)
	if err != nil {
		return orchestration.HandlerResult{}, err
	}
	intake, err := runtime.LoadRunIntakeSpecification(ctx, h.artifacts, binding.Intake)
	if err != nil {
		return orchestration.HandlerResult{}, h.fail(ctx, job.RunID, ids.CommandID, "invalid trusted specification intake", nil)
	}
	created := h.now().UTC()
	request, err := runtime.BuildSpecificationReasoningRequest(runtime.SpecificationReasoningInput{
		RequestID: ids.InvocationID, RunID: job.RunID, Attempt: job.Attempt,
		ManifestDigest: h.config.SpecificationManifestDigest, CreatedAt: created,
		ExpiresAt: created.Add(h.config.ReasoningTimeout), Intake: binding.Intake,
		Specification: intake, Budget: h.config.SpecificationBudget,
	})
	if err != nil {
		return orchestration.HandlerResult{}, h.fail(ctx, job.RunID, ids.CommandID, "invalid specification reasoning request", nil)
	}
	outcome, err := h.specifications.ProposeSpecification(ctx, request)
	if err != nil {
		return orchestration.HandlerResult{}, err
	}
	if outcome.Rejection != nil {
		return orchestration.HandlerResult{}, h.fail(ctx, job.RunID, ids.CommandID, "specification proposal rejected", outcome.Rejection)
	}
	ref := workflow.ArtifactRef{URI: outcome.ProposalArtifact.URI, Digest: outcome.ProposalArtifact.SHA256}
	if outcome.Proposal == nil || ref.Validate() != nil {
		return orchestration.HandlerResult{}, runtime.ErrConflict
	}
	run, err := h.workflow.GetRun(ctx, job.RunID)
	if err != nil {
		return orchestration.HandlerResult{}, err
	}
	_, err = h.workflow.ExecuteRun(ctx, workflow.ProposeSpecification{Meta: h.envelope(ids.CommandID,
		h.config.ServiceActorID, workflow.ActorWorkflowService, run.Revision, created), ID: job.RunID,
		Specification: ref})
	return orchestration.HandlerResult{Artifact: ref, Continue: false}, err
}

func (h *LivePlanningHandlers) plan( // skipcq: GO-R1005 -- explicit fail-closed planning evidence path
	ctx context.Context, job runtime.Job,
	ids orchestration.Identities) (orchestration.HandlerResult, error) {
	if job.TaskID != nil {
		return orchestration.HandlerResult{}, runtime.ErrConflict
	}
	binding, err := h.runtime.GetRun(ctx, job.RunID)
	if err != nil || binding.ApprovedSpecification == nil || binding.RepositoryMap == nil {
		if err == nil {
			err = runtime.ErrNotFound
		}
		return orchestration.HandlerResult{}, err
	}
	specBody, err := h.artifacts.Get(ctx, *binding.ApprovedSpecification)
	if err != nil {
		return orchestration.HandlerResult{}, err
	}
	var specification reasoningv1.SpecificationProposal
	if proto.Unmarshal(specBody, &specification) != nil {
		return orchestration.HandlerResult{}, h.fail(ctx, job.RunID, ids.CommandID, "invalid approved specification", nil)
	}
	mapBody, err := h.artifacts.Get(ctx, *binding.RepositoryMap)
	if err != nil {
		return orchestration.HandlerResult{}, err
	}
	snapshot, err := runtime.DecodeRepositorySnapshot(mapBody, binding.BaseCommit)
	if err != nil {
		return orchestration.HandlerResult{}, h.fail(ctx, job.RunID, ids.CommandID, "invalid repository map", nil)
	}
	created := h.now().UTC()
	request, err := runtime.BuildPlanningReasoningRequest(&runtime.PlanningReasoningInput{
		RequestID: ids.InvocationID, RunID: job.RunID, Attempt: job.Attempt,
		ManifestDigest: h.config.PlanningManifestDigest, CreatedAt: created,
		ExpiresAt: created.Add(h.config.ReasoningTimeout), ApprovedSpecification: *binding.ApprovedSpecification,
		Specification: &specification, RepositoryMap: *binding.RepositoryMap, Snapshot: snapshot,
		Policy: h.config.Policy, Budget: h.config.PlanningBudget,
	})
	if err != nil {
		return orchestration.HandlerResult{}, h.fail(ctx, job.RunID, ids.CommandID, "invalid planning reasoning request", nil)
	}
	outcome, err := h.planning.ProposeTaskGraph(ctx, request)
	if err != nil {
		return orchestration.HandlerResult{}, err
	}
	if outcome.Rejection != nil {
		return orchestration.HandlerResult{}, h.fail(ctx, job.RunID, ids.CommandID, "task planning proposal rejected", outcome.Rejection)
	}
	ref := workflow.ArtifactRef{URI: outcome.ProposalArtifact.URI, Digest: outcome.ProposalArtifact.SHA256}
	if outcome.Proposal == nil || ref.Validate() != nil {
		return orchestration.HandlerResult{}, runtime.ErrConflict
	}
	run, err := h.workflow.GetRun(ctx, job.RunID)
	if err != nil {
		return orchestration.HandlerResult{}, err
	}
	_, err = h.workflow.ExecuteRun(ctx, workflow.ProposeTaskGraph{Meta: h.envelope(ids.CommandID,
		h.config.ServiceActorID, workflow.ActorWorkflowService, run.Revision, created), ID: job.RunID,
		TaskGraph: ref})
	return orchestration.HandlerResult{Artifact: ref, Continue: false}, err
}

func (h *LivePlanningHandlers) fail(ctx context.Context, runID, commandID, reason string,
	rejection *reasoningv1.ProposalRejection) error {
	body, err := proto.MarshalOptions{Deterministic: true}.Marshal(rejection)
	if err != nil {
		return err
	}
	ref, err := h.artifacts.Put(ctx, body)
	if err != nil {
		return err
	}
	run, err := h.workflow.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	_, err = h.workflow.ExecuteRun(ctx, workflow.FailRun{Meta: h.envelope(commandID,
		h.config.ServiceActorID, workflow.ActorWorkflowService, run.Revision, h.now()),
		ID: runID, Evidence: ref, Reason: reason})
	return err
}

func (*LivePlanningHandlers) envelope(commandID, actorID string, kind workflow.ActorKind,
	revision uint64, at time.Time) workflow.CommandEnvelope {
	return workflow.CommandEnvelope{CommandID: commandID, Actor: workflow.Actor{ID: actorID, Kind: kind},
		ExpectedRevision: revision, Timestamp: at.UTC(), CorrelationID: commandID, CausationID: commandID}
}
