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
	next, events, err := applyRunCommand(current, command)
	if err != nil {
		return CommandResult{}, err
	}
	if err := persistRunTransition(ctx, tx, command, digest, current, next, events); err != nil {
		return CommandResult{}, err
	}
	return recordRunCommandResult(ctx, tx, command, next, events)
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

func applyRunCommand(current Run, command RunCommand) (Run, []Event, error) {
	if create, ok := command.(CreateRun); ok {
		return NewRun(create)
	}
	return current.Apply(command)
}

func persistRunTransition(
	ctx context.Context,
	tx pgx.Tx,
	command RunCommand,
	digest string,
	current, next Run,
	events []Event,
) error {
	if err := reserveCommand(ctx, tx, command, digest); err != nil {
		if isUniqueViolation(err) {
			return ErrCommandConflict
		}
		return err
	}
	for _, event := range events {
		if err := insertEvent(ctx, tx, event, command.Envelope().CommandID); err != nil {
			return err
		}
	}
	if _, ok := command.(CreateRun); ok {
		return insertRun(ctx, tx, next)
	}
	return updateRun(ctx, tx, next, current.Revision)
}

func recordRunCommandResult(
	ctx context.Context, tx pgx.Tx, command RunCommand, next Run, events []Event,
) (CommandResult, error) {
	result := CommandResult{
		AggregateID: next.ID, State: next.State, Revision: next.Revision,
		EventIDs: eventIDs(events),
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
	var specificationURI, specificationDigest *string
	err := tx.QueryRow(ctx, `SELECT run_id, state, revision, specification_uri,
		specification_digest, created_at, updated_at
		FROM workflow_runs WHERE run_id = $1 FOR UPDATE`, id).Scan(
		&run.ID, &run.State, &run.Revision, &specificationURI,
		&specificationDigest, &run.CreatedAt, &run.UpdatedAt)
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
	if err := run.Validate(); err != nil {
		return Run{}, false, err
	}
	return run, true, nil
}

func insertRun(ctx context.Context, tx pgx.Tx, run Run) error {
	var uri, digest *string
	if run.Specification != nil {
		uri, digest = &run.Specification.URI, &run.Specification.Digest
	}
	_, err := tx.Exec(ctx, `INSERT INTO workflow_runs
		(run_id, state, revision, specification_uri, specification_digest, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		run.ID, run.State, run.Revision, uri, digest, run.CreatedAt, run.UpdatedAt)
	if err != nil {
		return fmt.Errorf("insert run: %w", err)
	}
	return nil
}

func updateRun(ctx context.Context, tx pgx.Tx, run Run, expected uint64) error {
	var uri, digest *string
	if run.Specification != nil {
		uri, digest = &run.Specification.URI, &run.Specification.Digest
	}
	tag, err := tx.Exec(ctx, `UPDATE workflow_runs SET state=$2, revision=$3,
		specification_uri=$4, specification_digest=$5, updated_at=$6
		WHERE run_id=$1 AND revision=$7`,
		run.ID, run.State, run.Revision, uri, digest, run.UpdatedAt, expected)
	if err != nil {
		return fmt.Errorf("update run: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrRevisionConflict
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

func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "23505"
}
