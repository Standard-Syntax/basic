package workflow

import (
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"
)

var commitPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)

type LeaseRef struct {
	ID        string    `json:"id"`
	OwnerID   string    `json:"owner_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (l LeaseRef) Validate() error {
	if err := validateID("lease", l.ID); err != nil {
		return err
	}
	if err := validateID("lease owner", l.OwnerID); err != nil {
		return err
	}
	if l.ExpiresAt.IsZero() {
		return fmt.Errorf("%w: lease expiry", ErrInvalid)
	}
	return nil
}

func (l LeaseRef) Equal(other LeaseRef) bool {
	return l.ID == other.ID && l.OwnerID == other.OwnerID && l.ExpiresAt.Equal(other.ExpiresAt)
}

type TaskDefinition struct {
	ID          string `json:"task_id"`
	MaxAttempts uint32 `json:"max_attempts"`
}

type TaskDependency struct {
	TaskID      string `json:"task_id"`
	DependsOnID string `json:"depends_on_id"`
}

type Task struct {
	ID              string       `json:"id"`
	RunID           string       `json:"run_id"`
	State           TaskState    `json:"state"`
	Revision        uint64       `json:"revision"`
	MaxAttempts     uint32       `json:"max_attempts"`
	CurrentAttempt  uint32       `json:"current_attempt"`
	Lease           *LeaseRef    `json:"lease,omitempty"`
	Proposal        *ArtifactRef `json:"proposal,omitempty"`
	Execution       *ArtifactRef `json:"execution,omitempty"`
	CandidateCommit string       `json:"candidate_commit,omitempty"`
	Verification    *ArtifactRef `json:"verification,omitempty"`
	Review          *ArtifactRef `json:"review,omitempty"`
	Approval        *ArtifactRef `json:"approval,omitempty"`
	CreatedAt       time.Time    `json:"created_at"`
	UpdatedAt       time.Time    `json:"updated_at"`
}

func (t Task) Validate() error {
	if err := t.validateSnapshot(); err != nil {
		return err
	}
	return t.validateBindings()
}

func (t Task) validateSnapshot() error {
	if err := validateID("task", t.ID); err != nil {
		return err
	}
	if err := validateID("run", t.RunID); err != nil {
		return err
	}
	if err := t.State.Validate(); err != nil {
		return err
	}
	if t.Revision == 0 || t.MaxAttempts == 0 || t.CurrentAttempt > t.MaxAttempts ||
		t.CreatedAt.IsZero() || t.UpdatedAt.IsZero() {
		return fmt.Errorf("%w: task snapshot", ErrInvalid)
	}
	return nil
}

func (t Task) validateBindings() error {
	if t.Lease != nil {
		if err := t.Lease.Validate(); err != nil {
			return err
		}
	}
	for _, binding := range []*ArtifactRef{
		t.Proposal, t.Execution, t.Verification, t.Review, t.Approval,
	} {
		if binding != nil {
			if err := binding.Validate(); err != nil {
				return err
			}
		}
	}
	if t.CandidateCommit != "" && !commitPattern.MatchString(t.CandidateCommit) {
		return fmt.Errorf("%w: candidate commit", ErrInvalid)
	}
	return nil
}

func NewPlannedTasks(
	runID string,
	definitions []TaskDefinition,
	dependencies []TaskDependency,
	meta CommandEnvelope,
) ([]Task, []TaskDependency, []Event, error) {
	if err := validateID("run", runID); err != nil {
		return nil, nil, nil, err
	}
	taskIDs, err := plannedTaskIDs(definitions)
	if err != nil {
		return nil, nil, nil, err
	}
	incoming, adjacency, err := plannedTaskGraph(taskIDs, dependencies)
	if err != nil {
		return nil, nil, nil, err
	}
	if !taskGraphAcyclic(taskIDs, incoming, adjacency) {
		return nil, nil, nil, fmt.Errorf("%w: cyclic task graph", ErrInvalid)
	}
	tasks, events := buildPlannedTasks(runID, definitions, incoming, meta)
	return tasks, append([]TaskDependency(nil), dependencies...), events, nil
}

func plannedTaskIDs(definitions []TaskDefinition) (map[string]struct{}, error) {
	if len(definitions) == 0 {
		return nil, fmt.Errorf("%w: empty task graph", ErrInvalid)
	}
	taskIDs := make(map[string]struct{}, len(definitions))
	for _, definition := range definitions {
		if err := validateID("task", definition.ID); err != nil {
			return nil, err
		}
		if definition.MaxAttempts == 0 {
			return nil, fmt.Errorf("%w: max attempts", ErrInvalid)
		}
		if _, exists := taskIDs[definition.ID]; exists {
			return nil, fmt.Errorf("%w: duplicate task", ErrInvalid)
		}
		taskIDs[definition.ID] = struct{}{}
	}
	return taskIDs, nil
}

func plannedTaskGraph(
	taskIDs map[string]struct{}, dependencies []TaskDependency,
) (map[string]int, map[string][]string, error) {
	incoming := make(map[string]int, len(taskIDs))
	adjacency := make(map[string][]string, len(taskIDs))
	edges := make(map[string]struct{}, len(dependencies))
	for _, dependency := range dependencies {
		if _, exists := taskIDs[dependency.TaskID]; !exists {
			return nil, nil, fmt.Errorf("%w: dependency task", ErrInvalid)
		}
		if _, exists := taskIDs[dependency.DependsOnID]; !exists {
			return nil, nil, fmt.Errorf("%w: dependency target", ErrInvalid)
		}
		if dependency.TaskID == dependency.DependsOnID {
			return nil, nil, fmt.Errorf("%w: self dependency", ErrInvalid)
		}
		key := dependency.TaskID + ":" + dependency.DependsOnID
		if _, exists := edges[key]; exists {
			return nil, nil, fmt.Errorf("%w: duplicate dependency", ErrInvalid)
		}
		edges[key] = struct{}{}
		incoming[dependency.TaskID]++
		adjacency[dependency.DependsOnID] = append(adjacency[dependency.DependsOnID], dependency.TaskID)
	}
	return incoming, adjacency, nil
}

func taskGraphAcyclic(
	taskIDs map[string]struct{}, incoming map[string]int, adjacency map[string][]string,
) bool {
	queue := make([]string, 0, len(taskIDs))
	degrees := make(map[string]int, len(incoming))
	for id := range taskIDs {
		degrees[id] = incoming[id]
		if incoming[id] == 0 {
			queue = append(queue, id)
		}
	}
	visited := 0
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		visited++
		for _, dependent := range adjacency[id] {
			degrees[dependent]--
			if degrees[dependent] == 0 {
				queue = append(queue, dependent)
			}
		}
	}
	return visited == len(taskIDs)
}

func buildPlannedTasks(
	runID string, definitions []TaskDefinition, incoming map[string]int, meta CommandEnvelope,
) ([]Task, []Event) {
	tasks := make([]Task, 0, len(definitions))
	events := make([]Event, 0, len(definitions))
	for _, definition := range definitions {
		state := TaskStatePending
		eventType := "TASK_CREATED"
		if incoming[definition.ID] == 0 {
			state = TaskStateReady
			eventType = "TASK_READY"
		}
		task := Task{
			ID: definition.ID, RunID: runID, State: state, Revision: 1,
			MaxAttempts: definition.MaxAttempts, CreatedAt: meta.Timestamp.UTC(),
			UpdatedAt: meta.Timestamp.UTC(),
		}
		tasks = append(tasks, task)
		eventID := uuid.NewSHA1(
			uuid.MustParse(meta.CommandID),
			[]byte(definition.ID+":"+eventType),
		).String()
		events = append(events, Event{
			ID: eventID, AggregateType: "TASK", AggregateID: task.ID,
			Revision: 1, Type: eventType, Timestamp: meta.Timestamp.UTC(),
			Actor: meta.Actor, CorrelationID: meta.CorrelationID,
			CausationID: meta.CausationID,
			Payload:     map[string]any{"run_id": runID, "max_attempts": definition.MaxAttempts},
		})
	}
	return tasks, events
}

type TaskCommand interface {
	taskCommand()
	Envelope() CommandEnvelope
	TaskID() string
	RunID() string
}

type LeaseTask struct {
	Meta  CommandEnvelope `json:"meta"`
	Run   string          `json:"run_id"`
	ID    string          `json:"task_id"`
	Lease LeaseRef        `json:"lease"`
}

func (LeaseTask) taskCommand()                {}
func (c LeaseTask) Envelope() CommandEnvelope { return c.Meta }
func (c LeaseTask) TaskID() string            { return c.ID }
func (c LeaseTask) RunID() string             { return c.Run }

type ReleaseTaskLease struct {
	Meta  CommandEnvelope `json:"meta"`
	Run   string          `json:"run_id"`
	ID    string          `json:"task_id"`
	Lease LeaseRef        `json:"lease"`
}

func (ReleaseTaskLease) taskCommand()                {}
func (c ReleaseTaskLease) Envelope() CommandEnvelope { return c.Meta }
func (c ReleaseTaskLease) TaskID() string            { return c.ID }
func (c ReleaseTaskLease) RunID() string             { return c.Run }

type StartReasoning struct {
	Meta  CommandEnvelope `json:"meta"`
	Run   string          `json:"run_id"`
	ID    string          `json:"task_id"`
	Lease LeaseRef        `json:"lease"`
}

func (StartReasoning) taskCommand()                {}
func (c StartReasoning) Envelope() CommandEnvelope { return c.Meta }
func (c StartReasoning) TaskID() string            { return c.ID }
func (c StartReasoning) RunID() string             { return c.Run }

type RejectTaskProposal struct {
	Meta     CommandEnvelope `json:"meta"`
	Run      string          `json:"run_id"`
	ID       string          `json:"task_id"`
	Proposal ArtifactRef     `json:"proposal"`
	Reason   string          `json:"reason"`
}

func (RejectTaskProposal) taskCommand()                {}
func (c RejectTaskProposal) Envelope() CommandEnvelope { return c.Meta }
func (c RejectTaskProposal) TaskID() string            { return c.ID }
func (c RejectTaskProposal) RunID() string             { return c.Run }

type AcceptTaskProposal struct {
	Meta     CommandEnvelope `json:"meta"`
	Run      string          `json:"run_id"`
	ID       string          `json:"task_id"`
	Proposal ArtifactRef     `json:"proposal"`
}

func (AcceptTaskProposal) taskCommand()                {}
func (c AcceptTaskProposal) Envelope() CommandEnvelope { return c.Meta }
func (c AcceptTaskProposal) TaskID() string            { return c.ID }
func (c AcceptTaskProposal) RunID() string             { return c.Run }

type RecordTaskExecution struct {
	Meta            CommandEnvelope `json:"meta"`
	Run             string          `json:"run_id"`
	ID              string          `json:"task_id"`
	Proposal        ArtifactRef     `json:"proposal"`
	Execution       ArtifactRef     `json:"execution"`
	CandidateCommit string          `json:"candidate_commit"`
}

func (RecordTaskExecution) taskCommand()                {}
func (c RecordTaskExecution) Envelope() CommandEnvelope { return c.Meta }
func (c RecordTaskExecution) TaskID() string            { return c.ID }
func (c RecordTaskExecution) RunID() string             { return c.Run }

type RecordTaskVerification struct {
	Meta            CommandEnvelope `json:"meta"`
	Run             string          `json:"run_id"`
	ID              string          `json:"task_id"`
	CandidateCommit string          `json:"candidate_commit"`
	Evidence        ArtifactRef     `json:"evidence"`
	Passed          bool            `json:"passed"`
}

func (RecordTaskVerification) taskCommand()                {}
func (c RecordTaskVerification) Envelope() CommandEnvelope { return c.Meta }
func (c RecordTaskVerification) TaskID() string            { return c.ID }
func (c RecordTaskVerification) RunID() string             { return c.Run }

type RecordTaskReview struct {
	Meta            CommandEnvelope `json:"meta"`
	Run             string          `json:"run_id"`
	ID              string          `json:"task_id"`
	CandidateCommit string          `json:"candidate_commit"`
	Review          ArtifactRef     `json:"review"`
	Passed          bool            `json:"passed"`
}

func (RecordTaskReview) taskCommand()                {}
func (c RecordTaskReview) Envelope() CommandEnvelope { return c.Meta }
func (c RecordTaskReview) TaskID() string            { return c.ID }
func (c RecordTaskReview) RunID() string             { return c.Run }

type ApproveTask struct {
	Meta            CommandEnvelope `json:"meta"`
	Run             string          `json:"run_id"`
	ID              string          `json:"task_id"`
	CandidateCommit string          `json:"candidate_commit"`
	Review          ArtifactRef     `json:"review"`
	Approval        ArtifactRef     `json:"approval"`
}

func (ApproveTask) taskCommand()                {}
func (c ApproveTask) Envelope() CommandEnvelope { return c.Meta }
func (c ApproveTask) TaskID() string            { return c.ID }
func (c ApproveTask) RunID() string             { return c.Run }

type RequireTaskRework struct {
	Meta            CommandEnvelope `json:"meta"`
	Run             string          `json:"run_id"`
	ID              string          `json:"task_id"`
	CandidateCommit string          `json:"candidate_commit"`
	Review          ArtifactRef     `json:"review"`
	Reason          string          `json:"reason"`
}

func (RequireTaskRework) taskCommand()                {}
func (c RequireTaskRework) Envelope() CommandEnvelope { return c.Meta }
func (c RequireTaskRework) TaskID() string            { return c.ID }
func (c RequireTaskRework) RunID() string             { return c.Run }

type RetryTask struct {
	Meta CommandEnvelope `json:"meta"`
	Run  string          `json:"run_id"`
	ID   string          `json:"task_id"`
}

func (RetryTask) taskCommand()                {}
func (c RetryTask) Envelope() CommandEnvelope { return c.Meta }
func (c RetryTask) TaskID() string            { return c.ID }
func (c RetryTask) RunID() string             { return c.Run }

type FailTask struct {
	Meta     CommandEnvelope `json:"meta"`
	Run      string          `json:"run_id"`
	ID       string          `json:"task_id"`
	Evidence ArtifactRef     `json:"evidence"`
	Reason   string          `json:"reason"`
}

func (FailTask) taskCommand()                {}
func (c FailTask) Envelope() CommandEnvelope { return c.Meta }
func (c FailTask) TaskID() string            { return c.ID }
func (c FailTask) RunID() string             { return c.Run }

type CancelTask struct {
	Meta   CommandEnvelope `json:"meta"`
	Run    string          `json:"run_id"`
	ID     string          `json:"task_id"`
	Reason string          `json:"reason"`
}

func (CancelTask) taskCommand()                {}
func (c CancelTask) Envelope() CommandEnvelope { return c.Meta }
func (c CancelTask) TaskID() string            { return c.ID }
func (c CancelTask) RunID() string             { return c.Run }

func (t Task) Apply(command TaskCommand) (Task, []Event, error) {
	if err := t.validateCommand(command); err != nil {
		return Task{}, nil, err
	}
	transition, err := t.applyCommand(command)
	if err != nil {
		return Task{}, nil, err
	}
	return finishTaskTransition(transition, command.Envelope())
}

func (t Task) applyCommand(command TaskCommand) (taskTransition, error) {
	switch command := command.(type) {
	case LeaseTask:
		return t.lease(command)
	case ReleaseTaskLease:
		return t.releaseLease(command)
	case StartReasoning:
		return t.startReasoning(command)
	case RejectTaskProposal:
		return t.rejectProposal(command)
	case AcceptTaskProposal:
		return t.acceptProposal(command)
	case RecordTaskExecution:
		return t.recordExecution(command)
	case RecordTaskVerification:
		return t.recordVerification(command)
	case RecordTaskReview:
		return t.recordReview(command)
	case ApproveTask:
		return t.approve(command)
	case RequireTaskRework:
		return t.requireRework(command)
	case RetryTask:
		return t.retry(command)
	case FailTask:
		return t.fail(command)
	case CancelTask:
		return t.cancel(command)
	default:
		return taskTransition{}, fmt.Errorf("%w: unsupported task command", ErrInvalid)
	}
}

type taskTransition struct {
	next      Task
	eventType string
	payload   map[string]any
}

func (t Task) validateCommand(command TaskCommand) error {
	if err := t.Validate(); err != nil {
		return err
	}
	if err := validateTaskCommand(command); err != nil {
		return err
	}
	if command.TaskID() != t.ID || command.RunID() != t.RunID {
		return fmt.Errorf("%w: task identity mismatch", ErrInvalid)
	}
	if command.Envelope().ExpectedRevision != t.Revision {
		return ErrRevisionConflict
	}
	if t.State.Terminal() {
		return ErrInvalidTransition
	}
	return nil
}

func finishTaskTransition(transition taskTransition, meta CommandEnvelope) (Task, []Event, error) {
	transition.next.Revision++
	transition.next.UpdatedAt = meta.Timestamp.UTC()
	event := Event{
		ID: meta.CommandID, AggregateType: "TASK", AggregateID: transition.next.ID,
		Revision: transition.next.Revision, Type: transition.eventType, Timestamp: meta.Timestamp.UTC(),
		Actor: meta.Actor, CorrelationID: meta.CorrelationID,
		CausationID: meta.CausationID, Payload: transition.payload,
	}
	return transition.next, []Event{event}, nil
}

func (t Task) lease(command LeaseTask) (taskTransition, error) {
	if t.State != TaskStateReady || t.CurrentAttempt >= t.MaxAttempts {
		return taskTransition{}, ErrInvalidTransition
	}
	if command.Meta.Actor.Kind != ActorWorkflowService {
		return taskTransition{}, ErrUnauthorized
	}
	if err := command.Lease.Validate(); err != nil {
		return taskTransition{}, err
	}
	next := t
	lease := command.Lease
	next.Lease = &lease
	next.CurrentAttempt++
	next.State = TaskStateLeased
	return taskTransition{
		next: next, eventType: "TASK_LEASED",
		payload: map[string]any{"lease_id": lease.ID},
	}, nil
}

func (t Task) releaseLease(command ReleaseTaskLease) (taskTransition, error) {
	if t.State != TaskStateLeased {
		return taskTransition{}, ErrInvalidTransition
	}
	if command.Meta.Actor.Kind != ActorWorkflowService {
		return taskTransition{}, ErrUnauthorized
	}
	if err := validateLease(t.Lease, command.Lease); err != nil {
		return taskTransition{}, err
	}
	next := t
	next.Lease = nil
	next.State = TaskStateReady
	return taskTransition{next: next, eventType: "TASK_LEASE_RELEASED", payload: map[string]any{}}, nil
}

func (t Task) startReasoning(command StartReasoning) (taskTransition, error) {
	if t.State != TaskStateLeased {
		return taskTransition{}, ErrInvalidTransition
	}
	if command.Meta.Actor.Kind != ActorReasoningService {
		return taskTransition{}, ErrUnauthorized
	}
	if err := validateLease(t.Lease, command.Lease); err != nil {
		return taskTransition{}, err
	}
	next := t
	next.State = TaskStateReasoning
	return taskTransition{next: next, eventType: "TASK_REASONING_STARTED", payload: map[string]any{}}, nil
}

func (t Task) rejectProposal(command RejectTaskProposal) (taskTransition, error) {
	if t.State != TaskStateReasoning {
		return taskTransition{}, ErrInvalidTransition
	}
	if command.Meta.Actor.Kind != ActorHuman {
		return taskTransition{}, ErrUnauthorized
	}
	if err := command.Proposal.Validate(); err != nil {
		return taskTransition{}, err
	}
	if err := validateReason(command.Reason); err != nil {
		return taskTransition{}, err
	}
	next := t
	proposal := command.Proposal
	next.Proposal = &proposal
	next.State = TaskStateProposalRejected
	return taskTransition{next: next, eventType: "TASK_PROPOSAL_REJECTED", payload: map[string]any{}}, nil
}

func (t Task) acceptProposal(command AcceptTaskProposal) (taskTransition, error) {
	if t.State != TaskStateReasoning {
		return taskTransition{}, ErrInvalidTransition
	}
	if command.Meta.Actor.Kind != ActorExecutionService {
		return taskTransition{}, ErrUnauthorized
	}
	if err := command.Proposal.Validate(); err != nil {
		return taskTransition{}, err
	}
	next := t
	proposal := command.Proposal
	next.Proposal = &proposal
	next.State = TaskStateExecuting
	return taskTransition{next: next, eventType: "TASK_EXECUTION_STARTED", payload: map[string]any{}}, nil
}

func (t Task) recordExecution(command RecordTaskExecution) (taskTransition, error) {
	if t.State != TaskStateExecuting {
		return taskTransition{}, ErrInvalidTransition
	}
	if command.Meta.Actor.Kind != ActorExecutionService {
		return taskTransition{}, ErrUnauthorized
	}
	if err := validateBoundArtifact(t.Proposal, command.Proposal, "proposal"); err != nil {
		return taskTransition{}, err
	}
	if err := command.Execution.Validate(); err != nil ||
		!commitPattern.MatchString(command.CandidateCommit) {
		return taskTransition{}, fmt.Errorf("%w: execution binding", ErrInvalid)
	}
	next := t
	execution := command.Execution
	next.Execution = &execution
	next.CandidateCommit = command.CandidateCommit
	next.State = TaskStateVerifying
	return taskTransition{next: next, eventType: "TASK_EXECUTED", payload: map[string]any{}}, nil
}

func (t Task) recordVerification(command RecordTaskVerification) (taskTransition, error) {
	if t.State != TaskStateVerifying {
		return taskTransition{}, ErrInvalidTransition
	}
	if command.Meta.Actor.Kind != ActorVerificationService {
		return taskTransition{}, ErrUnauthorized
	}
	if command.CandidateCommit != t.CandidateCommit {
		return taskTransition{}, fmt.Errorf("%w: candidate commit", ErrInvalid)
	}
	if err := command.Evidence.Validate(); err != nil {
		return taskTransition{}, err
	}
	next := t
	evidence := command.Evidence
	next.Verification = &evidence
	eventType := "TASK_VERIFICATION_FAILED"
	next.State = TaskStateReworkRequired
	if command.Passed {
		next.State, eventType = TaskStateReviewing, "TASK_VERIFIED"
	}
	return taskTransition{next: next, eventType: eventType, payload: map[string]any{}}, nil
}

func (t Task) recordReview(command RecordTaskReview) (taskTransition, error) {
	if t.State != TaskStateReviewing {
		return taskTransition{}, ErrInvalidTransition
	}
	if command.Meta.Actor.Kind != ActorReviewService {
		return taskTransition{}, ErrUnauthorized
	}
	if command.CandidateCommit != t.CandidateCommit {
		return taskTransition{}, fmt.Errorf("%w: candidate commit", ErrInvalid)
	}
	if err := command.Review.Validate(); err != nil {
		return taskTransition{}, err
	}
	next := t
	review := command.Review
	next.Review = &review
	eventType := "TASK_REVIEW_FAILED"
	next.State = TaskStateReworkRequired
	if command.Passed {
		next.State, eventType = TaskStateAwaitingApproval, "TASK_REVIEWED"
	}
	return taskTransition{next: next, eventType: eventType, payload: map[string]any{}}, nil
}

func (t Task) approve(command ApproveTask) (taskTransition, error) {
	if err := t.validateHumanReview(command.Meta.Actor, command.CandidateCommit, command.Review); err != nil {
		return taskTransition{}, err
	}
	if err := command.Approval.Validate(); err != nil {
		return taskTransition{}, err
	}
	next := t
	approval := command.Approval
	next.Approval = &approval
	next.State = TaskStateAccepted
	return taskTransition{next: next, eventType: "TASK_ACCEPTED", payload: map[string]any{}}, nil
}

func (t Task) requireRework(command RequireTaskRework) (taskTransition, error) {
	if err := t.validateHumanReview(command.Meta.Actor, command.CandidateCommit, command.Review); err != nil {
		return taskTransition{}, err
	}
	if err := validateReason(command.Reason); err != nil {
		return taskTransition{}, err
	}
	next := t
	next.State = TaskStateReworkRequired
	return taskTransition{next: next, eventType: "TASK_REWORK_REQUIRED", payload: map[string]any{}}, nil
}

func (t Task) validateHumanReview(actor Actor, candidateCommit string, review ArtifactRef) error {
	if t.State != TaskStateAwaitingApproval {
		return ErrInvalidTransition
	}
	if actor.Kind != ActorHuman {
		return ErrUnauthorized
	}
	if candidateCommit != t.CandidateCommit {
		return fmt.Errorf("%w: candidate commit", ErrInvalid)
	}
	return validateBoundArtifact(t.Review, review, "review")
}

func (t Task) retry(command RetryTask) (taskTransition, error) {
	if t.State != TaskStateProposalRejected && t.State != TaskStateReworkRequired {
		return taskTransition{}, ErrInvalidTransition
	}
	if command.Meta.Actor.Kind != ActorWorkflowService {
		return taskTransition{}, ErrUnauthorized
	}
	next := t
	next.Lease, next.Proposal, next.Execution = nil, nil, nil
	next.Verification, next.Review, next.Approval = nil, nil, nil
	next.CandidateCommit = ""
	eventType := "TASK_RETRY_READY"
	next.State = TaskStateReady
	if t.CurrentAttempt >= t.MaxAttempts {
		next.State, eventType = TaskStateFailed, "TASK_ATTEMPTS_EXHAUSTED"
	}
	return taskTransition{next: next, eventType: eventType, payload: map[string]any{}}, nil
}

func (t Task) fail(command FailTask) (taskTransition, error) {
	if !trustedOperationalActor(command.Meta.Actor.Kind) {
		return taskTransition{}, ErrUnauthorized
	}
	if err := command.Evidence.Validate(); err != nil {
		return taskTransition{}, err
	}
	if err := validateReason(command.Reason); err != nil {
		return taskTransition{}, err
	}
	next := t
	next.State = TaskStateFailed
	return taskTransition{next: next, eventType: "TASK_FAILED", payload: map[string]any{}}, nil
}

func (t Task) cancel(command CancelTask) (taskTransition, error) {
	if command.Meta.Actor.Kind != ActorWorkflowService {
		return taskTransition{}, ErrUnauthorized
	}
	if err := validateReason(command.Reason); err != nil {
		return taskTransition{}, err
	}
	next := t
	next.State = TaskStateCancelled
	return taskTransition{next: next, eventType: "TASK_CANCELLED", payload: map[string]any{}}, nil
}

func validateTaskCommand(command TaskCommand) error {
	if command == nil {
		return fmt.Errorf("%w: nil command", ErrInvalid)
	}
	if err := command.Envelope().Validate(); err != nil {
		return err
	}
	if err := validateID("run", command.RunID()); err != nil {
		return err
	}
	return validateID("task", command.TaskID())
}

func validateLease(bound *LeaseRef, supplied LeaseRef) error {
	if err := supplied.Validate(); err != nil {
		return err
	}
	if bound == nil || !bound.Equal(supplied) {
		return fmt.Errorf("%w: lease binding", ErrInvalid)
	}
	return nil
}

func trustedOperationalActor(kind ActorKind) bool {
	switch kind {
	case ActorWorkflowService, ActorReasoningService, ActorExecutionService,
		ActorVerificationService, ActorReviewService:
		return true
	default:
		return false
	}
}
