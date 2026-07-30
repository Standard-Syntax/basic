package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

func (s *Store) GetRun(ctx context.Context, runID string) (Run, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Run{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	run, found, err := loadRun(ctx, tx, runID)
	if err != nil {
		return Run{}, err
	}
	if !found {
		return Run{}, ErrNotFound
	}
	return cloneRun(run), nil
}

func (s *Store) GetTask(ctx context.Context, runID, taskID string) (Task, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Task{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	task, err := loadTask(ctx, tx, runID, taskID)
	return cloneTask(task), err
}

func (s *Store) ListTasks(ctx context.Context, runID string) ([]Task, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `SELECT task_id FROM workflow_tasks WHERE run_id=$1 ORDER BY task_id`, runID)
	if err != nil {
		return nil, fmt.Errorf("list task identities: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	tasks := make([]Task, 0, len(ids))
	for _, id := range ids {
		task, err := loadTask(ctx, tx, runID, id)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, cloneTask(task))
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return tasks, nil
}

func (s *Store) ListEvents(ctx context.Context, aggregateType, aggregateID string) ([]Event, error) {
	rows, err := s.pool.Query(ctx, `SELECT event_id,aggregate_type,aggregate_id,revision,
		event_type,occurred_at,actor_id,actor_kind,correlation_id,causation_id,payload
		FROM workflow_events WHERE aggregate_type=$1 AND aggregate_id=$2 ORDER BY sequence`,
		aggregateType, aggregateID)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()
	var events []Event
	for rows.Next() {
		var event Event
		var payload []byte
		if err := rows.Scan(&event.ID, &event.AggregateType, &event.AggregateID,
			&event.Revision, &event.Type, &event.Timestamp, &event.Actor.ID,
			&event.Actor.Kind, &event.CorrelationID, &event.CausationID, &payload); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(payload, &event.Payload); err != nil {
			return nil, fmt.Errorf("%w: event payload", ErrInvalid)
		}
		if err := event.Actor.Validate(); err != nil || event.Revision == 0 {
			return nil, fmt.Errorf("%w: event snapshot", ErrInvalid)
		}
		event.Payload = cloneMap(event.Payload)
		events = append(events, event)
	}
	if err := rows.Err(); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	return events, nil
}

func cloneRun(value Run) Run {
	if value.Specification != nil {
		cloned := *value.Specification
		value.Specification = &cloned
	}
	if value.TaskGraph != nil {
		cloned := *value.TaskGraph
		value.TaskGraph = &cloned
	}
	for src, dst := range map[*ArtifactRef]**ArtifactRef{
		value.Execution: &value.Execution, value.Verification: &value.Verification,
		value.Review: &value.Review, value.Approval: &value.Approval,
		value.Publication: &value.Publication, value.Merge: &value.Merge,
	} {
		if src != nil {
			cloned := *src
			*dst = &cloned
		}
	}
	return value
}

func cloneTask(value Task) Task {
	if value.Lease != nil {
		cloned := *value.Lease
		value.Lease = &cloned
	}
	for src, dst := range map[*ArtifactRef]**ArtifactRef{
		value.Proposal: &value.Proposal, value.Execution: &value.Execution,
		value.Verification: &value.Verification, value.Review: &value.Review,
		value.Approval: &value.Approval,
	} {
		if src != nil {
			cloned := *src
			*dst = &cloned
		}
	}
	return value
}

func cloneMap(value map[string]any) map[string]any {
	if value == nil {
		return nil
	}
	encoded, _ := json.Marshal(value)
	var cloned map[string]any
	_ = json.Unmarshal(encoded, &cloned)
	return cloned
}
