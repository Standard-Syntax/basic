package workflow

import (
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
)

func matrixRun(state RunState) Run {
	specification := artifact("artifact://matrix/specification", 'a')
	graph := artifact("artifact://matrix/graph", 'b')
	execution := artifact("artifact://matrix/execution", 'c')
	verification := artifact("artifact://matrix/verification", 'd')
	review := artifact("artifact://matrix/review", 'e')
	approval := artifact("artifact://matrix/approval", 'f')
	return Run{
		ID: uuid.NewString(), State: state, Revision: 10,
		Specification: &specification, TaskGraph: &graph, Execution: &execution,
		CandidateCommit: "0123456789012345678901234567890123456789",
		Verification:    &verification, Review: &review, Approval: &approval,
		CreatedAt: testTime, UpdatedAt: testTime,
	}
}

func TestRunCommandStateMatrix(t *testing.T) {
	states := []RunState{
		RunStateDraft, RunStateSpecificationReview, RunStateTaskPlanning,
		RunStateTaskPlanReview, RunStateReady, RunStateExecuting,
		RunStateVerifying, RunStateReviewing, RunStateAwaitingApproval,
		RunStateMergeReady, RunStateMerged, RunStateRejected, RunStateFailed,
		RunStateCancelled,
	}
	type runCase struct {
		name      string
		allowed   map[RunState]bool
		nextState RunState
		eventType string
		command   func(Run) RunCommand
	}
	cases := []runCase{
		{"propose specification", setRunStates(RunStateDraft),
			RunStateSpecificationReview, "SPECIFICATION_PROPOSED", func(r Run) RunCommand {
				return ProposeSpecification{
					Meta: envelope(ActorWorkflowService, r.Revision), ID: r.ID,
					Specification: *r.Specification,
				}
			}},
		{"reject specification", setRunStates(RunStateSpecificationReview),
			RunStateDraft, "SPECIFICATION_REJECTED", func(r Run) RunCommand {
				return RejectSpecification{
					Meta: envelope(ActorHuman, r.Revision), ID: r.ID,
					Specification: *r.Specification, Reason: "revise",
				}
			}},
		{"terminal reject specification", setRunStates(RunStateSpecificationReview),
			RunStateRejected, "SPECIFICATION_REJECTED", func(r Run) RunCommand {
				return RejectSpecification{
					Meta: envelope(ActorHuman, r.Revision), ID: r.ID,
					Specification: *r.Specification, Reason: "reject", Terminal: true,
				}
			}},
		{"approve specification", setRunStates(RunStateSpecificationReview),
			RunStateTaskPlanning, "SPECIFICATION_APPROVED", func(r Run) RunCommand {
				return ApproveSpecification{
					Meta: envelope(ActorHuman, r.Revision), ID: r.ID,
					Specification: *r.Specification,
				}
			}},
		{"propose task graph", setRunStates(RunStateTaskPlanning),
			RunStateTaskPlanReview, "TASK_GRAPH_PROPOSED", func(r Run) RunCommand {
				return ProposeTaskGraph{
					Meta: envelope(ActorWorkflowService, r.Revision), ID: r.ID,
					TaskGraph: *r.TaskGraph,
				}
			}},
		{"reject task graph", setRunStates(RunStateTaskPlanReview),
			RunStateTaskPlanning, "TASK_GRAPH_REJECTED", func(r Run) RunCommand {
				return RejectTaskGraph{
					Meta: envelope(ActorHuman, r.Revision), ID: r.ID,
					TaskGraph: *r.TaskGraph, Reason: "revise",
				}
			}},
		{"terminal reject task graph", setRunStates(RunStateTaskPlanReview),
			RunStateRejected, "TASK_GRAPH_REJECTED", func(r Run) RunCommand {
				return RejectTaskGraph{
					Meta: envelope(ActorHuman, r.Revision), ID: r.ID,
					TaskGraph: *r.TaskGraph, Reason: "reject", Terminal: true,
				}
			}},
		{"start run", setRunStates(RunStateReady),
			RunStateExecuting, "RUN_EXECUTION_STARTED", func(r Run) RunCommand {
				return StartRun{Meta: envelope(ActorWorkflowService, r.Revision), ID: r.ID}
			}},
		{"record execution", setRunStates(RunStateExecuting),
			RunStateVerifying, "RUN_EXECUTED", func(r Run) RunCommand {
				return RecordRunExecution{
					Meta: envelope(ActorExecutionService, r.Revision), ID: r.ID,
					Execution: *r.Execution, CandidateCommit: r.CandidateCommit,
				}
			}},
		{"record verification", setRunStates(RunStateVerifying),
			RunStateReviewing, "RUN_VERIFIED", func(r Run) RunCommand {
				return RecordRunVerification{
					Meta: envelope(ActorVerificationService, r.Revision), ID: r.ID,
					Evidence: *r.Verification, CandidateCommit: r.CandidateCommit,
				}
			}},
		{"record review", setRunStates(RunStateReviewing),
			RunStateAwaitingApproval, "RUN_REVIEWED", func(r Run) RunCommand {
				return RecordRunReview{
					Meta: envelope(ActorReviewService, r.Revision), ID: r.ID,
					Review: *r.Review, CandidateCommit: r.CandidateCommit,
				}
			}},
		{"approve run", setRunStates(RunStateAwaitingApproval),
			RunStateMergeReady, "RUN_APPROVED", func(r Run) RunCommand {
				return ApproveRun{
					Meta: envelope(ActorHuman, r.Revision), ID: r.ID,
					Review: *r.Review, Approval: *r.Approval, CandidateCommit: r.CandidateCommit,
				}
			}},
		{"reject run", setRunStates(RunStateAwaitingApproval),
			RunStateRejected, "RUN_REJECTED", func(r Run) RunCommand {
				return RejectRun{
					Meta: envelope(ActorHuman, r.Revision), ID: r.ID,
					Review: *r.Review, CandidateCommit: r.CandidateCommit, Reason: "reject",
				}
			}},
		{"record merge", setRunStates(RunStateMergeReady),
			RunStateMerged, "RUN_MERGED", func(r Run) RunCommand {
				return RecordMerge{
					Meta: envelope(ActorMergeService, r.Revision), ID: r.ID,
					Approval: *r.Approval, CandidateCommit: r.CandidateCommit,
					Merge: artifact("artifact://matrix/merge", '1'),
				}
			}},
		{"fail run", nonterminalRunStates(),
			RunStateFailed, "RUN_FAILED", func(r Run) RunCommand {
				return FailRun{
					Meta: envelope(ActorWorkflowService, r.Revision), ID: r.ID,
					Evidence: artifact("artifact://matrix/failure", '2'), Reason: "failed",
				}
			}},
		{"cancel run", nonterminalRunStates(),
			RunStateCancelled, "RUN_CANCELLED", func(r Run) RunCommand {
				return CancelRun{Meta: envelope(ActorHuman, r.Revision), ID: r.ID, Reason: "cancel"}
			}},
	}
	for _, test := range cases {
		for _, state := range states {
			t.Run(test.name+"/"+string(state), func(t *testing.T) {
				run := matrixRun(state)
				before := run
				next, events, err := run.Apply(test.command(run))
				if test.allowed[state] {
					if err != nil || len(events) != 1 || next.Revision != run.Revision+1 ||
						next.State != test.nextState || events[0].Type != test.eventType {
						t.Fatalf("allowed transition failed: %#v %#v %v", next, events, err)
					}
					return
				}
				if err == nil || !reflect.DeepEqual(run, before) ||
					!reflect.DeepEqual(next, Run{}) || events != nil {
					t.Fatalf("invalid edge leaked output: %#v %#v %v", next, events, err)
				}
			})
		}
	}
}

func TestTaskGraphApprovalStateMatrix(t *testing.T) {
	for _, state := range []RunState{
		RunStateDraft, RunStateSpecificationReview, RunStateTaskPlanning,
		RunStateTaskPlanReview, RunStateReady, RunStateExecuting,
		RunStateVerifying, RunStateReviewing, RunStateAwaitingApproval,
		RunStateMergeReady, RunStateMerged, RunStateRejected, RunStateFailed,
		RunStateCancelled,
	} {
		run := matrixRun(state)
		decision, err := run.ApproveTaskGraph(ApproveTaskGraph{
			Meta: envelope(ActorHuman, run.Revision), ID: run.ID, TaskGraph: *run.TaskGraph,
			Tasks: []TaskDefinition{{ID: uuid.NewString(), MaxAttempts: 1}},
		})
		if state == RunStateTaskPlanReview {
			if err != nil || decision.Run.State != RunStateReady {
				t.Fatalf("approval failed: %#v %v", decision, err)
			}
		} else if err == nil || !reflect.DeepEqual(decision, PlanningDecision{}) {
			t.Fatalf("%s accepted graph approval: %#v %v", state, decision, err)
		}
	}
}

func matrixTask(state TaskState) Task {
	lease := LeaseRef{
		ID: uuid.NewString(), OwnerID: uuid.NewString(), ExpiresAt: testTime.Add(time.Hour),
	}
	proposal := artifact("artifact://matrix/proposal", 'a')
	execution := artifact("artifact://matrix/task-execution", 'b')
	verification := artifact("artifact://matrix/task-verification", 'c')
	review := artifact("artifact://matrix/task-review", 'd')
	return Task{
		ID: uuid.NewString(), RunID: uuid.NewString(), State: state, Revision: 10,
		MaxAttempts: 3, CurrentAttempt: 1, Lease: &lease, Proposal: &proposal,
		Execution: &execution, CandidateCommit: "0123456789012345678901234567890123456789",
		Verification: &verification, Review: &review,
		CreatedAt: testTime, UpdatedAt: testTime,
	}
}

func TestTaskCommandStateMatrix(t *testing.T) {
	states := []TaskState{
		TaskStatePending, TaskStateReady, TaskStateLeased, TaskStateReasoning,
		TaskStateProposalRejected, TaskStateExecuting, TaskStateVerifying,
		TaskStateReviewing, TaskStateReworkRequired, TaskStateAwaitingApproval,
		TaskStateAccepted, TaskStateFailed, TaskStateCancelled,
	}
	type taskCase struct {
		name         string
		allowed      map[TaskState]bool
		nextState    TaskState
		eventType    string
		attemptDelta uint32
		command      func(Task) TaskCommand
	}
	cases := []taskCase{
		{"lease", setTaskStates(TaskStateReady), TaskStateLeased, "TASK_LEASED", 1,
			func(v Task) TaskCommand {
				return LeaseTask{Meta: envelope(ActorWorkflowService, v.Revision), Run: v.RunID, ID: v.ID, Lease: *v.Lease}
			}},
		{"release lease", setTaskStates(TaskStateLeased), TaskStateReady, "TASK_LEASE_RELEASED", 0,
			func(v Task) TaskCommand {
				return ReleaseTaskLease{Meta: envelope(ActorWorkflowService, v.Revision), Run: v.RunID, ID: v.ID, Lease: *v.Lease}
			}},
		{"reason", setTaskStates(TaskStateLeased), TaskStateReasoning, "TASK_REASONING_STARTED", 0,
			func(v Task) TaskCommand {
				return StartReasoning{Meta: envelope(ActorReasoningService, v.Revision), Run: v.RunID, ID: v.ID, Lease: *v.Lease}
			}},
		{"reject proposal", setTaskStates(TaskStateReasoning),
			TaskStateProposalRejected, "TASK_PROPOSAL_REJECTED", 0, func(v Task) TaskCommand {
				return RejectTaskProposal{Meta: envelope(ActorHuman, v.Revision), Run: v.RunID, ID: v.ID, Proposal: *v.Proposal, Reason: "reject"}
			}},
		{"accept proposal", setTaskStates(TaskStateReasoning),
			TaskStateExecuting, "TASK_EXECUTION_STARTED", 0, func(v Task) TaskCommand {
				return AcceptTaskProposal{Meta: envelope(ActorExecutionService, v.Revision), Run: v.RunID, ID: v.ID, Proposal: *v.Proposal}
			}},
		{"execute", setTaskStates(TaskStateExecuting),
			TaskStateVerifying, "TASK_EXECUTED", 0, func(v Task) TaskCommand {
				return RecordTaskExecution{Meta: envelope(ActorExecutionService, v.Revision), Run: v.RunID, ID: v.ID, Proposal: *v.Proposal, Execution: *v.Execution, CandidateCommit: v.CandidateCommit}
			}},
		{"verification pass", setTaskStates(TaskStateVerifying),
			TaskStateReviewing, "TASK_VERIFIED", 0, func(v Task) TaskCommand {
				return RecordTaskVerification{Meta: envelope(ActorVerificationService, v.Revision), Run: v.RunID, ID: v.ID, CandidateCommit: v.CandidateCommit, Evidence: *v.Verification, Passed: true}
			}},
		{"verification fail", setTaskStates(TaskStateVerifying),
			TaskStateReworkRequired, "TASK_VERIFICATION_FAILED", 0, func(v Task) TaskCommand {
				return RecordTaskVerification{Meta: envelope(ActorVerificationService, v.Revision), Run: v.RunID, ID: v.ID, CandidateCommit: v.CandidateCommit, Evidence: *v.Verification}
			}},
		{"review pass", setTaskStates(TaskStateReviewing),
			TaskStateAwaitingApproval, "TASK_REVIEWED", 0, func(v Task) TaskCommand {
				return RecordTaskReview{Meta: envelope(ActorReviewService, v.Revision), Run: v.RunID, ID: v.ID, CandidateCommit: v.CandidateCommit, Review: *v.Review, Passed: true}
			}},
		{"review fail", setTaskStates(TaskStateReviewing),
			TaskStateReworkRequired, "TASK_REVIEW_FAILED", 0, func(v Task) TaskCommand {
				return RecordTaskReview{Meta: envelope(ActorReviewService, v.Revision), Run: v.RunID, ID: v.ID, CandidateCommit: v.CandidateCommit, Review: *v.Review}
			}},
		{"approve", setTaskStates(TaskStateAwaitingApproval),
			TaskStateAccepted, "TASK_ACCEPTED", 0, func(v Task) TaskCommand {
				return ApproveTask{Meta: envelope(ActorHuman, v.Revision), Run: v.RunID, ID: v.ID, CandidateCommit: v.CandidateCommit, Review: *v.Review, Approval: artifact("artifact://matrix/task-approval", 'e')}
			}},
		{"rework", setTaskStates(TaskStateAwaitingApproval),
			TaskStateReworkRequired, "TASK_REWORK_REQUIRED", 0, func(v Task) TaskCommand {
				return RequireTaskRework{Meta: envelope(ActorHuman, v.Revision), Run: v.RunID, ID: v.ID, CandidateCommit: v.CandidateCommit, Review: *v.Review, Reason: "rework"}
			}},
		{"retry", setTaskStates(TaskStateProposalRejected, TaskStateReworkRequired),
			TaskStateReady, "TASK_RETRY_READY", 0, func(v Task) TaskCommand {
				return RetryTask{Meta: envelope(ActorWorkflowService, v.Revision), Run: v.RunID, ID: v.ID}
			}},
		{"fail", nonterminalTaskStates(),
			TaskStateFailed, "TASK_FAILED", 0, func(v Task) TaskCommand {
				return FailTask{Meta: envelope(ActorWorkflowService, v.Revision), Run: v.RunID, ID: v.ID, Evidence: artifact("artifact://matrix/task-failure", 'f'), Reason: "failed"}
			}},
		{"cancel", nonterminalTaskStates(),
			TaskStateCancelled, "TASK_CANCELLED", 0, func(v Task) TaskCommand {
				return CancelTask{Meta: envelope(ActorWorkflowService, v.Revision), Run: v.RunID, ID: v.ID, Reason: "cancel"}
			}},
	}
	for _, test := range cases {
		for _, state := range states {
			t.Run(test.name+"/"+string(state), func(t *testing.T) {
				task := matrixTask(state)
				before := task
				next, events, err := task.Apply(test.command(task))
				if test.allowed[state] {
					if err != nil || len(events) != 1 || next.Revision != task.Revision+1 ||
						next.State != test.nextState || events[0].Type != test.eventType ||
						next.CurrentAttempt != task.CurrentAttempt+test.attemptDelta {
						t.Fatalf("allowed transition failed: %#v %#v %v", next, events, err)
					}
					return
				}
				if err == nil || !reflect.DeepEqual(task, before) ||
					!reflect.DeepEqual(next, Task{}) || events != nil {
					t.Fatalf("invalid edge leaked output: %#v %#v %v", next, events, err)
				}
			})
		}
	}
}

func setRunStates(states ...RunState) map[RunState]bool {
	result := make(map[RunState]bool, len(states))
	for _, state := range states {
		result[state] = true
	}
	return result
}

func nonterminalRunStates() map[RunState]bool {
	return setRunStates(
		RunStateDraft, RunStateSpecificationReview, RunStateTaskPlanning,
		RunStateTaskPlanReview, RunStateReady, RunStateExecuting,
		RunStateVerifying, RunStateReviewing, RunStateAwaitingApproval,
		RunStateMergeReady,
	)
}

func setTaskStates(states ...TaskState) map[TaskState]bool {
	result := make(map[TaskState]bool, len(states))
	for _, state := range states {
		result[state] = true
	}
	return result
}

func nonterminalTaskStates() map[TaskState]bool {
	return setTaskStates(
		TaskStatePending, TaskStateReady, TaskStateLeased, TaskStateReasoning,
		TaskStateProposalRejected, TaskStateExecuting, TaskStateVerifying,
		TaskStateReviewing, TaskStateReworkRequired, TaskStateAwaitingApproval,
	)
}
