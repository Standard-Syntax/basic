package workflow

import (
	"errors"
	"reflect"
	"testing"
)

func readyRun(t *testing.T) Run {
	t.Helper()
	run := planningReviewRun(t)
	decision, err := run.ApproveTaskGraph(ApproveTaskGraph{
		Meta: envelope(ActorHuman, run.Revision), ID: run.ID, TaskGraph: *run.TaskGraph,
		Tasks: []TaskDefinition{{ID: readyTask().ID, MaxAttempts: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return decision.Run
}

func TestCompleteRunLifecycleRecordsExternalMergeFact(t *testing.T) {
	run := readyRun(t)
	execution := artifact("artifact://run-executions/1", 'a')
	verification := artifact("artifact://run-verifications/1", 'b')
	review := artifact("artifact://run-reviews/1", 'c')
	approval := artifact("artifact://run-approvals/1", 'd')
	merge := artifact("artifact://merges/1", 'e')
	commit := "abcdefabcdefabcdefabcdefabcdefabcdefabcd"
	steps := []struct {
		state   RunState
		command func(Run) RunCommand
	}{
		{RunStateExecuting, func(r Run) RunCommand {
			return StartRun{Meta: envelope(ActorWorkflowService, r.Revision), ID: r.ID}
		}},
		{RunStateVerifying, func(r Run) RunCommand {
			return RecordRunExecution{
				Meta: envelope(ActorExecutionService, r.Revision), ID: r.ID,
				Execution: execution, CandidateCommit: commit,
			}
		}},
		{RunStateReviewing, func(r Run) RunCommand {
			return RecordRunVerification{
				Meta: envelope(ActorVerificationService, r.Revision), ID: r.ID,
				CandidateCommit: commit, Evidence: verification,
			}
		}},
		{RunStateAwaitingApproval, func(r Run) RunCommand {
			return RecordRunReview{
				Meta: envelope(ActorReviewService, r.Revision), ID: r.ID,
				CandidateCommit: commit, Review: review,
			}
		}},
		{RunStateMergeReady, func(r Run) RunCommand {
			return ApproveRun{
				Meta: envelope(ActorHuman, r.Revision), ID: r.ID,
				CandidateCommit: commit, Review: review, Approval: approval,
			}
		}},
		{RunStateMerged, func(r Run) RunCommand {
			return RecordMerge{
				Meta: envelope(ActorMergeService, r.Revision), ID: r.ID,
				CandidateCommit: commit, Approval: approval, Merge: merge,
			}
		}},
	}
	for _, step := range steps {
		next, events, err := run.Apply(step.command(run))
		if err != nil {
			t.Fatalf("%s -> %s: %v", run.State, step.state, err)
		}
		if next.State != step.state || len(events) != 1 {
			t.Fatalf("unexpected result: %#v %#v", next, events)
		}
		run = next
	}
	if run.Merge == nil || !run.Merge.Equal(merge) {
		t.Fatal("merge fact was not bound")
	}
}

func TestRunCancellationRequiresHuman(t *testing.T) {
	run := readyRun(t)
	_, _, err := run.Apply(CancelRun{
		Meta: envelope(ActorWorkflowService, run.Revision), ID: run.ID, Reason: "stop",
	})
	if err != ErrUnauthorized {
		t.Fatalf("error = %v", err)
	}
	next, events, err := run.Apply(CancelRun{
		Meta: envelope(ActorHuman, run.Revision), ID: run.ID, Reason: "stop",
	})
	if err != nil || next.State != RunStateCancelled || events[0].Type != "RUN_CANCELLED" {
		t.Fatalf("unexpected cancellation: %#v %#v %v", next, events, err)
	}
}

func TestTerminalRunsRejectFailureAndCancellation(t *testing.T) {
	for _, state := range []RunState{
		RunStateMerged, RunStateRejected, RunStateFailed, RunStateCancelled,
	} {
		for _, command := range []func(Run) RunCommand{
			func(run Run) RunCommand {
				return FailRun{
					Meta: envelope(ActorWorkflowService, run.Revision), ID: run.ID,
					Evidence: artifact("artifact://run-failures/terminal", 'f'), Reason: "failed",
				}
			},
			func(run Run) RunCommand {
				return CancelRun{
					Meta: envelope(ActorHuman, run.Revision), ID: run.ID, Reason: "cancel",
				}
			},
		} {
			run := readyRun(t)
			run.State = state
			before := run
			next, events, err := run.Apply(command(run))
			if !errors.Is(err, ErrInvalidTransition) {
				t.Fatalf("%s: error = %v", state, err)
			}
			if !reflect.DeepEqual(run, before) ||
				!reflect.DeepEqual(next, Run{}) || events != nil {
				t.Fatalf("%s leaked output: %#v %#v", state, next, events)
			}
		}
	}
}
