package workflow

import "fmt"

type StartRun struct {
	Meta CommandEnvelope `json:"meta"`
	ID   string          `json:"run_id"`
}

func (StartRun) runCommand()                 {}
func (c StartRun) Envelope() CommandEnvelope { return c.Meta }
func (c StartRun) RunID() string             { return c.ID }

type RecordRunExecution struct {
	Meta            CommandEnvelope `json:"meta"`
	ID              string          `json:"run_id"`
	Execution       ArtifactRef     `json:"execution"`
	CandidateCommit string          `json:"candidate_commit"`
}

func (RecordRunExecution) runCommand()                 {}
func (c RecordRunExecution) Envelope() CommandEnvelope { return c.Meta }
func (c RecordRunExecution) RunID() string             { return c.ID }

type RecordRunVerification struct {
	Meta            CommandEnvelope `json:"meta"`
	ID              string          `json:"run_id"`
	CandidateCommit string          `json:"candidate_commit"`
	Evidence        ArtifactRef     `json:"evidence"`
}

func (RecordRunVerification) runCommand()                 {}
func (c RecordRunVerification) Envelope() CommandEnvelope { return c.Meta }
func (c RecordRunVerification) RunID() string             { return c.ID }

type RecordRunReview struct {
	Meta            CommandEnvelope `json:"meta"`
	ID              string          `json:"run_id"`
	CandidateCommit string          `json:"candidate_commit"`
	Review          ArtifactRef     `json:"review"`
}

func (RecordRunReview) runCommand()                 {}
func (c RecordRunReview) Envelope() CommandEnvelope { return c.Meta }
func (c RecordRunReview) RunID() string             { return c.ID }

type ApproveRun struct {
	Meta            CommandEnvelope `json:"meta"`
	ID              string          `json:"run_id"`
	CandidateCommit string          `json:"candidate_commit"`
	Review          ArtifactRef     `json:"review"`
	Approval        ArtifactRef     `json:"approval"`
}

func (ApproveRun) runCommand()                 {}
func (c ApproveRun) Envelope() CommandEnvelope { return c.Meta }
func (c ApproveRun) RunID() string             { return c.ID }

type RejectRun struct {
	Meta            CommandEnvelope `json:"meta"`
	ID              string          `json:"run_id"`
	CandidateCommit string          `json:"candidate_commit"`
	Review          ArtifactRef     `json:"review"`
	Reason          string          `json:"reason"`
}

func (RejectRun) runCommand()                 {}
func (c RejectRun) Envelope() CommandEnvelope { return c.Meta }
func (c RejectRun) RunID() string             { return c.ID }

type RecordMerge struct {
	Meta            CommandEnvelope `json:"meta"`
	ID              string          `json:"run_id"`
	CandidateCommit string          `json:"candidate_commit"`
	Approval        ArtifactRef     `json:"approval"`
	Merge           ArtifactRef     `json:"merge"`
}

func (RecordMerge) runCommand()                 {}
func (c RecordMerge) Envelope() CommandEnvelope { return c.Meta }
func (c RecordMerge) RunID() string             { return c.ID }

type FailRun struct {
	Meta     CommandEnvelope `json:"meta"`
	ID       string          `json:"run_id"`
	Evidence ArtifactRef     `json:"evidence"`
	Reason   string          `json:"reason"`
}

func (FailRun) runCommand()                 {}
func (c FailRun) Envelope() CommandEnvelope { return c.Meta }
func (c FailRun) RunID() string             { return c.ID }

type CancelRun struct {
	Meta   CommandEnvelope `json:"meta"`
	ID     string          `json:"run_id"`
	Reason string          `json:"reason"`
}

func (CancelRun) runCommand()                 {}
func (c CancelRun) Envelope() CommandEnvelope { return c.Meta }
func (c CancelRun) RunID() string             { return c.ID }

func (r Run) applyCompletion(command RunCommand) (Run, []Event, error) {
	var transition runTransition
	var err error
	switch command := command.(type) {
	case StartRun:
		transition, err = r.start(command)
	case RecordRunExecution:
		transition, err = r.recordExecution(command)
	case RecordRunVerification:
		transition, err = r.recordVerification(command)
	case RecordRunReview:
		transition, err = r.recordReview(command)
	case ApproveRun:
		transition, err = r.approve(command)
	case RejectRun:
		transition, err = r.reject(command)
	case RecordMerge:
		transition, err = r.recordMerge(command)
	case FailRun:
		transition, err = r.fail(command)
	case CancelRun:
		transition, err = r.cancel(command)
	default:
		return Run{}, nil, fmt.Errorf("%w: unsupported completion command", ErrInvalid)
	}
	if err != nil {
		return Run{}, nil, err
	}
	return finishRunTransition(transition, command.Envelope())
}

func (r Run) start(command StartRun) (runTransition, error) {
	if r.State != RunStateReady {
		return runTransition{}, ErrInvalidTransition
	}
	if command.Meta.Actor.Kind != ActorWorkflowService {
		return runTransition{}, ErrUnauthorized
	}
	next := r
	next.State = RunStateExecuting
	return runTransition{next: next, eventType: "RUN_EXECUTION_STARTED"}, nil
}

func (r Run) recordExecution(command RecordRunExecution) (runTransition, error) {
	if r.State != RunStateExecuting {
		return runTransition{}, ErrInvalidTransition
	}
	if command.Meta.Actor.Kind != ActorExecutionService {
		return runTransition{}, ErrUnauthorized
	}
	if err := command.Execution.Validate(); err != nil ||
		!commitPattern.MatchString(command.CandidateCommit) {
		return runTransition{}, fmt.Errorf("%w: run execution binding", ErrInvalid)
	}
	next := r
	execution := command.Execution
	next.Execution, next.CandidateCommit = &execution, command.CandidateCommit
	next.State = RunStateVerifying
	return runTransition{next: next, eventType: "RUN_EXECUTED"}, nil
}

func (r Run) recordVerification(command RecordRunVerification) (runTransition, error) {
	if r.State != RunStateVerifying {
		return runTransition{}, ErrInvalidTransition
	}
	if command.Meta.Actor.Kind != ActorVerificationService {
		return runTransition{}, ErrUnauthorized
	}
	if command.CandidateCommit != r.CandidateCommit {
		return runTransition{}, fmt.Errorf("%w: run candidate commit", ErrInvalid)
	}
	if err := command.Evidence.Validate(); err != nil {
		return runTransition{}, err
	}
	next := r
	evidence := command.Evidence
	next.Verification, next.State = &evidence, RunStateReviewing
	return runTransition{next: next, eventType: "RUN_VERIFIED"}, nil
}

func (r Run) recordReview(command RecordRunReview) (runTransition, error) {
	if r.State != RunStateReviewing {
		return runTransition{}, ErrInvalidTransition
	}
	if command.Meta.Actor.Kind != ActorReviewService {
		return runTransition{}, ErrUnauthorized
	}
	if command.CandidateCommit != r.CandidateCommit {
		return runTransition{}, fmt.Errorf("%w: run candidate commit", ErrInvalid)
	}
	if err := command.Review.Validate(); err != nil {
		return runTransition{}, err
	}
	next := r
	review := command.Review
	next.Review, next.State = &review, RunStateAwaitingApproval
	return runTransition{next: next, eventType: "RUN_REVIEWED"}, nil
}

func (r Run) approve(command ApproveRun) (runTransition, error) {
	if err := r.validateHumanReview(command.Meta.Actor, command.CandidateCommit, command.Review); err != nil {
		return runTransition{}, err
	}
	if err := command.Approval.Validate(); err != nil {
		return runTransition{}, err
	}
	next := r
	approval := command.Approval
	next.Approval, next.State = &approval, RunStateMergeReady
	return runTransition{next: next, eventType: "RUN_APPROVED"}, nil
}

func (r Run) reject(command RejectRun) (runTransition, error) {
	if err := r.validateHumanReview(command.Meta.Actor, command.CandidateCommit, command.Review); err != nil {
		return runTransition{}, err
	}
	if err := validateReason(command.Reason); err != nil {
		return runTransition{}, err
	}
	next := r
	next.State = RunStateRejected
	return runTransition{next: next, eventType: "RUN_REJECTED"}, nil
}

func (r Run) validateHumanReview(actor Actor, candidateCommit string, review ArtifactRef) error {
	if r.State != RunStateAwaitingApproval {
		return ErrInvalidTransition
	}
	if actor.Kind != ActorHuman {
		return ErrUnauthorized
	}
	if candidateCommit != r.CandidateCommit {
		return fmt.Errorf("%w: run candidate commit", ErrInvalid)
	}
	return validateBoundArtifact(r.Review, review, "run review")
}

func (r Run) recordMerge(command RecordMerge) (runTransition, error) {
	if r.State != RunStateMergeReady {
		return runTransition{}, ErrInvalidTransition
	}
	if command.Meta.Actor.Kind != ActorMergeService {
		return runTransition{}, ErrUnauthorized
	}
	if command.CandidateCommit != r.CandidateCommit {
		return runTransition{}, fmt.Errorf("%w: run candidate commit", ErrInvalid)
	}
	if err := validateBoundArtifact(r.Approval, command.Approval, "run approval"); err != nil {
		return runTransition{}, err
	}
	if err := command.Merge.Validate(); err != nil {
		return runTransition{}, err
	}
	next := r
	merge := command.Merge
	next.Merge, next.State = &merge, RunStateMerged
	return runTransition{next: next, eventType: "RUN_MERGED"}, nil
}

func (r Run) fail(command FailRun) (runTransition, error) {
	if r.State.Terminal() {
		return runTransition{}, ErrInvalidTransition
	}
	if !trustedOperationalActor(command.Meta.Actor.Kind) {
		return runTransition{}, ErrUnauthorized
	}
	if err := command.Evidence.Validate(); err != nil {
		return runTransition{}, err
	}
	if err := validateReason(command.Reason); err != nil {
		return runTransition{}, err
	}
	next := r
	next.State = RunStateFailed
	return runTransition{next: next, eventType: "RUN_FAILED"}, nil
}

func (r Run) cancel(command CancelRun) (runTransition, error) {
	if r.State.Terminal() {
		return runTransition{}, ErrInvalidTransition
	}
	if command.Meta.Actor.Kind != ActorHuman {
		return runTransition{}, ErrUnauthorized
	}
	if err := validateReason(command.Reason); err != nil {
		return runTransition{}, err
	}
	next := r
	next.State = RunStateCancelled
	return runTransition{next: next, eventType: "RUN_CANCELLED"}, nil
}
