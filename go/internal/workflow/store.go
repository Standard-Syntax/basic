package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	postgresutil "github.com/Standard-Syntax/basic/go/internal/postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	pool   *pgxpool.Pool
	inject func(FaultPoint) error
}

type FaultPoint string

var errReplayAfterConflict = errors.New("replay command after reservation conflict")

const (
	FaultBeforeEvents   FaultPoint = "before_events"
	FaultBeforeSnapshot FaultPoint = "before_snapshot"
	FaultBeforeCommit   FaultPoint = "before_commit"
)

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) fault(point FaultPoint) error {
	if s.inject != nil {
		return s.inject(point)
	}
	return nil
}

type CommandResult struct {
	AggregateID string   `json:"aggregate_id"`
	State       string   `json:"state"`
	Revision    uint64   `json:"revision"`
	EventIDs    []string `json:"event_ids"`
	TaskIDs     []string `json:"task_ids,omitempty"`
	Replay      bool     `json:"replay"`
}

func (s *Store) ExecuteRun(ctx context.Context, command RunCommand) (CommandResult, error) {
	if err := validateRunCommand(command); err != nil {
		return CommandResult{}, err
	}
	digest, err := commandDigest(command)
	if err != nil {
		return CommandResult{}, err
	}
	return postgresutil.RetryTransaction(ctx, func() (CommandResult, error) {
		tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
		if err != nil {
			return CommandResult{}, fmt.Errorf("begin workflow transaction: %w", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		result, err := s.executeRunTransaction(ctx, tx, command, digest)
		if errors.Is(err, errReplayAfterConflict) {
			_ = tx.Rollback(ctx)
			return s.replayAfterConflict(ctx, command.Envelope().CommandID, digest)
		}
		if err != nil {
			return CommandResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return CommandResult{}, fmt.Errorf("commit workflow transaction: %w", err)
		}
		return result, nil
	})
}

// ExecuteRunTx persists a run command inside a caller-owned transaction. The
// caller is responsible for using a serializable transaction and committing or
// rolling it back. This seam exists for application boundaries that must bind a
// workflow transition atomically with records owned by other packages.
func (s *Store) ExecuteRunTx(
	ctx context.Context, tx pgx.Tx, command RunCommand,
) (CommandResult, error) {
	if tx == nil {
		return CommandResult{}, fmt.Errorf("%w: nil transaction", ErrInvalid)
	}
	if err := validateRunCommand(command); err != nil {
		return CommandResult{}, err
	}
	digest, err := commandDigest(command)
	if err != nil {
		return CommandResult{}, err
	}
	result, err := s.executeRunTransaction(ctx, tx, command, digest)
	if errors.Is(err, errReplayAfterConflict) {
		return CommandResult{}, ErrCommandConflict
	}
	return result, err
}

func commandDigest(command RunCommand) (string, error) {
	return typedCommandDigest(command, "command")
}

func typedCommandDigest(command any, label string) (string, error) {
	request, err := json.Marshal(command)
	if err != nil {
		return "", fmt.Errorf("marshal %s: %w", label, err)
	}
	typedRequest := append([]byte(fmt.Sprintf("%T\x00", command)), request...)
	sum := sha256.Sum256(typedRequest)
	return hex.EncodeToString(sum[:]), nil
}

func (s *Store) executeRunTransaction(
	ctx context.Context, tx pgx.Tx, command RunCommand, digest string,
) (CommandResult, error) {
	if result, found, err := loadCommandResult(ctx, tx, command.Envelope().CommandID, digest); err != nil {
		return CommandResult{}, err
	} else if found {
		result.Replay = true
		return result, nil
	}

	current, err := loadRunForCommand(ctx, tx, command)
	if err != nil {
		return CommandResult{}, err
	}
	if err := validateRunTaskGate(ctx, tx, command); err != nil {
		return CommandResult{}, err
	}
	decision, err := decideRunCommand(current, command)
	if err != nil {
		return CommandResult{}, err
	}
	if cancellation, ok := command.(CancelRun); ok {
		cancelEvents, err := cancelRunTasks(
			ctx, tx, decision.next.ID, cancellation.Meta, cancellation.Reason,
		)
		if err != nil {
			return CommandResult{}, err
		}
		decision.events = append(decision.events, cancelEvents...)
	}
	if err := s.persistRunTransition(ctx, tx, command, digest, decision); err != nil {
		return CommandResult{}, err
	}
	result, err := recordRunCommandResult(ctx, tx, command, decision)
	if err != nil {
		return CommandResult{}, err
	}
	if err := s.fault(FaultBeforeCommit); err != nil {
		return CommandResult{}, err
	}
	return result, nil
}

func loadRunForCommand(ctx context.Context, tx pgx.Tx, command RunCommand) (Run, error) {
	if _, create := command.(CreateRun); !create {
		run, _, err := loadRun(ctx, tx, command.RunID())
		return run, err
	}
	_, _, err := loadRun(ctx, tx, command.RunID())
	if err == nil {
		return Run{}, fmt.Errorf("%w: run already exists", ErrInvalidTransition)
	}
	if !errors.Is(err, ErrNotFound) {
		return Run{}, err
	}
	return Run{}, nil
}

type runPersistenceDecision struct {
	current      Run
	next         Run
	tasks        []Task
	dependencies []TaskDependency
	events       []Event
}

func decideRunCommand(current Run, command RunCommand) (runPersistenceDecision, error) {
	if create, ok := command.(CreateRun); ok {
		next, events, err := NewRun(create)
		return runPersistenceDecision{
			current: current, next: next, events: events,
		}, err
	}
	if approval, ok := command.(ApproveTaskGraph); ok {
		decision, err := current.ApproveTaskGraph(approval)
		return runPersistenceDecision{
			current: current, next: decision.Run, tasks: decision.Tasks,
			dependencies: decision.Dependencies, events: decision.Events,
		}, err
	}
	next, events, err := current.Apply(command)
	return runPersistenceDecision{
		current: current, next: next, events: events,
	}, err
}

func (s *Store) persistRunTransition(
	ctx context.Context,
	tx pgx.Tx,
	command RunCommand,
	digest string,
	decision runPersistenceDecision,
) error {
	if err := reserveCommand(ctx, tx, command, digest); err != nil {
		if isUniqueViolation(err) {
			return errReplayAfterConflict
		}
		return err
	}
	if err := s.fault(FaultBeforeEvents); err != nil {
		return err
	}
	for _, event := range decision.events {
		if err := insertEvent(ctx, tx, event, command.Envelope().CommandID); err != nil {
			return err
		}
	}
	for _, task := range decision.tasks {
		if err := insertTask(ctx, tx, task); err != nil {
			return err
		}
	}
	for _, dependency := range decision.dependencies {
		if _, err := tx.Exec(ctx, `INSERT INTO workflow_task_dependencies
			(run_id, task_id, depends_on_task_id) VALUES ($1,$2,$3)`,
			decision.next.ID, dependency.TaskID, dependency.DependsOnID); err != nil {
			return fmt.Errorf("insert task dependency: %w", err)
		}
	}
	if err := s.fault(FaultBeforeSnapshot); err != nil {
		return err
	}
	if _, ok := command.(CreateRun); ok {
		return insertRun(ctx, tx, decision.next)
	}
	return updateRun(ctx, tx, decision.next, decision.current.Revision)
}

func recordRunCommandResult(
	ctx context.Context, tx pgx.Tx, command RunCommand, decision runPersistenceDecision,
) (CommandResult, error) {
	result := CommandResult{
		AggregateID: decision.next.ID, State: string(decision.next.State),
		Revision: decision.next.Revision, EventIDs: eventIDs(decision.events),
		TaskIDs: taskIDs(decision.tasks),
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return CommandResult{}, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE workflow_commands SET result = $2, completed_at = now() WHERE command_id = $1`,
		command.Envelope().CommandID, encoded); err != nil {
		return CommandResult{}, fmt.Errorf("record command result: %w", err)
	}
	return result, nil
}

func (s *Store) ExecuteTask(ctx context.Context, command TaskCommand) (CommandResult, error) {
	if err := validateTaskCommand(command); err != nil {
		return CommandResult{}, err
	}
	digest, err := taskCommandDigest(command)
	if err != nil {
		return CommandResult{}, err
	}
	return postgresutil.RetryTransaction(ctx, func() (CommandResult, error) {
		tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
		if err != nil {
			return CommandResult{}, fmt.Errorf("begin workflow transaction: %w", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		result, err := s.executeTaskTransaction(ctx, tx, command, digest)
		if errors.Is(err, errReplayAfterConflict) {
			_ = tx.Rollback(ctx)
			return s.replayAfterConflict(ctx, command.Envelope().CommandID, digest)
		}
		if err != nil {
			return CommandResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return CommandResult{}, fmt.Errorf("commit task transaction: %w", err)
		}
		return result, nil
	})
}

func taskCommandDigest(command TaskCommand) (string, error) {
	return typedCommandDigest(command, "task command")
}

func (s *Store) executeTaskTransaction(
	ctx context.Context, tx pgx.Tx, command TaskCommand, digest string,
) (CommandResult, error) {
	if result, found, err := loadCommandResult(ctx, tx, command.Envelope().CommandID, digest); err != nil {
		return CommandResult{}, err
	} else if found {
		result.Replay = true
		return result, nil
	}
	if err := validateTaskRunGate(ctx, tx, command); err != nil {
		return CommandResult{}, err
	}
	current, err := loadTask(ctx, tx, command.RunID(), command.TaskID())
	if err != nil {
		return CommandResult{}, err
	}
	next, events, err := current.Apply(command)
	if err != nil {
		return CommandResult{}, err
	}
	events, err = s.persistTaskTransition(ctx, tx, command, digest, current, next, events)
	if err != nil {
		return CommandResult{}, err
	}
	result, err := recordTaskCommandResult(ctx, tx, command, next, events)
	if err != nil {
		return CommandResult{}, err
	}
	if err := s.fault(FaultBeforeCommit); err != nil {
		return CommandResult{}, err
	}
	return result, nil
}

func (s *Store) persistTaskTransition(
	ctx context.Context,
	tx pgx.Tx,
	command TaskCommand,
	digest string,
	current Task,
	next Task,
	events []Event,
) ([]Event, error) {
	if err := reserveTaskCommand(ctx, tx, command, digest); err != nil {
		if isUniqueViolation(err) {
			return nil, errReplayAfterConflict
		}
		return nil, err
	}
	if err := s.fault(FaultBeforeEvents); err != nil {
		return nil, err
	}
	for _, event := range events {
		if err := insertEvent(ctx, tx, event, command.Envelope().CommandID); err != nil {
			return nil, err
		}
	}
	if err := s.fault(FaultBeforeSnapshot); err != nil {
		return nil, err
	}
	if err := updateTask(ctx, tx, next, current.Revision); err != nil {
		return nil, err
	}
	if next.State == TaskStateAccepted {
		readyEvents, err := makeDependentsReady(ctx, tx, next, command.Envelope())
		if err != nil {
			return nil, err
		}
		for _, event := range readyEvents {
			if err := insertEvent(ctx, tx, event, command.Envelope().CommandID); err != nil {
				return nil, err
			}
		}
		events = append(events, readyEvents...)
	}
	return events, nil
}

func recordTaskCommandResult(
	ctx context.Context, tx pgx.Tx, command TaskCommand, next Task, events []Event,
) (CommandResult, error) {
	result := CommandResult{
		AggregateID: next.ID, State: string(next.State), Revision: next.Revision,
		EventIDs: eventIDs(events),
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return CommandResult{}, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE workflow_commands SET result=$2, completed_at=now() WHERE command_id=$1`,
		command.Envelope().CommandID, encoded); err != nil {
		return CommandResult{}, fmt.Errorf("record task command result: %w", err)
	}
	return result, nil
}

func (s *Store) replayAfterConflict(
	ctx context.Context, commandID, digest string,
) (CommandResult, error) {
	var storedDigest string
	var encoded []byte
	err := s.pool.QueryRow(ctx, `SELECT request_digest,result FROM workflow_commands
		WHERE command_id=$1`, commandID).Scan(&storedDigest, &encoded)
	if err != nil {
		return CommandResult{}, fmt.Errorf("load conflicting command: %w", err)
	}
	if storedDigest != digest || len(encoded) == 0 {
		return CommandResult{}, ErrCommandConflict
	}
	var result CommandResult
	if err := json.Unmarshal(encoded, &result); err != nil {
		return CommandResult{}, fmt.Errorf("decode conflicting command: %w", err)
	}
	result.Replay = true
	return result, nil
}

func validateTaskRunGate(ctx context.Context, tx pgx.Tx, command TaskCommand) error {
	var state RunState
	if err := tx.QueryRow(ctx,
		`SELECT state FROM workflow_runs WHERE run_id=$1 FOR UPDATE`,
		command.RunID()).Scan(&state); errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return fmt.Errorf("lock owning run: %w", err)
	}
	if _, cancellation := command.(CancelTask); cancellation {
		if state != RunStateCancelled {
			return ErrInvalidTransition
		}
		return nil
	}
	if state != RunStateExecuting {
		return ErrInvalidTransition
	}
	return nil
}

func reserveTaskCommand(ctx context.Context, tx pgx.Tx, command TaskCommand, digest string) error {
	meta := command.Envelope()
	_, err := tx.Exec(ctx, `INSERT INTO workflow_commands
		(command_id, aggregate_type, aggregate_id, request_digest, actor_id, actor_kind,
		 expected_revision, correlation_id, causation_id, requested_at)
		VALUES ($1,'TASK',$2,$3,$4,$5,$6,$7,$8,$9)`,
		meta.CommandID, command.TaskID(), digest, meta.Actor.ID, meta.Actor.Kind,
		meta.ExpectedRevision, meta.CorrelationID, meta.CausationID, meta.Timestamp.UTC())
	if err != nil {
		return fmt.Errorf("reserve task command: %w", err)
	}
	return nil
}

func loadTask(ctx context.Context, tx pgx.Tx, runID, taskID string) (Task, error) {
	var task Task
	var bindings taskRowBindings
	row := tx.QueryRow(ctx, `SELECT task_id,run_id,state,revision,max_attempts,current_attempt,
		lease_id::text,lease_owner_id::text,lease_expires_at,lease_fencing_token,
		proposal_uri,proposal_digest,
		execution_uri,execution_digest,candidate_commit,verification_uri,verification_digest,
		review_uri,review_digest,approval_uri,approval_digest,created_at,updated_at
		FROM workflow_tasks WHERE run_id=$1 AND task_id=$2 FOR UPDATE`,
		runID, taskID)
	err := row.Scan(
		&task.ID, &task.RunID, &task.State, &task.Revision, &task.MaxAttempts,
		&task.CurrentAttempt, &bindings.leaseID, &bindings.leaseOwner, &bindings.leaseExpiry,
		&bindings.leaseFence,
		&bindings.proposalURI, &bindings.proposalDigest,
		&bindings.executionURI, &bindings.executionDigest,
		&bindings.candidateCommit, &bindings.verificationURI, &bindings.verificationDigest,
		&bindings.reviewURI, &bindings.reviewDigest,
		&bindings.approvalURI, &bindings.approvalDigest,
		&task.CreatedAt, &task.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Task{}, ErrNotFound
	}
	if err != nil {
		return Task{}, fmt.Errorf("load task: %w", err)
	}
	if err := bindings.apply(&task); err != nil {
		return Task{}, err
	}
	if err := task.Validate(); err != nil {
		return Task{}, err
	}
	return task, nil
}

type taskRowBindings struct {
	leaseID, leaseOwner                 *string
	leaseExpiry                         *time.Time
	leaseFence                          *int64
	proposalURI, proposalDigest         *string
	executionURI, executionDigest       *string
	candidateCommit                     *string
	verificationURI, verificationDigest *string
	reviewURI, reviewDigest             *string
	approvalURI, approvalDigest         *string
}

func (b taskRowBindings) apply(task *Task) error {
	if err := b.applyLease(task); err != nil {
		return err
	}
	var err error
	if task.Proposal, err = optionalArtifact(b.proposalURI, b.proposalDigest); err != nil {
		return err
	}
	if task.Execution, err = optionalArtifact(b.executionURI, b.executionDigest); err != nil {
		return err
	}
	if task.Verification, err = optionalArtifact(b.verificationURI, b.verificationDigest); err != nil {
		return err
	}
	if task.Review, err = optionalArtifact(b.reviewURI, b.reviewDigest); err != nil {
		return err
	}
	if task.Approval, err = optionalArtifact(b.approvalURI, b.approvalDigest); err != nil {
		return err
	}
	if b.candidateCommit != nil {
		task.CandidateCommit = *b.candidateCommit
	}
	return nil
}

func (b taskRowBindings) applyLease(task *Task) error {
	if b.leaseID == nil && b.leaseOwner == nil && b.leaseExpiry == nil && b.leaseFence == nil {
		return nil
	}
	if b.leaseID == nil || b.leaseOwner == nil || b.leaseExpiry == nil ||
		b.leaseFence == nil || *b.leaseFence < 1 || *b.leaseFence > int64(^uint32(0)) {
		return fmt.Errorf("%w: partial lease", ErrInvalid)
	}
	task.Lease = &LeaseRef{
		ID: *b.leaseID, OwnerID: *b.leaseOwner, ExpiresAt: *b.leaseExpiry,
		FencingToken: uint32(*b.leaseFence),
	}
	return nil
}

func updateTask(ctx context.Context, tx pgx.Tx, task Task, expected uint64) error {
	values := taskBindingValues(task)
	tag, err := tx.Exec(ctx, `UPDATE workflow_tasks SET state=$3,revision=$4,
		current_attempt=$5,lease_id=$6,lease_owner_id=$7,lease_expires_at=$8,
		lease_fencing_token=$9,proposal_uri=$10,proposal_digest=$11,
		execution_uri=$12,execution_digest=$13,candidate_commit=$14,
		verification_uri=$15,verification_digest=$16,review_uri=$17,review_digest=$18,
		approval_uri=$19,approval_digest=$20,updated_at=$21
		WHERE run_id=$1 AND task_id=$2 AND revision=$22`,
		task.RunID, task.ID, task.State, task.Revision, task.CurrentAttempt,
		values[0], values[1], values[2], values[3], values[4], values[5], values[6],
		values[7], values[8], values[9], values[10], values[11], values[12],
		values[13], values[14], task.UpdatedAt, expected)
	if err != nil {
		return fmt.Errorf("update task: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrRevisionConflict
	}
	return nil
}

func taskBindingValues(task Task) []any {
	values := make([]any, 15)
	if task.Lease != nil {
		values[0], values[1], values[2], values[3] =
			task.Lease.ID, task.Lease.OwnerID, task.Lease.ExpiresAt, task.Lease.FencingToken
	}
	putArtifact := func(offset int, value *ArtifactRef) {
		if value != nil {
			values[offset], values[offset+1] = value.URI, value.Digest
		}
	}
	putArtifact(4, task.Proposal)
	putArtifact(6, task.Execution)
	if task.CandidateCommit != "" {
		values[8] = task.CandidateCommit
	}
	putArtifact(9, task.Verification)
	putArtifact(11, task.Review)
	putArtifact(13, task.Approval)
	return values
}

func optionalArtifact(uri, digest *string) (*ArtifactRef, error) {
	if uri == nil && digest == nil {
		return nil, nil
	}
	if uri == nil || digest == nil {
		return nil, fmt.Errorf("%w: partial artifact binding", ErrInvalid)
	}
	return &ArtifactRef{URI: *uri, Digest: *digest}, nil
}

func makeDependentsReady(
	ctx context.Context, tx pgx.Tx, accepted Task, meta CommandEnvelope,
) ([]Event, error) {
	rows, err := tx.Query(ctx, `SELECT t.task_id,t.revision
		FROM workflow_tasks t
		JOIN workflow_task_dependencies d ON d.run_id=t.run_id AND d.task_id=t.task_id
		WHERE d.run_id=$1 AND d.depends_on_task_id=$2 AND t.state='PENDING'
		FOR UPDATE OF t`, accepted.RunID, accepted.ID)
	if err != nil {
		return nil, fmt.Errorf("lock dependent tasks: %w", err)
	}
	defer rows.Close()
	type dependent struct {
		id       string
		revision uint64
	}
	var candidates []dependent
	for rows.Next() {
		var value dependent
		if err := rows.Scan(&value.id, &value.revision); err != nil {
			return nil, err
		}
		candidates = append(candidates, value)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	var events []Event
	for _, candidate := range candidates {
		var unmet int
		if err := tx.QueryRow(ctx, `SELECT count(*)
			FROM workflow_task_dependencies d
			JOIN workflow_tasks prerequisite ON prerequisite.run_id=d.run_id
				AND prerequisite.task_id=d.depends_on_task_id
			WHERE d.run_id=$1 AND d.task_id=$2 AND prerequisite.state <> 'ACCEPTED'`,
			accepted.RunID, candidate.id).Scan(&unmet); err != nil {
			return nil, fmt.Errorf("check task dependencies: %w", err)
		}
		if unmet != 0 {
			continue
		}
		tag, err := tx.Exec(ctx, `UPDATE workflow_tasks SET state='READY',
			revision=revision+1,updated_at=$3 WHERE run_id=$1 AND task_id=$2
			AND state='PENDING' AND revision=$4`,
			accepted.RunID, candidate.id, meta.Timestamp.UTC(), candidate.revision)
		if err != nil {
			return nil, fmt.Errorf("ready dependent task: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return nil, ErrRevisionConflict
		}
		eventID := uuid.NewSHA1(uuid.MustParse(meta.CommandID), []byte(candidate.id+":ready")).String()
		events = append(events, Event{
			ID: eventID, AggregateType: "TASK", AggregateID: candidate.id,
			Revision: candidate.revision + 1, Type: "TASK_READY",
			Timestamp: meta.Timestamp.UTC(), Actor: meta.Actor,
			CorrelationID: meta.CorrelationID, CausationID: meta.CausationID,
			Payload: map[string]any{"dependency_task_id": accepted.ID},
		})
	}
	return events, nil
}

func loadCommandResult(
	ctx context.Context, tx pgx.Tx, commandID, digest string,
) (CommandResult, bool, error) {
	var storedDigest string
	var encoded []byte
	err := tx.QueryRow(ctx,
		`SELECT request_digest, result FROM workflow_commands WHERE command_id = $1 FOR UPDATE`,
		commandID).Scan(&storedDigest, &encoded)
	if errors.Is(err, pgx.ErrNoRows) {
		return CommandResult{}, false, nil
	}
	if err != nil {
		return CommandResult{}, false, fmt.Errorf("load command: %w", err)
	}
	if storedDigest != digest {
		return CommandResult{}, false, ErrCommandConflict
	}
	if len(encoded) == 0 {
		return CommandResult{}, false, fmt.Errorf("%w: incomplete prior command", ErrCommandConflict)
	}
	var result CommandResult
	if err := json.Unmarshal(encoded, &result); err != nil {
		return CommandResult{}, false, fmt.Errorf("decode command result: %w", err)
	}
	return result, true, nil
}

func reserveCommand(ctx context.Context, tx pgx.Tx, command RunCommand, digest string) error {
	meta := command.Envelope()
	_, err := tx.Exec(ctx, `INSERT INTO workflow_commands
		(command_id, aggregate_type, aggregate_id, request_digest, actor_id, actor_kind,
		 expected_revision, correlation_id, causation_id, requested_at)
		VALUES ($1, 'RUN', $2, $3, $4, $5, $6, $7, $8, $9)`,
		meta.CommandID, command.RunID(), digest, meta.Actor.ID, meta.Actor.Kind,
		meta.ExpectedRevision, meta.CorrelationID, meta.CausationID, meta.Timestamp.UTC())
	if err != nil {
		return fmt.Errorf("reserve command: %w", err)
	}
	return nil
}

func loadRun(ctx context.Context, tx pgx.Tx, id string) (Run, bool, error) {
	var run Run
	var specificationURI, specificationDigest, taskGraphURI, taskGraphDigest *string
	var lifecycle []byte
	err := tx.QueryRow(ctx, `SELECT run_id, state, revision, specification_uri,
		specification_digest, task_graph_uri, task_graph_digest, lifecycle_bindings,
		created_at, updated_at
		FROM workflow_runs WHERE run_id = $1 FOR UPDATE`, id).Scan(
		&run.ID, &run.State, &run.Revision, &specificationURI,
		&specificationDigest, &taskGraphURI, &taskGraphDigest, &lifecycle,
		&run.CreatedAt, &run.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, false, ErrNotFound
	}
	if err != nil {
		return Run{}, false, fmt.Errorf("load run: %w", err)
	}
	if specificationURI != nil || specificationDigest != nil {
		if specificationURI == nil || specificationDigest == nil {
			return Run{}, false, fmt.Errorf("%w: partial specification binding", ErrInvalid)
		}
		run.Specification = &ArtifactRef{URI: *specificationURI, Digest: *specificationDigest}
	}
	if taskGraphURI != nil || taskGraphDigest != nil {
		if taskGraphURI == nil || taskGraphDigest == nil {
			return Run{}, false, fmt.Errorf("%w: partial task graph binding", ErrInvalid)
		}
		run.TaskGraph = &ArtifactRef{URI: *taskGraphURI, Digest: *taskGraphDigest}
	}
	if err := decodeRunLifecycle(lifecycle, &run); err != nil {
		return Run{}, false, err
	}
	if err := run.Validate(); err != nil {
		return Run{}, false, err
	}
	return run, true, nil
}

func insertRun(ctx context.Context, tx pgx.Tx, run Run) error {
	var uri, digest, graphURI, graphDigest *string
	if run.Specification != nil {
		uri, digest = &run.Specification.URI, &run.Specification.Digest
	}
	if run.TaskGraph != nil {
		graphURI, graphDigest = &run.TaskGraph.URI, &run.TaskGraph.Digest
	}
	lifecycle, err := encodeRunLifecycle(run)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO workflow_runs
		(run_id, state, revision, specification_uri, specification_digest,
		 task_graph_uri, task_graph_digest, lifecycle_bindings, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		run.ID, run.State, run.Revision, uri, digest, graphURI, graphDigest,
		lifecycle, run.CreatedAt, run.UpdatedAt)
	if err != nil {
		return fmt.Errorf("insert run: %w", err)
	}
	return nil
}

func updateRun(ctx context.Context, tx pgx.Tx, run Run, expected uint64) error {
	var uri, digest, graphURI, graphDigest *string
	if run.Specification != nil {
		uri, digest = &run.Specification.URI, &run.Specification.Digest
	}
	if run.TaskGraph != nil {
		graphURI, graphDigest = &run.TaskGraph.URI, &run.TaskGraph.Digest
	}
	lifecycle, err := encodeRunLifecycle(run)
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE workflow_runs SET state=$2, revision=$3,
		specification_uri=$4, specification_digest=$5, task_graph_uri=$6,
		task_graph_digest=$7, lifecycle_bindings=$8, updated_at=$9
		WHERE run_id=$1 AND revision=$10`,
		run.ID, run.State, run.Revision, uri, digest, graphURI, graphDigest,
		lifecycle, run.UpdatedAt, expected)
	if err != nil {
		return fmt.Errorf("update run: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrRevisionConflict
	}
	return nil
}

type runLifecycleBindings struct {
	Execution       *ArtifactRef `json:"execution,omitempty"`
	CandidateCommit string       `json:"candidate_commit,omitempty"`
	Verification    *ArtifactRef `json:"verification,omitempty"`
	Review          *ArtifactRef `json:"review,omitempty"`
	Approval        *ArtifactRef `json:"approval,omitempty"`
	Publication     *ArtifactRef `json:"publication,omitempty"`
	Merge           *ArtifactRef `json:"merge,omitempty"`
}

func encodeRunLifecycle(run Run) ([]byte, error) {
	encoded, err := json.Marshal(runLifecycleBindings{
		Execution: run.Execution, CandidateCommit: run.CandidateCommit,
		Verification: run.Verification, Review: run.Review,
		Approval: run.Approval, Publication: run.Publication, Merge: run.Merge,
	})
	if err != nil {
		return nil, fmt.Errorf("encode run lifecycle: %w", err)
	}
	return encoded, nil
}

func decodeRunLifecycle(encoded []byte, run *Run) error {
	var bindings runLifecycleBindings
	if err := json.Unmarshal(encoded, &bindings); err != nil {
		return fmt.Errorf("decode run lifecycle: %w", err)
	}
	run.Execution, run.CandidateCommit = bindings.Execution, bindings.CandidateCommit
	run.Verification, run.Review = bindings.Verification, bindings.Review
	run.Approval, run.Publication, run.Merge = bindings.Approval, bindings.Publication, bindings.Merge
	return nil
}

func validateRunTaskGate(ctx context.Context, tx pgx.Tx, command RunCommand) error {
	switch command.(type) {
	case StartRun:
		var total, terminal int
		err := tx.QueryRow(ctx, `SELECT count(*),
			count(*) FILTER (WHERE state IN ('FAILED','CANCELLED'))
			FROM workflow_tasks WHERE run_id=$1`, command.RunID()).Scan(&total, &terminal)
		if err != nil {
			return fmt.Errorf("check runnable tasks: %w", err)
		}
		if total == 0 || terminal != 0 {
			return ErrInvalidTransition
		}
	case RecordRunExecution, RecordRunVerification, RecordRunReview,
		ApproveRun, RejectRun, RecordDraftPullRequest, RecordMerge:
		var total, accepted int
		err := tx.QueryRow(ctx, `SELECT count(*),
			count(*) FILTER (WHERE state='ACCEPTED')
			FROM workflow_tasks WHERE run_id=$1`, command.RunID()).Scan(&total, &accepted)
		if err != nil {
			return fmt.Errorf("check completed tasks: %w", err)
		}
		if total == 0 || accepted != total {
			return ErrInvalidTransition
		}
	}
	return nil
}

func cancelRunTasks(
	ctx context.Context, tx pgx.Tx, runID string, meta CommandEnvelope, reason string,
) ([]Event, error) {
	rows, err := tx.Query(ctx, `SELECT task_id,revision FROM workflow_tasks
		WHERE run_id=$1 AND state NOT IN ('ACCEPTED','FAILED','CANCELLED') FOR UPDATE`, runID)
	if err != nil {
		return nil, fmt.Errorf("lock tasks for cancellation: %w", err)
	}
	defer rows.Close()
	type cancellable struct {
		id       string
		revision uint64
	}
	var tasks []cancellable
	for rows.Next() {
		var task cancellable
		if err := rows.Scan(&task.id, &task.revision); err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	events := make([]Event, 0, len(tasks))
	for _, task := range tasks {
		if _, err := tx.Exec(ctx, `UPDATE workflow_tasks SET state='CANCELLED',
			revision=revision+1,updated_at=$3 WHERE run_id=$1 AND task_id=$2`,
			runID, task.id, meta.Timestamp.UTC()); err != nil {
			return nil, fmt.Errorf("cancel task: %w", err)
		}
		eventID := uuid.NewSHA1(uuid.MustParse(meta.CommandID), []byte(task.id+":cancel")).String()
		events = append(events, Event{
			ID: eventID, AggregateType: "TASK", AggregateID: task.id,
			Revision: task.revision + 1, Type: "TASK_CANCELLED",
			Timestamp: meta.Timestamp.UTC(), Actor: meta.Actor,
			CorrelationID: meta.CorrelationID, CausationID: meta.CausationID,
			Payload: map[string]any{"run_id": runID, "reason": reason},
		})
	}
	return events, nil
}

func insertTask(ctx context.Context, tx pgx.Tx, task Task) error {
	values := taskBindingValues(task)
	_, err := tx.Exec(ctx, `INSERT INTO workflow_tasks
		(task_id,run_id,state,revision,max_attempts,current_attempt,
		 lease_id,lease_owner_id,lease_expires_at,lease_fencing_token,proposal_uri,proposal_digest,
		 execution_uri,execution_digest,candidate_commit,verification_uri,verification_digest,
		 review_uri,review_digest,approval_uri,approval_digest,created_at,updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23)`,
		task.ID, task.RunID, task.State, task.Revision, task.MaxAttempts,
		task.CurrentAttempt, values[0], values[1], values[2], values[3], values[4],
		values[5], values[6], values[7], values[8], values[9], values[10], values[11],
		values[12], values[13], values[14], task.CreatedAt, task.UpdatedAt)
	if err != nil {
		return fmt.Errorf("insert task: %w", err)
	}
	return nil
}

func insertEvent(ctx context.Context, tx pgx.Tx, event Event, commandID string) error {
	payload, err := json.Marshal(event.Payload)
	if err != nil {
		return fmt.Errorf("marshal event payload: %w", err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO workflow_events
		(event_id, command_id, aggregate_type, aggregate_id, revision, event_type,
		 occurred_at, actor_id, actor_kind, correlation_id, causation_id, payload)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		event.ID, commandID, event.AggregateType, event.AggregateID, event.Revision,
		event.Type, event.Timestamp, event.Actor.ID, event.Actor.Kind,
		event.CorrelationID, event.CausationID, payload)
	if err != nil {
		return fmt.Errorf("insert event: %w", err)
	}
	return nil
}

func eventIDs(events []Event) []string {
	ids := make([]string, len(events))
	for index := range events {
		ids[index] = events[index].ID
	}
	return ids
}

func taskIDs(tasks []Task) []string {
	ids := make([]string, len(tasks))
	for index := range tasks {
		ids[index] = tasks[index].ID
	}
	return ids
}

func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "23505"
}
