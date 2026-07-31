package stage

import (
	"context"
	"errors"
	"testing"
	"time"

	reasoningv1 "github.com/Standard-Syntax/basic/go/gen/harness/reasoning/v1"
	"github.com/Standard-Syntax/basic/go/internal/execution"
	"github.com/Standard-Syntax/basic/go/internal/orchestration"
	"github.com/Standard-Syntax/basic/go/internal/reasoning/gateway"
	"github.com/Standard-Syntax/basic/go/internal/review"
	"github.com/Standard-Syntax/basic/go/internal/runtime"
	"github.com/Standard-Syntax/basic/go/internal/verification"
	"github.com/Standard-Syntax/basic/go/internal/workflow"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
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

func TestSchemaInvalidImplementationStopsBeforeExecution(t *testing.T) {
	for _, name := range []string{"malformed provider JSON", "empty change proposal"} {
		t.Run(name, func(t *testing.T) {
			fixture := newReasoningTerminalFixture(t)
			fixture.gateway.outcome = gateway.Outcome{
				Rejection: &reasoningv1.ProposalRejection{
					Code:    reasoningv1.RejectionCode_REJECTION_CODE_SCHEMA_INVALID,
					Summary: name,
				},
			}
			result, err := fixture.handlers.reason(
				t.Context(), fixture.job, orchestration.StableIdentities(fixture.job),
			)
			if err != nil {
				t.Fatal(err)
			}
			if result.Continue || fixture.execution.calls != 0 ||
				fixture.verification.calls != 0 || fixture.review.calls != 0 {
				t.Fatalf("terminal result=%#v downstream=%d/%d/%d", result,
					fixture.execution.calls, fixture.verification.calls, fixture.review.calls)
			}
			if len(fixture.workflow.commands) != 1 {
				t.Fatalf("workflow commands = %#v", fixture.workflow.commands)
			}
			if _, ok := fixture.workflow.commands[0].(workflow.RejectTaskProposal); !ok {
				t.Fatalf("workflow command = %T", fixture.workflow.commands[0])
			}
		})
	}
}

func TestProviderFailureReturnsToReconcilerWithoutDownstreamCalls(t *testing.T) {
	fixture := newReasoningTerminalFixture(t)
	fixture.gateway.err = &gateway.ProviderError{
		Kind: gateway.ProviderErrorTransport, Attempts: 3,
	}
	_, err := fixture.handlers.reason(
		t.Context(), fixture.job, orchestration.StableIdentities(fixture.job),
	)
	var providerError *gateway.ProviderError
	if !errors.As(err, &providerError) || providerError.Attempts != 3 {
		t.Fatalf("provider error = %v", err)
	}
	if len(fixture.workflow.commands) != 0 || fixture.execution.calls != 0 ||
		fixture.verification.calls != 0 || fixture.review.calls != 0 {
		t.Fatal("provider failure reached a successor stage")
	}
}

type reasoningTerminalFixture struct {
	handlers     *Handlers
	job          runtime.Job
	workflow     *stageTestWorkflow
	gateway      *stageTestGateway
	execution    *stageTestExecution
	verification *stageTestVerification
	review       *stageTestReview
}

func newReasoningTerminalFixture(t *testing.T) *reasoningTerminalFixture {
	t.Helper()
	runID, taskID := uuid.NewString(), uuid.NewString()
	requestBody, err := proto.Marshal(&reasoningv1.ImplementationRequest{})
	if err != nil {
		t.Fatal(err)
	}
	artifacts := &stageTestArtifacts{values: make(map[string][]byte)}
	requestRef, err := artifacts.Put(t.Context(), requestBody)
	if err != nil {
		t.Fatal(err)
	}
	runtimeStore := &stageTestRuntime{completed: map[string]workflow.ArtifactRef{
		orchestration.StageImplementationRequest: requestRef,
	}}
	workflowStore := &stageTestWorkflow{task: workflow.Task{
		ID: taskID, RunID: runID, State: workflow.TaskStateReasoning,
		Revision: 3, CurrentAttempt: 1, MaxAttempts: 2,
		Lease: &workflow.LeaseRef{
			ID: uuid.NewString(), OwnerID: uuid.NewString(),
			ExpiresAt: time.Now().Add(time.Hour), FencingToken: 1,
		},
	}}
	gatewayPort := &stageTestGateway{}
	executionPort := &stageTestExecution{}
	verificationPort := &stageTestVerification{}
	reviewPort := &stageTestReview{}
	return &reasoningTerminalFixture{
		handlers: &Handlers{
			config:    Config{ReasoningActorID: uuid.NewString()},
			artifacts: artifacts, runtime: runtimeStore, workflow: workflowStore,
			gateway: gatewayPort, execution: executionPort,
			verification: verificationPort, review: reviewPort, now: time.Now,
		},
		job: runtime.Job{
			ID: uuid.NewString(), RunID: runID, TaskID: &taskID, Attempt: 1,
			Stage: orchestration.StageReasoning,
		},
		workflow: workflowStore, gateway: gatewayPort, execution: executionPort,
		verification: verificationPort, review: reviewPort,
	}
}

type stageTestArtifacts struct {
	values map[string][]byte
}

func (s *stageTestArtifacts) Put(
	_ context.Context, body []byte,
) (workflow.ArtifactRef, error) {
	digest := runtime.Digest(body)
	s.values[digest] = append([]byte(nil), body...)
	return workflow.ArtifactRef{
		URI: "artifact://sha256/" + digest, Digest: digest,
	}, nil
}

func (s *stageTestArtifacts) Get(
	_ context.Context, ref workflow.ArtifactRef,
) ([]byte, error) {
	body, ok := s.values[ref.Digest]
	if !ok {
		return nil, runtime.ErrNotFound
	}
	return append([]byte(nil), body...), nil
}

type stageTestRuntime struct {
	completed map[string]workflow.ArtifactRef
}

func (*stageTestRuntime) GetRun(context.Context, string) (runtime.RunBinding, error) {
	return runtime.RunBinding{}, runtime.ErrNotFound
}

func (*stageTestRuntime) GetTask(
	context.Context, string, string,
) (runtime.TaskBinding, error) {
	return runtime.TaskBinding{}, runtime.ErrNotFound
}

func (*stageTestRuntime) CheckpointRepository(
	context.Context, string, workflow.ArtifactRef,
) error {
	return nil
}

func (s *stageTestRuntime) CompletedResult(
	_ context.Context, _ string, _ *string, _ uint32, stage string,
) (workflow.ArtifactRef, error) {
	ref, ok := s.completed[stage]
	if !ok {
		return workflow.ArtifactRef{}, runtime.ErrNotFound
	}
	return ref, nil
}

type stageTestWorkflow struct {
	task     workflow.Task
	commands []workflow.TaskCommand
}

func (*stageTestWorkflow) ExecuteRun(
	context.Context, workflow.RunCommand,
) (workflow.CommandResult, error) {
	return workflow.CommandResult{}, errors.New("unexpected run command")
}

func (s *stageTestWorkflow) ExecuteTask(
	_ context.Context, command workflow.TaskCommand,
) (workflow.CommandResult, error) {
	s.commands = append(s.commands, command)
	return workflow.CommandResult{Revision: s.task.Revision + 1}, nil
}

func (*stageTestWorkflow) GetRun(context.Context, string) (workflow.Run, error) {
	return workflow.Run{}, workflow.ErrNotFound
}

func (s *stageTestWorkflow) GetTask(
	context.Context, string, string,
) (workflow.Task, error) {
	return s.task, nil
}

type stageTestGateway struct {
	outcome gateway.Outcome
	err     error
}

func (s *stageTestGateway) ProposeImplementation(
	context.Context, *reasoningv1.ImplementationRequest,
) (gateway.Outcome, error) {
	return s.outcome, s.err
}

type stageTestExecution struct{ calls int }

func (s *stageTestExecution) Execute(
	context.Context, execution.Request,
) (execution.Result, error) {
	s.calls++
	return execution.Result{}, nil
}

type stageTestVerification struct{ calls int }

func (s *stageTestVerification) Verify(
	context.Context, verification.Request,
) (verification.Result, error) {
	s.calls++
	return verification.Result{}, nil
}

type stageTestReview struct{ calls int }

func (s *stageTestReview) Review(
	context.Context, review.Request,
) (review.Result, error) {
	s.calls++
	return review.Result{}, nil
}
