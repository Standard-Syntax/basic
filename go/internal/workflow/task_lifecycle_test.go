package workflow

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
)

func readyTask() Task {
	return Task{
		ID: uuid.NewString(), RunID: uuid.NewString(), State: TaskStateReady,
		Revision: 1, MaxAttempts: 2, CreatedAt: testTime, UpdatedAt: testTime,
	}
}

func artifact(uri string, letter byte) ArtifactRef {
	digest := make([]byte, 64)
	for index := range digest {
		digest[index] = letter
	}
	return ArtifactRef{URI: uri, Digest: string(digest)}
}

func TestTaskCommandDigestIncludesConcreteType(t *testing.T) {
	meta := envelope(ActorWorkflowService, 1)
	runID, taskID := uuid.NewString(), uuid.NewString()
	lease := LeaseRef{
		ID: uuid.NewString(), OwnerID: uuid.NewString(), ExpiresAt: testTime.Add(time.Hour),
	}
	leaseDigest, err := taskCommandDigest(LeaseTask{
		Meta: meta, Run: runID, ID: taskID, Lease: lease,
	})
	if err != nil {
		t.Fatal(err)
	}
	releaseDigest, err := taskCommandDigest(ReleaseTaskLease{
		Meta: meta, Run: runID, ID: taskID, Lease: lease,
	})
	if err != nil {
		t.Fatal(err)
	}
	if leaseDigest == releaseDigest {
		t.Fatal("same-shaped task commands produced the same digest")
	}
}

func TestCompleteTaskLifecycle(t *testing.T) {
	task := readyTask()
	lease := LeaseRef{
		ID: uuid.NewString(), OwnerID: uuid.NewString(), ExpiresAt: testTime.Add(time.Hour),
	}
	proposal := artifact("artifact://proposals/1", 'a')
	execution := artifact("artifact://executions/1", 'b')
	verification := artifact("artifact://verifications/1", 'c')
	review := artifact("artifact://reviews/1", 'd')
	approval := artifact("artifact://approvals/1", 'e')
	commit := "0123456789012345678901234567890123456789"

	steps := []struct {
		state   TaskState
		command func(Task) TaskCommand
	}{
		{TaskStateLeased, func(t Task) TaskCommand {
			return LeaseTask{Meta: envelope(ActorWorkflowService, t.Revision), Run: t.RunID, ID: t.ID, Lease: lease}
		}},
		{TaskStateReasoning, func(t Task) TaskCommand {
			return StartReasoning{Meta: envelope(ActorReasoningService, t.Revision), Run: t.RunID, ID: t.ID, Lease: lease}
		}},
		{TaskStateExecuting, func(t Task) TaskCommand {
			return AcceptTaskProposal{Meta: envelope(ActorExecutionService, t.Revision), Run: t.RunID, ID: t.ID, Proposal: proposal}
		}},
		{TaskStateVerifying, func(t Task) TaskCommand {
			return RecordTaskExecution{
				Meta: envelope(ActorExecutionService, t.Revision), Run: t.RunID, ID: t.ID,
				Proposal: proposal, Execution: execution, CandidateCommit: commit,
			}
		}},
		{TaskStateReviewing, func(t Task) TaskCommand {
			return RecordTaskVerification{
				Meta: envelope(ActorVerificationService, t.Revision), Run: t.RunID, ID: t.ID,
				CandidateCommit: commit, Evidence: verification, Passed: true,
			}
		}},
		{TaskStateAwaitingApproval, func(t Task) TaskCommand {
			return RecordTaskReview{
				Meta: envelope(ActorReviewService, t.Revision), Run: t.RunID, ID: t.ID,
				CandidateCommit: commit, Review: review, Passed: true,
			}
		}},
		{TaskStateAccepted, func(t Task) TaskCommand {
			return ApproveTask{
				Meta: envelope(ActorHuman, t.Revision), Run: t.RunID, ID: t.ID,
				CandidateCommit: commit, Review: review, Approval: approval,
			}
		}},
	}
	for _, step := range steps {
		next, events, err := task.Apply(step.command(task))
		if err != nil {
			t.Fatalf("%s -> %s: %v", task.State, step.state, err)
		}
		if next.State != step.state || len(events) != 1 {
			t.Fatalf("unexpected transition: %#v %#v", next, events)
		}
		task = next
	}
}

func TestTaskRetriesAndAttemptExhaustion(t *testing.T) {
	task := readyTask()
	task.MaxAttempts = 1
	lease := LeaseRef{
		ID: uuid.NewString(), OwnerID: uuid.NewString(), ExpiresAt: testTime.Add(time.Hour),
	}
	var err error
	task, _, err = task.Apply(LeaseTask{
		Meta: envelope(ActorWorkflowService, task.Revision), Run: task.RunID, ID: task.ID, Lease: lease,
	})
	if err != nil {
		t.Fatal(err)
	}
	task, _, err = task.Apply(StartReasoning{
		Meta: envelope(ActorReasoningService, task.Revision), Run: task.RunID, ID: task.ID, Lease: lease,
	})
	if err != nil {
		t.Fatal(err)
	}
	task, _, err = task.Apply(RejectTaskProposal{
		Meta: envelope(ActorHuman, task.Revision), Run: task.RunID, ID: task.ID,
		Proposal: artifact("artifact://proposals/rejected", 'f'), Reason: "unsafe",
	})
	if err != nil {
		t.Fatal(err)
	}
	task, events, err := task.Apply(RetryTask{
		Meta: envelope(ActorWorkflowService, task.Revision), Run: task.RunID, ID: task.ID,
	})
	if err != nil || task.State != TaskStateFailed ||
		events[0].Type != "TASK_ATTEMPTS_EXHAUSTED" {
		t.Fatalf("unexpected exhaustion: %#v %#v %v", task, events, err)
	}
}

func TestTaskInvalidTransitionsLeaveByteEquivalentSnapshot(t *testing.T) {
	task := readyTask()
	commands := []TaskCommand{
		StartReasoning{
			Meta: envelope(ActorReasoningService, task.Revision), Run: task.RunID, ID: task.ID,
			Lease: LeaseRef{ID: uuid.NewString(), OwnerID: uuid.NewString(), ExpiresAt: testTime.Add(time.Hour)},
		},
		LeaseTask{
			Meta: envelope(ActorModel, task.Revision), Run: task.RunID, ID: task.ID,
			Lease: LeaseRef{ID: uuid.NewString(), OwnerID: uuid.NewString(), ExpiresAt: testTime.Add(time.Hour)},
		},
		LeaseTask{
			Meta: envelope(ActorWorkflowService, task.Revision-1), Run: task.RunID, ID: task.ID,
			Lease: LeaseRef{ID: uuid.NewString(), OwnerID: uuid.NewString(), ExpiresAt: testTime.Add(time.Hour)},
		},
	}
	for _, command := range commands {
		before := task
		next, events, err := task.Apply(command)
		if err == nil {
			t.Fatalf("command unexpectedly succeeded: %T", command)
		}
		if !reflect.DeepEqual(task, before) || !reflect.DeepEqual(next, Task{}) || events != nil {
			t.Fatalf("partial mutation for %T", command)
		}
	}
}

func TestTerminalTaskIsImmutable(t *testing.T) {
	for _, state := range []TaskState{TaskStateAccepted, TaskStateFailed, TaskStateCancelled} {
		task := readyTask()
		task.State = state
		_, events, err := task.Apply(FailTask{
			Meta: envelope(ActorWorkflowService, task.Revision), Run: task.RunID, ID: task.ID,
			Evidence: artifact("artifact://failures/1", 'a'), Reason: "failure",
		})
		if !errors.Is(err, ErrInvalidTransition) || events != nil {
			t.Fatalf("%s was mutable: %v %#v", state, err, events)
		}
	}
}
