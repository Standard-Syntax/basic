package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
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
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return CommandResult{}, fmt.Errorf("begin workflow transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	result, err := executeRunTransaction(ctx, tx, command, digest)
	if err != nil {
		return CommandResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return CommandResult{}, fmt.Errorf("commit workflow transaction: %w", err)
	}
	return result, nil
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

func executeRunTransaction(
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
	decision, err := decideRunCommand(current, command)
	if err != nil {
		return CommandResult{}, err
	}
	if err := persistRunTransition(ctx, tx, command, digest, decision); err != nil {
		return CommandResult{}, err
	}
	return recordRunCommandResult(ctx, tx, command, decision)
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

func persistRunTransition(
	ctx context.Context,
	tx pgx.Tx,
	command RunCommand,
	digest string,
	decision runPersistenceDecision,
) error {
	if err := reserveCommand(ctx, tx, command, digest); err != nil {
		if isUniqueViolation(err) {
			return ErrCommandConflict
		}
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
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return CommandResult{}, fmt.Errorf("begin workflow transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	result, err := executeTaskTransaction(ctx, tx, command, digest)
	if err != nil {
		return CommandResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return CommandResult{}, fmt.Errorf("commit task transaction: %w", err)
	}
	return result, nil
}

func taskCommandDigest(command TaskCommand) (string, error) {
	return typedCommandDigest(command, "task command")
}

func executeTaskTransaction(
	ctx context.Context, tx pgx.Tx, command TaskCommand, digest string,
) (CommandResult, error) {
	if result, found, err := loadCommandResult(ctx, tx, command.Envelope().CommandID, digest); err != nil {
		return CommandResult{}, err
	} else if found {
		result.Replay = true
		return result, nil
	}
	current, err := loadTask(ctx, tx, command.RunID(), command.TaskID())
	if err != nil {
		return CommandResult{}, err
	}
	next, events, err := current.Apply(command)
	if err != nil {
		return CommandResult{}, err
	}
	events, err = persistTaskTransition(ctx, tx, command, digest, current, next, events)
	if err != nil {
		return CommandResult{}, err
	}
	return recordTaskCommandResult(ctx, tx, command, next, events)
}

func persistTaskTransition(
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
			return nil, ErrCommandConflict
		}
		return nil, err
	}
	for _, event := range events {
		if err := insertEvent(ctx, tx, event, command.Envelope().CommandID); err != nil {
			return nil, err
		}
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
		lease_id::text,lease_owner_id::text,lease_expires_at,proposal_uri,proposal_digest,
		execution_uri,execution_digest,candidate_commit,verification_uri,verification_digest,
		review_uri,review_digest,approval_uri,approval_digest,created_at,updated_at
		FROM workflow_tasks WHERE run_id=$1 AND task_id=$2 FOR UPDATE`,
		runID, taskID)
	err := row.Scan(
		&task.ID, &task.RunID, &task.State, &task.Revision, &task.MaxAttempts,
		&task.CurrentAttempt, &bindings.leaseID, &bindings.leaseOwner, &bindings.leaseExpiry,
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
	if b.leaseID == nil && b.leaseOwner == nil && b.leaseExpiry == nil {
		return nil
	}
	if b.leaseID == nil || b.leaseOwner == nil || b.leaseExpiry == nil {
		return fmt.Errorf("%w: partial lease", ErrInvalid)
	}
	task.Lease = &LeaseRef{
		ID: *b.leaseID, OwnerID: *b.leaseOwner, ExpiresAt: *b.leaseExpiry,
	}
	return nil
}

func updateTask(ctx context.Context, tx pgx.Tx, task Task, expected uint64) error {
	values := taskBindingValues(task)
	tag, err := tx.Exec(ctx, `UPDATE workflow_tasks SET state=$3,revision=$4,
		current_attempt=$5,lease_id=$6,lease_owner_id=$7,lease_expires_at=$8,
		proposal_uri=$9,proposal_digest=$10,execution_uri=$11,execution_digest=$12,
		candidate_commit=$13,verification_uri=$14,verification_digest=$15,
		review_uri=$16,review_digest=$17,approval_uri=$18,approval_digest=$19,updated_at=$20
		WHERE run_id=$1 AND task_id=$2 AND revision=$21`,
		task.RunID, task.ID, task.State, task.Revision, task.CurrentAttempt,
		values[0], values[1], values[2], values[3], values[4], values[5], values[6],
		values[7], values[8], values[9], values[10], values[11], values[12],
		values[13], task.UpdatedAt, expected)
	if err != nil {
		return fmt.Errorf("update task: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrRevisionConflict
	}
	return nil
}

func taskBindingValues(task Task) []any {
	values := make([]any, 14)
	if task.Lease != nil {
		values[0], values[1], values[2] = task.Lease.ID, task.Lease.OwnerID, task.Lease.ExpiresAt
	}
	putArtifact := func(offset int, value *ArtifactRef) {
		if value != nil {
			values[offset], values[offset+1] = value.URI, value.Digest
		}
	}
	putArtifact(3, task.Proposal)
	putArtifact(5, task.Execution)
	if task.CandidateCommit != "" {
		values[7] = task.CandidateCommit
	}
	putArtifact(8, task.Verification)
	putArtifact(10, task.Review)
	putArtifact(12, task.Approval)
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
	err := tx.QueryRow(ctx, `SELECT run_id, state, revision, specification_uri,
		specification_digest, task_graph_uri, task_graph_digest, created_at, updated_at
		FROM workflow_runs WHERE run_id = $1 FOR UPDATE`, id).Scan(
		&run.ID, &run.State, &run.Revision, &specificationURI,
		&specificationDigest, &taskGraphURI, &taskGraphDigest,
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
	_, err := tx.Exec(ctx, `INSERT INTO workflow_runs
		(run_id, state, revision, specification_uri, specification_digest,
		 task_graph_uri, task_graph_digest, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		run.ID, run.State, run.Revision, uri, digest, graphURI, graphDigest,
		run.CreatedAt, run.UpdatedAt)
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
	tag, err := tx.Exec(ctx, `UPDATE workflow_runs SET state=$2, revision=$3,
		specification_uri=$4, specification_digest=$5, task_graph_uri=$6,
		task_graph_digest=$7, updated_at=$8
		WHERE run_id=$1 AND revision=$9`,
		run.ID, run.State, run.Revision, uri, digest, graphURI, graphDigest,
		run.UpdatedAt, expected)
	if err != nil {
		return fmt.Errorf("update run: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrRevisionConflict
	}
	return nil
}

func insertTask(ctx context.Context, tx pgx.Tx, task Task) error {
	values := taskBindingValues(task)
	_, err := tx.Exec(ctx, `INSERT INTO workflow_tasks
		(task_id,run_id,state,revision,max_attempts,current_attempt,
		 lease_id,lease_owner_id,lease_expires_at,proposal_uri,proposal_digest,
		 execution_uri,execution_digest,candidate_commit,verification_uri,verification_digest,
		 review_uri,review_digest,approval_uri,approval_digest,created_at,updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22)`,
		task.ID, task.RunID, task.State, task.Revision, task.MaxAttempts,
		task.CurrentAttempt, values[0], values[1], values[2], values[3], values[4],
		values[5], values[6], values[7], values[8], values[9], values[10], values[11],
		values[12], values[13], task.CreatedAt, task.UpdatedAt)
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
