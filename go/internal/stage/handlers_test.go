package stage

import (
	"testing"

	"github.com/Standard-Syntax/basic/go/internal/workflow"
)

func TestExecutionExpectedRevisionSurvivesPartialExecution(t *testing.T) {
	tests := []struct {
		state    workflow.TaskState
		revision uint64
		want     uint64
	}{
		{workflow.TaskStateReasoning, 7, 7},
		{workflow.TaskStateExecuting, 8, 7},
		{workflow.TaskStateVerifying, 9, 7},
	}
	for _, test := range tests {
		got, err := executionExpectedRevision(workflow.Task{
			State: test.state, Revision: test.revision,
		})
		if err != nil || got != test.want {
			t.Fatalf("%s revision = %d, %v; want %d", test.state, got, err, test.want)
		}
	}
	if _, err := executionExpectedRevision(workflow.Task{
		State: workflow.TaskStateAwaitingApproval, Revision: 10,
	}); err == nil {
		t.Fatal("unrelated task state resumed execution")
	}
}

func TestVerificationExpectedRevisionSurvivesCompletedTransition(t *testing.T) {
	for _, test := range []struct {
		state    workflow.TaskState
		revision uint64
		want     uint64
	}{
		{workflow.TaskStateVerifying, 9, 9},
		{workflow.TaskStateReviewing, 10, 9},
	} {
		got, err := verificationExpectedRevision(workflow.Task{
			State: test.state, Revision: test.revision,
		})
		if err != nil || got != test.want {
			t.Fatalf("%s revision = %d, %v; want %d", test.state, got, err, test.want)
		}
	}
}
