package workflow

import (
	"fmt"
	"time"
)

type Run struct {
	ID            string       `json:"id"`
	State         RunState     `json:"state"`
	Revision      uint64       `json:"revision"`
	Specification *ArtifactRef `json:"specification,omitempty"`
	CreatedAt     time.Time    `json:"created_at"`
	UpdatedAt     time.Time    `json:"updated_at"`
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
		return r.Specification.Validate()
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
