package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	reasoningv1 "github.com/Standard-Syntax/basic/go/gen/harness/reasoning/v1"
	"github.com/Standard-Syntax/basic/go/internal/beta"
	"github.com/Standard-Syntax/basic/go/internal/workflow"
)

const RunIntakeSpecificationSchema = "run_intake_specification.v1"

type RunIntakeSpecification struct {
	SchemaVersion     string   `json:"schema_version"`
	ProblemStatement  string   `json:"problem_statement"`
	DesiredOutcome    string   `json:"desired_outcome"`
	KnownConstraints  []string `json:"known_constraints"`
	KnownNonGoals     []string `json:"known_non_goals"`
	Stakeholders      []string `json:"stakeholders"`
	RepositorySummary *string  `json:"repository_summary,omitempty"`
}

func DecodeRunIntakeSpecification(body []byte) (RunIntakeSpecification, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var value RunIntakeSpecification
	if err := decoder.Decode(&value); err != nil {
		return RunIntakeSpecification{}, ErrConflict
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return RunIntakeSpecification{}, ErrConflict
	}
	if value.SchemaVersion != RunIntakeSpecificationSchema ||
		!boundedPlanningText(value.ProblemStatement, 4096) ||
		!boundedPlanningText(value.DesiredOutcome, 4096) || len(value.Stakeholders) == 0 ||
		!boundedPlanningList(value.KnownConstraints, 100, 2048) ||
		!boundedPlanningList(value.KnownNonGoals, 100, 2048) ||
		!boundedPlanningList(value.Stakeholders, 100, 256) ||
		(value.RepositorySummary != nil && !boundedPlanningText(*value.RepositorySummary, 4096)) {
		return RunIntakeSpecification{}, ErrConflict
	}
	return value, nil
}

func boundedPlanningList(values []string, maximumItems, maximumLength int) bool {
	if len(values) > maximumItems {
		return false
	}
	for _, value := range values {
		if !boundedPlanningText(value, maximumLength) {
			return false
		}
	}
	return true
}

func boundedPlanningText(value string, maximum int) bool {
	return strings.TrimSpace(value) != "" && len(value) <= maximum
}

type SpecificationReasoningInput struct {
	RequestID, RunID, ManifestDigest string
	Attempt                          uint32
	CreatedAt, ExpiresAt             time.Time
	Intake                           workflow.ArtifactRef
	Specification                    RunIntakeSpecification
	Budget                           ReasoningLimits
}

func BuildSpecificationReasoningRequest(
	input SpecificationReasoningInput,
) (*reasoningv1.SpecificationRequest, error) {
	if input.RunID == "" || input.RequestID == "" || input.ManifestDigest == "" ||
		input.Attempt == 0 || !input.ExpiresAt.After(input.CreatedAt) {
		return nil, ErrConflict
	}
	if err := input.Intake.Validate(); err != nil {
		return nil, err
	}
	envelope := requestEnvelope(input.RequestID, input.RunID, "", input.Attempt,
		reasoningv1.ReasoningStage_REASONING_STAGE_SPECIFICATION, input.CreatedAt,
		input.ExpiresAt, input.ManifestDigest, input.Budget,
		[]*reasoningv1.ArtifactDigest{artifactDigest(input.Intake)})
	envelope.TaskId = nil
	return &reasoningv1.SpecificationRequest{Envelope: envelope,
		ProblemStatement:  input.Specification.ProblemStatement,
		DesiredOutcome:    input.Specification.DesiredOutcome,
		KnownConstraints:  input.Specification.KnownConstraints,
		KnownNonGoals:     input.Specification.KnownNonGoals,
		Stakeholders:      input.Specification.Stakeholders,
		RepositorySummary: input.Specification.RepositorySummary}, nil
}

type PlanningReasoningInput struct {
	RequestID, RunID, ManifestDigest string
	Attempt                          uint32
	CreatedAt, ExpiresAt             time.Time
	ApprovedSpecification            workflow.ArtifactRef
	Specification                    *reasoningv1.SpecificationProposal
	RepositoryMap                    workflow.ArtifactRef
	Snapshot                         RepositorySnapshot
	Policy                           beta.Policy
	Budget                           ReasoningLimits
}

func BuildPlanningReasoningRequest(
	input PlanningReasoningInput,
) (*reasoningv1.TaskPlanningRequest, error) {
	if input.Specification == nil || len(input.Specification.GetAcceptanceCriteria()) == 0 ||
		input.RunID == "" || input.RequestID == "" || input.ManifestDigest == "" ||
		input.Attempt == 0 || !input.ExpiresAt.After(input.CreatedAt) ||
		input.Policy.Limits.MaximumTasks != 1 {
		return nil, ErrConflict
	}
	if err := input.ApprovedSpecification.Validate(); err != nil {
		return nil, err
	}
	if err := input.RepositoryMap.Validate(); err != nil {
		return nil, err
	}
	if err := input.Snapshot.Validate(input.Policy.Repository.BaseCommit); err != nil {
		return nil, err
	}
	criteria := make([]string, 0, len(input.Specification.GetAcceptanceCriteria()))
	for _, criterion := range input.Specification.GetAcceptanceCriteria() {
		if criterion.GetCriterionId() == "" {
			return nil, ErrConflict
		}
		criteria = append(criteria, criterion.GetCriterionId())
	}
	envelope := requestEnvelope(input.RequestID, input.RunID, "", input.Attempt,
		reasoningv1.ReasoningStage_REASONING_STAGE_PLANNING, input.CreatedAt,
		input.ExpiresAt, input.ManifestDigest, input.Budget,
		[]*reasoningv1.ArtifactDigest{artifactDigest(input.ApprovedSpecification),
			artifactDigest(input.RepositoryMap)})
	envelope.TaskId = nil
	return &reasoningv1.TaskPlanningRequest{Envelope: envelope,
		ApprovedSpecificationId:     input.ApprovedSpecification.Digest,
		ApprovedSpecificationDigest: input.ApprovedSpecification.Digest,
		RepositoryMap:               input.Snapshot.Entries,
		ReadablePaths:               input.Policy.Paths.Readable, WritablePaths: input.Policy.Paths.Writable,
		ProhibitedPaths: input.Policy.Paths.Prohibited, TaskCountLimit: 1, ParallelismLimit: 1,
		AcceptanceCriterionIds: criteria}, nil
}

func LoadRunIntakeSpecification(
	ctx context.Context, store interface {
		Get(context.Context, workflow.ArtifactRef) ([]byte, error)
	}, ref workflow.ArtifactRef,
) (RunIntakeSpecification, error) {
	body, err := store.Get(ctx, ref)
	if err != nil {
		return RunIntakeSpecification{}, err
	}
	if Digest(body) != ref.Digest {
		return RunIntakeSpecification{}, ErrConflict
	}
	return DecodeRunIntakeSpecification(body)
}
