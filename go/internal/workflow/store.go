package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

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
	State       RunState `json:"state"`
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
		AggregateID: decision.next.ID, State: decision.next.State,
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
	_, err := tx.Exec(ctx, `INSERT INTO workflow_tasks
		(task_id, run_id, state, revision, max_attempts, current_attempt, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		task.ID, task.RunID, task.State, task.Revision, task.MaxAttempts,
		task.CurrentAttempt, task.CreatedAt, task.UpdatedAt)
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
