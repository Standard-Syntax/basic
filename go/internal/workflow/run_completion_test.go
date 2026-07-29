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

func TestRecordDraftPullRequestBindsPublicationWithoutLeavingMergeReady(t *testing.T) {
	run := readyRun(t)
	run.State = RunStateMergeReady
	run.CandidateCommit = "abcdefabcdefabcdefabcdefabcdefabcdefabcd"
	approval := artifact("artifact://run-approvals/publication", 'a')
	run.Approval = &approval
	publication := artifact("artifact://publications/1", 'b')
	command := RecordDraftPullRequest{
		Meta: envelope(ActorPublicationService, run.Revision), ID: run.ID,
		CandidateCommit: run.CandidateCommit, Approval: approval, Publication: publication,
	}
	next, events, err := run.Apply(command)
	if err != nil {
		t.Fatal(err)
	}
	if next.State != RunStateMergeReady || next.Revision != run.Revision+1 ||
		next.Publication == nil || !next.Publication.Equal(publication) ||
		len(events) != 1 || events[0].Type != "DRAFT_PULL_REQUEST_CREATED" {
		t.Fatalf("unexpected publication transition: %#v %#v", next, events)
	}

	for name, mutate := range map[string]func(*RecordDraftPullRequest){
		"actor": func(value *RecordDraftPullRequest) {
			value.Meta.Actor.Kind = ActorMergeService
		},
		"candidate": func(value *RecordDraftPullRequest) {
			value.CandidateCommit = "1111111111111111111111111111111111111111"
		},
		"approval": func(value *RecordDraftPullRequest) {
			value.Approval = artifact("artifact://run-approvals/other", 'c')
		},
	} {
		t.Run(name, func(t *testing.T) {
			invalid := command
			mutate(&invalid)
			before := run
			got, gotEvents, gotErr := run.Apply(invalid)
			if gotErr == nil || !reflect.DeepEqual(run, before) ||
				!reflect.DeepEqual(got, Run{}) || gotEvents != nil {
				t.Fatalf("invalid publication leaked output: %#v %#v %v", got, gotEvents, gotErr)
			}
		})
	}
}

func TestRecordDraftPullRequestRejectsRepeatedPublication(t *testing.T) {
	run := readyRun(t)
	run.State = RunStateMergeReady
	run.CandidateCommit = "abcdefabcdefabcdefabcdefabcdefabcdefabcd"
	approval := artifact("artifact://run-approvals/publication", 'a')
	run.Approval = &approval
	command := RecordDraftPullRequest{
		Meta: envelope(ActorPublicationService, run.Revision), ID: run.ID,
		CandidateCommit: run.CandidateCommit, Approval: approval,
		Publication: artifact("artifact://publications/1", 'b'),
	}
	published, _, err := run.Apply(command)
	if err != nil {
		t.Fatal(err)
	}

	repeated := command
	repeated.Meta = envelope(ActorPublicationService, published.Revision)
	before := published
	got, gotEvents, gotErr := published.Apply(repeated)
	if !errors.Is(gotErr, ErrInvalid) || !reflect.DeepEqual(published, before) ||
		!reflect.DeepEqual(got, Run{}) || gotEvents != nil {
		t.Fatalf("repeated publication leaked output: %#v %#v %v", got, gotEvents, gotErr)
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
