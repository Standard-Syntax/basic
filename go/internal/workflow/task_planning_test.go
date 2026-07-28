package workflow

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func planningReviewRun(t *testing.T) Run {
	t.Helper()
	run := createTestRun(t)
	var err error
	run, _, err = run.Apply(ProposeSpecification{
		Meta: envelope(ActorWorkflowService, run.Revision),
		ID:   run.ID, Specification: testSpec,
	})
	if err != nil {
		t.Fatal(err)
	}
	run, _, err = run.Apply(ApproveSpecification{
		Meta: envelope(ActorHuman, run.Revision), ID: run.ID, Specification: testSpec,
	})
	if err != nil {
		t.Fatal(err)
	}
	graph := ArtifactRef{
		URI:    "artifact://task-graphs/v1",
		Digest: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
	}
	run, _, err = run.Apply(ProposeTaskGraph{
		Meta: envelope(ActorWorkflowService, run.Revision), ID: run.ID, TaskGraph: graph,
	})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func TestTaskGraphApprovalCreatesReadyRootsAndPendingDependents(t *testing.T) {
	run := planningReviewRun(t)
	rootID, dependentID := uuid.NewString(), uuid.NewString()
	decision, err := run.ApproveTaskGraph(ApproveTaskGraph{
		Meta: envelope(ActorHuman, run.Revision), ID: run.ID, TaskGraph: *run.TaskGraph,
		Tasks: []TaskDefinition{
			{ID: rootID, MaxAttempts: 2},
			{ID: dependentID, MaxAttempts: 3},
		},
		Dependencies: []TaskDependency{{TaskID: dependentID, DependsOnID: rootID}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Run.State != RunStateReady || len(decision.Tasks) != 2 ||
		decision.Tasks[0].State != TaskStateReady ||
		decision.Tasks[1].State != TaskStatePending {
		t.Fatalf("unexpected decision: %#v", decision)
	}
	if len(decision.Events) != 3 || decision.Events[0].Type != "TASK_GRAPH_APPROVED" ||
		decision.Events[1].Type != "TASK_READY" ||
		decision.Events[2].Type != "TASK_CREATED" {
		t.Fatalf("unexpected events: %#v", decision.Events)
	}
}

func TestTaskGraphApprovalRejectsCyclesWithoutPartialOutput(t *testing.T) {
	run := planningReviewRun(t)
	first, second := uuid.NewString(), uuid.NewString()
	decision, err := run.ApproveTaskGraph(ApproveTaskGraph{
		Meta: envelope(ActorHuman, run.Revision), ID: run.ID, TaskGraph: *run.TaskGraph,
		Tasks: []TaskDefinition{{ID: first, MaxAttempts: 1}, {ID: second, MaxAttempts: 1}},
		Dependencies: []TaskDependency{
			{TaskID: first, DependsOnID: second},
			{TaskID: second, DependsOnID: first},
		},
	})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("error = %v", err)
	}
	if decision.Run.ID != "" || decision.Tasks != nil || decision.Events != nil {
		t.Fatalf("partial decision: %#v", decision)
	}
}

func TestTaskGraphRejectionRequiresHumanAndBinding(t *testing.T) {
	run := planningReviewRun(t)
	for _, kind := range []ActorKind{ActorModel, ActorPython, ActorWorkflowService} {
		next, events, err := run.Apply(RejectTaskGraph{
			Meta: envelope(kind, run.Revision), ID: run.ID,
			TaskGraph: *run.TaskGraph, Reason: "needs work",
		})
		if !errors.Is(err, ErrUnauthorized) || next.ID != "" || events != nil {
			t.Fatalf("kind %s: %#v %#v %v", kind, next, events, err)
		}
	}
	next, events, err := run.Apply(RejectTaskGraph{
		Meta: envelope(ActorHuman, run.Revision), ID: run.ID,
		TaskGraph: *run.TaskGraph, Reason: "needs work",
	})
	if err != nil || next.State != RunStateTaskPlanning ||
		len(events) != 1 || events[0].Type != "TASK_GRAPH_REJECTED" {
		t.Fatalf("unexpected rejection: %#v %#v %v", next, events, err)
	}
}
