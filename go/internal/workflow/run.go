package workflow

import (
	"fmt"
	"time"
)

type Run struct {
	ID              string       `json:"id"`
	State           RunState     `json:"state"`
	Revision        uint64       `json:"revision"`
	Specification   *ArtifactRef `json:"specification,omitempty"`
	TaskGraph       *ArtifactRef `json:"task_graph,omitempty"`
	Execution       *ArtifactRef `json:"execution,omitempty"`
	CandidateCommit string       `json:"candidate_commit,omitempty"`
	Verification    *ArtifactRef `json:"verification,omitempty"`
	Review          *ArtifactRef `json:"review,omitempty"`
	Approval        *ArtifactRef `json:"approval,omitempty"`
	Publication     *ArtifactRef `json:"publication,omitempty"`
	Merge           *ArtifactRef `json:"merge,omitempty"`
	CreatedAt       time.Time    `json:"created_at"`
	UpdatedAt       time.Time    `json:"updated_at"`
}

func (r Run) Validate() error {
	if err := validateID("run", r.ID); err != nil {
		return err
	}
	if err := r.State.Validate(); err != nil {
		return err
	}
	if r.Revision == 0 || r.CreatedAt.IsZero() || r.UpdatedAt.IsZero() {
		return fmt.Errorf("%w: run snapshot", ErrInvalid)
	}
	if r.Specification != nil {
		if err := r.Specification.Validate(); err != nil {
			return err
		}
	}
	if r.TaskGraph != nil {
		if err := r.TaskGraph.Validate(); err != nil {
			return err
		}
	}
	for _, binding := range []*ArtifactRef{
		r.Execution, r.Verification, r.Review, r.Approval, r.Publication, r.Merge,
	} {
		if binding != nil {
			if err := binding.Validate(); err != nil {
				return err
			}
		}
	}
	if r.CandidateCommit != "" && !commitPattern.MatchString(r.CandidateCommit) {
		return fmt.Errorf("%w: run candidate commit", ErrInvalid)
	}
	return nil
}

type RunCommand interface {
	runCommand()
	Envelope() CommandEnvelope
	RunID() string
}

type CreateRun struct {
	Meta CommandEnvelope `json:"meta"`
	ID   string          `json:"run_id"`
}

func (CreateRun) runCommand()                 {}
func (c CreateRun) Envelope() CommandEnvelope { return c.Meta }
func (c CreateRun) RunID() string             { return c.ID }

type ProposeSpecification struct {
	Meta          CommandEnvelope `json:"meta"`
	ID            string          `json:"run_id"`
	Specification ArtifactRef     `json:"specification"`
}

func (ProposeSpecification) runCommand()                 {}
func (c ProposeSpecification) Envelope() CommandEnvelope { return c.Meta }
func (c ProposeSpecification) RunID() string             { return c.ID }

type RejectSpecification struct {
	Meta          CommandEnvelope `json:"meta"`
	ID            string          `json:"run_id"`
	Specification ArtifactRef     `json:"specification"`
	Reason        string          `json:"reason"`
	Terminal      bool            `json:"terminal"`
}

func (RejectSpecification) runCommand()                 {}
func (c RejectSpecification) Envelope() CommandEnvelope { return c.Meta }
func (c RejectSpecification) RunID() string             { return c.ID }

type ApproveSpecification struct {
	Meta          CommandEnvelope `json:"meta"`
	ID            string          `json:"run_id"`
	Specification ArtifactRef     `json:"specification"`
}

func (ApproveSpecification) runCommand()                 {}
func (c ApproveSpecification) Envelope() CommandEnvelope { return c.Meta }
func (c ApproveSpecification) RunID() string             { return c.ID }

type ProposeTaskGraph struct {
	Meta      CommandEnvelope `json:"meta"`
	ID        string          `json:"run_id"`
	TaskGraph ArtifactRef     `json:"task_graph"`
}

func (ProposeTaskGraph) runCommand()                 {}
func (c ProposeTaskGraph) Envelope() CommandEnvelope { return c.Meta }
func (c ProposeTaskGraph) RunID() string             { return c.ID }

type RejectTaskGraph struct {
	Meta      CommandEnvelope `json:"meta"`
	ID        string          `json:"run_id"`
	TaskGraph ArtifactRef     `json:"task_graph"`
	Reason    string          `json:"reason"`
	Terminal  bool            `json:"terminal"`
}

func (RejectTaskGraph) runCommand()                 {}
func (c RejectTaskGraph) Envelope() CommandEnvelope { return c.Meta }
func (c RejectTaskGraph) RunID() string             { return c.ID }

type ApproveTaskGraph struct {
	Meta         CommandEnvelope  `json:"meta"`
	ID           string           `json:"run_id"`
	TaskGraph    ArtifactRef      `json:"task_graph"`
	Tasks        []TaskDefinition `json:"tasks"`
	Dependencies []TaskDependency `json:"dependencies"`
}

func (ApproveTaskGraph) runCommand()                 {}
func (c ApproveTaskGraph) Envelope() CommandEnvelope { return c.Meta }
func (c ApproveTaskGraph) RunID() string             { return c.ID }

type Event struct {
	ID            string         `json:"id"`
	AggregateType string         `json:"aggregate_type"`
	AggregateID   string         `json:"aggregate_id"`
	Revision      uint64         `json:"revision"`
	Type          string         `json:"type"`
	Timestamp     time.Time      `json:"timestamp"`
	Actor         Actor          `json:"actor"`
	CorrelationID string         `json:"correlation_id"`
	CausationID   string         `json:"causation_id"`
	Payload       map[string]any `json:"payload"`
}

func NewRun(command CreateRun) (Run, []Event, error) {
	if err := validateRunCommand(command); err != nil {
		return Run{}, nil, err
	}
	if command.Meta.ExpectedRevision != 0 {
		return Run{}, nil, ErrRevisionConflict
	}
	if command.Meta.Actor.Kind != ActorHuman &&
		command.Meta.Actor.Kind != ActorWorkflowService {
		return Run{}, nil, ErrUnauthorized
	}
	next := Run{
		ID: command.ID, State: RunStateDraft, Revision: 1,
		CreatedAt: command.Meta.Timestamp.UTC(), UpdatedAt: command.Meta.Timestamp.UTC(),
	}
	return next, []Event{newRunEvent(next, command.Meta, "RUN_CREATED", nil)}, nil
}

func (r Run) Apply(command RunCommand) (Run, []Event, error) {
	if err := r.validateCommand(command); err != nil {
		return Run{}, nil, err
	}
	var transition runTransition
	var err error
	switch command := command.(type) {
	case ProposeSpecification:
		transition, err = r.proposeSpecification(command)
	case RejectSpecification:
		transition, err = r.rejectSpecification(command)
	case ApproveSpecification:
		transition, err = r.approveSpecification(command)
	case ProposeTaskGraph:
		transition, err = r.proposeTaskGraph(command)
	case RejectTaskGraph:
		transition, err = r.rejectTaskGraph(command)
	case ApproveTaskGraph:
		return Run{}, nil, fmt.Errorf("%w: task graph approval requires planning apply", ErrInvalid)
	case StartRun, RecordRunExecution, RecordRunVerification, RecordRunReview,
		ApproveRun, RejectRun, RecordDraftPullRequest, RecordMerge, FailRun, CancelRun:
		return r.applyCompletion(command)
	default:
		return Run{}, nil, fmt.Errorf("%w: unsupported run command", ErrInvalid)
	}
	if err != nil {
		return Run{}, nil, err
	}
	return finishRunTransition(transition, command.Envelope())
}

type runTransition struct {
	next      Run
	eventType string
	payload   map[string]any
}

func (r Run) validateCommand(command RunCommand) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if err := validateRunCommand(command); err != nil {
		return err
	}
	if command.RunID() != r.ID {
		return fmt.Errorf("%w: command run mismatch", ErrInvalid)
	}
	if command.Envelope().ExpectedRevision != r.Revision {
		return ErrRevisionConflict
	}
	if r.State.Terminal() {
		return ErrInvalidTransition
	}
	return nil
}

func (r Run) proposeSpecification(command ProposeSpecification) (runTransition, error) {
	if r.State != RunStateDraft {
		return runTransition{}, ErrInvalidTransition
	}
	if command.Meta.Actor.Kind != ActorWorkflowService {
		return runTransition{}, ErrUnauthorized
	}
	if err := command.Specification.Validate(); err != nil {
		return runTransition{}, err
	}
	next := r
	specification := command.Specification
	next.Specification = &specification
	next.State = RunStateSpecificationReview
	return runTransition{
		next: next, eventType: "SPECIFICATION_PROPOSED",
		payload: artifactPayload("specification", specification),
	}, nil
}

func (r Run) rejectSpecification(command RejectSpecification) (runTransition, error) {
	if err := r.validateSpecificationReview(command.Meta.Actor, command.Specification); err != nil {
		return runTransition{}, err
	}
	if err := validateReason(command.Reason); err != nil {
		return runTransition{}, err
	}
	next := r
	next.State = RunStateDraft
	if command.Terminal {
		next.State = RunStateRejected
	}
	return runTransition{
		next: next, eventType: "SPECIFICATION_REJECTED",
		payload: map[string]any{
			"specification_uri":    command.Specification.URI,
			"specification_digest": command.Specification.Digest,
			"reason":               command.Reason,
		},
	}, nil
}

func (r Run) approveSpecification(command ApproveSpecification) (runTransition, error) {
	if err := r.validateSpecificationReview(command.Meta.Actor, command.Specification); err != nil {
		return runTransition{}, err
	}
	next := r
	next.State = RunStateTaskPlanning
	return runTransition{
		next: next, eventType: "SPECIFICATION_APPROVED",
		payload: artifactPayload("specification", command.Specification),
	}, nil
}

func (r Run) proposeTaskGraph(command ProposeTaskGraph) (runTransition, error) {
	if r.State != RunStateTaskPlanning {
		return runTransition{}, ErrInvalidTransition
	}
	if command.Meta.Actor.Kind != ActorWorkflowService {
		return runTransition{}, ErrUnauthorized
	}
	if err := command.TaskGraph.Validate(); err != nil {
		return runTransition{}, err
	}
	next := r
	taskGraph := command.TaskGraph
	next.TaskGraph = &taskGraph
	next.State = RunStateTaskPlanReview
	return runTransition{
		next: next, eventType: "TASK_GRAPH_PROPOSED",
		payload: artifactPayload("task_graph", taskGraph),
	}, nil
}

func (r Run) rejectTaskGraph(command RejectTaskGraph) (runTransition, error) {
	if r.State != RunStateTaskPlanReview {
		return runTransition{}, ErrInvalidTransition
	}
	if command.Meta.Actor.Kind != ActorHuman {
		return runTransition{}, ErrUnauthorized
	}
	if err := validateBoundArtifact(r.TaskGraph, command.TaskGraph, "task graph"); err != nil {
		return runTransition{}, err
	}
	if err := validateReason(command.Reason); err != nil {
		return runTransition{}, err
	}
	next := r
	next.State = RunStateTaskPlanning
	if command.Terminal {
		next.State = RunStateRejected
	}
	payload := artifactPayload("task_graph", command.TaskGraph)
	payload["reason"] = command.Reason
	return runTransition{
		next: next, eventType: "TASK_GRAPH_REJECTED", payload: payload,
	}, nil
}

func (r Run) validateSpecificationReview(actor Actor, specification ArtifactRef) error {
	if r.State != RunStateSpecificationReview {
		return ErrInvalidTransition
	}
	if actor.Kind != ActorHuman {
		return ErrUnauthorized
	}
	if err := specification.Validate(); err != nil {
		return err
	}
	if r.Specification == nil || !r.Specification.Equal(specification) {
		return fmt.Errorf("%w: specification binding", ErrInvalid)
	}
	return nil
}

func finishRunTransition(transition runTransition, meta CommandEnvelope) (Run, []Event, error) {
	transition.next.Revision++
	transition.next.UpdatedAt = meta.Timestamp.UTC()
	return transition.next, []Event{newRunEvent(
		transition.next, meta, transition.eventType, transition.payload,
	)}, nil
}

type PlanningDecision struct {
	Run          Run
	Tasks        []Task
	Dependencies []TaskDependency
	Events       []Event
}

func (r Run) ApproveTaskGraph(command ApproveTaskGraph) (PlanningDecision, error) {
	if err := r.Validate(); err != nil {
		return PlanningDecision{}, err
	}
	if err := validateRunCommand(command); err != nil {
		return PlanningDecision{}, err
	}
	if command.ID != r.ID {
		return PlanningDecision{}, fmt.Errorf("%w: command run mismatch", ErrInvalid)
	}
	if command.Meta.ExpectedRevision != r.Revision {
		return PlanningDecision{}, ErrRevisionConflict
	}
	if r.State != RunStateTaskPlanReview {
		return PlanningDecision{}, ErrInvalidTransition
	}
	if command.Meta.Actor.Kind != ActorHuman {
		return PlanningDecision{}, ErrUnauthorized
	}
	if err := validateBoundArtifact(r.TaskGraph, command.TaskGraph, "task graph"); err != nil {
		return PlanningDecision{}, err
	}
	tasks, dependencies, taskEvents, err := NewPlannedTasks(
		r.ID, command.Tasks, command.Dependencies, command.Meta,
	)
	if err != nil {
		return PlanningDecision{}, err
	}
	next := r
	next.State = RunStateReady
	next.Revision++
	next.UpdatedAt = command.Meta.Timestamp.UTC()
	runEvent := newRunEvent(
		next, command.Meta, "TASK_GRAPH_APPROVED", artifactPayload("task_graph", command.TaskGraph),
	)
	events := append([]Event{runEvent}, taskEvents...)
	return PlanningDecision{
		Run: next, Tasks: tasks, Dependencies: dependencies, Events: events,
	}, nil
}

func validateRunCommand(command RunCommand) error {
	if command == nil {
		return fmt.Errorf("%w: nil command", ErrInvalid)
	}
	if err := command.Envelope().Validate(); err != nil {
		return err
	}
	return validateID("run", command.RunID())
}

func newRunEvent(run Run, meta CommandEnvelope, eventType string, payload map[string]any) Event {
	if payload == nil {
		payload = map[string]any{}
	}
	return Event{
		ID: meta.CommandID, AggregateType: "RUN", AggregateID: run.ID,
		Revision: run.Revision, Type: eventType, Timestamp: meta.Timestamp.UTC(),
		Actor: meta.Actor, CorrelationID: meta.CorrelationID,
		CausationID: meta.CausationID, Payload: payload,
	}
}

func artifactPayload(prefix string, artifact ArtifactRef) map[string]any {
	return map[string]any{
		prefix + "_uri": artifact.URI, prefix + "_digest": artifact.Digest,
	}
}

func validateBoundArtifact(bound *ArtifactRef, supplied ArtifactRef, label string) error {
	if err := supplied.Validate(); err != nil {
		return err
	}
	if bound == nil || !bound.Equal(supplied) {
		return fmt.Errorf("%w: %s binding", ErrInvalid, label)
	}
	return nil
}
