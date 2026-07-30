package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

type PendingApproval struct {
	Kind            string       `json:"kind"`
	RunID           string       `json:"run_id"`
	TaskID          string       `json:"task_id,omitempty"`
	Revision        uint64       `json:"revision"`
	CandidateCommit string       `json:"candidate_commit,omitempty"`
	Artifact        *ArtifactRef `json:"artifact,omitempty"`
}

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

func (s *Store) ListPendingApprovals(ctx context.Context) ([]PendingApproval, error) {
	rows, err := s.pool.Query(ctx, `SELECT run_id::text,state FROM workflow_runs
		WHERE state IN ('SPECIFICATION_REVIEW','TASK_PLAN_REVIEW','AWAITING_APPROVAL')
		ORDER BY created_at,run_id`)
	if err != nil {
		return nil, fmt.Errorf("list pending run approvals: %w", err)
	}
	type runIdentity struct {
		id    string
		state RunState
	}
	var runs []runIdentity
	for rows.Next() {
		var value runIdentity
		if err := rows.Scan(&value.id, &value.state); err != nil {
			rows.Close()
			return nil, err
		}
		runs = append(runs, value)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	pending := make([]PendingApproval, 0, len(runs))
	for _, identity := range runs {
		run, err := s.GetRun(ctx, identity.id)
		if err != nil {
			return nil, err
		}
		item := PendingApproval{
			Kind: string(identity.state), RunID: run.ID, Revision: run.Revision,
			CandidateCommit: run.CandidateCommit,
		}
		switch identity.state {
		case RunStateSpecificationReview:
			item.Artifact = run.Specification
		case RunStateTaskPlanReview:
			item.Artifact = run.TaskGraph
		case RunStateAwaitingApproval:
			item.Artifact = run.Review
		}
		pending = append(pending, item)
	}
	taskRows, err := s.pool.Query(ctx, `SELECT run_id::text,task_id::text
		FROM workflow_tasks WHERE state='AWAITING_APPROVAL' ORDER BY created_at,task_id`)
	if err != nil {
		return nil, fmt.Errorf("list pending task approvals: %w", err)
	}
	defer taskRows.Close()
	for taskRows.Next() {
		var runID, taskID string
		if err := taskRows.Scan(&runID, &taskID); err != nil {
			return nil, err
		}
		task, err := s.GetTask(ctx, runID, taskID)
		if err != nil {
			return nil, err
		}
		pending = append(pending, PendingApproval{
			Kind: "TASK", RunID: runID, TaskID: taskID, Revision: task.Revision,
			CandidateCommit: task.CandidateCommit, Artifact: task.Review,
		})
	}
	if err := taskRows.Err(); err != nil {
		return nil, err
	}
	return pending, nil
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
