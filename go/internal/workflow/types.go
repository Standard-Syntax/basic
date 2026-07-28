package workflow

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

type RunState string

const (
	RunStateDraft               RunState = "DRAFT"
	RunStateSpecificationReview RunState = "SPECIFICATION_REVIEW"
	RunStateTaskPlanning        RunState = "TASK_PLANNING"
	RunStateTaskPlanReview      RunState = "TASK_PLAN_REVIEW"
	RunStateReady               RunState = "READY"
	RunStateExecuting           RunState = "EXECUTING"
	RunStateVerifying           RunState = "VERIFYING"
	RunStateReviewing           RunState = "REVIEWING"
	RunStateAwaitingApproval    RunState = "AWAITING_APPROVAL"
	RunStateMergeReady          RunState = "MERGE_READY"
	RunStateMerged              RunState = "MERGED"
	RunStateRejected            RunState = "REJECTED"
	RunStateFailed              RunState = "FAILED"
	RunStateCancelled           RunState = "CANCELLED"
)

func (s RunState) Validate() error {
	switch s {
	case RunStateDraft, RunStateSpecificationReview, RunStateTaskPlanning,
		RunStateTaskPlanReview, RunStateReady, RunStateExecuting,
		RunStateVerifying, RunStateReviewing, RunStateAwaitingApproval,
		RunStateMergeReady, RunStateMerged, RunStateRejected, RunStateFailed,
		RunStateCancelled:
		return nil
	default:
		return fmt.Errorf("%w: run state %q", ErrInvalid, s)
	}
}

func (s RunState) Terminal() bool {
	return s == RunStateMerged || s == RunStateRejected ||
		s == RunStateFailed || s == RunStateCancelled
}

type TaskState string

const (
	TaskStatePending          TaskState = "PENDING"
	TaskStateReady            TaskState = "READY"
	TaskStateLeased           TaskState = "LEASED"
	TaskStateReasoning        TaskState = "REASONING"
	TaskStateProposalRejected TaskState = "PROPOSAL_REJECTED"
	TaskStateExecuting        TaskState = "EXECUTING"
	TaskStateVerifying        TaskState = "VERIFYING"
	TaskStateReviewing        TaskState = "REVIEWING"
	TaskStateReworkRequired   TaskState = "REWORK_REQUIRED"
	TaskStateAwaitingApproval TaskState = "AWAITING_APPROVAL"
	TaskStateAccepted         TaskState = "ACCEPTED"
	TaskStateFailed           TaskState = "FAILED"
	TaskStateCancelled        TaskState = "CANCELLED"
)

func (s TaskState) Validate() error {
	switch s {
	case TaskStatePending, TaskStateReady, TaskStateLeased, TaskStateReasoning,
		TaskStateProposalRejected, TaskStateExecuting, TaskStateVerifying,
		TaskStateReviewing, TaskStateReworkRequired, TaskStateAwaitingApproval,
		TaskStateAccepted, TaskStateFailed, TaskStateCancelled:
		return nil
	default:
		return fmt.Errorf("%w: task state %q", ErrInvalid, s)
	}
}

func (s TaskState) Terminal() bool {
	return s == TaskStateAccepted || s == TaskStateFailed || s == TaskStateCancelled
}

type ActorKind string

const (
	ActorHuman               ActorKind = "HUMAN"
	ActorWorkflowService     ActorKind = "WORKFLOW_SERVICE"
	ActorReasoningService    ActorKind = "REASONING_SERVICE"
	ActorExecutionService    ActorKind = "EXECUTION_SERVICE"
	ActorVerificationService ActorKind = "VERIFICATION_SERVICE"
	ActorReviewService       ActorKind = "REVIEW_SERVICE"
	ActorMergeService        ActorKind = "MERGE_SERVICE"
	ActorPython              ActorKind = "PYTHON"
	ActorModel               ActorKind = "MODEL"
)

type Actor struct {
	ID   string    `json:"id"`
	Kind ActorKind `json:"kind"`
}

func (a Actor) Validate() error {
	if err := validateID("actor", a.ID); err != nil {
		return err
	}
	switch a.Kind {
	case ActorHuman, ActorWorkflowService, ActorReasoningService,
		ActorExecutionService, ActorVerificationService, ActorReviewService,
		ActorMergeService, ActorPython, ActorModel:
		return nil
	default:
		return fmt.Errorf("%w: actor kind %q", ErrInvalid, a.Kind)
	}
}

type ArtifactRef struct {
	URI    string `json:"uri"`
	Digest string `json:"digest"`
}

func (a ArtifactRef) Validate() error {
	parsed, err := url.ParseRequestURI(a.URI)
	if err != nil || !parsed.IsAbs() {
		return fmt.Errorf("%w: artifact URI", ErrInvalid)
	}
	if !digestPattern.MatchString(a.Digest) {
		return fmt.Errorf("%w: artifact digest", ErrInvalid)
	}
	return nil
}

func (a ArtifactRef) Equal(other ArtifactRef) bool {
	return a.URI == other.URI && a.Digest == other.Digest
}

type CommandEnvelope struct {
	CommandID        string    `json:"command_id"`
	Actor            Actor     `json:"actor"`
	ExpectedRevision uint64    `json:"expected_revision"`
	Timestamp        time.Time `json:"timestamp"`
	CorrelationID    string    `json:"correlation_id"`
	CausationID      string    `json:"causation_id"`
}

func (e CommandEnvelope) Validate() error {
	for label, value := range map[string]string{
		"command": e.CommandID, "correlation": e.CorrelationID,
		"causation": e.CausationID,
	} {
		if err := validateID(label, value); err != nil {
			return err
		}
	}
	if err := e.Actor.Validate(); err != nil {
		return err
	}
	if e.Timestamp.IsZero() {
		return fmt.Errorf("%w: command timestamp", ErrInvalid)
	}
	return nil
}

func validateID(label, value string) error {
	if _, err := uuid.Parse(value); err != nil {
		return fmt.Errorf("%w: %s ID", ErrInvalid, label)
	}
	return nil
}

func validateReason(reason string) error {
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("%w: reason", ErrInvalid)
	}
	return nil
}

var (
	ErrInvalid           = errors.New("invalid workflow value")
	ErrInvalidTransition = errors.New("invalid transition")
	ErrUnauthorized      = errors.New("unauthorized actor")
	ErrRevisionConflict  = errors.New("revision conflict")
	ErrCommandConflict   = errors.New("command ID reused with different content")
	ErrNotFound          = errors.New("aggregate not found")
)
