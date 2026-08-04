//go:build integration

package controlapi

import (
	"errors"
	"testing"
	"time"

	"github.com/Standard-Syntax/basic/go/internal/orchestration"
	"github.com/Standard-Syntax/basic/go/internal/runtime"
	"github.com/Standard-Syntax/basic/go/internal/workflow"
	"github.com/google/uuid"
)

func TestSpecificationApprovalRollsBackEveryWriteAndReplaysExactly(t *testing.T) {
	for _, point := range []SpecificationApprovalFaultPoint{
		FaultSpecificationAfterWorkflow, FaultSpecificationAfterBinding,
		FaultSpecificationAfterEnqueue,
	} {
		t.Run(string(point), func(t *testing.T) {
			intake := newIntakeFixture(t)
			if _, err := intake.coordinator.Accept(t.Context(), intake.request); err != nil {
				t.Fatal(err)
			}
			store := workflow.NewStore(intake.pool)
			ref, err := intake.artifacts.Put(t.Context(), []byte("live specification proposal"))
			if err != nil {
				t.Fatal(err)
			}
			runID := intake.request.Command.ID
			at := intake.request.Command.Meta.Timestamp.Add(time.Second)
			if _, err := store.ExecuteRun(t.Context(), workflow.ProposeSpecification{
				Meta: workflow.CommandEnvelope{CommandID: uuid.NewString(),
					Actor:            workflow.Actor{ID: uuid.NewString(), Kind: workflow.ActorWorkflowService},
					ExpectedRevision: 1, Timestamp: at, CorrelationID: uuid.NewString(), CausationID: uuid.NewString()},
				ID: runID, Specification: ref,
			}); err != nil {
				t.Fatal(err)
			}
			command := workflow.ApproveSpecification{Meta: workflow.CommandEnvelope{
				CommandID: uuid.NewString(), Actor: workflow.Actor{ID: uuid.NewString(), Kind: workflow.ActorHuman},
				ExpectedRevision: 2, Timestamp: at.Add(time.Second), CorrelationID: uuid.NewString(),
				CausationID: uuid.NewString()}, ID: runID, Specification: ref}
			request := SpecificationApprovalRequest{Command: command, PlanningJob: runtime.Job{
				ID:    orchestration.StableID(runID, "-", "1", orchestration.StagePlanningReasoning, "job"),
				RunID: runID, Attempt: 1, Stage: orchestration.StagePlanningReasoning,
				AvailableAt: command.Meta.Timestamp,
			}}
			coordinator, err := NewSpecificationApprovalCoordinator(intake.pool, store)
			if err != nil {
				t.Fatal(err)
			}
			coordinator.inject = func(actual SpecificationApprovalFaultPoint) error {
				if actual == point {
					return errors.New("injected specification approval failure")
				}
				return nil
			}
			if _, err := coordinator.ApproveSpecification(t.Context(), request); err == nil {
				t.Fatal("injection succeeded")
			}
			assertSpecificationApproval(t, intake, request, false)
			coordinator.inject = nil
			first, err := coordinator.ApproveSpecification(t.Context(), request)
			if err != nil || first.Replay {
				t.Fatalf("first=%+v err=%v", first, err)
			}
			replay, err := coordinator.ApproveSpecification(t.Context(), request)
			if err != nil || !replay.Replay {
				t.Fatalf("replay=%+v err=%v", replay, err)
			}
			assertSpecificationApproval(t, intake, request, true)
		})
	}
}

func assertSpecificationApproval(t *testing.T, intake *intakeFixture,
	request SpecificationApprovalRequest, complete bool) {
	t.Helper()
	var state string
	var revision uint64
	var commandCount, planningJobs int
	var approvedURI *string
	err := intake.pool.QueryRow(t.Context(), `SELECT r.state,r.revision,
		(SELECT count(*) FROM workflow_commands WHERE command_id=$2),
		(SELECT count(*) FROM runtime_stage_jobs WHERE run_id=$1 AND stage=$3),
		b.approved_specification_uri FROM workflow_runs r
		JOIN runtime_run_bindings b ON b.run_id=r.run_id WHERE r.run_id=$1`,
		request.Command.ID, request.Command.Meta.CommandID,
		orchestration.StagePlanningReasoning).Scan(&state, &revision, &commandCount, &planningJobs, &approvedURI)
	if err != nil {
		t.Fatal(err)
	}
	if complete {
		if state != string(workflow.RunStateTaskPlanning) || revision != 3 || commandCount != 1 ||
			planningJobs != 1 || approvedURI == nil || *approvedURI != request.Command.Specification.URI {
			t.Fatalf("complete state=%s rev=%d commands=%d jobs=%d binding=%v", state, revision, commandCount, planningJobs, approvedURI)
		}
	} else if state != string(workflow.RunStateSpecificationReview) || revision != 2 ||
		commandCount != 0 || planningJobs != 0 || approvedURI != nil {
		t.Fatalf("rollback state=%s rev=%d commands=%d jobs=%d binding=%v", state, revision, commandCount, planningJobs, approvedURI)
	}
}
