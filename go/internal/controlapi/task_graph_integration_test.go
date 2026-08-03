//go:build integration

package controlapi

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Standard-Syntax/basic/go/internal/orchestration"
	"github.com/Standard-Syntax/basic/go/internal/runtime"
	"github.com/Standard-Syntax/basic/go/internal/workflow"
	"github.com/google/uuid"
)

type taskGraphFixture struct {
	intake      *intakeFixture
	coordinator *TaskGraphApprovalCoordinator
	request     TaskGraphApprovalRequest
}

func newTaskGraphFixture(t *testing.T) *taskGraphFixture {
	t.Helper()
	intake := newIntakeFixture(t)
	if _, err := intake.coordinator.Accept(t.Context(), intake.request); err != nil {
		t.Fatal(err)
	}
	store := workflow.NewStore(intake.pool)
	runID := intake.request.Command.ID
	serviceID, humanID := uuid.NewString(), intake.request.Command.Meta.Actor.ID
	at := intake.request.Command.Meta.Timestamp.Add(time.Second)
	put := func(body string) workflow.ArtifactRef {
		t.Helper()
		ref, err := intake.artifacts.Put(t.Context(), []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		return ref
	}
	specification := put("approved specification")
	graph := put("approved task graph")
	taskArtifact := put("approved task")
	envelope := func(commandID, actorID string, kind workflow.ActorKind, revision uint64, offset time.Duration) workflow.CommandEnvelope {
		return workflow.CommandEnvelope{
			CommandID: commandID, Actor: workflow.Actor{ID: actorID, Kind: kind},
			ExpectedRevision: revision, Timestamp: at.Add(offset),
			CorrelationID: intake.request.Command.Meta.CorrelationID, CausationID: commandID,
		}
	}
	proposeSpecification := uuid.NewString()
	if _, err := store.ExecuteRun(t.Context(), workflow.ProposeSpecification{
		Meta: envelope(proposeSpecification, serviceID, workflow.ActorWorkflowService, 1, 0),
		ID:   runID, Specification: specification,
	}); err != nil {
		t.Fatal(err)
	}
	approveSpecification := uuid.NewString()
	if _, err := store.ExecuteRun(t.Context(), workflow.ApproveSpecification{
		Meta: envelope(approveSpecification, humanID, workflow.ActorHuman, 2, time.Second),
		ID:   runID, Specification: specification,
	}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.NewBindingRepository(intake.pool).CheckpointSpecification(
		t.Context(), runID, specification,
	); err != nil {
		t.Fatal(err)
	}
	proposeGraph := uuid.NewString()
	if _, err := store.ExecuteRun(t.Context(), workflow.ProposeTaskGraph{
		Meta: envelope(proposeGraph, serviceID, workflow.ActorWorkflowService, 3, 2*time.Second),
		ID:   runID, TaskGraph: graph,
	}); err != nil {
		t.Fatal(err)
	}
	taskID := uuid.NewString()
	approveGraph := uuid.NewString()
	command := workflow.ApproveTaskGraph{
		Meta: envelope(approveGraph, humanID, workflow.ActorHuman, 4, 3*time.Second),
		ID:   runID, TaskGraph: graph,
		Tasks: []workflow.TaskDefinition{{ID: taskID, MaxAttempts: 2}},
	}
	job := runtime.Job{
		ID:    orchestration.StableID(runID, taskID, "1", orchestration.StageStart, "job"),
		RunID: runID, TaskID: &taskID, Attempt: 1, Stage: orchestration.StageStart,
		AvailableAt: command.Meta.Timestamp.UTC(),
	}
	coordinator, err := NewTaskGraphApprovalCoordinator(intake.pool, store)
	if err != nil {
		t.Fatal(err)
	}
	return &taskGraphFixture{
		intake: intake, coordinator: coordinator,
		request: TaskGraphApprovalRequest{
			Command: command, Graph: graph,
			Task:     runtime.TaskBinding{RunID: runID, TaskID: taskID, ApprovedTask: taskArtifact},
			StartJob: job,
		},
	}
}

func TestTaskGraphApprovalRollsBackEveryPrecommitFailure(t *testing.T) {
	for _, point := range []TaskGraphApprovalFaultPoint{
		FaultTaskGraphAfterWorkflow, FaultTaskGraphAfterBinding, FaultTaskGraphAfterEnqueue,
	} {
		t.Run(string(point), func(t *testing.T) {
			fixture := newTaskGraphFixture(t)
			fixture.coordinator.inject = func(actual TaskGraphApprovalFaultPoint) error {
				if actual == point {
					return errors.New("injected task graph approval failure")
				}
				return nil
			}
			if _, err := fixture.coordinator.ApproveTaskGraph(t.Context(), fixture.request); err == nil {
				t.Fatal("injected task graph approval failure succeeded")
			}
			assertNoTaskGraphApproval(t, fixture)
			fixture.coordinator.inject = nil
			if _, err := fixture.coordinator.ApproveTaskGraph(t.Context(), fixture.request); err != nil {
				t.Fatal(err)
			}
			assertCompleteTaskGraphApproval(t, fixture)
		})
	}
}

func TestTaskGraphApprovalCancellationUsesDetachedRollback(t *testing.T) {
	fixture := newTaskGraphFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	fixture.coordinator.inject = func(point TaskGraphApprovalFaultPoint) error {
		if point == FaultTaskGraphAfterWorkflow {
			cancel()
			return context.Canceled
		}
		return nil
	}
	if _, err := fixture.coordinator.ApproveTaskGraph(ctx, fixture.request); !errors.Is(err, context.Canceled) {
		t.Fatalf("approval error=%v", err)
	}
	assertNoTaskGraphApproval(t, fixture)
}

func TestTaskGraphApprovalExactReplayConvergesOnOneJob(t *testing.T) {
	fixture := newTaskGraphFixture(t)
	first, err := fixture.coordinator.ApproveTaskGraph(t.Context(), fixture.request)
	if err != nil || first.Replay {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	replay, err := fixture.coordinator.ApproveTaskGraph(t.Context(), fixture.request)
	if err != nil || !replay.Replay {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	assertCompleteTaskGraphApproval(t, fixture)
}

func assertNoTaskGraphApproval(t *testing.T, fixture *taskGraphFixture) {
	t.Helper()
	var state string
	var revision uint64
	var commandCount, taskCount, bindingCount, jobCount int
	err := fixture.intake.pool.QueryRow(t.Context(), `SELECT state,revision,
		(SELECT count(*) FROM workflow_commands WHERE command_id=$2),
		(SELECT count(*) FROM workflow_tasks WHERE run_id=$1),
		(SELECT count(*) FROM runtime_task_bindings WHERE run_id=$1),
		(SELECT count(*) FROM runtime_stage_jobs WHERE run_id=$1)
		FROM workflow_runs WHERE run_id=$1`, fixture.request.Command.ID,
		fixture.request.Command.Meta.CommandID).Scan(
		&state, &revision, &commandCount, &taskCount, &bindingCount, &jobCount)
	if err != nil {
		t.Fatal(err)
	}
	if state != string(workflow.RunStateTaskPlanReview) || revision != 4 || commandCount != 0 ||
		taskCount != 0 || bindingCount != 0 || jobCount != 0 {
		t.Fatalf("state=%s revision=%d commands=%d tasks=%d bindings=%d jobs=%d",
			state, revision, commandCount, taskCount, bindingCount, jobCount)
	}
	var graphURI *string
	if err := fixture.intake.pool.QueryRow(t.Context(), `SELECT approved_task_graph_uri
		FROM runtime_run_bindings WHERE run_id=$1`, fixture.request.Command.ID).Scan(&graphURI); err != nil {
		t.Fatal(err)
	}
	if graphURI != nil {
		t.Fatalf("approved task graph binding=%q after rollback", *graphURI)
	}
}

func assertCompleteTaskGraphApproval(t *testing.T, fixture *taskGraphFixture) {
	t.Helper()
	var state string
	var revision uint64
	var commandCount, taskCount, bindingCount, jobCount int
	err := fixture.intake.pool.QueryRow(t.Context(), `SELECT state,revision,
		(SELECT count(*) FROM workflow_commands WHERE command_id=$2),
		(SELECT count(*) FROM workflow_tasks WHERE run_id=$1),
		(SELECT count(*) FROM runtime_task_bindings WHERE run_id=$1),
		(SELECT count(*) FROM runtime_stage_jobs WHERE run_id=$1)
		FROM workflow_runs WHERE run_id=$1`, fixture.request.Command.ID,
		fixture.request.Command.Meta.CommandID).Scan(
		&state, &revision, &commandCount, &taskCount, &bindingCount, &jobCount)
	if err != nil {
		t.Fatal(err)
	}
	if state != string(workflow.RunStateReady) || revision != 5 || commandCount != 1 ||
		taskCount != 1 || bindingCount != 1 || jobCount != 1 {
		t.Fatalf("state=%s revision=%d commands=%d tasks=%d bindings=%d jobs=%d",
			state, revision, commandCount, taskCount, bindingCount, jobCount)
	}
	var graph, task workflow.ArtifactRef
	err = fixture.intake.pool.QueryRow(t.Context(), `SELECT
		r.approved_task_graph_uri,r.approved_task_graph_digest,
		t.approved_task_uri,t.approved_task_digest
		FROM runtime_run_bindings r JOIN runtime_task_bindings t ON t.run_id=r.run_id
		WHERE r.run_id=$1 AND t.task_id=$2`, fixture.request.Command.ID,
		fixture.request.Task.TaskID).Scan(&graph.URI, &graph.Digest, &task.URI, &task.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if graph != fixture.request.Graph || task != fixture.request.Task.ApprovedTask {
		t.Fatalf("graph=%+v want=%+v task=%+v want=%+v", graph, fixture.request.Graph,
			task, fixture.request.Task.ApprovedTask)
	}
	var jobID, jobTaskID, stage string
	var attempt uint32
	err = fixture.intake.pool.QueryRow(t.Context(), `SELECT
		job_id::text,task_id::text,attempt,stage FROM runtime_stage_jobs WHERE run_id=$1`,
		fixture.request.Command.ID).Scan(&jobID, &jobTaskID, &attempt, &stage)
	if err != nil {
		t.Fatal(err)
	}
	wantJob := fixture.request.StartJob
	if jobID != wantJob.ID || jobTaskID != *wantJob.TaskID ||
		attempt != wantJob.Attempt || stage != string(wantJob.Stage) {
		t.Fatalf("job id=%s task=%s attempt=%d stage=%s want=%+v",
			jobID, jobTaskID, attempt, stage, wantJob)
	}
}
